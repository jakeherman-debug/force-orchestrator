package experiments

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"force-orchestrator/internal/analysis"
)

// TestShakedown_FactorialEndToEnd exercises the full Phase 4 factorial
// path end-to-end: author a 2x2 manifest → ratify → enroll synthetic
// units → seed deterministic per-cell scores → terminate → run the
// factorial analyzer (main effects + 2-way interactions) → assert
// DecideFactorialOutcome surfaces the seeded best cell as the winner
// with posterior > 0.95.
//
// The seeded cell-effect surface is non-additive only mildly so that
// the analyzer DOES declare a winner (instead of routing to
// significant_interaction). For a strong-interaction shakedown, see
// internal/analysis/factorial_analysis_test.go's
// Test2WayInteractions_StrongInteraction.
func TestShakedown_FactorialEndToEnd(t *testing.T) {
	db := openDB(t)
	ctx := context.Background()

	// 1. Author 2x2 factorial.
	const yaml2x2 = `
name: shakedown-factorial-2x2
hypothesis: prompt B with tight rules wins
kind: factorial
subject_agent: captain
assignment_unit: convoy
stakes_tier: low
factors:
  - name: prompt
    levels: [A, B]
  - name: rules
    levels: [tight, loose]
treatments:
  - arm_label: cell_A_tight
    prompt_template_ref: captain/A
    target_cell_weight: 0.25
    cell: {prompt: A, rules: tight}
  - arm_label: cell_A_loose
    prompt_template_ref: captain/A
    target_cell_weight: 0.25
    cell: {prompt: A, rules: loose}
  - arm_label: cell_B_tight
    prompt_template_ref: captain/B
    target_cell_weight: 0.25
    cell: {prompt: B, rules: tight}
  - arm_label: cell_B_loose
    prompt_template_ref: captain/B
    target_cell_weight: 0.25
    cell: {prompt: B, rules: loose}
metrics:
  - metric_name: approval_rate
    metric_version: "1"
    direction: higher_is_better
    is_primary: true
`
	expID, err := AuthorFactorialFromBytes(ctx, db, []byte(yaml2x2))
	if err != nil {
		t.Fatalf("AuthorFactorialFromBytes: %v", err)
	}

	// 2. Operator ratify.
	if err := Ratify(ctx, db, expID, "operator@upstart.com"); err != nil {
		t.Fatalf("Ratify: %v", err)
	}

	// 3. Enroll a balanced batch of synthetic units; the deterministic
	// cell picker spreads them across all four cells. 1200 / 4 ≈ 300
	// per cell with reasonable balance (sub-agent A's 1000-unit
	// shakedown observed each cell within 200..300, so 1200 lands at
	// ~300 ± tolerance).
	const totalUnits = 1200
	for unitID := 1; unitID <= totalUnits; unitID++ {
		if _, err := EnrollFactorialUnit(ctx, db, expID, "convoy", unitID); err != nil {
			t.Fatalf("EnrollFactorialUnit(%d): %v", unitID, err)
		}
	}

	// 4. Seed scores. The cell-effect surface is mildly non-additive
	// but not so strong that the analyzer routes to interaction:
	//   cell_A_tight: 0.55 (control region)
	//   cell_A_loose: 0.50
	//   cell_B_tight: 0.85 (clear winner)
	//   cell_B_loose: 0.65
	// Main effect of prompt (B-A): ~0.225
	// Main effect of rules (tight-loose): ~0.125
	// Interaction term: (0.85-0.55) - (0.65-0.50) = 0.30 - 0.15 = 0.15
	// (mild interaction; analyzer's DecideFactorialOutcome should still
	// declare cell_B_tight the winner because its posterior dominates
	// every other cell with >0.95 confidence at this sample size.)
	cellSuccessRate := map[string]float64{
		"cell_A_tight": 0.55,
		"cell_A_loose": 0.50,
		"cell_B_tight": 0.85,
		"cell_B_loose": 0.65,
	}
	if err := stampDeterministicScores(ctx, db, expID, cellSuccessRate); err != nil {
		t.Fatalf("stampDeterministicScores: %v", err)
	}

	// Sanity-check enrollment balance — every cell got at least 200
	// runs (1200 / 4 = 300 ideal, sub-agent A observed ≥200 tolerance).
	for arm := range cellSuccessRate {
		var n int
		if err := db.QueryRowContext(ctx, `
			SELECT COUNT(*)
			FROM ExperimentRuns r
			JOIN ExperimentTreatments t ON t.id = r.treatment_id
			WHERE r.experiment_id = ? AND t.arm_label = ?
		`, expID, arm).Scan(&n); err != nil {
			t.Fatalf("count cell %q: %v", arm, err)
		}
		if n < 200 {
			t.Errorf("cell %q: only %d enrollments (expect ≥200 of ~300)", arm, n)
		}
	}

	// 5. Terminate the factorial — writes ExperimentOutcomes with
	// per-cell means.
	if err := TerminateFactorial(ctx, db, expID, "shakedown_complete"); err != nil {
		t.Fatalf("TerminateFactorial: %v", err)
	}

	// 6. Per-cell outcome row landed.
	var outcomeReason, cellMeansJSON string
	if err := db.QueryRowContext(ctx, `
		SELECT termination_reason, IFNULL(cell_means_json, '{}')
		FROM ExperimentOutcomes WHERE experiment_id = ?
	`, expID).Scan(&outcomeReason, &cellMeansJSON); err != nil {
		t.Fatalf("SELECT ExperimentOutcomes: %v", err)
	}
	if outcomeReason != "shakedown_complete" {
		t.Errorf("ExperimentOutcomes.termination_reason: got %q want %q", outcomeReason, "shakedown_complete")
	}
	if !strings.Contains(cellMeansJSON, "prompt=A,rules=tight") || !strings.Contains(cellMeansJSON, "prompt=B,rules=tight") {
		t.Errorf("cell_means_json missing per-cell entries: %s", cellMeansJSON)
	}

	// 7. Run the factorial analyzer — main effects + 2-way interactions.
	mains, err := analysis.ComputeMainEffects(ctx, db, expID)
	if err != nil {
		t.Fatalf("ComputeMainEffects: %v", err)
	}
	if len(mains) == 0 {
		t.Fatalf("ComputeMainEffects returned no rows — factorial analyzer not engaging")
	}

	inters, err := analysis.Compute2WayInteractions(ctx, db, expID)
	if err != nil {
		t.Fatalf("Compute2WayInteractions: %v", err)
	}
	// 2x2 with one factor pair (prompt, rules) yields one interaction
	// row at the canonical first-level contrast.
	if len(inters) == 0 {
		t.Fatalf("Compute2WayInteractions returned no rows — interactions analyzer not engaging")
	}

	// 8. ExperimentInteractions populated.
	var interRowCount int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM ExperimentInteractions WHERE experiment_id = ?`, expID).Scan(&interRowCount); err != nil {
		t.Fatalf("count ExperimentInteractions: %v", err)
	}
	if interRowCount == 0 {
		t.Fatalf("ExperimentInteractions empty after Compute2WayInteractions — persistence broken")
	}

	// 9. DecideFactorialOutcome declares cell_B_tight the winner.
	rule := analysis.DecisionRule{
		PriorAlpha:        1.0,
		PriorBeta:         1.0,
		MinSamplesPerArm:  100,
		WinnerThreshold:   0.95,
		MonteCarloSamples: 50000,
		RandomSeed:        0xD3B7B1003,
	}
	decision, err := analysis.DecideFactorialOutcome(ctx, db, expID, rule)
	if err != nil {
		t.Fatalf("DecideFactorialOutcome: %v", err)
	}
	if decision.Reason != "declared_winner" {
		t.Fatalf("DecideFactorialOutcome.Reason: got %q want %q (interactions=%d, posterior=%.4f, winner=%v)",
			decision.Reason, "declared_winner",
			len(decision.SignificantInteractions),
			decision.BestCellPosterior,
			decision.BestCell)
	}
	if decision.BestCell["prompt"] != "B" || decision.BestCell["rules"] != "tight" {
		t.Errorf("BestCell: got %v want {prompt:B, rules:tight}", decision.BestCell)
	}
	if decision.BestCellPosterior <= 0.95 {
		t.Errorf("BestCellPosterior: got %.4f want > 0.95", decision.BestCellPosterior)
	}
}

// stampDeterministicScores writes ExperimentRuns.score values so each
// cell's observed success rate matches the requested target. The score
// flips between 1.0 and 0.0 in id-order so the realised rate equals
// floor(target * n) / n — deterministic and re-running the test never
// trips the analyzer's Monte Carlo on a different seed-of-data.
func stampDeterministicScores(
	ctx context.Context,
	db *sql.DB,
	experimentID int,
	cellRates map[string]float64,
) error {
	for arm, rate := range cellRates {
		// Find the treatment id for this arm.
		var treatmentID int
		if err := db.QueryRowContext(ctx, `
			SELECT id FROM ExperimentTreatments
			WHERE experiment_id = ? AND arm_label = ?
		`, experimentID, arm).Scan(&treatmentID); err != nil {
			return fmt.Errorf("stampDeterministicScores: lookup arm %q: %w", arm, err)
		}

		// Order runs by id; flip 1/0 by index so cumulative rate
		// converges deterministically to the target.
		rows, err := db.QueryContext(ctx, `
			SELECT id FROM ExperimentRuns
			WHERE experiment_id = ? AND treatment_id = ?
			ORDER BY id
		`, experimentID, treatmentID)
		if err != nil {
			return fmt.Errorf("stampDeterministicScores: load runs for %q: %w", arm, err)
		}
		var ids []int
		for rows.Next() {
			var id int
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return fmt.Errorf("stampDeterministicScores: scan id: %w", err)
			}
			ids = append(ids, id)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return fmt.Errorf("stampDeterministicScores: rows: %w", err)
		}

		successesNeeded := int(float64(len(ids))*rate + 0.5)
		for i, id := range ids {
			score := 0.0
			if i < successesNeeded {
				score = 1.0
			}
			if _, err := db.ExecContext(ctx, `
				UPDATE ExperimentRuns
				SET score = ?, score_source = 'shakedown_synthetic', completed_at = datetime('now')
				WHERE id = ?
			`, score, id); err != nil {
				return fmt.Errorf("stampDeterministicScores: update run %d: %w", id, err)
			}
		}
	}
	return nil
}

// jsonStringMap unmarshals a JSON object into a map for cell_means
// inspection. Used only by this test; placed here rather than in a
// shared helpers file to keep the shakedown self-contained.
//
//nolint:unused // future shakedown extensions will inspect the cell_means_json shape
func jsonStringMap(t *testing.T, raw string) map[string]float64 {
	t.Helper()
	out := map[string]float64{}
	if raw == "" || raw == "{}" {
		return out
	}
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		t.Fatalf("jsonStringMap(%q): %v", raw, err)
	}
	return out
}
