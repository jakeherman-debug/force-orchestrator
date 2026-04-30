package experiments

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

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

const minimalManifest = `
name: lifecycle-shakedown
hypothesis: lifecycle smoke test
subject_agent: captain
assignment_unit: task
stakes_tier: low
analysis_framework_version: "2026-04-29"
treatments:
  - arm_label: control
    prompt_template_ref: captain/default@HEAD
    target_cell_weight: 0.5
  - arm_label: treatment
    prompt_template_ref: captain/treatmentA@HEAD
    target_cell_weight: 0.5
metrics:
  - metric_name: captain_rejection_rate
    metric_version: "2026-04-23"
    direction: lower_is_better
    is_primary: true
promote:
  rule_key: captain-prompt-test
  proposed_content: shakedown
`

// TestAuthorFromYAML_HappyPath — sample YAML produces correct schema
// rows in Experiments + ExperimentTreatments + ExperimentMetrics.
func TestAuthorFromYAML_HappyPath(t *testing.T) {
	db := openDB(t)
	ctx := context.Background()
	id, err := AuthorFromBytes(ctx, db, []byte(minimalManifest))
	if err != nil {
		t.Fatalf("AuthorFromBytes: %v", err)
	}
	if id <= 0 {
		t.Fatalf("expected positive experiment id, got %d", id)
	}
	var status, agent, unit string
	err = db.QueryRowContext(ctx, `
		SELECT status, subject_agent, assignment_unit FROM Experiments WHERE id = ?
	`, id).Scan(&status, &agent, &unit)
	if err != nil {
		t.Fatalf("scan experiment: %v", err)
	}
	if status != StatusAuthored {
		t.Errorf("status: got %q, want %q", status, StatusAuthored)
	}
	if agent != "captain" || unit != "task" {
		t.Errorf("agent/unit: got %q/%q, want captain/task", agent, unit)
	}
	var armCount, metricCount int
	db.QueryRowContext(ctx, `SELECT COUNT(*) FROM ExperimentTreatments WHERE experiment_id = ?`, id).Scan(&armCount)
	db.QueryRowContext(ctx, `SELECT COUNT(*) FROM ExperimentMetrics WHERE experiment_id = ?`, id).Scan(&metricCount)
	if armCount != 2 {
		t.Errorf("treatment count: got %d, want 2", armCount)
	}
	if metricCount != 1 {
		t.Errorf("metric count: got %d, want 1", metricCount)
	}
}

// TestAuthorFromYAML_RejectsMissingHypothesis — manifests without a
// hypothesis hit the Pre-registration gate.
func TestAuthorFromYAML_RejectsMissingHypothesis(t *testing.T) {
	db := openDB(t)
	bad := `name: x
subject_agent: captain
assignment_unit: task
treatments:
  - arm_label: control
  - arm_label: treatment
metrics:
  - metric_name: m
    metric_version: v
    is_primary: true
`
	if _, err := AuthorFromBytes(context.Background(), db, []byte(bad)); err == nil {
		t.Errorf("expected error on missing hypothesis, got nil")
	}
}

// TestAuthorFromYAML_RejectsMultiplePrimaryMetrics — exactly one
// is_primary must be true (paired-runs.md § Pre-registration).
func TestAuthorFromYAML_RejectsMultiplePrimaryMetrics(t *testing.T) {
	db := openDB(t)
	bad := `name: x
hypothesis: testing
subject_agent: captain
assignment_unit: task
treatments:
  - arm_label: control
  - arm_label: treatment
metrics:
  - metric_name: a
    is_primary: true
  - metric_name: b
    is_primary: true
`
	if _, err := AuthorFromBytes(context.Background(), db, []byte(bad)); err == nil {
		t.Errorf("expected error on two primary metrics, got nil")
	}
}

// TestRatify_RequiresOperatorRoute_AuditLogged — Ratify rejects calls
// without an operator email and records an AuditLog row when it succeeds.
func TestRatify_RequiresOperatorRoute_AuditLogged(t *testing.T) {
	db := openDB(t)
	ctx := context.Background()
	id, err := AuthorFromBytes(ctx, db, []byte(minimalManifest))
	if err != nil {
		t.Fatalf("author: %v", err)
	}
	if err := Ratify(ctx, db, id, ""); err == nil {
		t.Errorf("Ratify with empty operator email should error")
	}
	if err := Ratify(ctx, db, id, "operator@upstart.com"); err != nil {
		t.Fatalf("Ratify: %v", err)
	}
	var status string
	db.QueryRowContext(ctx, `SELECT status FROM Experiments WHERE id = ?`, id).Scan(&status)
	if status != StatusRunning {
		t.Errorf("status after Ratify: got %q, want %q", status, StatusRunning)
	}
	var auditCount int
	db.QueryRowContext(ctx, `SELECT COUNT(*) FROM AuditLog WHERE action = 'experiment.ratify' AND task_id = ?`, id).Scan(&auditCount)
	if auditCount != 1 {
		t.Errorf("AuditLog rows: got %d, want 1", auditCount)
	}
}

