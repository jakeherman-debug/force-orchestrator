package agents

import (
	"database/sql"
	"testing"
	"time"

	"force-orchestrator/internal/gh"
	"force-orchestrator/internal/store"
)

// seedStalePRScenario sets up a sub-PR whose created_at is `age` in the past,
// returning the AskBranchPR row fresh from the DB plus the *sql.DB handle.
func seedStalePRScenario(t *testing.T, age time.Duration) (*sql.DB, store.AskBranchPR) {
	t.Helper()
	db := store.InitHolocronDSN(":memory:")
	t.Cleanup(func() { db.Close() })

	convoyID, taskID, _, branchName := setupSubPRScenario(t, db)
	db.Exec(`UPDATE BountyBoard SET status = ?, branch_name = ? WHERE id = ?`, subPRCITaskStatus, branchName, taskID)
	prRowID, err := store.CreateAskBranchPR(db, taskID, convoyID, "api", "https://github.com/acme/api/pull/7701", 7701)
	if err != nil {
		t.Fatalf("create PR: %v", err)
	}
	backdate := time.Now().Add(-age).UTC().Format("2006-01-02 15:04:05")
	db.Exec(`UPDATE AskBranchPRs SET created_at = ? WHERE id = ?`, backdate, prRowID)
	pr := store.GetAskBranchPR(db, prRowID)
	if pr == nil {
		t.Fatalf("reload PR row %d: nil", prRowID)
	}
	return db, *pr
}

// ghChecksStub implements the narrow interface that onSubPRStalled needs for
// testing. Tests use it to control the per-check state returned for diagnosis.
type ghChecksStub struct {
	checks []gh.PRCheck
	state  gh.ChecksState
	err    error
}

func (s *ghChecksStub) PRChecks(cwd, repo string, number int) ([]gh.PRCheck, gh.ChecksState, error) {
	return s.checks, s.state, s.err
}

// TestOnSubPRStalled_AllQueuedChecks_TriggersRerun verifies the central self-
// healing path: if every check is QUEUED with no runner assigned, we push an
// empty commit to re-trigger the check suite and bump the retrigger counter.
// The PR must NOT be escalated yet.
func TestOnSubPRStalled_AllQueuedChecks_TriggersRerun(t *testing.T) {
	db, pr := seedStalePRScenario(t, 3*time.Hour) // over 2h but under hard 6h

	var triggered struct {
		called bool
		branch string
	}
	restore := SetTriggerStalledRerunForTest(func(repoPath, branch, message string) error {
		triggered.called = true
		triggered.branch = branch
		return nil
	})
	defer restore()

	stub := &ghChecksStub{
		checks: []gh.PRCheck{
			{Name: "build", State: "QUEUED", Bucket: "pending"},
			{Name: "test", State: "QUEUED", Bucket: "pending"},
		},
		state: gh.ChecksPending,
	}

	onSubPRStalled(db, stub, pr, testLogger{})

	if !triggered.called {
		t.Fatal("expected empty-commit re-trigger to be called")
	}
	if triggered.branch == "" {
		t.Error("re-trigger should have received a branch name")
	}

	reloaded := store.GetAskBranchPR(db, pr.ID)
	if reloaded.StallRetriggerCount != 1 {
		t.Errorf("expected stall_retrigger_count=1, got %d", reloaded.StallRetriggerCount)
	}

	var taskStatus string
	db.QueryRow(`SELECT status FROM BountyBoard WHERE id = ?`, pr.TaskID).Scan(&taskStatus)
	if taskStatus == "Escalated" {
		t.Error("stuck-runner diagnosis must not escalate; should have re-triggered instead")
	}
	if reloaded.State == "Closed" {
		t.Error("PR must stay Open during re-trigger attempt")
	}
}

// TestOnSubPRStalled_InProgressCheck_WaitsWithoutAction verifies that when at
// least one check is IN_PROGRESS (a runner engaged, CI is just slow), we do
// NOT intervene — no re-trigger push, no escalation. The next tick will check
// again. This protects actively-running CI from being clobbered.
func TestOnSubPRStalled_InProgressCheck_WaitsWithoutAction(t *testing.T) {
	db, pr := seedStalePRScenario(t, 3*time.Hour)

	var rerunCalled bool
	restore := SetTriggerStalledRerunForTest(func(repoPath, branch, message string) error {
		rerunCalled = true
		return nil
	})
	defer restore()

	stub := &ghChecksStub{
		checks: []gh.PRCheck{
			{Name: "build", State: "QUEUED", Bucket: "pending"},
			{Name: "test", State: "IN_PROGRESS", Bucket: "pending"},
		},
		state: gh.ChecksPending,
	}

	onSubPRStalled(db, stub, pr, testLogger{})

	if rerunCalled {
		t.Error("IN_PROGRESS check means CI is slow-but-alive; must not re-trigger")
	}
	reloaded := store.GetAskBranchPR(db, pr.ID)
	if reloaded.StallRetriggerCount != 0 {
		t.Errorf("no re-trigger should have been counted, got %d", reloaded.StallRetriggerCount)
	}
	var taskStatus string
	db.QueryRow(`SELECT status FROM BountyBoard WHERE id = ?`, pr.TaskID).Scan(&taskStatus)
	if taskStatus == "Escalated" {
		t.Error("slow IN_PROGRESS CI must not escalate on first stall tick")
	}
}

