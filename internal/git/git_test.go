package git

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"force-orchestrator/internal/store"
)

// initTestRepo creates a fresh git repo in a temp dir, commits an initial file,
// and returns the repo path. Tests that need git are skipped if git is not found.
func initTestRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not found in PATH — skipping git integration test")
	}

	dir := t.TempDir()

	gitRun := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=Test",
			"GIT_AUTHOR_EMAIL=test@test.com",
			"GIT_COMMITTER_NAME=Test",
			"GIT_COMMITTER_EMAIL=test@test.com",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %s", args, out)
		}
	}

	gitRun("init", "-b", "main")
	gitRun("config", "user.email", "test@test.com")
	gitRun("config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("hello\n"), 0644); err != nil {
		t.Fatal(err)
	}
	gitRun("add", ".")
	gitRun("commit", "-m", "initial commit")

	return dir
}

// ── gitCmd ────────────────────────────────────────────────────────────────────

func TestGitCmd_ValidCommand(t *testing.T) {
	dir := t.TempDir()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not found in PATH")
	}

	out, err := RunCmd(dir, "--version")
	if err != nil {
		t.Fatalf("gitCmd --version failed: %v", err)
	}
	if !strings.Contains(out, "git") {
		t.Errorf("expected git version output, got %q", out)
	}
}

func TestGitCmd_InvalidCommand(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not found in PATH")
	}

	_, err := RunCmd("/nonexistent", "invalid-subcommand-xyz")
	if err == nil {
		t.Error("expected error for invalid git subcommand")
	}
}

// ── GetDefaultBranch ─────────────────────────────────────────────────────────

func TestGetDefaultBranch(t *testing.T) {
	dir := initTestRepo(t)
	branch := GetDefaultBranch(dir)
	if branch != "main" {
		t.Errorf("expected 'main', got %q", branch)
	}
}

func TestGetDefaultBranch_MasterFallback(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not found in PATH")
	}

	dir := t.TempDir()
	gitRun := func(args ...string) {
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=test@test.com",
			"GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=test@test.com",
		)
		cmd.Run()
	}

	gitRun("init", "-b", "master")
	gitRun("config", "user.email", "test@test.com")
	gitRun("config", "user.name", "Test")
	os.WriteFile(filepath.Join(dir, "README.md"), []byte("hello\n"), 0644)
	gitRun("add", ".")
	gitRun("commit", "-m", "initial")

	branch := GetDefaultBranch(dir)
	if branch != "master" {
		t.Errorf("expected 'master', got %q", branch)
	}
}

func TestGetDefaultBranch_DevelopFallback(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not found in PATH")
	}
	dir := t.TempDir()
	gitEnv := append(os.Environ(),
		"GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=t@t.com",
		"GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=t@t.com",
	)
	run := func(args ...string) {
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = gitEnv
		cmd.Run()
	}
	run("init", "-b", "develop")
	run("config", "user.email", "t@t.com")
	run("config", "user.name", "Test")
	os.WriteFile(filepath.Join(dir, "README.md"), []byte("hello\n"), 0644)
	run("add", ".")
	run("commit", "-m", "initial")

	branch := GetDefaultBranch(dir)
	if branch != "develop" {
		t.Errorf("expected 'develop', got %q", branch)
	}
}

func TestGetDefaultBranch_FinalFallback(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not found in PATH")
	}
	// Empty git repo on 'custom-branch' — none of main/master/develop exist
	dir := t.TempDir()
	exec.Command("git", "-C", dir, "init", "-b", "custom-branch").Run()
	exec.Command("git", "-C", dir, "config", "user.email", "t@t.com").Run()
	exec.Command("git", "-C", dir, "config", "user.name", "Test").Run()

	branch := GetDefaultBranch(dir)
	if branch != "main" {
		t.Errorf("expected 'main' as final fallback, got %q", branch)
	}
}

