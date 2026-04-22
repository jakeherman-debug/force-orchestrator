package store

import "database/sql"

// RecordConvoyEvent inserts a timestamped event into the ConvoyEvents timeline.
func RecordConvoyEvent(db *sql.DB, convoyID int, eventType, detail string) error {
	_, err := db.Exec(
		`INSERT INTO ConvoyEvents (convoy_id, event_type, detail) VALUES (?, ?, ?)`,
		convoyID, eventType, detail,
	)
	return err
}

// ListConvoyEvents returns all events for a convoy ordered by created_at ASC.
func ListConvoyEvents(db *sql.DB, convoyID int) ([]ConvoyEvent, error) {
	rows, err := db.Query(
		`SELECT id, convoy_id, event_type, IFNULL(detail, ''), created_at
		 FROM ConvoyEvents WHERE convoy_id = ? ORDER BY created_at ASC`,
		convoyID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var events []ConvoyEvent
	for rows.Next() {
		var e ConvoyEvent
		if err := rows.Scan(&e.ID, &e.ConvoyID, &e.EventType, &e.Detail, &e.CreatedAt); err != nil {
			return nil, err
		}
		events = append(events, e)
	}
	return events, rows.Err()
}
