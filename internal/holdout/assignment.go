package holdout

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// IsInHoldout returns true iff the (kind, id) natural unit is a
// member of the holdout at the current wall-clock time. The function
// is the canonical entry point used by treatments.Apply: a single DB
// read + a stable hash + the time-keyed CurrentFraction comparison.
//
// The decision is deterministic in (holdoutID, kind, id, now) — the
// (holdoutID, kind, id) hash is fixed per the contract documented in
// hashFraction; only the time component varies, and only during the
// ramp-up / fade phases (steady-state plateau is constant). Because
// the hash is content-addressed by holdout id, minting a new holdout
// produces an entirely fresh assignment — no implicit grandfathering.
func IsInHoldout(ctx context.Context, db *sql.DB, holdoutID int, naturalUnitKind string, naturalUnitID int) (bool, error) {
	return IsInHoldoutAt(ctx, db, holdoutID, naturalUnitKind, naturalUnitID, time.Now().UTC())
}

// IsInHoldoutAt is the testable shape of IsInHoldout: it takes the
// time argument explicitly so test fixtures can pin "today is 3 days
// after reference_date" without monkey-patching the clock.
func IsInHoldoutAt(ctx context.Context, db *sql.DB, holdoutID int, naturalUnitKind string, naturalUnitID int, now time.Time) (bool, error) {
	h, err := LoadHoldout(ctx, db, holdoutID)
	if err != nil {
		return false, fmt.Errorf("IsInHoldoutAt: load holdout %d: %w", holdoutID, err)
	}
	frac := h.CurrentFraction(now)
	if frac <= 0 {
		return false, nil
	}
	return hashFraction(holdoutID, naturalUnitKind, naturalUnitID) < frac, nil
}

// IsInHoldoutWithSnapshot is the hot-path variant: caller has already
// loaded the Holdout row (e.g. cached at daemon startup) and only
// wants to evaluate membership. Pure function over (h, kind, id, now).
func IsInHoldoutWithSnapshot(h Holdout, naturalUnitKind string, naturalUnitID int, now time.Time) bool {
	frac := h.CurrentFraction(now)
	if frac <= 0 {
		return false
	}
	return hashFraction(h.ID, naturalUnitKind, naturalUnitID) < frac
}
