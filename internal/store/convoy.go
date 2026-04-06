package store

import (
	"database/sql"
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

// ListConvoys returns all convoys ordered by creation date.
func ListConvoys(db *sql.DB) []Convoy {
	rows, err := db.Query(`SELECT id, name, status, created_at FROM Convoys ORDER BY created_at DESC`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var convoys []Convoy
	for rows.Next() {
		var c Convoy
		if err := rows.Scan(&c.ID, &c.Name, &c.Status, &c.CreatedAt); err != nil {
			log.Printf("ListConvoys: scan error: %v", err)
			return nil
		}
		convoys = append(convoys, c)
	}
	return convoys
}
