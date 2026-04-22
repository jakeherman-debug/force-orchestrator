package agents

import (
	"database/sql"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"force-orchestrator/internal/gh"
	"force-orchestrator/internal/store"
)

// TestPRFlow_EndToEnd exercises the full PR-flow pipeline from Jedi Council
// approval through sub-PR auto-merge, convoy completion, Diplomat draft-PR
// creation, Ship-it, and draft-pr-watch's terminal transition to Shipped.
//
// It uses a real bare git origin + clone, stubbed gh (for network-y calls),
// and a stubbed Claude (for the council ruling and Diplomat body generation).
// The astromech work is simulated by pre-staging a committed agent branch —
// we don't run a real astromech because that would require a stubbed claude
// that issues file-editing commands. Everything after the astromech commit
// is exercised against the real Go code.
func TestPRFlow_EndToEnd(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	// ── Scenario setup ─────────────────────────────────────────────────────
	origin := t.TempDir()
	if err := exec.Command("git", "init", "-q", "--bare", "-b", "main", origin).Run(); err != nil {
		t.Fatal(err)
	}
	repoDir := t.TempDir()
	if err := exec.Command("git", "clone", "-q", origin, repoDir).Run(); err != nil {
		t.Fatal(err)
	}
	gitRun := func(args ...string) {
		cmd := exec.Command("git", append([]string{"-C", repoDir}, args...)...)
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=t@t",
			"GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %s", args, string(out))
		}
	}
	gitRun("config", "user.email", "t@t")
	gitRun("config", "user.name", "Test")
	os.WriteFile(filepath.Join(repoDir, "README"), []byte("hi\n"), 0644)
	gitRun("add", ".")
	gitRun("commit", "-m", "initial")
	gitRun("push", "-u", "origin", "main")
	gitRun("remote", "set-head", "origin", "main")

	// Install a PR template.
	tplPath := filepath.Join(repoDir, ".github", "pull_request_template.md")
	os.MkdirAll(filepath.Dir(tplPath), 0755)
	os.WriteFile(tplPath, []byte("## Summary\n{{summary}}\n\n## Testing\n{{testing}}\n"), 0644)

	// Register the repo.
	store.AddRepo(db, "api", repoDir, "")
	_ = store.SetRepoRemoteInfo(db, "api", "https://github.com/acme/api.git", "main")
	_ = store.SetRepoPRTemplatePath(db, "api", tplPath)

	// ── Step 1: Commander creates convoy + CodeEdit task ──────────────────
	convoyID, _ := store.CreateConvoy(db, "[1] add feature X")
	taskID, _ := store.AddConvoyTask(db, 0, "api", "implement feature X", convoyID, 0, "Pending")

	// ── Step 2: Pilot cuts the ask-branch ──────────────────────────────────
	createID, _ := QueueCreateAskBranch(db, convoyID)
	cb, _ := store.GetBounty(db, createID)
	runCreateAskBranch(db, cb, testLogger{})
	ab := store.GetConvoyAskBranch(db, convoyID, "api")
	if ab == nil {
		t.Fatal("ask-branch was not created")
	}

	// ── Step 3: Astromech is simulated by pre-committing on an agent branch.
	agentBranch := fmt.Sprintf("agent/R2-D2/task-%d", taskID)
	gitRun("fetch", "origin", ab.AskBranch)
	gitRun("checkout", "-b", agentBranch, "refs/remotes/origin/"+ab.AskBranch)
	os.WriteFile(filepath.Join(repoDir, "feature.go"), []byte("package x\n// feature impl\n"), 0644)
	gitRun("add", ".")
	gitRun("commit", "-m", "implement feature X")
	gitRun("checkout", "main")
	store.SetBranchName(db, taskID, agentBranch)
	db.Exec(`UPDATE BountyBoard SET status = 'AwaitingCouncilReview' WHERE id = ?`, taskID)

	// Register a worktree for the council so runCouncilTask can get a diff.
	if _, err := igitGetOrCreate(db, "Council-Yoda", repoDir); err != nil {
		t.Fatal(err)
	}

	// ── Step 4: Jedi Council approves → sub-PR opens via gh stub ──────────
	installGHStub(t, map[string]ghStubResp{
		"pr create": {stdout: "https://github.com/acme/api/pull/123\n"},
	})
	withStubCLIRunner(t, `{"approved":true,"feedback":"lgtm"}`, nil)

	b, _ := store.GetBounty(db, taskID)
	runCouncilTask(db, "Council-Yoda", b, log.New(io.Discard, "", 0))

	taskAfter, _ := store.GetBounty(db, taskID)
	if taskAfter.Status != subPRCITaskStatus {
		t.Fatalf("expected %s after Jedi approval, got %q", subPRCITaskStatus, taskAfter.Status)
	}
	pr := store.GetAskBranchPRByTask(db, taskID)
	if pr == nil || pr.PRNumber != 123 {
		t.Fatalf("sub-PR not recorded correctly: %+v", pr)
	}

	// ── Step 5: sub-pr-ci-watch with stubbed gh — CI green → auto-merge ───
	installGHStub(t, map[string]ghStubResp{
		"pr view 123":   {stdout: `{"number":123,"url":"u","state":"OPEN","isDraft":false,"merged":false}`},
		"pr checks 123": {stdout: `[{"name":"build","state":"SUCCESS","bucket":"pass"}]`},
		"pr merge 123":  {stdout: ""},
	})
	if err := dogSubPRCIWatch(db, testLogger{}); err != nil {
		t.Fatal(err)
	}

	// ── Step 6: Next tick sees PR merged → task Completed ─────────────────
	installGHStub(t, map[string]ghStubResp{
		"pr view 123": {stdout: `{"number":123,"url":"u","state":"MERGED","isDraft":false,"merged":true,"mergedAt":"2024-01-01"}`},
	})
	if err := dogSubPRCIWatch(db, testLogger{}); err != nil {
		t.Fatal(err)
	}
	taskAfter, _ = store.GetBounty(db, taskID)
	if taskAfter.Status != "Completed" {
		t.Fatalf("task should be Completed after sub-PR merge, got %q", taskAfter.Status)
	}
	prAfter := store.GetAskBranchPR(db, pr.ID)
	if prAfter.State != "Merged" {
		t.Errorf("PR row should be Merged, got %q", prAfter.State)
	}

	// ── Step 7: Chancellor detects convoy done → enqueues Diplomat ───────
	CheckConvoyCompletions(db, testLogger{})
	conv := store.GetConvoy(db, convoyID)
	if conv.Status != "AwaitingDraftPR" {
		t.Fatalf("convoy should be AwaitingDraftPR, got %q", conv.Status)
	}
	var shipTaskID int
	db.QueryRow(`SELECT id FROM BountyBoard WHERE type = 'ShipConvoy' LIMIT 1`).Scan(&shipTaskID)
	if shipTaskID == 0 {
		t.Fatal("ShipConvoy task not queued")
	}

	// ── Step 8: Diplomat opens the draft PR ───────────────────────────────
	installGHStub(t, map[string]ghStubResp{
		"pr create": {stdout: "https://github.com/acme/api/pull/200\n"},
	})
	withStubCLIRunner(t, "## Summary\n\nAdds feature X.\n\n## Testing\n\nVerified via CI.\n", nil)

	shipB, _ := store.GetBounty(db, shipTaskID)
	runShipConvoy(db, "Diplomat", shipB, testLogger{})

	conv = store.GetConvoy(db, convoyID)
	if conv.Status != "DraftPROpen" {
		t.Fatalf("convoy should be DraftPROpen, got %q (err=%s)", conv.Status, conv.ShippedAt)
	}
	abAfter := store.GetConvoyAskBranch(db, convoyID, "api")
	if abAfter.DraftPRNumber != 200 {
		t.Fatalf("draft PR not recorded: %+v", abAfter)
	}

	// ── Step 9: Human ships (draft-pr-watch observes merged) ──────────────
	installDraftPRViewStub(t, map[int]struct {
		State  string
		Merged bool
		Err    error
	}{200: {State: "MERGED", Merged: true}})

	if err := dogDraftPRWatch(db, testLogger{}); err != nil {
		t.Fatal(err)
	}
	conv = store.GetConvoy(db, convoyID)
	if conv.Status != "Shipped" {
		t.Fatalf("convoy should be Shipped, got %q", conv.Status)
	}

	// The ConvoyAskBranch row must reflect the merge — otherwise a subsequent
	// draft-pr-watch would see it as Open and loop.
	abShipped := store.GetConvoyAskBranch(db, convoyID, "api")
	if abShipped.DraftPRState != "Merged" {
		t.Errorf("ask-branch row should show DraftPRState=Merged, got %q", abShipped.DraftPRState)
	}
	if abShipped.ShippedAt == "" {
		t.Error("shipped_at on ask-branch row must be stamped")
	}

	// Cleanup task was queued.
	var cleanupID int
	db.QueryRow(`SELECT id FROM BountyBoard WHERE type = 'CleanupAskBranch' AND status = 'Pending'`).Scan(&cleanupID)
	if cleanupID == 0 {
		t.Error("CleanupAskBranch was not queued")
	}

	// Librarian memory was queued for the convoy.
	var memID int
	db.QueryRow(`SELECT id FROM BountyBoard WHERE type = 'WriteMemory' AND payload LIKE '%convoy-shipped%' LIMIT 1`).Scan(&memID)
	if memID == 0 {
		t.Error("Librarian convoy memory was not queued")
	}
}

