package agents

import (
	"database/sql"

	"force-orchestrator/internal/store"
)

// dogEscalationSweeper auto-resolves Open escalations whose stated problem
// no longer exists in the fleet. Two disjoint rules fire:
//
//  1. **Task succeeded / was cancelled** — the task transitioned to Completed
//     or Cancelled after the escalation was filed. The work landed by some
//     other path (Medic auto-complete on empty diff, WorktreeReset re-queue,
//     operator ResetTask, successor ConvoyReview fix). The referenced task is
//     no longer stuck, so the escalation is moot regardless of its original
//     cause.
//  2. **Sub-PR closed/merged** — the referenced sub-PR is no longer live. CI
//     can't be stuck on a PR that doesn't exist. This catches the case where
//     the task stays Escalated forever but the PR that worried us is gone.
//
// Everything else stays Open — we specifically do NOT auto-resolve based on
// the existence of a successor task, because that successor might itself fail.
// The trigger is always "this exact problem is demonstrably absent now."
func dogEscalationSweeper(db *sql.DB, logger interface{ Printf(string, ...any) }) error {
	// Rule 1: task has reached a success terminal state (Completed or Cancelled)
	// since the escalation was filed. Failed/Escalated do NOT qualify — those
	// are the terminal states that typically CAUSED the escalation.
	rows, err := db.Query(`
		SELECT e.id, e.task_id, b.status
		FROM Escalations e
		JOIN BountyBoard b ON b.id = e.task_id
		WHERE e.status = 'Open'
		  AND b.status IN ('Completed','Cancelled')`)
	if err != nil {
		return err
	}
	type taskResolved struct {
		escID   int
		taskID  int
		newStat string
	}
	var taskTargets []taskResolved
	for rows.Next() {
		var r taskResolved
		if err := rows.Scan(&r.escID, &r.taskID, &r.newStat); err == nil {
			taskTargets = append(taskTargets, r)
		}
	}
	rows.Close()

	for _, r := range taskTargets {
		if _, err := db.Exec(`UPDATE Escalations
			SET status = 'Resolved', acknowledged_at = datetime('now')
			WHERE id = ? AND status = 'Open'`, r.escID); err != nil {
			logger.Printf("escalation-sweeper: failed to resolve #%d: %v", r.escID, err)
			continue
		}
		logger.Printf("escalation-sweeper: auto-resolved escalation #%d (task %d transitioned to %s)",
			r.escID, r.taskID, r.newStat)
		store.LogAudit(db, "escalation-sweeper", "auto-resolve", r.taskID,
			"task transitioned to "+r.newStat+" — escalation condition no longer present")
	}

	// Rule 2: sub-PR is in a terminal state even though the task isn't.
	// Covers the stuck-CI class where the PR itself has been force-closed by
	// our terminal-task early-exit.
	rows, err = db.Query(`
		SELECT e.id, e.task_id, pr.pr_number, pr.state
		FROM Escalations e
		JOIN BountyBoard b ON b.id = e.task_id
		JOIN (
			SELECT task_id, MAX(id) AS pr_id
			FROM AskBranchPRs
			GROUP BY task_id
		) latest ON latest.task_id = e.task_id
		JOIN AskBranchPRs pr ON pr.id = latest.pr_id
		WHERE e.status = 'Open'
		  AND b.status IN ('Escalated','Failed')
		  AND pr.state IN ('Merged','Closed')`)
	if err != nil {
		return err
	}
	defer rows.Close()

	type prResolved struct {
		escID    int
		taskID   int
		prNumber int
		prState  string
	}
	var prTargets []prResolved
	for rows.Next() {
		var r prResolved
		if err := rows.Scan(&r.escID, &r.taskID, &r.prNumber, &r.prState); err == nil {
			prTargets = append(prTargets, r)
		}
	}
	rows.Close()

	for _, r := range prTargets {
		if _, err := db.Exec(`UPDATE Escalations
			SET status = 'Resolved', acknowledged_at = datetime('now')
			WHERE id = ? AND status = 'Open'`, r.escID); err != nil {
			logger.Printf("escalation-sweeper: failed to resolve #%d: %v", r.escID, err)
			continue
		}
		logger.Printf("escalation-sweeper: auto-resolved escalation #%d (task %d, sub-PR #%d state=%s)",
			r.escID, r.taskID, r.prNumber, r.prState)
		store.LogAudit(db, "escalation-sweeper", "auto-resolve", r.taskID,
			"sub-PR transitioned to "+r.prState+" — escalation condition no longer present")
	}
	return nil
}
