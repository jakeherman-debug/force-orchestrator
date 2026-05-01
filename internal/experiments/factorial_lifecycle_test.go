package experiments

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// TestAuthorFactorial_2x2HappyPath — AuthorFactorialFromBytes accepts
// the canonical 2x2 manifest and writes id>0, kind='factorial', and
// 4 ExperimentTreatments rows with distinct cell_json bodies.
func TestAuthorFactorial_2x2HappyPath(t *testing.T) {
	db := openDB(t)
	ctx := context.Background()
	id, err := AuthorFactorialFromBytes(ctx, db, []byte(factorialYAML2x2))
	if err != nil {
		t.Fatalf("AuthorFactorialFromBytes: %v", err)
	}
	if id <= 0 {
		t.Fatalf("expected positive experiment id, got %d", id)
	}
	var kind string
	if err := db.QueryRowContext(ctx, `SELECT kind FROM Experiments WHERE id = ?`, id).Scan(&kind); err != nil {
		t.Fatalf("SELECT kind: %v", err)
	}
	if kind != KindFactorial {
		t.Errorf("kind: got %q, want %q", kind, KindFactorial)
	}

	rows, err := db.QueryContext(ctx, `SELECT cell_json FROM ExperimentTreatments WHERE experiment_id = ? ORDER BY id`, id)
	if err != nil {
		t.Fatalf("SELECT treatments: %v", err)
	}
	defer rows.Close()
	seen := map[string]struct{}{}
	for rows.Next() {
		var cell string
		if err := rows.Scan(&cell); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if _, dup := seen[cell]; dup {
			t.Errorf("duplicate cell_json %q across arms", cell)
		}
		seen[cell] = struct{}{}
	}
	if len(seen) != 4 {
		t.Errorf("expected 4 distinct cells, got %d (%v)", len(seen), seen)
	}
}

