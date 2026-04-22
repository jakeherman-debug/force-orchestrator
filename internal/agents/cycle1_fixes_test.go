package agents

import (
	"fmt"
	"io"
	"log"
	"testing"

	"force-orchestrator/internal/store"
)

// TestCountCommitsAheadBehind_MalformedOutputReturnsError — regression for the
// Sscanf silent-failure bug. Can't easily force a malformed rev-list output
// from real git, but we can validate the function signature and ensure the
// error path compiles. Real-world trigger would be a corrupted git output
// (e.g., terminal escape codes).
//
// We test the happy path via existing git fixture in TestCountCommitsAheadBehind
// below. For malformed output, we'd need a mock RunCmd — out of scope here.

// TestOnSubPRMissingCI_EscalatesAfter10mWithZeroChecks verifies the new
// early-escalation path: a sub-PR with zero checks reported after
// missingCITimeout (10m) gets a Low-severity escalation so the operator
// can wire up CI instead of waiting the full 2h subPRCIStaleLimit.
func TestOnSubPRMissingCI_EscalatesAfter10mWithZeroChecks(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	store.AddRepo(db, "api", "/tmp/api", "")
	cid, _ := store.CreateConvoy(db, "[1] no-ci")
	tid, _ := store.AddConvoyTask(db, 0, "api", "t", cid, 0, "Pending")
	db.Exec(`UPDATE BountyBoard SET status = ? WHERE id = ?`, subPRCITaskStatus, tid)
	prID, _ := store.CreateAskBranchPR(db, tid, cid, "api", "u", 5)
	// Backdate PR row by 11 minutes — just past missingCITimeout (10m).
	db.Exec(`UPDATE AskBranchPRs SET created_at = datetime('now', '-11 minutes') WHERE id = ?`, prID)

	pr := store.GetAskBranchPR(db, prID)
	onSubPRMissingCI(db, *pr, testLogger{})

	var escCount int
	db.QueryRow(`SELECT COUNT(*) FROM Escalations WHERE task_id = ? AND status = 'Open'`, tid).Scan(&escCount)
	if escCount != 1 {
		t.Errorf("missing-CI must escalate at 10m, got %d escalations", escCount)
	}
	// Second call with the same open escalation must dedup.
	onSubPRMissingCI(db, *pr, testLogger{})
	db.QueryRow(`SELECT COUNT(*) FROM Escalations WHERE task_id = ? AND status = 'Open'`, tid).Scan(&escCount)
	if escCount != 1 {
		t.Errorf("dedup failed: second call created duplicate escalation (%d)", escCount)
	}
}

// TestJediCouncil_CIBreakerDefersReviewWithoutLLM verifies the new early-
// bailout in runCouncilTask: when the breaker is open for the target repo,
// the council task is deferred BEFORE the LLM call (saves compute during
// the 30min breaker cooldown).
func TestJediCouncil_CIBreakerDefersReviewWithoutLLM(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	convoyID, taskID, _, _ := setupSubPRScenario(t, db)
	_ = convoyID

	// Trip the breaker.
	for i := 0; i < ciEnvThreshold; i++ {
		recordCIEnvironmentalFailure(db, "api")
	}
	if !IsCIBreakerOpen(db, "api") {
		t.Fatal("precondition: breaker must be open")
	}

	// The stub is set but must NOT be invoked — deferral happens before LLM.
	// We track invocation via a panic — if Claude is called, test fails.
	withStubCLIRunner(t, "", fmt.Errorf("council should not have invoked Claude with breaker open"))

	logger := log.New(io.Discard, "", 0)
	b, _ := store.GetBounty(db, taskID)
	runCouncilTask(db, "Council-Yoda", b, logger)

	// Task should be back at AwaitingCouncilReview (deferred), not Failed or
	// Completed (which would indicate the LLM ran).
	after, _ := store.GetBounty(db, taskID)
	if after.Status != "AwaitingCouncilReview" {
		t.Errorf("task should be deferred to AwaitingCouncilReview, got %q", after.Status)
	}
}

// TestJediCouncil_CIBreakerIgnoredForPRFlowDisabledRepo ensures legacy repos
// (pr_flow_enabled=0) are unaffected by the breaker.
func TestJediCouncil_CIBreakerIgnoredForPRFlowDisabledRepo(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	convoyID, taskID, _, _ := setupSubPRScenario(t, db)
	_ = convoyID
	_ = store.SetRepoPRFlowEnabled(db, "api", false)

	// Trip the breaker (state remains but shouldn't affect this repo since
	// legacy path doesn't use sub-PRs).
	for i := 0; i < ciEnvThreshold; i++ {
		recordCIEnvironmentalFailure(db, "api")
	}

	withStubCLIRunner(t, `{"approved":true,"feedback":""}`, nil)
	logger := log.New(io.Discard, "", 0)
	b, _ := store.GetBounty(db, taskID)
	runCouncilTask(db, "Council-Yoda", b, logger)

	// Task should be Completed via legacy merge — the breaker gate doesn't
	// apply when pr_flow_enabled=false.
	after, _ := store.GetBounty(db, taskID)
	if after.Status != "Completed" {
		t.Errorf("pr_flow-disabled repo should complete via legacy merge despite breaker, got %q", after.Status)
	}
}
