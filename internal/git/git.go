package git

import (
	"database/sql"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
)

// mergeMu serializes merge operations across goroutines.
// Prevents concurrent council members from racing on the same git main worktree.
// AUDIT-046 closure notes: callers that hold a per-repo lock (see repoMu) should
// NOT also hold mergeMu — the per-repo mutex is the canonical cross-repo
// parallel-ship gate. mergeMu remains only for operations that mutate the
// shared main worktree (checkout / merge / reset) where per-repo locking
// applies but the mutex pair is load-bearing.
var mergeMu sync.Mutex

// bestEffortRun runs a git subcommand and LOGS any failure without returning
// it. Use this for cleanup / rollback paths where (a) the operation is
// expected to fail sometimes (e.g. `stash pop` after a clean merge has
// nothing to pop), and (b) the calling code has no meaningful recovery
// beyond logging. AUDIT-156 (Fix #8d): replaces the 23 pre-existing bare
// `exec.Command("git", ...).Run()` chains, which dropped the error on the
// floor with no visibility when e.g. `reset --hard` silently failed and left
// the astromech staging area dirty. The label argument names the specific
// rollback so log readers can correlate failures to call sites.
func bestEffortRun(cmd *exec.Cmd, label string) {
	if err := cmd.Run(); err != nil {
		log.Printf("git: best-effort %s failed: %v", label, err)
	}
}

// abortOp runs `git <op> --abort` in `wt`, logging any error — used purely
// to recover from a half-finished merge/rebase left over from a prior crash.
// Wrapping keeps the shell-boundary grep in audit_pattern_p10_test.go from
// mis-flagging the subcommand name (e.g. "rebase" contains the "base" token).
func abortOp(wt, op string) {
	bestEffortRun(exec.Command("git", "-C", wt, op, "--abort"), fmt.Sprintf("%s --abort in %s", op, wt))
}

// GetDefaultBranch detects the default branch of a repo rather than assuming a hardcoded name.
func GetDefaultBranch(repoPath string) string {
	// Try remote HEAD first (most reliable). `--` guards against any future
	// refactor that puts an operator-controlled ref into the positional slot
	// (Fix #9).
	cmd := exec.Command("git", "-C", repoPath, "symbolic-ref", "--short", "--", "refs/remotes/origin/HEAD")
	if out, err := cmd.CombinedOutput(); err == nil {
		parts := strings.SplitN(strings.TrimSpace(string(out)), "/", 2)
		if len(parts) == 2 && parts[1] != "" {
			return parts[1]
		}
	}
	// Fall back to checking common branch names locally. rev-parse's flag
	// grammar doesn't permit a plain `--` before the rev, but the hard-coded
	// iteration is over literal constants — no attacker-controlled input —
	// so we pass a trailing `--` (with no pathspec) purely as a defence-in-
	// depth signal that the ref is positional.
	for _, branch := range []string{"main", "master", "develop"} {
		check := exec.Command("git", "-C", repoPath, "rev-parse", "--verify", branch, "--")
		if check.Run() == nil {
			return branch
		}
	}
	return "main"
}

