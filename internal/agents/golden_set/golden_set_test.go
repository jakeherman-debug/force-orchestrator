package golden_set

import (
	"context"
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

// The skeleton stubs that previously lived here have been replaced
// by production implementations in curator.go and evaluator.go. These
// tests now live in curator_test.go and evaluator_test.go.
//
// We keep one fail-closed assertion to prove the production functions
// validate inputs (no silent zero-returns on nil DB).
func TestProduction_FailsClosedOnNilDB(t *testing.T) {
	if _, err := CurateFromCleanShipping(context.Background(), nil, "council"); err == nil {
		t.Errorf("CurateFromCleanShipping nil db: want error")
	}
	if _, err := AddManualFixture(context.Background(), nil, "council", "x", "y", "op"); err == nil {
		t.Errorf("AddManualFixture nil db: want error")
	}
	if _, err := ReportAccuracyTrend(context.Background(), nil, "council", ""); err == nil {
		t.Errorf("ReportAccuracyTrend nil db: want error")
	}
}
