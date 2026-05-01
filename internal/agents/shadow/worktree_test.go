package shadow

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"force-orchestrator/internal/store"
)

// initBareRepo initializes a real git repo at dir with one commit on
// main. Returns the absolute repo path.
func initBareRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	abs, err := filepath.Abs(dir)
	if err != nil {
		t.Fatalf("abs: %v", err)
	}
	run := func(args ...string) {
		cmd := exec.Command("git", append([]string{"-C", abs}, args...)...)
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=test",
			"GIT_AUTHOR_EMAIL=test@example.com",
			"GIT_COMMITTER_NAME=test",
			"GIT_COMMITTER_EMAIL=test@example.com",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %s: %v", args, string(out), err)
		}
	}
	run("init", "-b", "main")
	if err := os.WriteFile(filepath.Join(abs, "README.md"), []byte("hi\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	run("add", "README.md")
	run("commit", "-m", "init")
	return abs
}

func seedShadowRun(t *testing.T) (*sql.DB, int64) {
	t.Helper()
	db := store.InitHolocronDSN(":memory:")
	res, err := db.Exec(`INSERT INTO ExperimentRuns
		(experiment_id, treatment_id, natural_unit_kind, natural_unit_id, mode, agent_name)
		VALUES (1, 1, 'feature', 100, 'paired_shadow', 'astromech-1')`)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	runID, _ := res.LastInsertId()
	return db, runID
}

func TestShadowWorktree_HappyPath(t *testing.T) {
	repo := initBareRepo(t)
	db, runID := seedShadowRun(t)
	defer db.Close()

	sess, err := SetupShadowWorktreeAt(context.Background(), db, runID, SetupOptions{
		RepoPath:  repo,
		BaseRef:   "main",
		AgentName: "astromech-1",
	})
	if err != nil {
		t.Fatalf("SetupShadowWorktreeAt: %v", err)
	}
	if sess.WorktreePath == "" || !strings.Contains(sess.WorktreePath, ShadowWorktreeRoot) {
		t.Fatalf("WorktreePath malformed: %q", sess.WorktreePath)
	}
	if _, err := os.Stat(sess.WorktreePath); err != nil {
		t.Fatalf("worktree dir not created: %v", err)
	}
	if _, err := os.Stat(sess.GhRecordingPath); err != nil {
		t.Fatalf("gh recording file not created: %v", err)
	}
	if !strings.Contains(sess.WorktreePath, ShadowWorktreeRoot) {
		t.Fatalf("shadow worktree path missing the .force-shadow-worktrees prefix — would collide with production")
	}

	// Cleanup tears down the worktree + branch.
	if err := CleanupShadowWorktreeAt(context.Background(), repo, sess); err != nil {
		t.Fatalf("Cleanup: %v", err)
	}
	if _, err := os.Stat(sess.WorktreePath); !os.IsNotExist(err) {
		t.Fatalf("worktree dir not removed after cleanup")
	}
}

func TestShadowWorktree_RealModeReturnsNotConfigured(t *testing.T) {
	repo := initBareRepo(t)
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	res, err := db.Exec(`INSERT INTO ExperimentRuns
		(experiment_id, treatment_id, natural_unit_kind, natural_unit_id, mode, agent_name)
		VALUES (1, 1, 'feature', 100, 'paired_real', 'astromech-1')`)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	runID, _ := res.LastInsertId()

	_, err = SetupShadowWorktreeAt(context.Background(), db, runID, SetupOptions{
		RepoPath: repo,
		BaseRef:  "main",
	})
	if !errors.Is(err, ErrShadowNotConfigured) {
		t.Fatalf("real-mode setup: want ErrShadowNotConfigured, got %v", err)
	}
}

func TestShadowWorktree_UnknownRun(t *testing.T) {
	repo := initBareRepo(t)
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	_, err := SetupShadowWorktreeAt(context.Background(), db, 999, SetupOptions{
		RepoPath: repo,
		BaseRef:  "main",
	})
	if err == nil {
		t.Fatalf("want error for unknown run, got nil")
	}
}

func TestShadowWorktree_CleanupIdempotent(t *testing.T) {
	repo := initBareRepo(t)
	db, runID := seedShadowRun(t)
	defer db.Close()

	sess, err := SetupShadowWorktreeAt(context.Background(), db, runID, SetupOptions{
		RepoPath:  repo,
		BaseRef:   "main",
		AgentName: "astromech-1",
	})
	if err != nil {
		t.Fatalf("Setup: %v", err)
	}
	if err := CleanupShadowWorktreeAt(context.Background(), repo, sess); err != nil {
		t.Fatalf("Cleanup 1: %v", err)
	}
	if err := CleanupShadowWorktreeAt(context.Background(), repo, sess); err != nil {
		t.Fatalf("Cleanup 2 (idempotent): %v", err)
	}
}

func TestShadowWorktree_DistinctFromProductionTree(t *testing.T) {
	// Anti-cheat: shadow worktree must NOT live under the production
	// .force-worktrees/ prefix; if it did, the production-worktree GC
	// dog could nuke shadow runs.
	repo := initBareRepo(t)
	db, runID := seedShadowRun(t)
	defer db.Close()

	sess, err := SetupShadowWorktreeAt(context.Background(), db, runID, SetupOptions{
		RepoPath:  repo,
		BaseRef:   "main",
		AgentName: "astromech-1",
	})
	if err != nil {
		t.Fatalf("Setup: %v", err)
	}
	defer CleanupShadowWorktreeAt(context.Background(), repo, sess)

	if strings.Contains(sess.WorktreePath, "/.force-worktrees/") {
		t.Fatalf("shadow worktree path contains production prefix — sweeper-collision risk: %q", sess.WorktreePath)
	}
}

func TestShadowWorktree_SanitizeAgentName(t *testing.T) {
	cases := map[string]string{
		"astromech-1":      "astromech-1",
		"foo/bar":          "foo_bar",
		"../../etc/passwd": "______etc_passwd",
		"":                 "agent",
	}
	for in, want := range cases {
		if got := sanitizePathSegment(in); got != want {
			t.Errorf("sanitizePathSegment(%q) = %q, want %q", in, got, want)
		}
	}
}
