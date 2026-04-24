package agents

// Fix #7 — ConvoyReview tightening regression tests.
//
// These tests pin the four invariants introduced by Fix #7:
//
//   1. convoy_review_max_findings default is 2 (not 5). 8 findings in →
//      2 fix tasks spawned (cap applied). Other tests in convoy_review_test.go
//      still assert the configured-override path works.
//
//   2. ConvoyReview parse-failure escalation.
//      First LLM parse failure → retry on the same task row with a critic
//      note appended. Second parse failure → escalate (FailBounty +
//      CreateEscalation), NOT "complete → dog retrigger". Previously the
//      silent-complete path let the dog burn ~$5/pass × 5 passes.
//
//   3. Pass-to-pass fingerprint dedup.
//      If pass N's finding set fingerprint equals the most recent
//      Completed ConvoyReview's fingerprint, escalate (conflicted_loop).
//      The fix tasks spawned last pass were supposed to resolve these
//      findings — they didn't, and we refuse to spawn identical fix tasks
//      again.
//
//   4. Adversarial LLM bounded-cost.
//      Across many alternating LLM responses (needs_work / malformed /
//      clean), the total Claude call count stays under a hard cap
//      irrespective of how many passes the dog re-triggers. Fix #7
//      shifts the worst-case per-convoy cost from 25+ sessions to ≤10.
//
// These tests replace the red-phase static greps in audit_cost_loops_test.go
// for AUDIT-006 / AUDIT-007 / AUDIT-113 / AUDIT-138.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"force-orchestrator/internal/store"
)

// ── Fix #7 invariant: max_findings default is 2 ──────────────────────────────

// TestRunConvoyReview_MaxFindingsDefaultIsTwo verifies the SystemConfig
// default for convoy_review_max_findings is 2 (not 5). Stub returns 8
// findings — caller must cap at 2.
func TestRunConvoyReview_MaxFindingsDefaultIsTwo(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	convoyID := seedDraftPROpenConvoy(t, db)

	// 8 findings, each uniquely identifiable so fingerprinting is
	// deterministic (AUDIT-006 requires at least as many distinct
	// findings as the cap).
	findings := make([]convoyReviewFinding, 8)
	for i := range findings {
		findings[i] = convoyReviewFinding{
			Type: "gap", Description: fmt.Sprintf("gap %d", i),
			Fix: fmt.Sprintf("fix %d", i), Repo: "api",
			File: fmt.Sprintf("f%d.go", i), Line: i + 1,
		}
	}
	stub := stubConvoyReviewLLM(t, convoyReviewResult{Status: "needs_work", Findings: findings})

	payload, _ := json.Marshal(convoyReviewPayload{ConvoyID: convoyID})
	bounty := &store.Bounty{ID: 1001, Type: "ConvoyReview", Payload: string(payload)}
	db.Exec(`INSERT INTO BountyBoard (id, parent_id, target_repo, type, status, payload, convoy_id, priority, created_at)
		VALUES (1001, 0, '', 'ConvoyReview', 'Locked', ?, ?, 5, datetime('now'))`, string(payload), convoyID)

	runConvoyReview(context.Background(), db, "Diplomat-1", bounty, testLogger{})

	var fixCount int
	db.QueryRow(`SELECT COUNT(*) FROM BountyBoard WHERE parent_id = 1001 AND type = 'CodeEdit'`).Scan(&fixCount)
	if fixCount != 2 {
		t.Errorf("expected 2 fix tasks (Fix #7 default cap), got %d", fixCount)
	}

	// Prompt shape assertion (AUDIT-135 companion).
	assertConvoyReviewPromptShape(t, stub.LastPrompt())

	// CallCount assertion (AUDIT-111 / AUDIT-113 companion).
	if got := stub.CallCount(); got != 1 {
		t.Errorf("expected exactly 1 Claude call per pass, got %d", got)
	}
}

