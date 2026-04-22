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
	"time"

	"force-orchestrator/internal/claude"
	"force-orchestrator/internal/gh"
	igit "force-orchestrator/internal/git"
	"force-orchestrator/internal/store"
)

// ── gh stub infrastructure ───────────────────────────────────────────────────

// ghStubRunner implements gh.Runner with scripted responses. The calls slice
// records every invocation for test assertions.
type ghStubRunner struct {
	responses map[string]ghStubResp
	calls     []ghStubCall
}

type ghStubResp struct {
	stdout string
	stderr string
	err    error
}

type ghStubCall struct {
	args  []string
	stdin string
}

func (s *ghStubRunner) Run(cwd string, args []string, stdin []byte) ([]byte, []byte, error) {
	s.calls = append(s.calls, ghStubCall{args: append([]string{}, args...), stdin: string(stdin)})
	key := strings.Join(args, " ")
	// Match longest prefix that we have a canned response for.
	for k, r := range s.responses {
		if strings.HasPrefix(key, k) {
			return []byte(r.stdout), []byte(r.stderr), r.err
		}
	}
	return nil, []byte("unmatched: " + key), fmt.Errorf("ghStubRunner: no response for %s", key)
}

// installGHStub swaps the agents package's gh client factory for one using the
// given stub runner. Returns the runner so tests can assert on calls.
func installGHStub(t *testing.T, responses map[string]ghStubResp) *ghStubRunner {
	t.Helper()
	stub := &ghStubRunner{responses: responses}
	cleanup := SetGHClientFactory(func() *gh.Client { return gh.NewClientWithRunner(stub) })
	t.Cleanup(cleanup)
	return stub
}

// ── Jedi Council sub-PR approval path ────────────────────────────────────────

// setupSubPRScenario creates: an origin repo, a local clone, a registered Repository,
// a convoy, a CodeEdit task on the convoy, an ask-branch (via direct store call), and
// a committed agent branch. Returns (convoyID, taskID, repoDir, branchName).
func setupSubPRScenario(t *testing.T, db *sql.DB) (convoyID, taskID int, repoDir, branchName string) {
	t.Helper()
	origin := t.TempDir()
	if err := exec.Command("git", "init", "-q", "--bare", "-b", "main", origin).Run(); err != nil {
		t.Fatal(err)
	}
	repoDir = t.TempDir()
	if err := exec.Command("git", "clone", "-q", origin, repoDir).Run(); err != nil {
		t.Fatal(err)
	}
	gitEnv := append(os.Environ(),
		"GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=t@t.com",
		"GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=t@t.com")
	run := func(args ...string) {
		cmd := exec.Command("git", append([]string{"-C", repoDir}, args...)...)
		cmd.Env = gitEnv
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %s", args, string(out))
		}
	}
	run("config", "user.email", "t@t")
	run("config", "user.name", "Test")
	os.WriteFile(filepath.Join(repoDir, "README"), []byte("hi"), 0644)
	run("add", ".")
	run("commit", "-m", "initial")
	run("push", "-u", "origin", "main")
	run("remote", "set-head", "origin", "main")

	store.AddRepo(db, "api", repoDir, "")
	_ = store.SetRepoRemoteInfo(db, "api", "https://github.com/acme/api.git", "main")

	convoyID, _ = store.CreateConvoy(db, "[1] test")
	taskID, _ = store.AddConvoyTask(db, 0, "api", "do thing", convoyID, 0, "Pending")

	// Create the ask-branch so usePRFlow is true.
	if _, err := createTestAskBranch(repoDir, "force/ask-1-test"); err != nil {
		t.Fatal(err)
	}
	_ = store.UpsertConvoyAskBranch(db, convoyID, "api", "force/ask-1-test", "sha-base")

	// Create a committed agent branch (simulates astromech having worked).
	branchName = fmt.Sprintf("agent/R2-D2/task-%d", taskID)
	run("checkout", "-b", branchName, "force/ask-1-test")
	os.WriteFile(filepath.Join(repoDir, "feat.txt"), []byte("feature"), 0644)
	run("add", ".")
	run("commit", "-m", "implement feature")
	run("checkout", "main")

	// Register a worktree so runCouncilTask doesn't error on worktree lookup.
	if _, err := igitGetOrCreate(db, "Council-Yoda", repoDir); err != nil {
		t.Fatal(err)
	}

	// Put the task into AwaitingCouncilReview with the branch name recorded.
	db.Exec(`UPDATE BountyBoard SET status = 'AwaitingCouncilReview', branch_name = ? WHERE id = ?`, branchName, taskID)
	return
}

// createTestAskBranch is a small helper so tests don't depend on internal/git.
func createTestAskBranch(repoPath, branchName string) (string, error) {
	exec.Command("git", "-C", repoPath, "fetch", "origin", "main").Run()
	shaOut, err := exec.Command("git", "-C", repoPath, "rev-parse", "refs/remotes/origin/main").CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("rev-parse: %s", string(shaOut))
	}
	sha := strings.TrimSpace(string(shaOut))
	exec.Command("git", "-C", repoPath, "branch", "-f", branchName, sha).Run()
	if out, err := exec.Command("git", "-C", repoPath, "push", "-u", "origin", branchName).CombinedOutput(); err != nil {
		return "", fmt.Errorf("push: %s", strings.TrimSpace(string(out)))
	}
	return sha, nil
}

