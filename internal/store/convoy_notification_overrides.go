// internal/store/convoy_notification_overrides.go — D11 Phase 2 Sub-task B.
//
// Per-convoy operator override of the fleet-wide notification preset. The
// table itself is created in createSchema + runMigrations (D11 Phase 1);
// this file is the Go-side accessors used by the dashboard handler and
// the cleanup dog (sub-task C).
//
// Lifecycle:
//
//   - Operator clicks the "Watch" chip on a convoy card and chooses a
//     mode (verbose / quiet / custom_json) → dashboard handler calls
//     UpsertConvoyNotificationOverride.
//   - The notify dispatcher reads the row via its own loadConvoyOverride
//     (internal/notify/dispatcher.go) on every Dispatch call to short-
//     circuit the resolution chain.
//   - When the convoy hits a terminal status, the cleanup dog calls
//     MarkConvoyOverrideClosed to stamp convoy_closed_at, then later
//     deletes rows older than the retention window.
//
// All helpers return error per CLAUDE.md "no silent failures." Callers
// MUST surface the error — never log-and-continue.

package store

import (
	"database/sql"
	"fmt"
)

// ConvoyNotificationOverride is the row shape of ConvoyNotificationOverrides.
//
// Mode is one of:
//   - "verbose"     — every category fires regardless of preset / DND
//   - "quiet"       — every category is suppressed regardless of preset
//   - "custom_json" — CustomJSON carries a JSON object mapping category
//                     name → setting ("off"|"mail"|"slack"|"mail+slack").
//                     A "*" key acts as a fallback for unspecified categories.
//
// CustomJSON is the raw JSON text as stored in the column. Callers that
// need the parsed map should json.Unmarshal it themselves.
//
// ConvoyClosedAt is empty while the convoy is still active; the cleanup
// dog (sub-task C) stamps it when the convoy hits a terminal status.
type ConvoyNotificationOverride struct {
	ConvoyID       int
	Mode           string
	CustomJSON     string
	SetAt          string
	SetBy          string
	Reason         string
	ConvoyClosedAt string
}

// UpsertConvoyNotificationOverride inserts or replaces the override for a
// convoy. The set_at column is stamped server-side via NowSQLite so all
// rows compare apples-to-apples with `datetime('now')` reads.
//
// Idempotent: re-calling with the same (convoy_id, mode, custom_json,
// reason) overwrites set_at + set_by. Callers that need atomic CAS
// semantics should use UpdateBountyStatusFrom-style guards in their own
// flow; this helper is unconditional.
//
// Returns error on an underlying DB failure or invalid mode.
func UpsertConvoyNotificationOverride(db *sql.DB, ov ConvoyNotificationOverride) error {
	if ov.ConvoyID <= 0 {
		return fmt.Errorf("store: UpsertConvoyNotificationOverride convoy_id must be > 0")
	}
	switch ov.Mode {
	case "verbose", "quiet", "custom_json":
	default:
		return fmt.Errorf("store: UpsertConvoyNotificationOverride invalid mode %q (want verbose|quiet|custom_json)", ov.Mode)
	}
	custom := ov.CustomJSON
	if custom == "" {
		custom = "{}"
	}
	setAt := ov.SetAt
	if setAt == "" {
		setAt = NowSQLite()
	}
	_, err := db.Exec(
		`INSERT INTO ConvoyNotificationOverrides
		   (convoy_id, mode, custom_json, set_at, set_by, reason, convoy_closed_at)
		 VALUES (?, ?, ?, ?, ?, ?, NULL)
		 ON CONFLICT(convoy_id) DO UPDATE SET
		   mode = excluded.mode,
		   custom_json = excluded.custom_json,
		   set_at = excluded.set_at,
		   set_by = excluded.set_by,
		   reason = excluded.reason,
		   convoy_closed_at = NULL`,
		ov.ConvoyID, ov.Mode, custom, setAt, ov.SetBy, ov.Reason,
	)
	if err != nil {
		return fmt.Errorf("store: UpsertConvoyNotificationOverride convoy=%d: %w", ov.ConvoyID, err)
	}
	return nil
}