// TestRunConvoyReview_OperatorOverrideStillHonoured makes sure Fix #7's
// default change didn't break the SystemConfig escape hatch.
func TestRunConvoyReview_OperatorOverrideStillHonoured(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	convoyID := seedDraftPROpenConvoy(t, db)
	// Operator raises the cap for this fleet deployment.
	db.Exec(`INSERT OR REPLACE INTO SystemConfig (key, value) VALUES ('convoy_review_max_findings', '4')`)

	findings := make([]convoyReviewFinding, 5)
	for i := range findings {
		findings[i] = convoyReviewFinding{
			Type: "gap", Description: fmt.Sprintf("override-gap %d", i),
			Fix: fmt.Sprintf("fix %d", i), Repo: "api",
			File: fmt.Sprintf("f%d.go", i), Line: i + 1,
		}
	}
	stubConvoyReviewLLM(t, convoyReviewResult{Status: "needs_work", Findings: findings})

	payload, _ := json.Marshal(convoyReviewPayload{ConvoyID: convoyID})
	bounty := &store.Bounty{ID: 1002, Type: "ConvoyReview", Payload: string(payload)}
	db.Exec(`INSERT INTO BountyBoard (id, parent_id, target_repo, type, status, payload, convoy_id, priority, created_at)
		VALUES (1002, 0, '', 'ConvoyReview', 'Locked', ?, ?, 5, datetime('now'))`, string(payload), convoyID)

	runConvoyReview(context.Background(), db, "Diplomat-1", bounty, testLogger{})

	var fixCount int
	db.QueryRow(`SELECT COUNT(*) FROM BountyBoard WHERE parent_id = 1002 AND type = 'CodeEdit'`).Scan(&fixCount)
	if fixCount != 4 {
		t.Errorf("operator-override cap 4 not honoured, got %d fix tasks", fixCount)
	}
}

// ── Fix #7 invariant: parse-failure escalation after 2 tries ─────────────────

// TestRunConvoyReview_ParseFailure_EscalatesAfterCap verifies the new
// behaviour replacing "complete → dog retrigger on next tick" —
// after 2 parse failures on the same ConvoyReview row, escalate with
// CreateEscalation and FailBounty. Claude must be called exactly 2 times
// (original + one retry with critic note).
func TestRunConvoyReview_ParseFailure_EscalatesAfterCap(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	convoyID := seedDraftPROpenConvoy(t, db)

	// Stub always returns garbage so the JSON parse always fails.
	stub := withStubCLIRunner(t, "this is not json { broken", nil)

	payload, _ := json.Marshal(convoyReviewPayload{ConvoyID: convoyID})
	bounty := &store.Bounty{ID: 1003, Type: "ConvoyReview", Payload: string(payload)}
	db.Exec(`INSERT INTO BountyBoard (id, parent_id, target_repo, type, status, payload, convoy_id, priority, created_at)
		VALUES (1003, 0, '', 'ConvoyReview', 'Locked', ?, ?, 5, datetime('now'))`, string(payload), convoyID)

	runConvoyReview(context.Background(), db, "Diplomat-1", bounty, testLogger{})

	// Must have called Claude exactly 2 times: first try + one critic-note retry.
	if got := stub.CallCount(); got != 2 {
		t.Errorf("expected exactly 2 Claude calls (one retry), got %d", got)
	}

	var status string
	db.QueryRow(`SELECT status FROM BountyBoard WHERE id = 1003`).Scan(&status)
	if status != "Failed" {
		t.Errorf("expected Failed (escalated), got %s", status)
	}

	var escCount int
	db.QueryRow(`SELECT COUNT(*) FROM Escalations WHERE task_id = 1003`).Scan(&escCount)
	if escCount != 1 {
		t.Errorf("expected 1 escalation row, got %d", escCount)
	}

	// parse_failure_count on the row should be exactly 2 (incremented twice).
	var parseFailures int
	db.QueryRow(`SELECT IFNULL(parse_failure_count, 0) FROM BountyBoard WHERE id = 1003`).Scan(&parseFailures)
	if parseFailures != 2 {
		t.Errorf("expected parse_failure_count=2, got %d", parseFailures)
	}
}

// ── Fix #7 invariant: pass-to-pass fingerprint dedup ─────────────────────────

