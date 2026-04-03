package agents

import (
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"force-orchestrator/internal/store"
)

// ── detectStalledTasks ────────────────────────────────────────────────────────

func TestDetectStalledTasks_NoStalledTasks(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	// No tasks or all tasks are Pending → no stalls to detect
	store.AddBounty(db, 0, "CodeEdit", "pending task")
	logger := log.New(io.Discard, "", 0)

	// Should not panic or deadlock
	detectStalledTasks(db, logger)
}

func TestDetectStalledTasks_StalledTaskGitError(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	// Register repo and agent with nonexistent worktree so git fails (safe path)
	store.AddRepo(db, "api", "/tmp/api", "api service")
	db.Exec(`INSERT OR REPLACE INTO Agents (agent_name, repo, worktree_path) VALUES (?, ?, ?)`,
		"R2-D2", "/tmp/api", "/nonexistent/worktree")

	// Add a task locked just over StallWarnTimeout ago but under stallEscTimeout
	// so it detects the stall but does NOT call BootTriage (which requires Claude CLI)
	id := store.AddBounty(db, 0, "CodeEdit", "stalled task")
	db.Exec(`UPDATE BountyBoard SET status = 'Locked', owner = 'R2-D2', target_repo = 'api',
		branch_name = 'agent/R2-D2/task-1', locked_at = datetime('now', '-25 minutes') WHERE id = ?`, id)

	logger := log.New(io.Discard, "", 0)
	// Should not panic; git error in worktree → "progress assumed" → task not escalated
	detectStalledTasks(db, logger)

	// Task should still be Locked (git error means "assumed making progress")
	b, _ := store.GetBounty(db, id)
	if b.Status != "Locked" {
		t.Errorf("expected task to remain Locked when git error occurs, got %q", b.Status)
	}
}

func TestDetectStalledTasks_MissingRepo(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	// Task locked >20 min but repo not registered → repoPath="" → skip
	id := store.AddBounty(db, 0, "CodeEdit", "stalled task")
	db.Exec(`UPDATE BountyBoard SET status = 'Locked', owner = 'R2-D2', target_repo = 'unregistered-repo',
		branch_name = 'agent/R2-D2/task-1', locked_at = datetime('now', '-25 minutes') WHERE id = ?`, id)

	logger := log.New(io.Discard, "", 0)
	detectStalledTasks(db, logger) // should not panic
}

func TestDetectStalledTasks_StallWarnPath(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not found in PATH")
	}
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	// Create a git repo whose only commit was dated in the distant past.
	// git log --since=<locked_at> HEAD returns 0 results → stall detected.
	dir := t.TempDir()
	gitEnv := append(os.Environ(),
		"GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=t@t.com",
		"GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=t@t.com",
		"GIT_AUTHOR_DATE=2020-01-01T00:00:00Z",
		"GIT_COMMITTER_DATE=2020-01-01T00:00:00Z",
	)
	run := func(args ...string) {
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = gitEnv
		cmd.Run()
	}
	run("init", "-b", "main")
	run("config", "user.email", "t@t.com")
	run("config", "user.name", "Test")
	os.WriteFile(filepath.Join(dir, "README.md"), []byte("hello\n"), 0644)
	run("add", ".")
	run("commit", "-m", "old commit")

	// Register repo and agent worktree (same dir for simplicity)
	store.AddRepo(db, "testrepo", dir, "test")
	db.Exec(`INSERT OR REPLACE INTO Agents (agent_name, repo, worktree_path) VALUES (?, ?, ?)`,
		"R2-D2", dir, dir)

	// Lock task 25 minutes ago (past StallWarnTimeout=20, below stallEscTimeout=30)
	taskID := store.AddBounty(db, 0, "CodeEdit", "stalled task")
	db.Exec(`UPDATE BountyBoard SET status = 'Locked', owner = 'R2-D2', target_repo = 'testrepo',
		branch_name = 'agent/R2-D2/task-1', locked_at = datetime('now', '-25 minutes') WHERE id = ?`, taskID)

	logger := log.New(io.Discard, "", 0)
	detectStalledTasks(db, logger)

	// Task should remain Locked (stall < stallEscTimeout; no Boot triage)
	b, _ := store.GetBounty(db, taskID)
	if b.Status != "Locked" {
		t.Errorf("expected task to stay Locked at stall-warn threshold, got %q", b.Status)
	}
}

func TestDetectStalledTasks_BootThrottle(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not found in PATH")
	}
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	// Create a git repo with an old commit (so no progress is detected)
	dir := t.TempDir()
	gitEnv := append(os.Environ(),
		"GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=t@t.com",
		"GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=t@t.com",
		"GIT_AUTHOR_DATE=2020-01-01T00:00:00Z",
		"GIT_COMMITTER_DATE=2020-01-01T00:00:00Z",
	)
	run := func(args ...string) {
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = gitEnv
		cmd.Run()
	}
	run("init", "-b", "main")
	run("config", "user.email", "t@t.com")
	run("config", "user.name", "Test")
	os.WriteFile(filepath.Join(dir, "README.md"), []byte("hello\n"), 0644)
	run("add", ".")
	run("commit", "-m", "old commit")

	store.AddRepo(db, "testrepo", dir, "test")
	db.Exec(`INSERT OR REPLACE INTO Agents (agent_name, repo, worktree_path) VALUES (?, ?, ?)`,
		"R2-D2", dir, dir)

	// Lock task >stallEscTimeout (35 min) so boot triage threshold is hit
	taskID := store.AddBounty(db, 0, "CodeEdit", "stalled long")
	db.Exec(`UPDATE BountyBoard SET status = 'Locked', owner = 'R2-D2', target_repo = 'testrepo',
		branch_name = 'agent/R2-D2/task-1', locked_at = datetime('now', '-35 minutes') WHERE id = ?`, taskID)

	// Pre-populate bootLastCalled to simulate a recent Boot triage call
	// This prevents BootTriage from being called (which requires Claude CLI)
	bootLastCalled[taskID] = time.Now()
	defer delete(bootLastCalled, taskID)

	logger := log.New(io.Discard, "", 0)
	detectStalledTasks(db, logger) // should throttle Boot triage

	// Task should still be Locked (throttled — no Boot action taken)
	b, _ := store.GetBounty(db, taskID)
	if b.Status != "Locked" {
		t.Errorf("expected task to stay Locked when Boot triage is throttled, got %q", b.Status)
	}
}

