package store

import (
	"database/sql"
	"fmt"
	"strconv"
)

// ── Ask-branch sub-PR state ──────────────────────────────────────────────────
//
// Each astromech task that is approved by the Jedi Council under the PR flow
// produces a real GitHub PR against the convoy's ask-branch. AskBranchPRs is
// the per-PR state machine: the sub-pr-ci-watch dog polls GitHub and advances
// rows through (Open, Pending) → (Open, Success) → auto-merged (Merged), or
// to (Open, Failure) → Medic CIFailureTriage. A PR closed by a human outside
// the fleet transitions to state=Closed and escalates the parent task.

// CreateAskBranchPR records a freshly-opened sub-PR. pr_url/pr_number come
// straight from `gh pr create --json`. Returns the AskBranchPR row ID.
// Idempotent: returns the existing row's ID if a PR with the same (repo,
// pr_number) already exists.
func CreateAskBranchPR(db *sql.DB, taskID, convoyID int, repo, prURL string, prNumber int) (int, error) {
	if taskID <= 0 || convoyID <= 0 || repo == "" || prNumber <= 0 {
		return 0, fmt.Errorf("CreateAskBranchPR: all fields required (got task=%d convoy=%d repo=%q pr=%d)",
			taskID, convoyID, repo, prNumber)
	}
	// Attempt insert; on unique (repo, pr_number) collision, reuse the existing row.
	res, err := db.Exec(`INSERT INTO AskBranchPRs
		(task_id, convoy_id, repo, pr_number, pr_url, state, checks_state)
		VALUES (?, ?, ?, ?, ?, 'Open', 'Pending')`,
		taskID, convoyID, repo, prNumber, prURL)
	if err != nil {
		// Unique constraint violation → look up existing row.
		var existingID int
		if scanErr := db.QueryRow(`SELECT id FROM AskBranchPRs WHERE repo = ? AND pr_number = ?`,
			repo, prNumber).Scan(&existingID); scanErr == nil {
			return existingID, nil
		}
		return 0, err
	}
	id, _ := res.LastInsertId()
	return int(id), nil
}

// GetAskBranchPR fetches a sub-PR row by its internal ID.
func GetAskBranchPR(db *sql.DB, id int) *AskBranchPR {
	var p AskBranchPR
	err := db.QueryRow(`SELECT id, task_id, convoy_id, repo,
		IFNULL(pr_number, 0), IFNULL(pr_url, ''),
		IFNULL(state, ''), IFNULL(checks_state, ''),
		IFNULL(failure_count, 0), IFNULL(stall_retrigger_count, 0),
		IFNULL(merged_at, ''), IFNULL(created_at, '')
		FROM AskBranchPRs WHERE id = ?`, id).
		Scan(&p.ID, &p.TaskID, &p.ConvoyID, &p.Repo,
			&p.PRNumber, &p.PRURL,
			&p.State, &p.ChecksState,
			&p.FailureCount, &p.StallRetriggerCount, &p.MergedAt, &p.CreatedAt)
	if err != nil {
		return nil
	}
	return &p
}

// GetAskBranchPRByTask fetches the (most recent) sub-PR row for a task. Returns
// nil if the task has no sub-PR yet (task is still pre-council or running on
// the legacy local-merge path).
func GetAskBranchPRByTask(db *sql.DB, taskID int) *AskBranchPR {
	var p AskBranchPR
	err := db.QueryRow(`SELECT id, task_id, convoy_id, repo,
		IFNULL(pr_number, 0), IFNULL(pr_url, ''),
		IFNULL(state, ''), IFNULL(checks_state, ''),
		IFNULL(failure_count, 0), IFNULL(stall_retrigger_count, 0),
		IFNULL(merged_at, ''), IFNULL(created_at, '')
		FROM AskBranchPRs WHERE task_id = ? ORDER BY id DESC LIMIT 1`, taskID).
		Scan(&p.ID, &p.TaskID, &p.ConvoyID, &p.Repo,
			&p.PRNumber, &p.PRURL,
			&p.State, &p.ChecksState,
			&p.FailureCount, &p.StallRetriggerCount, &p.MergedAt, &p.CreatedAt)
	if err != nil {
		return nil
	}
	return &p
}