// GetOrCreateAgentWorktree returns the persistent worktree path for an agent+repo pair,
// creating it if it doesn't exist or was removed from disk.
func GetOrCreateAgentWorktree(db *sql.DB, agentName, repoPath string) (string, error) {
	var worktreePath string
	db.QueryRow(`SELECT worktree_path FROM Agents WHERE agent_name = ? AND repo = ?`,
		agentName, repoPath).Scan(&worktreePath)

	if worktreePath != "" {
		if _, err := os.Stat(worktreePath); err == nil {
			return worktreePath, nil
		}
		// Stale DB entry — prune git's internal records and recreate
		bestEffortRun(exec.Command("git", "-C", repoPath, "worktree", "prune"), "worktree prune (stale entry)")
	}

	// Place worktrees in a sibling directory (.force-worktrees/<repo>/<agent>) so they
	// live outside the repo working tree and never appear in git status.
	worktreeBase := filepath.Join(filepath.Dir(repoPath), ".force-worktrees", filepath.Base(repoPath))
	worktreePath = filepath.Join(worktreeBase, agentName)
	// AUDIT-100 (Fix #8d): 0700 so the worktree tree — and the injected
	// inbox mail captured in astromech Claude output that lands underneath —
	// is operator-private on multi-user hosts.
	if err := os.MkdirAll(worktreeBase, 0700); err != nil {
		return "", fmt.Errorf("failed to create worktree base dir: %w", err)
	}
	bestEffortRun(exec.Command("git", "-C", repoPath, "worktree", "remove", worktreePath, "--force"), "worktree remove before recreate")

	base := GetDefaultBranch(repoPath)
	// `--` before the (path, ref) positional pair. `git worktree add` accepts
	// it and treats everything after as positional (Fix #9).
	out, err := exec.Command("git", "-C", repoPath, "worktree", "add", "--detach", "--", worktreePath, base).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("failed to create agent worktree: %s", strings.TrimSpace(string(out)))
	}

	db.Exec(`INSERT OR REPLACE INTO Agents (agent_name, repo, worktree_path) VALUES (?, ?, ?)`,
		agentName, repoPath, worktreePath)

	return worktreePath, nil
}

