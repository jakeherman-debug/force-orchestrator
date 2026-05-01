package golden_set

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"force-orchestrator/internal/store"
)

// EvaluatorFn is the per-fixture LLM-call function injected at the
// call site. Real production wiring loads the agent's profile and
// shells out via claude.AskClaudeCLIContext. Tests inject a
// deterministic stub for hand-computed accuracy assertions.
type EvaluatorFn func(ctx context.Context, fx Fixture) (actual string, err error)

// AccuracyFn scores actual against expected and returns a 0.0–1.0
// score. Default implementation uses scoreExactMatch (1.0 if
// whitespace-stripped equal, else 0.0); call sites that want
// LLM-judge or token-overlap scoring inject their own.
type AccuracyFn func(actual, expected string) float64

// scoreExactMatch is the baseline accuracy scorer: 1.0 on
// whitespace-insensitive exact match, else 0.0.
func scoreExactMatch(actual, expected string) float64 {
	a := strings.Join(strings.Fields(actual), "")
	b := strings.Join(strings.Fields(expected), "")
	if a == b {
		return 1.0
	}
	return 0.0
}

// RunEvaluationCycleWith runs `evaluator` against every non-retired
// fixture for `agent`, scores via `scorer`, and persists results to
// GoldenSetEvaluations. Returns the count of evaluations performed.
//
// The deterministic-on-same-fixtures invariant is enforced by the
// implementation: same fixtures + same evaluator + same scorer must
// produce the same accuracy_score (i.e., we don't sneak time/random
// noise into the score path).
func RunEvaluationCycleWith(ctx context.Context, db *sql.DB, agent, promptVersion string,
	evaluator EvaluatorFn, scorer AccuracyFn,
) (int, error) {
	if db == nil {
		return 0, fmt.Errorf("golden_set.RunEvaluationCycleWith: db is required")
	}
	if agent == "" || promptVersion == "" {
		return 0, fmt.Errorf("golden_set.RunEvaluationCycleWith: agent and promptVersion are required")
	}
	if evaluator == nil {
		return 0, fmt.Errorf("golden_set.RunEvaluationCycleWith: evaluator function is required")
	}
	if scorer == nil {
		scorer = scoreExactMatch
	}

	rows, err := db.QueryContext(ctx, `
		SELECT id, agent, input, expected_output, source,
		       IFNULL(curated_at,''), IFNULL(curated_by,''), IFNULL(retired_at,'')
		FROM GoldenSetFixtures
		WHERE agent = ? AND IFNULL(retired_at,'') = ''
		ORDER BY id ASC`, agent,
	)
	if err != nil {
		return 0, fmt.Errorf("golden_set.RunEvaluationCycleWith: query fixtures: %w", err)
	}
	defer rows.Close()

	var fixtures []Fixture
	for rows.Next() {
		var f Fixture
		var src string
		if err := rows.Scan(&f.ID, &f.Agent, &f.Input, &f.ExpectedOutput,
			&src, &f.CuratedAt, &f.CuratedBy, &f.RetiredAt); err != nil {
			return 0, fmt.Errorf("golden_set.RunEvaluationCycleWith: scan fixture: %w", err)
		}
		f.Source = FixtureSource(src)
		fixtures = append(fixtures, f)
	}
	if rErr := rows.Err(); rErr != nil {
		return 0, fmt.Errorf("golden_set.RunEvaluationCycleWith: rows iteration: %w", rErr)
	}
	if len(fixtures) == 0 {
		return 0, ErrNoFixtures
	}

	count := 0
	for _, fx := range fixtures {
		actual, evalErr := evaluator(ctx, fx)
		if evalErr != nil {
			return count, fmt.Errorf("golden_set.RunEvaluationCycleWith: evaluator failed on fixture %d: %w", fx.ID, evalErr)
		}
		score := scorer(actual, fx.ExpectedOutput)
		_, err := db.ExecContext(ctx, `
			INSERT INTO GoldenSetEvaluations
				(agent, prompt_version, fixture_id, actual_output, accuracy_score, evaluated_at)
			VALUES (?, ?, ?, ?, ?, ?)`,
			agent, promptVersion, fx.ID, actual, score, store.NowSQLite(),
		)
		if err != nil {
			return count, fmt.Errorf("golden_set.RunEvaluationCycleWith: insert evaluation: %w", err)
		}
		count++
	}
	return count, nil
}