// ListOpenAskBranchPRs returns every AskBranchPR in state='Open'. Used by
// sub-pr-ci-watch to enumerate candidates for CI polling.
func ListOpenAskBranchPRs(db *sql.DB) []AskBranchPR {
	rows, err := db.Query(`SELECT id, task_id, convoy_id, repo,
		IFNULL(pr_number, 0), IFNULL(pr_url, ''),
		IFNULL(state, ''), IFNULL(checks_state, ''),
		IFNULL(failure_count, 0), IFNULL(stall_retrigger_count, 0),
		IFNULL(merged_at, ''), IFNULL(created_at, '')
		FROM AskBranchPRs WHERE state = 'Open' ORDER BY id ASC`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var prs []AskBranchPR
	for rows.Next() {
		var p AskBranchPR
		if err := rows.Scan(&p.ID, &p.TaskID, &p.ConvoyID, &p.Repo,
			&p.PRNumber, &p.PRURL,
			&p.State, &p.ChecksState,
			&p.FailureCount, &p.StallRetriggerCount, &p.MergedAt, &p.CreatedAt); err == nil {
			prs = append(prs, p)
		}
	}
	return prs
}

// ListAskBranchPRsByConvoy returns every sub-PR for a convoy in creation order.
// Dashboard and Diplomat use this to roll up CI state across the convoy.
func ListAskBranchPRsByConvoy(db *sql.DB, convoyID int) []AskBranchPR {
	rows, err := db.Query(`SELECT id, task_id, convoy_id, repo,
		IFNULL(pr_number, 0), IFNULL(pr_url, ''),
		IFNULL(state, ''), IFNULL(checks_state, ''),
		IFNULL(failure_count, 0), IFNULL(stall_retrigger_count, 0),
		IFNULL(merged_at, ''), IFNULL(created_at, '')
		FROM AskBranchPRs WHERE convoy_id = ? ORDER BY id ASC`, convoyID)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var prs []AskBranchPR
	for rows.Next() {
		var p AskBranchPR
		if err := rows.Scan(&p.ID, &p.TaskID, &p.ConvoyID, &p.Repo,
			&p.PRNumber, &p.PRURL,
			&p.State, &p.ChecksState,
			&p.FailureCount, &p.StallRetriggerCount, &p.MergedAt, &p.CreatedAt); err == nil {
			prs = append(prs, p)
		}
	}
	return prs
}

// UpdateAskBranchPRChecks updates the checks_state ('Pending'|'Success'|'Failure').
// Does NOT change the state column — that transitions separately via MarkAskBranchPRMerged
// or MarkAskBranchPRClosed.
func UpdateAskBranchPRChecks(db *sql.DB, id int, checksState string) error {
	switch checksState {
	case "Pending", "Success", "Failure":
	default:
		return fmt.Errorf("UpdateAskBranchPRChecks: invalid checks_state %q", checksState)
	}
	_, err := db.Exec(`UPDATE AskBranchPRs SET checks_state = ? WHERE id = ?`, checksState, id)
	return err
}

// IncrementAskBranchPRFailureCount bumps failure_count by 1 and returns the new value.
// Used by Medic CIFailureTriage to enforce retry caps (e.g. 3 RealBug fix attempts).
func IncrementAskBranchPRFailureCount(db *sql.DB, id int) (int, error) {
	if _, err := db.Exec(`UPDATE AskBranchPRs SET failure_count = failure_count + 1 WHERE id = ?`, id); err != nil {
		return 0, err
	}
	var count int
	if err := db.QueryRow(`SELECT failure_count FROM AskBranchPRs WHERE id = ?`, id).Scan(&count); err != nil {
		return 0, err
	}
	return count, nil
}

// MarkAskBranchPRMerged transitions the PR to state=Merged, clearing further
// polling by sub-pr-ci-watch. Stamps merged_at to the current time.
func MarkAskBranchPRMerged(db *sql.DB, id int) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := MarkAskBranchPRMergedTx(tx, id); err != nil {
		return err
	}
	return tx.Commit()
}

// MarkAskBranchPRMergedTx is the transactional sibling of MarkAskBranchPRMerged.
func MarkAskBranchPRMergedTx(tx *sql.Tx, id int) error {
	var convoyID, prNumber int
	var repo string
	tx.QueryRow(`SELECT convoy_id, pr_number, repo FROM AskBranchPRs WHERE id = ?`, id).
		Scan(&convoyID, &prNumber, &repo)
	_, err := tx.Exec(`UPDATE AskBranchPRs
		SET state = 'Merged', checks_state = 'Success', merged_at = datetime('now')
		WHERE id = ?`, id)
	if err == nil && convoyID > 0 {
		AppendConvoyEventTx(tx, convoyID, "sub_pr_merged", "", strconv.Itoa(prNumber), repo)
	}
	return err
}

