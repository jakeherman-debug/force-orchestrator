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

// TestCompleteAskBranchResolution_ForcePushesAndUpdatesSHA verifies the
// rebase-conflict-resolution path: when Jedi Council approves a task whose
// branch_name == the convoy's ask-branch, openSubPRForApprovedTask must NOT
// open a sub-PR. Instead it must force-push the resolved ask-branch and
// update the stored base SHA. Regression test for the original gap where
// Jedi would have tried to open a PR with head=base=ask-branch.
func TestCompleteAskBranchResolution_ForcePushesAndUpdatesSHA(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	// Set up: origin + clone + ask-branch with resolution commit.
	origin := t.TempDir()
	if err := exec.Command("git", "init", "-q", "--bare", "-b", "main", origin).Run(); err != nil {
		t.Fatal(err)
	}
	repoDir := t.TempDir()
	if err := exec.Command("git", "clone", "-q", origin, repoDir).Run(); err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) {
		cmd := exec.Command("git", append([]string{"-C", repoDir}, args...)...)
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=T", "GIT_AUTHOR_EMAIL=t@t",
			"GIT_COMMITTER_NAME=T", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %s", args, string(out))
		}
	}
	run("config", "user.email", "t@t")
	run("config", "user.name", "Test")
	os.WriteFile(filepath.Join(repoDir, "README"), []byte("hi\n"), 0644)
	run("add", ".")
	run("commit", "-m", "initial")
	run("push", "-u", "origin", "main")
	run("remote", "set-head", "origin", "main")

	// Ask-branch exists on origin at initial SHA.
	initialSHA, _ := exec.Command("git", "-C", repoDir, "rev-parse", "HEAD").Output()
	store.AddRepo(db, "api", repoDir, "")
	_ = store.SetRepoRemoteInfo(db, "api", "https://github.com/acme/api.git", "main")

	cid, _ := store.CreateConvoy(db, "[1] conflict-resolution")
	_ = store.UpsertConvoyAskBranch(db, cid, "api", "force/ask-1-conflict-resolution",
		strings.TrimSpace(string(initialSHA)))
	// Push the ask-branch to origin so the force-push has something to compare.
	run("push", "origin", "main:refs/heads/force/ask-1-conflict-resolution")

	// Astromech "resolves conflict" by committing onto the ask-branch locally.
	run("checkout", "-b", "force/ask-1-conflict-resolution", "main")
	os.WriteFile(filepath.Join(repoDir, "resolved.txt"), []byte("conflict markers fixed"), 0644)
	run("add", ".")
	run("commit", "-m", "resolve rebase conflicts")
	resolvedSHA, _ := exec.Command("git", "-C", repoDir, "rev-parse", "HEAD").Output()

	// Task that claims to be on the ask-branch.
	tid, _ := store.AddConvoyTask(db, 0, "api", "[REBASE_CONFLICT...] resolve", cid, 0, "Pending")
	store.SetBranchName(db, tid, "force/ask-1-conflict-resolution")
	bounty, _ := store.GetBounty(db, tid)

	errClass, err := openSubPRForApprovedTask(db, bounty, "Council-Yoda",
		"force/ask-1-conflict-resolution", "title", "body", log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatalf("resolution path should succeed, got err=%v class=%s", err, errClass)
	}

	// Task must be Completed (no sub-PR, no AwaitingSubPRCI).
	updated, _ := store.GetBounty(db, tid)
	if updated.Status != "Completed" {
		t.Errorf("task should be Completed after ask-branch resolution, got %q", updated.Status)
	}
	// No AskBranchPR row should have been created.
	if store.GetAskBranchPRByTask(db, tid) != nil {
		t.Error("ask-branch resolution must NOT create a sub-PR")
	}
	// Stored base SHA should be the resolved SHA.
	ab := store.GetConvoyAskBranch(db, cid, "api")
	if ab.AskBranchBaseSHA != strings.TrimSpace(string(resolvedSHA)) {
		t.Errorf("base SHA should be updated to resolved tip: got %q, want %q",
			ab.AskBranchBaseSHA, strings.TrimSpace(string(resolvedSHA)))
	}
	if ab.LastRebasedAt == "" {
		t.Error("last_rebased_at should be stamped")
	}

	// Origin should have received the resolved branch.
	out, _ := exec.Command("git", "ls-remote", origin, "force/ask-1-conflict-resolution").Output()
	if !strings.Contains(string(out), strings.TrimSpace(string(resolvedSHA))) {
		t.Errorf("origin should have resolved SHA, got: %s", out)
	}
}
