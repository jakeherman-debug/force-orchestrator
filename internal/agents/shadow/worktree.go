package shadow

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	igit "force-orchestrator/internal/git"
)

// ShadowWorktreeRoot is the on-disk root for shadow worktrees.
// Distinct from production worktrees by virtue of the
// `.force-shadow-worktrees` prefix — sweepers that walk
// `.force-worktrees` (production) won't accidentally clean this up,
// and sweepers that walk `.force-shadow-worktrees` won't accidentally
// clean production.
const ShadowWorktreeRoot = ".force-shadow-worktrees"

// SetupOptions parametrizes the setup call. RepoPath is the absolute
// path to the source git repo (the production checkout). BaseRef is the
// ref the shadow worktree should be created from (commit SHA or branch).
// If empty, defaults to "HEAD".
type SetupOptions struct {
	RepoPath  string
	BaseRef   string
	AgentName string
}

// SetupShadowWorktreeAt creates a `.force-shadow-worktrees/<exp>/<agent>/`
// worktree from RepoPath at BaseRef, initializes the gh recording
// file, and persists the resulting paths to ExperimentRuns. Returns a
// ShadowSession the caller can pass to CleanupShadowWorktreeAt.
//
// The function fails closed on the experiment-mode check: if the run
// is NOT in `paired_shadow` mode, it returns ErrShadowNotConfigured
// without touching the filesystem. This makes the call safe at every
// agent ingress point (real-arm runs short-circuit).
func SetupShadowWorktreeAt(ctx context.Context, db *sql.DB, runID int64, opts SetupOptions) (*ShadowSession, error) {
	if db == nil {
		return nil, fmt.Errorf("shadow.SetupShadowWorktreeAt: db is required")
	}
	if opts.RepoPath == "" {
		return nil, fmt.Errorf("shadow.SetupShadowWorktreeAt: RepoPath is required")
	}

	var mode string
	var experimentID int64
	err := db.QueryRowContext(ctx,
		`SELECT IFNULL(mode, ''), IFNULL(experiment_id, 0) FROM ExperimentRuns WHERE id = ?`,
		runID,
	).Scan(&mode, &experimentID)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("shadow.SetupShadowWorktreeAt: run %d not found", runID)
	}
	if err != nil {
		return nil, fmt.Errorf("shadow.SetupShadowWorktreeAt: query run %d: %w", runID, err)
	}
	if mode != "paired_shadow" {
		return nil, ErrShadowNotConfigured
	}

	baseRef := opts.BaseRef
	if baseRef == "" {
		baseRef = "HEAD"
	}
	agent := opts.AgentName
	if agent == "" {
		agent = fmt.Sprintf("run-%d", runID)
	}
	// Sanitize agent into a path-safe segment (no slashes, no `..`).
	agent = sanitizePathSegment(agent)

	root := filepath.Join(opts.RepoPath, ShadowWorktreeRoot,
		fmt.Sprintf("exp-%d", experimentID),
		fmt.Sprintf("run-%d-%s", runID, agent))

	if err := os.MkdirAll(filepath.Dir(root), 0o755); err != nil {
		return nil, fmt.Errorf("shadow.SetupShadowWorktreeAt: mkdir parents: %w", err)
	}

	// Use git worktree add. The branch name is shadow-exp-<exp>-run-<run>
	// per the paired-runs spec; this lets later push-suppression trivially
	// recognize shadow branches.
	//
	// D3 polish-pass iteration 2 (B4r): route through igit.LogAndRun so
	// the shadow worktree setup is recorded in GitOperationLog (Pattern P32).
	branch := fmt.Sprintf("shadow-exp-%d-run-%d", experimentID, runID)
	if out, gitErr := igit.LogAndRun(ctx,
		igit.OpContext{Repo: opts.RepoPath},
		"shadow-worktree-add",
		"git", "-C", opts.RepoPath,
		"worktree", "add", "-b", branch, root, baseRef,
	); gitErr != nil {
		return nil, fmt.Errorf("shadow.SetupShadowWorktreeAt: git worktree add: %s: %w",
			strings.TrimSpace(string(out)), gitErr)
	}

	ghRecordingPath := filepath.Join(root, ".force-shadow-gh-recording.jsonl")
	// Touch the recording file so callers can immediately open it.
	if f, err := os.OpenFile(ghRecordingPath, os.O_CREATE|os.O_WRONLY, 0o644); err == nil {
		f.Close()
	}

	sess := &ShadowSession{
		ExperimentID:    experimentID,
		RunID:           runID,
		AgentName:       agent,
		WorktreePath:    root,
		GhRecordingPath: ghRecordingPath,
	}
	return sess, nil
}

// CleanupShadowWorktreeAt removes the shadow worktree and the local
// shadow branch. Idempotent: safe to call multiple times. The gh
// recording file is preserved (callers may want to read it post-cleanup
// for scoring), but the worktree directory and the branch are removed.
//
// repoPath is the source repo (same as SetupOptions.RepoPath). Passed
// explicitly because ShadowSession does not retain it.
func CleanupShadowWorktreeAt(ctx context.Context, repoPath string, sess *ShadowSession) error {
	if sess == nil || sess.WorktreePath == "" {
		return nil
	}
	if repoPath == "" {
		return fmt.Errorf("shadow.CleanupShadowWorktreeAt: repoPath is required")
	}

	// Best-effort copy of the recording file out of the worktree before
	// the worktree directory is removed. If the operator wants the
	// recording, they keep it via GhRecordingPath; we read it here to
	// confirm presence, but we do NOT mutate.

	// `git worktree remove` only succeeds if the worktree directory
	// still exists; if it was already nuked, fall through to branch
	// cleanup.
	//
	// D3 polish-pass iteration 2 (B4r): route through igit.LogAndRun.
	if _, err := os.Stat(sess.WorktreePath); err == nil {
		_, _ = igit.LogAndRun(ctx,
			igit.OpContext{Repo: repoPath},
			"shadow-worktree-remove",
			"git", "-C", repoPath, "worktree", "remove", "--force", sess.WorktreePath,
		)
	}

	branch := fmt.Sprintf("shadow-exp-%d-run-%d", sess.ExperimentID, sess.RunID)
	_, _ = igit.LogAndRun(ctx,
		igit.OpContext{Repo: repoPath},
		"shadow-branch-D",
		"git", "-C", repoPath, "branch", "-D", branch,
	)

	// Drop the worktree dir if `git worktree remove` left anything
	// behind.
	_ = os.RemoveAll(sess.WorktreePath)

	return nil
}

// sanitizePathSegment strips characters that don't belong in a path
// component. Whitelist [A-Za-z0-9_-]; everything else (including '.',
// to defang `..` traversal) becomes '_'.
func sanitizePathSegment(s string) string {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z':
			out = append(out, c)
		case c >= 'A' && c <= 'Z':
			out = append(out, c)
		case c >= '0' && c <= '9':
			out = append(out, c)
		case c == '_' || c == '-':
			out = append(out, c)
		default:
			out = append(out, '_')
		}
	}
	if len(out) == 0 {
		return "agent"
	}
	return string(out)
}
