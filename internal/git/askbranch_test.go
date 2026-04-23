package git

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// makeOriginAndClone creates an origin repo with a main branch + one commit,
// then clones it into a working copy. Returns (workingCopyPath, originPath).
// This lets us test push/fetch against a real remote without hitting the net.
func makeOriginAndClone(t *testing.T) (worktree, origin string) {
	t.Helper()
	originDir := t.TempDir()
	if err := exec.Command("git", "init", "-q", "--bare", "-b", "main", originDir).Run(); err != nil {
		t.Fatal(err)
	}
	wt := t.TempDir()
	if err := exec.Command("git", "clone", "-q", originDir, wt).Run(); err != nil {
		t.Fatal(err)
	}
	exec.Command("git", "-C", wt, "config", "user.email", "t@t").Run()
	exec.Command("git", "-C", wt, "config", "user.name", "Test").Run()
	os.WriteFile(filepath.Join(wt, "README"), []byte("hi"), 0644)
	exec.Command("git", "-C", wt, "add", ".").Run()
	exec.Command("git", "-C", wt, "commit", "-q", "-m", "initial").Run()
	// Push main so origin has history.
	if out, err := exec.Command("git", "-C", wt, "push", "-u", "origin", "main").CombinedOutput(); err != nil {
		t.Fatalf("initial push failed: %s", strings.TrimSpace(string(out)))
	}
	// Set origin HEAD to main so GetDefaultBranch resolves correctly.
	exec.Command("git", "-C", wt, "remote", "set-head", "origin", "main").Run()
	return wt, originDir
}

// ── BranchNameSlug ───────────────────────────────────────────────────────────

func TestBranchNameSlug(t *testing.T) {
	cases := []struct {
		in, want string
		maxLen   int
	}{
		{"Add OAuth to api", "add-oauth-to-api", 40},
		{"[5] Fix the critical bug!!", "5-fix-the-critical-bug", 40},
		{"---", "ask", 40},
		{"", "ask", 40},
		{"CamelCase and spaces", "camelcase-and-spaces", 40},
		{"really-really-really-really-really-long-name", "really-really-really-really-really-long", 40},
		{"trim-trailing-dashes-", "trim-trailing-dashes", 40},
	}
	for _, c := range cases {
		got := BranchNameSlug(c.in, c.maxLen)
		if got != c.want {
			t.Errorf("BranchNameSlug(%q, %d) = %q, want %q", c.in, c.maxLen, got, c.want)
		}
	}
}

// ── CreateAskBranch ──────────────────────────────────────────────────────────

func TestCreateAskBranch_HappyPath(t *testing.T) {
	wt, origin := makeOriginAndClone(t)
	sha, err := CreateAskBranch(wt, "force/ask-1-test")
	if err != nil {
		t.Fatalf("CreateAskBranch: %v", err)
	}
	if len(sha) < 7 {
		t.Errorf("unexpected SHA: %q", sha)
	}
	// The branch exists on origin.
	out, err := exec.Command("git", "-C", origin, "branch", "--list", "force/ask-1-test").Output()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "force/ask-1-test") {
		t.Errorf("branch not on origin: %q", out)
	}
}

func TestCreateAskBranch_IdempotentOnRerun(t *testing.T) {
	wt, _ := makeOriginAndClone(t)
	sha1, err1 := CreateAskBranch(wt, "force/ask-1-test")
	if err1 != nil {
		t.Fatal(err1)
	}
	sha2, err2 := CreateAskBranch(wt, "force/ask-1-test")
	if err2 != nil {
		t.Fatalf("second run should succeed: %v", err2)
	}
	if sha1 != sha2 {
		t.Errorf("idempotent re-run should yield same SHA: %q vs %q", sha1, sha2)
	}
}

func TestCreateAskBranch_EmptyName(t *testing.T) {
	wt, _ := makeOriginAndClone(t)
	if _, err := CreateAskBranch(wt, ""); err == nil {
		t.Errorf("expected error for empty branch name")
	}
}

// ── DeleteAskBranch ──────────────────────────────────────────────────────────

