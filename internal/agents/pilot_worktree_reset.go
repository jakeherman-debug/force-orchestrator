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
func QueueWorktreeReset(db *sql.DB, p worktreeResetPayload) (int, error) {
	if p.Repo == "" || p.TargetBranch == "" {
		return 0, fmt.Errorf("QueueWorktreeReset: repo and target_branch required")
	}
	var existing int
	db.QueryRow(`SELECT id FROM BountyBoard
		WHERE type = 'WorktreeReset' AND status IN ('Pending','Locked')
		  AND (payload LIKE '%"parent_task_id":' || ? || ',%'
		    OR payload LIKE '%"parent_task_id":' || ? || '}%')`,
		p.ParentTaskID, p.ParentTaskID).Scan(&existing)
	if existing > 0 {
		return existing, nil
	}
	payload, _ := json.Marshal(p)
	res, err := db.Exec(
		`INSERT INTO BountyBoard (parent_id, target_repo, type, status, payload, priority, created_at)
		 VALUES (?, ?, 'WorktreeReset', 'Pending', ?, 4, datetime('now'))`,
		p.ParentTaskID, p.Repo, string(payload))
	if err != nil {
		return 0, err
	}
	id, _ := res.LastInsertId()
	return int(id), nil
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

	// Resolve the target ref up front. Fetching the origin remote first keeps
	// us honest — we reset to what's actually on the remote, not whatever
	// stale refs/remotes/origin/* happen to be cached locally.
	if out, err := exec.Command("git", "-C", repo.LocalPath, "fetch", "origin", p.TargetBranch).CombinedOutput(); err != nil {
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
	if p.ParentTaskID > 0 {
		_, _ = db.Exec(`UPDATE BountyBoard
			SET status = 'Pending', branch_name = '', owner = '', locked_at = '',
			    error_log = 'Reset by WorktreeReset #' || ? || ' after contamination detected: ' || ?
			WHERE id = ? AND status IN ('Failed','Escalated','ConflictPending')`,
			bounty.ID, p.Reason, p.ParentTaskID)
		// Also resolve any Open escalations on the parent — the cleanup IS the fix.
		_, _ = db.Exec(`UPDATE Escalations
			SET status = 'Resolved', acknowledged_at = datetime('now')
			WHERE task_id = ? AND status = 'Open'`, p.ParentTaskID)
	}

	logger.Printf("WorktreeReset #%d: wiped %d worktree(s) for repo %s, reset to %s (%d failures)",
		bounty.ID, wiped, p.Repo, targetRef, len(failures))
	store.LogAudit(db, "Pilot", "worktree-reset", bounty.ID,
		fmt.Sprintf("wiped=%d, reason=%s, target=%s", wiped, p.Reason, targetRef))
	store.UpdateBountyStatus(db, bounty.ID, "Completed")
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
func resetAndCleanWorktree(worktreePath, targetRef string) error {
	// Abort any in-progress rebase / merge — those leave HEAD detached and
	// make reset behave unexpectedly.
	exec.Command("git", "-C", worktreePath, "rebase", "--abort").Run()
	exec.Command("git", "-C", worktreePath, "merge", "--abort").Run()
	if out, err := exec.Command("git", "-C", worktreePath, "reset", "--hard", targetRef).CombinedOutput(); err != nil {
		return fmt.Errorf("reset --hard %s: %s", targetRef, strings.TrimSpace(string(out)))
	}
	if out, err := exec.Command("git", "-C", worktreePath, "clean", "-fdx").CombinedOutput(); err != nil {
		return fmt.Errorf("clean -fdx: %s", strings.TrimSpace(string(out)))
	}
	return nil
}
