package store

import "database/sql"

// SetConvoyHold places a hard hold on a convoy. The Captain and Council will
// reject any task from this convoy before even calling Claude.
func SetConvoyHold(db *sql.DB, convoyID int, reason string) {
	db.Exec(`INSERT OR REPLACE INTO ConvoyHolds (convoy_id, reason) VALUES (?, ?)`,
		convoyID, reason)
}

// ClearConvoyHold removes the hold on a convoy, allowing Captain/Council to resume.
func ClearConvoyHold(db *sql.DB, convoyID int) {
	db.Exec(`DELETE FROM ConvoyHolds WHERE convoy_id = ?`, convoyID)
}

// ClearConvoyHoldTx is the transactional sibling of ClearConvoyHold.
// AUDIT-069 (Fix #8d): ResolveFeatureBlockers wraps its multi-table mutation
// in a single tx; this sibling lets the hold release participate in that
// atomic commit so a crash between "unblock dependencies" and "clear hold"
// cannot leave a convoy with wired deps but a lingering hold.
func ClearConvoyHoldTx(tx *sql.Tx, convoyID int) error {
	_, err := tx.Exec(`DELETE FROM ConvoyHolds WHERE convoy_id = ?`, convoyID)
	return err
}

// GetConvoyHold returns the hold reason for a convoy, or ("", false) if not held.
func GetConvoyHold(db *sql.DB, convoyID int) (string, bool) {
	if convoyID == 0 {
		return "", false
	}
	var reason string
	err := db.QueryRow(`SELECT reason FROM ConvoyHolds WHERE convoy_id = ?`, convoyID).Scan(&reason)
	if err != nil {
		return "", false
	}
	return reason, true
}
