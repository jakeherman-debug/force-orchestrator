// Package store — D4 Phase 0 — FleetMemory quality scoring.
//
// Three scoring axes per memory row:
//
//  1. freshness_score (REAL, 0..1) — how recent is this memory?
//     RecomputeFreshnessScores decays the score exponentially with row
//     age; called by the librarian-quality-recompute dog every 24h.
//     A row at age 0 has score 1.0; at age = halflife it has 0.5;
//     at age = 4*halflife it has ~0.0625.
//
//  2. validation_score (REAL, -1..1) — does this memory lead to
//     successful outcomes when injected? RecordValidation adjusts the
//     score by a fixed delta on each positive/negative signal. The
//     score is clamped to [-1, 1] so a long sequence of one-sided
//     feedback can't run away.
//
//  3. retrieval_count (INT) + last_retrieved_at — how often is this
//     memory consulted? RecordRetrieval bumps the count and stamps
//     the timestamp. Used by EmitHypothesisCandidates to decide
//     which memories are signal-rich enough to surface as
//     hypotheses.
//
// All three helpers are explicitly NOT silent-failing: store-mutator
// errors return up to the caller (CLAUDE.md "No silent failures").
package store

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"time"
)

// FreshnessHalfLife is the time after which an unmodified memory's
// freshness_score halves. Default 30 days — this is calibrated so a
// memory written today still ranks above a 90-day-old memory by a
// factor of ~8x, which is roughly the operator-tested "stale" window
// (decisions that were correct 3 months ago are usually no longer
// the most-relevant lesson).
//
// Exported so tests can shrink the half-life to milliseconds for
// deterministic decay verification.
var FreshnessHalfLife = 30 * 24 * time.Hour

// ValidationDelta is the per-signal nudge applied to validation_score
// by RecordValidation. Default 0.05 — a memory needs ~20 positive
// signals to saturate at +1.0, which roughly maps to "consulted
// in 20 successful tasks" — the threshold we've informally
// considered "Librarian has high confidence in this memory."
var ValidationDelta = 0.05

// ValidationOutcome distinguishes positive vs negative validation
// signals for RecordValidation.
type ValidationOutcome string

const (
	ValidationPositive ValidationOutcome = "positive"
	ValidationNegative ValidationOutcome = "negative"
)

// RecomputeFreshnessScores walks the FleetMemory table and updates
// every row's freshness_score based on its age relative to
// FreshnessHalfLife. Returns the number of rows updated. Does not
// touch rows that are already at the computed score (idempotent).
//
// Why not store the decay coefficient and let read sites compute it?
// Two reasons: (1) the dashboard "weighted memories" view sorts by
// composite score, and pushing the multiplier into every read query
// is more expensive than a once-per-day batch update; (2) the dog
// surface is the canonical recompute trigger, so a single source of
// truth for the score lives in the column itself.
func RecomputeFreshnessScores(ctx context.Context, db *sql.DB) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	rows, err := db.QueryContext(ctx, `
		SELECT id, IFNULL(created_at, ''), IFNULL(freshness_score, 1.0)
		  FROM FleetMemory`)
	if err != nil {
		return 0, fmt.Errorf("RecomputeFreshnessScores: query: %w", err)
	}
	type rowSnap struct {
		id        int
		createdAt string
		current   float64
	}
	var all []rowSnap
	for rows.Next() {
		var r rowSnap
		if err := rows.Scan(&r.id, &r.createdAt, &r.current); err != nil {
			rows.Close()
			return 0, fmt.Errorf("RecomputeFreshnessScores: scan: %w", err)
		}
		all = append(all, r)
	}
	if rerr := rows.Err(); rerr != nil {
		rows.Close()
		return 0, fmt.Errorf("RecomputeFreshnessScores: rows iter: %w", rerr)
	}
	rows.Close()

	now := time.Now().UTC()
	halfLifeSeconds := FreshnessHalfLife.Seconds()
	updated := 0
	for _, r := range all {
		if r.createdAt == "" {
			continue
		}
		t, perr := ParseSQLiteTime(r.createdAt)
		if perr != nil {
			// Legacy rows pre-time-fix may have an unparseable shape;
			// skip rather than error out so one weird row can't fail
			// the whole recompute.
			continue
		}
		ageSeconds := now.Sub(t).Seconds()
		if ageSeconds < 0 {
			ageSeconds = 0
		}
		score := math.Pow(0.5, ageSeconds/halfLifeSeconds)
		// Idempotence: skip a no-op write if the score is already
		// approximately equal (within 1e-6).
		if math.Abs(score-r.current) < 1e-6 {
			continue
		}
		if _, err := db.ExecContext(ctx,
			`UPDATE FleetMemory SET freshness_score = ? WHERE id = ?`,
			score, r.id); err != nil {
			return updated, fmt.Errorf("RecomputeFreshnessScores: update id=%d: %w", r.id, err)
		}
		updated++
	}
	return updated, nil
}