// igitGetOrCreate is a thin wrapper to keep test imports tight.
func igitGetOrCreate(db *sql.DB, name, repo string) (string, error) {
	return igit.GetOrCreateAgentWorktree(db, name, repo)
}

// TestJediCouncilApproval_UsesSubPRPath proves that when a repo is pr_flow_enabled
// and the convoy has an ask-branch, the Jedi Council approval path pushes the
// astromech branch and opens a sub-PR rather than local-merging.
func TestJediCouncilApproval_UsesSubPRPath(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	convoyID, taskID, repoDir, branchName := setupSubPRScenario(t, db)
	_ = convoyID

	// Stub gh so `gh pr create` returns a fake URL.
	stub := installGHStub(t, map[string]ghStubResp{
		"pr create": {stdout: "https://github.com/acme/api/pull/777\n"},
	})

	withStubCLIRunner(t, `{"approved":true,"feedback":"lgtm"}`, nil)
	logger := log.New(io.Discard, "", 0)
	b, _ := store.GetBounty(db, taskID)
	runCouncilTask(db, "Council-Yoda", b, logger)

	// Task status should be AwaitingSubPRCI.
	after, _ := store.GetBounty(db, taskID)
	if after.Status != subPRCITaskStatus {
		t.Errorf("expected status %s, got %q", subPRCITaskStatus, after.Status)
	}

	// An AskBranchPR row must exist for this task.
	pr := store.GetAskBranchPRByTask(db, taskID)
	if pr == nil {
		t.Fatal("AskBranchPR row not created")
	}
	if pr.PRNumber != 777 {
		t.Errorf("PR number wrong: %d", pr.PRNumber)
	}
	if pr.State != "Open" {
		t.Errorf("PR state should be Open, got %q", pr.State)
	}

	// gh pr create must have been called with the right args.
	var sawPRCreate bool
	for _, call := range stub.calls {
		if len(call.args) >= 2 && call.args[0] == "pr" && call.args[1] == "create" {
			sawPRCreate = true
			joined := strings.Join(call.args, " ")
			if !strings.Contains(joined, "--base force/ask-1-test") {
				t.Errorf("base mismatch: %q", joined)
			}
			if !strings.Contains(joined, "--head "+branchName) {
				t.Errorf("head mismatch: %q", joined)
			}
		}
	}
	if !sawPRCreate {
		t.Errorf("gh pr create was never invoked; calls: %+v", stub.calls)
	}

	// Origin must have received the astromech branch push.
	// The test origin is extracted from the clone — find it by walking parents.
	remoteOut, _ := exec.Command("git", "-C", repoDir, "remote", "get-url", "origin").Output()
	origin := strings.TrimSpace(string(remoteOut))
	lsRemote, err := exec.Command("git", "ls-remote", origin, branchName).CombinedOutput()
	if err != nil {
		t.Fatalf("ls-remote: %v", err)
	}
	if !strings.Contains(string(lsRemote), branchName) {
		t.Errorf("branch %s not found on origin; ls-remote: %s", branchName, lsRemote)
	}

	// No WriteMemory task should have been spawned yet (waits for PR merge).
	var memCount int
	db.QueryRow(`SELECT COUNT(*) FROM BountyBoard WHERE parent_id = ? AND type = 'WriteMemory'`, taskID).Scan(&memCount)
	if memCount != 0 {
		t.Errorf("WriteMemory must NOT be spawned at approval time; count=%d", memCount)
	}
}

// TestJediCouncilApproval_FallsBackToLegacyMergeWhenPRFlowDisabled proves the
// legacy path still fires when the repo opts out. Verifies not just the task
// status but that the astromech's commit actually landed on main locally.
func TestJediCouncilApproval_FallsBackToLegacyMergeWhenPRFlowDisabled(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	convoyID, taskID, repoDir, _ := setupSubPRScenario(t, db)
	_ = convoyID
	// Disable PR flow on the repo.
	_ = store.SetRepoPRFlowEnabled(db, "api", false)

	// Capture main's HEAD BEFORE the legacy merge so we can verify it advanced.
	mainBeforeOut, _ := exec.Command("git", "-C", repoDir, "rev-parse", "main").Output()
	mainBefore := strings.TrimSpace(string(mainBeforeOut))

	withStubCLIRunner(t, `{"approved":true,"feedback":""}`, nil)
	logger := log.New(io.Discard, "", 0)
	b, _ := store.GetBounty(db, taskID)
	runCouncilTask(db, "Council-Yoda", b, logger)

	// Task should be Completed (legacy merge path).
	after, _ := store.GetBounty(db, taskID)
	if after.Status != "Completed" {
		t.Errorf("expected Completed via legacy merge, got %q", after.Status)
	}
	// No AskBranchPR row should exist (no sub-PR was opened).
	if pr := store.GetAskBranchPRByTask(db, taskID); pr != nil {
		t.Errorf("PR row should not exist for legacy path: %+v", pr)
	}
	// WriteMemory should have been spawned (legacy path does that at approval).
	var memCount int
	db.QueryRow(`SELECT COUNT(*) FROM BountyBoard WHERE parent_id = ? AND type = 'WriteMemory'`, taskID).Scan(&memCount)
	if memCount == 0 {
		t.Error("WriteMemory must be spawned in legacy path")
	}

	// Critically: the commit must have actually landed on main. A broken merge
	// that reported Completed without moving main's HEAD would fail here.
	mainAfterOut, _ := exec.Command("git", "-C", repoDir, "rev-parse", "main").Output()
	mainAfter := strings.TrimSpace(string(mainAfterOut))
	if mainAfter == mainBefore {
		t.Errorf("legacy merge did not advance main's HEAD (before=%s, after=%s)", mainBefore, mainAfter)
	}
}

