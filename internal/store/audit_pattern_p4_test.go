package store

import (
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"testing"
)

// Pattern P4 — Missing indexes on every hot table.
//
// Owns AUDIT-009 (BountyBoard), AUDIT-010 (TaskHistory), AUDIT-024
// (Fleet_Mail / Escalations / AuditLog / FleetMemory), AUDIT-058
// (dashboard correlated scans), AUDIT-059 (/api/status COUNT scans),
// AUDIT-134 (no EXPLAIN QUERY PLAN coverage for claim).
//
// These tests fail today and pass once the missing CREATE INDEX
// statements land in internal/store/schema.go.

// indexedColumns returns the list of indexes on a table and the
// ordered column list for each index.
func indexedColumns(t *testing.T, db *sql.DB, table string) map[string][]string {
	t.Helper()

	idxRows, err := db.Query(fmt.Sprintf(`PRAGMA index_list(%q)`, table))
	if err != nil {
		t.Fatalf("PRAGMA index_list(%s): %v", table, err)
	}
	defer idxRows.Close()

	type idxMeta struct {
		name   string
		unique int
		origin string
		partial int
	}
	var metas []idxMeta
	for idxRows.Next() {
		var (
			seq     int
			name    string
			unique  int
			origin  string
			partial int
		)
		if err := idxRows.Scan(&seq, &name, &unique, &origin, &partial); err != nil {
			t.Fatalf("scan index_list(%s): %v", table, err)
		}
		metas = append(metas, idxMeta{name: name, unique: unique, origin: origin, partial: partial})
	}
	if err := idxRows.Err(); err != nil {
		t.Fatalf("index_list rows err: %v", err)
	}

	out := map[string][]string{}
	for _, m := range metas {
		infoRows, err := db.Query(fmt.Sprintf(`PRAGMA index_info(%q)`, m.name))
		if err != nil {
			t.Fatalf("PRAGMA index_info(%s): %v", m.name, err)
		}
		type col struct {
			seqno int
			name  string
		}
		var cols []col
		for infoRows.Next() {
			var (
				seqno int
				cid   int
				name  sql.NullString
			)
			if err := infoRows.Scan(&seqno, &cid, &name); err != nil {
				infoRows.Close()
				t.Fatalf("scan index_info(%s): %v", m.name, err)
			}
			cols = append(cols, col{seqno: seqno, name: name.String})
		}
		infoRows.Close()
		sort.Slice(cols, func(i, j int) bool { return cols[i].seqno < cols[j].seqno })
		names := make([]string, 0, len(cols))
		for _, c := range cols {
			names = append(names, c.name)
		}
		out[m.name] = names
	}
	return out
}

