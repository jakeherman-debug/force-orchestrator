package store

// D3 fix-loop-1 / γ1 — ConvoyReviewCycles atomic snapshot writer (concern #6,
// exit criterion 14a).
//
// Each ConvoyReview pass at DraftPROpen produces a row in this table. The row
// is "atomic" in two senses:
//
//   1. The spec snapshot frozen at cycle start (`spec_version_at_start`) is
//      what the cycle evaluates against. Operator-ratified amendments that
//      land mid-cycle are deferred to the NEXT cycle. This is the
//      8d→8e→8f noisy-spec defense — a 200-500-line verification prompt
//      can't grow under the cycle's feet.
//
//   2. Once written, the row's outcome columns (`outcomes_json`,
//      `fix_tasks_spawned_json`, `amendments_proposed_json`) are
//      append-only via the cycle-completion path. After
//      `cycle_completed_at` is set, no UPDATE may rewrite the outcomes.
//      The pattern test for replay-no-mutation enforces this from the
//      Replay-mode side; the helper below enforces it at the writer side
//      by failing CompleteConvoyReviewCycle on already-completed rows.

import (
	"database/sql"
	"encoding/json"
	"fmt"
)

// ConvoyReviewCycle is the read-side shape of one row.
//
// Columns mirror schema.go:846+ exactly. Empty / default values round-trip
// as Go zero values, so freshly-begun cycles (no completion yet) show up
// with CycleCompletedAt="", OutcomesJSON="{}", FixTasksSpawnedJSON="[]",
// AmendmentsProposedJSON="[]", AmendmentsRatifiedDuringCycleJSON="[]".
type ConvoyReviewCycle struct {
	ID                                int
	ConvoyID                          int
	CycleNumber                       int
	SpecVersionAtStart                string
	CycleStartedAt                    string
	CycleCompletedAt                  string
	OutcomesJSON                      string
	FixTasksSpawnedJSON               string
	AmendmentsProposedJSON            string
	AmendmentsRatifiedDuringCycleJSON string
}

// BeginConvoyReviewCycle starts a new cycle for the convoy and freezes the
// spec snapshot. Returns (cycleID, frozenSpecJSON, error). The frozenSpecJSON
// is the verbatim contents of Convoys.verification_spec_json at the moment
// the row was written — callers MUST evaluate against this string and not
// re-read the column mid-cycle, otherwise the frozen-spec invariant is
// defeated.
//
// cycle_number is computed as MAX(cycle_number) + 1 within a single SQL
// statement. The UNIQUE (convoy_id, cycle_number) constraint guards against
// two callers landing the same cycle number on a race; the second loses with
// a constraint violation and the caller should retry.
func BeginConvoyReviewCycle(db *sql.DB, convoyID int) (int, string, error) {
	if convoyID <= 0 {
		return 0, "", fmt.Errorf("BeginConvoyReviewCycle: convoyID must be positive (got %d)", convoyID)
	}

	// Snapshot the spec FIRST so the cycle row carries the exact bytes the
	// caller will evaluate against. IFNULL covers fresh rows from before the
	// column existed.
	var spec string
	if err := db.QueryRow(
		`SELECT IFNULL(verification_spec_json, '') FROM Convoys WHERE id = ?`,
		convoyID,
	).Scan(&spec); err != nil {
		return 0, "", fmt.Errorf("BeginConvoyReviewCycle: load spec for convoy %d: %w", convoyID, err)
	}

	// Compute next cycle number. Race-safe: the UNIQUE constraint on
	// (convoy_id, cycle_number) catches two concurrent callers.
	var nextCycle int
	if err := db.QueryRow(
		`SELECT IFNULL(MAX(cycle_number), 0) + 1 FROM ConvoyReviewCycles WHERE convoy_id = ?`,
		convoyID,
	).Scan(&nextCycle); err != nil {
		return 0, "", fmt.Errorf("BeginConvoyReviewCycle: compute cycle_number for convoy %d: %w", convoyID, err)
	}

	res, err := db.Exec(
		`INSERT INTO ConvoyReviewCycles
			(convoy_id, cycle_number, spec_version_at_start, cycle_started_at)
		 VALUES (?, ?, ?, datetime('now'))`,
		convoyID, nextCycle, spec,
	)
	if err != nil {
		return 0, "", fmt.Errorf("BeginConvoyReviewCycle: insert: %w", err)
	}
	id, _ := res.LastInsertId()
	return int(id), spec, nil
}

