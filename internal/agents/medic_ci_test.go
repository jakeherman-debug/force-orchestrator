package agents

import (
	"database/sql"
	"io"
	"log"
	"strings"
	"testing"
	"time"

	"force-orchestrator/internal/store"
)

// ── CI Circuit Breaker ───────────────────────────────────────────────────────

func TestCIBreaker_InitiallyClosed(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	if IsCIBreakerOpen(db, "api") {
		t.Errorf("breaker must start closed")
	}
}

func TestCIBreaker_OpensAtThreshold(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	// Fire exactly ciEnvThreshold failures.
	for i := 0; i < ciEnvThreshold-1; i++ {
		if recordCIEnvironmentalFailure(db, "api") {
			t.Fatalf("breaker should not open before threshold (i=%d)", i)
		}
		if IsCIBreakerOpen(db, "api") {
			t.Errorf("breaker open too early at i=%d", i)
		}
	}
	if !recordCIEnvironmentalFailure(db, "api") {
		t.Fatalf("breaker should open at threshold")
	}
	if !IsCIBreakerOpen(db, "api") {
		t.Errorf("breaker must be open after threshold")
	}
}

func TestCIBreaker_IsPerRepo(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	for i := 0; i < ciEnvThreshold; i++ {
		recordCIEnvironmentalFailure(db, "api")
	}
	if !IsCIBreakerOpen(db, "api") {
		t.Fatal("api breaker should be open")
	}
	if IsCIBreakerOpen(db, "monolith") {
		t.Errorf("monolith breaker must stay closed; events on api should not affect it")
	}
}

func TestCIBreaker_WindowResetsCount(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	// Put some count into the window.
	for i := 0; i < ciEnvThreshold-2; i++ {
		recordCIEnvironmentalFailure(db, "api")
	}
	// Manually roll the window_start back beyond ciEnvWindow.
	oldStart := time.Now().UTC().Add(-2 * time.Hour).Format(time.RFC3339)
	store.SetConfig(db, "circuit_breaker:api:window_start", oldStart)
	// The next failure should reset the counter to 1.
	if recordCIEnvironmentalFailure(db, "api") {
		t.Errorf("single failure after window reset must not trip breaker")
	}
	if getCIEnvCount(db, "api") != 1 {
		t.Errorf("count after reset should be 1, got %d", getCIEnvCount(db, "api"))
	}
}

func TestResetCIBreaker_ClearsState(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	for i := 0; i < ciEnvThreshold; i++ {
		recordCIEnvironmentalFailure(db, "api")
	}
	if !IsCIBreakerOpen(db, "api") {
		t.Fatal("precondition: breaker should be open")
	}
	ResetCIBreaker(db, "api")
	if IsCIBreakerOpen(db, "api") {
		t.Errorf("ResetCIBreaker should close the breaker")
	}
	if getCIEnvCount(db, "api") != 0 {
		t.Errorf("count should be zero after reset")
	}
}

// ── CIFailureTriage handler ─────────────────────────────────────────────────

func setupTriageScenario(t *testing.T, db *sql.DB) (pr *store.AskBranchPR, payload ciTriagePayload, taskID int) {
	t.Helper()
	store.AddRepo(db, "api", "/tmp/api", "")
	// Use a unique convoy name so multiple setup calls in a single test don't
	// conflict on Convoys.name UNIQUE.
	convoyName := "[" + itoaInt(int(time.Now().UnixNano()%1000000)) + "] t"
	convoyID, _ := store.CreateConvoy(db, convoyName)
	tID, _ := store.AddConvoyTask(db, 0, "api", "fix thing", convoyID, 0, "Pending")
	store.SetBranchName(db, tID, "agent/R2-D2/task-"+itoaInt(tID))
	db.Exec(`UPDATE BountyBoard SET status = ? WHERE id = ?`, subPRCITaskStatus, tID)
	// PR number derives from taskID so every setup call yields a unique
	// (repo, pr_number) pair and the AskBranchPRs UNIQUE constraint never trips.
	prNumber := 1000 + tID
	prRowID, _ := store.CreateAskBranchPR(db, tID, convoyID, "api",
		"https://github.com/acme/api/pull/"+itoaInt(prNumber), prNumber)
	_ = store.UpdateAskBranchPRChecks(db, prRowID, "Failure")
	_, _ = store.IncrementAskBranchPRFailureCount(db, prRowID) // count=1
	pr = store.GetAskBranchPR(db, prRowID)
	payload = ciTriagePayload{
		SubPRRowID: prRowID, Repo: "api", PRNumber: prNumber,
		Branch: "agent/R2-D2/task-" + itoaInt(tID), TaskID: tID,
	}
	return pr, payload, tID
}

