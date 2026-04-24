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
//
// Fix #3 (AUDIT-035): canonical idempotency key `rebase-agent:<sub_pr_row_id>`
// gated by idx_bounty_idem replaces the previous payload-LIKE dedup. The
// REBASE_CONFLICT sibling-task check still uses branch_name lookup (that's
// the column's primary purpose) and stays on its own gate below.
func QueueRebaseAgentBranch(db *sql.DB, p rebaseAgentPayload) (int, error) {
	if p.Branch == "" || p.AskBranch == "" || p.Repo == "" {
		return 0, fmt.Errorf("QueueRebaseAgentBranch: branch, ask_branch, and repo required")
	}
	if p.SubPRRowID <= 0 {
		return 0, fmt.Errorf("QueueRebaseAgentBranch: sub_pr_row_id required for canonical idempotency key")
	}

	// Also dedup if there's an active REBASE_CONFLICT resolution task for the
	// same agent branch. RebaseAgentBranch marks itself Completed after spawning
	// the child, so the key-based dedup above misses the escalated-child case
	// and lets sub-pr-ci-watch spawn duplicate rebase chains. Block until the
	// conflict task is resolved (Completed or Cancelled) — if it escalated, the
	// operator must dismiss it before the system retries.
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
	key := fmt.Sprintf("rebase-agent:%d", p.SubPRRowID)
	id, existed, err := store.AddIdempotentTask(db, key,
		0, p.Repo, "RebaseAgentBranch", string(payload), p.ConvoyID, 4, "Pending")
	if err != nil {
		return 0, err
	}
	if existed {
		return 0, nil // already queued under this canonical key
	}
	return id, nil
}

func runRebaseAgentBranch(db *sql.DB, bounty *store.Bounty, logger interface{ Printf(string, ...any) }) {
	var p rebaseAgentPayload
	if err := json.Unmarshal([]byte(bounty.Payload), &p); err != nil {
		if fbErr := store.FailBounty(db, bounty.ID, fmt.Sprintf("invalid payload: %v", err)); fbErr != nil {
			logger.Printf("RebaseAgentBranch #%d: FailBounty after invalid payload failed: %v — stale-lock detector will recover", bounty.ID, fbErr)
		}
		return
	}

	repo := store.GetRepo(db, p.Repo)
	if repo == nil || repo.LocalPath == "" {
		if fbErr := store.FailBounty(db, bounty.ID, fmt.Sprintf("repo %s not found", p.Repo)); fbErr != nil {
			logger.Printf("RebaseAgentBranch #%d: FailBounty after repo-not-found failed: %v — stale-lock detector will recover", bounty.ID, fbErr)
		}
		return
	}

	newTip, rebaseErr := igit.RebaseBranchOnto(repo.LocalPath, p.Branch, p.AskBranch)
	if rebaseErr != nil {
		// Rebase conflict — spawn (or reuse) a CodeEdit task for the astromech to
		// resolve, then complete Pilot's task (resolution is the astromech's job).
		// Idempotency key dedups on the agent branch: two concurrent rebase
		// attempts can't produce two conflict tasks for the same branch.
		idKey := "rebase-conflict:branch:" + p.Branch
		conflictPayload := fmt.Sprintf(
			"[REBASE_CONFLICT for task #%d repo %s]\n\nAgent branch: %s\nAsk-branch: %s\n\nThe rebase of the agent branch onto the ask-branch conflicted. Merge %s into %s, resolve conflict markers, and commit. The branch will be force-pushed after council review.",
			p.TaskID, p.Repo, p.Branch, p.AskBranch, p.AskBranch, p.Branch,
		)
		conflictTaskID, existed, addErr := store.AddConvoyTaskIdempotent(db, idKey, bounty.ID, p.Repo, conflictPayload, p.ConvoyID, 5, "Pending")
		if addErr != nil {
			if fbErr := store.FailBounty(db, bounty.ID, fmt.Sprintf("queue rebase-conflict task: %v", addErr)); fbErr != nil {
				logger.Printf("RebaseAgentBranch #%d: FailBounty after conflict-queue failure failed: %v — stale-lock detector will recover", bounty.ID, fbErr)
			}
			return
		}
		if existed {
			logger.Printf("RebaseAgentBranch #%d: conflict — reusing existing task #%d for branch %s", bounty.ID, conflictTaskID, p.Branch)
			if err := store.UpdateBountyStatus(db, bounty.ID, "Completed"); err != nil {
				logger.Printf("RebaseAgentBranch #%d: failed to mark Completed after reusing conflict task #%d: %v — stale-lock detector will recover", bounty.ID, conflictTaskID, err)
			}
			return
		}
		logger.Printf("RebaseAgentBranch #%d: conflict — spawned task #%d for branch %s", bounty.ID, conflictTaskID, p.Branch)
		store.SetBranchName(db, conflictTaskID, p.Branch)
		store.SendMail(db, "Pilot", "astromech",
			fmt.Sprintf("[REBASE CONFLICT] Task #%d — resolve and commit on %s", conflictTaskID, p.Branch),
			fmt.Sprintf("Rebase of %s onto ask-branch %s conflicted.\n\nResolve conflict markers on %s and commit. Council review will approve and the branch will be force-pushed.\n\nError:\n%v",
				p.Branch, p.AskBranch, p.Branch, rebaseErr),
			conflictTaskID, store.MailTypeFeedback)
		if err := store.UpdateBountyStatus(db, bounty.ID, "Completed"); err != nil {
			logger.Printf("RebaseAgentBranch #%d: failed to mark Completed after spawning conflict task #%d: %v — stale-lock detector will recover", bounty.ID, conflictTaskID, err)
		}
		return
	}

	// Clean rebase — force-push the agent branch so the open sub-PR auto-updates.
	if pushErr := igit.ForcePushBranch(repo.LocalPath, p.Branch); pushErr != nil {
		if fbErr := store.FailBounty(db, bounty.ID, fmt.Sprintf("force-push %s failed: %v", p.Branch, pushErr)); fbErr != nil {
			logger.Printf("RebaseAgentBranch #%d: FailBounty after force-push failure failed: %v — stale-lock detector will recover", bounty.ID, fbErr)
		}
		return
	}

	logger.Printf("RebaseAgentBranch #%d: rebased %s onto ask-branch %s, new tip %s",
		bounty.ID, p.Branch, p.AskBranch, newTip[:minInt(8, len(newTip))])
	if err := store.UpdateBountyStatus(db, bounty.ID, "Completed"); err != nil {
		logger.Printf("RebaseAgentBranch #%d: failed to mark Completed: %v — stale-lock detector will recover", bounty.ID, err)
	}
}