// PrepareAgentBranch creates or resumes a task branch in the agent's persistent worktree.
// If existingBranch is non-empty and that branch still exists in the repo, the function
// checks it out so the agent can build on top of its prior work (isResume=true).
// Otherwise a fresh branch is created from the current default branch HEAD (isResume=false).
// Any uncommitted changes in the worktree are forcibly discarded before switching branches.
//
// baseBranch (if non-empty) is used as the fresh-branch base INSTEAD of the repo's
// default branch. Under the PR flow this is the convoy's ask-branch, so astromechs
// branch off the integration branch rather than main. Pass "" to use the default
// branch (legacy path + tasks with no ask-branch).
func PrepareAgentBranch(worktreeDir, repoPath string, taskID int, agentName, existingBranch, baseBranch string) (branchName string, isResume bool, err error) {
	// Nuclear pre-flight: every astromech claim starts from a provably clean
	// worktree. `-fdx` (vs -fd) is critical — `-x` removes gitignored files
	// too, which is where contamination survives between agents (build
	// artifacts, stale .force-worktrees/*, generated files, partial checkouts
	// from a killed prior agent). Without -x, 12 different astromechs can
	// produce identical contaminating diffs because they all inherit the same
	// ignored-but-dirty state. Ran without -x on convoys 35/37 and saw exactly
	// that pattern; adding -x closes the source.
	// `--` separator protects against CVE-2017-1000117-class ref names in the
	// (validated-but-defence-in-depth) HEAD slot (Fix #9).
	// AUDIT-156 (Fix #8d): wrap both pre-use hygiene calls so a transient
	// filesystem EBUSY / stale lock leaves a log line rather than dropping
	// the failure on the floor — the astromech would otherwise proceed on a
	// dirty worktree with no indication the reset didn't actually clean.
	bestEffortRun(exec.Command("git", "-C", worktreeDir, "reset", "--hard", "HEAD", "--"), "pre-use reset --hard")
	bestEffortRun(exec.Command("git", "-C", worktreeDir, "clean", "-fdx"), "pre-use clean -fdx")

	// Resume an existing branch if one was preserved from a prior attempt.
	if existingBranch != "" {
		// Fetch first so origin/<existingBranch> reflects any commits that were
		// pushed (e.g. the agent committed and pushed before being rejected).
		bestEffortRun(exec.Command("git", "-C", repoPath, "fetch", "origin", "--", existingBranch), "fetch existing branch for resume")

		// Try direct checkout. Works when this is the same worktree that created
		// the branch, or when the branch isn't checked out in any worktree.
		verifyOut, verifyErr := exec.Command("git", "-C", repoPath, "rev-parse", "--verify", existingBranch, "--").CombinedOutput()
		if verifyErr == nil && strings.TrimSpace(string(verifyOut)) != "" {
			if _, coErr := exec.Command("git", "-C", worktreeDir, "checkout", existingBranch, "--").CombinedOutput(); coErr == nil {
				return existingBranch, true, nil
			}
		}

		// Direct checkout failed — the branch is checked out in a different
		// persistent worktree (a different agent owns it). Seed a new branch from
		// origin/<existingBranch> so all prior commits are preserved and the
		// resuming agent only needs to apply the rework delta, not redo everything.
		// `--verify` guarantees single-line SHA output (plain rev-parse echoes
		// a spurious `--` on stdout in trailing-`--` form).
		remoteRef := "refs/remotes/origin/" + existingBranch
		if resumeSHAOut, shaErr := exec.Command("git", "-C", repoPath, "rev-parse", "--verify", remoteRef, "--").CombinedOutput(); shaErr == nil {
			if resumeSHA := strings.TrimSpace(string(resumeSHAOut)); resumeSHA != "" {
				newBranch := fmt.Sprintf("%sagent/%s/task-%d", BranchPrefix(), agentName, taskID)
				bestEffortRun(exec.Command("git", "-C", repoPath, "branch", "-D", "--", newBranch), "delete stale resume branch")
				if _, coErr := exec.Command("git", "-C", worktreeDir, "checkout", "-b", newBranch, resumeSHA, "--").CombinedOutput(); coErr == nil {
					return newBranch, true, nil // isResume=true: seeded from origin prior work
				}
			}
		}
		// Neither worked — fall through to fresh branch.
	}

	// Pick the base for the new branch: convoy ask-branch if supplied, else the
	// repo's default branch.
	//
	// Invariant: when a baseBranch is supplied (ask-branch), we ALWAYS fetch
	// and use refs/remotes/origin/<base> — never the local ref. Sub-PRs merge
	// into the ask-branch on origin and the local tracking branch is never
	// updated by the fleet. Using the local ref silently drops prior sibling
	// tasks' work; new agent branches would start pre-merge and clash with
	// already-landed changes when Jedi's sub-PR opens.
	base := baseBranch
	if base == "" {
		base = GetDefaultBranch(repoPath)
	} else {
		// Always fetch — cheap (milliseconds on an up-to-date remote) and
		// ensures origin/<base> reflects any sub-PR merges that happened
		// between this task and the prior sibling.
		bestEffortRun(exec.Command("git", "-C", repoPath, "fetch", "origin", "--", base), "fetch origin/base before branch")
		remoteRef := "refs/remotes/origin/" + base
		if _, verifyErr := exec.Command("git", "-C", repoPath, "rev-parse", "--verify", remoteRef, "--").CombinedOutput(); verifyErr == nil {
			base = remoteRef
		} else {
			// Remote ref is unreachable (ask-branch was deleted, auth broken,
			// etc.). Try the local ref as a fallback, then default branch. This
			// is defensive — in practice Pilot's CreateAskBranch always pushes,
			// so origin has the branch.
			if _, localErr := exec.Command("git", "-C", repoPath, "rev-parse", "--verify", base, "--").CombinedOutput(); localErr != nil {
				base = GetDefaultBranch(repoPath)
			}
		}
	}

	newBranch := fmt.Sprintf("%sagent/%s/task-%d", BranchPrefix(), agentName, taskID)

	if out, coErr := exec.Command("git", "-C", worktreeDir, "checkout", "--detach", base, "--").CombinedOutput(); coErr != nil {
		return "", false, fmt.Errorf("failed to detach to %s: %s", base, strings.TrimSpace(string(out)))
	}

	// Clean up any stale branch from a prior failed attempt.
	bestEffortRun(exec.Command("git", "-C", repoPath, "branch", "-D", "--", newBranch), "delete stale agent branch")

	out, coErr := exec.Command("git", "-C", worktreeDir, "checkout", "-b", newBranch, "--").CombinedOutput()
	if coErr != nil {
		return "", false, fmt.Errorf("failed to create task branch: %s", strings.TrimSpace(string(out)))
	}

	return newBranch, false, nil
}

// GetAgentWorktreePath looks up the persistent worktree path for an agent+repo pair.
func GetAgentWorktreePath(db *sql.DB, agentName, repoPath string) string {
	var path string
	db.QueryRow(`SELECT worktree_path FROM Agents WHERE agent_name = ? AND repo = ?`,
		agentName, repoPath).Scan(&path)
	return path
}

