package git

import (
	"fmt"
	"os/exec"
	"strings"
)

// ── Ask-branch operations ────────────────────────────────────────────────────
//
// Helpers for Pilot's CreateAskBranch / RebaseAskBranch / CleanupAskBranch
// tasks. These are thin wrappers around git commands; the policy (retries,
// conflict handling) lives in Pilot.

// FetchMain runs `git fetch origin <default>` to refresh main's tip. Returns
// the new HEAD SHA, or error.
func FetchMain(repoPath string) (string, error) {
	base := GetDefaultBranch(repoPath)
	if out, err := exec.Command("git", "-C", repoPath, "fetch", "origin", base).CombinedOutput(); err != nil {
		return "", fmt.Errorf("git fetch: %s", strings.TrimSpace(string(out)))
	}
	out, err := exec.Command("git", "-C", repoPath, "rev-parse", "refs/remotes/origin/"+base).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git rev-parse: %s", strings.TrimSpace(string(out)))
	}
	return strings.TrimSpace(string(out)), nil
}

// RemoteHeadSHA returns the HEAD SHA of the default branch on origin via
// `git ls-remote` — does NOT fetch, only queries. This is the cheap
// event-detection call main-drift-watch uses every 15 minutes.
func RemoteHeadSHA(repoPath string) (string, error) {
	base := GetDefaultBranch(repoPath)
	out, err := exec.Command("git", "-C", repoPath, "ls-remote", "origin", "refs/heads/"+base).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git ls-remote: %s", strings.TrimSpace(string(out)))
	}
	// Output format: "<SHA>\trefs/heads/<branch>"
	line := strings.TrimSpace(string(out))
	if line == "" {
		return "", fmt.Errorf("git ls-remote: empty response (branch %q missing on origin?)", base)
	}
	parts := strings.Fields(line)
	if len(parts) < 1 {
		return "", fmt.Errorf("git ls-remote: unexpected output %q", line)
	}
	return parts[0], nil
}

// CreateAskBranch cuts a new branch at the current tip of origin's default branch
// and pushes it. Returns the SHA the branch points at (the base SHA recorded in
// ConvoyAskBranches) or error.
//
// The flow:
//
//  1. `git fetch origin <default>` so origin/<default> is current
//  2. `git branch <branchName> refs/remotes/origin/<default>` locally
//  3. `git push origin <branchName>`
//
// If the branch already exists locally or on origin, this is idempotent: the
// local branch operation either no-ops or we force-reset to the remote SHA.
func CreateAskBranch(repoPath, branchName string) (string, error) {
	if branchName == "" {
		return "", fmt.Errorf("CreateAskBranch: branchName required")
	}
	base := GetDefaultBranch(repoPath)

	// Step 1: fetch.
	if out, err := exec.Command("git", "-C", repoPath, "fetch", "origin", base).CombinedOutput(); err != nil {
		return "", fmt.Errorf("git fetch: %s", strings.TrimSpace(string(out)))
	}

	// Capture the base SHA BEFORE any branch work so it reflects the main tip
	// we're branching from. Using refs/remotes/origin/<base> is what origin's
	// current HEAD actually is.
	shaOut, err := exec.Command("git", "-C", repoPath, "rev-parse", "refs/remotes/origin/"+base).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git rev-parse origin/%s: %s", base, strings.TrimSpace(string(shaOut)))
	}
	baseSHA := strings.TrimSpace(string(shaOut))
	if baseSHA == "" {
		return "", fmt.Errorf("empty base SHA for origin/%s", base)
	}

	// Step 2: create the branch locally. If it already exists, force-update it
	// to the same SHA so we're idempotent.
	exec.Command("git", "-C", repoPath, "branch", "-f", branchName, baseSHA).Run()

	// Step 3: push to origin. Accepts the case where origin already has the
	// branch at the same SHA (no-op push).
	if out, err := exec.Command("git", "-C", repoPath, "push", "-u", "origin", branchName).CombinedOutput(); err != nil {
		// If the branch already exists on origin at a different SHA, do not
		// force-push here — that's for the rebase flow, not initial create.
		return "", fmt.Errorf("git push: %s", strings.TrimSpace(string(out)))
	}
	return baseSHA, nil
}

