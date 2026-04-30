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
