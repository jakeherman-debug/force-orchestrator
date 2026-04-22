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

// TestPrepareAgentBranch_FetchesWhenLocalAskBranchIsStale is the regression
// test for the 248-was-missing-247 bug. Scenario:
//
//  1. Task A is cut from the ask-branch, commits, its sub-PR merges into the
//     ask-branch on origin. Origin's ask-branch advances.
//  2. The local repo still has a tracking branch at the pre-merge tip (we
//     never ran `git fetch`).
//  3. Task B now branches off the same ask-branch name.
//
// Before the fix, PrepareAgentBranch used the local (stale) ref and task B
// missed task A's committed work entirely. After the fix, PrepareAgentBranch
// always fetches origin/<ask-branch> first and uses the remote ref.
func TestPrepareAgentBranch_FetchesWhenLocalAskBranchIsStale(t *testing.T) {
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

	// Create ask-branch at main's tip, push to origin, keep a local tracking
	// branch (simulating how Pilot's CreateAskBranch leaves things).
	exec.Command("git", "-C", wt, "checkout", "-b", "force/ask-42-demo").Run()
	exec.Command("git", "-C", wt, "push", "-u", "origin", "force/ask-42-demo").Run()
	exec.Command("git", "-C", wt, "checkout", "main").Run()
	// The local `force/ask-42-demo` tracking branch still exists, pointing at
	// the main-tip commit.

	// Out-of-band: simulate a sub-PR merging into origin's ask-branch by
	// cloning origin into a throwaway dir, committing, and pushing back. This
	// models what happens when Jedi Council auto-merges a sub-PR: origin
	// advances, but the local repo's tracking branch stays put.
	tmpClone := t.TempDir()
	if err := exec.Command("git", "clone", "-q", origin, tmpClone).Run(); err != nil {
		t.Fatal(err)
	}
	exec.Command("git", "-C", tmpClone, "config", "user.email", "t@t").Run()
	exec.Command("git", "-C", tmpClone, "config", "user.name", "Test").Run()
	exec.Command("git", "-C", tmpClone, "checkout", "-b", "force/ask-42-demo", "origin/force/ask-42-demo").Run()
	os.WriteFile(filepath.Join(tmpClone, "task-247-work.txt"), []byte("work"), 0644)
	exec.Command("git", "-C", tmpClone, "add", "task-247-work.txt").Run()
	exec.Command("git", "-C", tmpClone, "commit", "-q", "-m", "task 247 work").Run()
	exec.Command("git", "-C", tmpClone, "push", "origin", "force/ask-42-demo").Run()

	// At this point: origin's ask-branch is ahead, local ask-branch is stale.
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	agentWT, err := GetOrCreateAgentWorktree(db, "R3-S6", wt)
	if err != nil {
		t.Fatal(err)
	}

	// Task 248 branches off the ask-branch. With the fix, we fetch first and
	// use the remote ref — so 247's file should be present.
	branchName, _, err := PrepareAgentBranch(agentWT, wt, 248, "R3-S6", "", "force/ask-42-demo")
	if err != nil {
		t.Fatalf("PrepareAgentBranch: %v", err)
	}
	if branchName != "agent/R3-S6/task-248" {
		t.Errorf("unexpected branch name: %q", branchName)
	}
	// The critical assertion: task-247-work.txt must be present in the new
	// task branch. Before the fix, this file was missing because the local
	// ask-branch ref was stale.
	if _, statErr := os.Stat(filepath.Join(agentWT, "task-247-work.txt")); statErr != nil {
		t.Errorf("task 248 branch must carry task 247's merged work (stale local ask-branch bug): %v", statErr)
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