// TestAuthorFactorial_3x2Asymmetric — 3x2 factorial yields 6 cells.
func TestAuthorFactorial_3x2Asymmetric(t *testing.T) {
	const yaml3x2 = `
name: factorial-3x2-author
hypothesis: model x prompt
kind: factorial
subject_agent: captain
assignment_unit: convoy
factors:
  - name: model
    levels: [haiku, sonnet, opus]
  - name: prompt
    levels: [A, B]
treatments:
  - arm_label: haiku_A
    prompt_template_ref: captain/A
    target_cell_weight: 0.166
    cell: {model: haiku, prompt: A}
  - arm_label: haiku_B
    prompt_template_ref: captain/B
    target_cell_weight: 0.166
    cell: {model: haiku, prompt: B}
  - arm_label: sonnet_A
    prompt_template_ref: captain/A
    target_cell_weight: 0.166
    cell: {model: sonnet, prompt: A}
  - arm_label: sonnet_B
    prompt_template_ref: captain/B
    target_cell_weight: 0.166
    cell: {model: sonnet, prompt: B}
  - arm_label: opus_A
    prompt_template_ref: captain/A
    target_cell_weight: 0.166
    cell: {model: opus, prompt: A}
  - arm_label: opus_B
    prompt_template_ref: captain/B
    target_cell_weight: 0.166
    cell: {model: opus, prompt: B}
metrics:
  - metric_name: approval_rate
    metric_version: "1"
    direction: higher_is_better
    is_primary: true
`
	db := openDB(t)
	ctx := context.Background()
	id, err := AuthorFactorialFromBytes(ctx, db, []byte(yaml3x2))
	if err != nil {
		t.Fatalf("AuthorFactorialFromBytes: %v", err)
	}
	var n int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM ExperimentTreatments WHERE experiment_id = ?`, id).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 6 {
		t.Errorf("expected 6 cells, got %d", n)
	}
}

// TestAuthorFactorial_RejectsSingleManifest — passing a single-treatment
// manifest to the factorial entry point produces a typed error so a
// caller cannot silently mis-route.
func TestAuthorFactorial_RejectsSingleManifest(t *testing.T) {
	db := openDB(t)
	ctx := context.Background()
	_, err := AuthorFactorialFromBytes(ctx, db, []byte(minimalManifest))
	if err == nil {
		t.Fatalf("expected error on single-treatment manifest, got nil")
	}
	msg := err.Error()
	if !strings.Contains(msg, "factorial") || !strings.Contains(msg, "kind") {
		t.Errorf("error %q should mention 'kind' and 'factorial'", msg)
	}
}

// TestEnrollFactorialUnit_Deterministic — same (exp, kind, unit_id)
// always picks the same cell.
func TestEnrollFactorialUnit_Deterministic(t *testing.T) {
	db := openDB(t)
	ctx := context.Background()
	id, err := AuthorFactorialFromBytes(ctx, db, []byte(factorialYAML2x2))
	if err != nil {
		t.Fatalf("author: %v", err)
	}
	if err := Ratify(ctx, db, id, "op@x.com"); err != nil {
		t.Fatalf("ratify: %v", err)
	}
	first, err := EnrollFactorialUnit(ctx, db, id, "convoy", 4242)
	if err != nil {
		t.Fatalf("first enroll: %v", err)
	}
	for i := 0; i < 5; i++ {
		again, err := EnrollFactorialUnit(ctx, db, id, "convoy", 4242)
		if err != nil {
			t.Fatalf("repeat enroll: %v", err)
		}
		if again != first {
			t.Errorf("non-deterministic: first=%d again=%d", first, again)
		}
	}
}

// TestEnrollFactorialUnit_SpreadAcrossCells — 1000 distinct units
// across a balanced 2x2 land in all four cells with reasonable
// balance (200..300 per cell).
func TestEnrollFactorialUnit_SpreadAcrossCells(t *testing.T) {
	db := openDB(t)
	ctx := context.Background()
	id, err := AuthorFactorialFromBytes(ctx, db, []byte(factorialYAML2x2))
	if err != nil {
		t.Fatalf("author: %v", err)
	}
	if err := Ratify(ctx, db, id, "op@x.com"); err != nil {
		t.Fatalf("ratify: %v", err)
	}

	const N = 1000
	for unit := 1; unit <= N; unit++ {
		if _, err := EnrollFactorialUnit(ctx, db, id, "convoy", unit); err != nil {
			t.Fatalf("enroll unit %d: %v", unit, err)
		}
	}

	rows, err := db.QueryContext(ctx, `
		SELECT t.arm_label, COUNT(r.id)
		FROM ExperimentTreatments t
		LEFT JOIN ExperimentRuns r ON r.treatment_id = t.id
		WHERE t.experiment_id = ?
		GROUP BY t.id
		ORDER BY t.id
	`, id)
	if err != nil {
		t.Fatalf("count rows: %v", err)
	}
	defer rows.Close()
	counts := map[string]int{}
	total := 0
	for rows.Next() {
		var arm string
		var n int
		if err := rows.Scan(&arm, &n); err != nil {
			t.Fatalf("scan: %v", err)
		}
		counts[arm] = n
		total += n
	}
	if total != N {
		t.Errorf("total enrollments: got %d, want %d", total, N)
	}
	if len(counts) != 4 {
		t.Errorf("expected 4 cells with counts, got %d (%v)", len(counts), counts)
	}
	// Tolerance window per spec: each cell sees 200..300 of the 1000
	// units (balanced 0.25 weights, +/-30% headroom for hash skew).
	for arm, n := range counts {
		if n < 200 || n > 300 {
			t.Errorf("arm %q saw %d units, want 200..300 (full counts: %v)", arm, n, counts)
		}
	}
}

// TestEnrollFactorialUnit_RejectsNonFactorial — factorial entry
// point against a single-treatment experiment errors with
// ErrNotFactorial.
func TestEnrollFactorialUnit_RejectsNonFactorial(t *testing.T) {
	db := openDB(t)
	ctx := context.Background()
	id, err := AuthorFromBytes(ctx, db, []byte(minimalManifest))
	if err != nil {
		t.Fatalf("author single: %v", err)
	}
	if err := Ratify(ctx, db, id, "op@x.com"); err != nil {
		t.Fatalf("ratify: %v", err)
	}
	_, err = EnrollFactorialUnit(ctx, db, id, "task", 1)
	if err == nil {
		t.Fatalf("expected ErrNotFactorial, got nil")
	}
	if !errors.Is(err, ErrNotFactorial) {
		t.Errorf("expected ErrNotFactorial, got %v", err)
	}
}

// TestEnrollFactorialUnit_Idempotent — second enroll for same
// (experiment, kind, id) returns the same treatment_id and inserts no
// second ExperimentRuns row.
func TestEnrollFactorialUnit_Idempotent(t *testing.T) {
	db := openDB(t)
	ctx := context.Background()
	id, err := AuthorFactorialFromBytes(ctx, db, []byte(factorialYAML2x2))
	if err != nil {
		t.Fatalf("author: %v", err)
	}
	if err := Ratify(ctx, db, id, "op@x.com"); err != nil {
		t.Fatalf("ratify: %v", err)
	}
	first, err := EnrollFactorialUnit(ctx, db, id, "convoy", 7777)
	if err != nil {
		t.Fatalf("first enroll: %v", err)
	}
	for i := 0; i < 4; i++ {
		again, err := EnrollFactorialUnit(ctx, db, id, "convoy", 7777)
		if err != nil {
			t.Fatalf("repeat enroll %d: %v", i, err)
		}
		if again != first {
			t.Errorf("idempotency broken: first=%d again=%d", first, again)
		}
	}
	var n int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM ExperimentRuns WHERE experiment_id = ? AND natural_unit_id = ?`, id, 7777).Scan(&n); err != nil {
		t.Fatalf("count rows: %v", err)
	}
	if n != 1 {
		t.Errorf("expected 1 ExperimentRuns row, got %d", n)
	}
}