func TestDeleteAskBranch_RemovesBothLocalAndRemote(t *testing.T) {
	wt, origin := makeOriginAndClone(t)
	_, _ = CreateAskBranch(wt, "force/ask-1-doomed")

	if err := DeleteAskBranch(wt, "force/ask-1-doomed"); err != nil {
		t.Fatalf("DeleteAskBranch: %v", err)
	}
	// Gone from origin.
	out, _ := exec.Command("git", "-C", origin, "branch", "--list", "force/ask-1-doomed").Output()
	if strings.TrimSpace(string(out)) != "" {
		t.Errorf("branch should be gone on origin, got: %q", out)
	}
	// Local delete should have already happened.
	out, _ = exec.Command("git", "-C", wt, "branch", "--list", "force/ask-1-doomed").Output()
	if strings.TrimSpace(string(out)) != "" {
		t.Errorf("branch should be gone locally, got: %q", out)
	}
}

func TestDeleteAskBranch_IdempotentOnMissingBranch(t *testing.T) {
	wt, _ := makeOriginAndClone(t)
	// Branch was never created — delete should succeed without error.
	if err := DeleteAskBranch(wt, "force/ask-1-neverexisted"); err != nil {
		t.Errorf("expected clean exit for missing branch: %v", err)
	}
}

// ── RemoteHeadSHA / FetchMain ────────────────────────────────────────────────

func TestRemoteHeadSHA(t *testing.T) {
	wt, _ := makeOriginAndClone(t)
	sha, err := RemoteHeadSHA(wt)
	if err != nil {
		t.Fatal(err)
	}
	if len(sha) != 40 {
		t.Errorf("expected 40-char SHA, got %q", sha)
	}
}

func TestFetchMain_MatchesRemoteHead(t *testing.T) {
	wt, _ := makeOriginAndClone(t)
	fetchSHA, err := FetchMain(wt)
	if err != nil {
		t.Fatal(err)
	}
	remoteSHA, err := RemoteHeadSHA(wt)
	if err != nil {
		t.Fatal(err)
	}
	if fetchSHA != remoteSHA {
		t.Errorf("FetchMain and RemoteHeadSHA disagree: %q vs %q", fetchSHA, remoteSHA)
	}
}

// ── RebaseBranchOnto ─────────────────────────────────────────────────────────

// advanceOriginMain creates a second commit on origin's main to simulate drift.
func advanceOriginMain(t *testing.T, wt string) string {
	t.Helper()
	os.WriteFile(filepath.Join(wt, "extra.txt"), []byte("new"), 0644)
	exec.Command("git", "-C", wt, "add", "extra.txt").Run()
	exec.Command("git", "-C", wt, "commit", "-q", "-m", "drift").Run()
	exec.Command("git", "-C", wt, "push", "origin", "main").Run()
	// Reset local main so subsequent operations don't carry extra commits.
	shaOut, _ := exec.Command("git", "-C", wt, "rev-parse", "HEAD").Output()
	return strings.TrimSpace(string(shaOut))
}

func TestRebaseBranchOnto_CleanRebaseAdvancesTip(t *testing.T) {
	wt, _ := makeOriginAndClone(t)
	_, err := CreateAskBranch(wt, "force/ask-1-clean")
	if err != nil {
		t.Fatal(err)
	}
	// Add a commit on the ask-branch (separate file so no conflict on rebase)
	// and push to origin — RebaseBranchOnto works off origin/<branch>, not
	// the local tracking ref, so unpushed commits are invisible to it. This
	// matches production reality where every commit reaches the ask-branch
	// via a push (astromechs push their agent branches, sub-PRs merge on
	// GitHub which pushes to origin/<ask-branch>).
	exec.Command("git", "-C", wt, "checkout", "force/ask-1-clean").Run()
	os.WriteFile(filepath.Join(wt, "feature.txt"), []byte("feat"), 0644)
	exec.Command("git", "-C", wt, "add", "feature.txt").Run()
	exec.Command("git", "-C", wt, "commit", "-q", "-m", "add feature").Run()
	exec.Command("git", "-C", wt, "push", "origin", "force/ask-1-clean").Run()

	// Now advance main.
	exec.Command("git", "-C", wt, "checkout", "main").Run()
	advanceOriginMain(t, wt)

	// Rebase the ask-branch onto main.
	newTip, err := RebaseBranchOnto(wt, "force/ask-1-clean", "main")
	if err != nil {
		t.Fatalf("rebase should succeed (non-conflicting): %v", err)
	}
	if len(newTip) != 40 {
		t.Errorf("unexpected new tip: %q", newTip)
	}
	// The new tip must contain both feature.txt and extra.txt.
	exec.Command("git", "-C", wt, "checkout", "force/ask-1-clean").Run()
	for _, f := range []string{"feature.txt", "extra.txt"} {
		if _, err := os.Stat(filepath.Join(wt, f)); err != nil {
			t.Errorf("after rebase, expected %s to be present: %v", f, err)
		}
	}
}

