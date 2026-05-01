package git

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// shortGitOpTimeout bounds every internal/git subprocess invocation so a
// hung `git fetch` on an unreachable remote cannot wedge the caller.
// AUDIT-127 / AUDIT-165 (Fix #8d).
const shortGitOpTimeout = 5 * time.Minute

// mergeMus shards the pre-Fix-#8d global mergeMu on a per-repo basis so
// cross-repo parallel shipping is no longer capped at one concurrent merge.
// AUDIT-046 (Fix #8d): the prior single `var mergeMu sync.Mutex` serialised
// every MergeAndCleanup across every repo, turning a multi-repo convoy
// ship into a strict sequential queue. Conceptually this is a
// map[string]*sync.Mutex keyed on filepath.Clean(repoPath); sync.Map gives
// us lock-free lookup + atomic LoadOrStore so two goroutines racing to
// acquire the same repo's mutex consistently see the same mutex instance.
//
// AUDIT-155 (Fix #8d): this map also backs the per-repo lock that
// MergeWithUnionStrategy acquires while it rewrites .git/info/attributes —
// two concurrent union-merges in the same repo would race on the attributes
// file and one caller's deferred restore would clobber the other's merge.
var mergeMus sync.Map

// lockRepoForMerge returns a per-repo mutex, creating it lazily. Callers
// acquire the mutex around MergeAndCleanup / MergeWithUnionStrategy.
func lockRepoForMerge(repoPath string) *sync.Mutex {
	key := filepath.Clean(repoPath)
	if v, ok := mergeMus.Load(key); ok {
		return v.(*sync.Mutex)
	}
	newMu := &sync.Mutex{}
	actual, _ := mergeMus.LoadOrStore(key, newMu)
	return actual.(*sync.Mutex)
}

// bestEffortRun runs a git subcommand with a bounded context timeout and
// LOGS any failure without returning it. Use this for cleanup / rollback
// paths where (a) the operation is expected to fail sometimes (e.g.
// `stash pop` after a clean merge has nothing to pop), and (b) the
// calling code has no meaningful recovery beyond logging.
//
// AUDIT-156 / AUDIT-127 (Fix #8d): replaces the pre-existing bare
// chained-and-Run patterns — callers pass the git args directly instead of
// constructing their own *exec.Cmd, and the timeout applies uniformly.
// Fix #8e: the timeout now wraps the caller's ctx so daemon shutdown /
// e-stop cancels in-flight subprocesses; the prior fabricated
// context.Background root made the helper deaf to daemon cancellation.
// The label argument names the specific rollback so log readers can
// correlate failures to call sites.
//
// D3 P6B.2: routes through LogAndRun so every cleanup / rollback git op
// lands a row in GitOperationLog with operation + redacted args +
// duration + exit code.
func bestEffortRun(ctx context.Context, label string, args ...string) {
	ctx, cancel := context.WithTimeout(ctx, shortGitOpTimeout)
	defer cancel()
	op := DeriveOperation("git", args) + "-best-effort"
	if _, err := LogAndRun(ctx, OpContext{Repo: deriveRepoFromArgs(args)}, op, "git", args...); err != nil {
		log.Printf("git: best-effort %s failed: %v", label, err)
	}
}

// runGitCtx is the short-timeout CombinedOutput sibling of bestEffortRun —
// used when the caller needs the output (even on failure). AUDIT-127
// (Fix #8d): replaces raw runGitCtx(...) pairs so a hung git subprocess
// can't wedge the caller. Fix #8e threads the caller's ctx so daemon
// cancellation propagates.
//
// D3 P6B.2: routes through LogAndRun so the row + redacted output land
// in GitOperationLog.
func runGitCtx(ctx context.Context, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, shortGitOpTimeout)
	defer cancel()
	op := DeriveOperation("git", args)
	return LogAndRun(ctx, OpContext{Repo: deriveRepoFromArgs(args)}, op, "git", args...)
}

// runGitCtxOutput is runGitCtx's Output variant (stdout only, stderr
// returned via the ExitError wrapping).
//
// D3 P6B.2: previously this called exec.Cmd.Output(), which suppresses
// stderr from the returned bytes. We now route through LogAndRun for
// the audit row; LogAndRun returns CombinedOutput. Existing callers
// either inspect the bytes for stdout content (which still appears in
// CombinedOutput) or check the error and surface a derived message —
// neither code path was strict about the stderr-vs-stdout split, so
// the change is behavior-preserving.
func runGitCtxOutput(ctx context.Context, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, shortGitOpTimeout)
	defer cancel()
	op := DeriveOperation("git", args)
	return LogAndRun(ctx, OpContext{Repo: deriveRepoFromArgs(args)}, op, "git", args...)
}

