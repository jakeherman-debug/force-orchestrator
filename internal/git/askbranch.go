package git

import (
	"fmt"
	"os"
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
// the new tip SHA on success. On conflict, returns an error.
//
// Uses a temporary git worktree so the operation never touches the HEAD of the
// caller's checkout — the main working directory stays on whatever branch the
// operator has checked out.
//
// This deliberately does NOT force-push — the caller (Pilot) decides whether
// to push after classifying the outcome.
func RebaseBranchOnto(repoPath, branch, onto string) (newTipSHA string, err error) {
	// Fetch both branches so origin refs are current. This updates
	// refs/remotes/origin/<name>, but NOT the local refs/heads/<name>.
	if out, fetchErr := exec.Command("git", "-C", repoPath, "fetch", "origin", onto, branch).CombinedOutput(); fetchErr != nil {
		return "", fmt.Errorf("git fetch: %s", strings.TrimSpace(string(out)))
	}

	// Create a temporary worktree so we never disturb the main checkout's HEAD.
	wtPath, wtErr := os.MkdirTemp("", "force-rebase-*")
	if wtErr != nil {
		return "", fmt.Errorf("mktemp: %w", wtErr)
	}
	defer func() {
		exec.Command("git", "-C", repoPath, "worktree", "remove", "--force", wtPath).Run()
		os.RemoveAll(wtPath)
	}()

	// Check out `branch` in the worktree, CREATING OR RESETTING the local
	// ref to match origin/<branch>. Without the `-B` form that resets from
	// origin, the worktree would check out a stale local branch ref — which
	// is how task 292 silently lost its sub-PR merge commits: sibling sub-PRs
	// had merged into the branch on origin, the local ref was never updated
	// after the initial fetch at ask-branch creation, and a "rebase" then
	// replayed nothing (because stale local == origin/main), followed by a
	// --force-with-lease push that silently reset origin to the stale tip.
	// Force-with-lease doesn't guard against backwards moves from a stale
	// starting point, only against concurrent writes — so this would have
	// looked clean but was catastrophic.
	if out, wtAddErr := exec.Command("git", "-C", repoPath, "worktree", "add",
		"-B", branch, wtPath, "refs/remotes/origin/"+branch).CombinedOutput(); wtAddErr != nil {
		return "", fmt.Errorf("git worktree add: %s", strings.TrimSpace(string(out)))
	}

	// Rebase. Conflicts leave the worktree mid-rebase; we abort and return an
	// error with conflict output so Pilot can spawn a RebaseConflict task.
	if out, rbErr := exec.Command("git", "-C", wtPath, "rebase", "refs/remotes/origin/"+onto).CombinedOutput(); rbErr != nil {
		exec.Command("git", "-C", wtPath, "rebase", "--abort").Run()
		return "", fmt.Errorf("git rebase: %s", strings.TrimSpace(string(out)))
	}

	// Capture the new tip SHA from the worktree.
	shaOut, shaErr := exec.Command("git", "-C", wtPath, "rev-parse", "HEAD").CombinedOutput()
	if shaErr != nil {
		return "", fmt.Errorf("git rev-parse HEAD: %s", strings.TrimSpace(string(shaOut)))
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

// TriggerCIRerun adds an empty commit to `branch` and pushes it to origin.
// The new HEAD SHA re-triggers any push-event-driven CI, which is how we
// recover from stuck check runs (checks stayed QUEUED and never ran because
// the runner never picked up the original push event).
//
// Implemented with git plumbing (commit-tree + push) instead of
// checkout/commit/push so the main worktree's HEAD is never moved. This
// matters because the target branch is often an astromech's agent branch
// that is ALREADY checked out in its persistent worktree (e.g.
// .force-worktrees/force-orchestrator/R7-A7). A `git checkout` in the main
// worktree would fail with "already used by worktree at ..." — which is
// exactly the production failure that forced this rewrite.
//
// The sequence:
//   1. fetch origin <branch> — pulls the latest ref without touching HEAD
//   2. rev-parse origin/<branch>^{tree} — gets the tree OID of the tip
//   3. commit-tree <tree> -p origin/<branch> -m <msg> — creates a new empty
//      commit whose parent is the current tip, reusing the tree
//   4. push origin <newsha>:refs/heads/<branch> — fast-forwards origin
//
// None of those commands read or write the working tree. Safe regardless of
// what else is checked out.
func TriggerCIRerun(repoPath, branch, message string) error {
	if message == "" {
		message = "ci: trigger stalled check run"
	}
	if out, err := exec.Command("git", "-C", repoPath, "fetch", "origin", branch).CombinedOutput(); err != nil {
		return fmt.Errorf("git fetch %s: %s", branch, strings.TrimSpace(string(out)))
	}

	treeOut, err := exec.Command("git", "-C", repoPath, "rev-parse", "origin/"+branch+"^{tree}").Output()
	if err != nil {
		stderr := ""
		if ee, ok := err.(*exec.ExitError); ok {
			stderr = strings.TrimSpace(string(ee.Stderr))
		}
		return fmt.Errorf("rev-parse origin/%s^{tree}: %s", branch, stderr)
	}
	treeSHA := strings.TrimSpace(string(treeOut))

	// commit-tree reads the message from stdin — avoids shell-escape hazards
	// when the caller passes arbitrary text. Parent is origin/<branch>^{0}
	// (dereferenced commit OID) so the new commit FFs cleanly from the tip.
	cmd := exec.Command("git", "-C", repoPath, "commit-tree", treeSHA, "-p", "origin/"+branch, "-m", message)
	// Inherit the real environment (PATH, HOME, etc.) and OVERRIDE just the
	// author/committer identity — otherwise commit-tree refuses to run if no
	// user.email is configured for the invoking shell.
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=force-orchestrator",
		"GIT_AUTHOR_EMAIL=force@localhost",
		"GIT_COMMITTER_NAME=force-orchestrator",
		"GIT_COMMITTER_EMAIL=force@localhost",
	)
	newOut, err := cmd.Output()
	if err != nil {
		stderr := ""
		if ee, ok := err.(*exec.ExitError); ok {
			stderr = strings.TrimSpace(string(ee.Stderr))
		}
		return fmt.Errorf("commit-tree: %s", stderr)
	}
	newSHA := strings.TrimSpace(string(newOut))

	if out, err := exec.Command("git", "-C", repoPath, "push", "origin",
		newSHA+":refs/heads/"+branch).CombinedOutput(); err != nil {
		return fmt.Errorf("git push origin %s:refs/heads/%s: %s",
			newSHA[:minInt(8, len(newSHA))], branch, strings.TrimSpace(string(out)))
	}
	return nil
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
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