// TestRatify_RejectsAlreadyRunning — second Ratify against a running
// experiment errors via the CAS update.
func TestRatify_RejectsAlreadyRunning(t *testing.T) {
	db := openDB(t)
	ctx := context.Background()
	id, _ := AuthorFromBytes(ctx, db, []byte(minimalManifest))
	if err := Ratify(ctx, db, id, "op@x.com"); err != nil {
		t.Fatalf("first Ratify: %v", err)
	}
	if err := Ratify(ctx, db, id, "op@x.com"); err == nil {
		t.Errorf("second Ratify should error")
	}
}

// TestEnrollUnit_DeterministicAcrossRestarts — same unit lands in the
// same arm each call; idempotent and does not insert duplicates.
func TestEnrollUnit_DeterministicAcrossRestarts(t *testing.T) {
	db := openDB(t)
	ctx := context.Background()
	id, _ := AuthorFromBytes(ctx, db, []byte(minimalManifest))
	if err := Ratify(ctx, db, id, "op@x.com"); err != nil {
		t.Fatalf("ratify: %v", err)
	}
	first, err := EnrollUnit(ctx, db, id, "task", 9001)
	if err != nil {
		t.Fatalf("first enroll: %v", err)
	}
	for i := 0; i < 5; i++ {
		again, err := EnrollUnit(ctx, db, id, "task", 9001)
		if err != nil {
			t.Fatalf("repeat enroll: %v", err)
		}
		if again != first {
			t.Errorf("enroll non-deterministic: first=%d again=%d", first, again)
		}
	}
	var rowCount int
	db.QueryRowContext(ctx, `SELECT COUNT(*) FROM ExperimentRuns WHERE experiment_id = ? AND natural_unit_id = ?`, id, 9001).Scan(&rowCount)
	if rowCount != 1 {
		t.Errorf("idempotent enroll left %d rows; want 1", rowCount)
	}
}

// TestTerminate_ComputesOutcome_BayesianFramework — seed runs that
// give treatment a clear win, terminate, and assert the outcome row
// declares treatment.
func TestTerminate_ComputesOutcome_BayesianFramework(t *testing.T) {
	db := openDB(t)
	ctx := context.Background()
	id, err := AuthorFromBytes(ctx, db, []byte(minimalManifest))
	if err != nil {
		t.Fatalf("author: %v", err)
	}
	if err := Ratify(ctx, db, id, "op@x.com"); err != nil {
		t.Fatalf("ratify: %v", err)
	}

	// Seed runs: 200 in each arm, treatment hits 60%, control 30%.
	armIDs := loadArmIDs(t, db, id)
	seedRuns(t, db, id, armIDs["control"], 200, 60)
	seedRuns(t, db, id, armIDs["treatment"], 200, 120)

	if err := Terminate(ctx, db, id, "shakedown_complete"); err != nil {
		t.Fatalf("Terminate: %v", err)
	}

	var status, reason string
	var winnerID int
	var winnerPosterior float64
	db.QueryRowContext(ctx, `SELECT status FROM Experiments WHERE id = ?`, id).Scan(&status)
	db.QueryRowContext(ctx, `SELECT termination_reason, winner_treatment_id, IFNULL(winner_posterior, 0) FROM ExperimentOutcomes WHERE experiment_id = ?`, id).Scan(&reason, &winnerID, &winnerPosterior)
	if status != StatusTerminated {
		t.Errorf("status: got %q, want %q", status, StatusTerminated)
	}
	if reason != "declared_winner" {
		t.Errorf("termination_reason: got %q, want declared_winner (control=%d successes, treatment=%d)", reason, 60, 120)
	}
	if winnerID != armIDs["treatment"] {
		t.Errorf("winner_treatment_id: got %d, want %d (treatment arm)", winnerID, armIDs["treatment"])
	}
	if winnerPosterior <= 0.95 {
		t.Errorf("winner_posterior: got %v, want > 0.95", winnerPosterior)
	}
}

