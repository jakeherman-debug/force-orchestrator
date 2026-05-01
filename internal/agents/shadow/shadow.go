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

// SetupShadowWorktree creates the shadow worktree + opens the gh
// recording file for the given experiment run. Sub-agent A's
// worktree.go provides the concrete implementation; this stub exists
// so adversarial / golden_set call sites compile against a stable
// signature during the parallel build.
func SetupShadowWorktree(_ context.Context, _ *sql.DB, _ int64) (*ShadowSession, error) {
	// Sub-agent A overwrites this with the real implementation.
	return nil, ErrShadowNotConfigured
}

// CleanupShadowWorktree tears down the shadow worktree and finalizes
// the gh recording file. Idempotent — safe to call multiple times.
func CleanupShadowWorktree(_ context.Context, _ *sql.DB, _ *ShadowSession) error {
	// Sub-agent A overwrites this with the real implementation.
	return nil
}
