package golden_set

import (
	"context"
	"database/sql"
	"errors"
	"math"
	"testing"

	"force-orchestrator/internal/store"
)

// alwaysReturn returns a deterministic actual_output for every fixture.
func alwaysReturn(out string) EvaluatorFn {
	return func(ctx context.Context, fx Fixture) (string, error) { return out, nil }
}

// echo returns each fixture's expected_output verbatim — perfect 1.0 every time.
func echoExpected() EvaluatorFn {
	return func(ctx context.Context, fx Fixture) (string, error) { return fx.ExpectedOutput, nil }
}

func seedFixtures(t *testing.T, db *sql.DB, agent string, fixtures []Fixture) {
	t.Helper()
	for _, f := range fixtures {
		_, err := db.Exec(`
			INSERT INTO GoldenSetFixtures (agent, input, expected_output, source, curated_by)
			VALUES (?, ?, ?, ?, ?)`,
			agent, f.Input, f.ExpectedOutput, string(f.Source), f.CuratedBy)
		if err != nil {
			t.Fatalf("seed fixture: %v", err)
		}
	}
}

func TestRunEvaluationCycle_ScoresAllFixtures(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	seedFixtures(t, db, "council", []Fixture{
		{Input: `{"x":1}`, ExpectedOutput: `{"approved":true}`, Source: SourceAutoCleanShipping},
		{Input: `{"x":2}`, ExpectedOutput: `{"approved":false}`, Source: SourceAutoCleanShipping},
		{Input: `{"x":3}`, ExpectedOutput: `{"approved":true}`, Source: SourceOperatorCurated},
	})

	n, err := RunEvaluationCycleWith(context.Background(), db, "council", "council-v3",
		echoExpected(), scoreExactMatch)
	if err != nil {
		t.Fatalf("RunEvaluationCycleWith: %v", err)
	}
	if n != 3 {
		t.Fatalf("want 3 evaluations, got %d", n)
	}
	var total int
	db.QueryRow(`SELECT COUNT(*) FROM GoldenSetEvaluations WHERE agent='council' AND prompt_version='council-v3'`).Scan(&total)
	if total != 3 {
		t.Fatalf("want 3 rows in GoldenSetEvaluations, got %d", total)
	}
	// All three must be 1.0 (echoExpected matches exactly).
	rows, err := db.Query(`SELECT accuracy_score FROM GoldenSetEvaluations WHERE agent='council'`)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var score float64
		rows.Scan(&score)
		if math.Abs(score-1.0) > 1e-9 {
			t.Fatalf("echoExpected score must be 1.0, got %v", score)
		}
	}
}

func TestRunEvaluationCycle_DeterministicOnSameFixtures(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	seedFixtures(t, db, "council", []Fixture{
		{Input: `{"x":1}`, ExpectedOutput: `{"approved":true}`, Source: SourceAutoCleanShipping},
	})

	// Run twice with the same evaluator; scores must be identical.
	if _, err := RunEvaluationCycleWith(context.Background(), db, "council", "council-v3",
		alwaysReturn(`{"approved":false}`), scoreExactMatch); err != nil {
		t.Fatalf("first cycle: %v", err)
	}
	if _, err := RunEvaluationCycleWith(context.Background(), db, "council", "council-v3",
		alwaysReturn(`{"approved":false}`), scoreExactMatch); err != nil {
		t.Fatalf("second cycle: %v", err)
	}
	rows, err := db.Query(`SELECT accuracy_score FROM GoldenSetEvaluations ORDER BY id ASC`)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer rows.Close()
	var scores []float64
	for rows.Next() {
		var s float64
		rows.Scan(&s)
		scores = append(scores, s)
	}
	if len(scores) != 2 {
		t.Fatalf("want 2 scores, got %d", len(scores))
	}
	if scores[0] != scores[1] {
		t.Fatalf("determinism broken: scores=%v", scores)
	}
	if scores[0] != 0.0 {
		// alwaysReturn(false) vs expected true → mismatch → 0.0
		t.Fatalf("expected 0.0 (mismatch), got %v", scores[0])
	}
}

func TestRunEvaluationCycle_ErrNoFixtures(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	_, err := RunEvaluationCycleWith(context.Background(), db, "council", "council-v3",
		echoExpected(), scoreExactMatch)
	if !errors.Is(err, ErrNoFixtures) {
		t.Fatalf("no fixtures: want ErrNoFixtures, got %v", err)
	}
}