// TestTerminate_Inconclusive_NullEffect — equal arms terminate with
// inconclusive, no winner.
func TestTerminate_Inconclusive_NullEffect(t *testing.T) {
	db := openDB(t)
	ctx := context.Background()
	id, _ := AuthorFromBytes(ctx, db, []byte(minimalManifest))
	if err := Ratify(ctx, db, id, "op@x.com"); err != nil {
		t.Fatalf("ratify: %v", err)
	}
	armIDs := loadArmIDs(t, db, id)
	seedRuns(t, db, id, armIDs["control"], 100, 50)
	seedRuns(t, db, id, armIDs["treatment"], 100, 50)
	if err := Terminate(ctx, db, id, "shakedown_complete"); err != nil {
		t.Fatalf("Terminate: %v", err)
	}
	var reason string
	var winnerID int
	db.QueryRowContext(ctx, `SELECT termination_reason, winner_treatment_id FROM ExperimentOutcomes WHERE experiment_id = ?`, id).Scan(&reason, &winnerID)
	if reason != "inconclusive" {
		t.Errorf("reason: got %q, want inconclusive", reason)
	}
	if winnerID != 0 {
		t.Errorf("winnerID: got %d, want 0", winnerID)
	}
}

// TestMaybePromoteRule_OnlyOnDeclaredWinner — terminated-inconclusive
// experiments do NOT mint proposals; declared-winner experiments do.
func TestMaybePromoteRule_OnlyOnDeclaredWinner(t *testing.T) {
	db := openDB(t)
	ctx := context.Background()

	// Inconclusive run.
	id1, _ := AuthorFromBytes(ctx, db, []byte(minimalManifest))
	Ratify(ctx, db, id1, "op@x.com")
	arms1 := loadArmIDs(t, db, id1)
	seedRuns(t, db, id1, arms1["control"], 100, 50)
	seedRuns(t, db, id1, arms1["treatment"], 100, 50)
	Terminate(ctx, db, id1, "")
	pid, err := MaybePromoteRule(ctx, db, id1)
	if err != nil {
		t.Fatalf("inconclusive MaybePromoteRule: %v", err)
	}
	if pid != 0 {
		t.Errorf("expected no proposal on inconclusive, got id %d", pid)
	}

	// Declared-winner run.
	id2, _ := AuthorFromBytes(ctx, db, []byte(minimalManifest))
	Ratify(ctx, db, id2, "op@x.com")
	arms2 := loadArmIDs(t, db, id2)
	seedRuns(t, db, id2, arms2["control"], 200, 60)
	seedRuns(t, db, id2, arms2["treatment"], 200, 120)
	Terminate(ctx, db, id2, "")
	pid2, err := MaybePromoteRule(ctx, db, id2)
	if err != nil {
		t.Fatalf("winner MaybePromoteRule: %v", err)
	}
	if pid2 == 0 {
		t.Errorf("expected proposal on declared winner, got 0")
	}
	var ruleKey, body string
	db.QueryRowContext(ctx, `SELECT rule_key, IFNULL(proposed_content,'') FROM PromotionProposals WHERE id = ?`, pid2).Scan(&ruleKey, &body)
	if ruleKey != "captain-prompt-test" {
		t.Errorf("rule_key: got %q, want captain-prompt-test", ruleKey)
	}
	if body == "" {
		t.Errorf("proposed_content should be non-empty")
	}
}

// TestLifecycle_EndToEnd_ShakedownExperiment — author → ratify →
// enrol N units → simulate runs → terminate → outcome → promotion
// proposal. Single test exercising the full path.
func TestLifecycle_EndToEnd_ShakedownExperiment(t *testing.T) {
	db := openDB(t)
	ctx := context.Background()

	id, err := AuthorFromBytes(ctx, db, []byte(minimalManifest))
	if err != nil {
		t.Fatalf("author: %v", err)
	}
	if err := Ratify(ctx, db, id, "operator@upstart.com"); err != nil {
		t.Fatalf("ratify: %v", err)
	}
	if got, _ := GetStatus(ctx, db, id); got.Status != StatusRunning {
		t.Errorf("post-ratify status: got %q, want %q", got.Status, StatusRunning)
	}

	const N = 80
	for unitID := 1; unitID <= N; unitID++ {
		if _, err := EnrollUnit(ctx, db, id, "task", unitID); err != nil {
			t.Fatalf("enroll unit %d: %v", unitID, err)
		}
	}

	// Simulate run outcomes. Treatment outperforms control: 80% vs
	// 30% Bernoulli rate.
	armIDs := loadArmIDs(t, db, id)
	scoreRunsBySimulation(t, db, id, armIDs["control"], 0.30)
	scoreRunsBySimulation(t, db, id, armIDs["treatment"], 0.80)

	if err := Terminate(ctx, db, id, "shakedown_complete"); err != nil {
		t.Fatalf("terminate: %v", err)
	}

	st, _ := GetStatus(ctx, db, id)
	if st.Status != StatusTerminated {
		t.Errorf("terminal status: got %q, want %q", st.Status, StatusTerminated)
	}
	if st.OutcomeReason != "declared_winner" {
		t.Errorf("outcome reason: got %q, want declared_winner", st.OutcomeReason)
	}
	if st.WinnerTreatmentID != armIDs["treatment"] {
		t.Errorf("winner: got %d, want %d", st.WinnerTreatmentID, armIDs["treatment"])
	}

	pid, err := MaybePromoteRule(ctx, db, id)
	if err != nil {
		t.Fatalf("promote: %v", err)
	}
	if pid == 0 {
		t.Fatalf("expected a promotion proposal, got 0")
	}
	// Total enrollment.
	total := 0
	for _, n := range st.EnrollmentByArm {
		total += n
	}
	if total != N {
		t.Errorf("enrollment total: got %d, want %d", total, N)
	}
}

