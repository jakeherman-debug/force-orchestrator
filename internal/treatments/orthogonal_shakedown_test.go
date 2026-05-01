package treatments

import (
	"context"
	"testing"
)

// TestShakedown_OrthogonalOverlap verifies the multi-experiment
// orthogonal-selection contract end-to-end at the treatments.Apply
// layer. Three running experiments are seeded against the same
// (subject_agent, assignment_unit):
//
//   - A: factors={prompt}
//   - B: factors={prompt} — conflicts with A on the "prompt" factor
//   - C: factors={rules} — orthogonal to both A and B
//
// For each synthetic unit that flows through Apply, the contract is:
//   (a) the unit is enrolled in EXACTLY 2 experiments (never all 3),
//   (b) C is always enrolled (orthogonal to both A and B),
//   (c) A and B are mutually exclusive (one wins, the other is
//       skipped) — by greedy id-order tie-break, A always wins
//       because it was inserted first,
//   (d) the same unit re-flowing through Apply produces the same
//       enrollment set (sticky assignment, paired-runs.md § Sticky
//       task retries).
//
// Companion to TestApply_OrthogonalOverlap_NoDoubleEnrollment in
// apply_test.go (which exercises a single unit) — this shakedown
// asserts the determinism + sticky-assignment contract over many
// units, the property that lets Phase 2 retries re-route to the same
// experiments without re-hashing.
func TestShakedown_OrthogonalOverlap(t *testing.T) {
	db := openDB(t)
	ctx := context.Background()

	expA := seedFactorialExperiment(t, db, "captain", "convoy", `[{"name":"prompt","levels":["A","B"]}]`)
	seedTreatment(t, db, expA, "treatment", "captain/factorA@HEAD", 1.0)

	expB := seedFactorialExperiment(t, db, "captain", "convoy", `[{"name":"prompt","levels":["A","B"]}]`)
	seedTreatment(t, db, expB, "treatment", "captain/factorB@HEAD", 1.0)

	expC := seedFactorialExperiment(t, db, "captain", "convoy", `[{"name":"rules","levels":["on","off"]}]`)
	seedTreatment(t, db, expC, "treatment", "captain/rulesC@HEAD", 1.0)

	const totalUnits = 50
	type enrollSet struct {
		hasA, hasB, hasC bool
	}
	first := make(map[int]enrollSet)
	for unitID := 1; unitID <= totalUnits; unitID++ {
		call := CallDescriptor{
			AgentName:       "captain",
			NaturalUnitKind: "convoy",
			NaturalUnitID:   unitID,
			PromptTemplate:  "captain/default@HEAD",
		}
		_, assignments, err := Apply(ctx, db, call)
		if err != nil {
			t.Fatalf("Apply(unit=%d): %v", unitID, err)
		}

		// (a) Exactly 2 enrollments per unit.
		if len(assignments) != 2 {
			t.Fatalf("unit %d: got %d assignments, want 2 (orthogonal subset of A/B/C): %+v",
				unitID, len(assignments), assignments)
		}

		var es enrollSet
		for _, a := range assignments {
			switch a.ExperimentID {
			case expA:
				es.hasA = true
			case expB:
				es.hasB = true
			case expC:
				es.hasC = true
			}
		}

		// (b) C always enrolled.
		if !es.hasC {
			t.Errorf("unit %d: C (id=%d) not enrolled — orthogonal experiment must always be picked", unitID, expC)
		}
		// (c) A and B mutually exclusive — never both, never neither.
		if es.hasA && es.hasB {
			t.Errorf("unit %d: BOTH A and B enrolled — conflicts on factor 'prompt' should have been resolved", unitID)
		}
		if !es.hasA && !es.hasB {
			t.Errorf("unit %d: NEITHER A nor B enrolled — at least one of the two should win the conflict", unitID)
		}
		// Greedy id-order tie-break: A wins (lower id).
		if !es.hasA {
			t.Errorf("unit %d: expected A (lower id=%d) to win tie-break over B (id=%d)", unitID, expA, expB)
		}

		first[unitID] = es
	}

	// (d) Sticky assignment: re-flow the same units, expect identical
	// enrollment sets. Apply is idempotent on (experiment, unit) — the
	// recordExperimentRun path skips re-insert when a prior assignment
	// exists — so a second pass returns the SAME set.
	for unitID := 1; unitID <= totalUnits; unitID++ {
		call := CallDescriptor{
			AgentName:       "captain",
			NaturalUnitKind: "convoy",
			NaturalUnitID:   unitID,
			PromptTemplate:  "captain/default@HEAD",
		}
		_, assignments, err := Apply(ctx, db, call)
		if err != nil {
			t.Fatalf("Apply(unit=%d, second pass): %v", unitID, err)
		}
		if len(assignments) != 2 {
			t.Fatalf("unit %d (second pass): got %d assignments, want 2", unitID, len(assignments))
		}
		var es enrollSet
		for _, a := range assignments {
			switch a.ExperimentID {
			case expA:
				es.hasA = true
			case expB:
				es.hasB = true
			case expC:
				es.hasC = true
			}
		}
		if es != first[unitID] {
			t.Errorf("unit %d: enrollment set changed across calls — sticky assignment broken (was %+v now %+v)",
				unitID, first[unitID], es)
		}
	}

	// Database invariant: no unit accumulated 3 ExperimentRuns rows
	// (one per enrollment), regardless of how many times Apply ran.
	rows, err := db.QueryContext(ctx, `
		SELECT natural_unit_id, COUNT(DISTINCT experiment_id)
		FROM ExperimentRuns
		WHERE natural_unit_kind = 'convoy'
		GROUP BY natural_unit_id
	`)
	if err != nil {
		t.Fatalf("scan ExperimentRuns: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var unit, distinctExps int
		if err := rows.Scan(&unit, &distinctExps); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if distinctExps != 2 {
			t.Errorf("unit %d: %d distinct experiments in ExperimentRuns, want exactly 2", unit, distinctExps)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows.Err: %v", err)
	}

	// And specifically: B was never enrolled by ANY unit.
	var bRuns int
	if err := db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM ExperimentRuns WHERE experiment_id = ?
	`, expB).Scan(&bRuns); err != nil {
		t.Fatalf("count B runs: %v", err)
	}
	if bRuns != 0 {
		t.Errorf("ExperimentRuns rows for skipped experiment B: got %d, want 0", bRuns)
	}
}
