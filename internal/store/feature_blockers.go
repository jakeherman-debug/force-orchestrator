package store

import "database/sql"

// CreateFeatureBlocker records that blockedConvoyID cannot proceed until
// blockingFeatureID has a completed convoy. Also sets a ConvoyHold so the
// Captain and Council hard-reject in-flight tasks from the blocked convoy.
func CreateFeatureBlocker(db *sql.DB, blockedConvoyID, blockingFeatureID int, reason string) {
	db.Exec(`INSERT OR IGNORE INTO FeatureBlockers (blocked_convoy_id, blocking_feature_id)
	         VALUES (?, ?)`, blockedConvoyID, blockingFeatureID)
	SetConvoyHold(db, blockedConvoyID, reason)
}

// ResolveFeatureBlockers is called when blockingFeatureID's convoy (newConvoyID) is
// approved by the Chancellor. It wires real TaskDependencies from the blocked convoy's
// root Pending/Planned tasks to the new convoy's tail tasks, then clears the blockers
// and any ConvoyHold on the blocked convoy (if no other blockers remain).
// Returns the number of cross-convoy dependency edges injected.
func ResolveFeatureBlockers(db *sql.DB, blockingFeatureID, newConvoyID int) int {
	rows, err := db.Query(`
		SELECT blocked_convoy_id FROM FeatureBlockers
		WHERE blocking_feature_id = ? AND resolved_at IS NULL`, blockingFeatureID)
	if err != nil {
		return 0
	}
	var blockedConvoyIDs []int
	for rows.Next() {
		var id int
		rows.Scan(&id)
		blockedConvoyIDs = append(blockedConvoyIDs, id)
	}
	rows.Close()

	tailIDs := GetConvoyTailTaskIDs(db, newConvoyID)
	injected := 0

	for _, blockedConvoyID := range blockedConvoyIDs {
		// Root tasks: Pending/Planned tasks in the blocked convoy with no existing deps.
		rootRows, err := db.Query(`
			SELECT id FROM BountyBoard
			WHERE convoy_id = ? AND status IN ('Pending', 'Planned')
			  AND id NOT IN (SELECT task_id FROM TaskDependencies)`, blockedConvoyID)
		if err != nil {
			continue
		}
		var rootIDs []int
		for rootRows.Next() {
			var id int
			rootRows.Scan(&id)
			rootIDs = append(rootIDs, id)
		}
		rootRows.Close()

		for _, rootID := range rootIDs {
			for _, tailID := range tailIDs {
				AddDependency(db, rootID, tailID)
				injected++
			}
		}

		// Mark blocker resolved.
		db.Exec(`UPDATE FeatureBlockers SET resolved_at = datetime('now')
		         WHERE blocked_convoy_id = ? AND blocking_feature_id = ?`,
			blockedConvoyID, blockingFeatureID)

		// Clear ConvoyHold only if no other unresolved blockers remain.
		var remaining int
		db.QueryRow(`SELECT COUNT(*) FROM FeatureBlockers
		             WHERE blocked_convoy_id = ? AND resolved_at IS NULL`, blockedConvoyID).Scan(&remaining)
		if remaining == 0 {
			ClearConvoyHold(db, blockedConvoyID)
		}
	}
	return injected
}

// GetUnresolvedBlockers returns all unresolved FeatureBlocker rows for a convoy.
func GetUnresolvedBlockers(db *sql.DB, blockedConvoyID int) []int {
	rows, err := db.Query(`
		SELECT blocking_feature_id FROM FeatureBlockers
		WHERE blocked_convoy_id = ? AND resolved_at IS NULL`, blockedConvoyID)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var ids []int
	for rows.Next() {
		var id int
		rows.Scan(&id)
		ids = append(ids, id)
	}
	return ids
}
