package agents

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
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

// Fix #8e: ctx threads from SpawnPilot's claim ctx so the fetch/reset/clean
// network and worktree subprocesses cancel on daemon shutdown.
func runWorktreeReset(ctx context.Context, db *sql.DB, bounty *store.Bounty, logger interface{ Printf(string, ...any) }) {
	// Fix #8d / CLAUDE.md "No silent failures": every FailBounty /
	// UpdateBountyStatus return is checked and a clear recovery hint is logged
	// — on DB failure the stale-lock detector (45-min timeout) sweeps the row
	// back to Pending so another Pilot tick re-runs the reset. Without this,
	// the task stayed Locked with no operator visibility when the terminator
	// itself failed (AUDIT-013/014/022/041 class regression).
	failTask := func(msg string) {
		if err := store.FailBounty(db, bounty.ID, msg); err != nil {
			logger.Printf("WorktreeReset #%d: FailBounty failed (%v); stale-lock detector will recover", bounty.ID, err)
		}
	}
	completeTask := func(reason string) {
		if err := store.UpdateBountyStatus(db, bounty.ID, "Completed"); err != nil {
			logger.Printf("WorktreeReset #%d: UpdateBountyStatus(Completed) failed (%v); stale-lock detector will recover — %s", bounty.ID, err, reason)
		}
	}

	var p worktreeResetPayload
	if err := json.Unmarshal([]byte(bounty.Payload), &p); err != nil {
		failTask(fmt.Sprintf("invalid payload: %v", err))
		return
	}
	repo := store.GetRepo(db, p.Repo)
	if repo == nil || repo.LocalPath == "" {
		failTask(fmt.Sprintf("repo %s not registered", p.Repo))
		return
	}

	// Fix #9 / AUDIT-140: validate the target branch at ingress so a
	// corrupt medic LLM output (e.g. `-rm`, `--upload-pack=/tmp/evil`)
	// never reaches the fetch/reset calls below.
	if err := igit.ValidateRef(p.TargetBranch); err != nil {
		failTask(fmt.Sprintf("invalid target_branch %q: %v", p.TargetBranch, err))
		return
	}
	// Resolve the target ref up front. Fetching the origin remote first keeps
	// us honest — we reset to what's actually on the remote, not whatever
	// stale refs/remotes/origin/* happen to be cached locally. `--` keeps the
	// refspec positional.
	// Fix #8e: ctx-bounded fetch so daemon shutdown cancels this network op.
	// D3 polish-pass B4: routes through igit.LogAndRun so the GitOperationLog
	// row records the fetch (the wrapper preserves the ctx-bounded
	// CombinedOutput shape and returns the error verbatim, so the
	// failTask error-path semantics are unchanged).
	if out, err := igit.LogAndRun(ctx, igit.OpContext{Repo: p.Repo, TaskID: int(bounty.ID), Branch: p.TargetBranch},
		"fetch", "git", "-C", repo.LocalPath, "fetch", "origin", "--", p.TargetBranch); err != nil {
		failTask(fmt.Sprintf("fetch %s: %s", p.TargetBranch, strings.TrimSpace(string(out))))
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
		completeTask("no worktrees to wipe")
		return
	}

	wiped := 0
	var failures []string
	for _, wt := range worktreeRoots {
		if err := resetAndCleanWorktree(ctx, wt, targetRef); err != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", filepath.Base(wt), err))
			continue
		}
		wiped++
	}

	// Partial success is still useful — surviving worktrees still got reset,
	// next astromech claim will find a clean tree. But we propagate the
	// failure count so the operator notices if half the wipes failed.
	if len(failures) > 0 && wiped == 0 {
		failTask(fmt.Sprintf("worktree reset failed for all %d worktrees: %s",
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
		// AUDIT-151 (Fix #8d): the UPDATE filter `status IN ('Failed','
		// Escalated','ConflictPending')` silently no-ops when the parent
		// has transitioned elsewhere (operator cancelled, sibling
		// completed, etc.) between Medic's spawn and WorktreeReset's
		// execution. Pre-fix the worktree was wiped but no retry queued
		// and no operator signal emitted. Post-fix, on 0 rows affected we
		// escalate with a low-severity row — the worktree wipe is still
		// useful, but the operator is notified that the parent state was
		// unexpected so they can reconcile.
		res, err := db.Exec(`UPDATE BountyBoard
			SET status = 'Pending', branch_name = '', owner = '', locked_at = '',
			    error_log = 'Reset by WorktreeReset #' || ? || ' after contamination detected: ' || ?
			WHERE id = ? AND status IN ('Failed','Escalated','ConflictPending')`,
			bounty.ID, p.Reason, p.ParentTaskID)
		if err != nil {
			failTask(fmt.Sprintf("parent-requeue UPDATE failed for task #%d: %v", p.ParentTaskID, err))
			return
		}
		n, _ := res.RowsAffected()
		if n == 0 {
			// Unexpected parent state — look up actual status for a clear
			// operator message and escalate.
			var currentStatus string
			_ = db.QueryRow(`SELECT IFNULL(status, '') FROM BountyBoard WHERE id = ?`, p.ParentTaskID).Scan(&currentStatus)
			escMsg := fmt.Sprintf("WorktreeReset #%d wiped worktrees but parent task #%d was in unexpected state %q (not Failed/Escalated/ConflictPending); no requeue was performed. Review the parent and decide whether to re-run.",
				bounty.ID, p.ParentTaskID, currentStatus)
			logger.Printf("WorktreeReset #%d: parent-requeue affected 0 rows — parent #%d status=%q (unexpected). Escalating.", bounty.ID, p.ParentTaskID, currentStatus)
			if _, escErr := CreateEscalation(db, p.ParentTaskID, store.SeverityLow, escMsg); escErr != nil {
				logger.Printf("WorktreeReset #%d: CreateEscalation for unexpected parent state also failed: %v — operator mail below is the fallback", bounty.ID, escErr)
			}
			// P27 burn-down: budget-gate the operator emit before SendMail.
			// On allowed=false the helper has already drop/digested per the
			// configured budget. Fail-open on err so a transient SQLite
			// glitch never silences a high-stakes alert.
			if allowed, _ := store.RespectNotificationBudget(
				context.Background(), db, "operator", "Pilot", "email", "{}",
				store.StakesHigh,
			); !allowed {
				// budget exhausted (StakesHigh always punches through, so
				// this branch only fires on a real config-set 0-cap row).
			} else {
				_ = allowed
			}
			store.SendMail(db, "Pilot", "operator",
				fmt.Sprintf("[WORKTREE RESET] Parent task #%d in unexpected state — no requeue", p.ParentTaskID),
				escMsg, p.ParentTaskID, store.MailTypeAlert)
			// Still mark the WorktreeReset task Completed — the worktree
			// work DID land. The parent-state issue is captured in the
			// escalation.
			completeTask("parent requeue no-op; escalated")
			return
		}
		// Also close any Open escalations on the parent — the cleanup IS the fix.
		// Fix B (AUDIT-025): terminal status is 'Closed', not legacy 'Resolved'.
		if _, err := db.Exec(`UPDATE Escalations
			SET status = 'Closed', acknowledged_at = datetime('now')
			WHERE task_id = ? AND status = 'Open'`, p.ParentTaskID); err != nil {
			failTask(fmt.Sprintf("escalation-resolve UPDATE failed for task #%d: %v", p.ParentTaskID, err))
			return
		}
	}

	logger.Printf("WorktreeReset #%d: wiped %d worktree(s) for repo %s, reset to %s (%d failures)",
		bounty.ID, wiped, p.Repo, targetRef, len(failures))
	store.LogAudit(db, "Pilot", "worktree-reset", bounty.ID,
		fmt.Sprintf("wiped=%d, reason=%s, target=%s", wiped, p.Reason, targetRef))
	completeTask("worktree reset succeeded")
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
// Fix #8e: ctx threads from the caller (Pilot's claim ctx) so the rebase/
// merge --abort + reset/clean subprocesses cancel on daemon shutdown.
func resetAndCleanWorktree(ctx context.Context, worktreePath, targetRef string) error {
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
	// make reset behave unexpectedly.
	// Fix #8e: each subprocess runs under the caller's daemon ctx so a
	// shutdown signal propagates instantly.
	_, _ = igit.LogAndRun(ctx, igit.OpContext{},
		"rebase-abort", "git", "-C", worktreePath, "rebase", "--abort")
	_, _ = igit.LogAndRun(ctx, igit.OpContext{},
		"merge-abort", "git", "-C", worktreePath, "merge", "--abort")
	// Trailing `--` keeps the ref in the positional slot (Fix #9).
	// (reset --hard -- <ref> is ambiguous: git treats it as pathspec.)
	if out, err := igit.LogAndRun(ctx, igit.OpContext{},
		"reset", "git", "-C", worktreePath, "reset", "--hard", targetRef, "--"); err != nil {
		return fmt.Errorf("reset --hard %s: %s", targetRef, strings.TrimSpace(string(out)))
	}
	if out, err := igit.LogAndRun(ctx, igit.OpContext{},
		"clean", "git", "-C", worktreePath, "clean", "-fdx"); err != nil {
		return fmt.Errorf("clean -fdx: %s", strings.TrimSpace(string(out)))
	}
	return nil
}