// RunEvaluationCycle is the production entry point — at the moment it
// fails closed (no production EvaluatorFn is wired yet). Tests use
// RunEvaluationCycleWith directly. Call sites that want production
// evaluation pass an EvaluatorFn that wraps the agent's claude-CLI
// call.
func RunEvaluationCycle(ctx context.Context, db *sql.DB, agent, promptVersion string) (int, error) {
	return 0, errors.New("golden_set.RunEvaluationCycle: no production EvaluatorFn wired; use RunEvaluationCycleWith")
}

// ReportAccuracyTrend aggregates per-week mean accuracy for an agent
// + prompt-version combo. sinceDate filters to evaluations on or
// after the given RFC3339 date; pass "" to aggregate the full
// history.
//
// Each returned AccuracyTrend row represents one ISO week (Mon-Sun)
// of evaluations. RegressionFromPriorWeek is a positive value when
// the current week's MeanAccuracy is BELOW the prior week's;
// negative or zero otherwise.
func ReportAccuracyTrend(ctx context.Context, db *sql.DB, agent, sinceDate string) ([]AccuracyTrend, error) {
	if db == nil {
		return nil, fmt.Errorf("golden_set.ReportAccuracyTrend: db is required")
	}
	if agent == "" {
		return nil, fmt.Errorf("golden_set.ReportAccuracyTrend: agent is required")
	}

	q := `
		SELECT prompt_version, evaluated_at, accuracy_score
		FROM GoldenSetEvaluations
		WHERE agent = ?`
	args := []any{agent}
	if sinceDate != "" {
		q += ` AND evaluated_at >= ?`
		args = append(args, sinceDate)
	}
	q += ` ORDER BY evaluated_at ASC`

	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("golden_set.ReportAccuracyTrend: query: %w", err)
	}
	defer rows.Close()

	type bucket struct {
		sum   float64
		count int
	}
	// key = (prompt_version, weekStartISODate)
	weekly := map[string]*bucket{}
	versions := map[string]string{}
	weekStarts := map[string]string{}
	order := []string{}
	seen := map[string]bool{}

	for rows.Next() {
		var pv, ts string
		var score float64
		if err := rows.Scan(&pv, &ts, &score); err != nil {
			return nil, err
		}
		ws := weekStartUTC(ts)
		key := pv + "::" + ws
		b, ok := weekly[key]
		if !ok {
			b = &bucket{}
			weekly[key] = b
			versions[key] = pv
			weekStarts[key] = ws
		}
		b.sum += score
		b.count++
		if !seen[key] {
			order = append(order, key)
			seen[key] = true
		}
	}
	if rErr := rows.Err(); rErr != nil {
		return nil, fmt.Errorf("golden_set.ReportAccuracyTrend: rows iteration: %w", rErr)
	}

	out := make([]AccuracyTrend, 0, len(order))
	priorMean := map[string]float64{} // per-version prior week's mean
	for _, key := range order {
		b := weekly[key]
		mean := 0.0
		if b.count > 0 {
			mean = b.sum / float64(b.count)
		}
		pv := versions[key]
		regression := 0.0
		if prior, ok := priorMean[pv]; ok {
			regression = prior - mean // positive means regression
		}
		priorMean[pv] = mean
		out = append(out, AccuracyTrend{
			Agent:                   agent,
			PromptVersion:           pv,
			WeekStart:               weekStarts[key],
			MeanAccuracy:            mean,
			SampleCount:             b.count,
			RegressionFromPriorWeek: regression,
		})
	}
	return out, nil
}

// weekStartUTC returns the ISO Monday-week start (YYYY-MM-DD) for the
// given timestamp string in `datetime('now')` shape (UTC). Falls back
// to the input string when parsing fails.
func weekStartUTC(ts string) string {
	t, err := time.Parse("2006-01-02 15:04:05", strings.TrimSpace(ts))
	if err != nil {
		t, err = time.Parse(time.RFC3339, strings.TrimSpace(ts))
	}
	if err != nil {
		return ts
	}
	t = t.UTC()
	// Monday start.
	wd := int(t.Weekday())
	if wd == 0 {
		wd = 7 // Sunday → 7
	}
	monday := t.AddDate(0, 0, -(wd - 1))
	return monday.Format("2006-01-02")
}
