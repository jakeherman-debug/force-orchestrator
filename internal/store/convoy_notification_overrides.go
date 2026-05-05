// convoy_notification_overrides.go — D11 Phase 2 sub-task C.
//
// Helpers around the ConvoyNotificationOverrides table that are not
// owned by the notify dispatcher (which only reads the row). The
// closure stamp + cleanup couple is the surface we expose here.
//
// MarkConvoyOverrideClosed is called from the convoy terminal-transition
// hook (Shipped / Abandoned / Failed) so the row is eligible for the
// notification-override-cleanup dog 7 days later. Idempotent —
// re-stamping a row already stamped is a silent no-op as far as
// downstream cleanup is concerned.

package store

import (
	"database/sql"
	"fmt"
)

// MarkConvoyOverrideClosed stamps convoy_closed_at on the
// ConvoyNotificationOverrides row for the given convoy so the
// notification-override-cleanup dog can purge it after 7 days.
//
// If ts is empty, the helper substitutes NowSQLite() — the canonical
// shape comparable to datetime('now') values written elsewhere. If
// no override row exists for this convoy, the UPDATE silently affects
// zero rows; the caller does not need to pre-check existence (the
// terminal transition fires for every convoy regardless of whether
// the operator ever set an override).
//
// Idempotent: re-stamping a row already stamped just slides the
// 7-day cleanup boundary forward, which is harmless.
func MarkConvoyOverrideClosed(db *sql.DB, convoyID int, ts string) error {
	if ts == "" {
		ts = NowSQLite()
	}
	_, err := db.Exec(
		`UPDATE ConvoyNotificationOverrides SET convoy_closed_at = ? WHERE convoy_id = ?`,
		ts, convoyID,
	)
	if err != nil {
		return fmt.Errorf("MarkConvoyOverrideClosed: convoy=%d: %w", convoyID, err)
	}
	return nil
}
