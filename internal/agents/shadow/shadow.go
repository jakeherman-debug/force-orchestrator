// Package shadow implements Level-3 paired shadow runs for tool-using
// agents. A shadow arm runs alongside a real arm with identical inputs,
// but its side effects (gh writes, git pushes, CI triggers) are
// recorded rather than dispatched.
//
// The package surface is intentionally small — three concerns mapped to
// three files:
//
//   - gh_proxy.go     — gh CLI recording proxy (records command + args
//     + response to a JSONL file in shadow mode; pass-through in real
//     mode).
//   - worktree.go     — parallel `.force-shadow-worktrees/<exp>/<agent>/`
//     worktrees so shadow runs do not contend with production worktrees.
//   - ci_suppress.go  — push-side suppression: shadow-mode pushes
//     no-op or rewrite to a local-only refspec so Jenkins / GitHub
//     Actions never see them.
//
// The shadow package depends only on internal/gh, internal/git, and
// internal/store. Agents that opt in to shadow execution call into
// shadow.SetupShadowWorktree / shadow.NewShadowGhClient at the start of
// the run, and shadow.CleanupShadowWorktree on termination.
package shadow

import (
	"context"
	"database/sql"
	"errors"
)

// ShadowSession is the per-run handle that ties together the gh proxy
// recording file, the shadow worktree path, and the experiment-run row
// that bookkeeps the shadow arm. Sub-agent A fills in the concrete
// fields; here we declare the shape so adversarial / golden_set / agent
// code can reference the type before A lands.
type ShadowSession struct {
	// ExperimentID is the parent experiment driving the shadow run.
	ExperimentID int64

	// RunID is the ExperimentRuns row for this shadow arm
	// (mode='paired_shadow').
	RunID int64

	// AgentName is the agent the shadow arm runs as (e.g. "astromech-1",
	// "pilot-A"). Mirrors the production agent name for parity.
	AgentName string

	// WorktreePath is the absolute path to the shadow worktree
	// (`.force-shadow-worktrees/<exp>/<agent>/`). Empty until
	// SetupShadowWorktree returns.
	WorktreePath string

	// GhRecordingPath is the absolute path to the JSONL file the gh
	// proxy writes to. One line per gh invocation. Read-only after the
	// session terminates.
	GhRecordingPath string
}

// ErrShadowNotConfigured is returned by SetupShadowWorktree when the
// caller asks for a shadow worktree but the experiment is not enrolled
// in paired-shadow mode. Callers may treat this as a no-op signal —
// shadow infrastructure stays inactive on real-arm-only runs.
var ErrShadowNotConfigured = errors.New("shadow: experiment not configured for paired-shadow mode")

// SetupShadowWorktree is the legacy stub kept for downstream
// compatibility with skeleton-era call sites. Production code should
// use SetupShadowWorktreeAt (worktree.go) which takes the full
// SetupOptions, including RepoPath. This stub fails closed —
// downstream callers that haven't wired RepoPath through yet receive
// ErrShadowNotConfigured and short-circuit, so a missing wire-up never
// silently turns into a real-arm-only run.
func SetupShadowWorktree(_ context.Context, _ *sql.DB, _ int64) (*ShadowSession, error) {
	return nil, ErrShadowNotConfigured
}

// CleanupShadowWorktree is the legacy stub kept for downstream
// compatibility. Production code should use CleanupShadowWorktreeAt.
// Idempotent — safe to call with a nil session.
func CleanupShadowWorktree(_ context.Context, _ *sql.DB, sess *ShadowSession) error {
	if sess == nil {
		return nil
	}
	// We don't have repoPath here; delegate to the *At cleanup is
	// intentionally not done (it requires repoPath). Caller should use
	// CleanupShadowWorktreeAt directly. Returning nil is the safe
	// idempotent contract.
	return nil
}
