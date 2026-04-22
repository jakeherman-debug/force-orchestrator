package store

import (
	"database/sql"
	"fmt"
	"log"
)

// ConvoyProgress returns (completed, total) task counts for a convoy.
// Cancelled tasks are excluded from total — they represent intentionally removed scope,
// not blocking work. A convoy with 2 done + 2 cancelled shows 2/2, not 2/4.
func ConvoyProgress(db *sql.DB, convoyID int) (completed, total int) {
	if err := db.QueryRow(`SELECT COUNT(*) FROM BountyBoard WHERE convoy_id = ? AND type = 'CodeEdit' AND status != 'Cancelled'`, convoyID).Scan(&total); err != nil {
		log.Printf("ConvoyProgress: scan total error for convoy %d: %v", convoyID, err)
		return
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM BountyBoard WHERE convoy_id = ? AND type = 'CodeEdit' AND status = 'Completed'`, convoyID).Scan(&completed); err != nil {
		log.Printf("ConvoyProgress: scan completed error for convoy %d: %v", convoyID, err)
		return
	}
	return
}

// CreateConvoy creates a named convoy and returns its ID.
func CreateConvoy(db *sql.DB, name string) (int, error) {
	res, err := db.Exec(`INSERT INTO Convoys (name, status) VALUES (?, 'Active')`, name)
	if err != nil {
		return 0, err
	}
	id, _ := res.LastInsertId()
	return int(id), nil
}

// ApproveConvoyTasks transitions all Planned tasks in a convoy to Pending.
// Returns the number of tasks activated.
func ApproveConvoyTasks(db *sql.DB, convoyID int) int {
	res, _ := db.Exec(`UPDATE BountyBoard SET status = 'Pending' WHERE convoy_id = ? AND status = 'Planned'`, convoyID)
	n, _ := res.RowsAffected()
	return int(n)
}

// AutoRecoverConvoy resets a Failed convoy back to Active if no problem tasks remain.
// Called automatically after a task is completed or reset. Safe to call with convoyID=0.
func AutoRecoverConvoy(db *sql.DB, convoyID int, logger interface{ Printf(string, ...any) }) {
	if convoyID == 0 {
		return
	}
	var convoyStatus string
	db.QueryRow(`SELECT status FROM Convoys WHERE id = ?`, convoyID).Scan(&convoyStatus)
	if convoyStatus != "Failed" {
		return
	}
	var problemCount int
	db.QueryRow(`SELECT COUNT(*) FROM BountyBoard WHERE convoy_id = ? AND status IN ('Failed','Escalated')`, convoyID).Scan(&problemCount)
	if problemCount == 0 {
		db.Exec(`UPDATE Convoys SET status = 'Active' WHERE id = ?`, convoyID)
		_ = AppendConvoyEvent(db, int64(convoyID), "status_change", "Failed", "Active", "auto-recover")
		if logger != nil {
			logger.Printf("Convoy #%d auto-recovered to Active (no remaining problem tasks)", convoyID)
		}
	}
}

// ResetConvoyTasks resets all Failed/Escalated tasks in a convoy back to Pending.
// Returns the number of tasks reset.
func ResetConvoyTasks(db *sql.DB, convoyID int) int {
	res, _ := db.Exec(`
		UPDATE BountyBoard
		SET status = 'Pending', owner = '', locked_at = '', error_log = '', retry_count = 0, infra_failures = 0, checkpoint = '', branch_name = ''
		WHERE convoy_id = ? AND status IN ('Failed', 'Escalated')`, convoyID)
	n, _ := res.RowsAffected()
	return int(n)
}

// CancelConvoyPendingTasks cancels all Planned/Pending tasks in a convoy.
// Returns the number of tasks cancelled.
func CancelConvoyPendingTasks(db *sql.DB, convoyID int) int {
	res, _ := db.Exec(`
		UPDATE BountyBoard
		SET status = 'Cancelled', owner = '', error_log = 'Operator rejected convoy plan'
		WHERE convoy_id = ? AND status IN ('Planned', 'Pending')`, convoyID)
	n, _ := res.RowsAffected()
	return int(n)
}

// RecoverStaleConvoys scans all Failed convoys and auto-recovers those that have no
// remaining problem tasks. Call at daemon startup to fix up convoys that were manually
// reset via CLI or DB without going through the normal task-completion path.
func RecoverStaleConvoys(db *sql.DB) {
	rows, err := db.Query(`SELECT id FROM Convoys WHERE status = 'Failed'`)
	if err != nil {
		return
	}
	var ids []int
	for rows.Next() {
		var id int
		rows.Scan(&id)
		ids = append(ids, id)
	}
	rows.Close()
	for _, id := range ids {
		AutoRecoverConvoy(db, id, nil)
	}
}

// ListConvoys returns all convoys ordered by creation date.
func ListConvoys(db *sql.DB) []Convoy {
	rows, err := db.Query(`SELECT
		id, name, status, IFNULL(coordinated, 0),
		IFNULL(ask_branch, ''), IFNULL(ask_branch_base_sha, ''),
		IFNULL(draft_pr_url, ''), IFNULL(draft_pr_number, 0),
		IFNULL(draft_pr_state, ''), IFNULL(shipped_at, ''),
		created_at
		FROM Convoys ORDER BY created_at DESC`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var convoys []Convoy
	for rows.Next() {
		var (
			c           Convoy
			coordinated int
		)
		if err := rows.Scan(&c.ID, &c.Name, &c.Status, &coordinated,
			&c.AskBranch, &c.AskBranchBaseSHA,
			&c.DraftPRURL, &c.DraftPRNumber, &c.DraftPRState, &c.ShippedAt,
			&c.CreatedAt); err != nil {
			log.Printf("ListConvoys: scan error: %v", err)
			return nil
		}
		c.Coordinated = coordinated == 1
		convoys = append(convoys, c)
	}
	return convoys
}

// GetConvoy returns the full Convoy row, or nil if not found.
func GetConvoy(db *sql.DB, convoyID int) *Convoy {
	var (
		c           Convoy
		coordinated int
	)
	err := db.QueryRow(`SELECT
		id, name, status, IFNULL(coordinated, 0),
		IFNULL(ask_branch, ''), IFNULL(ask_branch_base_sha, ''),
		IFNULL(draft_pr_url, ''), IFNULL(draft_pr_number, 0),
		IFNULL(draft_pr_state, ''), IFNULL(shipped_at, ''),
		created_at
		FROM Convoys WHERE id = ?`, convoyID).
		Scan(&c.ID, &c.Name, &c.Status, &coordinated,
			&c.AskBranch, &c.AskBranchBaseSHA,
			&c.DraftPRURL, &c.DraftPRNumber, &c.DraftPRState, &c.ShippedAt,
			&c.CreatedAt)
	if err != nil {
		return nil
	}
	c.Coordinated = coordinated == 1
	return &c
}

// SetConvoyAskBranch records the ask-branch and its base SHA on main. Called by
// Pilot after CreateAskBranch completes (branch cut and pushed). Both values
// must be non-empty — an empty ask_branch is the signal for main-drift-watch
// to skip the convoy, and an empty base_sha makes drift detection impossible.
func SetConvoyAskBranch(db *sql.DB, convoyID int, branch, baseSHA string) error {
	if branch == "" || baseSHA == "" {
		return fmt.Errorf("SetConvoyAskBranch: branch and baseSHA must be non-empty (got %q, %q)", branch, baseSHA)
	}
	_, err := db.Exec(`UPDATE Convoys SET ask_branch = ?, ask_branch_base_sha = ? WHERE id = ?`,
		branch, baseSHA, convoyID)
	if err != nil {
		return err
	}
	_ = AppendConvoyEvent(db, int64(convoyID), "ask_branch_created", "", branch, "")
	return nil
}

// UpdateConvoyAskBranchBaseSHA rewrites the stored base SHA after a successful
// rebase onto main. Called by Pilot.RebaseAskBranch when the rebase lands; the
// branch name does not change.
func UpdateConvoyAskBranchBaseSHA(db *sql.DB, convoyID int, newBaseSHA string) error {
	if newBaseSHA == "" {
		return fmt.Errorf("UpdateConvoyAskBranchBaseSHA: newBaseSHA must be non-empty")
	}
	_, err := db.Exec(`UPDATE Convoys SET ask_branch_base_sha = ? WHERE id = ?`,
		newBaseSHA, convoyID)
	return err
}

// SetConvoyDraftPR records the draft PR created by Diplomat. state should be
// "Open" at creation time; draft-pr-watch transitions it to Merged or Closed.
func SetConvoyDraftPR(db *sql.DB, convoyID int, url string, number int, state string) error {
	_, err := db.Exec(`UPDATE Convoys SET draft_pr_url = ?, draft_pr_number = ?, draft_pr_state = ? WHERE id = ?`,
		url, number, state, convoyID)
	if err != nil {
		return err
	}
	_ = AppendConvoyEvent(db, int64(convoyID), "draft_pr_opened", "", url, "")
	return nil
}

// UpdateConvoyDraftPRState transitions the draft PR state (Open → Merged/Closed).
// When state == "Merged", also stamps shipped_at.
func UpdateConvoyDraftPRState(db *sql.DB, convoyID int, state string) error {
	if state == "Merged" {
		_, err := db.Exec(`UPDATE Convoys SET draft_pr_state = ?, shipped_at = datetime('now') WHERE id = ?`,
			state, convoyID)
		if err != nil {
			return err
		}
		_ = AppendConvoyEvent(db, int64(convoyID), "shipped", "", "", "")
		return nil
	}
	_, err := db.Exec(`UPDATE Convoys SET draft_pr_state = ? WHERE id = ?`, state, convoyID)
	return err
}

// SetConvoyStatus updates the convoy lifecycle status. Separate from status
// transitions driven by individual task completions (AutoRecoverConvoy etc.) —
// used for PR-flow state machine moves: Active → AwaitingDraftPR → DraftPROpen
// → Shipped / Abandoned.
func SetConvoyStatus(db *sql.DB, convoyID int, status string) error {
	var oldStatus string
	db.QueryRow(`SELECT IFNULL(status, '') FROM Convoys WHERE id = ?`, convoyID).Scan(&oldStatus)
	_, err := db.Exec(`UPDATE Convoys SET status = ? WHERE id = ?`, status, convoyID)
	if err != nil {
		return err
	}
	_ = AppendConvoyEvent(db, int64(convoyID), "status_change", oldStatus, status, "")
	return nil
}

// SetConvoyStatusTx is the transactional sibling of SetConvoyStatus.
func SetConvoyStatusTx(tx *sql.Tx, convoyID int, status string) error {
	var oldStatus string
	tx.QueryRow(`SELECT IFNULL(status, '') FROM Convoys WHERE id = ?`, convoyID).Scan(&oldStatus)
	_, err := tx.Exec(`UPDATE Convoys SET status = ? WHERE id = ?`, status, convoyID)
	if err != nil {
		return err
	}
	_ = AppendConvoyEventTx(tx, int64(convoyID), "status_change", oldStatus, status, "")
	return nil
}

// ActiveConvoysMissingAskBranch returns convoy IDs that are Active but have at
// least one touched repo without a ConvoyAskBranch row. Correctly handles the
// multi-repo case: a convoy with repos [api, monolith] where api has a branch
// but monolith doesn't is still returned (monolith needs backfilling).
//
// Used by the Layer C lazy-backfill inquisitor check to enqueue CreateAskBranch
// tasks. Note: CreateAskBranch itself fans out per-repo, so a single task per
// convoy is sufficient — the query only needs to find convoys where some repo
// is missing, not enumerate which repo.
func ActiveConvoysMissingAskBranch(db *sql.DB) []int {
	rows, err := db.Query(`
		SELECT DISTINCT c.id
		FROM Convoys c
		JOIN BountyBoard b ON b.convoy_id = c.id
		WHERE c.status = 'Active'
		  AND b.type = 'CodeEdit'
		  AND IFNULL(b.target_repo, '') != ''
		  AND b.status != 'Cancelled'
		  AND NOT EXISTS (
		    SELECT 1 FROM ConvoyAskBranches cab
		    WHERE cab.convoy_id = c.id AND cab.repo = b.target_repo
		  )
		ORDER BY c.id ASC`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var ids []int
	for rows.Next() {
		var id int
		if err := rows.Scan(&id); err == nil {
			ids = append(ids, id)
		}
	}
	return ids
}