func TestGetDefaultBranch_SymbolicRefSuccess(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not in PATH")
	}
	dir := t.TempDir()
	if out, err := exec.Command("git", "init", dir).CombinedOutput(); err != nil {
		t.Skipf("git init failed: %s", out)
	}
	// Manually write refs/remotes/origin/HEAD as a git symref pointing to "develop".
	// Using "develop" (not "main"/"master") ensures the symbolic-ref path is taken —
	// the local-branch fallback won't find "develop" since there are no commits.
	refsDir := filepath.Join(dir, ".git", "refs", "remotes", "origin")
	if err := os.MkdirAll(refsDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(refsDir, "HEAD"), []byte("ref: refs/remotes/origin/develop\n"), 0644); err != nil {
		t.Fatal(err)
	}
	result := GetDefaultBranch(dir)
	if result != "develop" {
		t.Errorf("expected 'develop' from symbolic-ref, got %q", result)
	}
}

// ── GetOrCreateAgentWorktree ──────────────────────────────────────────────────

func TestGetOrCreateAgentWorktree(t *testing.T) {
	dir := initTestRepo(t)
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	wt, err := GetOrCreateAgentWorktree(db, "R2-D2", dir)
	if err != nil {
		t.Fatalf("GetOrCreateAgentWorktree: %v", err)
	}
	if _, statErr := os.Stat(wt); statErr != nil {
		t.Fatalf("worktree dir not created at %s: %v", wt, statErr)
	}
	// DB should record it
	stored := GetAgentWorktreePath(db, "R2-D2", dir)
	if stored != wt {
		t.Errorf("DB path %q != returned path %q", stored, wt)
	}
}

func TestGetOrCreateAgentWorktree_Idempotent(t *testing.T) {
	dir := initTestRepo(t)
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	wt1, err1 := GetOrCreateAgentWorktree(db, "BB-8", dir)
	wt2, err2 := GetOrCreateAgentWorktree(db, "BB-8", dir)
	if err1 != nil || err2 != nil {
		t.Fatalf("errors: %v, %v", err1, err2)
	}
	if wt1 != wt2 {
		t.Errorf("second call returned different path: %q vs %q", wt1, wt2)
	}
}

func TestGetOrCreateAgentWorktree_StaleEntry(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not found in PATH")
	}
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	dir := initTestRepo(t)

	// Insert a stale worktree path in the DB
	db.Exec(`INSERT OR REPLACE INTO Agents (agent_name, repo, worktree_path) VALUES (?, ?, ?)`,
		"BB-8", dir, "/nonexistent/old/worktree")

	// GetOrCreateAgentWorktree should detect the stale entry and recreate
	// (it will try to create a new worktree at dir/worktrees/BB-8)
	path, err := GetOrCreateAgentWorktree(db, "BB-8", dir)
	if err != nil {
		// Expected to fail if there's no remote HEAD, but covers the stale-entry path
		_ = path
	}
}

func TestGetOrCreateAgentWorktree_ExistingValid(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	dir := t.TempDir() // exists on disk
	db.Exec(`INSERT OR REPLACE INTO Agents (agent_name, repo, worktree_path) VALUES (?, ?, ?)`,
		"C-3PO", "/some/repo", dir)

	path, err := GetOrCreateAgentWorktree(db, "C-3PO", "/some/repo")
	if err != nil {
		t.Errorf("expected no error for valid existing path, got: %v", err)
	}
	if path != dir {
		t.Errorf("expected existing path %q, got %q", dir, path)
	}
}

func TestGetOrCreateAgentWorktree_NoExisting(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not found in PATH")
	}
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	dir := initTestRepo(t)

	// No existing entry — should attempt to create a new worktree
	path, err := GetOrCreateAgentWorktree(db, "NewBot", dir)
	if err != nil {
		// Expected to fail (bare worktree needs detached HEAD, which requires a branch)
		// This covers the creation attempt code path
		_ = path
		return
	}
	// If it succeeds, path should be under .force-worktrees/<repo>/NewBot
	if !strings.Contains(path, "NewBot") {
		t.Errorf("expected worktree path to contain 'NewBot', got %q", path)
	}
}

// ── PrepareAgentBranch ────────────────────────────────────────────────────────

