package shadow

import (
	"context"
	"fmt"
	"strings"
)

// PushOutcome reports what happened to a push attempt under shadow
// suppression. Callers use this to know whether they should still
// expect Jenkins / GitHub Actions to run (real arm) vs. skip CI-bound
// scoring (shadow arm).
type PushOutcome struct {
	// Suppressed is true when shadow mode rewrote / dropped the push.
	Suppressed bool

	// RewrittenBranch is the local-only refspec the push was rewritten
	// to (typically `shadow-exp-<exp>-run-<run>`). Empty if the push
	// was outright dropped.
	RewrittenBranch string

	// Reason is operator-readable explanation of why the call was
	// suppressed. Logged + included in the gh recording for forensics.
	Reason string
}

// ShouldSuppressPush returns true when a `git push` initiated from a
// shadow-mode run must be rewritten to a local-only refspec or
// outright dropped. Real-arm runs (sess == nil OR
// sess.WorktreePath == "") return false — pass-through.
//
// Anti-cheat: real network pushes from a shadow arm leak the
// experiment into production CI. The shadow arm's whole purpose is to
// observe agent behavior without side effects; a stray push to origin
// defeats the shadow concept.
func ShouldSuppressPush(sess *ShadowSession) bool {
	if sess == nil {
		return false
	}
	return sess.WorktreePath != ""
}

// SuppressPush computes the local-only refspec a shadow-mode push
// should land on. Returns a PushOutcome the caller can use to
// short-circuit the real `git push` invocation. The returned branch
// name is the same one SetupShadowWorktreeAt creates the worktree on,
// so a `git push` to it stays purely local.
func SuppressPush(_ context.Context, sess *ShadowSession, requestedRefspec string) PushOutcome {
	if sess == nil {
		return PushOutcome{}
	}
	branch := fmt.Sprintf("shadow-exp-%d-run-%d", sess.ExperimentID, sess.RunID)
	return PushOutcome{
		Suppressed:      true,
		RewrittenBranch: branch,
		Reason: fmt.Sprintf(
			"shadow-mode push for run %d rewritten from %q to local-only branch %q",
			sess.RunID, requestedRefspec, branch,
		),
	}
}

// IsShadowGhWrite classifies a gh CLI argument list as a write
// operation that must be suppressed in shadow mode. Reads (gh pr view,
// gh pr checks, gh api GET) are pass-through; writes (gh pr create,
// gh pr merge, gh pr close, gh api -X POST/PUT/PATCH/DELETE,
// gh issue close, gh issue comment) are suppressed.
//
// The classifier is conservative: an unrecognized verb defaults to
// "write" so a new gh feature doesn't slip through unchecked.
func IsShadowGhWrite(args []string) bool {
	if len(args) == 0 {
		return false
	}
	// Top-level verb determines the read/write decision in most cases.
	switch args[0] {
	case "auth", "version", "help", "config", "completion":
		return false
	case "api":
		// gh api defaults to GET; -X / --method changes that.
		return ghAPIIsWrite(args[1:])
	case "pr":
		return prSubcommandIsWrite(args[1:])
	case "issue":
		return issueSubcommandIsWrite(args[1:])
	case "release":
		return releaseSubcommandIsWrite(args[1:])
	case "repo":
		return repoSubcommandIsWrite(args[1:])
	case "run", "workflow":
		// `gh run rerun`, `gh workflow run` are writes; list/view are reads.
		if len(args) >= 2 {
			switch args[1] {
			case "list", "view", "watch":
				return false
			}
		}
		return true
	default:
		// Unknown top-level verb: be conservative.
		return true
	}
}

func ghAPIIsWrite(rest []string) bool {
	for i := 0; i < len(rest); i++ {
		a := rest[i]
		if a == "-X" || a == "--method" {
			if i+1 < len(rest) {
				m := strings.ToUpper(rest[i+1])
				return m == "POST" || m == "PUT" || m == "PATCH" || m == "DELETE"
			}
		}
		if strings.HasPrefix(a, "--method=") {
			m := strings.ToUpper(strings.TrimPrefix(a, "--method="))
			return m == "POST" || m == "PUT" || m == "PATCH" || m == "DELETE"
		}
	}
	return false
}

func prSubcommandIsWrite(rest []string) bool {
	if len(rest) == 0 {
		return false
	}
	switch rest[0] {
	case "view", "list", "checks", "diff", "status":
		return false
	case "create", "merge", "close", "reopen", "ready", "comment", "edit", "review":
		return true
	}
	return true
}

func issueSubcommandIsWrite(rest []string) bool {
	if len(rest) == 0 {
		return false
	}
	switch rest[0] {
	case "view", "list", "status":
		return false
	case "create", "close", "reopen", "comment", "edit", "delete", "transfer":
		return true
	}
	return true
}

func releaseSubcommandIsWrite(rest []string) bool {
	if len(rest) == 0 {
		return false
	}
	switch rest[0] {
	case "view", "list", "download":
		return false
	}
	return true
}

func repoSubcommandIsWrite(rest []string) bool {
	if len(rest) == 0 {
		return false
	}
	switch rest[0] {
	case "view", "list", "clone":
		return false
	}
	return true
}
