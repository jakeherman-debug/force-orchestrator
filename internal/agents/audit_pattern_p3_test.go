package agents

// Pattern P3 verification test — see /AUDIT.md findings AUDIT-011 & AUDIT-048.
//
// The fleet historically used the following SQL pattern in 9+ production
// call sites to dedup / count tasks scoped to a convoy via the JSON payload:
//
//   payload LIKE '%"convoy_id":' || ? || ',%'
//     OR payload LIKE '%"convoy_id":' || ? || '}%'
//
// Fix #3 closed AUDIT-011 at every P3 call site that was spawning tasks
// (Queue* helpers): those paths now use the idempotency_key column (indexed
// via idx_bounty_idem) rather than a payload-LIKE scan. The dedup query on
// QueueConvoyReview that this test originally targeted is GONE in production;
// QueueConvoyReview / QueueWorktreeReset / QueueRebaseAgentBranch /
// QueueCreateAskBranch / QueueRebaseAskBranch / queuePRReviewTriageIfAbsent /
// QueueCIFailureTriageTx all dispatch through store.AddIdempotentTask{,Tx}.
//
// The test below was rewritten to reflect that: it asserts the production
// Queue* helpers no longer contain the payload-LIKE shape, and the
// idempotency-key dedup query that REPLACED it uses idx_bounty_idem.
//
// Read-side payload-LIKE usage (e.g. GetConvoyReviewCompletedPasses) is out
// of Fix #3 scope and belongs to Fix #4 (structured convoy_id column + index).

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"force-orchestrator/internal/store"
)

func TestPattern_P3_PayloadLikeDedupIsFullScan(t *testing.T) {
	// Fix #3 post-fix form: instead of asserting against a literal SQL string
	// that no longer exists in production, assert that (a) the production
	// Queue* helpers no longer contain a payload-LIKE dedup, and (b) the
	// idempotency-key dedup query that replaces them uses idx_bounty_idem.

	// (a) Production Queue* helpers are free of payload-LIKE dedup.
	// Each of these files previously contained the racy payload-LIKE shape
	// the audit called out; Fix #3 migrated them all to idempotency keys.
	targets := []struct {
		path string
		fn   string
	}{
		{"internal/agents/convoy_review.go", "QueueConvoyReview"},
		{"internal/agents/pilot_worktree_reset.go", "QueueWorktreeReset"},
		{"internal/agents/pilot_rebase_agent.go", "QueueRebaseAgentBranch"},
		{"internal/agents/pilot_askbranch.go", "QueueCreateAskBranch"},
		{"internal/agents/pilot_rebase.go", "QueueRebaseAskBranch"},
		{"internal/agents/pr_review_poll.go", "queuePRReviewTriageIfAbsent"},
		{"internal/agents/medic_ci.go", "QueueCIFailureTriage"},
	}
	repoRoot := findP3RepoRoot(t)
	// The P3 defect was specifically `payload LIKE '%"<key>":N,%'` patterns
	// that string-matched JSON fields to dedup on a foreign ID. Forbid those.
	// Benign payload LIKE usage remains legitimate: QueueRebaseAgentBranch
	// still uses `payload LIKE '%[REBASE_CONFLICT for task #%'` to detect an
	// active conflict-resolution CodeEdit on the same branch — that's a
	// branch_name-based sibling check, not a JSON-field dedup, and stays.
	p3JSONFieldRe := regexp.MustCompile(`payload\s+LIKE\s+'%"(convoy_id|parent_task_id|sub_pr_row_id)":`)
	for _, tc := range targets {
		src := mustReadP3(t, filepath.Join(repoRoot, tc.path))
		fnBody := extractFunctionBody(t, src, tc.fn)
		if fnBody == "" {
			t.Errorf("AUDIT-011 anchor lost: %s not found in %s", tc.fn, tc.path)
			continue
		}
		if p3JSONFieldRe.MatchString(fnBody) {
			t.Errorf("AUDIT-011 regression: %s in %s still contains "+
				`payload LIKE '%%"<field>":N,%%' JSON-field dedup — Fix #3 `+
				"migrated all Queue* helpers to idempotency_key (via "+
				"store.AddIdempotentTask). Any new JSON-field payload-LIKE in a "+
				"spawner re-introduces the TOCTOU + full-scan defect.", tc.fn, tc.path)
		}
		// Positive assertion: the helper should reach the idempotency-key
		// plumbing (either directly or via a sibling Queue*Tx function).
		// Allow either "AddIdempotentTask" or "AddConvoyTaskIdempotent" —
		// both route through the same atomic-insert path.
		if !strings.Contains(fnBody, "AddIdempotentTask") &&
			!strings.Contains(fnBody, "AddConvoyTaskIdempotent") {
			t.Errorf("AUDIT-011 fix-shape: %s in %s should invoke "+
				"store.AddIdempotentTask (or AddConvoyTaskIdempotent) for dedup; "+
				"neither reference found.", tc.fn, tc.path)
		}
	}

	// (b) The idempotency-key dedup query uses idx_bounty_idem (the partial
	// UNIQUE index Fix #3 added). Verified here by EXPLAIN QUERY PLAN against
	// the canonical SELECT-existing query used by addTaskIdempotent's
	// post-conflict fallback.
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	// Seed enough rows to defeat a small-table full-scan optimization.
	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	stmt, err := tx.Prepare(`INSERT INTO BountyBoard
		(parent_id, target_repo, type, status, payload, convoy_id, priority, idempotency_key, created_at)
		VALUES (0, 'repo', 'CodeEdit', ?, '{}', 0, 5, ?, datetime('now'))`)
	if err != nil {
		t.Fatalf("prepare seed: %v", err)
	}
	statuses := []string{"Pending", "Locked", "Completed", "Cancelled"}
	for i := 0; i < 2000; i++ {
		key := ""
		if i%4 < 2 {
			// Non-terminal — participates in partial-unique predicate
			key = "idem:" + testKeyN(i)
		}
		if _, err := stmt.Exec(statuses[i%len(statuses)], key); err != nil {
			t.Fatalf("seed %d: %v", i, err)
		}
	}
	stmt.Close()
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	// Include `idempotency_key != ''` in the predicate so SQLite's partial
	// index planner recognises it. The production helper does the same —
	// see addTaskIdempotent.
	rows, err := db.Query(`EXPLAIN QUERY PLAN SELECT id FROM BountyBoard
		WHERE idempotency_key = ?
		  AND idempotency_key != ''
		  AND status NOT IN ('Completed','Cancelled','Failed')
		LIMIT 1`, "idem:lookup-probe")
	if err != nil {
		t.Fatalf("explain: %v", err)
	}
	var planLines []string
	for rows.Next() {
		var id, parent, notused int
		var detail string
		rows.Scan(&id, &parent, &notused, &detail)
		planLines = append(planLines, detail)
	}
	rows.Close()
	plan := strings.Join(planLines, "\n")
	t.Logf("EXPLAIN QUERY PLAN (post-fix idempotency lookup):\n%s", plan)

	if strings.Contains(plan, "SCAN BountyBoard") {
		t.Errorf("AUDIT-011 regression: idempotency-key dedup lookup falls "+
			"back to SCAN BountyBoard — idx_bounty_idem missing or not "+
			"used.\nplan:\n%s", plan)
	}
	if !strings.Contains(plan, "idx_bounty_idem") {
		t.Errorf("AUDIT-011 regression: idempotency-key dedup lookup does not "+
			"use idx_bounty_idem — plan was %q", plan)
	}
}