// TestRunConvoyReview_FingerprintDedup_SpawnIsSuppressed is the headline
// regression test for the pass-N == pass-(N-1) short-circuit. Both passes
// produce the same findings; second pass must NOT spawn more fix tasks,
// must escalate (conflicted_loop), and the stub must be called exactly
// once on the second pass (no retry, no extra work).
func TestRunConvoyReview_FingerprintDedup_SpawnIsSuppressed(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	convoyID := seedDraftPROpenConvoy(t, db)

	// Both passes will return the same two findings.
	sameFindings := []convoyReviewFinding{
		{Type: "gap", Description: "rate limit patterns not updated",
			Fix: "add stream idle timeout", Repo: "api", File: "rate.go", Line: 42},
		{Type: "regression", Description: "flusher removed",
			Fix: "restore flusher", Repo: "api", File: "stream.go", Line: 100},
	}

	// ── Pass 1 — spawns fix tasks as normal ──
	stubConvoyReviewLLM(t, convoyReviewResult{Status: "needs_work", Findings: sameFindings})

	payload, _ := json.Marshal(convoyReviewPayload{ConvoyID: convoyID})
	bounty1 := &store.Bounty{ID: 2001, Type: "ConvoyReview", Payload: string(payload)}
	db.Exec(`INSERT INTO BountyBoard (id, parent_id, target_repo, type, status, payload, convoy_id, priority, created_at)
		VALUES (2001, 0, '', 'ConvoyReview', 'Locked', ?, ?, 5, datetime('now'))`, string(payload), convoyID)

	runConvoyReview(context.Background(), db, "Diplomat-1", bounty1, testLogger{})

	// Pass 1 should have spawned 2 fix tasks (both findings, under the default cap of 2).
	var pass1Fix int
	db.QueryRow(`SELECT COUNT(*) FROM BountyBoard WHERE parent_id = 2001 AND type = 'CodeEdit'`).Scan(&pass1Fix)
	if pass1Fix != 2 {
		t.Errorf("pass 1: expected 2 fix tasks, got %d", pass1Fix)
	}
	// The fingerprint must be persisted on the Completed row.
	var fp1 string
	db.QueryRow(`SELECT IFNULL(last_findings_fingerprint, '') FROM BountyBoard WHERE id = 2001`).Scan(&fp1)
	if fp1 == "" {
		t.Fatal("pass 1: expected last_findings_fingerprint to be set")
	}

	// Simulate the pass-1 fix tasks completing so the dog would normally
	// re-trigger; also mark any in-flight convoy work Completed so the
	// "diff still moving" gate doesn't short-circuit pass 2.
	db.Exec(`UPDATE BountyBoard SET status = 'Completed' WHERE parent_id = 2001`)

	// ── Pass 2 — SAME findings, identical fingerprint ──
	stub := stubConvoyReviewLLM(t, convoyReviewResult{Status: "needs_work", Findings: sameFindings})

	bounty2 := &store.Bounty{ID: 2002, Type: "ConvoyReview", Payload: string(payload)}
	db.Exec(`INSERT INTO BountyBoard (id, parent_id, target_repo, type, status, payload, convoy_id, priority, created_at)
		VALUES (2002, 0, '', 'ConvoyReview', 'Locked', ?, ?, 5, datetime('now'))`, string(payload), convoyID)

	runConvoyReview(context.Background(), db, "Diplomat-1", bounty2, testLogger{})

	// Pass 2 must NOT have spawned any fix tasks.
	var pass2Fix int
	db.QueryRow(`SELECT COUNT(*) FROM BountyBoard WHERE parent_id = 2002 AND type = 'CodeEdit'`).Scan(&pass2Fix)
	if pass2Fix != 0 {
		t.Errorf("pass 2 (same fingerprint): expected 0 fix tasks, got %d", pass2Fix)
	}

	// Pass 2 must have escalated (FailBounty path).
	var status string
	db.QueryRow(`SELECT status FROM BountyBoard WHERE id = 2002`).Scan(&status)
	if status != "Failed" {
		t.Errorf("pass 2 expected Failed (conflicted_loop), got %s", status)
	}

	// Pass 2 must have created an Escalation row.
	var escCount int
	db.QueryRow(`SELECT COUNT(*) FROM Escalations WHERE task_id = 2002`).Scan(&escCount)
	if escCount != 1 {
		t.Errorf("pass 2 expected 1 Escalation, got %d", escCount)
	}

	// Pass 2 must have called Claude exactly once (no retry — the
	// fingerprint check fires AFTER the LLM call, which is the correct
	// place: we need the finding set to compute it).
	if got := stub.CallCount(); got != 1 {
		t.Errorf("pass 2: expected exactly 1 Claude call (no retry on successful parse), got %d", got)
	}
}

