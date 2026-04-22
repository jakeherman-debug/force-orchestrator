package agents

import (
	"database/sql"
	"encoding/json"
	"fmt"

	igit "force-orchestrator/internal/git"
	"force-orchestrator/internal/store"
)

// ── Pilot — RebaseAgentBranch ─────────────────────────────────────────────────
//
// Queued by sub-pr-ci-watch when a sub-PR's mergeStateStatus=BEHIND, meaning
// another sub-PR merged into the ask-branch after this one was opened and the
// agent's branch now lags the ask-branch tip.
//
// Pilot rebases the agent branch onto the current tip of the ask-branch and
// force-pushes. GitHub auto-updates the open sub-PR's diff. On conflict,
// a RebaseConflict CodeEdit task is spawned so an astromech can resolve it;
// Pilot's job ends there.

type rebaseAgentPayload struct {
	SubPRRowID int    `json:"sub_pr_row_id"`
	TaskID     int    `json:"task_id"`
	Branch     string `json:"branch"`
	AskBranch  string `json:"ask_branch"`
	ConvoyID   int    `json:"convoy_id"`
	Repo       string `json:"repo"`
}

// QueueRebaseAgentBranch enqueues a Pilot task to rebase an agent branch onto
// its convoy's ask-branch. Idempotent: returns existing task ID if a Pending or
// Locked RebaseAgentBranch task for the same sub_pr_row_id already exists.
func QueueRebaseAgentBranch(db *sql.DB, p rebaseAgentPayload) (int, error) {
	if p.Branch == "" || p.AskBranch == "" || p.Repo == "" {
		return 0, fmt.Errorf("QueueRebaseAgentBranch: branch, ask_branch, and repo required")
	}

	// Dedup: only one outstanding rebase per sub-PR row.
	var existing int
	db.QueryRow(`SELECT COUNT(*) FROM BountyBoard
		WHERE type = 'RebaseAgentBranch' AND status IN ('Pending', 'Locked')
		  AND (payload LIKE '%"sub_pr_row_id":' || ? || ',%'
		    OR payload LIKE '%"sub_pr_row_id":' || ? || '}%')`,
		p.SubPRRowID, p.SubPRRowID).Scan(&existing)
	if existing > 0 {
		return 0, nil // already queued
	}

	// Also dedup if there's an active REBASE_CONFLICT resolution task for the
	// same agent branch. RebaseAgentBranch marks itself Completed after spawning
	// the child, so the check above misses the escalated-child case and lets
	// sub-pr-ci-watch spawn duplicate rebase chains. Block until the conflict
	// task is resolved (Completed or Cancelled) — if it escalated, the operator
	// must dismiss it before the system retries.
	if p.Branch != "" {
		var existingConflict int
		db.QueryRow(`SELECT COUNT(*) FROM BountyBoard
			WHERE status NOT IN ('Completed', 'Cancelled')
			  AND branch_name = ?
			  AND payload LIKE '%[REBASE_CONFLICT for task #%'`,
			p.Branch).Scan(&existingConflict)
		if existingConflict > 0 {
			return 0, nil // conflict resolution still in flight or escalated
		}

		// Rebase loop cap: if too many REBASE_CONFLICT tasks have been spawned
		// for this branch (including cancelled ones), the loop is stuck. Return
		// an error so queueAgentBranchRebase can escalate the task instead of
		// silently no-op-ing while the loop continues.
		const maxRebaseConflictTasks = 5
		var totalConflicts int
		db.QueryRow(`SELECT COUNT(*) FROM BountyBoard
			WHERE payload LIKE '[REBASE_CONFLICT for task #' || ? || ' %'`,
			p.TaskID).Scan(&totalConflicts)
		if totalConflicts >= maxRebaseConflictTasks {
			return 0, fmt.Errorf("rebase loop cap: %d REBASE_CONFLICT tasks already spawned for task %d — manual resolution needed",
				totalConflicts, p.TaskID)
		}
	}

	payload, _ := json.Marshal(p)
	res, err := db.Exec(
		`INSERT INTO BountyBoard (parent_id, target_repo, type, status, payload, priority, created_at)
		 VALUES (0, ?, 'RebaseAgentBranch', 'Pending', ?, 4, datetime('now'))`,
		p.Repo, string(payload))
	if err != nil {
		return 0, err
	}
	id, _ := res.LastInsertId()
	return int(id), nil
}

func runRebaseAgentBranch(db *sql.DB, bounty *store.Bounty, logger interface{ Printf(string, ...any) }) {
	var p rebaseAgentPayload
	if err := json.Unmarshal([]byte(bounty.Payload), &p); err != nil {
		store.FailBounty(db, bounty.ID, fmt.Sprintf("invalid payload: %v", err))
		return
	}

	repo := store.GetRepo(db, p.Repo)
	if repo == nil || repo.LocalPath == "" {
		store.FailBounty(db, bounty.ID, fmt.Sprintf("repo %s not found", p.Repo))
		return
	}

	newTip, rebaseErr := igit.RebaseBranchOnto(repo.LocalPath, p.Branch, p.AskBranch)
	if rebaseErr != nil {
		// Rebase conflict — spawn a CodeEdit task for the astromech to resolve,
		// then complete Pilot's task (resolution is the astromech's job).
		logger.Printf("RebaseAgentBranch #%d: conflict — spawning RebaseConflict task for branch %s", bounty.ID, p.Branch)
		conflictPayload := fmt.Sprintf(
			"[REBASE_CONFLICT for task #%d repo %s]\n\nAgent branch: %s\nAsk-branch: %s\n\nThe rebase of the agent branch onto the ask-branch conflicted. Merge %s into %s, resolve conflict markers, and commit. The branch will be force-pushed after council review.",
			p.TaskID, p.Repo, p.Branch, p.AskBranch, p.AskBranch, p.Branch,
		)
		conflictTaskID, _ := store.AddConvoyTask(db, bounty.ID, p.Repo, conflictPayload, p.ConvoyID, 5, "Pending")
		store.SetBranchName(db, conflictTaskID, p.Branch)
		store.SendMail(db, "Pilot", "astromech",
			fmt.Sprintf("[REBASE CONFLICT] Task #%d — resolve and commit on %s", conflictTaskID, p.Branch),
			fmt.Sprintf("Rebase of %s onto ask-branch %s conflicted.\n\nResolve conflict markers on %s and commit. Council review will approve and the branch will be force-pushed.\n\nError:\n%v",
				p.Branch, p.AskBranch, p.Branch, rebaseErr),
			conflictTaskID, store.MailTypeFeedback)
		store.UpdateBountyStatus(db, bounty.ID, "Completed")
		return
	}

	// Clean rebase — force-push the agent branch so the open sub-PR auto-updates.
	if pushErr := igit.ForcePushBranch(repo.LocalPath, p.Branch); pushErr != nil {
		store.FailBounty(db, bounty.ID, fmt.Sprintf("force-push %s failed: %v", p.Branch, pushErr))
		return
	}

	logger.Printf("RebaseAgentBranch #%d: rebased %s onto ask-branch %s, new tip %s",
		bounty.ID, p.Branch, p.AskBranch, newTip[:minInt(8, len(newTip))])
	store.UpdateBountyStatus(db, bounty.ID, "Completed")
}