func TestRebaseBranchOnto_ConflictReturnsErrorAndAborts(t *testing.T) {
	wt, _ := makeOriginAndClone(t)
	_, _ = CreateAskBranch(wt, "force/ask-1-conflict")

	// Ask-branch modifies README, then pushes (RebaseBranchOnto operates on
	// origin/<branch>, not the local ref).
	exec.Command("git", "-C", wt, "checkout", "force/ask-1-conflict").Run()
	os.WriteFile(filepath.Join(wt, "README"), []byte("ask-branch version"), 0644)
	exec.Command("git", "-C", wt, "add", "README").Run()
	exec.Command("git", "-C", wt, "commit", "-q", "-m", "ask branch modifies README").Run()
	exec.Command("git", "-C", wt, "push", "origin", "force/ask-1-conflict").Run()

	// Main ALSO modifies README.
	exec.Command("git", "-C", wt, "checkout", "main").Run()
	os.WriteFile(filepath.Join(wt, "README"), []byte("main-drift version"), 0644)
	exec.Command("git", "-C", wt, "add", "README").Run()
	exec.Command("git", "-C", wt, "commit", "-q", "-m", "main version").Run()
	exec.Command("git", "-C", wt, "push", "origin", "main").Run()

	// Rebase must fail with an error message.
	_, err := RebaseBranchOnto(wt, "force/ask-1-conflict", "main")
	if err == nil {
		t.Fatal("expected rebase conflict to return error")
	}
	if !strings.Contains(err.Error(), "rebase") {
		t.Errorf("error should mention rebase: %v", err)
	}
	// After the error, repo must NOT be mid-rebase — abort must have run.
	out, _ := exec.Command("git", "-C", wt, "status", "--porcelain=v2", "--branch").Output()
	if strings.Contains(string(out), "rebase in progress") {
		t.Errorf("repo left in rebase state: %s", out)
	}
}

