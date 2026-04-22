package store

import "database/sql"

// AppendConvoyEvent inserts one event row into the ConvoyEvents timeline.
// Errors are silently swallowed — event recording must never block a state
// transition (it's advisory, not load-bearing).
func AppendConvoyEvent(db *sql.DB, convoyID int, eventType, oldValue, newValue, detail string) {
	db.Exec(`INSERT INTO ConvoyEvents (convoy_id, event_type, old_value, new_value, detail)
		VALUES (?, ?, ?, ?, ?)`,
		convoyID, eventType, oldValue, newValue, detail)
}

// AppendConvoyEventTx is the transactional sibling of AppendConvoyEvent.
// The event rolls back with the parent transaction if the caller rolls back.
func AppendConvoyEventTx(tx *sql.Tx, convoyID int, eventType, oldValue, newValue, detail string) {
	tx.Exec(`INSERT INTO ConvoyEvents (convoy_id, event_type, old_value, new_value, detail)
		VALUES (?, ?, ?, ?, ?)`,
		convoyID, eventType, oldValue, newValue, detail)
}

// ListConvoyEvents returns all events for a convoy in chronological order.
func ListConvoyEvents(db *sql.DB, convoyID int) []ConvoyEvent {
	rows, err := db.Query(`SELECT id, convoy_id, event_type,
		IFNULL(old_value, ''), IFNULL(new_value, ''), IFNULL(detail, ''), created_at
		FROM ConvoyEvents WHERE convoy_id = ? ORDER BY id ASC`, convoyID)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var events []ConvoyEvent
	for rows.Next() {
		var e ConvoyEvent
		if err := rows.Scan(&e.ID, &e.ConvoyID, &e.EventType,
			&e.OldValue, &e.NewValue, &e.Detail, &e.CreatedAt); err == nil {
			events = append(events, e)
		}
	}
	if rows.Err() != nil {
		return nil
	}
	return events
}