// TestTerminateFactorial_PerCellOutcomes — seed runs per cell with
// known success rates, terminate, and assert cell_means_json contains
// all four cells with the expected approximate means.
func TestTerminateFactorial_PerCellOutcomes(t *testing.T) {
	db := openDB(t)
	ctx := context.Background()
	id, err := AuthorFactorialFromBytes(ctx, db, []byte(factorialYAML2x2))
	if err != nil {
		t.Fatalf("author: %v", err)
	}
	if err := Ratify(ctx, db, id, "op@x.com"); err != nil {
		t.Fatalf("ratify: %v", err)
	}

	// Per-cell deterministic seeding. The test uses fixed (cell, rate)
	// pairs and walks ExperimentTreatments by arm_label so the test is
	// independent of treatment_id ordering.
	wantRates := map[string]float64{
		"cell_A_tight": 0.80,
		"cell_A_loose": 0.50,
		"cell_B_tight": 0.60,
		"cell_B_loose": 0.70,
	}
	armIDs := loadArmIDs(t, db, id)
	for arm, rate := range wantRates {
		tID, ok := armIDs[arm]
		if !ok {
			t.Fatalf("arm %q not present in treatments", arm)
		}
		successes := int(100 * rate)
		seedRuns(t, db, id, tID, 100, successes)
	}

	if err := TerminateFactorial(ctx, db, id, "operator_closed"); err != nil {
		t.Fatalf("TerminateFactorial: %v", err)
	}

	var status, reason, cellMeansJSON string
	if err := db.QueryRowContext(ctx, `SELECT status FROM Experiments WHERE id = ?`, id).Scan(&status); err != nil {
		t.Fatalf("SELECT status: %v", err)
	}
	if status != StatusTerminated {
		t.Errorf("status: got %q, want %q", status, StatusTerminated)
	}
	if err := db.QueryRowContext(ctx, `SELECT termination_reason, cell_means_json FROM ExperimentOutcomes WHERE experiment_id = ?`, id).Scan(&reason, &cellMeansJSON); err != nil {
		t.Fatalf("SELECT outcome: %v", err)
	}
	if reason != "operator_closed" {
		t.Errorf("termination_reason: got %q, want operator_closed", reason)
	}

	got := map[string]float64{}
	if err := json.Unmarshal([]byte(cellMeansJSON), &got); err != nil {
		t.Fatalf("unmarshal cell_means_json: %v (raw: %s)", err, cellMeansJSON)
	}
	// The cell key shape is "factor=level,factor=level" in factor
	// declaration order — see factorial_lifecycle.go cellJSONToKey.
	want := map[string]float64{
		"prompt=A,rules=tight": 0.80,
		"prompt=A,rules=loose": 0.50,
		"prompt=B,rules=tight": 0.60,
		"prompt=B,rules=loose": 0.70,
	}
	if len(got) != len(want) {
		t.Errorf("cell_means_json key count: got %d, want %d (got=%v want=%v)", len(got), len(want), got, want)
	}
	for k, v := range want {
		gv, ok := got[k]
		if !ok {
			t.Errorf("cell %q missing from cell_means_json (got %v)", k, got)
			continue
		}
		if gv < v-0.01 || gv > v+0.01 {
			t.Errorf("cell %q mean: got %v, want ~%v", k, gv, v)
		}
	}
}