// TestPRFlow_CIFailure_SelfHealsViaMedic exercises the Medic CIFailureTriage
// path: sub-pr-ci-watch sees a failure, queues Medic, Medic classifies as
// RealBug and spawns a fix task targeting the astromech branch. Verifies the
// fix task has the correct branch_name (so the astromech resumes on top).
func TestPRFlow_CIFailure_SelfHealsViaMedic(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	store.AddRepo(db, "api", "/tmp/api", "")
	cid, _ := store.CreateConvoy(db, "[1] t")
	tid, _ := store.AddConvoyTask(db, 0, "api", "implement", cid, 0, "Pending")
	store.SetBranchName(db, tid, "agent/R2-D2/task-"+fmt.Sprint(tid))
	db.Exec(`UPDATE BountyBoard SET status = ? WHERE id = ?`, subPRCITaskStatus, tid)
	prID, _ := store.CreateAskBranchPR(db, tid, cid, "api", "u", 1)

	// First dog tick: CI fails.
	installGHStub(t, map[string]ghStubResp{
		"pr view 1":   {stdout: `{"number":1,"state":"OPEN","merged":false}`},
		"pr checks 1": {stdout: `[{"name":"test","state":"FAILURE","bucket":"fail"}]`, err: fmt.Errorf("exit 1")},
	})
	if err := dogSubPRCIWatch(db, testLogger{}); err != nil {
		t.Fatal(err)
	}

	// A CIFailureTriage task should be queued for Medic.
	var triageID int
	db.QueryRow(`SELECT id FROM BountyBoard WHERE type = 'CIFailureTriage' LIMIT 1`).Scan(&triageID)
	if triageID == 0 {
		t.Fatal("CIFailureTriage not queued")
	}

	// Medic runs, classifies as RealBug.
	withStubCLIRunner(t, `{"classification":"RealBug","diagnosis":"assertion wrong","fix_guidance":"update expect(3) to expect(4)","operator_note":""}`, nil)
	triageB, _ := store.GetBounty(db, triageID)
	runMedicCITriage(db, "Medic-Bacta", triageB, testLogger{})

	// A CodeEdit fix task should have been spawned with the astromech branch.
	var fixID int
	var fixBranch string
	db.QueryRow(`SELECT id, branch_name FROM BountyBoard WHERE type = 'CodeEdit' AND parent_id = ? AND status = 'Pending'`, tid).Scan(&fixID, &fixBranch)
	if fixID == 0 {
		t.Fatal("Medic did not spawn fix task")
	}
	if !strings.HasPrefix(fixBranch, "agent/R2-D2/task-") {
		t.Errorf("fix task should resume on agent branch, got %q", fixBranch)
	}
	_ = prID // keep the lint happy — pr row id exists for reference
}