// ResolveWorktreeDir returns the worktree directory for a task, using the agent's
// persistent worktree if the branch name encodes one, or a per-task fallback otherwise.
func ResolveWorktreeDir(db *sql.DB, branchName, repoPath string, taskID int, branchAgentName func(string) string) string {
	if agent := branchAgentName(branchName); agent != "" {
		if p := GetAgentWorktreePath(db, agent, repoPath); p != "" {
			return p
		}
	}
	return filepath.Join(filepath.Dir(repoPath), ".force-worktrees", filepath.Base(repoPath), fmt.Sprintf("task-%d", taskID))
}

// ListAgentWorktreePaths returns every astromech worktree directory that
// exists on disk for the given repo, as absolute paths. Used by the
// WorktreeReset handler to enumerate targets for cleanup when contamination
// is detected. repoName is accepted for interface parity but not required —
// the filesystem layout is keyed only by the repo directory name.
//
// Fix #9: entries are `os.Lstat`-inspected and any os.ModeSymlink match is
// skipped. A malicious symlink under .force-worktrees/<repo>/<agent>
// pointing to e.g. /etc would let the downstream `git clean -fdx` wipe
// arbitrary filesystem locations. Rejection at discovery is the first line
// of defence; resetAndCleanWorktree has a second (containment) check.
func ListAgentWorktreePaths(repoPath, repoName string) []string {
	_ = repoName // reserved for future multi-repo disambiguation
	base := filepath.Join(filepath.Dir(repoPath), ".force-worktrees", filepath.Base(repoPath))
	entries, err := os.ReadDir(base)
	if err != nil {
		return nil
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		// Skip the per-task fallback directories (task-NNN) — those are
		// short-lived and not persistent-agent contamination targets.
		if strings.HasPrefix(e.Name(), "task-") {
			continue
		}
		full := filepath.Join(base, e.Name())
		// Lstat (not Stat) so we see the entry itself, not its target.
		info, lerr := os.Lstat(full)
		if lerr != nil {
			continue
		}
		if info.Mode()&os.ModeSymlink != 0 {
			// Skip symlinked worktree entries — refusing here prevents the
			// downstream `git clean -fdx` call in resetAndCleanWorktree
			// from wiping whatever the symlink points at.
			continue
		}
		out = append(out, full)
	}
	return out
}

// RunCmd runs a git subcommand in repoPath and returns combined output.
func RunCmd(repoPath string, args ...string) (string, error) {
	fullArgs := append([]string{"-C", repoPath}, args...)
	out, err := exec.Command("git", fullArgs...).CombinedOutput()
	return string(out), err
}

// ExtractDiffFiles parses a unified diff and returns a deduplicated, sorted list
// of file paths that were added, modified, or deleted.
func ExtractDiffFiles(diff string) []string {
	seen := map[string]bool{}
	var files []string
	for _, line := range strings.Split(diff, "\n") {
		// Match "diff --git a/path b/path" — take the b/ side (destination)
		if strings.HasPrefix(line, "diff --git ") {
			parts := strings.Fields(line)
			if len(parts) == 4 {
				path := strings.TrimPrefix(parts[3], "b/")
				if !seen[path] {
					seen[path] = true
					files = append(files, path)
				}
			}
		}
	}
	return files
}

// detachWorktreesHoldingBranch scans all worktrees for the repo and force-detaches any
// that have branchName checked out (excluding the calling worktree itself).
// This frees the branch so it can be checked out in a different worktree.
func detachWorktreesHoldingBranch(repoPath, currentWorktreeDir, branchName string) {
	out, err := exec.Command("git", "-C", repoPath, "worktree", "list", "--porcelain").CombinedOutput()
	if err != nil {
		return
	}
	var candidate string
	for _, line := range strings.Split(string(out), "\n") {
		if strings.HasPrefix(line, "worktree ") {
			candidate = strings.TrimPrefix(line, "worktree ")
		} else if line == "branch refs/heads/"+branchName {
			if candidate != "" && filepath.Clean(candidate) != filepath.Clean(currentWorktreeDir) {
				// `--` after the HEAD literal (Fix #9 defence-in-depth).
				bestEffortRun(exec.Command("git", "-C", candidate, "checkout", "--detach", "HEAD", "--"), "detach worktree holding branch")
			}
		}
	}
}

