package agents

import (
	"database/sql"
	"fmt"

	"force-orchestrator/internal/store"
)

// backfillMissingAskBranches is the Layer C lazy-backfill inquisitor check.
//
// For every Active convoy with at least one touched repo lacking a ConvoyAskBranch
// row, it enqueues a single CreateAskBranch task. The CreateAskBranch handler
// then fans out per-repo branch creation for the convoy (idempotent — repos
// that already have a branch are skipped).
//
// Multi-repo correctness: a convoy with repos [api, monolith] where api has a
// branch but monolith doesn't IS returned by ActiveConvoysMissingAskBranch, so
// CreateAskBranch runs and creates the missing monolith branch.
//
// Guards against queuing duplicates: if a CreateAskBranch task for the same
// convoy is already Pending or Locked, skip this tick.
//
// This check runs every Inquisitor cycle (5 min) but the guards make it a
// cheap no-op when there's nothing to do.
func backfillMissingAskBranches(db *sql.DB, logger interface{ Printf(string, ...any) }) {
	candidates := store.ActiveConvoysMissingAskBranch(db)
	if len(candidates) == 0 {
		return
	}

	for _, cid := range candidates {
		// Skip if a CreateAskBranch task for this convoy is already queued
		// or in-flight. JSON-boundary-matched to avoid false-positives.
		var existing int
		db.QueryRow(`SELECT COUNT(*) FROM BountyBoard
			WHERE type = 'CreateAskBranch' AND status IN ('Pending', 'Locked')
			  AND (payload LIKE '%"convoy_id":' || ? || ',%'
			    OR payload LIKE '%"convoy_id":' || ? || '}%')`,
			cid, cid).Scan(&existing)
		if existing > 0 {
			continue
		}

		taskID, qErr := QueueCreateAskBranch(db, cid)
		if qErr != nil {
			logger.Printf("backfillMissingAskBranches: convoy %d: queue failed: %v", cid, qErr)
			continue
		}
		logger.Printf("backfillMissingAskBranches: queued CreateAskBranch #%d for convoy %d", taskID, cid)
	}
}

// Unused sentinels kept so the imports can be pruned without warnings; remove
// if/when the file grows real consumers.
var _ = fmt.Sprintf