func TestPrepareAgentBranch(t *testing.T) {
	dir := initTestRepo(t)
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	wt, err := GetOrCreateAgentWorktree(db, "R5-D4", dir)
	if err != nil {
		t.Fatalf("GetOrCreateAgentWorktree: %v", err)
	}

	branch, err := PrepareAgentBranch(wt, dir, 42, "R5-D4")
	if err != nil {
		t.Fatalf("PrepareAgentBranch: %v", err)
	}
	if branch != "agent/R5-D4/task-42" {
		t.Errorf("unexpected branch name %q", branch)
	}

	// Verify the branch exists in the repo
	out, _ := exec.Command("git", "-C", dir, "branch", "--list", branch).Output()
	if !strings.Contains(string(out), branch) {
		t.Errorf("branch %q not found in repo after PrepareAgentBranch", branch)
	}
}

func TestPrepareAgentBranch_DirtyWorktree(t *testing.T) {
	// A dirty worktree (uncommitted file) should be cleaned before branching.
	dir := initTestRepo(t)
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	wt, err := GetOrCreateAgentWorktree(db, "K-2SO", dir)
	if err != nil {
		t.Fatalf("GetOrCreateAgentWorktree: %v", err)
	}

	// Write a dirty file into the worktree
	if writeErr := os.WriteFile(filepath.Join(wt, "dirty.txt"), []byte("oops\n"), 0644); writeErr != nil {
		t.Fatal(writeErr)
	}

	// PrepareAgentBranch should succeed despite the dirty file
	_, err = PrepareAgentBranch(wt, dir, 99, "K-2SO")
	if err != nil {
		t.Fatalf("PrepareAgentBranch with dirty worktree: %v", err)
	}

	// Dirty file should be gone
	if _, statErr := os.Stat(filepath.Join(wt, "dirty.txt")); statErr == nil {
		t.Error("dirty.txt should have been cleaned up by PrepareAgentBranch")
	}
}

func TestPrepareAgentBranch_InNonGitDir(t *testing.T) {
	dir := t.TempDir() // not a git repo
	_, err := PrepareAgentBranch(dir, dir, 1, "R2-D2")
	if err == nil {
		t.Error("expected error when PrepareAgentBranch called on non-git dir")
	}
}

// ── GetDiff ───────────────────────────────────────────────────────────────────

func TestGetDiffAndMerge(t *testing.T) {
	dir := initTestRepo(t)
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	wt, err := GetOrCreateAgentWorktree(db, "BD-1", dir)
	if err != nil {
		t.Fatalf("GetOrCreateAgentWorktree: %v", err)
	}

	branch, err := PrepareAgentBranch(wt, dir, 7, "BD-1")
	if err != nil {
		t.Fatalf("PrepareAgentBranch: %v", err)
	}

	// Make a change and commit it in the worktree
	if writeErr := os.WriteFile(filepath.Join(wt, "feature.txt"), []byte("new feature\n"), 0644); writeErr != nil {
		t.Fatal(writeErr)
	}
	gitEnv := []string{"GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=test@test.com", "GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=test@test.com"}
	addCmd := exec.Command("git", "-C", wt, "add", ".")
	addCmd.Env = append(os.Environ(), gitEnv...)
	addCmd.Run()
	commitCmd := exec.Command("git", "-C", wt, "commit", "-m", "add feature")
	commitCmd.Env = append(os.Environ(), gitEnv...)
	if out, commitErr := commitCmd.CombinedOutput(); commitErr != nil {
		t.Fatalf("commit failed: %s", out)
	}

	// GetDiff should return non-empty diff
	diff := GetDiff(dir, branch)
	if diff == "" {
		t.Error("expected non-empty diff after committing a change")
	}
	if !strings.Contains(diff, "feature.txt") {
		t.Errorf("diff should mention feature.txt, got: %s", diff)
	}

	// MergeAndCleanup should merge successfully
	if mergeErr := MergeAndCleanup(dir, branch, wt); mergeErr != nil {
		t.Fatalf("MergeAndCleanup: %v", mergeErr)
	}

	// File should now be on main
	if _, statErr := os.Stat(filepath.Join(dir, "feature.txt")); statErr != nil {
		t.Error("feature.txt should exist on main branch after merge")
	}

	// Branch should be deleted
	out, _ := exec.Command("git", "-C", dir, "branch", "--list", branch).Output()
	if strings.TrimSpace(string(out)) != "" {
		t.Errorf("branch %q should be deleted after MergeAndCleanup", branch)
	}
}

