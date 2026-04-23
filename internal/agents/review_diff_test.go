package agents

import (
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"force-orchestrator/internal/store"
)

// makeRepoWithAskBranch constructs a real git setup that mimics production
// shape: a bare origin, a clone, main with an initial commit, an ask-branch
// with one commit on top, and (optionally) additional commits on main AFTER
// the ask-branch was cut. Returns the local repo path and the SHA on main
// at the moment the ask-branch was cut (the base SHA).
func makeRepoWithAskBranch(t *testing.T, askBranch string, extraMainCommits int) (repoPath, baseSHA string) {
	t.Helper()
	origin := t.TempDir()
	if err := exec.Command("git", "init", "-q", "--bare", "-b", "main", origin).Run(); err != nil {
		t.Fatal(err)
	}
	repoPath = t.TempDir()
	if err := exec.Command("git", "clone", "-q", origin, repoPath).Run(); err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) {
		cmd := exec.Command("git", append([]string{"-C", repoPath}, args...)...)
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=T", "GIT_AUTHOR_EMAIL=t@t",
			"GIT_COMMITTER_NAME=T", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %s", args, string(out))
		}
	}
	run("config", "user.email", "t@t")
	run("config", "user.name", "T")
	os.WriteFile(filepath.Join(repoPath, "README"), []byte("hi"), 0644)
	run("add", ".")
	run("commit", "-q", "-m", "initial")
	run("push", "-u", "origin", "main")
	run("remote", "set-head", "origin", "main")

	// Capture base SHA at ask-branch cut point.
	shaOut, _ := exec.Command("git", "-C", repoPath, "rev-parse", "HEAD").Output()
	baseSHA = strings.TrimSpace(string(shaOut))

	// Cut the ask-branch and put one commit on it.
	run("checkout", "-b", askBranch)
	os.WriteFile(filepath.Join(repoPath, "feature.txt"), []byte("ask-branch work\n"), 0644)
	run("add", ".")
	run("commit", "-q", "-m", "ask-branch: add feature")
	run("push", "-u", "origin", askBranch)

	// Advance main with extra commits AFTER the ask-branch was cut — this
	// simulates the "main drift" that produces phantom additions in 3-dot
	// diffs against main's tip.
	if extraMainCommits > 0 {
		run("checkout", "main")
		for i := 0; i < extraMainCommits; i++ {
			fname := filepath.Join(repoPath, "main-drift-"+string(rune('a'+i))+".txt")
			os.WriteFile(fname, []byte("main drift\n"), 0644)
			run("add", ".")
			run("commit", "-q", "-m", "main: drift commit")
		}
		run("push", "origin", "main")
		run("checkout", askBranch)
	}
	return repoPath, baseSHA
}

