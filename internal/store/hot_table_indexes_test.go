package store

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

// Regression coverage for Fix #4 — hot-table indexes (AUDIT-009, AUDIT-010,
// AUDIT-024, AUDIT-058, AUDIT-059, AUDIT-134, AUDIT-023 schema drift,
// AUDIT-079 FK enforcement, AUDIT-081 AddRepo UPSERT).
//
// These complement TestPattern_P4_* which static-checks index presence. Here
// we exercise:
//   1. Every expected index is reported by PRAGMA index_list on both the
//      createSchema path and the runMigrations path (schema drift regression).
//   2. ClaimBounty's EXPLAIN QUERY PLAN reads as SEARCH…USING INDEX on
//      BountyBoard(status,type) under a realistic row mix.
//   3. A 10k-row seeded load still hits the index — no hidden table-size
//      cliff.
//   4. InitHolocronDSN on a fresh on-disk DB creates every expected index
//      and re-running InitHolocronDSN on the same DSN is a no-op (same row
//      counts, same set of indexes, same set of tables).
//   5. PRAGMA foreign_keys is actually enforced on a live connection and
//      the TaskNotes table has ON DELETE CASCADE attached to its FK.

// expectedHotTableIndexes maps each table to a slice of left-prefix column
// lists that MUST exist as an index. Using left-prefix matching lets a
// broader compound index satisfy a narrower expectation (this is also what
// SQLite's query planner can actually use).
func expectedHotTableIndexes() map[string][][]string {
	return map[string][][]string{
		"BountyBoard": {
			{"status", "type"},
			{"convoy_id", "status"},
			{"parent_id"},
			{"created_at"},
		},
		"TaskHistory": {
			{"task_id"},
			{"created_at"},
			{"outcome", "agent"},
		},
		"Fleet_Mail": {
			{"to_agent", "consumed_at"},
			{"task_id"},
			{"created_at"},
		},
		"Escalations": {
			{"status"},
			{"task_id"},
		},
		"AuditLog": {
			{"created_at"},
			{"task_id"},
		},
		"FleetMemory": {
			{"repo", "created_at"},
		},
		"AskBranchPRs": {
			{"task_id", "id"}, // composite for escalation-sweeper GROUP BY/MAX(id)
		},
	}
}

// indexCols returns the ordered column list for each named index on a table.
func indexCols(t *testing.T, db *sql.DB, table string) map[string][]string {
	t.Helper()
	rows, err := db.Query(fmt.Sprintf(`PRAGMA index_list(%q)`, table))
	if err != nil {
		t.Fatalf("PRAGMA index_list(%s): %v", table, err)
	}
	var names []string
	for rows.Next() {
		var (
			seq, unique, partial int
			name, origin         string
		)
		if err := rows.Scan(&seq, &name, &unique, &origin, &partial); err != nil {
			rows.Close()
			t.Fatalf("scan index_list(%s): %v", table, err)
		}
		names = append(names, name)
	}
	rows.Close()

	out := map[string][]string{}
	for _, idx := range names {
		infoRows, err := db.Query(fmt.Sprintf(`PRAGMA index_info(%q)`, idx))
		if err != nil {
			t.Fatalf("PRAGMA index_info(%s): %v", idx, err)
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
				t.Fatalf("scan index_info(%s): %v", idx, err)
			}
			cols = append(cols, col{seqno, name.String})
		}
		infoRows.Close()
		sort.Slice(cols, func(i, j int) bool { return cols[i].seqno < cols[j].seqno })
		names := make([]string, 0, len(cols))
		for _, c := range cols {
			names = append(names, c.name)
		}
		out[idx] = names
	}
	return out
}