// MarkAskBranchPRClosed transitions the PR to state=Closed (closed on GitHub without
// merge — either by a human or because the astromech branch was deleted). The
// parent task should be escalated by the caller; this function only updates the
// PR row.
func MarkAskBranchPRClosed(db *sql.DB, id int) error {
	_, err := db.Exec(`UPDATE AskBranchPRs SET state = 'Closed' WHERE id = ?`, id)
	return err
}

// MarkAskBranchPRClosedTx is the transactional sibling of MarkAskBranchPRClosed.
func MarkAskBranchPRClosedTx(tx *sql.Tx, id int) error {
	_, err := tx.Exec(`UPDATE AskBranchPRs SET state = 'Closed' WHERE id = ?`, id)
	return err
}

// IncrementStallRetriggerCount bumps stall_retrigger_count by 1 and returns the
// new value. Used by sub-pr-ci-watch to cap the number of empty-commit re-trigger
// attempts on a stuck CI run before falling through to escalation.
func IncrementStallRetriggerCount(db *sql.DB, id int) (int, error) {
	if _, err := db.Exec(`UPDATE AskBranchPRs SET stall_retrigger_count = stall_retrigger_count + 1 WHERE id = ?`, id); err != nil {
		return 0, err
	}
	var count int
	if err := db.QueryRow(`SELECT stall_retrigger_count FROM AskBranchPRs WHERE id = ?`, id).Scan(&count); err != nil {
		return 0, err
	}
	return count, nil
}

// IncrementAskBranchPRFailureCountTx is the transactional sibling of IncrementAskBranchPRFailureCount.
func IncrementAskBranchPRFailureCountTx(tx *sql.Tx, id int) (int, error) {
	if _, err := tx.Exec(`UPDATE AskBranchPRs SET failure_count = failure_count + 1 WHERE id = ?`, id); err != nil {
		return 0, err
	}
	var count int
	if err := tx.QueryRow(`SELECT failure_count FROM AskBranchPRs WHERE id = ?`, id).Scan(&count); err != nil {
		return 0, err
	}
	return count, nil
}

// CreateAskBranchPRTx is the transactional sibling of CreateAskBranchPR.
func CreateAskBranchPRTx(tx *sql.Tx, taskID, convoyID int, repo, prURL string, prNumber int) (int, error) {
	if taskID <= 0 || convoyID <= 0 || repo == "" || prNumber <= 0 {
		return 0, fmt.Errorf("CreateAskBranchPRTx: all fields required (got task=%d convoy=%d repo=%q pr=%d)",
			taskID, convoyID, repo, prNumber)
	}
	res, err := tx.Exec(`INSERT INTO AskBranchPRs
		(task_id, convoy_id, repo, pr_number, pr_url, state, checks_state)
		VALUES (?, ?, ?, ?, ?, 'Open', 'Pending')`,
		taskID, convoyID, repo, prNumber, prURL)
	if err != nil {
		var existingID int
		if scanErr := tx.QueryRow(`SELECT id FROM AskBranchPRs WHERE repo = ? AND pr_number = ?`,
			repo, prNumber).Scan(&existingID); scanErr == nil {
			return existingID, nil
		}
		return 0, err
	}
	id, _ := res.LastInsertId()
	return int(id), nil
}

// AskBranchPRRollup summarises sub-PR state for a convoy. Used by Diplomat to
// gate draft-PR creation (needs all sub-PRs Merged and ask-branch CI green).
type AskBranchPRRollup struct {
	Total          int
	Open           int
	Merged         int
	Closed         int
	ChecksPending  int
	ChecksSuccess  int
	ChecksFailure  int
}

// RollupAskBranchPRs returns a summary of sub-PR state for a convoy.
func RollupAskBranchPRs(db *sql.DB, convoyID int) AskBranchPRRollup {
	var r AskBranchPRRollup
	rows, err := db.Query(`SELECT
		IFNULL(state, ''), IFNULL(checks_state, '')
		FROM AskBranchPRs WHERE convoy_id = ?`, convoyID)
	if err != nil {
		return r
	}
	defer rows.Close()
	for rows.Next() {
		var state, checks string
		if err := rows.Scan(&state, &checks); err != nil {
			continue
		}
		r.Total++
		switch state {
		case "Open":
			r.Open++
		case "Merged":
			r.Merged++
		case "Closed":
			r.Closed++
		}
		switch checks {
		case "Pending":
			r.ChecksPending++
		case "Success":
			r.ChecksSuccess++
		case "Failure":
			r.ChecksFailure++
		}
	}
	return r
}
