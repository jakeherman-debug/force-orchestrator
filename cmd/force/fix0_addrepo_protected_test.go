package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"force-orchestrator/internal/store"
)

// TestAddRepo_ProtectedBranchFlow is Fix #0's acceptance-level regression
// test. It drives `force add-repo` against a well-formed git repository and
// then confirms the defended invariant end-to-end: once the repo is
// registered, the store-level ingress MUST reject a follow-up attempt to
// stamp `ask_branch = "main"` against it. Without the ingress guard, a
// corrupt or hand-edited row would flow into completeAskBranchResolution
// and force-push origin/main.
//
// This test is intentionally narrow — it does not exercise the full Pilot
// path. That coverage lives in internal/git's integration tests. What this
// test proves is that the operator-facing CLI pipeline (register repo →
// fleet state) cannot silently admit a protected branch name into the DB
// row Fix #0's downstream guards depend on.
func TestAddRepo_ProtectedBranchFlow(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	// Build a well-formed git repo with a "main" default branch and a seed
	// commit, then register it via the same code path the CLI calls.
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
	if err := exec.Command("git", "-C", dir, "init", "-b", "main").Run(); err != nil {
		runGit("init")
		runGit("branch", "-m", "main")
	}
	if err := os.WriteFile(filepath.Join(dir, "README"), []byte("seed\n"), 0644); err != nil {
		t.Fatalf("write README: %v", err)
	}
	runGit("add", "README")
	runGit("-c", "commit.gpgsign=false", "commit", "-m", "seed")

	// Register the repo via the CLI function. cmdAddRepo calls os.Exit on
	// invalid input; our input is valid so this must not exit. Suppress
	// noisy stdout by redirecting while the function runs.
	origStdout := os.Stdout
	devnull, _ := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	os.Stdout = devnull
	defer func() { os.Stdout = origStdout; devnull.Close() }()
	cmdAddRepo(db, []string{"acceptance-repo", dir, "protected-branch acceptance"})

	// The repo is now in the DB. Fix #0 invariant: UpsertConvoyAskBranch
	// MUST reject "main" (and friends) despite the repo being registered.
	convoyID, err := store.CreateConvoy(db, "fix0-acceptance")
	if err != nil {
		t.Fatalf("CreateConvoy: %v", err)
	}
	for _, bad := range []string{"main", "master", "HEAD", "refs/heads/main"} {
		err := store.UpsertConvoyAskBranch(db, convoyID, "acceptance-repo", bad,
			"deadbeefdeadbeefdeadbeefdeadbeefdeadbeef")
		if err == nil {
			t.Errorf("Fix #0 regression: after add-repo, UpsertConvoyAskBranch accepted ask_branch=%q", bad)
		}
	}

	// Sanity: a well-formed ask-branch IS accepted, so the guard is not
	// over-broad.
	if err := store.UpsertConvoyAskBranch(db, convoyID, "acceptance-repo",
		"force/ask-1-hello", "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef"); err != nil {
		t.Errorf("Fix #0 regression: well-formed ask-branch rejected: %v", err)
	}
}
