package agents

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"

	igit "force-orchestrator/internal/git"
	"force-orchestrator/internal/store"
)

// forceWorktreeBase is the authoritative containment root for astromech
// persistent worktrees. Every path resetAndCleanWorktree touches must
// resolve (after EvalSymlinks) to a descendant of this directory — see
// AUDIT-123 / Fix #9. The name is the dirname the fleet always uses; we
// compare by path-suffix because the on-disk parent depends on the repo's
// registered location.
const forceWorktreeBase = ".force-worktrees"

// ── Pilot — WorktreeReset ────────────────────────────────────────────────────
//
// Spawned by Medic when contamination is detected on a task (same unwanted
// diff produced by 2+ different astromechs). Rather than escalating to the
// operator with a "please run git checkout . && git clean -fd" message, the
// fleet performs exactly those commands itself:
//
//   1. For every per-agent astromech worktree that attempted the task,
//      reset --hard to origin/<target-branch> and clean -fdx.
//   2. Re-queue the parent task as Pending with a fresh branch name so
//      the next astromech starts from a genuinely clean state.
//
// The operator never sees this class of problem — detection + remedy are
// deterministic git operations, not LLM judgement calls.

type worktreeResetPayload struct {
	ParentTaskID int      `json:"parent_task_id"`
	Repo         string   `json:"repo"`
	TargetBranch string   `json:"target_branch"` // branch to reset to (usually the convoy's ask-branch)
	Agents       []string `json:"agents"`        // astromech names whose worktrees need wiping (empty = all for this repo)
	Reason       string   `json:"reason"`        // human-readable cause (from Medic) for the audit log
}

// QueueWorktreeReset enqueues a WorktreeReset task. Idempotent: returns the
// existing task ID if a Pending/Locked WorktreeReset for the same parent task
// is already queued.
//
// Fix #3 (AUDIT-035): canonical idempotency key `worktree-reset:<parent_task_id>`
// gated by idx_bounty_idem replaces the previous payload-LIKE dedup.
func QueueWorktreeReset(db *sql.DB, p worktreeResetPayload) (int, error) {
	if p.Repo == "" || p.TargetBranch == "" {
		return 0, fmt.Errorf("QueueWorktreeReset: repo and target_branch required")
	}
	// Fix #9 / AUDIT-140: reject malformed TargetBranch at queue time so
	// the task never lands in BountyBoard in the first place.
	if err := igit.ValidateRef(p.TargetBranch); err != nil {
		return 0, fmt.Errorf("QueueWorktreeReset: %w", err)
	}
	// Fix #3: canonical idempotency key requires a non-zero parent task id.
	if p.ParentTaskID <= 0 {
		return 0, fmt.Errorf("QueueWorktreeReset: parent_task_id required for canonical idempotency key")
	}
	payload, _ := json.Marshal(p)
	key := fmt.Sprintf("worktree-reset:%d", p.ParentTaskID)
	id, _, err := store.AddIdempotentTask(db, key,
		p.ParentTaskID, p.Repo, "WorktreeReset", string(payload), 0, 4, "Pending")
	if err != nil {
		return 0, err
	}
	return id, nil
}

