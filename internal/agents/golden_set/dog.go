package golden_set

import (
	"context"
	"database/sql"
	"fmt"
)

// Gate is the small interface the weekly evaluator dog consults
// before kicking off an evaluation cycle. Callers in `package agents`
// supply `agents.IsEstopped` and `agents.SpendCapExceeded` via a
// satisfying adapter; the golden_set package itself stays
// dependency-free.
type Gate interface {
	IsEstopped() bool
	SpendCapExceeded() bool
}

// EvaluatorByAgent maps an agent name to the EvaluatorFn that
// invokes that agent's prompt against a fixture's input. Production
// wiring lives in `package agents` (one EvaluatorFn per agent that
// shells out via claude.AskClaudeCLIContext).
type EvaluatorByAgent map[string]EvaluatorFn

// PromptVersionByAgent records the current prompt-version tag for
// each agent. The dog persists this verbatim into
// GoldenSetEvaluations.prompt_version so per-version trend analysis
// can correlate.
type PromptVersionByAgent map[string]string

// RunWeeklyEvaluatorDog runs RunEvaluationCycleWith for every agent
// in evaluators (gated by `gate`). Returns a per-agent count of
// fixtures evaluated. Failures on individual agents are logged but do
// NOT halt the dog — one agent's missing fixtures shouldn't starve
// the rest.
//
// Anti-cheat: respects IsEstopped + SpendCapExceeded; both bail with
// no evaluations performed.
func RunWeeklyEvaluatorDog(ctx context.Context, db *sql.DB,
	evaluators EvaluatorByAgent, versions PromptVersionByAgent, gate Gate,
) (map[string]int, error) {
	if db == nil {
		return nil, fmt.Errorf("golden_set.RunWeeklyEvaluatorDog: db is required")
	}
	if gate != nil {
		if gate.IsEstopped() {
			return nil, fmt.Errorf("golden_set.RunWeeklyEvaluatorDog: estop active — skipping")
		}
		if gate.SpendCapExceeded() {
			return nil, fmt.Errorf("golden_set.RunWeeklyEvaluatorDog: spend cap exceeded — skipping")
		}
	}
	out := make(map[string]int, len(evaluators))
	for agent, eval := range evaluators {
		version := versions[agent]
		if version == "" {
			continue
		}
		n, err := RunEvaluationCycleWith(ctx, db, agent, version, eval, scoreExactMatch)
		if err != nil {
			// Log-only: don't halt the whole dog on one agent's failure.
			out[agent] = 0
			continue
		}
		out[agent] = n
	}
	return out, nil
}
