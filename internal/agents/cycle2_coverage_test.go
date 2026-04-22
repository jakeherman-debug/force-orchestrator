package agents

import (
	"database/sql"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"force-orchestrator/internal/gh"
	igit "force-orchestrator/internal/git"
	"force-orchestrator/internal/store"
)

// ── Cycle 2 additions: paths flagged as NOT-COVERED in the branch audit ──────

// TestRunCreateAskBranch_PartialFailureFailsTaskButKeepsSuccessfulRows
// exercises the multi-repo partial-failure path: if one repo's git push fails
// but another's succeeds, the task FailBountys overall but the successful
// ConvoyAskBranch row is retained so retries skip it.
func TestRunCreateAskBranch_PartialFailureFailsTaskButKeepsSuccessfulRows(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	// Repo A: real origin + clone — CreateAskBranch will succeed.
	wtA, _ := makeOriginAndClone(t)
	store.AddRepo(db, "api", wtA, "")
	_ = store.SetRepoRemoteInfo(db, "api", "https://github.com/acme/api.git", "main")

	// Repo B: no-remote clone (git push will fail) — CreateAskBranch fails on push.
	wtB := t.TempDir()
	exec.Command("git", "init", "-q", "-b", "main", wtB).Run()
	exec.Command("git", "-C", wtB, "config", "user.email", "t@t").Run()
	exec.Command("git", "-C", wtB, "config", "user.name", "Test").Run()
	os.WriteFile(filepath.Join(wtB, "README"), []byte("hi"), 0644)
	exec.Command("git", "-C", wtB, "add", ".").Run()
	exec.Command("git", "-C", wtB, "commit", "-q", "-m", "initial").Run()
	// No `remote add origin` — push will fail with "no such remote".
	store.AddRepo(db, "broken", wtB, "")
	_ = store.SetRepoRemoteInfo(db, "broken", "https://github.com/acme/broken.git", "main")

	cid, _ := store.CreateConvoy(db, "[1] partial")
	_, _ = store.AddConvoyTask(db, 0, "api", "t1", cid, 0, "Pending")
	_, _ = store.AddConvoyTask(db, 0, "broken", "t2", cid, 0, "Pending")

	taskID, _ := QueueCreateAskBranch(db, cid)
	b, _ := store.GetBounty(db, taskID)
	runCreateAskBranch(db, b, testLogger{})

	// Task is Failed (partial success triggers FailBounty per the handler).
	updated, _ := store.GetBounty(db, taskID)
	if updated.Status != "Failed" {
		t.Errorf("partial failure must FailBounty, got %q (error_log=%s)", updated.Status, updated.Owner)
	}

	// Successful repo A has its ConvoyAskBranch row; broken repo B does not.
	abA := store.GetConvoyAskBranch(db, cid, "api")
	abB := store.GetConvoyAskBranch(db, cid, "broken")
	if abA == nil {
		t.Errorf("successful repo should have ConvoyAskBranch row preserved for retry idempotency")
	}
	if abB != nil {
		t.Errorf("failed repo should have NO ConvoyAskBranch row: %+v", abB)
	}
}

// TestRunCreateAskBranch_HappyPath_OriginHasBranch strengthens the happy-path
// assertion by verifying the branch actually lands on origin (not just the DB
// row). Catches the "DB row written but push silently failed" scenario.
func TestRunCreateAskBranch_HappyPath_OriginHasBranch(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	wt, _ := makeOriginAndClone(t)
	store.AddRepo(db, "api", wt, "")
	_ = store.SetRepoRemoteInfo(db, "api", "https://github.com/acme/api.git", "main")

	cid, _ := store.CreateConvoy(db, "[1] origin-check")
	_, _ = store.AddConvoyTask(db, 0, "api", "t", cid, 0, "Pending")

	taskID, _ := QueueCreateAskBranch(db, cid)
	b, _ := store.GetBounty(db, taskID)
	runCreateAskBranch(db, b, testLogger{})

	ab := store.GetConvoyAskBranch(db, cid, "api")
	if ab == nil {
		t.Fatal("ConvoyAskBranch row not created")
	}
	// Verify the branch is actually reachable on origin. Pull the origin URL
	// from the registered repo and ls-remote it.
	remoteOut, _ := exec.Command("git", "-C", wt, "remote", "get-url", "origin").Output()
	origin := strings.TrimSpace(string(remoteOut))
	out, err := exec.Command("git", "ls-remote", origin, ab.AskBranch).CombinedOutput()
	if err != nil {
		t.Fatalf("ls-remote: %v (%s)", err, out)
	}
	if !strings.Contains(string(out), ab.AskBranch) {
		t.Errorf("branch %q missing from origin; ls-remote output: %q", ab.AskBranch, string(out))
	}
}