// TestRunConvoyReview_DifferentFindings_NoDedup verifies the dedup is
// precise: findings that differ in any fingerprint field (repo, file,
// line, type, normalised description) do NOT match, and pass 2 proceeds
// normally. Prevents an over-broad fingerprint that collides on
// incidentally-similar findings.
func TestRunConvoyReview_DifferentFindings_NoDedup(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	convoyID := seedDraftPROpenConvoy(t, db)

	// ── Pass 1 ──
	stubConvoyReviewLLM(t, convoyReviewResult{
		Status: "needs_work",
		Findings: []convoyReviewFinding{
			{Type: "gap", Description: "alpha", Fix: "a", Repo: "api", File: "a.go", Line: 1},
		},
	})
	payload, _ := json.Marshal(convoyReviewPayload{ConvoyID: convoyID})
	bounty1 := &store.Bounty{ID: 2101, Type: "ConvoyReview", Payload: string(payload)}
	db.Exec(`INSERT INTO BountyBoard (id, parent_id, target_repo, type, status, payload, convoy_id, priority, created_at)
		VALUES (2101, 0, '', 'ConvoyReview', 'Locked', ?, ?, 5, datetime('now'))`, string(payload), convoyID)
	runConvoyReview(context.Background(), db, "Diplomat-1", bounty1, testLogger{})
	db.Exec(`UPDATE BountyBoard SET status = 'Completed' WHERE parent_id = 2101`)

	// ── Pass 2 — DIFFERENT finding (different description + line + file) ──
	stubConvoyReviewLLM(t, convoyReviewResult{
		Status: "needs_work",
		Findings: []convoyReviewFinding{
			{Type: "gap", Description: "beta", Fix: "b", Repo: "api", File: "b.go", Line: 2},
		},
	})
	bounty2 := &store.Bounty{ID: 2102, Type: "ConvoyReview", Payload: string(payload)}
	db.Exec(`INSERT INTO BountyBoard (id, parent_id, target_repo, type, status, payload, convoy_id, priority, created_at)
		VALUES (2102, 0, '', 'ConvoyReview', 'Locked', ?, ?, 5, datetime('now'))`, string(payload), convoyID)
	runConvoyReview(context.Background(), db, "Diplomat-1", bounty2, testLogger{})

	// Pass 2 must have spawned the new finding's fix task (no false-positive dedup).
	var pass2Fix int
	db.QueryRow(`SELECT COUNT(*) FROM BountyBoard WHERE parent_id = 2102 AND type = 'CodeEdit'`).Scan(&pass2Fix)
	if pass2Fix != 1 {
		t.Errorf("different findings must NOT dedup; expected 1 fix task spawned, got %d", pass2Fix)
	}

	var status string
	db.QueryRow(`SELECT status FROM BountyBoard WHERE id = 2102`).Scan(&status)
	if status != "Completed" {
		t.Errorf("pass 2 with genuinely new findings should Complete; got %s", status)
	}
}

// ── Fix #7 invariant: adversarial LLM → bounded total Claude calls ───────────