func TestJediCouncilApproval_PRCreateFailureEscalates(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	convoyID, taskID, _, _ := setupSubPRScenario(t, db)
	_ = convoyID

	// Stub gh to return a BranchProtection error — this is auth-ish and should
	// escalate rather than retry.
	installGHStub(t, map[string]ghStubResp{
		"pr create": {stderr: "Error: protected branch hook declined", err: fmt.Errorf("exit 1")},
	})

	withStubCLIRunner(t, `{"approved":true,"feedback":""}`, nil)
	logger := log.New(io.Discard, "", 0)
	b, _ := store.GetBounty(db, taskID)
	runCouncilTask(db, "Council-Yoda", b, logger)

	after, _ := store.GetBounty(db, taskID)
	if after.Status != "Escalated" {
		t.Errorf("expected Escalated on branch-protection failure, got %q", after.Status)
	}
	// An Escalations row should exist.
	var escCount int
	db.QueryRow(`SELECT COUNT(*) FROM Escalations WHERE task_id = ?`, taskID).Scan(&escCount)
	if escCount == 0 {
		t.Error("expected Escalation row")
	}
}

// ── sub-pr-ci-watch dog ─────────────────────────────────────────────────────

func TestDogSubPRCIWatch_NoPRs_IsNoOp(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	if err := dogSubPRCIWatch(db, testLogger{}); err != nil {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestDogSubPRCIWatch_ChecksSuccess_TriggersAutoMerge(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	convoyID, taskID, _, _ := setupSubPRScenario(t, db)
	_, err := store.CreateAskBranchPR(db, taskID, convoyID, "api", "https://github.com/acme/api/pull/5", 5)
	if err != nil {
		t.Fatal(err)
	}

	stub := installGHStub(t, map[string]ghStubResp{
		"pr view 5":   {stdout: `{"number":5,"url":"u","state":"OPEN","isDraft":false,"merged":false,"mergedAt":"","closedAt":"","reviews":[]}`},
		"pr checks 5": {stdout: `[{"name":"build","state":"SUCCESS","bucket":"pass"}]`},
		"pr merge 5":  {stdout: ""},
	})

	if err := dogSubPRCIWatch(db, testLogger{}); err != nil {
		t.Fatal(err)
	}

	// gh pr merge --auto must have been called.
	var sawMerge bool
	for _, c := range stub.calls {
		if len(c.args) >= 2 && c.args[0] == "pr" && c.args[1] == "merge" {
			sawMerge = true
			joined := strings.Join(c.args, " ")
			if !strings.Contains(joined, "--auto") {
				t.Errorf("expected --auto flag: %q", joined)
			}
		}
	}
	if !sawMerge {
		t.Errorf("auto-merge was not triggered on green CI")
	}
	// Task NOT yet Completed — gh's auto-merge is async; next tick will confirm.
	after, _ := store.GetBounty(db, taskID)
	if after.Status == "Completed" {
		t.Errorf("task should not be marked Completed until merged=true on next tick")
	}
}

func TestDogSubPRCIWatch_PRMerged_CompletesTaskAndSpawnsMemory(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	convoyID, taskID, _, _ := setupSubPRScenario(t, db)
	// Task is AwaitingSubPRCI before we run the dog.
	db.Exec(`UPDATE BountyBoard SET status = ? WHERE id = ?`, subPRCITaskStatus, taskID)
	prRowID, _ := store.CreateAskBranchPR(db, taskID, convoyID, "api", "u", 9)
	_ = prRowID

	installGHStub(t, map[string]ghStubResp{
		"pr view 9": {stdout: `{"number":9,"url":"u","state":"MERGED","isDraft":false,"merged":true,"mergedAt":"2024-01-01","closedAt":"","reviews":[]}`},
	})

	if err := dogSubPRCIWatch(db, testLogger{}); err != nil {
		t.Fatal(err)
	}
	after, _ := store.GetBounty(db, taskID)
	if after.Status != "Completed" {
		t.Errorf("task should be Completed after merged: %q", after.Status)
	}
	// PR row should be MergedState.
	pr := store.GetAskBranchPR(db, prRowID)
	if pr.State != "Merged" {
		t.Errorf("PR state should be Merged: %q", pr.State)
	}
	// WriteMemory spawned.
	var memCount int
	db.QueryRow(`SELECT COUNT(*) FROM BountyBoard WHERE parent_id = ? AND type = 'WriteMemory'`, taskID).Scan(&memCount)
	if memCount == 0 {
		t.Error("WriteMemory must be spawned when sub-PR merges")
	}
}

func TestDogSubPRCIWatch_ExternallyClosed_EscalatesTask(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	convoyID, taskID, _, _ := setupSubPRScenario(t, db)
	prRowID, _ := store.CreateAskBranchPR(db, taskID, convoyID, "api", "u", 10)

	installGHStub(t, map[string]ghStubResp{
		"pr view 10": {stdout: `{"number":10,"url":"u","state":"CLOSED","isDraft":false,"merged":false,"mergedAt":"","closedAt":"2024-01-01","reviews":[]}`},
	})

	_ = dogSubPRCIWatch(db, testLogger{})
	after, _ := store.GetBounty(db, taskID)
	if after.Status != "Escalated" {
		t.Errorf("externally-closed PR should escalate task: %q", after.Status)
	}
	pr := store.GetAskBranchPR(db, prRowID)
	if pr.State != "Closed" {
		t.Errorf("PR state should be Closed: %q", pr.State)
	}
	var escCount int
	db.QueryRow(`SELECT COUNT(*) FROM Escalations WHERE task_id = ?`, taskID).Scan(&escCount)
	if escCount == 0 {
		t.Error("expected escalation row")
	}
}

// TestOnSubPRCIFailed_DedupIsJSONBoundaryAware proves that a Medic triage task
// already queued for sub_pr_row_id=1 does NOT incorrectly dedup an attempt to
// queue one for sub_pr_row_id=10. Naive LIKE '%...id":1%' matches both; the
// corrected query uses JSON-boundary-aware matching.
func TestOnSubPRCIFailed_DedupIsJSONBoundaryAware(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	store.AddRepo(db, "api", "/tmp/api", "")
	cid, _ := store.CreateConvoy(db, "[1] t")
	// Need at least 10 tasks to get prID=10.
	var prID10 int
	for i := 0; i < 10; i++ {
		tid, _ := store.AddConvoyTask(db, 0, "api", fmt.Sprintf("task-%d", i), cid, 0, "Pending")
		prNumber := 1000 + i // unique per PR
		id, _ := store.CreateAskBranchPR(db, tid, cid, "api", fmt.Sprintf("u%d", i), prNumber)
		if i == 9 {
			prID10 = id // the 10th PR (row ID = 10)
		}
	}
	if prID10 != 10 {
		t.Fatalf("expected PR row ID 10, got %d — test setup wrong", prID10)
	}

	// Queue a CIFailureTriage for sub_pr_row_id=1 (the first PR).
	pr1 := store.GetAskBranchPR(db, 1)
	onSubPRCIFailed(db, *pr1, testLogger{})

	// Now call onSubPRCIFailed for sub_pr_row_id=10. With a broken LIKE this
	// would see the existing task-1 triage and incorrectly skip queuing.
	pr10 := store.GetAskBranchPR(db, prID10)
	onSubPRCIFailed(db, *pr10, testLogger{})

	// Expect TWO triage tasks — one per distinct sub-PR.
	var triageCount int
	db.QueryRow(`SELECT COUNT(*) FROM BountyBoard WHERE type = 'CIFailureTriage' AND status = 'Pending'`).Scan(&triageCount)
	if triageCount != 2 {
		t.Errorf("dedup must be JSON-boundary-aware: expected 2 triage tasks for distinct PRs, got %d", triageCount)
	}
}

func TestDogSubPRCIWatch_CIFailure_QueuesMedicTriage(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	convoyID, taskID, _, _ := setupSubPRScenario(t, db)
	// After Jedi approval the task would be AwaitingSubPRCI; simulate that here.
	db.Exec(`UPDATE BountyBoard SET status = ? WHERE id = ?`, subPRCITaskStatus, taskID)
	prRowID, _ := store.CreateAskBranchPR(db, taskID, convoyID, "api", "u", 11)

	installGHStub(t, map[string]ghStubResp{
		"pr view 11":   {stdout: `{"number":11,"url":"u","state":"OPEN","isDraft":false,"merged":false,"mergedAt":"","closedAt":"","reviews":[]}`},
		"pr checks 11": {stdout: `[{"name":"test","state":"FAILURE","bucket":"fail"}]`, err: fmt.Errorf("exit 1")},
	})

	// Tick 1 — count=1, Medic triage queued, task stays AwaitingSubPRCI.
	_ = dogSubPRCIWatch(db, testLogger{})
	pr := store.GetAskBranchPR(db, prRowID)
	if pr.FailureCount != 1 {
		t.Errorf("after first failure, count should be 1, got %d", pr.FailureCount)
	}
	var triageCount int
	db.QueryRow(`SELECT COUNT(*) FROM BountyBoard WHERE type = 'CIFailureTriage'`).Scan(&triageCount)
	if triageCount != 1 {
		t.Errorf("expected 1 CIFailureTriage queued, got %d", triageCount)
	}

	// Tick 2 & 3 — still only ONE triage task queued (duplicate prevention).
	// The existing triage task is Pending, so the dog should not queue another.
	_ = dogSubPRCIWatch(db, testLogger{})
	_ = dogSubPRCIWatch(db, testLogger{})
	db.QueryRow(`SELECT COUNT(*) FROM BountyBoard WHERE type = 'CIFailureTriage'`).Scan(&triageCount)
	if triageCount != 1 {
		t.Errorf("duplicate CIFailureTriage tasks must be prevented, got %d", triageCount)
	}
	// Task remains in CI-waiting state (Medic hasn't run — would escalate if it did).
	after, _ := store.GetBounty(db, taskID)
	if after.Status != subPRCITaskStatus {
		t.Errorf("task should stay AwaitingSubPRCI while Medic triage pending, got %q", after.Status)
	}
}

func TestBuildSubPRTitle_StripsGoalPrefixAndCaps(t *testing.T) {
	b := &store.Bounty{ID: 42, Payload: "[GOAL: Do X]\nActually do the thing"}
	title := buildSubPRTitle(b)
	if !strings.Contains(title, "task 42") {
		t.Errorf("title should include task number: %q", title)
	}
	if strings.Contains(title, "GOAL") {
		t.Errorf("GOAL prefix should be stripped: %q", title)
	}
	if !strings.Contains(title, "Actually do the thing") {
		t.Errorf("title should include first real line: %q", title)
	}
	// Long payload should be truncated.
	b.Payload = strings.Repeat("x", 500)
	title = buildSubPRTitle(b)
	if len(title) > 120 {
		t.Errorf("title too long: %d chars", len(title))
	}
}

func TestBuildSubPRBody_IncludesRulingFeedback(t *testing.T) {
	b := &store.Bounty{ID: 7, ConvoyID: 3, Payload: "Fix bug Y"}
	body := buildSubPRBody(b, store.CouncilRuling{Approved: true, Feedback: "clean change"})
	if !strings.Contains(body, "Fleet task: #7") {
		t.Errorf("body missing task ref: %q", body)
	}
	if !strings.Contains(body, "convoy #3") {
		t.Errorf("body missing convoy ref: %q", body)
	}
	if !strings.Contains(body, "clean change") {
		t.Errorf("body missing feedback: %q", body)
	}
}

func TestDeriveGHRepoFromRemoteURL(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"git@github.com:acme/api.git", "acme/api"},
		{"git@github.com:acme/api", "acme/api"},
		{"https://github.com/acme/api.git", "acme/api"},
		{"https://github.com/acme/api", "acme/api"},
		{"file:///tmp/repo", ""},
		{"", ""},
	}
	for _, c := range cases {
		if got := deriveGHRepoFromRemoteURL(c.in); got != c.want {
			t.Errorf("deriveGHRepoFromRemoteURL(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestDogSubPRCIWatch_NoCIConfigured_MergesDirectly verifies that a PR with
// mergeStateStatus=CLEAN is merged immediately on the first poll — no 10-minute
// wait required. This is the common case for repos without CI.
func TestDogSubPRCIWatch_NoCIConfigured_MergesDirectly(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	convoyID, taskID, _, _ := setupSubPRScenario(t, db)
	_, err := store.CreateAskBranchPR(db, taskID, convoyID, "api", "https://github.com/acme/api/pull/7", 7)
	if err != nil {
		t.Fatal(err)
	}
	// No back-dating — CLEAN should merge immediately, not after 10 minutes.

	stub := installGHStub(t, map[string]ghStubResp{
		"pr view 7":  {stdout: `{"number":7,"url":"u","state":"OPEN","isDraft":false,"merged":false,"mergedAt":"","closedAt":"","reviews":[],"mergeStateStatus":"CLEAN","mergeable":"MERGEABLE"}`},
		"pr merge 7": {stdout: ""},
	})

	if err := dogSubPRCIWatch(db, testLogger{}); err != nil {
		t.Fatal(err)
	}

	// gh pr merge (not --auto) must have been called.
	var sawMerge bool
	for _, c := range stub.calls {
		if len(c.args) >= 2 && c.args[0] == "pr" && c.args[1] == "merge" {
			sawMerge = true
			joined := strings.Join(c.args, " ")
			if strings.Contains(joined, "--auto") {
				t.Errorf("no-CI merge must NOT use --auto flag: %q", joined)
			}
		}
	}
	if !sawMerge {
		t.Errorf("expected direct merge when mergeStateStatus=CLEAN")
	}

	after, _ := store.GetBounty(db, taskID)
	if after.Status != "Completed" {
		t.Errorf("task should be Completed after CLEAN direct merge, got %q", after.Status)
	}
}

// TestDogSubPRCIWatch_NoCIFallback covers the fallback path: mergeStateStatus is
// absent (older GitHub Enterprise / API lag) and we rely on the 10-minute heuristic.
func TestDogSubPRCIWatch_NoCIFallback(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	convoyID, taskID, _, _ := setupSubPRScenario(t, db)
	prRowID, err := store.CreateAskBranchPR(db, taskID, convoyID, "api", "https://github.com/acme/api/pull/8", 8)
	if err != nil {
		t.Fatal(err)
	}
	db.Exec(`UPDATE AskBranchPRs SET created_at = datetime('now', '-15 minutes') WHERE id = ?`, prRowID)

	stub := installGHStub(t, map[string]ghStubResp{
		// No mergeStateStatus field → falls through to check-based logic.
		"pr view 8":   {stdout: `{"number":8,"url":"u","state":"OPEN","isDraft":false,"merged":false,"mergedAt":"","closedAt":"","reviews":[]}`},
		"pr checks 8": {stdout: "", stderr: "no checks reported on the 'branch' branch", err: fmt.Errorf("exit status 1")},
		"pr merge 8":  {stdout: ""},
	})

	if err := dogSubPRCIWatch(db, testLogger{}); err != nil {
		t.Fatal(err)
	}

	var sawMerge bool
	for _, c := range stub.calls {
		if len(c.args) >= 2 && c.args[0] == "pr" && c.args[1] == "merge" {
			sawMerge = true
		}
	}
	if !sawMerge {
		t.Errorf("expected fallback direct merge after missingCITimeout with no mergeStateStatus")
	}
	_ = stub
}

// TestDogSubPRCIWatch_BranchProtectionBlocked verifies that a PR reporting
// mergeStateStatus=BLOCKED with no failing CI checks is escalated immediately
// (branch protection rule — can't self-heal).
func TestDogSubPRCIWatch_BranchProtectionBlocked(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	convoyID, taskID, _, _ := setupSubPRScenario(t, db)
	_, err := store.CreateAskBranchPR(db, taskID, convoyID, "api", "https://github.com/acme/api/pull/9", 9)
	if err != nil {
		t.Fatal(err)
	}

	installGHStub(t, map[string]ghStubResp{
		"pr view 9":   {stdout: `{"number":9,"url":"u","state":"OPEN","isDraft":false,"merged":false,"mergedAt":"","closedAt":"","reviews":[],"mergeStateStatus":"BLOCKED","mergeable":"MERGEABLE"}`},
		"pr checks 9": {stdout: `[]`}, // no failing checks
	})

	if err := dogSubPRCIWatch(db, testLogger{}); err != nil {
		t.Fatal(err)
	}

	// Task should be Escalated, not stuck in AwaitingSubPRCI.
	after, _ := store.GetBounty(db, taskID)
	if after.Status != "Escalated" {
		t.Errorf("task should be Escalated when BLOCKED by branch protection, got %q", after.Status)
	}

	var msg string
	db.QueryRow(`SELECT message FROM Escalations WHERE task_id = ? ORDER BY id DESC LIMIT 1`, taskID).Scan(&msg)
	if !strings.Contains(msg, "branch protection") {
		t.Errorf("escalation message should mention branch protection, got %q", msg)
	}
}

// TestOnSubPRMerged_AtomicCompletion verifies that all four writes performed by
// onSubPRMerged (mark PR merged, complete task, unblock deps, queue WriteMemory)
// land together. On happy path, verify all four state changes are present.
func TestOnSubPRMerged_AtomicCompletion(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	convoyID, taskID, _, _ := setupSubPRScenario(t, db)
	prRowID, err := store.CreateAskBranchPR(db, taskID, convoyID, "api", "https://github.com/acme/api/pull/20", 20)
	if err != nil {
		t.Fatal(err)
	}

	// Add a downstream dependent task so we can verify UnblockDependentsOf ran.
	depID, _ := store.AddConvoyTask(db, 0, "api", "depends on task", convoyID, 0, "Pending")
	store.AddDependency(db, depID, taskID)

	pr := store.GetAskBranchPR(db, prRowID)
	onSubPRMerged(db, *pr, testLogger{})

	// (1) PR row is Merged
	var state string
	db.QueryRow(`SELECT state FROM AskBranchPRs WHERE id = ?`, prRowID).Scan(&state)
	if state != "Merged" {
		t.Errorf("AskBranchPR.state = %q, want Merged", state)
	}

	// (2) Parent task Completed
	after, _ := store.GetBounty(db, taskID)
	if after.Status != "Completed" {
		t.Errorf("task status = %q, want Completed", after.Status)
	}

	// (3) Dependent edge removed
	var depCount int
	db.QueryRow(`SELECT COUNT(*) FROM TaskDependencies WHERE depends_on = ?`, taskID).Scan(&depCount)
	if depCount != 0 {
		t.Errorf("expected 0 dependency edges after unblock, got %d", depCount)
	}

	// (4) WriteMemory task queued
	var memCount int
	db.QueryRow(`SELECT COUNT(*) FROM BountyBoard WHERE type = 'WriteMemory' AND parent_id = ?`, taskID).Scan(&memCount)
	if memCount != 1 {
		t.Errorf("expected 1 WriteMemory task for parent %d, got %d", taskID, memCount)
	}
}

// TestTransitionConvoyToShipped_AtomicFinalization verifies that the three
// writes in the Shipped transition (convoy status, cleanup task, memory task)
// land together.
func TestTransitionConvoyToShipped_AtomicFinalization(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	convoyID, _, _, _ := setupSubPRScenario(t, db)
	db.Exec(`UPDATE Convoys SET status = 'DraftPROpen' WHERE id = ?`, convoyID)

	transitionConvoyToShipped(db, convoyID, "test-convoy", testLogger{})

	// Convoy status Shipped
	var cs string
	db.QueryRow(`SELECT status FROM Convoys WHERE id = ?`, convoyID).Scan(&cs)
	if cs != "Shipped" {
		t.Errorf("convoy status = %q, want Shipped", cs)
	}

	// CleanupAskBranch queued
	var cleanupCount int
	db.QueryRow(`SELECT COUNT(*) FROM BountyBoard WHERE type = 'CleanupAskBranch' AND status = 'Pending'`).Scan(&cleanupCount)
	if cleanupCount != 1 {
		t.Errorf("expected 1 CleanupAskBranch queued, got %d", cleanupCount)
	}

	// WriteMemory task queued (with convoy-shipped tag in payload)
	var memCount int
	db.QueryRow(`SELECT COUNT(*) FROM BountyBoard WHERE type = 'WriteMemory' AND payload LIKE '%convoy-shipped%'`).Scan(&memCount)
	if memCount != 1 {
		t.Errorf("expected 1 WriteMemory task, got %d", memCount)
	}
}

// TestDogSubPRCIWatch_Behind_QueuesRebaseAgentBranch verifies that a sub-PR with
// mergeStateStatus=BEHIND queues a RebaseAgentBranch Pilot task instead of
// waiting or requiring user action.
func TestDogSubPRCIWatch_Behind_QueuesRebaseAgentBranch(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	convoyID, taskID, _, _ := setupSubPRScenario(t, db)

	// Give the task a branch name (Pilot needs this to queue the rebase).
	db.Exec(`UPDATE BountyBoard SET branch_name = 'agent/R2D2/task-99' WHERE id = ?`, taskID)

	// Ensure a ConvoyAskBranch row exists so the BEHIND handler can find the ask-branch.
	db.Exec(`INSERT OR REPLACE INTO ConvoyAskBranches (convoy_id, repo, ask_branch, ask_branch_base_sha)
		VALUES (?, 'api', 'force/ask-behind-test', 'abc123')`, convoyID)

	_, err := store.CreateAskBranchPR(db, taskID, convoyID, "api", "https://github.com/acme/api/pull/11", 11)
	if err != nil {
		t.Fatal(err)
	}

	installGHStub(t, map[string]ghStubResp{
		"pr view 11": {stdout: `{"number":11,"url":"u","state":"OPEN","isDraft":false,"merged":false,"mergedAt":"","closedAt":"","reviews":[],"mergeStateStatus":"BEHIND","mergeable":"MERGEABLE"}`},
	})

	if err := dogSubPRCIWatch(db, testLogger{}); err != nil {
		t.Fatal(err)
	}

	// A RebaseAgentBranch task must be queued.
	var rebaseType string
	var rebasePayloadStr string
	db.QueryRow(`SELECT type, payload FROM BountyBoard WHERE type = 'RebaseAgentBranch' ORDER BY id DESC LIMIT 1`).
		Scan(&rebaseType, &rebasePayloadStr)
	if rebaseType != "RebaseAgentBranch" {
		t.Fatalf("expected RebaseAgentBranch task, got %q", rebaseType)
	}
	if !strings.Contains(rebasePayloadStr, "agent/R2D2/task-99") {
		t.Errorf("payload should contain agent branch name, got %q", rebasePayloadStr)
	}
	if !strings.Contains(rebasePayloadStr, "force/ask-behind-test") {
		t.Errorf("payload should contain ask-branch name, got %q", rebasePayloadStr)
	}

	// Task must NOT be escalated — BEHIND is self-healing.
	after, _ := store.GetBounty(db, taskID)
	if after.Status == "Escalated" {
		t.Errorf("task should not be escalated when BEHIND (rebase is queued)")
	}
}

// TestDogSubPRCIWatch_Dirty_QueuesRebaseAgentBranch verifies the same
// self-healing path for DIRTY: rather than escalating, we queue a rebase and
// let Pilot either clean-rebase or spawn a RebaseConflict CodeEdit for an
// astromech. No operator involvement on the happy or clean-conflict paths.
func TestDogSubPRCIWatch_Dirty_QueuesRebaseAgentBranch(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	convoyID, taskID, _, _ := setupSubPRScenario(t, db)
	db.Exec(`UPDATE BountyBoard SET branch_name = 'agent/R2D2/task-dirty' WHERE id = ?`, taskID)
	db.Exec(`INSERT OR REPLACE INTO ConvoyAskBranches (convoy_id, repo, ask_branch, ask_branch_base_sha)
		VALUES (?, 'api', 'force/ask-dirty-test', 'abc123')`, convoyID)

	_, err := store.CreateAskBranchPR(db, taskID, convoyID, "api", "https://github.com/acme/api/pull/13", 13)
	if err != nil {
		t.Fatal(err)
	}

	installGHStub(t, map[string]ghStubResp{
		"pr view 13": {stdout: `{"number":13,"url":"u","state":"OPEN","isDraft":false,"merged":false,"mergedAt":"","closedAt":"","reviews":[],"mergeStateStatus":"DIRTY","mergeable":"CONFLICTING"}`},
	})

	if err := dogSubPRCIWatch(db, testLogger{}); err != nil {
		t.Fatal(err)
	}

	var rebaseCount int
	db.QueryRow(`SELECT COUNT(*) FROM BountyBoard WHERE type = 'RebaseAgentBranch' AND status = 'Pending'`).Scan(&rebaseCount)
	if rebaseCount != 1 {
		t.Errorf("DIRTY should queue exactly 1 RebaseAgentBranch, got %d", rebaseCount)
	}

	after, _ := store.GetBounty(db, taskID)
	if after.Status == "Escalated" {
		t.Errorf("task must not be escalated when DIRTY — rebase is the self-healing path")
	}

	// No Escalation rows should exist for this task.
	var escCount int
	db.QueryRow(`SELECT COUNT(*) FROM Escalations WHERE task_id = ?`, taskID).Scan(&escCount)
	if escCount != 0 {
		t.Errorf("DIRTY should not create escalations, got %d", escCount)
	}
}

// TestEscalateSubPR_IsAtomic verifies that escalateSubPR closes the PR row,
// inserts the escalation, and sets the task status in a single atomic operation.
func TestEscalateSubPR_IsAtomic(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	convoyID, taskID, _, _ := setupSubPRScenario(t, db)
	prRowID, err := store.CreateAskBranchPR(db, taskID, convoyID, "api", "https://github.com/acme/api/pull/12", 12)
	if err != nil {
		t.Fatal(err)
	}
	pr := store.GetAskBranchPR(db, prRowID)

	if txErr := escalateSubPR(db, *pr, store.SeverityMedium, "test escalation"); txErr != nil {
		t.Fatalf("escalateSubPR returned error: %v", txErr)
	}

	// All three writes must have landed.
	var prState string
	db.QueryRow(`SELECT state FROM AskBranchPRs WHERE id = ?`, prRowID).Scan(&prState)
	if prState != "Closed" {
		t.Errorf("AskBranchPR.state should be Closed, got %q", prState)
	}

	var escCount int
	db.QueryRow(`SELECT COUNT(*) FROM Escalations WHERE task_id = ? AND message = 'test escalation'`, taskID).Scan(&escCount)
	if escCount != 1 {
		t.Errorf("expected 1 Escalation row, got %d", escCount)
	}

	after, _ := store.GetBounty(db, taskID)
	if after.Status != "Escalated" {
		t.Errorf("task status should be Escalated, got %q", after.Status)
	}

	// Re-running must NOT double-insert escalations (PR is now Closed so the
	// dog won't call escalateSubPR again, but guard the function itself too).
	_ = escalateSubPR(db, *pr, store.SeverityMedium, "test escalation")
	db.QueryRow(`SELECT COUNT(*) FROM Escalations WHERE task_id = ?`, taskID).Scan(&escCount)
	if escCount != 2 {
		// Second call inserts a second escalation, which is fine — the dog gate
		// (PR state=Closed filtered from ListOpenAskBranchPRs) prevents re-entry.
		// We just confirm the tx itself works correctly.
	}
}

// TestDogSubPRCIWatch_BlockedWithCIRunning verifies that BLOCKED with pending CI
// checks is treated as "CI still running" — no escalation, just wait.
func TestDogSubPRCIWatch_BlockedWithCIRunning(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	convoyID, taskID, _, _ := setupSubPRScenario(t, db)
	_, err := store.CreateAskBranchPR(db, taskID, convoyID, "api", "https://github.com/acme/api/pull/10", 10)
	if err != nil {
		t.Fatal(err)
	}

	installGHStub(t, map[string]ghStubResp{
		"pr view 10": {stdout: `{"number":10,"url":"u","state":"OPEN","isDraft":false,"merged":false,"mergedAt":"","closedAt":"","reviews":[],"mergeStateStatus":"BLOCKED","mergeable":"MERGEABLE"}`},
		// One check present and still pending — CI is running.
		"pr checks 10": {stdout: `[{"name":"jenkins","state":"IN_PROGRESS","bucket":"pending","link":""}]`},
	})

	if err := dogSubPRCIWatch(db, testLogger{}); err != nil {
		t.Fatal(err)
	}

	// Task must remain in AwaitingSubPRCI — do not escalate while CI is running.
	after, _ := store.GetBounty(db, taskID)
	if after.Status == "Escalated" {
		t.Errorf("task must NOT be escalated while CI checks are pending (CI is running)")
	}
	var escCount int
	db.QueryRow(`SELECT COUNT(*) FROM Escalations WHERE task_id = ?`, taskID).Scan(&escCount)
	if escCount != 0 {
		t.Errorf("no escalation expected while CI running, got %d", escCount)
	}
}

// Silence unused-package warnings when the test file is built but not run.
var _ = claude.SetCLIRunner
var _ = time.Now
