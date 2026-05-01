package golden_set

import (
	"context"
	"errors"
	"testing"
)

func TestFixture_FieldsCompile(t *testing.T) {
	f := Fixture{
		ID:             1,
		Agent:          "council",
		Input:          `{"task_id":42,"diff":"..."}`,
		ExpectedOutput: `{"approved":true}`,
		Source:         SourceAutoCleanShipping,
		CuratedBy:      "system",
	}
	if f.Source != SourceAutoCleanShipping {
		t.Fatalf("Fixture source round-trip broken: %+v", f)
	}
}

func TestEvaluation_FieldsCompile(t *testing.T) {
	e := Evaluation{
		ID:            1,
		Agent:         "council",
		PromptVersion: "council-v3",
		FixtureID:     5,
		ActualOutput:  `{"approved":true}`,
		AccuracyScore: 1.0,
	}
	if e.AccuracyScore < 0 || e.AccuracyScore > 1 {
		t.Fatalf("Evaluation.AccuracyScore must clamp to [0,1]: %v", e.AccuracyScore)
	}
}

func TestAccuracyTrend_FieldsCompile(t *testing.T) {
	tr := AccuracyTrend{
		Agent:                   "council",
		PromptVersion:           "council-v3",
		WeekStart:               "2026-04-27",
		MeanAccuracy:            0.92,
		SampleCount:             24,
		RegressionFromPriorWeek: 0.05,
	}
	if tr.RegressionFromPriorWeek <= 0 {
		// Sanity: positive value indicates regression by convention.
		t.Logf("Sample trend has no regression: %+v", tr)
	}
}

func TestSourceConstants(t *testing.T) {
	// Anchor source values to what the schema documents in
	// schema/schema.sql:GoldenSetFixtures.source comments.
	if string(SourceAutoCleanShipping) != "auto-clean-shipping" ||
		string(SourceOperatorCurated) != "operator-curated" ||
		string(SourceArchaeologist) != "archaeologist" {
		t.Fatalf("FixtureSource constants drift from schema-documented values: auto=%q operator=%q archaeologist=%q",
			SourceAutoCleanShipping, SourceOperatorCurated, SourceArchaeologist)
	}
}

func TestCurateFromCleanShipping_Stub_ReturnsZero(t *testing.T) {
	n, err := CurateFromCleanShipping(context.Background(), nil, "council")
	if err != nil {
		t.Fatalf("CurateFromCleanShipping stub: want nil error, got %v", err)
	}
	if n != 0 {
		t.Fatalf("CurateFromCleanShipping stub: want 0 inserts, got %d", n)
	}
}

func TestAddManualFixture_Stub_ReturnsZero(t *testing.T) {
	id, err := AddManualFixture(context.Background(), nil, "council", "input", "expected", "operator@example.com")
	if err != nil {
		t.Fatalf("AddManualFixture stub: want nil error, got %v", err)
	}
	if id != 0 {
		t.Fatalf("AddManualFixture stub: want 0 id, got %d", id)
	}
}

func TestRunEvaluationCycle_Stub_ReturnsErrNoFixtures(t *testing.T) {
	_, err := RunEvaluationCycle(context.Background(), nil, "council", "council-v3")
	if !errors.Is(err, ErrNoFixtures) {
		t.Fatalf("RunEvaluationCycle stub: want ErrNoFixtures, got %v", err)
	}
}

func TestReportAccuracyTrend_Stub_ReturnsEmpty(t *testing.T) {
	rows, err := ReportAccuracyTrend(context.Background(), nil, "council", "")
	if err != nil {
		t.Fatalf("ReportAccuracyTrend stub: want nil error, got %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("ReportAccuracyTrend stub: want 0 rows, got %d", len(rows))
	}
}
