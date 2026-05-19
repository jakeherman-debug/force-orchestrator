package treatments

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"force-orchestrator/internal/holdout"
	"force-orchestrator/internal/store"
)

func openDB(t *testing.T) *sql.DB {
	t.Helper()
	db := store.InitHolocronDSN(":memory:")
	if db == nil {
		t.Fatalf("InitHolocronDSN returned nil")
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// TestApply_NotInHoldout_NoActiveExperiments_PassesThrough — the
// "Live mode plus no work to do" path. With no holdout row, no
// experiments, the live pipeline returns the descriptor unchanged
// and produces zero assignments.
func TestApply_NotInHoldout_NoActiveExperiments_PassesThrough(t *testing.T) {
	db := openDB(t)
	ctx := context.Background()

	in := CallDescriptor{
		AgentName:       "captain",
		NaturalUnitKind: "task",
		NaturalUnitID:   42,
		PromptTemplate:  "captain/default@HEAD",
		Model:           "claude-opus-4-7",
		InHoldout:       false,
	}
	out, assignments, err := Apply(ctx, db, in)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if out != in {
		t.Errorf("Apply mutated CallDescriptor: got %+v, want %+v", out, in)
	}
	if len(assignments) != 0 {
		t.Errorf("pass-through returned %d assignments; expected 0", len(assignments))
	}
}

// TestApply_RecordsToTreatmentApplyLog_PostFlip — every Apply call,
// regardless of pass-through / holdout / experiment, lands one row
// in TreatmentApplyLog. Default mode tagged 'live'.
func TestApply_RecordsToTreatmentApplyLog_PostFlip(t *testing.T) {
	db := openDB(t)
	ctx := context.Background()

	in := CallDescriptor{
		AgentName:       "council",
		NaturalUnitKind: "task",
		NaturalUnitID:   7,
		PromptTemplate:  "council/default@HEAD",
		Model:           "claude-sonnet-4-6",
		InHoldout:       true,
	}
	if _, _, err := Apply(ctx, db, in); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	var (
		count   int
		agent   string
		holdout int
		mode    string
	)
	err := db.QueryRow(`
		SELECT COUNT(*), MAX(agent_name), MAX(in_holdout), MAX(mode)
		FROM TreatmentApplyLog
	`).Scan(&count, &agent, &holdout, &mode)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if count != 1 {
		t.Errorf("TreatmentApplyLog row count: got %d, want 1", count)
	}
	if agent != "council" {
		t.Errorf("agent_name: got %q, want %q", agent, "council")
	}
	if holdout != 1 {
		t.Errorf("in_holdout: got %d, want 1", holdout)
	}
	if mode != ModeLive {
		t.Errorf("mode: got %q, want %q (default Phase 2+)", mode, ModeLive)
	}
}

// TestApply_LogOnlyRollback_RecordsLogOnlyMode — the operator-set
// SystemConfig rollback flips behaviour to log-only without a
// re-deploy. Same descriptor, no rewrite, mode='log_only'.
func TestApply_LogOnlyRollback_RecordsLogOnlyMode(t *testing.T) {
	db := openDB(t)
	ctx := context.Background()
	store.SetConfig(db, SystemConfigApplyMode, ModeLogOnly)

	in := CallDescriptor{AgentName: "medic", NaturalUnitKind: "task", NaturalUnitID: 1}
	if _, _, err := Apply(ctx, db, in); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	var mode string
	if err := db.QueryRow(`SELECT mode FROM TreatmentApplyLog ORDER BY id DESC LIMIT 1`).Scan(&mode); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if mode != ModeLogOnly {
		t.Errorf("mode: got %q, want %q after rollback", mode, ModeLogOnly)
	}
}

// TestApply_HoldoutMember_SkipsExperimentEnrollment — seed a holdout
// row that captures every unit (plateau_fraction=1.0) so the test
// unit is guaranteed in. With an active experiment present, holdout
// members are NOT enrolled. Their InHoldout flag flips to true.
func TestApply_HoldoutMember_SkipsExperimentEnrollment(t *testing.T) {
	db := openDB(t)
	ctx := context.Background()
	mintFullCoverageHoldout(t, db)
	expID := seedRunningExperiment(t, db, "captain", "task")

	in := CallDescriptor{
		AgentName:       "captain",
		NaturalUnitKind: "task",
		NaturalUnitID:   42,
		PromptTemplate:  "captain/default@HEAD",
	}
	out, assignments, err := Apply(ctx, db, in)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !out.InHoldout {
		t.Errorf("expected out.InHoldout=true for holdout member; got false")
	}
	if len(assignments) != 0 {
		t.Errorf("holdout member should produce zero assignments; got %d", len(assignments))
	}
	// And no ExperimentRuns row was created against the experiment.
	var runs int
	if err := db.QueryRow(`SELECT COUNT(*) FROM ExperimentRuns WHERE experiment_id = ?`, expID).Scan(&runs); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if runs != 0 {
		t.Errorf("holdout member produced %d ExperimentRuns; want 0", runs)
	}
}

// TestApply_SingleActiveExperiment_AppliesAssignedTreatment — a
// non-holdout unit hits a running experiment with two arms, one of
// which has prompt_template_ref='captain/treatmentA@HEAD'. The
// returned descriptor reflects the assigned treatment's prompt ref.
func TestApply_SingleActiveExperiment_AppliesAssignedTreatment(t *testing.T) {
	db := openDB(t)
	ctx := context.Background()
	expID := seedRunningExperiment(t, db, "captain", "task")
	// One arm is unique enough that we can see it in the post-Apply call.
	seedTreatment(t, db, expID, "treatment", "captain/treatmentA@HEAD", 1.0)
	// Second arm too — but with weight 0 so the picker always lands
	// on the first arm. (Sticky deterministic assignment is checked
	// in TestApply_DeterministicEnrollment.)
	seedTreatment(t, db, expID, "control", "", 0)

	in := CallDescriptor{
		AgentName:       "captain",
		NaturalUnitKind: "task",
		NaturalUnitID:   100,
		PromptTemplate:  "captain/default@HEAD",
	}
	out, assignments, err := Apply(ctx, db, in)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if len(assignments) != 1 {
		t.Fatalf("expected 1 assignment, got %d", len(assignments))
	}
	if assignments[0].ExperimentID != expID {
		t.Errorf("assignment.ExperimentID: got %d, want %d", assignments[0].ExperimentID, expID)
	}
	if out.PromptTemplate != "captain/treatmentA@HEAD" {
		t.Errorf("descriptor not rewritten: PromptTemplate=%q, want captain/treatmentA@HEAD", out.PromptTemplate)
	}
	// And ExperimentRuns picked up a row.
	var runs int
	if err := db.QueryRow(`SELECT COUNT(*) FROM ExperimentRuns WHERE experiment_id = ?`, expID).Scan(&runs); err != nil {
		t.Fatalf("scan ExperimentRuns: %v", err)
	}
	if runs != 1 {
		t.Errorf("ExperimentRuns rows: got %d, want 1", runs)
	}
}

// TestApply_DeterministicEnrollment — two Apply calls for the same
// (agent, kind, id) triple against an experiment with two equally
// weighted arms always return the same assigned arm.
func TestApply_DeterministicEnrollment(t *testing.T) {
	db := openDB(t)
	ctx := context.Background()
	expID := seedRunningExperiment(t, db, "council", "task")
	seedTreatment(t, db, expID, "control", "council/control@HEAD", 0.5)
	seedTreatment(t, db, expID, "treatment", "council/treatment@HEAD", 0.5)

	in := CallDescriptor{
		AgentName:       "council",
		NaturalUnitKind: "task",
		NaturalUnitID:   12345,
	}
	first, _, err := Apply(ctx, db, in)
	if err != nil {
		t.Fatalf("first Apply: %v", err)
	}
	for i := 0; i < 5; i++ {
		again, _, err := Apply(ctx, db, in)
		if err != nil {
			t.Fatalf("repeat Apply: %v", err)
		}
		if again.PromptTemplate != first.PromptTemplate {
			t.Errorf("non-deterministic enrollment: first=%q again=%q", first.PromptTemplate, again.PromptTemplate)
		}
	}
}

// TestApply_DeterministicEnrollment_SpreadsAcrossArms — over many
// distinct unit ids, both arms receive at least 30% of assignments.
// Confirms the picker is not pinned to a single arm.
func TestApply_DeterministicEnrollment_SpreadsAcrossArms(t *testing.T) {
	db := openDB(t)
	ctx := context.Background()
	expID := seedRunningExperiment(t, db, "council", "task")
	seedTreatment(t, db, expID, "control", "council/control@HEAD", 0.5)
	seedTreatment(t, db, expID, "treatment", "council/treatment@HEAD", 0.5)

	const N = 400
	controlHits, treatmentHits := 0, 0
	for unitID := 1; unitID <= N; unitID++ {
		out, _, err := Apply(ctx, db, CallDescriptor{
			AgentName:       "council",
			NaturalUnitKind: "task",
			NaturalUnitID:   unitID,
		})
		if err != nil {
			t.Fatalf("unit %d: %v", unitID, err)
		}
		switch out.PromptTemplate {
		case "council/control@HEAD":
			controlHits++
		case "council/treatment@HEAD":
			treatmentHits++
		}
	}
	if controlHits < N/3 || treatmentHits < N/3 {
		t.Errorf("uneven arm distribution: control=%d, treatment=%d (N=%d) — picker may be biased", controlHits, treatmentHits, N)
	}
}

// TestApply_NilDBNoPanic — pre-DB callers (very early daemon boot,
// tests using a builder) MUST be tolerated by Apply.
func TestApply_NilDBNoPanic(t *testing.T) {
	in := CallDescriptor{AgentName: "test"}
	out, assignments, err := Apply(context.Background(), nil, in)
	if err != nil {
		t.Fatalf("Apply(nil db): %v", err)
	}
	if out != in {
		t.Errorf("Apply(nil db) mutated descriptor")
	}
	if len(assignments) != 0 {
		t.Errorf("Apply(nil db) returned %d assignments", len(assignments))
	}
}

// TestApply_StickyAcrossRetries — paired-runs.md § Sticky task
// retries: a Medic-requeued task hitting the same experiment keeps
// its original cell assignment. The single-arm sticky property is
// the load-bearing piece of that contract — verify by re-querying
// the same descriptor and checking ExperimentRuns row count stays at
// 1, not 2.
func TestApply_StickyAcrossRetries(t *testing.T) {
	db := openDB(t)
	ctx := context.Background()
	expID := seedRunningExperiment(t, db, "captain", "task")
	seedTreatment(t, db, expID, "treatment", "captain/treatmentA@HEAD", 1.0)

	in := CallDescriptor{
		AgentName:       "captain",
		NaturalUnitKind: "task",
		NaturalUnitID:   77,
	}
	for i := 0; i < 3; i++ {
		if _, _, err := Apply(ctx, db, in); err != nil {
			t.Fatalf("Apply attempt %d: %v", i, err)
		}
	}
	var runs int
	if err := db.QueryRow(`
		SELECT COUNT(*) FROM ExperimentRuns
		WHERE experiment_id = ? AND natural_unit_kind = ? AND natural_unit_id = ?
	`, expID, "task", 77).Scan(&runs); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if runs != 1 {
		t.Errorf("sticky retry: got %d ExperimentRuns rows, want 1", runs)
	}
}

// TestApply_OrthogonalOverlap_NoDoubleEnrollment — three running
// experiments all match (captain, task). A and B both declare factor
// "prompt"; C declares "rules". The orthogonal-overlap scheduler
// must enroll the unit in {A, C} only — A wins over B by id-order
// tie-break, A and C are orthogonal (disjoint factor sets), B is
// skipped because it conflicts with the already-selected A on
// "prompt" (paired-runs.md § Orthogonal dimension invariant).
func TestApply_OrthogonalOverlap_NoDoubleEnrollment(t *testing.T) {
	db := openDB(t)
	ctx := context.Background()

	// A: factors={prompt}, declares a unique prompt rewrite so we can
	// confirm A's treatment landed on the descriptor.
	expA := seedFactorialExperiment(t, db, "captain", "task", `[{"name":"prompt","levels":["A","B"]}]`)
	seedTreatment(t, db, expA, "treatment", "captain/factorA@HEAD", 1.0)

	// B: factors={prompt} — conflicts with A on the "prompt" factor.
	// MUST be skipped.
	expB := seedFactorialExperiment(t, db, "captain", "task", `[{"name":"prompt","levels":["A","B"]}]`)
	seedTreatment(t, db, expB, "treatment", "captain/factorB@HEAD", 1.0)

	// C: factors={rules} — orthogonal to both A and B.
	expC := seedFactorialExperiment(t, db, "captain", "task", `[{"name":"rules","levels":["on","off"]}]`)
	seedTreatment(t, db, expC, "treatment", "captain/rulesC@HEAD", 1.0)

	in := CallDescriptor{
		AgentName:       "captain",
		NaturalUnitKind: "task",
		NaturalUnitID:   555,
		PromptTemplate:  "captain/default@HEAD",
	}
	_, assignments, err := Apply(ctx, db, in)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}

	if len(assignments) != 2 {
		t.Fatalf("expected exactly 2 assignments (A + C), got %d: %+v", len(assignments), assignments)
	}

	gotIDs := map[int]bool{}
	for _, a := range assignments {
		gotIDs[a.ExperimentID] = true
	}
	if !gotIDs[expA] {
		t.Errorf("expected A (id=%d) to be enrolled — lowest id wins tie-break with B", expA)
	}
	if !gotIDs[expC] {
		t.Errorf("expected C (id=%d) to be enrolled — orthogonal to A on factors", expC)
	}
	if gotIDs[expB] {
		t.Errorf("B (id=%d) should have been skipped — conflicts with A on factor 'prompt'", expB)
	}

	// Two ExperimentRuns rows for this unit — one per selected
	// experiment. B never recorded a run.
	var runs int
	if err := db.QueryRow(`
		SELECT COUNT(*) FROM ExperimentRuns
		WHERE natural_unit_kind = ? AND natural_unit_id = ?
	`, "task", 555).Scan(&runs); err != nil {
		t.Fatalf("scan ExperimentRuns: %v", err)
	}
	if runs != 2 {
		t.Errorf("ExperimentRuns rows for unit: got %d, want 2 (A + C, NOT B)", runs)
	}

	// And specifically: zero rows for B.
	var bRuns int
	if err := db.QueryRow(`
		SELECT COUNT(*) FROM ExperimentRuns
		WHERE experiment_id = ? AND natural_unit_id = ?
	`, expB, 555).Scan(&bRuns); err != nil {
		t.Fatalf("scan B runs: %v", err)
	}
	if bRuns != 0 {
		t.Errorf("ExperimentRuns rows for skipped experiment B: got %d, want 0", bRuns)
	}
}

// TestTreatmentsApply_Live — D16 P2 regression: Apply must run the live
// pipeline (descriptor rewrite + ExperimentRuns write) when no SystemConfig
// key is present (default) AND when the key is explicitly 'live'.
//
// Before D16 P2 any DB that had treatments_apply_mode = 'log_only' lingering
// from Phase 1 testing would silently skip experiment enrollment; this test
// confirms the observable state changes that prove the live path ran.
//
// Assertions:
//   (a) happy path — descriptor is rewritten, ExperimentRuns row written
//   (b) failure mode — holdout member is NOT enrolled despite active experiment
//   (c) idempotence — Apply twice for the same unit produces exactly one
//       ExperimentRuns row (sticky assignment)
//   (d) log-only rollback — explicit 'log_only' key skips enrollment
//   (e) migration gate — INSERT OR IGNORE seeds the 'live' key without
//       overwriting a deliberate 'log_only' row
func TestTreatmentsApply_Live(t *testing.T) {
	// (a) happy path: no holdout, one running experiment, one treatment arm
	// that rewrites the prompt template. Verify the descriptor comes back
	// modified and an ExperimentRuns row lands.
	t.Run("live_mode_rewrites_descriptor_and_records_run", func(t *testing.T) {
		db := openDB(t)
		ctx := context.Background()
		// No SystemConfig row — activeApplyMode must default to ModeLive.
		expID := seedRunningExperiment(t, db, "captain", "task")
		seedTreatment(t, db, expID, "treatment", "captain/live-treatment@HEAD", 1.0)

		in := CallDescriptor{
			AgentName:       "captain",
			NaturalUnitKind: "task",
			NaturalUnitID:   9001,
			PromptTemplate:  "captain/default@HEAD",
			Model:           "claude-sonnet-4-6",
		}
		out, assignments, err := Apply(ctx, db, in)
		if err != nil {
			t.Fatalf("Apply: %v", err)
		}

		// Descriptor must have been rewritten by the treatment.
		if out.PromptTemplate != "captain/live-treatment@HEAD" {
			t.Errorf("descriptor not rewritten: PromptTemplate=%q, want captain/live-treatment@HEAD", out.PromptTemplate)
		}
		if len(assignments) != 1 {
			t.Fatalf("expected 1 assignment (live mode); got %d", len(assignments))
		}

		// ExperimentRuns must have a row — this is the observable evidence that
		// the live path ran (log-only produces zero rows).
		var runs int
		if err := db.QueryRow(`
			SELECT COUNT(*) FROM ExperimentRuns
			WHERE experiment_id = ? AND natural_unit_kind = 'task' AND natural_unit_id = 9001
		`, expID).Scan(&runs); err != nil {
			t.Fatalf("scan ExperimentRuns: %v", err)
		}
		if runs != 1 {
			t.Errorf("ExperimentRuns rows: got %d, want 1 (live mode must write runs)", runs)
		}

		// TreatmentApplyLog must record mode='live'.
		var logMode string
		if err := db.QueryRow(`SELECT mode FROM TreatmentApplyLog ORDER BY id DESC LIMIT 1`).Scan(&logMode); err != nil {
			t.Fatalf("scan TreatmentApplyLog: %v", err)
		}
		if logMode != ModeLive {
			t.Errorf("TreatmentApplyLog.mode: got %q, want %q", logMode, ModeLive)
		}
	})

	// (b) failure mode: holdout member must not be enrolled even in live mode.
	t.Run("holdout_member_skips_enrollment_in_live_mode", func(t *testing.T) {
		db := openDB(t)
		ctx := context.Background()
		mintFullCoverageHoldout(t, db)
		expID := seedRunningExperiment(t, db, "council", "task")
		seedTreatment(t, db, expID, "treatment", "council/treatment@HEAD", 1.0)

		in := CallDescriptor{
			AgentName:       "council",
			NaturalUnitKind: "task",
			NaturalUnitID:   8888,
			PromptTemplate:  "council/default@HEAD",
		}
		out, assignments, err := Apply(ctx, db, in)
		if err != nil {
			t.Fatalf("Apply: %v", err)
		}
		if !out.InHoldout {
			t.Errorf("holdout member: expected InHoldout=true; got false")
		}
		if len(assignments) != 0 {
			t.Errorf("holdout member: expected 0 assignments; got %d", len(assignments))
		}
		var runs int
		if err := db.QueryRow(`SELECT COUNT(*) FROM ExperimentRuns WHERE experiment_id = ?`, expID).Scan(&runs); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if runs != 0 {
			t.Errorf("holdout member produced %d ExperimentRuns; want 0", runs)
		}
	})

	// (c) idempotence: Apply twice for the same unit produces exactly one
	// ExperimentRuns row — the sticky-assignment invariant.
	t.Run("idempotent_apply_yields_single_experiment_run", func(t *testing.T) {
		db := openDB(t)
		ctx := context.Background()
		expID := seedRunningExperiment(t, db, "medic", "task")
		seedTreatment(t, db, expID, "treatment", "medic/treatment@HEAD", 1.0)

		in := CallDescriptor{
			AgentName:       "medic",
			NaturalUnitKind: "task",
			NaturalUnitID:   7777,
		}
		for i := 0; i < 3; i++ {
			if _, _, err := Apply(ctx, db, in); err != nil {
				t.Fatalf("Apply attempt %d: %v", i, err)
			}
		}

		var runs int
		if err := db.QueryRow(`
			SELECT COUNT(*) FROM ExperimentRuns
			WHERE experiment_id = ? AND natural_unit_kind = 'task' AND natural_unit_id = 7777
		`, expID).Scan(&runs); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if runs != 1 {
			t.Errorf("idempotence: got %d ExperimentRuns rows, want exactly 1", runs)
		}
	})

	// (d) explicit log_only rollback via SystemConfig disables enrollment.
	t.Run("log_only_systemconfig_disables_enrollment", func(t *testing.T) {
		db := openDB(t)
		ctx := context.Background()
		store.SetConfig(db, SystemConfigApplyMode, ModeLogOnly)
		expID := seedRunningExperiment(t, db, "captain", "task")
		seedTreatment(t, db, expID, "treatment", "captain/treatment@HEAD", 1.0)

		in := CallDescriptor{
			AgentName:       "captain",
			NaturalUnitKind: "task",
			NaturalUnitID:   6666,
			PromptTemplate:  "captain/default@HEAD",
		}
		out, assignments, err := Apply(ctx, db, in)
		if err != nil {
			t.Fatalf("Apply: %v", err)
		}
		// Log-only: descriptor unchanged, no assignments, no ExperimentRuns row.
		if out.PromptTemplate != "captain/default@HEAD" {
			t.Errorf("log-only: descriptor was rewritten; got PromptTemplate=%q", out.PromptTemplate)
		}
		if len(assignments) != 0 {
			t.Errorf("log-only: expected 0 assignments; got %d", len(assignments))
		}
		var runs int
		if err := db.QueryRow(`SELECT COUNT(*) FROM ExperimentRuns WHERE experiment_id = ?`, expID).Scan(&runs); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if runs != 0 {
			t.Errorf("log-only: expected 0 ExperimentRuns; got %d", runs)
		}
	})

	// (e) D16 P2 migration: INSERT OR IGNORE seeds 'live' without overwriting
	// a deliberate 'log_only' row. Simulates the runMigrations behaviour.
	t.Run("migration_seeds_live_without_clobbering_log_only", func(t *testing.T) {
		db := openDB(t)

		// Case 1: no pre-existing row → migration seeds 'live'.
		db.Exec(`INSERT OR IGNORE INTO SystemConfig (key, value)
			VALUES ('treatments_apply_mode', 'live')`)
		var v1 string
		db.QueryRow(`SELECT value FROM SystemConfig WHERE key = 'treatments_apply_mode'`).Scan(&v1)
		if v1 != "live" {
			t.Errorf("migration case 1: got %q, want 'live'", v1)
		}

		// Case 2: pre-existing 'log_only' row → migration does NOT overwrite.
		db2 := openDB(t)
		store.SetConfig(db2, SystemConfigApplyMode, ModeLogOnly)
		db2.Exec(`INSERT OR IGNORE INTO SystemConfig (key, value)
			VALUES ('treatments_apply_mode', 'live')`)
		var v2 string
		db2.QueryRow(`SELECT value FROM SystemConfig WHERE key = 'treatments_apply_mode'`).Scan(&v2)
		if v2 != ModeLogOnly {
			t.Errorf("migration case 2: got %q, want %q (operator rollback must survive)", v2, ModeLogOnly)
		}
	})
}

// ──────────────────────────────────────────────────────────────────────────
// helpers
// ──────────────────────────────────────────────────────────────────────────

// mintFullCoverageHoldout creates a holdout that includes EVERY unit
// (plateau_fraction=1.0) so tests don't have to fish for a unit-id
// that hashes into the 2% baseline plateau.
func mintFullCoverageHoldout(t *testing.T, db *sql.DB) {
	t.Helper()
	if _, err := holdout.MintBaseline2026(context.Background(), db); err != nil {
		t.Fatalf("MintBaseline2026: %v", err)
	}
	// Backdate reference_date so the ramp window is far in the past
	// (we want plateau coverage, not ramp-fractional).
	past := time.Now().UTC().Add(-30 * 24 * time.Hour).Format("2006-01-02 15:04:05")
	if _, err := db.Exec(`
		UPDATE GlobalHoldouts
		SET plateau_fraction = 1.0, reference_date = ?
		WHERE name = ?
	`, past, holdout.BaselineHoldoutName); err != nil {
		t.Fatalf("expand holdout: %v", err)
	}
}

func seedRunningExperiment(t *testing.T, db *sql.DB, agent, unit string) int {
	t.Helper()
	res, err := db.Exec(`
		INSERT INTO Experiments
			(name, hypothesis_text, subject_agent, assignment_unit, status, created_by)
		VALUES (?, 'test hypothesis', ?, ?, 'running', 'test')
	`, "exp-"+agent+"-"+unit, agent, unit)
	if err != nil {
		t.Fatalf("seed experiment: %v", err)
	}
	id, _ := res.LastInsertId()
	return int(id)
}

// seedFactorialExperiment seeds a running factorial experiment with
// the given factors_json payload. Used by orthogonal-overlap tests
// where the conflict signal is the factor-name set, not the prompt
// template.
func seedFactorialExperiment(t *testing.T, db *sql.DB, agent, unit, factorsJSON string) int {
	t.Helper()
	res, err := db.Exec(`
		INSERT INTO Experiments
			(name, hypothesis_text, kind, factors_json, subject_agent, assignment_unit, status, created_by)
		VALUES (?, 'test hypothesis', 'factorial', ?, ?, ?, 'running', 'test')
	`, "factorial-"+agent+"-"+unit+"-"+factorsJSON, factorsJSON, agent, unit)
	if err != nil {
		t.Fatalf("seed factorial experiment: %v", err)
	}
	id, _ := res.LastInsertId()
	return int(id)
}

func seedTreatment(t *testing.T, db *sql.DB, experimentID int, arm, promptRef string, weight float64) int {
	t.Helper()
	// Spec rows are content-snapshotted; provide a unique hash so the
	// UNIQUE(spec_hash) constraint doesn't reject the second arm.
	specRes, err := db.Exec(`
		INSERT INTO TreatmentSpecs
			(spec_hash, prompt_template_ref)
		VALUES (?, ?)
	`, "h-"+arm+"-"+promptRef, promptRef)
	if err != nil {
		t.Fatalf("seed treatment spec: %v", err)
	}
	specID, _ := specRes.LastInsertId()
	res, err := db.Exec(`
		INSERT INTO ExperimentTreatments
			(experiment_id, arm_label, treatment_spec_id, target_cell_weight)
		VALUES (?, ?, ?, ?)
	`, experimentID, arm, specID, weight)
	if err != nil {
		t.Fatalf("seed treatment: %v", err)
	}
	id, _ := res.LastInsertId()
	return int(id)
}