// TestRebaseBranchOnto_StaleLocalBranchDoesNotLoseMergeCommits is the
// regression test for the task-292 silent-data-loss bug. Scenario:
//
//  1. Pilot creates ask-branch at main's tip; local and origin both at A.
//  2. Astromechs commit to their agent branches, open sub-PRs into the
//     ask-branch. Sub-PRs auto-merge on origin — origin's ask-branch tip
//     advances from A to A+mergecommits. The LOCAL tracking branch stays
//     at A because nothing on the local side ever re-fetched.
//  3. main-drift-watch detects main has moved; queues RebaseAskBranch.
//  4. RebaseBranchOnto runs: `git fetch` updates origin/ask-branch, but
//     without the -B flag on worktree-add, the worktree checks out the
//     local ref (still at A). Rebase from A onto new main replays zero
//     commits. Force-push-with-lease succeeds (no concurrent writer since
//     fetch) and clobbers origin's merge commits with A.
//
// After the fix, the worktree checks out origin/<branch> directly, so the
// rebase has the real branch contents (A + merge commits) and preserves them.
func TestRebaseBranchOnto_StaleLocalBranchDoesNotLoseMergeCommits(t *testing.T) {
	wt, origin := makeOriginAndClone(t)
	_, err := CreateAskBranch(wt, "force/ask-stale-test")
	if err != nil {
		t.Fatal(err)
	}

	// Simulate sub-PR merges landing on origin's ask-branch out-of-band
	// (via a throwaway clone) — this is the moral equivalent of GitHub's
	// auto-merge. The LOCAL ask-branch ref is not touched.
	tmp := t.TempDir()
	if err := exec.Command("git", "clone", "-q", origin, tmp).Run(); err != nil {
		t.Fatal(err)
	}
	exec.Command("git", "-C", tmp, "config", "user.email", "t@t").Run()
	exec.Command("git", "-C", tmp, "config", "user.name", "Test").Run()
	exec.Command("git", "-C", tmp, "checkout", "-b", "force/ask-stale-test", "origin/force/ask-stale-test").Run()
	os.WriteFile(filepath.Join(tmp, "sub-pr-work.txt"), []byte("work"), 0644)
	exec.Command("git", "-C", tmp, "add", "sub-pr-work.txt").Run()
	exec.Command("git", "-C", tmp, "commit", "-q", "-m", "simulated sub-PR merge").Run()
	exec.Command("git", "-C", tmp, "push", "origin", "force/ask-stale-test").Run()

	// Advance origin/main too, so the rebase has work to do onto main.
	advanceOriginMain(t, wt)

	// Now run the rebase from the "operator repo" (wt) — where the local
	// ask-branch ref is still stale at the original creation point.
	newTip, err := RebaseBranchOnto(wt, "force/ask-stale-test", "main")
	if err != nil {
		t.Fatalf("rebase should succeed (non-conflicting): %v", err)
	}

	// Critical assertion: the rebased branch must INCLUDE sub-pr-work.txt.
	// Before the fix, the branch would silently lose it because the stale
	// local ref didn't have it.
	exec.Command("git", "-C", wt, "fetch", "origin", "force/ask-stale-test").Run()
	showOut, _ := exec.Command("git", "-C", wt, "show", newTip+":sub-pr-work.txt").CombinedOutput()
	if strings.TrimSpace(string(showOut)) != "work" {
		t.Errorf("rebased tip must preserve sub-PR merge content; got show=%q", string(showOut))
	}
}

// ── TriggerCIRerun ───────────────────────────────────────────────────────────

// TestTriggerCIRerun_PushesEmptyCommit verifies the empty-commit push
// succeeds and origin's branch tip advances. Deliberately checks ORIGIN
// state (not the local branch) because the plumbing-based implementation
// never updates the local branch — that's the whole point: it leaves any
// checkout of the target branch in other worktrees untouched, which was
// the production bug ("branch is already used by worktree at ...").
func TestTriggerCIRerun_PushesEmptyCommit(t *testing.T) {
	wt, origin := makeOriginAndClone(t)
	_, _ = CreateAskBranch(wt, "force/ask-1-rerun")

	beforeOut, _ := exec.Command("git", "-C", origin, "rev-parse", "force/ask-1-rerun").Output()
	before := strings.TrimSpace(string(beforeOut))

	if err := TriggerCIRerun(wt, "force/ask-1-rerun", "ci: retrigger"); err != nil {
		t.Fatalf("TriggerCIRerun: %v", err)
	}

	afterOut, _ := exec.Command("git", "-C", origin, "rev-parse", "force/ask-1-rerun").Output()
	after := strings.TrimSpace(string(afterOut))
	if after == before {
		t.Errorf("branch tip unchanged after re-trigger (before=%s after=%s)", before, after)
	}

	// Verify on origin: new tip is an empty commit (tree matches parent tree)
	// and carries our message. Use `git -C origin show` directly on the SHA.
	treeOut, _ := exec.Command("git", "-C", origin, "show", "-s", "--format=%T", after).Output()
	parentTreeOut, _ := exec.Command("git", "-C", origin, "show", "-s", "--format=%T", before).Output()
	if strings.TrimSpace(string(treeOut)) != strings.TrimSpace(string(parentTreeOut)) {
		t.Errorf("new commit should be empty (same tree as parent): tip=%q parent=%q", treeOut, parentTreeOut)
	}
	msgOut, _ := exec.Command("git", "-C", origin, "show", "-s", "--format=%s", after).Output()
	if !strings.Contains(string(msgOut), "ci: retrigger") {
		t.Errorf("commit message mismatch: %q", msgOut)
	}
}

