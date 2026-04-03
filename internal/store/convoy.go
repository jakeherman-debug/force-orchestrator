package store

import "database/sql"

// ConvoyProgress returns (completed, total) task counts for a convoy.
func ConvoyProgress(db *sql.DB, convoyID int) (completed, total int) {
	db.QueryRow(`SELECT COUNT(*) FROM BountyBoard WHERE convoy_id = ? AND type = 'CodeEdit'`, convoyID).Scan(&total)
	db.QueryRow(`SELECT COUNT(*) FROM BountyBoard WHERE convoy_id = ? AND type = 'CodeEdit' AND status = 'Completed'`, convoyID).Scan(&completed)
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
		SET status = 'Failed', owner = '', error_log = 'Operator rejected convoy plan'
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
		rows.Scan(&c.ID, &c.Name, &c.Status, &c.CreatedAt)
		convoys = append(convoys, c)
	}
	return convoys
}