// TestReviewDiff_UsesAskBranchNotMain is the direct regression test for the
// convoy 35/37 bleed: when a bounty's convoy has an ask-branch, reviewDiff
// MUST compare against the ask-branch tip, not main. Otherwise main's drift
// shows up as phantom scope violations in the reviewer's prompt.
func TestReviewDiff_UsesAskBranchNotMain(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	repoPath, baseSHA := makeRepoWithAskBranch(t, "force/ask-1-feature", 3)
	store.AddRepo(db, "api", repoPath, "")
	_ = store.SetRepoRemoteInfo(db, "api", "https://github.com/acme/api.git", "main")
	cid, _ := store.CreateConvoy(db, "[1] feature")
	_ = store.UpsertConvoyAskBranch(db, cid, "api", "force/ask-1-feature", baseSHA)

	// Create an agent branch off the ask-branch with a tiny change — this is
	// what a normal astromech attempt looks like after the ask-branch has
	// already accumulated work AND main has drifted.
	run := func(args ...string) {
		cmd := exec.Command("git", append([]string{"-C", repoPath}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=T", "GIT_AUTHOR_EMAIL=t@t",
			"GIT_COMMITTER_NAME=T", "GIT_COMMITTER_EMAIL=t@t")
		_ = cmd.Run()
	}
	run("checkout", "-b", "agent/R2-D2/task-1", "force/ask-1-feature")
	os.WriteFile(filepath.Join(repoPath, "agent-work.txt"), []byte("my change\n"), 0644)
	run("add", ".")
	run("commit", "-q", "-m", "agent: my small change")

	b := &store.Bounty{
		ID:         99,
		ConvoyID:   cid,
		TargetRepo: "api",
		BranchName: "agent/R2-D2/task-1",
	}

	diff := reviewDiff(db, repoPath, b)

	// The reviewer's diff MUST include the agent's file.
	if !strings.Contains(diff, "agent-work.txt") {
		t.Errorf("agent's own file missing from diff: %q", diff)
	}
	// It must NOT include the main-drift files (those are out-of-scope from
	// the agent's perspective — they're phantom additions when the 3-dot
	// base is main's tip instead of the ask-branch).
	if strings.Contains(diff, "main-drift-") {
		t.Error("reviewDiff contained main's drift as phantom additions — scope-violation trap re-opened")
	}
	// Must also not include the ask-branch's earlier work (that's accumulated
	// convoy work, not this task's scope).
	if strings.Contains(diff, "feature.txt") {
		t.Error("reviewDiff contained the ask-branch's prior task work — wrong base")
	}
}

// TestReviewDiff_FallsBackToMainWithoutAskBranch covers the legacy path:
// when a bounty has no convoy or the convoy has no ask-branch, diff against
// main is the right semantic.
func TestReviewDiff_FallsBackToMainWithoutAskBranch(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	repoPath, _ := makeRepoWithAskBranch(t, "force/unused", 0)
	store.AddRepo(db, "api", repoPath, "")

	run := func(args ...string) {
		cmd := exec.Command("git", append([]string{"-C", repoPath}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=T", "GIT_AUTHOR_EMAIL=t@t",
			"GIT_COMMITTER_NAME=T", "GIT_COMMITTER_EMAIL=t@t")
		_ = cmd.Run()
	}
	run("checkout", "-b", "agent/legacy-task", "main")
	os.WriteFile(filepath.Join(repoPath, "legacy.txt"), []byte("legacy\n"), 0644)
	run("add", ".")
	run("commit", "-q", "-m", "legacy")

	b := &store.Bounty{
		ID:         100,
		ConvoyID:   0, // no convoy
		TargetRepo: "api",
		BranchName: "agent/legacy-task",
	}
	diff := reviewDiff(db, repoPath, b)
	if !strings.Contains(diff, "legacy.txt") {
		t.Error("legacy path should still produce a meaningful diff")
	}
}

// TestReviewCommitsAhead_OnlyCountsAgentCommits verifies the companion
// helper for auto-complete checks — "no commits ahead of the ask-branch"
// means this task added nothing, not "no commits ahead of main."
func TestReviewCommitsAhead_OnlyCountsAgentCommits(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	repoPath, baseSHA := makeRepoWithAskBranch(t, "force/ask-2-feature", 0)
	store.AddRepo(db, "api", repoPath, "")
	_ = store.SetRepoRemoteInfo(db, "api", "https://github.com/acme/api.git", "main")
	cid, _ := store.CreateConvoy(db, "[2] feature")
	_ = store.UpsertConvoyAskBranch(db, cid, "api", "force/ask-2-feature", baseSHA)

	// Agent branch is at the ask-branch tip — no net-new work.
	run := func(args ...string) {
		exec.Command("git", append([]string{"-C", repoPath}, args...)...).Run()
	}
	run("checkout", "-b", "agent/noop", "force/ask-2-feature")

	b := &store.Bounty{
		ID: 1, ConvoyID: cid, TargetRepo: "api", BranchName: "agent/noop",
	}

	// Against ask-branch: no unique commits — should be empty, signaling auto-complete.
	if got := reviewCommitsAhead(db, repoPath, b); got != "" {
		t.Errorf("expected no commits ahead of ask-branch, got %q", got)
	}
}

// TestAutoInsertReshardTasks_BypassesChancellor is the direct regression
// test for the tasks 533/541 stuck-in-AwaitingChancellorReview production
// bug. When Commander runs a Decompose with the INFRA_FAILURE_RESHARD
// marker, shards must land in the parent's convoy as Pending CodeEdits —
// NEVER in ProposedConvoys / AwaitingChancellorReview.
func TestAutoInsertReshardTasks_BypassesChancellor(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	store.AddRepo(db, "api", "/tmp/api", "")
	cid, _ := store.CreateConvoy(db, "[1] t")
	// Parent — the failed task that triggered the auto-reshard.
	parentID, _ := store.AddConvoyTask(db, 0, "api", "oversized task", cid, 7, "Pending")
	parent, _ := store.GetBounty(db, parentID)
	db.Exec(`UPDATE BountyBoard SET status = 'Failed' WHERE id = ?`, parentID)

	// Fake a Commander-run on a Decompose carrying the INFRA_FAILURE_RESHARD marker.
	decomposeRes, _ := db.Exec(`INSERT INTO BountyBoard
		(parent_id, target_repo, type, status, payload, convoy_id, priority, created_at)
		VALUES (?, 'api', 'Decompose', 'Locked',
		        '[INFRA_FAILURE_RESHARD from task #' || ? || ']', ?, ?, datetime('now'))`,
		parentID, parentID, cid, 7)
	decomposeID, _ := decomposeRes.LastInsertId()
	decompose, _ := store.GetBounty(db, int(decomposeID))

	plan := []store.TaskPlan{
		{TempID: 1, Repo: "api", Task: "smaller shard 1", BlockedBy: nil},
		{TempID: 2, Repo: "api", Task: "smaller shard 2", BlockedBy: []int{1}},
	}

	silencedLogger := log.New(io.Discard, "", 0)
	if err := autoInsertReshardTasks(db, decompose, parent, plan, "", "Commander-1", "sess", silencedLogger); err != nil {
		t.Fatalf("autoInsertReshardTasks: %v", err)
	}

	// Decompose should be Completed.
	d, _ := store.GetBounty(db, int(decomposeID))
	if d.Status != "Completed" {
		t.Errorf("decompose should be Completed, got %q", d.Status)
	}
	// NO ProposedConvoys row — we bypassed that path.
	var proposedCount int
	db.QueryRow(`SELECT COUNT(*) FROM ProposedConvoys WHERE feature_id = ?`, decomposeID).Scan(&proposedCount)
	if proposedCount != 0 {
		t.Errorf("auto-reshard must NOT use ProposedConvoys; got %d row(s)", proposedCount)
	}
	// Shards exist in the parent's convoy.
	var shardCount int
	db.QueryRow(`SELECT COUNT(*) FROM BountyBoard
		WHERE type = 'CodeEdit' AND convoy_id = ? AND status = 'Pending' AND parent_id = ?`,
		cid, decomposeID).Scan(&shardCount)
	if shardCount != 2 {
		t.Errorf("expected 2 shards Pending on convoy %d, got %d", cid, shardCount)
	}
	// Each shard carries the [RESHARD from task #N] prefix.
	var payload string
	db.QueryRow(`SELECT payload FROM BountyBoard WHERE parent_id = ? LIMIT 1`, decomposeID).Scan(&payload)
	if !strings.Contains(payload, "[RESHARD from task #") {
		t.Errorf("shard payload missing reshard marker: %q", payload)
	}
}

