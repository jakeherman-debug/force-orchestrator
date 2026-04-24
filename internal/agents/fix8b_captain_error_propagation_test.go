package agents

import (
	"bytes"
	"log"
	"os/exec"
	"strings"
	"testing"

	"force-orchestrator/internal/store"
)

// Fix #8b — Captain error-propagation coverage.
//
// runCaptainTask is called from a Spawn loop and cannot propagate an
// error up (there's no receiver to act on it). The Fix #8b sweep over
// captain.go therefore converts each `_ = store.FailBounty(...)` /
// `_ = store.UpdateBountyStatus(...)` marker into an explicit
// logger.Printf with a recovery hint — the operator can see the DB
// write failure in the log stream and the stale-lock detector will
// re-evaluate on the next sweep.
//
// This test forces the "unknown target repository" path by giving the
// task a target_repo that isn't registered, then drops the BountyBoard
// table mid-call so the follow-up FailBounty itself fails. Pre-Fix
// #8b the failure was silent; post-fix there is a logger.Printf line
// containing "stale-lock detector will recover".

// TestFix8b_Captain_UnknownRepo_FailBountyFailure_LogsRecoveryHint
// exercises the early-exit path in runCaptainTask where the repo isn't
// registered. We drop BountyBoard after the claim so FailBounty's
// UPDATE fails; the post-Fix #8b behavior is to log the write failure
// with the stale-lock recovery hint, NOT to silently return.
func TestFix8b_Captain_UnknownRepo_FailBountyFailure_LogsRecoveryHint(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not found")
	}
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	id := store.AddBounty(db, 0, "CodeEdit", "fix bug")
	db.Exec(`UPDATE BountyBoard SET status = 'AwaitingCaptainReview', target_repo = 'ghost-repo' WHERE id = ?`, id)
	b, _ := store.GetBounty(db, id)

	// Drop BountyBoard so the runCaptainTask FailBounty path fails.
	// runCaptainTask calls GetRepoPath (Repositories table, not
	// BountyBoard) first; when the repo is unknown, it tries to
	// FailBounty the task. That FailBounty hits the dropped table
	// and returns an error — the Fix #8b conversion must log a
	// recovery hint instead of silently eating the error.
	if _, err := db.Exec(`DROP TABLE BountyBoard`); err != nil {
		t.Fatalf("drop BountyBoard setup failed: %v", err)
	}

	var buf bytes.Buffer
	logger := log.New(&buf, "", 0)
	withStubCLIRunner(t, "", nil)
	runCaptainTask(db, "Captain-Rex", b, logger)

	logged := buf.String()
	if !strings.Contains(logged, "stale-lock detector will recover") {
		t.Errorf("expected Fix #8b recovery-hint log line when FailBounty fails, got:\n%s", logged)
	}
	if !strings.Contains(logged, "FailBounty write failed") {
		t.Errorf("expected Fix #8b to log the FailBounty write failure explicitly, got:\n%s", logged)
	}
}

// TestFix8b_Captain_EscalateFallback_LogsRecoveryHint
// verifies that when the LLM returns "escalate", Captain calls
// CreateEscalation, and if that fails we fall back to FailBounty +
// operator mail (instead of silently eating the error as in pre-Fix
// #8b). We force CreateEscalation to fail by dropping the Escalations
// table before the captain runs.
func TestFix8b_Captain_EscalateFallback_LogsRecoveryHint(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not found")
	}
	repoDir := initTestRepo(t)
	branchName := setupBranchWithCommit(t, repoDir, "agent/Captain-Rex/task-fix8b-esc")

	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	store.AddRepo(db, "myrepo", repoDir, "test")
	convoyID, _ := store.CreateConvoy(db, "escalate-fallback convoy")

	id := store.AddBounty(db, 0, "CodeEdit", "fix bug")
	db.Exec(`UPDATE BountyBoard SET status = 'AwaitingCaptainReview', target_repo = 'myrepo', branch_name = ?, convoy_id = ? WHERE id = ?`,
		branchName, convoyID, id)
	b, _ := store.GetBounty(db, id)

	// Drop Escalations so CreateEscalation's INSERT fails.
	if _, err := db.Exec(`DROP TABLE Escalations`); err != nil {
		t.Fatalf("drop Escalations setup failed: %v", err)
	}

	ruling := `{"decision":"escalate","feedback":"plan fundamentally broken","task_updates":[],"new_tasks":[],"rejected_files":[]}`
	withStubCLIRunner(t, ruling, nil)

	var buf bytes.Buffer
	logger := log.New(&buf, "", 0)
	runCaptainTask(db, "Captain-Rex", b, logger)

	logged := buf.String()
	// Post-Fix #8b: CreateEscalation failure must not be silent.
	// Captain logs the fall-back path to FailBounty + operator mail.
	if !strings.Contains(logged, "CreateEscalation failed") {
		t.Errorf("expected Fix #8b 'CreateEscalation failed' log line, got:\n%s", logged)
	}
	if !strings.Contains(logged, "falling back to FailBounty") {
		t.Errorf("expected Fix #8b fall-back log line mentioning FailBounty, got:\n%s", logged)
	}

	// The task should either be Failed (if the fallback FailBounty
	// succeeded) or still in some non-Escalated state — critically,
	// it must NOT be silently marked Escalated without an
	// Escalations row, which was the AUDIT-041 defect.
	b, _ = store.GetBounty(db, id)
	if b.Status == "Escalated" {
		t.Errorf("AUDIT-041 REGRESSION: task ended in Escalated without an Escalations row — status=%q", b.Status)
	}
}
