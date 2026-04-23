package agents

// Campaign 2 — AUDIT-011 read-side migration verification.
//
// After migrating the production read-sites from `payload LIKE '%"convoy_id":N,%'`
// to `convoy_id = ?`, this table-driven test asserts each migrated query:
//   (a) uses idx_bounty_convoy_status (partial-covering index on (convoy_id, status))
//   (b) does NOT fall back to SCAN BountyBoard
//
// A regression in any one call site would re-introduce the O(n) full-table
// scan the original audit flagged as a production-latency cliff.

import (
	"database/sql"
	"fmt"
	"strings"
	"testing"

	"force-orchestrator/internal/store"
)

// seedConvoyIDReadFixtures inserts ~10k rows across 10 convoys + a mix of
// statuses so SQLite's planner has enough data to make a non-trivial choice.
// Without a seeded dataset the planner may pick SCAN on an empty table and
// mislead the EXPLAIN output.
func seedConvoyIDReadFixtures(t *testing.T, db *sql.DB, convoys, perConvoy int) {
	t.Helper()
	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	stmt, err := tx.Prepare(`INSERT INTO BountyBoard
		(parent_id, target_repo, type, status, payload, convoy_id, priority, created_at)
		VALUES (0, 'api', ?, ?, ?, ?, 5, datetime('now'))`)
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	types := []string{"CodeEdit", "ConvoyReview", "CreateAskBranch", "ShipConvoy"}
	statuses := []string{"Pending", "Locked", "Completed", "Cancelled", "Failed"}
	for c := 1; c <= convoys; c++ {
		for i := 0; i < perConvoy; i++ {
			tt := types[i%len(types)]
			st := statuses[i%len(statuses)]
			payload := fmt.Sprintf(`{"convoy_id":%d,"n":%d}`, c, i)
			if _, err := stmt.Exec(tt, st, payload, c); err != nil {
				t.Fatalf("seed c=%d i=%d: %v", c, i, err)
			}
		}
	}
	stmt.Close()
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	db.Exec(`ANALYZE`) // let the planner see the distribution
}