// TestTriggerCIRerun_DoesNotMoveLocalBranch is the direct regression test
// for task 445's failure: previously the function checked out the target
// branch in the main worktree, which fails when an astromech has the same
// branch checked out in a persistent worktree. The plumbing version must
// leave the local branch ref completely untouched.
func TestTriggerCIRerun_DoesNotMoveLocalBranch(t *testing.T) {
	wt, _ := makeOriginAndClone(t)
	_, _ = CreateAskBranch(wt, "force/ask-1-nolocal")

	localBefore, _ := exec.Command("git", "-C", wt, "rev-parse", "force/ask-1-nolocal").Output()
	headBefore, _ := exec.Command("git", "-C", wt, "rev-parse", "HEAD").Output()

	if err := TriggerCIRerun(wt, "force/ask-1-nolocal", "ci: retrigger"); err != nil {
		t.Fatalf("TriggerCIRerun: %v", err)
	}

	localAfter, _ := exec.Command("git", "-C", wt, "rev-parse", "force/ask-1-nolocal").Output()
	headAfter, _ := exec.Command("git", "-C", wt, "rev-parse", "HEAD").Output()
	if string(localBefore) != string(localAfter) {
		t.Errorf("local branch ref moved unexpectedly: before=%s after=%s", localBefore, localAfter)
	}
	if string(headBefore) != string(headAfter) {
		t.Errorf("HEAD moved unexpectedly: before=%s after=%s", headBefore, headAfter)
	}
}

func TestTriggerCIRerun_DefaultMessageWhenEmpty(t *testing.T) {
	wt, origin := makeOriginAndClone(t)
	_, _ = CreateAskBranch(wt, "force/ask-1-default-msg")

	if err := TriggerCIRerun(wt, "force/ask-1-default-msg", ""); err != nil {
		t.Fatalf("TriggerCIRerun: %v", err)
	}
	tipOut, _ := exec.Command("git", "-C", origin, "rev-parse", "force/ask-1-default-msg").Output()
	msgOut, _ := exec.Command("git", "-C", origin, "show", "-s", "--format=%s", strings.TrimSpace(string(tipOut))).Output()
	if !strings.Contains(string(msgOut), "ci:") {
		t.Errorf("default message should contain 'ci:' prefix; got %q", msgOut)
	}
}

// ── MergeWithUnionStrategy ───────────────────────────────────────────────────

// TestMergeWithUnionStrategy_AppendOnlyConflictResolves is the direct
// regression for tasks 519 and 537 — both Claude-CLI-failed 5 times trying
// to resolve a CLAUDE.md conflict where both sides appended new content.
// Union strategy concatenates those additions deterministically, no LLM.
func TestMergeWithUnionStrategy_AppendOnlyConflictResolves(t *testing.T) {
	wt, _ := makeOriginAndClone(t)
	run := func(args ...string) {
		cmd := exec.Command("git", append([]string{"-C", wt}, args...)...)
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=T", "GIT_AUTHOR_EMAIL=t@t",
			"GIT_COMMITTER_NAME=T", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %s", args, string(out))
		}
	}

	// Create a file both branches will append to (simulates CLAUDE.md).
	os.WriteFile(filepath.Join(wt, "DOC.md"), []byte("shared\n"), 0644)
	run("add", ".")
	run("commit", "-q", "-m", "base")
	run("push", "origin", "main")

	// Branch: appends a line at the bottom.
	run("checkout", "-b", "feature")
	os.WriteFile(filepath.Join(wt, "DOC.md"), []byte("shared\nfeature line\n"), 0644)
	run("add", ".")
	run("commit", "-q", "-m", "feature: append")

	// main: appends a different line.
	run("checkout", "main")
	os.WriteFile(filepath.Join(wt, "DOC.md"), []byte("shared\nmain line\n"), 0644)
	run("add", ".")
	run("commit", "-q", "-m", "main: append")
	run("push", "origin", "main")

	// Push the feature branch too so origin has it (temp worktree needs
	// origin/<branch> as the reset source).
	run("push", "-u", "origin", "feature")

	tipSHA, err := MergeWithUnionStrategy(wt, "feature", "refs/remotes/origin/main", "union integrate")
	if err != nil {
		t.Fatalf("expected union merge to resolve append-only conflict, got: %v", err)
	}
	if tipSHA == "" {
		t.Error("expected non-empty tip SHA on success")
	}

	// The local `feature` ref should now point at the merge tip, and the
	// merged tree should contain both sides' additions.
	refOut, _ := exec.Command("git", "-C", wt, "rev-parse", "feature").Output()
	if strings.TrimSpace(string(refOut)) != tipSHA {
		t.Errorf("local feature ref (%s) doesn't match returned tip (%s)",
			strings.TrimSpace(string(refOut)), tipSHA)
	}
	contents, _ := exec.Command("git", "-C", wt, "show", tipSHA+":DOC.md").Output()
	if !strings.Contains(string(contents), "feature line") {
		t.Error("feature's line missing from union-merged file")
	}
	if !strings.Contains(string(contents), "main line") {
		t.Error("main's line missing from union-merged file")
	}
	if strings.Contains(string(contents), "<<<<<<<") {
		t.Error("merge should have completed cleanly, not left conflict markers")
	}
}