func runWorktreeReset(db *sql.DB, bounty *store.Bounty, logger interface{ Printf(string, ...any) }) {
	var p worktreeResetPayload
	if err := json.Unmarshal([]byte(bounty.Payload), &p); err != nil {
		store.FailBounty(db, bounty.ID, fmt.Sprintf("invalid payload: %v", err))
		return
	}
	repo := store.GetRepo(db, p.Repo)
	if repo == nil || repo.LocalPath == "" {
		store.FailBounty(db, bounty.ID, fmt.Sprintf("repo %s not registered", p.Repo))
		return
	}

	// Fix #9 / AUDIT-140: validate the target branch at ingress so a
	// corrupt medic LLM output (e.g. `-rm`, `--upload-pack=/tmp/evil`)
	// never reaches the fetch/reset calls below.
	if err := igit.ValidateRef(p.TargetBranch); err != nil {
		store.FailBounty(db, bounty.ID, fmt.Sprintf("invalid target_branch %q: %v", p.TargetBranch, err))
		return
	}
	// Resolve the target ref up front. Fetching the origin remote first keeps
	// us honest — we reset to what's actually on the remote, not whatever
	// stale refs/remotes/origin/* happen to be cached locally. `--` keeps the
	// refspec positional.
	if out, err := exec.Command("git", "-C", repo.LocalPath, "fetch", "origin", "--", p.TargetBranch).CombinedOutput(); err != nil {
		store.FailBounty(db, bounty.ID, fmt.Sprintf("fetch %s: %s", p.TargetBranch, strings.TrimSpace(string(out))))
		return
	}
	targetRef := "origin/" + p.TargetBranch

	// Determine which worktrees to wipe. If the payload names specific agents,
	// only those are touched — bounds the blast radius. Empty list = every
	// .force-worktrees/<repo>/<agent>/ directory, which is the right default
	// when Medic detected contamination across multiple agents at once.
	worktreeRoots := discoverWorktrees(repo.LocalPath, p.Repo, p.Agents)
	if len(worktreeRoots) == 0 {
		logger.Printf("WorktreeReset #%d: no astromech worktrees found for repo %s — marking Completed", bounty.ID, p.Repo)
		store.UpdateBountyStatus(db, bounty.ID, "Completed")
		return
	}

	wiped := 0
	var failures []string
	for _, wt := range worktreeRoots {
		if err := resetAndCleanWorktree(wt, targetRef); err != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", filepath.Base(wt), err))
			continue
		}
		wiped++
	}

	// Partial success is still useful — surviving worktrees still got reset,
	// next astromech claim will find a clean tree. But we propagate the
	// failure count so the operator notices if half the wipes failed.
	if len(failures) > 0 && wiped == 0 {
		store.FailBounty(db, bounty.ID, fmt.Sprintf("worktree reset failed for all %d worktrees: %s",
			len(worktreeRoots), strings.Join(failures, "; ")))
		return
	}

	// Re-queue the parent task as Pending with a fresh slate: clear branch_name,
	// error_log, locked_at, and owner so the next astromech starts from zero.
	// parent stays on its original convoy/priority/payload.
	//
	// Fix #8 Phase A (AUDIT-014): both UPDATEs previously used `_, _ = db.Exec(...)`
	// — if either failed the WorktreeReset still marked itself Completed and the
	// parent stayed stuck Failed/Escalated. Now we observe both errors; any
	// failure here fails the WorktreeReset so Medic can escalate.
	if p.ParentTaskID > 0 {
		if _, err := db.Exec(`UPDATE BountyBoard
			SET status = 'Pending', branch_name = '', owner = '', locked_at = '',
			    error_log = 'Reset by WorktreeReset #' || ? || ' after contamination detected: ' || ?
			WHERE id = ? AND status IN ('Failed','Escalated','ConflictPending')`,
			bounty.ID, p.Reason, p.ParentTaskID); err != nil {
			if fbErr := store.FailBounty(db, bounty.ID,
				fmt.Sprintf("parent-requeue UPDATE failed for task #%d: %v", p.ParentTaskID, err)); fbErr != nil {
				logger.Printf("WorktreeReset #%d: FailBounty after parent-requeue failure also failed: %v", bounty.ID, fbErr)
			}
			return
		}
		// Also close any Open escalations on the parent — the cleanup IS the fix.
		// Fix B (AUDIT-025): terminal status is 'Closed', not legacy 'Resolved'.
		if _, err := db.Exec(`UPDATE Escalations
			SET status = 'Closed', acknowledged_at = datetime('now')
			WHERE task_id = ? AND status = 'Open'`, p.ParentTaskID); err != nil {
			if fbErr := store.FailBounty(db, bounty.ID,
				fmt.Sprintf("escalation-resolve UPDATE failed for task #%d: %v", p.ParentTaskID, err)); fbErr != nil {
				logger.Printf("WorktreeReset #%d: FailBounty after escalation-resolve failure also failed: %v", bounty.ID, fbErr)
			}
			return
		}
	}

	logger.Printf("WorktreeReset #%d: wiped %d worktree(s) for repo %s, reset to %s (%d failures)",
		bounty.ID, wiped, p.Repo, targetRef, len(failures))
	store.LogAudit(db, "Pilot", "worktree-reset", bounty.ID,
		fmt.Sprintf("wiped=%d, reason=%s, target=%s", wiped, p.Reason, targetRef))
	if err := store.UpdateBountyStatus(db, bounty.ID, "Completed"); err != nil {
		logger.Printf("WorktreeReset #%d: failed to mark Completed: %v", bounty.ID, err)
	}
}