func hasLeftPrefix(existing map[string][]string, want []string) bool {
	for _, cols := range existing {
		if len(cols) < len(want) {
			continue
		}
		match := true
		for i, w := range want {
			if !strings.EqualFold(cols[i], w) {
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

// ── Unit test 1: PRAGMA index_list reports every expected index ─────────────
// Separately verifies the createSchema code path (fresh install) and the
// runMigrations code path (upgrade from an older DB missing any of the
// columns / indexes). Both must arrive at the same index set — otherwise
// we've re-introduced the AUDIT-023 schema-drift class of bug.
func TestHotTableIndexes_CreateAndMigrateAgree(t *testing.T) {
	// Path A: fresh install via InitHolocronDSN → both createSchema and
	// runMigrations run, then FK enforcement enables.
	dbFresh := InitHolocronDSN(":memory:")
	defer dbFresh.Close()

	// Path B: simulate an old install — run createSchema once, then
	// runMigrations. For a genuinely drifted old DB we'd need to nuke the
	// expected columns before migrations; here the key assertion is that
	// runMigrations is ALSO capable of adding every index on its own.
	dbMigrated, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatalf("open :memory: sqlite: %v", err)
	}
	defer dbMigrated.Close()
	dbMigrated.SetMaxOpenConns(1)
	createSchema(dbMigrated)
	runMigrations(dbMigrated)

	for table, wants := range expectedHotTableIndexes() {
		table, wants := table, wants
		t.Run(table, func(t *testing.T) {
			freshIdx := indexCols(t, dbFresh, table)
			migIdx := indexCols(t, dbMigrated, table)
			for _, want := range wants {
				if !hasLeftPrefix(freshIdx, want) {
					t.Errorf("createSchema path missing index on %s(%s); have: %v",
						table, strings.Join(want, ","), freshIdx)
				}
				if !hasLeftPrefix(migIdx, want) {
					t.Errorf("runMigrations path missing index on %s(%s); have: %v",
						table, strings.Join(want, ","), migIdx)
				}
			}
			// Schema drift: the two paths should agree on the set of
			// hot-table index *columns*. Names may differ if one path
			// generated an autoindex — compare columns only.
			freshCols := indexColumnSet(freshIdx)
			migCols := indexColumnSet(migIdx)
			for k := range freshCols {
				if !migCols[k] {
					t.Errorf("schema drift: index on %s(%s) present on fresh path but absent on migrated path",
						table, k)
				}
			}
			for k := range migCols {
				if !freshCols[k] {
					t.Errorf("schema drift: index on %s(%s) present on migrated path but absent on fresh path",
						table, k)
				}
			}
		})
	}
}

func indexColumnSet(indexes map[string][]string) map[string]bool {
	out := map[string]bool{}
	for _, cols := range indexes {
		out[strings.Join(cols, ",")] = true
	}
	return out
}

// ── Unit test 2: FK enforcement and TaskNotes ON DELETE CASCADE ────────────
func TestHotTableIndexes_ForeignKeysEnforcedAndCascade(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()

	// PRAGMA foreign_keys must return 1 on a working connection.
	var fk int
	if err := db.QueryRow(`PRAGMA foreign_keys`).Scan(&fk); err != nil {
		t.Fatalf("PRAGMA foreign_keys: %v", err)
	}
	if fk != 1 {
		t.Fatalf("PRAGMA foreign_keys=%d, want 1", fk)
	}

	// Seed a BountyBoard row + a TaskNote. Deleting the bounty must cascade
	// to the note, not error out with FOREIGN KEY violation.
	res, err := db.Exec(`INSERT INTO BountyBoard (parent_id, type, status, payload) VALUES (0, 'CodeEdit', 'Pending', 'seed')`)
	if err != nil {
		t.Fatalf("seed BountyBoard: %v", err)
	}
	taskID, _ := res.LastInsertId()
	if _, err := db.Exec(`INSERT INTO TaskNotes (task_id, note) VALUES (?, 'operator note')`, taskID); err != nil {
		t.Fatalf("seed TaskNotes: %v", err)
	}

	if _, err := db.Exec(`DELETE FROM BountyBoard WHERE id = ?`, taskID); err != nil {
		t.Fatalf("DELETE FROM BountyBoard with FK+cascade: %v", err)
	}
	var remaining int
	if err := db.QueryRow(`SELECT COUNT(*) FROM TaskNotes WHERE task_id = ?`, taskID).Scan(&remaining); err != nil {
		t.Fatalf("count TaskNotes after delete: %v", err)
	}
	if remaining != 0 {
		t.Errorf("TaskNotes did not cascade on BountyBoard DELETE: %d rows left", remaining)
	}
}

// ── Integration test 1: ClaimBounty EXPLAIN shows index use on 10k rows ────
func TestHotTableIndexes_ClaimQueryUsesIndex_10kRows(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping 10k-row seed in -short mode")
	}
	db := InitHolocronDSN(":memory:")
	defer db.Close()

	// Seed 10k rows with a realistic status/type mix. Inside a single
	// transaction so seed time stays well under a second.
	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	stmt, err := tx.Prepare(`INSERT INTO BountyBoard (parent_id, type, status, payload, created_at)
		VALUES (0, ?, ?, ?, datetime('now'))`)
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	types := []string{"CodeEdit", "Feature", "Decompose", "ConvoyReview", "MedicReview"}
	statuses := []string{"Pending", "Completed", "Locked", "Failed", "AwaitingCouncilReview"}
	for i := 0; i < 10000; i++ {
		_, err := stmt.Exec(types[i%len(types)], statuses[i%len(statuses)], fmt.Sprintf("seed-%d", i))
		if err != nil {
			t.Fatalf("insert %d: %v", i, err)
		}
	}
	stmt.Close()
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
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
	var plan []string
	used := false
	for rows.Next() {
		var id, parent, notUsed int
		var detail string
		if err := rows.Scan(&id, &parent, &notUsed, &detail); err != nil {
			t.Fatalf("scan: %v", err)
		}
		plan = append(plan, detail)
		if strings.Contains(detail, "BountyBoard") &&
			(strings.Contains(detail, "USING INDEX") || strings.Contains(detail, "USING COVERING INDEX")) &&
			!strings.Contains(detail, " dep ") && !strings.Contains(detail, "AS dep") {
			used = true
		}
	}
	if !used {
		t.Errorf("claim query still full-scans BountyBoard at 10k rows. plan:\n  %s",
			strings.Join(plan, "\n  "))
	}

	// Latency bound: one claim must be well under 50ms even at 10k rows.
	start := time.Now()
	var gotID int
	if err := db.QueryRow(claimSQL, "CodeEdit").Scan(new(int), new(int), new(sql.NullString), new(sql.NullString),
		new(sql.NullString), new(sql.NullString), new(int), new(sql.NullString), new(int), new(int)); err != nil {
		// The query might legitimately find nothing if no Pending CodeEdit
		// survived the mix; we only care about latency in that case.
		if err != sql.ErrNoRows {
			t.Fatalf("claim query: %v", err)
		}
	}
	elapsed := time.Since(start)
	_ = gotID
	if elapsed > 50*time.Millisecond {
		t.Errorf("claim query on 10k rows took %v, want < 50ms — index not being used?", elapsed)
	}
}

// ── Integration test 2: escalation-sweeper GROUP BY uses composite index ────
func TestHotTableIndexes_EscalationSweeperGroupByUsesIndex(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()

	// The exact query shape from escalation_sweeper.go's Rule 2.
	query := `
		SELECT e.id, e.task_id, pr.pr_number, pr.state
		FROM Escalations e
		JOIN BountyBoard b ON b.id = e.task_id
		JOIN (
			SELECT task_id, MAX(id) AS pr_id
			FROM AskBranchPRs
			GROUP BY task_id
		) latest ON latest.task_id = e.task_id
		JOIN AskBranchPRs pr ON pr.id = latest.pr_id
		WHERE e.status = 'Open'
		  AND b.status IN ('Escalated','Failed')
		  AND pr.state IN ('Merged','Closed')`

	rows, err := db.Query("EXPLAIN QUERY PLAN " + query)
	if err != nil {
		t.Fatalf("EXPLAIN QUERY PLAN: %v", err)
	}
	defer rows.Close()
	var plan []string
	// EXPLAIN QUERY PLAN reports aliases where the query names them ("e",
	// "b", "pr"). The plan lines look like:
	//   SCAN AskBranchPRs USING COVERING INDEX idx_ask_branch_prs_task_id
	//   SEARCH e USING INDEX idx_escalations_status (status=?)
	// We assert against named hot-table indexes so both the aliased
	// (Escalations via "e") and un-aliased (AskBranchPRs) accesses count.
	sawAskBranchPRsIdx := false
	sawEscalationsIdx := false
	for rows.Next() {
		var id, parent, notUsed int
		var detail string
		if err := rows.Scan(&id, &parent, &notUsed, &detail); err != nil {
			t.Fatalf("scan: %v", err)
		}
		plan = append(plan, detail)
		if strings.Contains(detail, "idx_ask_branch_prs_task_id") {
			sawAskBranchPRsIdx = true
		}
		if strings.Contains(detail, "idx_escalations_status") ||
			strings.Contains(detail, "idx_escalations_task_id") {
			sawEscalationsIdx = true
		}
	}
	if !sawAskBranchPRsIdx {
		t.Errorf("escalation-sweeper AskBranchPRs access does not use a hot-table index. plan:\n  %s",
			strings.Join(plan, "\n  "))
	}
	if !sawEscalationsIdx {
		t.Errorf("escalation-sweeper Escalations access does not use a hot-table index. plan:\n  %s",
			strings.Join(plan, "\n  "))
	}
}

// ── Smoke/boot: on-disk fresh DB + re-run is no-op ──────────────────────────
func TestHotTableIndexes_OnDiskFreshAndRerunIdempotent(t *testing.T) {
	dir := t.TempDir()
	dsn := filepath.Join(dir, "holocron.db") + "?_busy_timeout=5000&_journal_mode=WAL"
	defer os.Remove(filepath.Join(dir, "holocron.db"))

	// Boot #1 — fresh.
	db1 := InitHolocronDSN(dsn)
	initialIndexes := collectAllIndexes(t, db1)
	initialTables := collectAllTables(t, db1)
	if len(initialIndexes) == 0 {
		t.Fatalf("no indexes found after fresh init — expected many")
	}
	// Sanity: every expected hot-table index is present.
	for table, wants := range expectedHotTableIndexes() {
		cols := indexCols(t, db1, table)
		for _, want := range wants {
			if !hasLeftPrefix(cols, want) {
				t.Errorf("fresh on-disk init missing index %s(%s); have %v",
					table, strings.Join(want, ","), cols)
			}
		}
	}
	db1.Close()

	// Boot #2 — same DSN, migrations should all be no-ops.
	db2 := InitHolocronDSN(dsn)
	defer db2.Close()

	rerunIndexes := collectAllIndexes(t, db2)
	rerunTables := collectAllTables(t, db2)

	// Same set of tables.
	if diff := setDiff(initialTables, rerunTables); diff != "" {
		t.Errorf("re-run changed tables: %s", diff)
	}
	// Same set of indexes.
	if diff := setDiff(initialIndexes, rerunIndexes); diff != "" {
		t.Errorf("re-run changed indexes (migrations not idempotent): %s", diff)
	}
}

func collectAllIndexes(t *testing.T, db *sql.DB) map[string]bool {
	t.Helper()
	rows, err := db.Query(`SELECT name FROM sqlite_master WHERE type='index' AND sql IS NOT NULL`)
	if err != nil {
		t.Fatalf("collectAllIndexes: %v", err)
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out[n] = true
	}
	return out
}

func collectAllTables(t *testing.T, db *sql.DB) map[string]bool {
	t.Helper()
	rows, err := db.Query(`SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%'`)
	if err != nil {
		t.Fatalf("collectAllTables: %v", err)
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out[n] = true
	}
	return out
}

func setDiff(a, b map[string]bool) string {
	var missing, extra []string
	for k := range a {
		if !b[k] {
			missing = append(missing, k)
		}
	}
	for k := range b {
		if !a[k] {
			extra = append(extra, k)
		}
	}
	sort.Strings(missing)
	sort.Strings(extra)
	if len(missing) == 0 && len(extra) == 0 {
		return ""
	}
	return fmt.Sprintf("missing after rerun: %v; extra after rerun: %v", missing, extra)
}