// TestOnSubPRStalled_RetriggerCapHit_Escalates verifies the loop cap. After
// we've already re-triggered subPRMaxStallRetriggers times, a further stall
// means GitHub's runner/config is genuinely broken; fall through to escalation.
func TestOnSubPRStalled_RetriggerCapHit_Escalates(t *testing.T) {
	db, pr := seedStalePRScenario(t, 3*time.Hour)
	db.Exec(`UPDATE AskBranchPRs SET stall_retrigger_count = ? WHERE id = ?`,
		subPRMaxStallRetriggers, pr.ID)
	pr.StallRetriggerCount = subPRMaxStallRetriggers

	var rerunCalled bool
	restore := SetTriggerStalledRerunForTest(func(repoPath, branch, message string) error {
		rerunCalled = true
		return nil
	})
	defer restore()

	stub := &ghChecksStub{
		checks: []gh.PRCheck{{Name: "build", State: "QUEUED", Bucket: "pending"}},
		state:  gh.ChecksPending,
	}

	onSubPRStalled(db, stub, pr, testLogger{})

	if rerunCalled {
		t.Error("cap hit: must not issue another re-trigger")
	}
	var taskStatus string
	db.QueryRow(`SELECT status FROM BountyBoard WHERE id = ?`, pr.TaskID).Scan(&taskStatus)
	if taskStatus != "Escalated" {
		t.Errorf("expected Escalated after re-trigger cap reached, got %q", taskStatus)
	}
	reloaded := store.GetAskBranchPR(db, pr.ID)
	if reloaded.State != "Closed" {
		t.Errorf("escalation must close the PR row, got state=%q", reloaded.State)
	}
}

// TestOnSubPRStalled_HardLimitReached_EscalatesRegardless verifies the safety
// ceiling: past subPRCIHardLimit we escalate without diagnosis, even if we
// had re-triggers left. This prevents a persistent QUEUED state from tying up
// a PR forever.
func TestOnSubPRStalled_HardLimitReached_EscalatesRegardless(t *testing.T) {
	db, pr := seedStalePRScenario(t, subPRCIHardLimit+30*time.Minute)

	var rerunCalled bool
	restore := SetTriggerStalledRerunForTest(func(repoPath, branch, message string) error {
		rerunCalled = true
		return nil
	})
	defer restore()

	stub := &ghChecksStub{
		checks: []gh.PRCheck{{Name: "build", State: "QUEUED", Bucket: "pending"}},
		state:  gh.ChecksPending,
	}

	onSubPRStalled(db, stub, pr, testLogger{})

	if rerunCalled {
		t.Error("hard limit reached: must not attempt re-trigger")
	}
	var taskStatus string
	db.QueryRow(`SELECT status FROM BountyBoard WHERE id = ?`, pr.TaskID).Scan(&taskStatus)
	if taskStatus != "Escalated" {
		t.Errorf("expected Escalated past hard limit, got %q", taskStatus)
	}
}

// TestOnSubPRStalled_RetriggerFailure_Escalates asserts that if the empty-commit
// push itself fails (e.g. auth expired, branch gone), we fall through to
// escalation rather than silently dropping the problem.
func TestOnSubPRStalled_RetriggerFailure_Escalates(t *testing.T) {
	db, pr := seedStalePRScenario(t, 3*time.Hour)

	restore := SetTriggerStalledRerunForTest(func(repoPath, branch, message string) error {
		return &triggerFailErr{msg: "git push: auth rejected"}
	})
	defer restore()

	stub := &ghChecksStub{
		checks: []gh.PRCheck{{Name: "build", State: "QUEUED", Bucket: "pending"}},
		state:  gh.ChecksPending,
	}

	onSubPRStalled(db, stub, pr, testLogger{})

	var taskStatus string
	db.QueryRow(`SELECT status FROM BountyBoard WHERE id = ?`, pr.TaskID).Scan(&taskStatus)
	if taskStatus != "Escalated" {
		t.Errorf("re-trigger-failure must escalate (not hide); got %q", taskStatus)
	}
}

type triggerFailErr struct{ msg string }

func (e *triggerFailErr) Error() string { return e.msg }