// TestRunCleanupAskBranch_NoRowsIsNoOp exercises the path where a cleanup
// task is queued for a convoy that has no ConvoyAskBranch rows (already
// cleaned, or legacy convoy). Should complete as no-op, not fail.
func TestRunCleanupAskBranch_NoRowsIsNoOp(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	cid, _ := store.CreateConvoy(db, "[1] nothing")
	taskID, _ := QueueCleanupAskBranch(db, cid)
	b, _ := store.GetBounty(db, taskID)
	runCleanupAskBranch(db, b, testLogger{})

	updated, _ := store.GetBounty(db, taskID)
	if updated.Status != "Completed" {
		t.Errorf("cleanup with no rows should complete as no-op, got %q", updated.Status)
	}
}

// TestRunMedicCITriage_InvalidPayloadFails — previously flagged as not covered.
func TestRunMedicCITriage_InvalidPayloadFails(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	res, _ := db.Exec(`INSERT INTO BountyBoard
		(parent_id, target_repo, type, status, payload, created_at)
		VALUES (0, 'api', 'CIFailureTriage', 'Pending', 'not-json', datetime('now'))`)
	id, _ := res.LastInsertId()
	b, _ := store.GetBounty(db, int(id))
	runMedicCITriage(db, "Medic-Bacta", b, testLogger{})
	updated, _ := store.GetBounty(db, int(id))
	if updated.Status != "Failed" {
		t.Errorf("invalid payload must fail task, got %q", updated.Status)
	}
}

// TestRunMedicCITriage_SubPRRowMissingFails — previously flagged.
func TestRunMedicCITriage_SubPRRowMissingFails(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	// Payload references sub_pr_row_id=999 which doesn't exist.
	taskID, _ := QueueCIFailureTriage(db, ciTriagePayload{
		SubPRRowID: 999, Repo: "api", PRNumber: 1,
		Branch: "agent/x/task-1", TaskID: 1,
	})
	b, _ := store.GetBounty(db, taskID)
	runMedicCITriage(db, "Medic-Bacta", b, testLogger{})
	updated, _ := store.GetBounty(db, taskID)
	if updated.Status != "Failed" {
		t.Errorf("missing sub-PR row must fail task, got %q", updated.Status)
	}
}

// TestHandleSubPRPoll_PRViewErrorLeavesStateUntouched: if gh pr view returns
// an error (transient network, auth), the code should log and return without
// transitioning state. Regression protection.
func TestHandleSubPRPoll_PRViewErrorLeavesStateUntouched(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	convoyID, taskID, _, _ := setupSubPRScenario(t, db)
	db.Exec(`UPDATE BountyBoard SET status = ? WHERE id = ?`, subPRCITaskStatus, taskID)
	prRowID, _ := store.CreateAskBranchPR(db, taskID, convoyID, "api", "u", 5)

	installGHStub(t, map[string]ghStubResp{
		"pr view 5": {stderr: "transient network", err: fmt.Errorf("exit 1")},
	})

	ghc := newGHClient()
	pr := store.GetAskBranchPR(db, prRowID)
	handleSubPRPoll(db, ghc, *pr, testLogger{})

	// State must NOT advance on transient view errors.
	after, _ := store.GetBounty(db, taskID)
	if after.Status != subPRCITaskStatus {
		t.Errorf("task should remain AwaitingSubPRCI on view error, got %q", after.Status)
	}
	pr = store.GetAskBranchPR(db, prRowID)
	if pr.State != "Open" {
		t.Errorf("PR row state should stay Open on transient error, got %q", pr.State)
	}
}

