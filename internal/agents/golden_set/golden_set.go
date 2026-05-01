// Package golden_set implements the periodic prompt-vs-fixture
// evaluation framework. A weekly dog runs each agent's current prompt
// against a curated set of input fixtures with known-correct outputs;
// accuracy regression below threshold alerts the operator.
//
// Two sources of fixtures keep the set honest:
//
//   - Auto-curation from clean-shipping convoys. CurateFromCleanShipping
//     scans terminated convoys with no rework, no escalations, no
//     fix-task spawn cycles, and lifts each task's input + accepted
//     output into a GoldenSetFixtures row with source='auto-clean-shipping'.
//
//   - Operator-curated negative examples. AddManualFixture lets the
//     operator pin known-wrong-answers (or known-tricky-edge-cases) so
//     auto-curation tautologies — fixtures the agent already produces
//     by construction — don't dominate the set.
//
// Package surface (filled in by sub-agent C):
//   - curator.go     — CurateFromCleanShipping, AddManualFixture
//   - evaluator.go   — RunEvaluationCycle, ReportAccuracyTrend
//   - dog.go         — dogGoldenSetEvaluator (weekly cadence)
package golden_set

import (
	"errors"
)

// FixtureSource enumerates the provenance of a GoldenSetFixtures row.
// Persisted as the `source` column.
type FixtureSource string

const (
	// SourceAutoCleanShipping — fixture lifted from a convoy that
	// shipped without rework or escalation. Empirical positive.
	SourceAutoCleanShipping FixtureSource = "auto-clean-shipping"

	// SourceOperatorCurated — fixture authored by an operator, usually
	// to capture a known-tricky case or a known-wrong-answer trap.
	SourceOperatorCurated FixtureSource = "operator-curated"

	// SourceArchaeologist — fixture lifted from historical
	// pre-paired-runs traffic by the Archaeologist agent.
	SourceArchaeologist FixtureSource = "archaeologist"
)

// Fixture is the GoldenSetFixtures row materialized as a Go value.
type Fixture struct {
	ID             int64
	Agent          string
	Input          string
	ExpectedOutput string
	Source         FixtureSource
	CuratedAt      string
	CuratedBy      string
	RetiredAt      string
}

// Evaluation is the GoldenSetEvaluations row materialized as a Go value.
type Evaluation struct {
	ID            int64
	Agent         string
	PromptVersion string
	FixtureID     int64
	ActualOutput  string
	AccuracyScore float64
	EvaluatedAt   string
}

// AccuracyTrend is the rolling-week aggregation that
// ReportAccuracyTrend produces. Surface to the operator as a
// dashboard panel.
type AccuracyTrend struct {
	Agent         string
	PromptVersion string
	WeekStart     string
	MeanAccuracy  float64
	SampleCount   int
	// RegressionFromPriorWeek is the negative-going delta from the
	// previous week's mean accuracy. > 0 means regression. Sub-agent C
	// fixes the alert threshold.
	RegressionFromPriorWeek float64
}

// ErrNoFixtures is returned by RunEvaluationCycle when the agent has no
// non-retired fixtures to evaluate. Callers should treat this as
// "skip" rather than "fail" — a new agent reasonably has no golden
// set on its first evaluation cycle.
var ErrNoFixtures = errors.New("golden_set: no non-retired fixtures for agent")

// Production implementations of CurateFromCleanShipping,
// AddManualFixture, RunEvaluationCycle, and ReportAccuracyTrend live
// in curator.go and evaluator.go. The skeleton stubs that previously
// lived here have been replaced.