// RecordRetrieval bumps retrieval_count and stamps last_retrieved_at
// for the given FleetMemory row. Called by the agent ingress path
// every time a memory is injected into a prompt (Pattern P33's
// graduation moves the call site into Librarian's GetWeightedMemories,
// so existing direct-store callers don't need to thread it).
//
// Returns ErrNotFound if no row matched the given id (defensive
// against a memory deleted between retrieval and bookkeeping).
func RecordRetrieval(ctx context.Context, db *sql.DB, memoryID int) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	res, err := db.ExecContext(ctx, `
		UPDATE FleetMemory
		   SET retrieval_count = IFNULL(retrieval_count, 0) + 1,
		       last_retrieved_at = datetime('now')
		 WHERE id = ?`, memoryID)
	if err != nil {
		return fmt.Errorf("RecordRetrieval: update: %w", err)
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("RecordRetrieval: rows-affected: %w", err)
	}
	if rows == 0 {
		return ErrFleetMemoryNotFound
	}
	return nil
}

// RecordValidation nudges a memory's validation_score by
// ValidationDelta in the direction of the supplied outcome
// (positive → +delta, negative → -delta). Result clamped to [-1, 1].
// The intended call site is the council/captain post-task hook: a
// successful task whose context included memory M registers a
// positive validation signal against M; a failed task registers
// negative. Concretely Phase 3 / agent ingress migration wires this
// into the council finalisation.
//
// Returns ErrNotFound if no row matched.
func RecordValidation(ctx context.Context, db *sql.DB, memoryID int, outcome ValidationOutcome) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	delta := ValidationDelta
	if outcome == ValidationNegative {
		delta = -ValidationDelta
	} else if outcome != ValidationPositive {
		return fmt.Errorf("RecordValidation: unknown outcome %q (want %q or %q)",
			outcome, ValidationPositive, ValidationNegative)
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("RecordValidation: begin: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	var current float64
	if err := tx.QueryRowContext(ctx,
		`SELECT IFNULL(validation_score, 0.0) FROM FleetMemory WHERE id = ?`, memoryID).
		Scan(&current); err != nil {
		if err == sql.ErrNoRows {
			return ErrFleetMemoryNotFound
		}
		return fmt.Errorf("RecordValidation: read: %w", err)
	}
	next := current + delta
	if next > 1.0 {
		next = 1.0
	}
	if next < -1.0 {
		next = -1.0
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE FleetMemory SET validation_score = ? WHERE id = ?`,
		next, memoryID); err != nil {
		return fmt.Errorf("RecordValidation: update: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("RecordValidation: commit: %w", err)
	}
	return nil
}

// ErrFleetMemoryNotFound is returned by RecordRetrieval / RecordValidation
// when the row id doesn't match anything. Sentinel so callers can
// distinguish "the row was deleted" from "the DB is broken."
var ErrFleetMemoryNotFound = fmt.Errorf("fleet memory: row not found")