// TestRunShipConvoy_ConvoyNotFoundFails — previously flagged.
func TestRunShipConvoy_ConvoyNotFoundFails(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	// Queue a ShipConvoy for a convoy that doesn't exist.
	taskID, _ := QueueShipConvoy(db, 9999)
	b, _ := store.GetBounty(db, taskID)
	runShipConvoy(db, "Diplomat", b, testLogger{})
	updated, _ := store.GetBounty(db, taskID)
	if updated.Status != "Failed" {
		t.Errorf("missing convoy must fail task, got %q", updated.Status)
	}
}

// TestRunShipConvoy_InvalidPayloadFails — previously flagged.
func TestRunShipConvoy_InvalidPayloadFails(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	res, _ := db.Exec(`INSERT INTO BountyBoard
		(parent_id, target_repo, type, status, payload, created_at)
		VALUES (0, '', 'ShipConvoy', 'Pending', 'not-json', datetime('now'))`)
	id, _ := res.LastInsertId()
	b, _ := store.GetBounty(db, int(id))
	runShipConvoy(db, "Diplomat", b, testLogger{})
	updated, _ := store.GetBounty(db, int(id))
	if updated.Status != "Failed" {
		t.Errorf("invalid payload must fail, got %q", updated.Status)
	}
}

// TestCountCommitsAheadBehind_HappyPath verifies the parse path with real git.
func TestCountCommitsAheadBehind_HappyPath(t *testing.T) {
	wt, _ := makeOriginAndClone(t)
	// Cut a branch, add a commit, compare.
	run := func(args ...string) {
		cmd := exec.Command("git", append([]string{"-C", wt}, args...)...)
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=T", "GIT_AUTHOR_EMAIL=t@t",
			"GIT_COMMITTER_NAME=T", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %s", args, out)
		}
	}
	run("checkout", "-b", "feature")
	os.WriteFile(filepath.Join(wt, "f.txt"), []byte("x"), 0644)
	run("add", ".")
	run("commit", "-m", "feat")
	run("push", "-u", "origin", "feature")

	// feature is 1 ahead, 0 behind origin/main.
	behind, ahead, err := countCommitsAheadBehind(wt, "feature", "main")
	if err != nil {
		t.Fatal(err)
	}
	if behind != 0 || ahead != 1 {
		t.Errorf("expected (0, 1), got (%d, %d)", behind, ahead)
	}
}

// TestSubPRStateStringTransition_PersistsOnChange verifies the observable side
// effect when the dog sees a state change: checks_state DB column is updated.
func TestSubPRStateStringTransition_PersistsOnChange(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	convoyID, taskID, _, _ := setupSubPRScenario(t, db)
	db.Exec(`UPDATE BountyBoard SET status = ? WHERE id = ?`, subPRCITaskStatus, taskID)
	prRowID, _ := store.CreateAskBranchPR(db, taskID, convoyID, "api", "u", 5)

	// Initially checks_state='Pending'. gh reports Success. Must persist.
	installGHStub(t, map[string]ghStubResp{
		"pr view 5":   {stdout: `{"number":5,"state":"OPEN","merged":false}`},
		"pr checks 5": {stdout: `[{"name":"ci","state":"SUCCESS","bucket":"pass"}]`},
		"pr merge 5":  {stdout: ""},
	})

	ghc := newGHClient()
	pr := store.GetAskBranchPR(db, prRowID)
	handleSubPRPoll(db, ghc, *pr, testLogger{})

	updated := store.GetAskBranchPR(db, prRowID)
	if updated.ChecksState != "Success" {
		t.Errorf("checks_state must persist Success transition, got %q", updated.ChecksState)
	}
}

// Silence unused imports when test selectively runs.
var _ = igit.RunCmd
var _ = gh.NewClient
var _ sql.DB