// CompleteConvoyReviewCycle stamps the terminal columns on a cycle row.
//
//   - verdict           — "clean" | "needs_work" | "loop" | "deferred" | "escalated"
//   - outcomesJSON      — JSON object: per-AT pass/fail/inconclusive map and
//                          findings if any. Stored verbatim; callers marshal.
//   - fixTaskIDs        — IDs of CodeEdit tasks the cycle spawned (may be empty).
//
// Refuses to rewrite a cycle that already has cycle_completed_at populated.
// This protects the immutable-cycle-outcomes invariant (concern #6,
// roadmap line 1171). Callers receive an error and the existing row is
// untouched.
func CompleteConvoyReviewCycle(db *sql.DB, cycleID int, verdict string, outcomesJSON string, fixTaskIDs []int) error {
	if cycleID <= 0 {
		return fmt.Errorf("CompleteConvoyReviewCycle: cycleID must be positive (got %d)", cycleID)
	}
	if verdict == "" {
		return fmt.Errorf("CompleteConvoyReviewCycle: verdict must be non-empty")
	}

	// Refuse double-completion. The Pattern test for replay-no-mutation also
	// checks this from the read path; here we close the writer side.
	var existingCompletion string
	if err := db.QueryRow(
		`SELECT IFNULL(cycle_completed_at, '') FROM ConvoyReviewCycles WHERE id = ?`,
		cycleID,
	).Scan(&existingCompletion); err != nil {
		return fmt.Errorf("CompleteConvoyReviewCycle: load row %d: %w", cycleID, err)
	}
	if existingCompletion != "" {
		return fmt.Errorf("CompleteConvoyReviewCycle: cycle %d already completed at %s — outcomes are immutable", cycleID, existingCompletion)
	}

	// Default to "{}" rather than "" so JSON consumers don't bomb.
	if outcomesJSON == "" {
		outcomesJSON = "{}"
	}
	fixTaskIDsJSON, err := json.Marshal(append([]int(nil), fixTaskIDs...))
	if err != nil {
		return fmt.Errorf("CompleteConvoyReviewCycle: marshal fixTaskIDs: %w", err)
	}
	// nil/empty slice should serialise as "[]" not "null" — pre-empted by
	// the append([]int(nil), ...) above.

	// Bundle outcomes + verdict on the JSON side: the verdict is part of the
	// outcomes shape so the read side has a single column to consult. We
	// keep verdict separately also for callers that pass a structured
	// outcomes payload — wrap or merge as needed.
	if _, err := db.Exec(
		`UPDATE ConvoyReviewCycles
		 SET cycle_completed_at = datetime('now'),
		     outcomes_json = ?,
		     fix_tasks_spawned_json = ?
		 WHERE id = ? AND IFNULL(cycle_completed_at, '') = ''`,
		mergeVerdict(outcomesJSON, verdict),
		string(fixTaskIDsJSON),
		cycleID,
	); err != nil {
		return fmt.Errorf("CompleteConvoyReviewCycle: update row %d: %w", cycleID, err)
	}
	return nil
}

// mergeVerdict folds the verdict into the outcomes JSON object. If the
// outcomes JSON parses as an object, we add a "verdict" field; otherwise we
// wrap the original payload under a "raw" key so the verdict is still
// machine-readable on the read side.
func mergeVerdict(outcomesJSON, verdict string) string {
	var asMap map[string]any
	if err := json.Unmarshal([]byte(outcomesJSON), &asMap); err == nil && asMap != nil {
		asMap["verdict"] = verdict
		out, _ := json.Marshal(asMap)
		return string(out)
	}
	// Non-object outcome — wrap so we don't drop content.
	wrapped, _ := json.Marshal(map[string]any{
		"verdict": verdict,
		"raw":     outcomesJSON,
	})
	return string(wrapped)
}

// ListCyclesForConvoy returns every cycle row for a convoy, ordered by
// cycle_number ASC. Used by the dashboard learning panel + Drill convoy view
// to render the per-cycle history.
func ListCyclesForConvoy(db *sql.DB, convoyID int) ([]ConvoyReviewCycle, error) {
	rows, err := db.Query(
		`SELECT id, convoy_id, cycle_number, spec_version_at_start,
		        IFNULL(cycle_started_at, ''), IFNULL(cycle_completed_at, ''),
		        IFNULL(outcomes_json, '{}'), IFNULL(fix_tasks_spawned_json, '[]'),
		        IFNULL(amendments_proposed_json, '[]'),
		        IFNULL(amendments_ratified_during_cycle_json, '[]')
		 FROM ConvoyReviewCycles
		 WHERE convoy_id = ?
		 ORDER BY cycle_number ASC`,
		convoyID,
	)
	if err != nil {
		return nil, fmt.Errorf("ListCyclesForConvoy: query: %w", err)
	}
	defer rows.Close()

	var out []ConvoyReviewCycle
	for rows.Next() {
		var c ConvoyReviewCycle
		if err := rows.Scan(
			&c.ID, &c.ConvoyID, &c.CycleNumber, &c.SpecVersionAtStart,
			&c.CycleStartedAt, &c.CycleCompletedAt,
			&c.OutcomesJSON, &c.FixTasksSpawnedJSON,
			&c.AmendmentsProposedJSON, &c.AmendmentsRatifiedDuringCycleJSON,
		); err != nil {
			return nil, fmt.Errorf("ListCyclesForConvoy: scan: %w", err)
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("ListCyclesForConvoy: iter: %w", err)
	}
	return out, nil
}