func itoaInt(i int) string {
	var b strings.Builder
	if i == 0 {
		return "0"
	}
	neg := i < 0
	if neg {
		i = -i
	}
	var digits [20]byte
	n := 0
	for i > 0 {
		digits[n] = byte('0' + i%10)
		n++
		i /= 10
	}
	if neg {
		b.WriteByte('-')
	}
	for j := n - 1; j >= 0; j-- {
		b.WriteByte(digits[j])
	}
	return b.String()
}

func TestRunMedicCITriage_RealBugSpawnsFixTask(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	_, payload, taskID := setupTriageScenario(t, db)

	withStubCLIRunner(t, `{"classification":"RealBug","diagnosis":"test X asserts wrong count","fix_guidance":"update assertion in foo_test.go to expect 2","operator_note":""}`, nil)

	triageID, _ := QueueCIFailureTriage(db, payload)
	b, _ := store.GetBounty(db, triageID)
	runMedicCITriage(db, "Medic-Bacta", b, testLogger{})

	updated, _ := store.GetBounty(db, triageID)
	if updated.Status != "Completed" {
		t.Errorf("triage task should complete: %q", updated.Status)
	}

	// A CodeEdit fix task should be spawned with parent_id = original task.
	var fixCount int
	db.QueryRow(`SELECT COUNT(*) FROM BountyBoard WHERE type = 'CodeEdit' AND parent_id = ? AND status = 'Pending'`, taskID).Scan(&fixCount)
	if fixCount != 1 {
		t.Errorf("expected 1 CodeEdit fix task, got %d", fixCount)
	}
	// The fix task should have the original branch name so astromech resumes it.
	var fixBranch string
	db.QueryRow(`SELECT branch_name FROM BountyBoard WHERE type = 'CodeEdit' AND parent_id = ? LIMIT 1`, taskID).Scan(&fixBranch)
	if !strings.HasPrefix(fixBranch, "agent/R2-D2/task-") {
		t.Errorf("fix task should inherit agent branch, got %q", fixBranch)
	}
}

func TestRunMedicCITriage_FlakyRetriesWithoutFix(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	_, payload, taskID := setupTriageScenario(t, db)

	withStubCLIRunner(t, `{"classification":"Flaky","diagnosis":"intermittent timeout","fix_guidance":"","operator_note":""}`, nil)
	triageID, _ := QueueCIFailureTriage(db, payload)
	b, _ := store.GetBounty(db, triageID)
	runMedicCITriage(db, "Medic-Bacta", b, testLogger{})

	// No fix task should be spawned.
	var fixCount int
	db.QueryRow(`SELECT COUNT(*) FROM BountyBoard WHERE type = 'CodeEdit' AND parent_id = ?`, taskID).Scan(&fixCount)
	if fixCount != 0 {
		t.Errorf("Flaky should not spawn fix tasks, got %d", fixCount)
	}
	// Checks state should be reset to Pending for re-evaluation.
	pr := store.GetAskBranchPR(db, payload.SubPRRowID)
	if pr.ChecksState != "Pending" {
		t.Errorf("Flaky should reset checks_state to Pending, got %q", pr.ChecksState)
	}
}