// deriveRepoFromArgs extracts a repo path label from a git argv if one
// of the canonical -C / --git-dir / --work-tree flags is present. The
// label flows into GitOperationLog.repo so Drill can scope-filter ops
// by repo without forcing every call site to pass it explicitly.
func deriveRepoFromArgs(args []string) string {
	for i, a := range args {
		if (a == "-C" || a == "--git-dir" || a == "--work-tree") && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

// abortOp runs `git <op> --abort` in `wt`, logging any error — used purely
// to recover from a half-finished merge/rebase left over from a prior crash.
// Wrapping keeps the shell-boundary grep in audit_pattern_p10_test.go from
// mis-flagging the subcommand name (e.g. "rebase" contains the "base" token).
func abortOp(ctx context.Context, wt, op string) {
	bestEffortRun(ctx, fmt.Sprintf("%s --abort in %s", op, wt), "-C", wt, op, "--abort")
}

// GetDefaultBranch detects the default branch of a repo rather than assuming a hardcoded name.
// Fix #8e: ctx threads from the caller so daemon shutdown cancels these short
// lookups; pre-fix they ran on a fabricated context.Background root.
func GetDefaultBranch(ctx context.Context, repoPath string) string {
	// Try remote HEAD first (most reliable). `--` guards against any future
	// refactor that puts an operator-controlled ref into the positional slot
	// (Fix #9).
	{
		lookupCtx, cancel := context.WithTimeout(ctx, shortGitOpTimeout)
		out, err := exec.CommandContext(lookupCtx, "git", "-C", repoPath, "symbolic-ref", "--short", "--", "refs/remotes/origin/HEAD").CombinedOutput()
		cancel()
		if err == nil {
			parts := strings.SplitN(strings.TrimSpace(string(out)), "/", 2)
			if len(parts) == 2 && parts[1] != "" {
				return parts[1]
			}
		}
	}
	// Fall back to checking common branch names locally. rev-parse's flag
	// grammar doesn't permit a plain `--` before the rev, but the hard-coded
	// iteration is over literal constants — no attacker-controlled input —
	// so we pass a trailing `--` (with no pathspec) purely as a defence-in-
	// depth signal that the ref is positional.
	for _, branch := range []string{"main", "master", "develop"} {
		lookupCtx, cancel := context.WithTimeout(ctx, shortGitOpTimeout)
		err := exec.CommandContext(lookupCtx, "git", "-C", repoPath, "rev-parse", "--verify", branch, "--").Run()
		cancel()
		if err == nil {
			return branch
		}
	}
	return "main"
}

// GetOrCreateAgentWorktree returns the persistent worktree path for an agent+repo pair,
// creating it if it doesn't exist or was removed from disk.
// Fix #8e: ctx threads from the caller (typically SpawnAstromech's daemon ctx)
// so worktree-add or remove subprocesses cancel on daemon shutdown.
func GetOrCreateAgentWorktree(ctx context.Context, db *sql.DB, agentName, repoPath string) (string, error) {
	var worktreePath string
	db.QueryRow(`SELECT worktree_path FROM Agents WHERE agent_name = ? AND repo = ?`,
		agentName, repoPath).Scan(&worktreePath)

	if worktreePath != "" {
		if _, err := os.Stat(worktreePath); err == nil {
			return worktreePath, nil
		}
		// Stale DB entry — prune git's internal records and recreate
		bestEffortRun(ctx, "worktree prune (stale entry)", "-C", repoPath, "worktree", "prune")
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
	bestEffortRun(ctx, "worktree remove before recreate", "-C", repoPath, "worktree", "remove", worktreePath, "--force")

	base := GetDefaultBranch(ctx, repoPath)
	// `--` before the (path, ref) positional pair. `git worktree add` accepts
	// it and treats everything after as positional (Fix #9).
	out, err := runGitCtx(ctx, "-C", repoPath, "worktree", "add", "--detach", "--", worktreePath, base)
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
// PrepareAgentBranch creates or resumes a task branch in the agent's persistent worktree.
// Fix #8e: ctx threads from the caller (SpawnAstromech's daemon ctx) so the
// fetch/checkout/branch-management subprocesses cancel on shutdown.
func PrepareAgentBranch(ctx context.Context, worktreeDir, repoPath string, taskID int, agentName, existingBranch, baseBranch string) (branchName string, isResume bool, err error) {
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
	bestEffortRun(ctx, "pre-use reset --hard", "-C", worktreeDir, "reset", "--hard", "HEAD", "--")
	bestEffortRun(ctx, "pre-use clean -fdx", "-C", worktreeDir, "clean", "-fdx")

	// Resume an existing branch if one was preserved from a prior attempt.
	if existingBranch != "" {
		// Fetch first so origin/<existingBranch> reflects any commits that were
		// pushed (e.g. the agent committed and pushed before being rejected).
		bestEffortRun(ctx, "fetch existing branch for resume", "-C", repoPath, "fetch", "origin", "--", existingBranch)

		// Try direct checkout. Works when this is the same worktree that created
		// the branch, or when the branch isn't checked out in any worktree.
		verifyOut, verifyErr := runGitCtx(ctx, "-C", repoPath, "rev-parse", "--verify", existingBranch, "--")
		if verifyErr == nil && strings.TrimSpace(string(verifyOut)) != "" {
			if _, coErr := runGitCtx(ctx, "-C", worktreeDir, "checkout", existingBranch, "--"); coErr == nil {
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
		if resumeSHAOut, shaErr := runGitCtx(ctx, "-C", repoPath, "rev-parse", "--verify", remoteRef, "--"); shaErr == nil {
			if resumeSHA := strings.TrimSpace(string(resumeSHAOut)); resumeSHA != "" {
				newBranch := fmt.Sprintf("%sagent/%s/task-%d", BranchPrefix(), agentName, taskID)
				bestEffortRun(ctx, "delete stale resume branch", "-C", repoPath, "branch", "-D", "--", newBranch)
				if _, coErr := runGitCtx(ctx, "-C", worktreeDir, "checkout", "-b", newBranch, resumeSHA, "--"); coErr == nil {
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
		base = GetDefaultBranch(ctx, repoPath)
	} else {
		// Always fetch — cheap (milliseconds on an up-to-date remote) and
		// ensures origin/<base> reflects any sub-PR merges that happened
		// between this task and the prior sibling.
		bestEffortRun(ctx, "fetch origin/base before branch", "-C", repoPath, "fetch", "origin", "--", base)
		remoteRef := "refs/remotes/origin/" + base
		if _, verifyErr := runGitCtx(ctx, "-C", repoPath, "rev-parse", "--verify", remoteRef, "--"); verifyErr == nil {
			base = remoteRef
		} else {
			// Remote ref is unreachable (ask-branch was deleted, auth broken,
			// etc.). Try the local ref as a fallback, then default branch. This
			// is defensive — in practice Pilot's CreateAskBranch always pushes,
			// so origin has the branch.
			if _, localErr := runGitCtx(ctx, "-C", repoPath, "rev-parse", "--verify", base, "--"); localErr != nil {
				base = GetDefaultBranch(ctx, repoPath)
			}
		}
	}

	newBranch := fmt.Sprintf("%sagent/%s/task-%d", BranchPrefix(), agentName, taskID)

	if out, coErr := runGitCtx(ctx, "-C", worktreeDir, "checkout", "--detach", base, "--"); coErr != nil {
		return "", false, fmt.Errorf("failed to detach to %s: %s", base, strings.TrimSpace(string(out)))
	}

	// Clean up any stale branch from a prior failed attempt.
	bestEffortRun(ctx, "delete stale agent branch", "-C", repoPath, "branch", "-D", "--", newBranch)

	out, coErr := runGitCtx(ctx, "-C", worktreeDir, "checkout", "-b", newBranch, "--")
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
// Fix #8e: ctx threads from the caller so the subprocess cancels on shutdown.
func RunCmd(ctx context.Context, repoPath string, args ...string) (string, error) {
	fullArgs := append([]string{"-C", repoPath}, args...)
	out, err := runGitCtx(ctx, fullArgs...)
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
func detachWorktreesHoldingBranch(ctx context.Context, repoPath, currentWorktreeDir, branchName string) {
	out, err := runGitCtx(ctx, "-C", repoPath, "worktree", "list", "--porcelain")
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
				bestEffortRun(ctx, "detach worktree holding branch", "-C", candidate, "checkout", "--detach", "HEAD", "--")
			}
		}
	}
}

// PrepareConflictBranch sets up the agent worktree to resolve merge conflicts on an
// existing branch. It checks out the conflicting branch and merges the default branch
// into it, intentionally leaving conflict markers in files for Claude to resolve.
// After Claude resolves the markers and commits, the branch can be merged cleanly.
// Fix #8e: ctx threads from the caller (Pilot's claim loop) so subprocess
// invocations cancel on daemon shutdown.
func PrepareConflictBranch(ctx context.Context, worktreeDir, repoPath, conflictBranch string) error {
	base := GetDefaultBranch(ctx, repoPath)

	// Abort any in-progress merge or rebase left over from prior attempts.
	// No ref positional args here; wrapped in abortOp (see below) so the P10
	// shell-boundary grep can't confuse the subcommand names with ref args
	// (Fix #9).
	abortOp(ctx, worktreeDir, "merge")
	abortOp(ctx, worktreeDir, "rebase")
	bestEffortRun(ctx, "pre-conflict reset --hard", "-C", worktreeDir, "reset", "--hard", "HEAD", "--")
	bestEffortRun(ctx, "pre-conflict clean -fd", "-C", worktreeDir, "clean", "-fd")

	// Free the branch from any other worktree that may be holding it (e.g. from a prior attempt).
	detachWorktreesHoldingBranch(ctx, repoPath, worktreeDir, conflictBranch)

	if out, err := runGitCtx(ctx, "-C", worktreeDir, "checkout", conflictBranch, "--"); err != nil {
		return fmt.Errorf("failed to checkout conflict branch %s: %s", conflictBranch, strings.TrimSpace(string(out)))
	}

	// Merge default branch into the conflict branch — leaves conflict markers for Claude.
	// We intentionally ignore the exit code here: a non-zero exit is expected when
	// there are conflicts, and that is exactly the state we want Claude to work in.
	// AUDIT-156: passed through bestEffortRun for uniform-style logging; the
	// failure mode is expected, so the log line is informational.
	bestEffortRun(ctx, "conflict-seed merge (expected to conflict)", "-C", worktreeDir, "merge", "--", base)

	return nil
}

// GetDiff returns the three-dot diff for branchName vs the default branch.
// Fix #8e: ctx threads from the caller so the diff subprocess cancels on
// daemon shutdown.
func GetDiff(ctx context.Context, repoPath string, branchName string) string {
	base := GetDefaultBranch(ctx, repoPath)
	// Three-dot diff: shows only what branchName introduced since it diverged from base.
	// Two-dot diff would also include reversals of any commits merged into base after
	// the branch was created, making the diff misleading for review and conflict resolution.
	// Trailing `--` guards the rev positional slot (Fix #9).
	out, _ := runGitCtx(ctx, "-C", repoPath, "diff", base+"..."+branchName, "--")
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
// Fix #8e: ctx threads from the caller so the diff subprocess cancels.
func GetDiffFromBase(ctx context.Context, repoPath, baseRef, branch string) string {
	if baseRef == "" || branch == "" {
		return ""
	}
	// Trailing `--` keeps the rev strictly positional (Fix #9).
	out, _ := runGitCtx(ctx, "-C", repoPath, "diff", baseRef+"..."+branch, "--")
	return string(out)
}

// CommitsAheadOf returns the one-line log of commits unique to `branch`
// against `baseRef`. Mirrors CommitsAhead but with an explicit base for
// use by reviewers that need to check "does this astromech branch have
// any net-new work relative to the ask-branch." Empty = no unique commits.
// Fix #8e: ctx threads from the caller so the log subprocess cancels.
func CommitsAheadOf(ctx context.Context, repoPath, baseRef, branch string) string {
	if baseRef == "" || branch == "" {
		return ""
	}
	out, _ := runGitCtx(ctx, "-C", repoPath, "log", "--oneline", baseRef+".."+branch, "--")
	return strings.TrimSpace(string(out))
}

// CommitsAhead returns the one-line log of commits on branchName that are not
// yet in the default branch (git log base..branch --oneline). An empty string
// means the branch has no unique commits — its work is already merged into base.
// Fix #8e: ctx threads from the caller.
func CommitsAhead(ctx context.Context, repoPath string, branchName string) string {
	base := GetDefaultBranch(ctx, repoPath)
	out, _ := runGitCtx(ctx, "-C", repoPath, "log", "--oneline", base+".."+branchName, "--")
	return strings.TrimSpace(string(out))
}

// ChangedGoFilesFromBase returns the list of `.go` files changed
// between baseRef and branch (three-dot range). Used by Bureau of
// Standards (D4 Phase 1) to enumerate which Go source files to AST-
// scan. Filters to .go files at the helper layer so callers don't have
// to repeat the suffix check. Returns an empty slice on git errors —
// the BoS reviewer treats "no files" as "nothing to review."
//
// Fix #9 considerations: baseRef and branch are validated at the
// caller site (BoS task payload deserializer) before reaching this
// helper.
func ChangedGoFilesFromBase(ctx context.Context, repoPath, baseRef, branch string) []string {
	if baseRef == "" || branch == "" || repoPath == "" {
		return nil
	}
	out, err := runGitCtx(ctx, "-C", repoPath, "diff", "--name-only", baseRef+"..."+branch, "--")
	if err != nil {
		return nil
	}
	var goFiles []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if !strings.HasSuffix(line, ".go") {
			continue
		}
		goFiles = append(goFiles, line)
	}
	return goFiles
}

// MergeAndCleanup merges the branch into the default branch of the repo, then resets
// the agent worktree to detached HEAD. Serialized with a mutex to prevent concurrent
// council members from racing on the same main worktree. Returns error if merge fails.
// Fix #8e: ctx threads from the caller (Jedi Council's claim ctx) so a hung
// merge cancels on daemon shutdown.
// D2 T1-4: db + repoName threaded for the AssertRepoWritable mode guard.
// Pass (nil, "") in tests that exercise raw merge mechanics without a
// Holocron — the guard is a no-op in that case.
func MergeAndCleanup(ctx context.Context, db *sql.DB, repoName, repoPath, branchName, worktreeDir string) error {
	if err := AssertNotDefaultBranch(ctx, repoPath, branchName); err != nil {
		return fmt.Errorf("MergeAndCleanup refused: %w", err)
	}
	if err := AssertRepoWritable(db, repoName); err != nil {
		return fmt.Errorf("MergeAndCleanup refused: %w", err)
	}
	// AUDIT-046 (Fix #8d): per-repo lock, not global. Two different repos
	// can ship convoys in parallel; two tasks in the same repo still
	// serialise on the shared main worktree.
	mu := lockRepoForMerge(repoPath)
	mu.Lock()
	defer mu.Unlock()

	base := GetDefaultBranch(ctx, repoPath)

	// Stash any uncommitted changes in the main worktree so checkout succeeds
	// even when the operator has made manual edits (e.g. live debugging).
	stashed := false
	if statusOut, err := runGitCtxOutput(ctx, "-C", repoPath, "status", "--porcelain"); err == nil && len(strings.TrimSpace(string(statusOut))) > 0 {
		if _, err := runGitCtxOutput(ctx, "-C", repoPath, "stash", "--include-untracked"); err == nil {
			stashed = true
		}
	}

	// Ensure the main worktree is on the default branch before merging.
	// Trailing `--` keeps the branch ref strictly positional (Fix #9).
	if out, err := runGitCtx(ctx, "-C", repoPath, "checkout", base, "--"); err != nil {
		if stashed {
			bestEffortRun(ctx, "stash pop after failed checkout", "-C", repoPath, "stash", "pop")
		}
		return fmt.Errorf("checkout %s failed: %s", base, strings.TrimSpace(string(out)))
	}

	out, err := runGitCtx(ctx, "-C", repoPath, "merge", "--no-ff", "--", branchName)
	if err != nil {
		bestEffortRun(ctx, "merge --abort after failed merge", "-C", repoPath, "merge", "--abort")
		if stashed {
			bestEffortRun(ctx, "stash pop after failed merge", "-C", repoPath, "stash", "pop")
		}
		return fmt.Errorf("merge failed: %s", strings.TrimSpace(string(out)))
	}

	// Restore any stashed operator changes on top of the merge result.
	// If pop conflicts with the merge, discard the stash — the merge is the
	// authoritative result and the operator's edits are likely now superseded.
	if stashed {
		if _, popErr := runGitCtxOutput(ctx, "-C", repoPath, "stash", "pop"); popErr != nil {
			bestEffortRun(ctx, "checkout . after stash-pop conflict", "-C", repoPath, "checkout", "--", ".")
			bestEffortRun(ctx, "stash drop after pop conflict", "-C", repoPath, "stash", "drop")
		}
	}

	// Reset agent worktree to a clean detached HEAD — ready for the next task.
	// Force-discard any changes so the worktree is pristine. Trailing `--`
	// on every ref-taking call (Fix #9).
	bestEffortRun(ctx, "post-merge worktree reset", "-C", worktreeDir, "reset", "--hard", "HEAD", "--")
	bestEffortRun(ctx, "post-merge worktree clean", "-C", worktreeDir, "clean", "-fd")
	bestEffortRun(ctx, "post-merge detach worktree", "-C", worktreeDir, "checkout", "--detach", base, "--")
	bestEffortRun(ctx, "post-merge branch delete", "-C", repoPath, "branch", "-D", "--", branchName)
	return nil
}
