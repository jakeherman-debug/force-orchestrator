package git

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Fix #0 coverage expansion — permanent regression tests for the protected-
// branch guard installed at every destructive git op. These complement the
// audit-test pattern file by exercising each mechanism directly (validator,
// integration against a real git repo, plus an ingress test) so future
// refactors that accidentally bypass AssertNotDefaultBranch fail here
// instead of in production.

// ── Unit: validator behaviour ───────────────────────────────────────────────

// TestAssertNotDefaultBranch_HardDenylist covers the static denylist path.
// This runs with repoPath="" so GetDefaultBranch is never consulted — any
// caller passing "main"/"master"/... gets a direct ErrProtectedBranch regardless
// of whether the repo on disk agrees.
func TestAssertNotDefaultBranch_HardDenylist(t *testing.T) {
	t.Parallel()
	denied := []string{
		"main", "master", "develop", "trunk", "production", "prod", "HEAD",
		"MAIN", "Master", // case-insensitive
		"refs/heads/main",         // fully-qualified ref
		"refs/remotes/origin/main", // remote-tracking ref
		"origin/master",           // remote-shortform ref
		" main ",                  // leading/trailing whitespace
		"",                        // empty
	}
	for _, branch := range denied {
		branch := branch
		t.Run("denies_"+branch, func(t *testing.T) {
			t.Parallel()
			err := AssertNotDefaultBranch("", branch)
			if err == nil {
				t.Fatalf("AssertNotDefaultBranch(%q) returned nil; want ErrProtectedBranch", branch)
			}
			if !strings.Contains(err.Error(), "protected") {
				t.Errorf("error does not mention 'protected': %v", err)
			}
		})
	}
}

// TestAssertNotDefaultBranch_AllowsAskBranches covers the happy path: a
// well-formed ask-branch or astromech branch passes the static check. Any
// future refactor that accidentally over-broadens the denylist fails here.
func TestAssertNotDefaultBranch_AllowsAskBranches(t *testing.T) {
	t.Parallel()
	allowed := []string{
		"force/ask-42-add-readme",
		"jake/force/ask-10-refactor",
		"jake/agent/r7-a7/task-100",
		"feature-123",
		"bugfix/login",
		"main-branch-refactor",         // substring of "main" but not equal
		"master-migration",             // substring of "master" but not equal
		"refs/heads/force/ask-1-test", // qualified but not default
	}
	for _, branch := range allowed {
		branch := branch
		t.Run("allows_"+strings.ReplaceAll(branch, "/", "_"), func(t *testing.T) {
			t.Parallel()
			if err := AssertNotDefaultBranch("", branch); err != nil {
				t.Errorf("AssertNotDefaultBranch(%q) rejected unexpectedly: %v", branch, err)
			}
		})
	}
}

// TestAssertNotDefaultBranch_HonoursRepoDefault exercises the repoPath-aware
// branch: for a repo whose default branch is "trunk", a call with "trunk" is
// rejected even though "trunk" is already in the static denylist. More
// importantly, a repo whose default is "develop" catches a branch called
// "develop" that the static list also covers — the point is that both layers
// agree. We make a temp repo, set its default, and verify.
func TestAssertNotDefaultBranch_HonoursRepoDefault(t *testing.T) {
	t.Parallel()
	repo := makeTempRepoForGuardTest(t)
	// GetDefaultBranch for this fresh repo should be "main" (created by
	// makeTempRepoForGuardTest via `git init -b main`). Confirm the resolved
	// default matches what we expect, then assert the guard rejects it.
	got := GetDefaultBranch(repo)
	if got == "" {
		t.Fatalf("GetDefaultBranch returned empty for %s", repo)
	}
	if err := AssertNotDefaultBranch(repo, got); err == nil {
		t.Fatalf("AssertNotDefaultBranch(%s, %q) must reject the repo's own default branch", repo, got)
	}
	// A sibling ask-branch on the same repo must still be accepted.
	if err := AssertNotDefaultBranch(repo, "force/ask-1-hello"); err != nil {
		t.Fatalf("AssertNotDefaultBranch(%s, ask-branch) unexpectedly refused: %v", repo, err)
	}
}

// ── Integration: destructive ops refuse a protected argument before any
// git invocation. These call ForcePushBranch / TriggerCIRerun with a "main"
// argument against a repo path that does NOT exist, and assert the call
// returns the protected-branch error rather than whatever bare-git error
// would come back from running `git -C /nonexistent`. That gap is the
// proof of order: the guard fires BEFORE shelling out.
// ────────────────────────────────────────────────────────────────────────

func TestForcePushBranch_RefusesProtectedBeforeShellout(t *testing.T) {
	t.Parallel()
	// Path never touched. The guard must fire first. If shellout happened,
	// we'd see a "not a git repository" error instead.
	err := ForcePushBranch("/definitely/does/not/exist", "main")
	if err == nil {
		t.Fatalf("ForcePushBranch(main) returned nil; expected protected-branch error")
	}
	if !strings.Contains(err.Error(), "ForcePushBranch refused") {
		t.Fatalf("error does not identify refusal: %v", err)
	}
	if !strings.Contains(err.Error(), "protected") {
		t.Fatalf("error does not mention 'protected': %v", err)
	}
	// Belt-and-suspenders: also test with a real repo whose HEAD is main.
	repo := makeTempRepoForGuardTest(t)
	if err := ForcePushBranch(repo, "main"); err == nil {
		t.Fatalf("ForcePushBranch(%s, main) on real repo returned nil", repo)
	}
}

func TestTriggerCIRerun_RefusesProtectedBeforeShellout(t *testing.T) {
	t.Parallel()
	err := TriggerCIRerun("/definitely/does/not/exist", "main", "")
	if err == nil {
		t.Fatalf("TriggerCIRerun(main) returned nil; expected protected-branch error")
	}
	if !strings.Contains(err.Error(), "TriggerCIRerun refused") {
		t.Fatalf("error does not identify refusal: %v", err)
	}
	// Empty-branch attack surface (formerly defaulted to pr.Repo in pr_flow.go).
	if err := TriggerCIRerun("/tmp", "", "msg"); err == nil {
		t.Fatalf("TriggerCIRerun(empty) returned nil; empty branch must be rejected")
	}
}

// ── Helpers ─────────────────────────────────────────────────────────────────

// makeTempRepoForGuardTest creates a throwaway git repo whose default branch
// is "main", with one commit so rev-parse succeeds. Registers cleanup.
func makeTempRepoForGuardTest(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	runGit := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@x",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@x",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v — %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
		}
	}
	// Some older gits don't support `init -b`; fall back to `branch -m`.
	if err := exec.Command("git", "-C", dir, "init", "-b", "main").Run(); err != nil {
		runGit("init")
		runGit("branch", "-m", "main")
	}
	if err := os.WriteFile(filepath.Join(dir, "README"), []byte("seed\n"), 0644); err != nil {
		t.Fatalf("write README: %v", err)
	}
	runGit("add", "README")
	runGit("-c", "commit.gpgsign=false", "commit", "-m", "seed")
	return dir
}