// TestConvoyReview_TotalClaudeCallsBounded is the headline cost-loop
// protection test (AUDIT-113 / AUDIT-138). Simulates a long-running
// convoy lifecycle where the LLM alternates between malformed JSON,
// needs_work, and clean responses across many dog-retrigger cycles.
// Asserts the total Claude call count stays under a hard cap.
//
// The hard cap is the worst-case bound of Fix #7:
//   5 passes max × (1 LLM call + possibly 1 retry on parse fail) = 10.
// Adding a small margin: 12.
func TestConvoyReview_TotalClaudeCallsBounded(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	convoyID := seedDraftPROpenConvoy(t, db)

	// Adversarial response rotation: parse-fail, needs-work, parse-fail, clean.
	// The dog would normally retrigger 50 times if we let it; the Fix #7
	// caps (pass count, parse-failure escalate, fingerprint dedup) must
	// keep total Claude calls under the hard bound regardless.
	responses := []struct {
		out string
		err error
	}{
		{`{"status":"needs_work","findings":[{"type":"gap","description":"x","fix":"y","repo":"api","file":"a.go","line":1}]}`, nil},
		{`broken not json`, nil},
		{`{"status":"needs_work","findings":[{"type":"gap","description":"x","fix":"y","repo":"api","file":"a.go","line":1}]}`, nil},
		{`{"status":"clean","findings":[]}`, nil},
	}
	idx := 0
	stub := withStubCLIRunnerFn(t, func(prompt, tools, dir string, maxTurns int, timeout time.Duration) (string, error) {
		r := responses[idx%len(responses)]
		idx++
		return r.out, r.err
	})

	const convoyReviewMaxTotalCalls = 12

	// Simulate the dog firing many times: each time, we queue a
	// ConvoyReview if none is pending and run it to terminal state.
	iterations := 0
	for i := 0; i < 50; i++ {
		iterations++
		// Mark any completed fix tasks so the dog's convoy-quiescent
		// gate passes and a new pass can run.
		db.Exec(`UPDATE BountyBoard SET status = 'Completed'
			WHERE convoy_id = ? AND status = 'Pending' AND type = 'CodeEdit'`, convoyID)

		taskID, _ := QueueConvoyReview(db, convoyID)
		if taskID == 0 {
			// Nothing to queue (e.g. parse-fail escalated the prior row
			// and the dog is waiting for operator attention). Exit early.
			break
		}
		// Lock + run.
		db.Exec(`UPDATE BountyBoard SET status = 'Locked', owner = 'Diplomat-1' WHERE id = ?`, taskID)
		b, _ := store.GetBounty(db, taskID)
		runConvoyReview(context.Background(), db, "Diplomat-1", b, testLogger{})

		// If the convoy hit the loop cap OR an escalation landed, we're done.
		var pendingReview int
		// Fix A (AUDIT-011 read-side): migrated from payload-LIKE to convoy_id equality.
		db.QueryRow(`SELECT COUNT(*) FROM BountyBoard WHERE type = 'ConvoyReview'
			AND status = 'Pending'
			AND convoy_id = ?`, convoyID).Scan(&pendingReview)

		var escCount int
		db.QueryRow(`SELECT COUNT(*) FROM Escalations
			WHERE task_id IN (
				SELECT id FROM BountyBoard WHERE type = 'ConvoyReview'
				  AND convoy_id = ?
			)`, convoyID).Scan(&escCount)

		if escCount > 0 {
			// Escalated — terminal state, stop iterating.
			break
		}
		_ = pendingReview
	}

	if got := stub.CallCount(); got > convoyReviewMaxTotalCalls {
		t.Errorf("AUDIT-113/138: total Claude calls (%d) exceeded hard cap (%d) over %d dog iterations — cost-loop protection is broken",
			got, convoyReviewMaxTotalCalls, iterations)
	}
	t.Logf("bounded call count: %d calls across %d iterations (cap=%d)",
		stub.CallCount(), iterations, convoyReviewMaxTotalCalls)
}

// TestFullConvoyLifecycle_AdversarialLLM is the AUDIT-138 companion to
// TestConvoyReview_TotalClaudeCallsBounded — it exercises the full dog
// re-trigger loop with alternating adversarial LLM responses and asserts
// both bounded Claude calls AND convoy reaches terminal state (Completed
// or Escalated). Without the Fix #7 changes, this would loop 50+ times.
func TestFullConvoyLifecycle_AdversarialLLM(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	convoyID := seedDraftPROpenConvoy(t, db)

	// All malformed — forces the parse-failure escalate path to fire.
	stub := withStubCLIRunnerFn(t, func(prompt, tools, dir string, maxTurns int, timeout time.Duration) (string, error) {
		return "still not json", nil
	})

	iterations := 0
	maxIterations := 50
	terminatedOK := false
	for i := 0; i < maxIterations; i++ {
		iterations++
		taskID, _ := QueueConvoyReview(db, convoyID)
		if taskID == 0 {
			// Dog refuses to queue (prior escalation in place) — terminal state.
			terminatedOK = true
			break
		}
		db.Exec(`UPDATE BountyBoard SET status = 'Locked', owner = 'Diplomat-1' WHERE id = ?`, taskID)
		b, _ := store.GetBounty(db, taskID)
		runConvoyReview(context.Background(), db, "Diplomat-1", b, testLogger{})

		var escCount int
		// Fix A (AUDIT-011 read-side): migrated from payload-LIKE to convoy_id equality.
		db.QueryRow(`SELECT COUNT(*) FROM Escalations
			WHERE task_id IN (
				SELECT id FROM BountyBoard WHERE type = 'ConvoyReview'
				  AND convoy_id = ?
			)`, convoyID).Scan(&escCount)
		if escCount > 0 {
			terminatedOK = true
			break
		}
	}

	if !terminatedOK {
		t.Errorf("adversarial LLM lifecycle ran %d iterations without reaching terminal state — Fix #7 self-healing loop is broken", iterations)
	}

	// Under pure-malformed LLM, each pass does exactly 2 Claude calls
	// (initial + 1 retry with critic note) before escalating. 1 pass ×
	// 2 calls = 2 total (the escalation stops the dog from requeuing).
	const hardBound = 4
	if got := stub.CallCount(); got > hardBound {
		t.Errorf("AUDIT-138: full-lifecycle adversarial-LLM Claude call count (%d) exceeded cap (%d) across %d iterations",
			got, hardBound, iterations)
	}
}

