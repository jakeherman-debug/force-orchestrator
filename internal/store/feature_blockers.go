package store

import (
	"database/sql"
	"log"
)

// CreateFeatureBlocker records that blockedConvoyID cannot proceed until
// blockingFeatureID has a completed convoy. Also sets a ConvoyHold so the
// Captain and Council hard-reject in-flight tasks from the blocked convoy.
//
// Fix #3 (AUDIT-036): FeatureBlockers now carries a partial UNIQUE on
// (blocked_convoy_id, blocking_feature_id) WHERE resolved_at IS NULL, backing
// ON CONFLICT DO NOTHING so a retry of ResolveFeatureBlockers or any other
// duplicate-wiring code path cannot land two unresolved rows for the same pair.
func CreateFeatureBlocker(db *sql.DB, blockedConvoyID, blockingFeatureID int, reason string) {
	db.Exec(`INSERT INTO FeatureBlockers (blocked_convoy_id, blocking_feature_id)
	         VALUES (?, ?)
	         ON CONFLICT(blocked_convoy_id, blocking_feature_id) WHERE resolved_at IS NULL
	         DO NOTHING`,
		blockedConvoyID, blockingFeatureID)
	SetConvoyHold(db, blockedConvoyID, reason)
}

// ResolveFeatureBlockers is called when blockingFeatureID's convoy (newConvoyID) is
// approved by the Chancellor. It wires real TaskDependencies from the blocked convoy's
// root Pending/Planned tasks to the new convoy's tail tasks, then clears the blockers
// and any ConvoyHold on the blocked convoy (if no other blockers remain).
// Returns the number of cross-convoy dependency edges injected.
//
// AUDIT-069 (Fix #8d): the per-blocked-convoy sequence (AddDependency,
// UPDATE FeatureBlockers SET resolved_at, optional ClearConvoyHold) now
// runs inside a single tx. Pre-fix, a crash mid-sequence could leave the
// hold cleared but dependencies unwired — the operator saw a "ready"
// convoy with no blockers wired, and the blocked work ran out of order.
// The outer read phase (finding blocked_convoy_ids, tail ids, root ids)
// stays outside the tx: those are snapshots, and holding the single
// SQLite writer while iterating them would block every other writer.
func ResolveFeatureBlockers(db *sql.DB, blockingFeatureID, newConvoyID int) int {
	rows, err := db.Query(`
		SELECT blocked_convoy_id FROM FeatureBlockers
		WHERE blocking_feature_id = ? AND resolved_at IS NULL`, blockingFeatureID)
	if err != nil {
		log.Printf("ResolveFeatureBlockers: blocked_convoy_ids query failed: %v", err)
		return 0
	}
	var blockedConvoyIDs []int
	for rows.Next() {
		var id int
		if sErr := rows.Scan(&id); sErr != nil {
			log.Printf("ResolveFeatureBlockers: blocked_convoy_id scan failed: %v", sErr)
			continue
		}
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
			log.Printf("ResolveFeatureBlockers: root-ids query for convoy %d failed: %v", blockedConvoyID, err)
			continue
		}
		var rootIDs []int
		for rootRows.Next() {
			var id int
			if sErr := rootRows.Scan(&id); sErr != nil {
				log.Printf("ResolveFeatureBlockers: root-id scan failed: %v", sErr)
				continue
			}
			rootIDs = append(rootIDs, id)
		}
		rootRows.Close()

		// Atomic mutation for this blocked convoy: deps + blocker-resolve +
		// optional hold-clear all land together (or not at all).
		tx, err := db.Begin()
		if err != nil {
			log.Printf("ResolveFeatureBlockers: begin tx for convoy %d failed: %v", blockedConvoyID, err)
			continue
		}
		localInjected := 0
		var txErr error
		for _, rootID := range rootIDs {
			for _, tailID := range tailIDs {
				if depErr := AddDependencyTx(tx, rootID, tailID); depErr != nil {
					txErr = depErr
					break
				}
				localInjected++
			}
			if txErr != nil {
				break
			}
		}
		if txErr == nil {
			if _, uErr := tx.Exec(`UPDATE FeatureBlockers SET resolved_at = datetime('now')
				WHERE blocked_convoy_id = ? AND blocking_feature_id = ?`,
				blockedConvoyID, blockingFeatureID); uErr != nil {
				txErr = uErr
			}
		}
		if txErr == nil {
			var remaining int
			// Count remaining unresolved blockers EXCLUDING the row we just
			// updated inside the tx — the tx hasn't committed yet so the
			// count would otherwise still include it.
			if cErr := tx.QueryRow(`SELECT COUNT(*) FROM FeatureBlockers
				WHERE blocked_convoy_id = ? AND resolved_at IS NULL`, blockedConvoyID).Scan(&remaining); cErr != nil {
				txErr = cErr
			} else if remaining == 0 {
				if hErr := ClearConvoyHoldTx(tx, blockedConvoyID); hErr != nil {
					txErr = hErr
				}
			}
		}
		if txErr != nil {
			log.Printf("ResolveFeatureBlockers: tx for convoy %d failed mid-sequence: %v — rolling back", blockedConvoyID, txErr)
			_ = tx.Rollback()
			continue
		}
		if cErr := tx.Commit(); cErr != nil {
			log.Printf("ResolveFeatureBlockers: tx commit for convoy %d failed: %v", blockedConvoyID, cErr)
			continue
		}
		injected += localInjected
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
