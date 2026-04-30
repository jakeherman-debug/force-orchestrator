// Package holdout implements the global-holdout discipline from
// paired-runs.md § Global Holdout: a small, indefinitely-frozen
// reference cohort that lets analysts compare the current fleet's
// behaviour against a fixed configuration over time.
//
// Two responsibilities live here:
//
//   1. Mint — MintBaseline2026 inserts the baseline-2026 row into
//      GlobalHoldouts. Idempotent on the UNIQUE name index; a re-run
//      finds the row and returns its existing id.
//
//   2. Assignment — IsInHoldout maps (holdoutID, naturalUnitKind,
//      naturalUnitID) onto a deterministic boolean: same input, same
//      output, indefinitely. The "decided once, never re-hashed"
//      property of paired-runs.md § Holdout inheritance is a property
//      of the CALLER (Features writes the bool once at creation, then
//      child units inherit), not this function — IsInHoldout is a
//      pure decision and may be re-called any number of times with the
//      same answer.
package holdout

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"fmt"
	"time"

	"force-orchestrator/internal/store"
)

// BaselineHoldoutName is the canonical name of the first global
// holdout (paired-runs.md § Global Holdout § Lifecycle).
const BaselineHoldoutName = "baseline-2026"

// MintBaseline2026 inserts the baseline-2026 row into GlobalHoldouts,
// returning its id. Idempotent: a re-call finds the existing row by
// name and returns its id without changing it.
//
// The row's parameters match paired-runs.md defaults: ramp_up_days=7,
// plateau_fraction=0.02, fade_days=90, fade_start_at NULL (operator
// retires by setting it). reference_date is captured at mint time as
// the canonical UTC SQLite-format string so the
// `current_fraction(holdout, t)` math (in IsInHoldoutAt) is anchored.
//
// The fleet_state_hash is left blank in this mint — Phase 2 ships
// the holdout substrate, but FleetStateSnapshots is not populated by
// any other agent yet (the dog that computes snapshots lands in a
// later phase). When that arrives, the mint can be re-run and the
// hash backfilled by an explicit operator command.
func MintBaseline2026(ctx context.Context, db *sql.DB) (int, error) {
	if db == nil {
		return 0, fmt.Errorf("MintBaseline2026: db is nil")
	}

	var existingID int
	err := db.QueryRowContext(ctx,
		`SELECT id FROM GlobalHoldouts WHERE name = ?`,
		BaselineHoldoutName,
	).Scan(&existingID)
	switch {
	case err == sql.ErrNoRows:
		// Brand new — insert below.
	case err != nil:
		return 0, fmt.Errorf("MintBaseline2026: lookup existing: %w", err)
	default:
		return existingID, nil
	}

	const notes = "Baseline holdout for D3 paired-runs experimentation. " +
		"2% indefinite plateau, 7d ramp-up, 90d fade once retired. " +
		"Minted by D3 Phase 2."
	res, err := db.ExecContext(ctx, `
		INSERT INTO GlobalHoldouts
			(name, reference_date, fleet_state_hash,
			 ramp_up_days, plateau_fraction, fade_days,
			 created_by, notes)
		VALUES (?, ?, '', 7, 0.02, 90, ?, ?)
	`,
		BaselineHoldoutName,
		store.NowSQLite(),
		"operator:jake.herman@upstart.com",
		notes,
	)
	if err != nil {
		return 0, fmt.Errorf("MintBaseline2026: insert: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("MintBaseline2026: last insert id: %w", err)
	}
	return int(id), nil
}

// Holdout is the in-memory shape of a GlobalHoldouts row. Loaded by
// LoadHoldout / LoadHoldoutByName and consumed by IsInHoldout's
// time-aware fraction math.
type Holdout struct {
	ID              int
	Name            string
	ReferenceDate   time.Time
	RampUpDays      int
	PlateauFraction float64
	FadeStartAt     time.Time // zero value means "not yet faded"
	FadeDays        int
	RetiredAt       time.Time // zero value means "still active"
}

// LoadHoldout reads a GlobalHoldouts row by id.
func LoadHoldout(ctx context.Context, db *sql.DB, holdoutID int) (Holdout, error) {
	return scanHoldoutRow(db.QueryRowContext(ctx, `
		SELECT id, name,
		       IFNULL(reference_date, ''),
		       IFNULL(ramp_up_days, 0),
		       IFNULL(plateau_fraction, 0),
		       IFNULL(fade_start_at, ''),
		       IFNULL(fade_days, 0),
		       IFNULL(retired_at, '')
		FROM GlobalHoldouts WHERE id = ?
	`, holdoutID))
}

// LoadHoldoutByName reads a GlobalHoldouts row by its UNIQUE name.
func LoadHoldoutByName(ctx context.Context, db *sql.DB, name string) (Holdout, error) {
	return scanHoldoutRow(db.QueryRowContext(ctx, `
		SELECT id, name,
		       IFNULL(reference_date, ''),
		       IFNULL(ramp_up_days, 0),
		       IFNULL(plateau_fraction, 0),
		       IFNULL(fade_start_at, ''),
		       IFNULL(fade_days, 0),
		       IFNULL(retired_at, '')
		FROM GlobalHoldouts WHERE name = ?
	`, name))
}

func scanHoldoutRow(row *sql.Row) (Holdout, error) {
	var h Holdout
	var refDate, fadeStart, retiredAt string
	if err := row.Scan(&h.ID, &h.Name, &refDate, &h.RampUpDays, &h.PlateauFraction, &fadeStart, &h.FadeDays, &retiredAt); err != nil {
		if err == sql.ErrNoRows {
			return Holdout{}, fmt.Errorf("holdout row not found")
		}
		return Holdout{}, fmt.Errorf("scan holdout row: %w", err)
	}
	if refDate != "" {
		t, err := store.ParseSQLiteTime(refDate)
		if err != nil {
			return Holdout{}, fmt.Errorf("parse reference_date %q: %w", refDate, err)
		}
		h.ReferenceDate = t
	}
	if fadeStart != "" {
		t, err := store.ParseSQLiteTime(fadeStart)
		if err == nil {
			h.FadeStartAt = t
		}
	}
	if retiredAt != "" {
		t, err := store.ParseSQLiteTime(retiredAt)
		if err == nil {
			h.RetiredAt = t
		}
	}
	return h, nil
}

// CurrentFraction returns the fraction of natural units that should
// be in the holdout at time t. Implements paired-runs.md § Global
// Holdout § Lifecycle:
//
//	if t < reference_date:                              return 0
//	if t < reference_date + ramp_up_days:               linear ramp
//	if fade_start_at is null or t < fade_start_at:      plateau_fraction
//	if t < fade_start_at + fade_days:                   linear fade
//	otherwise:                                          0
func (h Holdout) CurrentFraction(t time.Time) float64 {
	if !h.RetiredAt.IsZero() && !t.Before(h.RetiredAt) {
		return 0
	}
	if t.Before(h.ReferenceDate) {
		return 0
	}
	rampDuration := time.Duration(h.RampUpDays) * 24 * time.Hour
	rampEnd := h.ReferenceDate.Add(rampDuration)
	if t.Before(rampEnd) {
		if h.RampUpDays <= 0 {
			return h.PlateauFraction
		}
		elapsed := t.Sub(h.ReferenceDate).Hours() / 24
		return h.PlateauFraction * (elapsed / float64(h.RampUpDays))
	}
	if h.FadeStartAt.IsZero() || t.Before(h.FadeStartAt) {
		return h.PlateauFraction
	}
	fadeDuration := time.Duration(h.FadeDays) * 24 * time.Hour
	fadeEnd := h.FadeStartAt.Add(fadeDuration)
	if t.Before(fadeEnd) {
		if h.FadeDays <= 0 {
			return 0
		}
		elapsed := t.Sub(h.FadeStartAt).Hours() / 24
		return h.PlateauFraction * (1 - elapsed/float64(h.FadeDays))
	}
	return 0
}

// hashFraction maps (holdoutID, kind, id) onto a stable float in
// [0, 1). The hash domain is SHA-256 over fmt.Sprintf("%d:%s:%d", …);
// the first 8 bytes are interpreted as a big-endian uint64 and divided
// by 2^64 to land in [0, 1). The choice of bytes / endianness is part
// of the contract: changing it reassigns every existing membership and
// breaks comparability — never edit without minting a fresh holdout.
func hashFraction(holdoutID int, kind string, id int) float64 {
	key := fmt.Sprintf("%d:%s:%d", holdoutID, kind, id)
	sum := sha256.Sum256([]byte(key))
	v := binary.BigEndian.Uint64(sum[:8])
	// Divide by 2^64 — keep the high bits (a uint64 value of 0 maps
	// to 0.0; the unreachable 2^64 would map to 1.0).
	return float64(v) / 18446744073709551616.0
}