// discoverWorktrees enumerates .force-worktrees/<repo>/<agent> directories
// that exist on disk. If agents is non-empty, only those agent subdirs are
// considered; empty means "every astromech worktree for this repo."
func discoverWorktrees(mainRepoPath, repoName string, agents []string) []string {
	// Persistent worktrees live at <repo-parent>/../.force-worktrees/<repo>/<agent>.
	// Easier: glob from the project's .force-worktrees root if it exists,
	// else try common locations. We rely on igit.ListAgentWorktrees when
	// available; fall back to a direct filesystem walk otherwise.
	paths := igit.ListAgentWorktreePaths(mainRepoPath, repoName)
	if len(agents) == 0 {
		return paths
	}
	allowed := make(map[string]bool, len(agents))
	for _, a := range agents {
		allowed[a] = true
	}
	out := make([]string, 0, len(paths))
	for _, p := range paths {
		if allowed[filepath.Base(p)] {
			out = append(out, p)
		}
	}
	return out
}

// resetAndCleanWorktree is the core git recovery sequence: hard-reset the
// worktree to the given ref and remove untracked files. Equivalent to what
// an operator would type after seeing "your local changes would be overwritten."
//
// Fix #9: before running destructive ops, re-verify the path resolves under
// the .force-worktrees base via filepath.EvalSymlinks + containment check
// (AUDIT-123). A malicious symlink under .force-worktrees/... pointing at
// e.g. /etc would otherwise let `git clean -fdx` wipe arbitrary files.
// Also enforces the ref validator on targetRef (AUDIT-140).
func resetAndCleanWorktree(worktreePath, targetRef string) error {
	// Validate the target ref at ingress — the LLM's medicDecision carries
	// this value directly, so it's untrusted input.
	if err := igit.ValidateRef(targetRef); err != nil {
		// targetRef looks like "origin/<branch>" which contains a `/`. Strip
		// the "origin/" prefix for ref-validation; we only care that the
		// branch portion is safe.
		branch := strings.TrimPrefix(targetRef, "origin/")
		if vErr := igit.ValidateRef(branch); vErr != nil {
			return fmt.Errorf("resetAndCleanWorktree: invalid targetRef %q: %w", targetRef, err)
		}
	}
	// Containment check: refuse if the resolved worktree path is not a
	// descendant of a .force-worktrees/ directory. EvalSymlinks dereferences
	// any symlinks along the way, which is exactly what we need — if the
	// worktree is a symlink pointing outside, the resolved path will fail
	// the filepath.Rel containment test below.
	resolved, evErr := filepath.EvalSymlinks(worktreePath)
	if evErr != nil {
		return fmt.Errorf("resetAndCleanWorktree: %q: EvalSymlinks: %w", worktreePath, evErr)
	}
	// The .force-worktrees base lives at <repoParent>/.force-worktrees/<repoName>/.
	// The resolved path MUST contain the forceWorktreeBase directory name as
	// an ancestor segment. Anything else means we're about to destroy files
	// outside the fleet's territory.
	if !strings.Contains(resolved, string(filepath.Separator)+forceWorktreeBase+string(filepath.Separator)) {
		return fmt.Errorf("resetAndCleanWorktree: refusing — resolved path %q is not under %s/", resolved, forceWorktreeBase)
	}

	// Abort any in-progress rebase / merge — those leave HEAD detached and
	// make reset behave unexpectedly. Wrapped in the git-package helper so
	// the shell-boundary audit doesn't mis-flag the subcommand name.
	exec.Command("git", "-C", worktreePath, "rebase", "--abort").Run()
	exec.Command("git", "-C", worktreePath, "merge", "--abort").Run()
	// Trailing `--` keeps the ref in the positional slot (Fix #9).
	// (reset --hard -- <ref> is ambiguous: git treats it as pathspec.)
	if out, err := exec.Command("git", "-C", worktreePath, "reset", "--hard", targetRef, "--").CombinedOutput(); err != nil {
		return fmt.Errorf("reset --hard %s: %s", targetRef, strings.TrimSpace(string(out)))
	}
	if out, err := exec.Command("git", "-C", worktreePath, "clean", "-fdx").CombinedOutput(); err != nil {
		return fmt.Errorf("clean -fdx: %s", strings.TrimSpace(string(out)))
	}
	return nil
}