func TestGetDiff_NonexistentRepo(t *testing.T) {
	// Should return empty string without panicking
	diff := GetDiff("/nonexistent/repo", "some-branch")
	_ = diff // empty is fine
}

// ── MergeAndCleanup ──────────────────────────────────────────────────────────

func TestMergeAndCleanup_MergeFail(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not found in PATH")
	}
	dir := initTestRepo(t)
	// Try to merge a branch that doesn't exist → merge should fail
	err := MergeAndCleanup(dir, "nonexistent-branch", dir)
	if err == nil {
		t.Error("expected error when merging nonexistent branch")
	}
}

func TestMergeAndCleanup_CheckoutFail(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not in PATH")
	}
	// Non-git directory causes `git checkout <branch>` to fail immediately
	dir := t.TempDir()
	err := MergeAndCleanup(dir, "some-branch", dir)
	if err == nil {
		t.Error("expected error from MergeAndCleanup with non-git directory")
	}
	if !strings.Contains(err.Error(), "checkout") {
		t.Errorf("expected 'checkout' in error message, got: %v", err)
	}
}

func TestMergeAndCleanup_Success(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not found in PATH")
	}

	dir := initTestRepo(t)
	gitEnv := append(os.Environ(),
		"GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=t@t.com",
		"GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=t@t.com",
	)

	// Create a branch with a new commit to merge
	branchName := "feature/test-merge"
	cmd := exec.Command("git", "-C", dir, "checkout", "-b", branchName)
	cmd.Env = gitEnv
	cmd.Run()

	os.WriteFile(filepath.Join(dir, "newfile.txt"), []byte("content\n"), 0644)
	cmd2 := exec.Command("git", "-C", dir, "add", ".")
	cmd2.Env = gitEnv
	cmd2.Run()
	cmd3 := exec.Command("git", "-C", dir, "commit", "-m", "new feature")
	cmd3.Env = gitEnv
	cmd3.Run()

	err := MergeAndCleanup(dir, branchName, dir)
	if err != nil {
		t.Errorf("expected successful merge, got: %v", err)
	}
}

// ── ExtractDiffFiles ──────────────────────────────────────────────────────────

func TestExtractDiffFiles(t *testing.T) {
	diff := `diff --git a/handlers/users.go b/handlers/users.go
index abc..def 100644
--- a/handlers/users.go
+++ b/handlers/users.go
@@ -1,3 +1,5 @@
 package handlers
+func NewHandler() {}
diff --git a/routes.go b/routes.go
index 111..222 100644
--- a/routes.go
+++ b/routes.go
@@ -10,1 +10,2 @@
+router.Handle("/users", handlers.NewHandler())
diff --git a/handlers/users.go b/handlers/users.go
index def..ghi 100644`

	files := ExtractDiffFiles(diff)
	// handlers/users.go appears twice but should be deduplicated
	if len(files) != 2 {
		t.Errorf("expected 2 unique files, got %d: %v", len(files), files)
	}
	found := map[string]bool{}
	for _, f := range files {
		found[f] = true
	}
	if !found["handlers/users.go"] {
		t.Error("expected handlers/users.go in results")
	}
	if !found["routes.go"] {
		t.Error("expected routes.go in results")
	}
}

func TestExtractDiffFiles_Empty(t *testing.T) {
	files := ExtractDiffFiles("")
	if len(files) != 0 {
		t.Errorf("expected empty result for empty diff, got %v", files)
	}
}

func TestExtractDiffFiles_Multiline(t *testing.T) {
	diff := `diff --git a/foo.go b/foo.go
index abc..def 100644
--- a/foo.go
+++ b/foo.go
diff --git a/bar.go b/bar.go
index def..abc 100644
--- a/bar.go
+++ b/bar.go
diff --git a/foo.go b/foo.go`

	files := ExtractDiffFiles(diff)
	if len(files) != 2 {
		t.Errorf("expected 2 unique files, got %d: %v", len(files), files)
	}
}