func TestRunMedicCITriage_FlakyPromotedEscalatesAtCap(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	_, payload, taskID := setupTriageScenario(t, db)
	// Crank failure_count up to cap. Flaky → promoted to RealBug → RealBug at
	// cap escalates (we've already fixed it N times, don't try again).
	for i := 0; i < medicRetriggerCap; i++ {
		_, _ = store.IncrementAskBranchPRFailureCount(db, payload.SubPRRowID)
	}

	withStubCLIRunner(t, `{"classification":"Flaky","diagnosis":"looks flaky again","fix_guidance":"","operator_note":""}`, nil)
	triageID, _ := QueueCIFailureTriage(db, payload)
	b, _ := store.GetBounty(db, triageID)
	runMedicCITriage(db, "Medic-Bacta", b, testLogger{})

	updated, _ := store.GetBounty(db, taskID)
	if updated.Status != "Escalated" {
		t.Errorf("after cap, Flaky→RealBug should escalate, got status %q", updated.Status)
	}
	// No fix task because we're past the cap.
	var fixCount int
	db.QueryRow(`SELECT COUNT(*) FROM BountyBoard WHERE type = 'CodeEdit' AND parent_id = ? AND status = 'Pending'`, taskID).Scan(&fixCount)
	if fixCount != 0 {
		t.Errorf("past cap, should NOT spawn more fix tasks, got %d", fixCount)
	}
}

func TestRunMedicCITriage_EnvironmentalTripsBreaker(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	// Simulate multiple Environmental classifications until the breaker trips.
	withStubCLIRunner(t, `{"classification":"Environmental","diagnosis":"master broken","fix_guidance":"","operator_note":""}`, nil)

	for i := 0; i < ciEnvThreshold; i++ {
		_, payload, _ := setupTriageScenario(t, db)
		triageID, _ := QueueCIFailureTriage(db, payload)
		b, _ := store.GetBounty(db, triageID)
		runMedicCITriage(db, "Medic-Bacta", b, testLogger{})
	}
	if !IsCIBreakerOpen(db, "api") {
		t.Errorf("breaker should be open after %d Environmental failures", ciEnvThreshold)
	}
}

func TestRunMedicCITriage_BranchProtectionEscalates(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	_, payload, taskID := setupTriageScenario(t, db)
	withStubCLIRunner(t, `{"classification":"BranchProtection","diagnosis":"required check CI/jenkins missing","fix_guidance":"","operator_note":"Configure CI/jenkins as a required status check"}`, nil)

	triageID, _ := QueueCIFailureTriage(db, payload)
	b, _ := store.GetBounty(db, triageID)
	runMedicCITriage(db, "Medic-Bacta", b, testLogger{})

	updated, _ := store.GetBounty(db, taskID)
	if updated.Status != "Escalated" {
		t.Errorf("BranchProtection must escalate, got %q", updated.Status)
	}
	var escCount int
	db.QueryRow(`SELECT COUNT(*) FROM Escalations WHERE task_id = ?`, taskID).Scan(&escCount)
	if escCount == 0 {
		t.Error("expected Escalation row")
	}
}

func TestRunMedicCITriage_UnfixableEscalates(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	_, payload, taskID := setupTriageScenario(t, db)
	withStubCLIRunner(t, `{"classification":"Unfixable","diagnosis":"architectural conflict","fix_guidance":"","operator_note":"Need human to decide"}`, nil)

	triageID, _ := QueueCIFailureTriage(db, payload)
	b, _ := store.GetBounty(db, triageID)
	runMedicCITriage(db, "Medic-Bacta", b, testLogger{})

	updated, _ := store.GetBounty(db, taskID)
	if updated.Status != "Escalated" {
		t.Errorf("Unfixable must escalate, got %q", updated.Status)
	}
}