// TestFindingFingerprint_IsStable locks the fingerprint identity so a
// future refactor of findingFingerprint (e.g. field ordering, new field)
// doesn't silently change the hash and break pass-to-pass dedup on
// running convoys.
func TestFindingFingerprint_IsStable(t *testing.T) {
	a := convoyReviewFinding{
		Type: "gap", Description: "missing X", Fix: "add X",
		Repo: "api", File: "x.go", Line: 10,
	}
	// The order of findings in the set MUST NOT matter — set fingerprint
	// is computed from sorted per-finding hashes.
	b := convoyReviewFinding{
		Type: "regression", Description: "Y removed", Fix: "restore Y",
		Repo: "api", File: "y.go", Line: 20,
	}
	f1 := findingSetFingerprint([]convoyReviewFinding{a, b})
	f2 := findingSetFingerprint([]convoyReviewFinding{b, a})
	if f1 != f2 {
		t.Errorf("finding order must not change set fingerprint; f1=%s f2=%s", f1, f2)
	}
	// Description casing and whitespace should normalise.
	aUpper := a
	aUpper.Description = "  MISSING X  "
	f3 := findingSetFingerprint([]convoyReviewFinding{aUpper, b})
	if f1 != f3 {
		t.Errorf("normalisation broken: whitespace/casing flipped fingerprint; f1=%s f3=%s", f1, f3)
	}
	// Different line number MUST differ.
	aLineDiff := a
	aLineDiff.Line = 99
	f4 := findingSetFingerprint([]convoyReviewFinding{aLineDiff, b})
	if f1 == f4 {
		t.Errorf("line number not part of fingerprint; different-line findings collide")
	}
	// Empty set → empty fingerprint (so "no findings" never matches a
	// non-empty set).
	if findingSetFingerprint(nil) != "" {
		t.Error("empty set must produce empty fingerprint")
	}

	// Per-finding fingerprints must be deterministic across calls (no
	// time/random state leaking in).
	h1 := findingFingerprint(a)
	h2 := findingFingerprint(a)
	if h1 != h2 {
		t.Errorf("finding fingerprint is not deterministic; h1=%s h2=%s", h1, h2)
	}
	// Fingerprints are at least hex-ish and non-trivially long.
	if len(h1) != 64 { // SHA256 hex
		t.Errorf("expected 64-char sha256 hex, got len=%d val=%q", len(h1), h1)
	}
}

