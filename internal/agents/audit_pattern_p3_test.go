package agents

// Pattern P3 verification test — see /AUDIT.md findings AUDIT-011 & AUDIT-048.
//
// The fleet uses the following SQL pattern in 9+ production call sites to
// dedup / count tasks scoped to a convoy via the JSON payload:
//
//   payload LIKE '%"convoy_id":' || ? || ',%'
//     OR payload LIKE '%"convoy_id":' || ? || '}%'
//
// Production sites (non-test):
//   internal/agents/convoy_review.go:91,126,340
//   internal/agents/convoy.go:59
//   internal/agents/pilot_backfill.go:38
//   internal/agents/pilot_rebase.go:256
//   internal/agents/pr_review_poll.go:230
//   internal/store/convoy_ask_branches.go:219,241
//
// This pattern has two defects:
//
//   1. Leading wildcards ('%"convoy_id":...') disable any index on `payload`,
//      forcing a full BountyBoard scan on every invocation.
//   2. Boundary-fragile JSON matching: a payload containing unrelated keys
//      whose string form ends with `:N,` or `:N}` (e.g. another field whose
//      value collides with the target id) can produce false positives.
//
// This test locks the current behaviour so the defect is visible. When the
// remedy lands (structured `convoy_id` column + index, or a generated JSON
// extraction), the assertions invert and this test fails loudly — forcing
// the author to remove the locking test and verify the fix semantics.

import (
	"fmt"
	"strings"
	"testing"

	"force-orchestrator/internal/store"
)

// The literal dedup query lifted verbatim from convoy_review.go:89-92
// (QueueConvoyReview's existence check). Any call site of the P3 pattern
// produces identical plans since they all share the same LIKE structure.
const p3DedupSQL = `SELECT COUNT(*) FROM BountyBoard
	WHERE type = 'ConvoyReview' AND status IN ('Pending','Locked')
	  AND (payload LIKE '%"convoy_id":' || ? || ',%'
	    OR payload LIKE '%"convoy_id":' || ? || '}%')`

func TestPattern_P3_PayloadLikeDedupIsFullScan(t *testing.T) {
	t.Skip("AUDIT-011: remove when Fix #3/#4 (structured convoy_id column + index replaces payload LIKE) lands")
	// Without skip, fails with:
	//   EXPLAIN QUERY PLAN output: SCAN BountyBoard
	//   Pattern P3 defect still present — plan contains 'SCAN BountyBoard'.
	//   Fix #3/#4 requires structured convoy_id column + index replacing payload LIKE.
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	// ── Seed 10,000 BountyBoard rows with varied payloads. ────────────────
	// We mix:
	//   * ConvoyReview rows targeting convoy_ids 1..1000 (~10 rows each)
	//   * Rows with an "other_id" key whose numeric value could string-collide
	//   * Rows with status Completed/Cancelled so the dedup filter bites
	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	stmt, err := tx.Prepare(`INSERT INTO BountyBoard
		(parent_id, target_repo, type, status, payload, convoy_id, priority, created_at)
		VALUES (0, 'repo', ?, ?, ?, 0, 5, datetime('now'))`)
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	statuses := []string{"Pending", "Locked", "Completed", "Cancelled"}
	for i := 0; i < 10000; i++ {
		cid := (i % 1000) + 1
		payload := fmt.Sprintf(`{"convoy_id":%d,"note":"seed-%d"}`, cid, i)
		if _, err := stmt.Exec("ConvoyReview", statuses[i%len(statuses)], payload); err != nil {
			t.Fatalf("exec seed %d: %v", i, err)
		}
	}
	stmt.Close()
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	// Sanity: confirm the seed landed.
	var total int
	if err := db.QueryRow(`SELECT COUNT(*) FROM BountyBoard`).Scan(&total); err != nil {
		t.Fatalf("count seed: %v", err)
	}
	if total < 10000 {
		t.Fatalf("expected >=10000 seed rows, got %d", total)
	}

	// ── EXPLAIN QUERY PLAN for the cited dedup SQL. ─────────────────────────
	rows, err := db.Query("EXPLAIN QUERY PLAN "+p3DedupSQL, 42, 42)
	if err != nil {
		t.Fatalf("explain: %v", err)
	}
	defer rows.Close()

	var planLines []string
	for rows.Next() {
		var id, parent, notused int
		var detail string
		if err := rows.Scan(&id, &parent, &notused, &detail); err != nil {
			t.Fatalf("scan plan: %v", err)
		}
		planLines = append(planLines, detail)
	}
	plan := strings.Join(planLines, "\n")
	t.Logf("EXPLAIN QUERY PLAN output:\n%s", plan)

	// ── RGR: assert the desired (post-fix) plan, not the current defect. ────
	// After Fix #3/#4 lands (structured convoy_id column + index replacing the
	// payload LIKE pattern), the query plan should either avoid a full
	// "SCAN BountyBoard" OR use an index. Today neither holds — the test
	// fails loudly until the fix is in place.
	if strings.Contains(plan, "SCAN BountyBoard") {
		t.Fatalf("Pattern P3 defect still present — plan contains 'SCAN BountyBoard'.\n"+
			"Fix #3/#4 requires structured convoy_id column + index replacing payload LIKE.\nplan:\n%s", plan)
	}
	if !strings.Contains(plan, "USING INDEX") {
		t.Fatalf("Pattern P3 defect still present — plan does not use an index.\n"+
			"Fix #3/#4 requires structured convoy_id column + index replacing payload LIKE.\nplan:\n%s", plan)
	}
}