func TestRunMedicCITriage_MalformedJSONEscalates(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	_, payload, taskID := setupTriageScenario(t, db)
	withStubCLIRunner(t, `not json at all`, nil)

	triageID, _ := QueueCIFailureTriage(db, payload)
	b, _ := store.GetBounty(db, triageID)
	runMedicCITriage(db, "Medic-Bacta", b, testLogger{})

	updated, _ := store.GetBounty(db, taskID)
	if updated.Status != "Escalated" {
		t.Errorf("malformed Claude output should fall through to escalation, got %q", updated.Status)
	}
}

func TestRunMedicCITriage_ClaudeErrorEscalates(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	_, payload, taskID := setupTriageScenario(t, db)
	withStubCLIRunner(t, "", &stubErr{msg: "claude unavailable"})

	triageID, _ := QueueCIFailureTriage(db, payload)
	b, _ := store.GetBounty(db, triageID)
	runMedicCITriage(db, "Medic-Bacta", b, testLogger{})

	updated, _ := store.GetBounty(db, taskID)
	if updated.Status != "Escalated" {
		t.Errorf("Claude failure should escalate parent task, got %q", updated.Status)
	}
}

type stubErr struct{ msg string }

func (e *stubErr) Error() string { return e.msg }

func TestRunMedicCITriage_SkipsAlreadyMergedPR(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	_, payload, taskID := setupTriageScenario(t, db)
	// The PR merged between triage being queued and Medic claiming it.
	_ = store.MarkAskBranchPRMerged(db, payload.SubPRRowID)

	triageID, _ := QueueCIFailureTriage(db, payload)
	b, _ := store.GetBounty(db, triageID)
	runMedicCITriage(db, "Medic-Bacta", b, testLogger{})

	// Triage task completes as a no-op.
	updated, _ := store.GetBounty(db, triageID)
	if updated.Status != "Completed" {
		t.Errorf("should complete as no-op when PR already merged, got %q", updated.Status)
	}
	// No escalation, no fix task, parent task untouched — verify by counting
	// side effects. A broken implementation that escalated despite the merge
	// would leave evidence here.
	var escCount, fixCount int
	db.QueryRow(`SELECT COUNT(*) FROM Escalations WHERE task_id = ?`, taskID).Scan(&escCount)
	db.QueryRow(`SELECT COUNT(*) FROM BountyBoard WHERE parent_id = ? AND type = 'CodeEdit'`, taskID).Scan(&fixCount)
	if escCount != 0 {
		t.Errorf("merged-PR no-op path must NOT create escalations, got %d", escCount)
	}
	if fixCount != 0 {
		t.Errorf("merged-PR no-op path must NOT spawn fix tasks, got %d", fixCount)
	}
	// Parent task status must not have changed (was Pending originally).
	parent, _ := store.GetBounty(db, taskID)
	if parent.Status == "Escalated" {
		t.Errorf("parent task must not be escalated on merged-PR no-op, got %q", parent.Status)
	}
}

// ── Jedi Council respects circuit breaker ───────────────────────────────────

func TestJediCouncilApproval_RequeuesWhenBreakerOpen(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	convoyID, taskID, _, _ := setupSubPRScenario(t, db)
	_ = convoyID

	// Trip the breaker for this repo.
	for i := 0; i < ciEnvThreshold; i++ {
		recordCIEnvironmentalFailure(db, "api")
	}
	if !IsCIBreakerOpen(db, "api") {
		t.Fatal("breaker should be open (precondition)")
	}

	withStubCLIRunner(t, `{"approved":true,"feedback":""}`, nil)
	logger := log.New(io.Discard, "", 0)
	b, _ := store.GetBounty(db, taskID)
	runCouncilTask(db, "Council-Yoda", b, logger)

	// Task should NOT be AwaitingSubPRCI; it must be back at AwaitingCouncilReview
	// for a later retry after the breaker closes.
	after, _ := store.GetBounty(db, taskID)
	if after.Status != "AwaitingCouncilReview" {
		t.Errorf("task should be requeued while breaker open, got %q", after.Status)
	}
	// No AskBranchPR should have been created.
	if store.GetAskBranchPRByTask(db, taskID) != nil {
		t.Errorf("PR must NOT be created when breaker is open")
	}
}
