package git

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
)

// ── Ask-branch operations ────────────────────────────────────────────────────
//
// Helpers for Pilot's CreateAskBranch / RebaseAskBranch / CleanupAskBranch
// tasks. These are thin wrappers around git commands; the policy (retries,
// conflict handling) lives in Pilot.

// FetchMain runs `git fetch origin <default>` to refresh main's tip. Returns
// the new HEAD SHA, or error.
// Fix #8e: ctx threads from the caller so the network op cancels on shutdown.
func FetchMain(ctx context.Context, repoPath string) (string, error) {
	base := GetDefaultBranch(ctx, repoPath)
	// `--` after the remote name keeps the refspec positional (Fix #9).
	if out, err := runGitCtx(ctx, "-C", repoPath, "fetch", "origin", "--", base); err != nil {
		return "", fmt.Errorf("git fetch: %s", strings.TrimSpace(string(out)))
	}
	// `--verify` + trailing `--` pins the arg to a single positional rev;
	// plain `rev-parse <ref> --` would echo a spurious `--` line.
	out, err := runGitCtx(ctx, "-C", repoPath, "rev-parse", "--verify", "refs/remotes/origin/"+base, "--")
	if err != nil {
		return "", fmt.Errorf("git rev-parse: %s", strings.TrimSpace(string(out)))
	}
	return strings.TrimSpace(string(out)), nil
}