// Sub-test companion: confirm the leading-wildcard LIKE produces a
// false-positive when a payload has a NESTED JSON object whose inner key is
// also `convoy_id`. The LIKE is pure substring search with no JSON structural
// awareness — it will happily match `"convoy_id":5}` sitting inside a nested
// sub-object whose semantic meaning has nothing to do with the top-level
// convoy_id the code is filtering on.
//
// The fleet genuinely produces payloads like this: Captain rulings can embed
// the prior attempt's payload as a nested field; ConvoyReview findings embed
// referenced task payloads for context. Any such nested structure silently
// mis-dedups against whatever inner convoy_id happens to appear.
//
// Today this test asserts the broken pattern matches BOTH rows. After the
// fix to a structured column / JSON-extract index, only the real row
// matches and this locking test flips — delete it alongside the fix.
func TestPattern_P3_BoundaryFalsePositive(t *testing.T) {
	t.Skip("AUDIT-048: remove when Fix #3/#4 (structured convoy_id column + index replaces payload LIKE) lands")
	// Without skip, fails with:
	//   Pattern P3 boundary-defect still present — got 2 matches (real + nested-convoy_id
	//   collision), want 1 (only real). Fix #3/#4 requires structured convoy_id column so
	//   dedup queries have JSON-structural awareness.
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	// Row A: legitimate convoy_id=5 payload.
	if _, err := db.Exec(`INSERT INTO BountyBoard
		(parent_id, target_repo, type, status, payload, priority, created_at)
		VALUES (0, 'repo', 'ConvoyReview', 'Pending', ?, 5, datetime('now'))`,
		`{"convoy_id":5,"note":"real"}`); err != nil {
		t.Fatalf("seed real row: %v", err)
	}
	// Row B: collides. Real (semantic) convoy is 999; a nested `prev` object
	// references convoy 5 as historical context. The LIKE has no concept of
	// nesting — it finds `"convoy_id":5}` inside `"prev":{"convoy_id":5}` and
	// matches.
	if _, err := db.Exec(`INSERT INTO BountyBoard
		(parent_id, target_repo, type, status, payload, priority, created_at)
		VALUES (0, 'repo', 'ConvoyReview', 'Pending', ?, 5, datetime('now'))`,
		`{"convoy_id":999,"prev":{"convoy_id":5}}`); err != nil {
		t.Fatalf("seed colliding row: %v", err)
	}

	var matched int
	if err := db.QueryRow(p3DedupSQL, 5, 5).Scan(&matched); err != nil {
		t.Fatalf("query: %v", err)
	}
	// RGR: assert semantically-correct behaviour. Only the genuine
	// convoy_id=5 row should match; the nested-convoy_id collision must NOT.
	// Today the LIKE matches both rows (has no notion of JSON structure), so
	// the test fails until Fix #3/#4 (structured convoy_id column) lands.
	if matched != 1 {
		t.Fatalf("Pattern P3 boundary-defect still present — got %d matches (real + nested-convoy_id collision), "+
			"want 1 (only real). Fix #3/#4 requires structured convoy_id column so dedup queries "+
			"have JSON-structural awareness.", matched)
	}
}