func TestRunEvaluationCycle_RejectsMissingInputs(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	if _, err := RunEvaluationCycleWith(context.Background(), nil, "x", "y", echoExpected(), nil); err == nil {
		t.Fatalf("nil db: want error")
	}
	if _, err := RunEvaluationCycleWith(context.Background(), db, "", "y", echoExpected(), nil); err == nil {
		t.Fatalf("empty agent: want error")
	}
	if _, err := RunEvaluationCycleWith(context.Background(), db, "x", "", echoExpected(), nil); err == nil {
		t.Fatalf("empty promptVersion: want error")
	}
	if _, err := RunEvaluationCycleWith(context.Background(), db, "x", "y", nil, nil); err == nil {
		t.Fatalf("nil evaluator: want error")
	}
}

func TestReportAccuracyTrend_BaselineWeek(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	// Seed evaluations with hand-crafted scores. All in the same week
	// so we get one trend row.
	_, err := db.Exec(`INSERT INTO GoldenSetEvaluations (agent, prompt_version, fixture_id, actual_output, accuracy_score, evaluated_at) VALUES
		('council', 'council-v3', 1, '{}', 1.0, '2026-04-27 12:00:00'),
		('council', 'council-v3', 2, '{}', 0.5, '2026-04-28 12:00:00'),
		('council', 'council-v3', 3, '{}', 0.0, '2026-04-29 12:00:00')`)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	trends, err := ReportAccuracyTrend(context.Background(), db, "council", "")
	if err != nil {
		t.Fatalf("ReportAccuracyTrend: %v", err)
	}
	if len(trends) != 1 {
		t.Fatalf("want 1 weekly trend, got %d", len(trends))
	}
	tr := trends[0]
	if tr.SampleCount != 3 {
		t.Fatalf("SampleCount: want 3, got %d", tr.SampleCount)
	}
	want := (1.0 + 0.5 + 0.0) / 3.0
	if math.Abs(tr.MeanAccuracy-want) > 1e-9 {
		t.Fatalf("MeanAccuracy: want %v got %v", want, tr.MeanAccuracy)
	}
	if tr.RegressionFromPriorWeek != 0.0 {
		t.Fatalf("baseline week: regression must be 0.0, got %v", tr.RegressionFromPriorWeek)
	}
}

func TestReportAccuracyTrend_RegressionAlerts(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	// Week 1: 2026-04-20 (Mon) — perfect.
	// Week 2: 2026-04-27 (Mon) — degraded.
	_, err := db.Exec(`INSERT INTO GoldenSetEvaluations (agent, prompt_version, fixture_id, actual_output, accuracy_score, evaluated_at) VALUES
		('council', 'council-v3', 1, '{}', 1.0, '2026-04-20 12:00:00'),
		('council', 'council-v3', 2, '{}', 1.0, '2026-04-21 12:00:00'),
		('council', 'council-v3', 1, '{}', 0.5, '2026-04-27 12:00:00'),
		('council', 'council-v3', 2, '{}', 0.5, '2026-04-28 12:00:00')`)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	trends, err := ReportAccuracyTrend(context.Background(), db, "council", "")
	if err != nil {
		t.Fatalf("ReportAccuracyTrend: %v", err)
	}
	if len(trends) != 2 {
		t.Fatalf("want 2 weekly trends (baseline + regression), got %d", len(trends))
	}
	// Trends are time-ordered; week 2 must show RegressionFromPriorWeek = 1.0 - 0.5 = 0.5
	if math.Abs(trends[1].RegressionFromPriorWeek-0.5) > 1e-9 {
		t.Fatalf("week 2 regression: want 0.5, got %v", trends[1].RegressionFromPriorWeek)
	}
}

func TestReportAccuracyTrend_RejectsMissingInputs(t *testing.T) {
	if _, err := ReportAccuracyTrend(context.Background(), nil, "x", ""); err == nil {
		t.Fatalf("nil db: want error")
	}
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	if _, err := ReportAccuracyTrend(context.Background(), db, "", ""); err == nil {
		t.Fatalf("empty agent: want error")
	}
}

func TestRunEvaluationCycle_StubReturnsErrorForProductionPath(t *testing.T) {
	// Production RunEvaluationCycle has no wired EvaluatorFn yet — it
	// fails closed so callers don't accidentally believe a no-op
	// evaluation succeeded.
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	if _, err := RunEvaluationCycle(context.Background(), db, "council", "council-v3"); err == nil {
		t.Fatalf("production RunEvaluationCycle without EvaluatorFn must fail closed")
	}
}