// hasIndexCovering reports whether any existing index on `table`
// starts with the exact prefix `wantCols` (left-prefix match — the
// form SQLite's query planner can actually use).
func hasIndexCovering(existing map[string][]string, wantCols []string) bool {
	for _, cols := range existing {
		if len(cols) < len(wantCols) {
			continue
		}
		match := true
		for i, want := range wantCols {
			if !strings.EqualFold(cols[i], want) {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}

func TestPattern_P4_HotTablesMissingIndexes(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()

	cases := []struct {
		table  string
		cols   []string
		reason string
	}{
		// AUDIT-009 — BountyBoard: hot claim query filters on (status, type);
		// dashboard/convoy views filter on (convoy_id, status); parent rollups
		// walk parent_id; pruning dogs sort on created_at.
		{"BountyBoard", []string{"status", "type"}, "AUDIT-009: ClaimBounty WHERE status='Pending' AND type=?"},
		{"BountyBoard", []string{"convoy_id", "status"}, "AUDIT-009: convoy-scoped dashboard queries"},
		{"BountyBoard", []string{"parent_id"}, "AUDIT-009: parent rollups, child-task lookups"},
		{"BountyBoard", []string{"created_at"}, "AUDIT-009: prune / recency dashboards"},

		// AUDIT-010 — TaskHistory: dashboard correlated subqueries filter on
		// task_id; leaderboards & recency sort on created_at; some reports
		// filter (outcome, agent).
		{"TaskHistory", []string{"task_id"}, "AUDIT-010: handleTasks correlated subquery per row"},
		{"TaskHistory", []string{"created_at"}, "AUDIT-010: recency / digest sorts"},
		{"TaskHistory", []string{"outcome", "agent"}, "AUDIT-010: leaderboard & outcome reporting"},

		// AUDIT-024 — Fleet_Mail: every agent's claim loop runs
		// WHERE to_agent=? AND consumed_at=''.
		{"Fleet_Mail", []string{"to_agent", "consumed_at"}, "AUDIT-024: ReadInboxForAgent hot path"},
		{"Fleet_Mail", []string{"task_id"}, "AUDIT-024: task-scoped mail lookups"},
		{"Fleet_Mail", []string{"created_at"}, "AUDIT-024: MailStats / dashboard refresh"},

		// AUDIT-024 — Escalations: sweeper scans by status, joins by task_id.
		{"Escalations", []string{"status"}, "AUDIT-024: escalation-sweeper WHERE status='Open'"},
		{"Escalations", []string{"task_id"}, "AUDIT-024: sweeper join to BountyBoard"},

		// AUDIT-024 — AuditLog: prune-by-age + task detail views.
		{"AuditLog", []string{"created_at"}, "AUDIT-024: table-prune dog / retention"},
		{"AuditLog", []string{"task_id"}, "AUDIT-024: per-task audit view"},

		// AUDIT-024 — FleetMemory: per-repo recency retrieval before FTS.
		{"FleetMemory", []string{"repo", "created_at"}, "AUDIT-024: GetFleetMemories per-repo recency scan"},
	}

	for _, tc := range cases {
		tc := tc
		name := fmt.Sprintf("%s__%s", tc.table, strings.Join(tc.cols, "_"))
		t.Run(name, func(t *testing.T) {
			existing := indexedColumns(t, db, tc.table)
			if !hasIndexCovering(existing, tc.cols) {
				var have []string
				for idx, cols := range existing {
					have = append(have, fmt.Sprintf("%s(%s)", idx, strings.Join(cols, ",")))
				}
				sort.Strings(have)
				t.Errorf(
					"missing index on %s(%s) — %s\n    existing indexes: %s",
					tc.table, strings.Join(tc.cols, ", "), tc.reason,
					strings.Join(have, ", "),
				)
			}
		})
	}
}

// TestPattern_P4_ClaimQueryUsesIndex seeds BountyBoard and runs EXPLAIN
// QUERY PLAN against the exact SQL in ClaimBounty. AUDIT-134: without an
// index on (status, type), SQLite picks a full SCAN; we assert the plan
// shows USING INDEX (or USING COVERING INDEX) on BountyBoard.
func TestPattern_P4_ClaimQueryUsesIndex(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()

	// Seed a small realistic mix. We don't need 50k rows to read the
	// planner's decision — EXPLAIN QUERY PLAN reports the strategy the
	// planner would use regardless of cardinality, given stats/no stats.
	for i := 0; i < 200; i++ {
		typeStr := "CodeEdit"
		status := "Pending"
		if i%3 == 0 {
			status = "Completed"
		}
		if i%5 == 0 {
			typeStr = "Feature"
		}
		if _, err := db.Exec(
			`INSERT INTO BountyBoard (parent_id, type, status, payload, created_at)
			 VALUES (0, ?, ?, ?, datetime('now'))`,
			typeStr, status, fmt.Sprintf("seed-%d", i),
		); err != nil {
			t.Fatalf("seed insert: %v", err)
		}
	}

	claimSQL := `
		SELECT id, parent_id, target_repo, type, status, payload, convoy_id, checkpoint,
		       priority, IFNULL(task_timeout,0)
		FROM BountyBoard
		WHERE status = 'Pending' AND type = ?
		  AND NOT EXISTS (
		    SELECT 1 FROM TaskDependencies td
		    JOIN BountyBoard dep ON dep.id = td.depends_on
		    WHERE td.task_id = BountyBoard.id AND dep.status != 'Completed'
		  )
		  AND (convoy_id = 0 OR NOT EXISTS (
		    SELECT 1 FROM FeatureBlockers fb
		    WHERE fb.blocked_convoy_id = BountyBoard.convoy_id AND fb.resolved_at IS NULL
		  ))
		ORDER BY priority DESC, id ASC
		LIMIT 1`

	rows, err := db.Query("EXPLAIN QUERY PLAN "+claimSQL, "CodeEdit")
	if err != nil {
		t.Fatalf("EXPLAIN QUERY PLAN: %v", err)
	}
	defer rows.Close()

	var planLines []string
	sawBountyIndex := false
	for rows.Next() {
		var (
			id     int
			parent int
			notUsed int
			detail string
		)
		if err := rows.Scan(&id, &parent, &notUsed, &detail); err != nil {
			t.Fatalf("scan EXPLAIN row: %v", err)
		}
		planLines = append(planLines, detail)
		// The outer BountyBoard access is the one the claim loop runs
		// thousands of times. We want it to read as "SEARCH BountyBoard
		// USING INDEX <name>" rather than "SCAN BountyBoard".
		if strings.Contains(detail, "BountyBoard") &&
			(strings.Contains(detail, "USING INDEX") || strings.Contains(detail, "USING COVERING INDEX")) {
			// Exclude the inner "dep" alias — that one uses the primary
			// key regardless. We want an index used on the outer access,
			// which EXPLAIN reports without an alias.
			if !strings.Contains(detail, " dep ") && !strings.Contains(detail, "AS dep") {
				sawBountyIndex = true
			}
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("EXPLAIN rows err: %v", err)
	}

	if !sawBountyIndex {
		t.Errorf(
			"ClaimBounty does not use an index on BountyBoard — full SCAN in hot path (AUDIT-009, AUDIT-134)\n    plan:\n      %s",
			strings.Join(planLines, "\n      "),
		)
	}
}