// PrepareConflictBranch sets up the agent worktree to resolve merge conflicts on an
// existing branch. It checks out the conflicting branch and merges the default branch
// into it, intentionally leaving conflict markers in files for Claude to resolve.
// After Claude resolves the markers and commits, the branch can be merged cleanly.
func PrepareConflictBranch(worktreeDir, repoPath, conflictBranch string) error {
	base := GetDefaultBranch(repoPath)

	// Abort any in-progress merge or rebase left over from prior attempts.
	// No ref positional args here; wrapped in abortOp (see below) so the P10
	// shell-boundary grep can't confuse the subcommand names with ref args
	// (Fix #9).
	abortOp(worktreeDir, "merge")
	abortOp(worktreeDir, "rebase")
	bestEffortRun(exec.Command("git", "-C", worktreeDir, "reset", "--hard", "HEAD", "--"), "pre-conflict reset --hard")
	bestEffortRun(exec.Command("git", "-C", worktreeDir, "clean", "-fd"), "pre-conflict clean -fd")

	// Free the branch from any other worktree that may be holding it (e.g. from a prior attempt).
	detachWorktreesHoldingBranch(repoPath, worktreeDir, conflictBranch)

	if out, err := exec.Command("git", "-C", worktreeDir, "checkout", conflictBranch, "--").CombinedOutput(); err != nil {
		return fmt.Errorf("failed to checkout conflict branch %s: %s", conflictBranch, strings.TrimSpace(string(out)))
	}

	// Merge default branch into the conflict branch — leaves conflict markers for Claude.
	// We intentionally ignore the exit code here: a non-zero exit is expected when
	// there are conflicts, and that is exactly the state we want Claude to work in.
	// AUDIT-156: passed through bestEffortRun for uniform-style logging; the
	// failure mode is expected, so the log line is informational.
	bestEffortRun(exec.Command("git", "-C", worktreeDir, "merge", "--", base), "conflict-seed merge (expected to conflict)")

	return nil
}

func GetDiff(repoPath string, branchName string) string {
	base := GetDefaultBranch(repoPath)
	// Three-dot diff: shows only what branchName introduced since it diverged from base.
	// Two-dot diff would also include reversals of any commits merged into base after
	// the branch was created, making the diff misleading for review and conflict resolution.
	// Trailing `--` guards the rev positional slot (Fix #9).
	cmd := exec.Command("git", "-C", repoPath, "diff", base+"..."+branchName, "--")
	out, _ := cmd.CombinedOutput()
	return string(out)
}

// GetDiffFromBase returns the three-dot diff between `baseRef` and `branch` —
// only what `branch` has added since it diverged from `baseRef`. Reviewers
// must use this (not GetDiff) when the astromech branch was cut from an
// ask-branch rather than main, otherwise the diff includes every commit
// main has made since the ask-branch was created (as "phantom additions"
// from the ask-branch's perspective). Those phantom files look like
// out-of-scope changes to the Captain and like missing work to the
// ConvoyReview, producing the scope-violation avalanche we observed on
// convoys 35 and 37.
//
// baseRef can be any ref understood by git — a SHA, "origin/main",
// "origin/force/ask-37-...", etc. branch is resolved normally.
func GetDiffFromBase(repoPath, baseRef, branch string) string {
	if baseRef == "" || branch == "" {
		return ""
	}
	// Trailing `--` keeps the rev strictly positional (Fix #9).
	cmd := exec.Command("git", "-C", repoPath, "diff", baseRef+"..."+branch, "--")
	out, _ := cmd.CombinedOutput()
	return string(out)
}

// CommitsAheadOf returns the one-line log of commits unique to `branch`
// against `baseRef`. Mirrors CommitsAhead but with an explicit base for
// use by reviewers that need to check "does this astromech branch have
// any net-new work relative to the ask-branch." Empty = no unique commits.
func CommitsAheadOf(repoPath, baseRef, branch string) string {
	if baseRef == "" || branch == "" {
		return ""
	}
	cmd := exec.Command("git", "-C", repoPath, "log", "--oneline", baseRef+".."+branch, "--")
	out, _ := cmd.CombinedOutput()
	return strings.TrimSpace(string(out))
}