// GetConvoyNotificationOverride returns the override row for the given
// convoy. Returns sql.ErrNoRows when no row exists — callers check that
// sentinel to distinguish "no override" from "DB failure". Wrapping with
// %w preserves errors.Is(err, sql.ErrNoRows) semantics.
func GetConvoyNotificationOverride(db *sql.DB, convoyID int) (ConvoyNotificationOverride, error) {
	var ov ConvoyNotificationOverride
	if convoyID <= 0 {
		return ov, fmt.Errorf("store: GetConvoyNotificationOverride convoy_id must be > 0")
	}
	err := db.QueryRow(
		`SELECT convoy_id, mode, IFNULL(custom_json, '{}'), IFNULL(set_at, ''),
		        IFNULL(set_by, ''), IFNULL(reason, ''), IFNULL(convoy_closed_at, '')
		 FROM ConvoyNotificationOverrides
		 WHERE convoy_id = ?`,
		convoyID,
	).Scan(&ov.ConvoyID, &ov.Mode, &ov.CustomJSON, &ov.SetAt, &ov.SetBy, &ov.Reason, &ov.ConvoyClosedAt)
	if err == sql.ErrNoRows {
		return ov, sql.ErrNoRows
	}
	if err != nil {
		return ov, fmt.Errorf("store: GetConvoyNotificationOverride convoy=%d: %w", convoyID, err)
	}
	return ov, nil
}

// ClearConvoyNotificationOverride deletes the override row for the given
// convoy. No-op (returns nil) if the row doesn't exist — clearing a
// missing override is the operator's idempotent "make sure default
// rules apply" path, not an error condition.
func ClearConvoyNotificationOverride(db *sql.DB, convoyID int) error {
	if convoyID <= 0 {
		return fmt.Errorf("store: ClearConvoyNotificationOverride convoy_id must be > 0")
	}
	_, err := db.Exec(`DELETE FROM ConvoyNotificationOverrides WHERE convoy_id = ?`, convoyID)
	if err != nil {
		return fmt.Errorf("store: ClearConvoyNotificationOverride convoy=%d: %w", convoyID, err)
	}
	return nil
}

// ListActiveConvoyNotificationOverrides returns every override row whose
// convoy is still active (convoy_closed_at IS NULL OR ''). Closed rows
// are excluded so the dashboard "active overrides" list doesn't drag in
// rows from convoys that already shipped. Sorted by set_at desc so the
// most recent override appears first.
func ListActiveConvoyNotificationOverrides(db *sql.DB) ([]ConvoyNotificationOverride, error) {
	rows, err := db.Query(
		`SELECT convoy_id, mode, IFNULL(custom_json, '{}'), IFNULL(set_at, ''),
		        IFNULL(set_by, ''), IFNULL(reason, ''), IFNULL(convoy_closed_at, '')
		 FROM ConvoyNotificationOverrides
		 WHERE convoy_closed_at IS NULL OR convoy_closed_at = ''
		 ORDER BY set_at DESC`,
	)
	if err != nil {
		return nil, fmt.Errorf("store: ListActiveConvoyNotificationOverrides: %w", err)
	}
	defer rows.Close()
	var out []ConvoyNotificationOverride
	for rows.Next() {
		var ov ConvoyNotificationOverride
		if err := rows.Scan(&ov.ConvoyID, &ov.Mode, &ov.CustomJSON, &ov.SetAt,
			&ov.SetBy, &ov.Reason, &ov.ConvoyClosedAt); err != nil {
			return nil, fmt.Errorf("store: ListActiveConvoyNotificationOverrides scan: %w", err)
		}
		out = append(out, ov)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: ListActiveConvoyNotificationOverrides iter: %w", err)
	}
	return out, nil
}

// MarkConvoyOverrideClosed stamps convoy_closed_at on the override row
// for the given convoy. Called by the cleanup dog (D11 Phase 2 sub-task
// C) when the convoy hits a terminal status. The stamped value is
// retained until the dog's retention sweep deletes the row a fixed
// window later — callers don't need to delete after stamping.
//
// closedAt is the timestamp to write (typically NowSQLite at the moment
// the convoy entered terminal). Empty string is rejected: a closed-at
// value of "" is indistinguishable from "still active" in
// ListActiveConvoyNotificationOverrides.
//
// No-op (returns nil) if no row exists for the convoy — the cleanup dog
// may sweep convoys that never had an override.
func MarkConvoyOverrideClosed(db *sql.DB, convoyID int, closedAt string) error {
	if convoyID <= 0 {
		return fmt.Errorf("store: MarkConvoyOverrideClosed convoy_id must be > 0")
	}
	if closedAt == "" {
		return fmt.Errorf("store: MarkConvoyOverrideClosed closedAt must be non-empty")
	}
	_, err := db.Exec(
		`UPDATE ConvoyNotificationOverrides SET convoy_closed_at = ? WHERE convoy_id = ?`,
		closedAt, convoyID,
	)
	if err != nil {
		return fmt.Errorf("store: MarkConvoyOverrideClosed convoy=%d: %w", convoyID, err)
	}
	return nil
}
