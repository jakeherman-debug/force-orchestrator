package store

import (
	"database/sql"
	"fmt"
)

// ── ConvoyEvents CRUD ────────────────────────────────────────────────────────
//
// Append-only timeline of per-convoy state changes. Callers emit a row whenever
// something meaningful happens to a convoy (status transition, ask-branch cut,
// draft PR opened, sub-PR merged, shipped, etc.). The dashboard's real-time
// convoy timeline reads from this table in created_at ASC order.

// AppendConvoyEvent inserts a single event row. oldValue, newValue, and detail
// are all optional — pass "" for any the event doesn't need.
func AppendConvoyEvent(db *sql.DB, convoyID int64, eventType, oldValue, newValue, detail string) error {
	if convoyID <= 0 {
		return fmt.Errorf("AppendConvoyEvent: convoyID must be > 0, got %d", convoyID)
	}
	if eventType == "" {
		return fmt.Errorf("AppendConvoyEvent: eventType required")
	}
	_, err := db.Exec(`INSERT INTO ConvoyEvents
		(convoy_id, event_type, old_value, new_value, detail)
		VALUES (?, ?, ?, ?, ?)`,
		convoyID, eventType, oldValue, newValue, detail)
	return err
}

// ListConvoyEvents returns every event for a convoy, oldest first. Nullable
// text columns are coalesced to "" so callers never see sql.NullString.
func ListConvoyEvents(db *sql.DB, convoyID int64) ([]ConvoyEvent, error) {
	rows, err := db.Query(`SELECT
		id, convoy_id, event_type,
		IFNULL(old_value, ''), IFNULL(new_value, ''), IFNULL(detail, ''),
		created_at
		FROM ConvoyEvents
		WHERE convoy_id = ?
		ORDER BY created_at ASC, id ASC`, convoyID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ConvoyEvent
	for rows.Next() {
		var e ConvoyEvent
		if err := rows.Scan(&e.ID, &e.ConvoyID, &e.EventType,
			&e.OldValue, &e.NewValue, &e.Detail, &e.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