// CommitsAhead returns the one-line log of commits on branchName that are not
// yet in the default branch (git log base..branch --oneline). An empty string
// means the branch has no unique commits — its work is already merged into base.
func CommitsAhead(repoPath string, branchName string) string {
	base := GetDefaultBranch(repoPath)
	cmd := exec.Command("git", "-C", repoPath, "log", "--oneline", base+".."+branchName, "--")
	out, _ := cmd.CombinedOutput()
	return strings.TrimSpace(string(out))
}

// MergeAndCleanup merges the branch into the default branch of the repo, then resets
// the agent worktree to detached HEAD. Serialized with a mutex to prevent concurrent
// council members from racing on the same main worktree. Returns error if merge fails.
func MergeAndCleanup(repoPath string, branchName string, worktreeDir string) error {
	if err := AssertNotDefaultBranch(repoPath, branchName); err != nil {
		return fmt.Errorf("MergeAndCleanup refused: %w", err)
	}
	mergeMu.Lock()
	defer mergeMu.Unlock()

	base := GetDefaultBranch(repoPath)

	// Stash any uncommitted changes in the main worktree so checkout succeeds
	// even when the operator has made manual edits (e.g. live debugging).
	stashed := false
	if statusOut, err := exec.Command("git", "-C", repoPath, "status", "--porcelain").Output(); err == nil && len(strings.TrimSpace(string(statusOut))) > 0 {
		if _, err := exec.Command("git", "-C", repoPath, "stash", "--include-untracked").Output(); err == nil {
			stashed = true
		}
	}

	// Ensure the main worktree is on the default branch before merging.
	// Trailing `--` keeps the branch ref strictly positional (Fix #9).
	if out, err := exec.Command("git", "-C", repoPath, "checkout", base, "--").CombinedOutput(); err != nil {
		if stashed {
			bestEffortRun(exec.Command("git", "-C", repoPath, "stash", "pop"), "stash pop after failed checkout")
		}
		return fmt.Errorf("checkout %s failed: %s", base, strings.TrimSpace(string(out)))
	}

	out, err := exec.Command("git", "-C", repoPath, "merge", "--no-ff", "--", branchName).CombinedOutput()
	if err != nil {
		bestEffortRun(exec.Command("git", "-C", repoPath, "merge", "--abort"), "merge --abort after failed merge")
		if stashed {
			bestEffortRun(exec.Command("git", "-C", repoPath, "stash", "pop"), "stash pop after failed merge")
		}
		return fmt.Errorf("merge failed: %s", strings.TrimSpace(string(out)))
	}

	// Restore any stashed operator changes on top of the merge result.
	// If pop conflicts with the merge, discard the stash — the merge is the
	// authoritative result and the operator's edits are likely now superseded.
	if stashed {
		if _, popErr := exec.Command("git", "-C", repoPath, "stash", "pop").Output(); popErr != nil {
			bestEffortRun(exec.Command("git", "-C", repoPath, "checkout", "--", "."), "checkout . after stash-pop conflict")
			bestEffortRun(exec.Command("git", "-C", repoPath, "stash", "drop"), "stash drop after pop conflict")
		}
	}

	// Reset agent worktree to a clean detached HEAD — ready for the next task.
	// Force-discard any changes so the worktree is pristine. Trailing `--`
	// on every ref-taking call (Fix #9).
	bestEffortRun(exec.Command("git", "-C", worktreeDir, "reset", "--hard", "HEAD", "--"), "post-merge worktree reset")
	bestEffortRun(exec.Command("git", "-C", worktreeDir, "clean", "-fd"), "post-merge worktree clean")
	bestEffortRun(exec.Command("git", "-C", worktreeDir, "checkout", "--detach", base, "--"), "post-merge detach worktree")
	bestEffortRun(exec.Command("git", "-C", repoPath, "branch", "-D", "--", branchName), "post-merge branch delete")
	return nil
}
