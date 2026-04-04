package git

import (
	"database/sql"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
)

// mergeMu serializes merge operations across goroutines.
// Prevents concurrent council members from racing on the same git main worktree.
var mergeMu sync.Mutex

// GetDefaultBranch detects the default branch of a repo rather than assuming a hardcoded name.
func GetDefaultBranch(repoPath string) string {
	// Try remote HEAD first (most reliable)
	cmd := exec.Command("git", "-C", repoPath, "symbolic-ref", "refs/remotes/origin/HEAD", "--short")
	if out, err := cmd.CombinedOutput(); err == nil {
		parts := strings.SplitN(strings.TrimSpace(string(out)), "/", 2)
		if len(parts) == 2 && parts[1] != "" {
			return parts[1]
		}
	}
	// Fall back to checking common branch names locally
	for _, branch := range []string{"main", "master", "develop"} {
		check := exec.Command("git", "-C", repoPath, "rev-parse", "--verify", branch)
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
		exec.Command("git", "-C", repoPath, "worktree", "prune").Run()
	}

	// Place worktrees in a sibling directory (.force-worktrees/<repo>/<agent>) so they
	// live outside the repo working tree and never appear in git status.
	worktreeBase := filepath.Join(filepath.Dir(repoPath), ".force-worktrees", filepath.Base(repoPath))
	worktreePath = filepath.Join(worktreeBase, agentName)
	os.MkdirAll(worktreeBase, 0755)
	exec.Command("git", "-C", repoPath, "worktree", "remove", worktreePath, "--force").Run()

	base := GetDefaultBranch(repoPath)
	out, err := exec.Command("git", "-C", repoPath, "worktree", "add", "--detach", worktreePath, base).CombinedOutput()
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
func PrepareAgentBranch(worktreeDir, repoPath string, taskID int, agentName, existingBranch string) (branchName string, isResume bool, err error) {
	// Force-discard any uncommitted changes before switching branches.
	exec.Command("git", "-C", worktreeDir, "reset", "--hard", "HEAD").Run()
	exec.Command("git", "-C", worktreeDir, "clean", "-fd").Run()

	// Resume an existing branch if one was preserved from a prior attempt.
	if existingBranch != "" {
		verifyOut, verifyErr := exec.Command("git", "-C", repoPath, "rev-parse", "--verify", existingBranch).CombinedOutput()
		if verifyErr == nil && strings.TrimSpace(string(verifyOut)) != "" {
			if _, coErr := exec.Command("git", "-C", worktreeDir, "checkout", existingBranch).CombinedOutput(); coErr == nil {
				return existingBranch, true, nil
			}
			// Checkout failed (e.g. worktree conflict) — fall through to fresh branch.
		}
	}

	// Fresh branch from current main.
	base := GetDefaultBranch(repoPath)
	newBranch := fmt.Sprintf("agent/%s/task-%d", agentName, taskID)

	if out, coErr := exec.Command("git", "-C", worktreeDir, "checkout", "--detach", base).CombinedOutput(); coErr != nil {
		return "", false, fmt.Errorf("failed to detach to %s: %s", base, strings.TrimSpace(string(out)))
	}

	// Clean up any stale branch from a prior failed attempt.
	exec.Command("git", "-C", repoPath, "branch", "-D", newBranch).Run()

	out, coErr := exec.Command("git", "-C", worktreeDir, "checkout", "-b", newBranch).CombinedOutput()
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

// PrepareConflictBranch sets up the agent worktree to resolve merge conflicts on an
// existing branch. It checks out the conflicting branch and merges the default branch
// into it, intentionally leaving conflict markers in files for Claude to resolve.
// After Claude resolves the markers and commits, the branch can be merged cleanly.
func PrepareConflictBranch(worktreeDir, repoPath, conflictBranch string) error {
	base := GetDefaultBranch(repoPath)

	// Abort any in-progress merge or rebase left over from prior attempts
	exec.Command("git", "-C", worktreeDir, "merge", "--abort").Run()
	exec.Command("git", "-C", worktreeDir, "rebase", "--abort").Run()
	exec.Command("git", "-C", worktreeDir, "reset", "--hard", "HEAD").Run()
	exec.Command("git", "-C", worktreeDir, "clean", "-fd").Run()

	if out, err := exec.Command("git", "-C", worktreeDir, "checkout", conflictBranch).CombinedOutput(); err != nil {
		return fmt.Errorf("failed to checkout conflict branch %s: %s", conflictBranch, strings.TrimSpace(string(out)))
	}

	// Merge default branch into the conflict branch — leaves conflict markers for Claude.
	// We intentionally ignore the exit code here: a non-zero exit is expected when
	// there are conflicts, and that is exactly the state we want Claude to work in.
	exec.Command("git", "-C", worktreeDir, "merge", base).Run()

	return nil
}

func GetDiff(repoPath string, branchName string) string {
	base := GetDefaultBranch(repoPath)
	// Three-dot diff: shows only what branchName introduced since it diverged from base.
	// Two-dot diff would also include reversals of any commits merged into base after
	// the branch was created, making the diff misleading for review and conflict resolution.
	cmd := exec.Command("git", "-C", repoPath, "diff", base+"..."+branchName)
	out, _ := cmd.CombinedOutput()
	return string(out)
}

// MergeAndCleanup merges the branch into the default branch of the repo, then resets
// the agent worktree to detached HEAD. Serialized with a mutex to prevent concurrent
// council members from racing on the same main worktree. Returns error if merge fails.
func MergeAndCleanup(repoPath string, branchName string, worktreeDir string) error {
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
	if out, err := exec.Command("git", "-C", repoPath, "checkout", base).CombinedOutput(); err != nil {
		if stashed {
			exec.Command("git", "-C", repoPath, "stash", "pop").Run()
		}
		return fmt.Errorf("checkout %s failed: %s", base, strings.TrimSpace(string(out)))
	}

	out, err := exec.Command("git", "-C", repoPath, "merge", "--no-ff", branchName).CombinedOutput()
	if err != nil {
		exec.Command("git", "-C", repoPath, "merge", "--abort").Run()
		if stashed {
			exec.Command("git", "-C", repoPath, "stash", "pop").Run()
		}
		return fmt.Errorf("merge failed: %s", strings.TrimSpace(string(out)))
	}

	// Restore any stashed operator changes on top of the merge result.
	// If pop conflicts with the merge, discard the stash — the merge is the
	// authoritative result and the operator's edits are likely now superseded.
	if stashed {
		if _, popErr := exec.Command("git", "-C", repoPath, "stash", "pop").Output(); popErr != nil {
			exec.Command("git", "-C", repoPath, "checkout", "--", ".").Run()
			exec.Command("git", "-C", repoPath, "stash", "drop").Run()
		}
	}

	// Reset agent worktree to a clean detached HEAD — ready for the next task.
	// Force-discard any changes so the worktree is pristine.
	exec.Command("git", "-C", worktreeDir, "reset", "--hard", "HEAD").Run()
	exec.Command("git", "-C", worktreeDir, "clean", "-fd").Run()
	exec.Command("git", "-C", worktreeDir, "checkout", "--detach", base).Run()
	exec.Command("git", "-C", repoPath, "branch", "-D", branchName).Run()
	return nil
}
