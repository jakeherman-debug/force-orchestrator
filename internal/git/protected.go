package git

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// ── Protected-branch guard (Fix #0) ─────────────────────────────────────────
//
// Every destructive git operation consuming a branch name that originated in
// the database, an LLM payload, or external input MUST call
// AssertNotDefaultBranch before touching the remote. A single DB-corrupt row
// (ConvoyAskBranches.ask_branch = "main") or a payload-derived mistake must
// not be allowed to become `git push --force origin main`.
//
// The guard layers three checks so no single detection mechanism is load-
// bearing:
//
//   1. Empty branch is always rejected — empty args have silently defaulted
//      callers into destructive fallbacks in the past (pr_flow.go:709 used to
//      fall back to `branch := pr.Repo`).
//   2. A hard-coded denylist of common default-branch names (main, master,
//      develop, trunk, production, prod, HEAD). Catches repos we've never
//      detected a default for, plus the pathological case of a non-repo
//      argument where GetDefaultBranch returns the string literal "main".
//   3. GetDefaultBranch(repoPath) when repoPath != "" — so reconfigurable
//      repos (a team flipping from main → trunk) are still protected after
//      the flip without having to update the denylist.
//
// ErrProtectedBranch is returned so callers can distinguish this failure
// class from transient git errors if they ever need to (none do today —
// every caller just wraps the error and escalates).

var ErrProtectedBranch = errors.New("refusing destructive op on protected branch")

// protectedBranchNames is the hard-coded denylist. Every modern git hosting
// provider uses one of these as the default branch name; the list is
// intentionally short — adding "release" or similar would block legitimate
// ask-branches that borrow the name. Lowercase-compared.
var protectedBranchNames = map[string]struct{}{
	"main":       {},
	"master":     {},
	"develop":    {},
	"trunk":      {},
	"production": {},
	"prod":       {},
	"head":       {},
}

// refNamePrefixesToStrip are ref forms we unwrap before comparing. A caller
// that accidentally passes "refs/heads/main" must be rejected exactly the
// same as "main".
var refNamePrefixesToStrip = []string{
	"refs/heads/",
	"refs/remotes/origin/",
	"origin/",
}

// AssertNotDefaultBranch returns ErrProtectedBranch when `branch` names a
// protected/default branch of the repository at `repoPath`. Pass `repoPath = ""`
// to run the static-denylist check only (useful for ingress validators at the
// store boundary where no repo path is yet known).
//
// This is the common guard installed at every destructive git op's entry
// point: ForcePushBranch, TriggerCIRerun, DeleteAskBranch, MergeAndCleanup,
// and completeAskBranchResolution.
// Fix #8e: ctx threads through GetDefaultBranch so the symbolic-ref/rev-parse
// lookup cancels on daemon shutdown.
func AssertNotDefaultBranch(ctx context.Context, repoPath, branch string) error {
	if strings.TrimSpace(branch) == "" {
		return fmt.Errorf("%w: empty branch name", ErrProtectedBranch)
	}
	canonical := canonicaliseRef(branch)
	if _, denied := protectedBranchNames[strings.ToLower(canonical)]; denied {
		return fmt.Errorf("%w: %q is a protected/default branch name", ErrProtectedBranch, branch)
	}
	if repoPath != "" {
		def := GetDefaultBranch(ctx, repoPath)
		// GetDefaultBranch falls back to "main" when discovery fails. Match
		// exactly; the denylist above already handles the common names.
		if def != "" && strings.EqualFold(canonical, def) {
			return fmt.Errorf("%w: %q matches the default branch of %s", ErrProtectedBranch, branch, repoPath)
		}
	}
	return nil
}

// canonicaliseRef strips common ref prefixes and trims whitespace so a caller
// that passes "refs/heads/main" or " origin/main " is caught by the denylist.
func canonicaliseRef(branch string) string {
	out := strings.TrimSpace(branch)
	for {
		progress := false
		for _, p := range refNamePrefixesToStrip {
			if strings.HasPrefix(out, p) {
				out = out[len(p):]
				progress = true
			}
		}
		if !progress {
			break
		}
	}
	return out
}

// askBranchPattern matches a well-formed ask-branch:
//
//	<prefix>force/ask-<digits>-<slug>
//
// where <prefix> is either empty (no gh username detected) or the sanitized
// username followed by '/'. The prefix is checked via BranchPrefix() at the
// call site — this regex focuses on the `force/ask-<N>-<slug>` tail.
var askBranchTailPattern = regexp.MustCompile(`^force/ask-[0-9]+-[a-z0-9][a-z0-9_-]*$`)

// IsValidAskBranch reports whether `branch` is well-formed for a convoy
// ask-branch. Used by completeAskBranchResolution before force-pushing.
// A "main" or "refs/heads/<anything>" branch will fail this check even
// without the default-branch guard — defence in depth.
func IsValidAskBranch(branch string) bool {
	if branch == "" {
		return false
	}
	prefix := BranchPrefix()
	tail := branch
	if prefix != "" {
		if !strings.HasPrefix(branch, prefix) {
			return false
		}
		tail = strings.TrimPrefix(branch, prefix)
	}
	return askBranchTailPattern.MatchString(tail)
}

// IsProtectedBranchName reports whether `branch` is on the hard-coded
// default-branch denylist. Intended for ingress validators at the store
// boundary (UpsertConvoyAskBranch, SetBranchName) where no repo path is
// available to consult GetDefaultBranch. Exported separately from
// AssertNotDefaultBranch so store code can call it without importing the
// full git-op surface.
func IsProtectedBranchName(branch string) bool {
	if strings.TrimSpace(branch) == "" {
		return true
	}
	canonical := canonicaliseRef(branch)
	_, denied := protectedBranchNames[strings.ToLower(canonical)]
	return denied
}
