package store

import "database/sql"

// AppendTaskNote inserts an operator note for the given task.
func AppendTaskNote(db *sql.DB, taskID int, note string) error {
	_, err := db.Exec(
		`INSERT INTO TaskNotes (task_id, note) VALUES (?, ?)`,
		taskID, note,
	)
	return err
}

// GetTaskNotes returns all notes for the given task ordered by created_at ASC.
func GetTaskNotes(db *sql.DB, taskID int) ([]string, error) {
	rows, err := db.Query(
		`SELECT note FROM TaskNotes WHERE task_id = ? ORDER BY created_at ASC`,
		taskID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var notes []string
	for rows.Next() {
		var note string
		if err := rows.Scan(&note); err != nil {
			return nil, err
		}
		notes = append(notes, note)
	}
	return notes, rows.Err()
}