// TestMergeWithUnionStrategy_StructuralConflictStillFails verifies we do NOT
// silently resolve binary conflicts or concurrent same-function edits via
// union — the caller must still fall through to LLM-driven resolution for
// anything genuinely needing judgement.
func TestMergeWithUnionStrategy_StructuralConflictStillFails(t *testing.T) {
	wt, _ := makeOriginAndClone(t)
	run := func(args ...string) {
		cmd := exec.Command("git", append([]string{"-C", wt}, args...)...)
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=T", "GIT_AUTHOR_EMAIL=t@t",
			"GIT_COMMITTER_NAME=T", "GIT_COMMITTER_EMAIL=t@t")
		cmd.Run()
	}
	// Binary conflict: both sides write fully-different bytes to the same
	// file. Union can't resolve this — it's not append-only.
	os.WriteFile(filepath.Join(wt, "bin.dat"), []byte{0, 1, 2, 3}, 0644)
	run("add", ".")
	run("commit", "-q", "-m", "base bin")
	run("push", "origin", "main")

	run("checkout", "-b", "feature")
	os.WriteFile(filepath.Join(wt, "bin.dat"), []byte{9, 9, 9, 9}, 0644)
	run("add", ".")
	run("commit", "-q", "-m", "feature: overwrite bin")

	run("checkout", "main")
	os.WriteFile(filepath.Join(wt, "bin.dat"), []byte{7, 7, 7, 7}, 0644)
	run("add", ".")
	run("commit", "-q", "-m", "main: overwrite bin")
	run("push", "origin", "main")

	run("checkout", "feature")
	run("push", "-u", "origin", "feature")
	if _, err := MergeWithUnionStrategy(wt, "feature", "refs/remotes/origin/main", ""); err == nil {
		t.Error("binary conflict should NOT be silently resolved by union merge")
	}
	// The caller's main worktree should be untouched (temp worktree pattern).
	out, _ := exec.Command("git", "-C", wt, "status", "--porcelain").Output()
	if strings.Contains(string(out), "UU") {
		t.Error("failed union merge should not have leaked state into caller's worktree: " + string(out))
	}
}

// ── ForcePushBranch ──────────────────────────────────────────────────────────

func TestForcePushBranch_SucceedsAfterLocalCommit(t *testing.T) {
	wt, origin := makeOriginAndClone(t)
	_, _ = CreateAskBranch(wt, "force/ask-1-push")

	exec.Command("git", "-C", wt, "checkout", "force/ask-1-push").Run()
	os.WriteFile(filepath.Join(wt, "new.txt"), []byte("x"), 0644)
	exec.Command("git", "-C", wt, "add", "new.txt").Run()
	exec.Command("git", "-C", wt, "commit", "-q", "-m", "add new").Run()

	if err := ForcePushBranch(wt, "force/ask-1-push"); err != nil {
		t.Fatalf("force-push: %v", err)
	}
	// Origin should have the new commit.
	out, _ := exec.Command("git", "-C", origin, "log", "--oneline", "force/ask-1-push").Output()
	if !strings.Contains(string(out), "add new") {
		t.Errorf("force-push didn't land: %s", out)
	}
}