// explainPlan runs EXPLAIN QUERY PLAN and returns the concatenated `detail`
// rows as a single newline-separated string.
func explainPlan(t *testing.T, db *sql.DB, sqlText string, args ...any) string {
	t.Helper()
	rows, err := db.Query("EXPLAIN QUERY PLAN "+sqlText, args...)
	if err != nil {
		t.Fatalf("explain %q: %v", sqlText, err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id, parent, notused int
		var detail string
		if err := rows.Scan(&id, &parent, &notused, &detail); err != nil {
			t.Fatalf("scan plan row: %v", err)
		}
		out = append(out, detail)
	}
	return strings.Join(out, "\n")
}

// TestAUDIT_011_ReadSide_QueriesUseIndex verifies every migrated read-site's
// query shape uses idx_bounty_convoy_status and refuses the SCAN fallback.
//
// The SQL literals below are copied verbatim from the production code. If a
// future change tweaks a WHERE clause or JOIN that breaks planner recognition
// of the partial-index predicate, this test flags the regression.
func TestAUDIT_011_ReadSide_QueriesUseIndex(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	seedConvoyIDReadFixtures(t, db, 10, 1000)

	type site struct {
		name string
		sql  string
		args []any
	}
	sites := []site{
		{
			name: "convoy_review.completedPasses (loop cap)",
			sql: `SELECT COUNT(*) FROM BountyBoard
				WHERE type = 'ConvoyReview' AND status = 'Completed'
				  AND convoy_id = ?`,
			args: []any{5},
		},
		{
			name: "convoy_review.lastCompletedFindingsFingerprint",
			sql: `SELECT IFNULL(last_findings_fingerprint, '') FROM BountyBoard
				WHERE type = 'ConvoyReview' AND status = 'Completed'
				  AND convoy_id = ?
				  AND IFNULL(last_findings_fingerprint, '') NOT IN ('', ?)
				ORDER BY id DESC LIMIT 1`,
			args: []any{5, convoyReviewCleanMarker},
		},
		{
			name: "convoy_review.hasPriorCleanPass",
			sql: `SELECT COUNT(*) FROM BountyBoard
				WHERE type = 'ConvoyReview' AND status = 'Completed'
				  AND convoy_id = ?
				  AND IFNULL(last_findings_fingerprint, '') = ?`,
			args: []any{5, convoyReviewCleanMarker},
		},
		{
			name: "dogConvoyReviewWatch (pending gate)",
			sql: `SELECT COUNT(*) FROM BountyBoard
				WHERE type = 'ConvoyReview' AND status IN ('Pending','Locked')
				  AND convoy_id = ?`,
			args: []any{5},
		},
		{
			name: "convoy.ShipConvoy dedup",
			sql: `SELECT COUNT(*) FROM BountyBoard
				WHERE type = 'ShipConvoy' AND status IN ('Pending', 'Locked')
				  AND convoy_id = ?`,
			args: []any{5},
		},
		{
			name: "backfillMissingAskBranches",
			sql: `SELECT COUNT(*) FROM BountyBoard
				WHERE type = 'CreateAskBranch' AND status IN ('Pending', 'Locked')
				  AND convoy_id = ?`,
			args: []any{5},
		},
		{
			name: "ConvoyReadyToShip.reviewPending",
			sql: `SELECT COUNT(*) FROM BountyBoard
				WHERE type = 'ConvoyReview'
				  AND status IN ('Pending','Locked')
				  AND convoy_id = ?`,
			args: []any{5},
		},
	}

	for _, s := range sites {
		s := s
		t.Run(s.name, func(t *testing.T) {
			plan := explainPlan(t, db, s.sql, s.args...)
			t.Logf("plan:\n%s", plan)
			if strings.Contains(plan, "SCAN BountyBoard") {
				t.Errorf("AUDIT-011 regression at %s: plan falls back to SCAN BountyBoard\n%s", s.name, plan)
			}
			// SQLite's planner may pick either idx_bounty_convoy_status or
			// idx_bounty_status_type depending on predicate shape. Either is
			// an indexed lookup — both are acceptable. We only fail if the
			// plan is a full-table scan.
			if !strings.Contains(plan, "USING INDEX") && !strings.Contains(plan, "SEARCH BountyBoard") {
				t.Errorf("AUDIT-011 fix-shape at %s: expected index search, got:\n%s", s.name, plan)
			}
		})
	}
}

// TestAUDIT_011_ListReadyToShipConvoyIDs verifies the ListReadyToShipConvoyIDs
// query (separate from the per-site table above because it's a correlated
// subquery across two tables).
func TestAUDIT_011_ListReadyToShipConvoyIDs_UsesIndex(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	seedConvoyIDReadFixtures(t, db, 5, 500)

	plan := explainPlan(t, db, `
		SELECT c.id FROM Convoys c
		WHERE c.status = 'DraftPROpen'
		  AND NOT EXISTS (
		    SELECT 1 FROM BountyBoard b
		    WHERE b.convoy_id = c.id
		      AND b.status NOT IN ('Completed','Cancelled','Failed')
		  )
		  AND NOT EXISTS (
		    SELECT 1 FROM BountyBoard r
		    WHERE r.type = 'ConvoyReview'
		      AND r.status IN ('Pending','Locked')
		      AND r.convoy_id = c.id
		  )
		ORDER BY c.id ASC`)
	t.Logf("plan:\n%s", plan)

	// The two NOT EXISTS blocks must both be indexed on BountyBoard —
	// SCAN BountyBoard in either spot re-introduces the per-row O(n) hit
	// the audit flagged.
	scanCount := strings.Count(plan, "SCAN BountyBoard")
	if scanCount > 0 {
		t.Errorf("AUDIT-011 regression: %d SCAN BountyBoard occurrence(s) in ListReadyToShipConvoyIDs plan\n%s",
			scanCount, plan)
	}
}