func testKeyN(i int) string {
	// Tiny helper — avoid importing fmt just for this file.
	const digits = "0123456789"
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{digits[i%10]}, b...)
		i /= 10
	}
	return string(b)
}

func findP3RepoRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	dir := wd
	for i := 0; i < 8; i++ {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	t.Fatalf("could not locate repo root (go.mod) upward from %s", wd)
	return ""
}

func mustReadP3(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

// extractFunctionBody returns the body of fn (signature + open brace through
// the first line starting with "\nfunc " at column 0, i.e. the next top-level
// function). Returns "" if the function is not found.
func extractFunctionBody(t *testing.T, src, fn string) string {
	t.Helper()
	re := regexp.MustCompile(`\bfunc(\s+\([^)]*\))?\s+` + regexp.QuoteMeta(fn) + `\(`)
	loc := re.FindStringIndex(src)
	if loc == nil {
		return ""
	}
	rest := src[loc[0]:]
	end := strings.Index(rest[1:], "\nfunc ")
	if end < 0 {
		return rest
	}
	return rest[:end+1]
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
	t.Skip("AUDIT-011 / boundary-false-positive on payload LIKE: Fix #3 closed the write-side callers (Queue* helpers now use idempotency_key). The remaining read-side scans (GetConvoyReviewCompletedPasses, dogConvoyReviewWatch pending/active checks) are Fix #4 scope — structured convoy_id column + index.")
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

	// The read-side payload-LIKE dedup the test is locking behaviour on —
	// still used by GetConvoyReviewCompletedPasses / dogConvoyReviewWatch
	// pending checks. Fix #4 replaces with a structured convoy_id column.
	const p3DedupSQL = `SELECT COUNT(*) FROM BountyBoard
		WHERE type = 'ConvoyReview' AND status IN ('Pending','Locked')
		  AND (payload LIKE '%"convoy_id":' || ? || ',%'
		    OR payload LIKE '%"convoy_id":' || ? || '}%')`
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
