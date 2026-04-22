package git

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"force-orchestrator/internal/store"
)

// TestPrepareAgentBranch_BranchesOffAskBranch proves the PR-flow invariant:
// when a convoy has an ask-branch, astromechs branch off the ask-branch,
// not main. The ask-branch may only exist on origin (Pilot pushes without
// creating a local tracking branch), so we verify the remote-resolution path.
func TestPrepareAgentBranch_BranchesOffAskBranch(t *testing.T) {
	// Set up origin with main + ask-branch.
	origin := t.TempDir()
	if err := exec.Command("git", "init", "-q", "--bare", "-b", "main", origin).Run(); err != nil {
		t.Fatal(err)
	}
	wt := t.TempDir()
	if err := exec.Command("git", "clone", "-q", origin, wt).Run(); err != nil {
		t.Fatal(err)
	}
	exec.Command("git", "-C", wt, "config", "user.email", "t@t").Run()
	exec.Command("git", "-C", wt, "config", "user.name", "Test").Run()
	os.WriteFile(filepath.Join(wt, "README"), []byte("hi"), 0644)
	exec.Command("git", "-C", wt, "add", ".").Run()
	exec.Command("git", "-C", wt, "commit", "-q", "-m", "initial").Run()
	exec.Command("git", "-C", wt, "push", "-u", "origin", "main").Run()
	exec.Command("git", "-C", wt, "remote", "set-head", "origin", "main").Run()

	// Put a commit on the ask-branch that is NOT on main, so we can prove the
	// task branch was cut from the ask-branch.
	exec.Command("git", "-C", wt, "checkout", "-b", "force/ask-42-demo").Run()
	os.WriteFile(filepath.Join(wt, "unique-to-ask.txt"), []byte("x"), 0644)
	exec.Command("git", "-C", wt, "add", "unique-to-ask.txt").Run()
	exec.Command("git", "-C", wt, "commit", "-q", "-m", "ask-branch commit").Run()
	exec.Command("git", "-C", wt, "push", "-u", "origin", "force/ask-42-demo").Run()
	// Reset local HEAD back to main so the worktree setup doesn't carry the
	// ask-branch commit locally — we want to force the remote-resolution path.
	exec.Command("git", "-C", wt, "checkout", "main").Run()
	exec.Command("git", "-C", wt, "branch", "-D", "force/ask-42-demo").Run()

	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	agentWT, err := GetOrCreateAgentWorktree(db, "R2-D2", wt)
	if err != nil {
		t.Fatal(err)
	}

	branchName, _, err := PrepareAgentBranch(agentWT, wt, 99, "R2-D2", "", "force/ask-42-demo")
	if err != nil {
		t.Fatalf("PrepareAgentBranch with ask-branch base: %v", err)
	}
	if branchName != "agent/R2-D2/task-99" {
		t.Errorf("unexpected branch name: %q", branchName)
	}
	// The new task branch must contain unique-to-ask.txt (proving it branched
	// off ask-branch, not main).
	if _, err := os.Stat(filepath.Join(agentWT, "unique-to-ask.txt")); err != nil {
		t.Errorf("task branch should carry ask-branch commits: %v", err)
	}
}

// TestPrepareAgentBranch_FallsBackWhenAskBranchUnresolvable proves the safety
// net: if the ask-branch can't be resolved (typo, deleted, etc.), we fall back
// to the default branch rather than failing the task.
func TestPrepareAgentBranch_FallsBackWhenAskBranchUnresolvable(t *testing.T) {
	dir := initTestRepo(t)
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	agentWT, err := GetOrCreateAgentWorktree(db, "BB-8", dir)
	if err != nil {
		t.Fatal(err)
	}
	branchName, _, err := PrepareAgentBranch(agentWT, dir, 7, "BB-8", "", "does/not/exist")
	if err != nil {
		t.Fatalf("should fall back rather than fail: %v", err)
	}
	if !strings.Contains(branchName, "agent/BB-8/task-7") {
		t.Errorf("unexpected branch: %q", branchName)
	}
}