// ── validateWorktrees ─────────────────────────────────────────────────────────

func TestValidateWorktrees_StaleEntry(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	// Insert a stale Agents entry pointing to a nonexistent path
	db.Exec(`INSERT OR REPLACE INTO Agents (agent_name, repo, worktree_path) VALUES (?, ?, ?)`,
		"R2-D2", "/tmp/test-repo", "/nonexistent/worktree/path")

	// A locked task owned by the stale agent
	id := store.AddBounty(db, 0, "CodeEdit", "locked task")
	db.Exec(`UPDATE BountyBoard SET status = 'Locked', owner = 'R2-D2', locked_at = datetime('now') WHERE id = ?`, id)

	logger := log.New(io.Discard, "", 0)
	validateWorktrees(db, logger)

	// Agent entry should be removed
	var count int
	db.QueryRow(`SELECT COUNT(*) FROM Agents WHERE agent_name = 'R2-D2'`).Scan(&count)
	if count != 0 {
		t.Error("expected stale agent entry to be removed")
	}

	// Locked task should be reset to Pending
	b, _ := store.GetBounty(db, id)
	if b.Status != "Pending" {
		t.Errorf("expected task reset to Pending, got %q", b.Status)
	}
}

func TestValidateWorktrees_AllValid(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	// Valid path (temp dir exists)
	dir := t.TempDir()
	db.Exec(`INSERT OR REPLACE INTO Agents (agent_name, repo, worktree_path) VALUES (?, ?, ?)`,
		"BB-8", "/tmp/repo", dir)

	logger := log.New(io.Discard, "", 0)
	validateWorktrees(db, logger) // should not remove the valid entry

	var count int
	db.QueryRow(`SELECT COUNT(*) FROM Agents WHERE agent_name = 'BB-8'`).Scan(&count)
	if count != 1 {
		t.Error("expected valid agent entry to be preserved")
	}
}

// ── cleanOrphanedBranches ─────────────────────────────────────────────────────

func TestCleanOrphanedBranches_NoOrphans(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	// No failed/escalated tasks → nothing to clean
	store.AddBounty(db, 0, "CodeEdit", "pending task")
	logger := log.New(io.Discard, "", 0)

	// Should not panic or deadlock
	cleanOrphanedBranches(db, logger)
}

func TestCleanOrphanedBranches_WithFailedTask(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	// Register a repo with a nonexistent path → store.GetRepoPath returns "" → skip
	// This exercises the loop body without needing git
	store.AddRepo(db, "api", "/tmp/api", "service")
	id := store.AddBounty(db, 0, "CodeEdit", "failed task")
	store.FailBounty(db, id, "some error")
	store.SetBranchName(db, id, "agent/R2-D2/task-1")

	logger := log.New(io.Discard, "", 0)
	// Should not deadlock (loop body reached) and should skip gracefully when repoPath has no git
	cleanOrphanedBranches(db, logger)
}

func TestCleanOrphanedBranches_BranchNotFound(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not found in PATH")
	}
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	dir := initTestRepo(t)
	store.AddRepo(db, "repo", dir, "test")

	// Failed task with a nonexistent branch → git will say "not found" → else branch clears record
	id := store.AddBounty(db, 0, "CodeEdit", "failed task")
	store.FailBounty(db, id, "error")
	db.Exec(`UPDATE BountyBoard SET target_repo = 'repo' WHERE id = ?`, id)
	store.SetBranchName(db, id, "agent/R2-D2/task-nonexistent")

	logger := log.New(io.Discard, "", 0)
	cleanOrphanedBranches(db, logger)

	// DB record should be cleared (branch_name = '')
	b, _ := store.GetBounty(db, id)
	if b.BranchName != "" {
		t.Errorf("expected branch_name cleared after 'not found', got %q", b.BranchName)
	}
}

func TestCleanOrphanedBranches_BranchDeletedSuccessfully(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not found in PATH")
	}
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	dir := initTestRepo(t)
	store.AddRepo(db, "repo", dir, "test")

	// Create a branch in the repo that will be deleted
	branchName := "agent/R2-D2/task-99"
	exec.Command("git", "-C", dir, "branch", branchName).Run()

	// Failed task with that branch name → git will delete it successfully
	id := store.AddBounty(db, 0, "CodeEdit", "failed task")
	store.FailBounty(db, id, "error")
	db.Exec(`UPDATE BountyBoard SET target_repo = 'repo' WHERE id = ?`, id)
	store.SetBranchName(db, id, branchName)

	logger := log.New(io.Discard, "", 0)
	cleanOrphanedBranches(db, logger)

	// DB record should be cleared
	b, _ := store.GetBounty(db, id)
	if b.BranchName != "" {
		t.Errorf("expected branch_name cleared after successful delete, got %q", b.BranchName)
	}
}