// RemoteHeadSHA returns the HEAD SHA of the default branch on origin via
// `git ls-remote` — does NOT fetch, only queries. This is the cheap
// event-detection call main-drift-watch uses every 15 minutes.
// Fix #8e: ctx threads from the caller (the dog ctx) so daemon cancellation
// stops the ls-remote network op.
func RemoteHeadSHA(ctx context.Context, repoPath string) (string, error) {
	base := GetDefaultBranch(ctx, repoPath)
	// `--` before the refspec keeps it positional (Fix #9).
	out, err := runGitCtx(ctx, "-C", repoPath, "ls-remote", "--", "origin", "refs/heads/"+base)
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
// Fix #8e: ctx threads from the caller (Pilot's claim ctx) so a hung
// fetch/push on a slow remote cancels on daemon shutdown.
func CreateAskBranch(ctx context.Context, repoPath, branchName string) (string, error) {
	if branchName == "" {
		return "", fmt.Errorf("CreateAskBranch: branchName required")
	}
	base := GetDefaultBranch(ctx, repoPath)

	// Step 1: fetch. `--` separator before the positional refspec (Fix #9).
	if out, err := runGitCtx(ctx, "-C", repoPath, "fetch", "origin", "--", base); err != nil {
		return "", fmt.Errorf("git fetch: %s", strings.TrimSpace(string(out)))
	}

	// Capture the base SHA BEFORE any branch work so it reflects the main tip
	// we're branching from. Using refs/remotes/origin/<base> is what origin's
	// current HEAD actually is. `--verify` + trailing `--` keeps the rev
	// positional and suppresses the stray `--` echo line.
	shaOut, err := runGitCtx(ctx, "-C", repoPath, "rev-parse", "--verify", "refs/remotes/origin/"+base, "--")
	if err != nil {
		return "", fmt.Errorf("git rev-parse origin/%s: %s", base, strings.TrimSpace(string(shaOut)))
	}
	baseSHA := strings.TrimSpace(string(shaOut))
	if baseSHA == "" {
		return "", fmt.Errorf("empty base SHA for origin/%s", base)
	}

	// Step 2: create the branch locally. If it already exists, force-update it
	// to the same SHA so we're idempotent. `--` after `-f` keeps (branch, sha)
	// positional (Fix #9).
	bestEffortRun(ctx, "ask-branch create local", "-C", repoPath, "branch", "-f", "--", branchName, baseSHA)

	// Step 3: push to origin. Accepts the case where origin already has the
	// branch at the same SHA (no-op push). `--` keeps the branch positional.
	if out, err := runGitCtxOutput(ctx, "-C", repoPath, "push", "-u", "origin", "--", branchName); err != nil {
		// If the branch already exists on origin at a different SHA, do not
		// force-push here — that's for the rebase flow, not initial create.
		return "", fmt.Errorf("git push: %s", strings.TrimSpace(string(out)))
	}
	return baseSHA, nil
}

// DeleteAskBranch deletes the branch both locally and on origin. Idempotent:
// missing branches are not errors.
// Fix #8e: ctx threads from the caller.
// D2 T1-4: db + repoName threaded for the AssertRepoWritable mode guard.
// Pass (nil, "") in tests that exercise raw git mechanics without a
// Holocron — the guard is a no-op in that case.
func DeleteAskBranch(ctx context.Context, db *sql.DB, repoName, repoPath, branchName string) error {
	if branchName == "" {
		return fmt.Errorf("DeleteAskBranch: branchName required")
	}
	if err := AssertNotDefaultBranch(ctx, repoPath, branchName); err != nil {
		return fmt.Errorf("DeleteAskBranch refused: %w", err)
	}
	if err := AssertRepoWritable(db, repoName); err != nil {
		return fmt.Errorf("DeleteAskBranch refused: %w", err)
	}
	// Local delete — ignore errors (branch may not exist locally). `--` keeps
	// the branch positional (Fix #9).
	bestEffortRun(ctx, "ask-branch local delete", "-C", repoPath, "branch", "-D", "--", branchName)
	// Remote delete — a 404-style response is fine. `--` after `--delete`
	// keeps the branch positional.
	out, err := runGitCtx(ctx, "-C", repoPath, "push", "origin", "--delete", "--", branchName)
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
// Fix #8e: ctx threads from the caller (Pilot's claim ctx) so a hung
// fetch/rebase cancels on daemon shutdown.
func RebaseBranchOnto(ctx context.Context, repoPath, branch, onto string) (newTipSHA string, err error) {
	// Fetch both branches so origin refs are current. This updates
	// refs/remotes/origin/<name>, but NOT the local refs/heads/<name>.
	// `--` separator keeps the refspecs positional (Fix #9).
	if out, fetchErr := runGitCtx(ctx, "-C", repoPath, "fetch", "origin", "--", onto, branch); fetchErr != nil {
		return "", fmt.Errorf("git fetch: %s", strings.TrimSpace(string(out)))
	}

	// Create a temporary worktree so we never disturb the main checkout's HEAD.
	wtPath, wtErr := os.MkdirTemp("", "force-rebase-*")
	if wtErr != nil {
		return "", fmt.Errorf("mktemp: %w", wtErr)
	}
	defer func() {
		// AUDIT-165 (Fix #8d): wrap the worktree-remove so a hung
		// `git worktree remove` can't wedge this deferred cleanup.
		// Fix #8e: routed through bestEffortRun, which now derives its
		// timeout from the caller ctx so daemon shutdown cancels even
		// the cleanup. os.RemoveAll runs unconditionally — we'd rather
		// leave a stale git metadata entry than leak the tmpdir.
		bestEffortRun(ctx, "rebase-temp worktree remove", "-C", repoPath, "worktree", "remove", "--force", wtPath)
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
	// `--` separator before (branch, wtPath, ref) positionals (Fix #9).
	if out, wtAddErr := runGitCtx(ctx, "-C", repoPath, "worktree", "add",
		"-B", branch, "--", wtPath, "refs/remotes/origin/"+branch); wtAddErr != nil {
		return "", fmt.Errorf("git worktree add: %s", strings.TrimSpace(string(out)))
	}

	// Rebase. Conflicts leave the worktree mid-rebase; we abort and return an
	// error with conflict output so Pilot can spawn a RebaseConflict task.
	// `--` separator before the positional onto-ref (Fix #9).
	if out, rbErr := runGitCtx(ctx, "-C", wtPath, "rebase", "--", "refs/remotes/origin/"+onto); rbErr != nil {
		// Wrapped to keep shell-boundary grep clean — see abortOp doc.
		abortOp(ctx, wtPath, "rebase")
		return "", fmt.Errorf("git rebase: %s", strings.TrimSpace(string(out)))
	}

	// Capture the new tip SHA from the worktree. `--verify` + trailing `--`
	// pins the rev and suppresses the spurious `--` echo (Fix #9).
	shaOut, shaErr := runGitCtx(ctx, "-C", wtPath, "rev-parse", "--verify", "HEAD", "--")
	if shaErr != nil {
		return "", fmt.Errorf("git rev-parse HEAD: %s", strings.TrimSpace(string(shaOut)))
	}
	return strings.TrimSpace(string(shaOut)), nil
}

// unionMergeableGlobs enumerates file patterns where "both sides appended
// text" is the overwhelmingly common conflict shape AND where concatenation
// is the semantically correct resolution. Deliberately narrow: markdown
// docs and machine-generated lockfiles only. We do NOT include *.txt or
// *.go — those can contain arbitrary content where concatenation would
// produce semantic nonsense (garbled prose, invalid Go). CLAUDE.md is the
// observed blocker from the production convoys 35/37 failure; the lockfile
// entries are preemptive for the same append-only conflict shape.
var unionMergeableGlobs = []string{
	"*.md merge=union",
	"CLAUDE.md merge=union",
	"go.sum merge=union",
	"package-lock.json merge=union",
	"yarn.lock merge=union",
	"Cargo.lock merge=union",
}

// MergeWithUnionStrategy merges `baseRef` into `branch` in a temporary
// worktree, with union-strategy resolution installed for markdown/docs/lock
// files via LOCAL (uncommitted) .git/info/attributes. Both sides' additions
// in those files are concatenated rather than producing conflict markers —
// the exact pattern that blocked main-drift-watch on convoys 35/37 (tasks
// 519 and 537 both hit 5 Claude CLI infra failures trying to LLM-resolve
// identical CLAUDE.md append-vs-append conflicts).
//
// Uses a throwaway worktree so the main checkout's HEAD is never moved
// (same discipline as RebaseBranchOnto). The local ref `refs/heads/<branch>`
// IS updated to point at the new merge commit — callers then push that
// branch normally. The attributes file is written before and restored after,
// so ask-branch history never records any attributes change.
//
// Returns the new merge-commit SHA on success. Returns error when conflicts
// are structural (binary files, concurrent edits to same function body) —
// caller should fall through to LLM-driven astromech resolution.
// Fix #8e: ctx threads from the caller (Pilot's claim ctx).
func MergeWithUnionStrategy(ctx context.Context, repoPath, branch, baseRef, message string) (string, error) {
	if branch == "" || baseRef == "" {
		return "", fmt.Errorf("branch and baseRef required")
	}

	// AUDIT-155 (Fix #8d): acquire the per-repo merge lock before rewriting
	// .git/info/attributes. Two concurrent union-merges in the same repo
	// would otherwise race: one caller's `defer restore` could run while
	// the other is mid-merge, reverting the attributes file and producing
	// spurious conflict-marker storms (which cascade into RebaseConflict
	// escalations). The lock is shared with MergeAndCleanup — the two
	// operations both mutate the repo's `.git/info/attributes` and/or
	// shared main worktree, so serialising them per-repo is correct.
	mu := lockRepoForMerge(repoPath)
	mu.Lock()
	defer mu.Unlock()

	// Fetch both refs so we're merging against current origin state.
	// `--` separator before the branch refspec (Fix #9).
	if out, err := runGitCtx(ctx, "-C", repoPath, "fetch", "origin", "--", branch); err != nil {
		// Non-fatal — the branch may exist only locally. Record and continue.
		_ = out
	}
	// For baseRef like "refs/remotes/origin/main", fetch the short name.
	shortBase := strings.TrimPrefix(baseRef, "refs/remotes/origin/")
	if shortBase != baseRef {
		runGitCtx(ctx, "-C", repoPath, "fetch", "origin", "--", shortBase)
	}

	wtPath, wtErr := os.MkdirTemp("", "force-union-merge-*")
	if wtErr != nil {
		return "", fmt.Errorf("mktemp: %w", wtErr)
	}
	defer func() {
		bestEffortRun(ctx, "worktree remove after union merge", "-C", repoPath, "worktree", "remove", "--force", wtPath)
		os.RemoveAll(wtPath)
	}()

	// Check out the branch in the temp worktree, resetting local ref from
	// origin — same `-B` discipline as RebaseBranchOnto. `--` separator
	// before positional (path, ref) pair (Fix #9).
	if out, err := runGitCtx(ctx, "-C", repoPath, "worktree", "add", "-B", branch, "--", wtPath,
		"refs/remotes/origin/"+branch); err != nil {
		return "", fmt.Errorf("worktree add %s: %s", branch, strings.TrimSpace(string(out)))
	}

	// Install union attributes locally. Written to .git/info/attributes —
	// a working-copy-only file never committed.
	//
	// AUDIT-099 (Fix #8d): atomic write via .tmp + os.Rename. Pre-fix,
	// os.WriteFile truncate-then-write left a window where a crash or
	// SIGKILL between truncate and write-complete would produce an
	// empty/partial attributes file whose deferred restore would still
	// run (on normal exit) but whose content would have been half-
	// written on abnormal exit. The atomic rename is crash-safe: either
	// the new content is fully visible, or the old content is still
	// there.
	//
	// Signal handler: we also register a SIGINT/SIGTERM handler so that
	// on operator-initiated shutdown the original attributes are
	// restored BEFORE the daemon exits (defer doesn't fire on SIGKILL
	// and races with the signal on SIGTERM). The handler is set per-
	// call and deregistered by the defer.
	attrPath := filepath.Join(repoPath, ".git", "info", "attributes")
	existing, _ := os.ReadFile(attrPath)
	content := strings.Join(unionMergeableGlobs, "\n") + "\n" + string(existing)
	tmpPath := attrPath + ".tmp"
	if err := os.WriteFile(tmpPath, []byte(content), 0644); err != nil {
		return "", fmt.Errorf("write local attributes tmp: %w", err)
	}
	if err := os.Rename(tmpPath, attrPath); err != nil {
		_ = os.Remove(tmpPath)
		return "", fmt.Errorf("rename local attributes tmp: %w", err)
	}
	restoreAttrs := func() {
		if len(existing) == 0 {
			os.Remove(attrPath) //nolint:errcheck
		} else {
			restoreTmp := attrPath + ".restore.tmp"
			if werr := os.WriteFile(restoreTmp, existing, 0644); werr == nil {
				_ = os.Rename(restoreTmp, attrPath)
			}
		}
	}
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	sigDone := make(chan struct{})
	go func() {
		select {
		case <-sigCh:
			restoreAttrs()
		case <-sigDone:
		}
	}()
	defer func() {
		close(sigDone)
		signal.Stop(sigCh)
		restoreAttrs()
	}()

	if message == "" {
		message = "merge: pull " + baseRef + " into " + branch + " via union strategy"
	}

	// Merge with identity env so git doesn't refuse on a missing global config.
	// `--` separator before the positional baseRef (Fix #9).
	// Fix #8e: derive the timeout-bounded ctx from the caller's ctx so
	// daemon shutdown / e-stop cancels the merge subprocess. Pre-fix this
	// site fabricated a context.Background root.
	mergeCtx, mergeCancel := context.WithTimeout(ctx, shortGitOpTimeout)
	defer mergeCancel()
	cmd := exec.CommandContext(mergeCtx, "git", "-C", wtPath, "merge", "--no-ff", "-m", message, "--", baseRef)
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=force-orchestrator",
		"GIT_AUTHOR_EMAIL=force@localhost",
		"GIT_COMMITTER_NAME=force-orchestrator",
		"GIT_COMMITTER_EMAIL=force@localhost",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		bestEffortRun(ctx, "merge --abort after union-merge failure", "-C", wtPath, "merge", "--abort")
		return "", fmt.Errorf("union merge %s into %s: %s", baseRef, branch, strings.TrimSpace(string(out)))
	}
	// Paranoid: verify no unresolved paths even when git exits 0.
	if statusOut, _ := runGitCtxOutput(ctx, "-C", wtPath, "diff", "--name-only", "--diff-filter=U"); strings.TrimSpace(string(statusOut)) != "" {
		bestEffortRun(ctx, "merge --abort after unresolved-paths detection", "-C", wtPath, "merge", "--abort")
		return "", fmt.Errorf("union merge left unresolved paths: %s", strings.TrimSpace(string(statusOut)))
	}

	// `--verify` + trailing `--` pins the rev and suppresses the spurious
	// `--` echo that plain rev-parse prints (Fix #9).
	tipOut, tipErr := runGitCtxOutput(ctx, "-C", wtPath, "rev-parse", "--verify", "HEAD", "--")
	if tipErr != nil {
		return "", fmt.Errorf("rev-parse HEAD: %w", tipErr)
	}
	return strings.TrimSpace(string(tipOut)), nil
}

// ForcePushBranch force-pushes a branch to origin with lease. Used after a
// rebase — --force-with-lease fails if the remote has advanced, preventing us
// from clobbering another push that raced us.
// Fix #8e: ctx threads from the caller (Pilot/Diplomat claim ctx) so the
// network push cancels on daemon shutdown.
// D2 T1-4: db + repoName threaded for the AssertRepoWritable mode guard.
// Pass (nil, "") in tests that exercise raw push mechanics.
func ForcePushBranch(ctx context.Context, db *sql.DB, repoName, repoPath, branch string) error {
	if err := AssertNotDefaultBranch(ctx, repoPath, branch); err != nil {
		return fmt.Errorf("ForcePushBranch refused: %w", err)
	}
	if err := AssertRepoWritable(db, repoName); err != nil {
		return fmt.Errorf("ForcePushBranch refused: %w", err)
	}
	// `--` separator before the branch positional (Fix #9).
	out, err := runGitCtxOutput(ctx, "-C", repoPath, "push", "--force-with-lease", "origin", "--", branch)
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
// Fix #8e: ctx threads from the caller (the dog ctx for stall retriggers)
// so the network fetch/push cancels on daemon shutdown.
// D2 T1-4: db + repoName threaded for the AssertRepoWritable mode guard.
// Pass (nil, "") in tests that exercise raw retrigger mechanics.
func TriggerCIRerun(ctx context.Context, db *sql.DB, repoName, repoPath, branch, message string) error {
	if err := AssertNotDefaultBranch(ctx, repoPath, branch); err != nil {
		return fmt.Errorf("TriggerCIRerun refused: %w", err)
	}
	if err := AssertRepoWritable(db, repoName); err != nil {
		return fmt.Errorf("TriggerCIRerun refused: %w", err)
	}
	if message == "" {
		message = "ci: trigger stalled check run"
	}
	// `--` separator before the branch refspec (Fix #9).
	if out, err := runGitCtx(ctx, "-C", repoPath, "fetch", "origin", "--", branch); err != nil {
		return fmt.Errorf("git fetch %s: %s", branch, strings.TrimSpace(string(out)))
	}

	// `--verify` + trailing `--` pins the rev positional and suppresses the
	// spurious `--` echo plain rev-parse would print (Fix #9).
	treeOut, err := runGitCtx(ctx, "-C", repoPath, "rev-parse", "--verify", "origin/"+branch+"^{tree}", "--")
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
	// Trailing `--` defence-in-depth on the positional tree SHA (Fix #9).
	// Fix #8e: derive the bounded ctx from the caller's ctx so daemon
	// shutdown cancels commit-tree.
	ctCtx, ctCancel := context.WithTimeout(ctx, shortGitOpTimeout)
	defer ctCancel()
	cmd := exec.CommandContext(ctCtx, "git", "-C", repoPath, "commit-tree", treeSHA, "-p", "origin/"+branch, "-m", message, "--")
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

	if out, err := runGitCtx(ctx, "-C", repoPath, "push", "origin", newSHA+":refs/heads/"+branch); err != nil {
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