// TestTerminateFactorial_RejectsNonFactorial — calling
// TerminateFactorial on a single-treatment experiment errors with
// ErrNotFactorial without flipping the experiment status.
func TestTerminateFactorial_RejectsNonFactorial(t *testing.T) {
	db := openDB(t)
	ctx := context.Background()
	id, err := AuthorFromBytes(ctx, db, []byte(minimalManifest))
	if err != nil {
		t.Fatalf("author: %v", err)
	}
	if err := Ratify(ctx, db, id, "op@x.com"); err != nil {
		t.Fatalf("ratify: %v", err)
	}
	if err := TerminateFactorial(ctx, db, id, ""); err == nil {
		t.Fatalf("expected ErrNotFactorial, got nil")
	} else if !errors.Is(err, ErrNotFactorial) {
		t.Errorf("expected ErrNotFactorial, got %v", err)
	}
	var status string
	db.QueryRowContext(ctx, `SELECT status FROM Experiments WHERE id = ?`, id).Scan(&status)
	if status != StatusRunning {
		t.Errorf("status after rejected terminate: got %q, want %q (must not flip)", status, StatusRunning)
	}
}

// TestTerminateFactorial_CAS — author + (don't ratify) + try to
// terminate. The CAS update refuses to flip and the call errors.
// Idempotence: a second call after a successful terminate also errors.
func TestTerminateFactorial_CAS(t *testing.T) {
	db := openDB(t)
	ctx := context.Background()
	id, err := AuthorFactorialFromBytes(ctx, db, []byte(factorialYAML2x2))
	if err != nil {
		t.Fatalf("author: %v", err)
	}
	// Status is 'authored' — not 'running'. Terminate must refuse.
	if err := TerminateFactorial(ctx, db, id, "operator_closed"); err == nil {
		t.Fatalf("expected error on terminate-from-authored, got nil")
	} else if !strings.Contains(err.Error(), "running") && !strings.Contains(err.Error(), "confirming") {
		t.Errorf("error %q should mention running/confirming", err.Error())
	}
	// Verify status untouched.
	var status string
	db.QueryRowContext(ctx, `SELECT status FROM Experiments WHERE id = ?`, id).Scan(&status)
	if status != StatusAuthored {
		t.Errorf("status after rejected terminate: got %q, want %q", status, StatusAuthored)
	}

	// Now ratify + terminate, then a second terminate must also refuse.
	if err := Ratify(ctx, db, id, "op@x.com"); err != nil {
		t.Fatalf("ratify: %v", err)
	}
	if err := TerminateFactorial(ctx, db, id, "operator_closed"); err != nil {
		t.Fatalf("first terminate: %v", err)
	}
	if err := TerminateFactorial(ctx, db, id, "operator_closed"); err == nil {
		t.Fatalf("expected error on double-terminate, got nil")
	}
}

// TestEnrollFactorialUnit_DifferentExperimentsDifferentCells —
// determinism contract per paired-runs.md: the salt is experiment_id,
// so the same unit lands in different cells across different
// experiments. We assert that at least one of N units lands in
// different cells across two factorial experiments — a very weak
// bound that only fails if the salt was forgotten entirely.
func TestEnrollFactorialUnit_DifferentExperimentsDifferentCells(t *testing.T) {
	db := openDB(t)
	ctx := context.Background()
	idA, err := AuthorFactorialFromBytes(ctx, db, []byte(factorialYAML2x2))
	if err != nil {
		t.Fatalf("author A: %v", err)
	}
	idB, err := AuthorFactorialFromBytes(ctx, db, []byte(factorialYAML2x2))
	if err != nil {
		t.Fatalf("author B: %v", err)
	}
	if err := Ratify(ctx, db, idA, "op@x.com"); err != nil {
		t.Fatalf("ratify A: %v", err)
	}
	if err := Ratify(ctx, db, idB, "op@x.com"); err != nil {
		t.Fatalf("ratify B: %v", err)
	}
	// Across experiments, treatment ids differ — compare arm_label
	// instead so the cell identity is comparable.
	armA := loadArmIDs(t, db, idA)
	armB := loadArmIDs(t, db, idB)
	labelA := func(tid int) string {
		for k, v := range armA {
			if v == tid {
				return k
			}
		}
		return ""
	}
	labelB := func(tid int) string {
		for k, v := range armB {
			if v == tid {
				return k
			}
		}
		return ""
	}

	differ := 0
	for unit := 1; unit <= 200; unit++ {
		tA, _ := EnrollFactorialUnit(ctx, db, idA, "convoy", unit)
		tB, _ := EnrollFactorialUnit(ctx, db, idB, "convoy", unit)
		if labelA(tA) != labelB(tB) {
			differ++
		}
	}
	if differ == 0 {
		t.Errorf("salt missing: same units landed in identical cells across both experiments")
	}
}