// TestRunConvoyReview_AfterCleanPass_NewFindingsEscalate asserts the
// "clean pass → only verify regressions" invariant. Pass 1 returns
// clean; pass 2 returns a brand-new finding. Pass 2 must escalate rather
// than spawning the new fix task.
func TestRunConvoyReview_AfterCleanPass_NewFindingsEscalate(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	convoyID := seedDraftPROpenConvoy(t, db)
	payload, _ := json.Marshal(convoyReviewPayload{ConvoyID: convoyID})

	// ── Pass 1 — clean ──
	stubConvoyReviewLLM(t, convoyReviewResult{Status: "clean"})
	bounty1 := &store.Bounty{ID: 3001, Type: "ConvoyReview", Payload: string(payload)}
	db.Exec(`INSERT INTO BountyBoard (id, parent_id, target_repo, type, status, payload, convoy_id, priority, created_at)
		VALUES (3001, 0, '', 'ConvoyReview', 'Locked', ?, ?, 5, datetime('now'))`, string(payload), convoyID)
	runConvoyReview(context.Background(), db, "Diplomat-1", bounty1, testLogger{})

	var status string
	db.QueryRow(`SELECT status FROM BountyBoard WHERE id = 3001`).Scan(&status)
	if status != "Completed" {
		t.Fatalf("pass 1 clean: expected Completed, got %s", status)
	}

	// ── Pass 2 — finds something new that wasn't flagged before ──
	stubConvoyReviewLLM(t, convoyReviewResult{
		Status: "needs_work",
		Findings: []convoyReviewFinding{
			{Type: "gap", Description: "newly appeared issue",
				Fix: "handle it", Repo: "api", File: "new.go", Line: 1},
		},
	})
	bounty2 := &store.Bounty{ID: 3002, Type: "ConvoyReview", Payload: string(payload)}
	db.Exec(`INSERT INTO BountyBoard (id, parent_id, target_repo, type, status, payload, convoy_id, priority, created_at)
		VALUES (3002, 0, '', 'ConvoyReview', 'Locked', ?, ?, 5, datetime('now'))`, string(payload), convoyID)
	runConvoyReview(context.Background(), db, "Diplomat-1", bounty2, testLogger{})

	// Pass 2 after clean must escalate, NOT spawn new fix tasks.
	var spawned int
	db.QueryRow(`SELECT COUNT(*) FROM BountyBoard WHERE parent_id = 3002 AND type = 'CodeEdit'`).Scan(&spawned)
	if spawned != 0 {
		t.Errorf("post-clean new-findings gate bypassed: spawned %d fix tasks (expected 0)", spawned)
	}

	db.QueryRow(`SELECT status FROM BountyBoard WHERE id = 3002`).Scan(&status)
	if status != "Failed" {
		t.Errorf("expected Failed (escalated after clean+new findings), got %s", status)
	}

	var escCount int
	db.QueryRow(`SELECT COUNT(*) FROM Escalations WHERE task_id = 3002`).Scan(&escCount)
	if escCount != 1 {
		t.Errorf("expected 1 Escalation, got %d", escCount)
	}
}

// TestStubConvoyReviewLLM_CapturesPrompt verifies the AUDIT-135 fix:
// stubConvoyReviewLLM returns a *stubCLIRunner whose LastPrompt() method
// surfaces the full prompt that was fed to Claude, so tests can assert
// structural markers (convoy_name, convoy_tasks, diff).
func TestStubConvoyReviewLLM_CapturesPrompt(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	convoyID := seedDraftPROpenConvoy(t, db)
	db.Exec(`INSERT INTO BountyBoard (parent_id, target_repo, type, status, payload, convoy_id, priority, created_at)
		VALUES (0, 'api', 'CodeEdit', 'Completed', 'add rate limit patterns', ?, 5, datetime('now'))`, convoyID)

	stub := stubConvoyReviewLLM(t, convoyReviewResult{Status: "clean"})

	payload, _ := json.Marshal(convoyReviewPayload{ConvoyID: convoyID})
	bounty := &store.Bounty{ID: 4001, Type: "ConvoyReview", Payload: string(payload)}
	db.Exec(`INSERT INTO BountyBoard (id, parent_id, target_repo, type, status, payload, convoy_id, priority, created_at)
		VALUES (4001, 0, '', 'ConvoyReview', 'Locked', ?, ?, 5, datetime('now'))`, string(payload), convoyID)

	runConvoyReview(context.Background(), db, "Diplomat-1", bounty, testLogger{})

	prompt := stub.LastPrompt()
	if prompt == "" {
		t.Fatal("AUDIT-135: stub captured no prompt — withStubCLIRunner broke its contract")
	}
	assertConvoyReviewPromptShape(t, prompt)
	// The summarised task payload must also appear so the LLM actually
	// sees what it's reviewing.
	if !strings.Contains(prompt, "rate limit patterns") {
		t.Errorf("AUDIT-135: prompt missing the seeded convoy task payload snippet; summarizeConvoyTasks may be returning empty")
	}
	if stub.CallCount() != 1 {
		t.Errorf("expected 1 Claude call, got %d", stub.CallCount())
	}
}