// TestAuthorFromYAML_FromSampleManifestFile — sanity-checks the
// shipped sample manifest under experiments/ — broken sample is
// caught at test-time rather than at first operator invocation.
func TestAuthorFromYAML_FromSampleManifestFile(t *testing.T) {
	db := openDB(t)
	ctx := context.Background()
	path := filepath.Join("..", "..", "experiments", "2026-04-29-test-captain-prompt-v18", "manifest.yaml")
	id, err := AuthorFromYAML(ctx, db, path)
	if err != nil {
		t.Fatalf("AuthorFromYAML(%s): %v", path, err)
	}
	if id <= 0 {
		t.Fatalf("expected positive id, got %d", id)
	}
	st, _ := GetStatus(ctx, db, id)
	if st.Name != "test-captain-prompt-v18" {
		t.Errorf("name: got %q, want test-captain-prompt-v18", st.Name)
	}
}

// ──────────────────────────────────────────────────────────────────────────
// helpers
// ──────────────────────────────────────────────────────────────────────────

func loadArmIDs(t *testing.T, db *sql.DB, experimentID int) map[string]int {
	t.Helper()
	rows, err := db.Query(`SELECT id, arm_label FROM ExperimentTreatments WHERE experiment_id = ?`, experimentID)
	if err != nil {
		t.Fatalf("loadArmIDs: %v", err)
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var id int
		var label string
		if err := rows.Scan(&id, &label); err != nil {
			t.Fatalf("loadArmIDs scan: %v", err)
		}
		out[label] = id
	}
	return out
}

// seedRuns inserts `total` ExperimentRuns rows for the given arm,
// of which the first `successes` rows score 1.0 and the rest 0.0.
func seedRuns(t *testing.T, db *sql.DB, experimentID, treatmentID, total, successes int) {
	t.Helper()
	for i := 0; i < total; i++ {
		score := 0.0
		if i < successes {
			score = 1.0
		}
		_, err := db.Exec(`
			INSERT INTO ExperimentRuns
				(experiment_id, treatment_id, natural_unit_kind, natural_unit_id,
				 mode, score, completed_at)
			VALUES (?, ?, 'task', ?, 'paired_real', ?, datetime('now'))
		`, experimentID, treatmentID, i+1, score)
		if err != nil {
			t.Fatalf("seedRuns: %v", err)
		}
	}
}

// scoreRunsBySimulation walks every existing ExperimentRuns row for
// the given arm and back-fills its score. The pattern is "first
// rate*N succeed, rest fail" — deterministic, not stochastic, so
// tests don't have to bracket against MC noise.
func scoreRunsBySimulation(t *testing.T, db *sql.DB, experimentID, treatmentID int, rate float64) {
	t.Helper()
	rows, err := db.Query(`SELECT id FROM ExperimentRuns WHERE experiment_id = ? AND treatment_id = ?`, experimentID, treatmentID)
	if err != nil {
		t.Fatalf("scoreRunsBySimulation: %v", err)
	}
	defer rows.Close()
	var ids []int
	for rows.Next() {
		var id int
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan: %v", err)
		}
		ids = append(ids, id)
	}
	successes := int(float64(len(ids)) * rate)
	for i, id := range ids {
		score := 0.0
		if i < successes {
			score = 1.0
		}
		if _, err := db.Exec(`UPDATE ExperimentRuns SET score = ?, completed_at = datetime('now') WHERE id = ?`, score, id); err != nil {
			t.Fatalf("update score: %v", err)
		}
	}
}