// TestPRFlow_LegacyPath_StillWorksWhenPRFlowDisabled proves the fallback: a
// repo with pr_flow_enabled=0 goes through the old MergeAndCleanup path, and
// no ConvoyAskBranch rows / sub-PRs / draft PRs are produced.
func TestPRFlow_LegacyPath_StillWorksWhenPRFlowDisabled(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not found")
	}
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	// Minimal repo (non-bare, no remote).
	dir := t.TempDir()
	exec.Command("git", "init", "-q", "-b", "main", dir).Run()
	exec.Command("git", "-C", dir, "config", "user.email", "t@t").Run()
	exec.Command("git", "-C", dir, "config", "user.name", "Test").Run()
	os.WriteFile(filepath.Join(dir, "README"), []byte("hi"), 0644)
	exec.Command("git", "-C", dir, "add", ".").Run()
	exec.Command("git", "-C", dir, "commit", "-q", "-m", "initial").Run()

	store.AddRepo(db, "legacy", dir, "")
	_ = store.SetRepoPRFlowEnabled(db, "legacy", false)

	cid, _ := store.CreateConvoy(db, "[1] legacy-test")
	tid, _ := store.AddConvoyTask(db, 0, "legacy", "x", cid, 0, "Pending")

	// Even though we call CreateAskBranch for the convoy, the handler must
	// skip pr_flow_enabled=false repos.
	createID, _ := QueueCreateAskBranch(db, cid)
	cb, _ := store.GetBounty(db, createID)
	runCreateAskBranch(db, cb, testLogger{})
	if ab := store.GetConvoyAskBranch(db, cid, "legacy"); ab != nil {
		t.Errorf("no ask-branch should exist for pr_flow_enabled=false repo, got %+v", ab)
	}

	// Legacy Jedi approval path: add a branch with a commit, set Status, run.
	branch := fmt.Sprintf("agent/R5-D4/task-%d", tid)
	gitEnv := append(os.Environ(),
		"GIT_AUTHOR_NAME=T", "GIT_AUTHOR_EMAIL=t@t",
		"GIT_COMMITTER_NAME=T", "GIT_COMMITTER_EMAIL=t@t")
	runGit := func(args ...string) {
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = gitEnv
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %s", args, string(out))
		}
	}
	runGit("checkout", "-b", branch)
	os.WriteFile(filepath.Join(dir, "ch.txt"), []byte("change"), 0644)
	runGit("add", ".")
	runGit("commit", "-m", "astromech change")
	runGit("checkout", "main")

	if _, err := igitGetOrCreate(db, "Council-Yoda", dir); err != nil {
		t.Fatal(err)
	}
	store.SetBranchName(db, tid, branch)
	db.Exec(`UPDATE BountyBoard SET status = 'AwaitingCouncilReview' WHERE id = ?`, tid)

	withStubCLIRunner(t, `{"approved":true,"feedback":""}`, nil)
	b, _ := store.GetBounty(db, tid)
	runCouncilTask(db, "Council-Yoda", b, log.New(io.Discard, "", 0))

	after, _ := store.GetBounty(db, tid)
	if after.Status != "Completed" {
		t.Errorf("legacy repo should complete via local merge, got %q", after.Status)
	}
	// No AskBranchPR row should exist.
	if store.GetAskBranchPRByTask(db, tid) != nil {
		t.Errorf("legacy path must not create sub-PR")
	}
}

// Silence unused-import helpers in edge-case test paths.
var _ = sql.ErrNoRows
var _ = gh.NewClient