// DeleteAskBranch deletes the branch both locally and on origin. Idempotent:
// missing branches are not errors.
func DeleteAskBranch(repoPath, branchName string) error {
	if branchName == "" {
		return fmt.Errorf("DeleteAskBranch: branchName required")
	}
	// Local delete — ignore errors (branch may not exist locally).
	exec.Command("git", "-C", repoPath, "branch", "-D", branchName).Run()
	// Remote delete — a 404-style response is fine.
	out, err := exec.Command("git", "-C", repoPath, "push", "origin", "--delete", branchName).CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if strings.Contains(msg, "remote ref does not exist") || strings.Contains(msg, "unable to delete") {
			return nil
		}
		return fmt.Errorf("git push --delete: %s", msg)
	}
	return nil
}

// RebaseBranchOnto rebases branch onto the latest origin/<onto> and returns
// the new tip SHA on success. On conflict, returns an error and leaves the
// repo in a state the caller can inspect (rebase --abort is run first).
//
// This deliberately does NOT force-push — the caller (Pilot) decides whether
// to push after classifying the outcome.
func RebaseBranchOnto(repoPath, branch, onto string) (newTipSHA string, err error) {
	// Safety: always start from a clean state.
	exec.Command("git", "-C", repoPath, "rebase", "--abort").Run()
	exec.Command("git", "-C", repoPath, "merge", "--abort").Run()

	// Fetch the onto branch fresh.
	if out, fetchErr := exec.Command("git", "-C", repoPath, "fetch", "origin", onto).CombinedOutput(); fetchErr != nil {
		return "", fmt.Errorf("git fetch origin %s: %s", onto, strings.TrimSpace(string(out)))
	}
	// Check out the branch to rebase.
	if out, coErr := exec.Command("git", "-C", repoPath, "checkout", branch).CombinedOutput(); coErr != nil {
		return "", fmt.Errorf("git checkout %s: %s", branch, strings.TrimSpace(string(out)))
	}
	// Rebase. Conflicts leave the repo mid-rebase — we abort and return error
	// with the conflict output so Pilot can spawn a RebaseConflict task.
	if out, rbErr := exec.Command("git", "-C", repoPath, "rebase", "refs/remotes/origin/"+onto).CombinedOutput(); rbErr != nil {
		exec.Command("git", "-C", repoPath, "rebase", "--abort").Run()
		return "", fmt.Errorf("git rebase: %s", strings.TrimSpace(string(out)))
	}
	// Get the new tip SHA.
	shaOut, shaErr := exec.Command("git", "-C", repoPath, "rev-parse", branch).CombinedOutput()
	if shaErr != nil {
		return "", fmt.Errorf("git rev-parse %s: %s", branch, strings.TrimSpace(string(shaOut)))
	}
	return strings.TrimSpace(string(shaOut)), nil
}

// ForcePushBranch force-pushes a branch to origin with lease. Used after a
// rebase — --force-with-lease fails if the remote has advanced, preventing us
// from clobbering another push that raced us.
func ForcePushBranch(repoPath, branch string) error {
	out, err := exec.Command("git", "-C", repoPath, "push", "--force-with-lease", "origin", branch).CombinedOutput()
	if err != nil {
		return fmt.Errorf("git push --force-with-lease: %s", strings.TrimSpace(string(out)))
	}
	return nil
}

// BranchNameSlug sanitises a string into a git-branch-safe slug: lowercases,
// replaces non-alphanumerics with '-', collapses runs of '-', trims leading/
// trailing '-', and caps at maxLen characters. An empty result yields "ask".
func BranchNameSlug(s string, maxLen int) string {
	if maxLen <= 0 {
		maxLen = 40
	}
	var b strings.Builder
	lastDash := false
	for _, r := range s {
		var c byte
		switch {
		case r >= 'A' && r <= 'Z':
			c = byte(r-'A') + 'a'
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			c = byte(r)
		default:
			if !lastDash && b.Len() > 0 {
				b.WriteByte('-')
				lastDash = true
			}
			continue
		}
		b.WriteByte(c)
		lastDash = false
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		return "ask"
	}
	if len(out) > maxLen {
		out = strings.TrimRight(out[:maxLen], "-")
	}
	if out == "" {
		return "ask"
	}
	return out
}
