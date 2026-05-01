// Factorial extension of the Bayesian Beta-Binomial framework — D3
// Phase 4. Single-treatment math (NewPosterior, ComparePosteriors,
// DecideOutcome) lives in bayesian_beta_binomial.go and is unchanged.
// This file adds:
//
//   - ComputeMainEffects: per-(factor, level) marginal posteriors
//     (pool successes/trials across all cells where the factor takes
//     that level, then apply Beta-Binomial inference).
//
// The 2-way interaction and decision-rule layers land in follow-up
// commits.
//
// All inference reads the on-disk experiment shape (Experiments.
// factors_json, ExperimentRuns.cell_json, ExperimentRuns.score). It
// does NOT depend on the factorial-lifecycle authoring path's exact
// Go symbols — the analyzer parses the same canonical JSON the
// authoring path emits.
//
// Determinism: every Monte Carlo here pins its RNG seed off
// DecisionRule.RandomSeed (offset per-(factor, level) so different
// estimates don't collapse onto identical sample paths). Two reads
// of the same table state under the same rule produce identical
// outputs.

package analysis

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
)

// ──────────────────────────────────────────────────────────────────────────
// Public types
// ──────────────────────────────────────────────────────────────────────────

// MainEffect is the per-(factor, level) marginal effect: averaged
// across all other factors, what's the success-rate posterior at this
// level (and how does it compare to the reference level — the FIRST
// declared level for the factor in factors_json).
type MainEffect struct {
	Factor            string
	Level             string
	Posterior         BetaBinomialPosterior
	ProbBetterThanRef float64 // P(this level > reference level), Monte Carlo
	Mean              float64
}

// ──────────────────────────────────────────────────────────────────────────
// Internal: shared shape — a parsed factorial experiment
// ──────────────────────────────────────────────────────────────────────────

type factorSpec struct {
	Name   string   `json:"name"`
	Levels []string `json:"levels"`
}

// cellAggregate is the pooled (successes, trials) for one cell
// identified by its canonical-cell-key.
type cellAggregate struct {
	Cell      map[string]string
	Successes int
	Trials    int
}

// factorialView is the in-memory, normalised projection of one
// experiment's runs needed by every analyzer here. Built once per
// public entry point so we read the database in a single sweep.
type factorialView struct {
	ExperimentID int
	Factors      []factorSpec
	// Cells indexed by canonical key (canonicalCellKey ordered by
	// Factors). Cells with zero runs are still present (Trials=0).
	Cells map[string]*cellAggregate
}

// canonicalCellKey serialises a cell map into a stable, comparable
// string. Mirrors experiments.canonicalCellKey shape but lives here
// so the analysis package has no import dependency on the experiments
// package (preserving the layered shape — analysis is the sink, not
// the source).
func canonicalCellKey(factors []factorSpec, cell map[string]string) string {
	if len(factors) == 0 {
		return "{}"
	}
	parts := make([]string, 0, len(factors))
	for _, f := range factors {
		parts = append(parts, fmt.Sprintf("%s=%s", f.Name, cell[f.Name]))
	}
	return "{" + joinParts(parts, ",") + "}"
}

func joinParts(parts []string, sep string) string {
	out := ""
	for i, p := range parts {
		if i > 0 {
			out += sep
		}
		out += p
	}
	return out
}

// loadFactorialView reads Experiments.factors_json, then sweeps
// ExperimentRuns rows (filtering for completed runs with non-NULL
// score), groups by canonical cell key, and counts (score >= 0.5) as
// successes. Score is interpreted as Bernoulli per the framework's
// Phase 2 convention (see lifecycle.go computeOutcome).
func loadFactorialView(ctx context.Context, db *sql.DB, experimentID int) (*factorialView, error) {
	if db == nil {
		return nil, fmt.Errorf("loadFactorialView: db is nil")
	}
	if experimentID <= 0 {
		return nil, fmt.Errorf("loadFactorialView: experimentID must be > 0 (got %d)", experimentID)
	}

	var factorsJSON string
	err := db.QueryRowContext(ctx,
		`SELECT IFNULL(factors_json, '[]') FROM Experiments WHERE id = ?`,
		experimentID,
	).Scan(&factorsJSON)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("loadFactorialView: experiment %d not found", experimentID)
	}
	if err != nil {
		return nil, fmt.Errorf("loadFactorialView: read factors_json: %w", err)
	}
	var factors []factorSpec
	if err := json.Unmarshal([]byte(factorsJSON), &factors); err != nil {
		return nil, fmt.Errorf("loadFactorialView: parse factors_json %q: %w", factorsJSON, err)
	}
	if len(factors) < 2 {
		return nil, fmt.Errorf("loadFactorialView: experiment %d declares %d factors; factorial analysis requires >= 2", experimentID, len(factors))
	}

	view := &factorialView{
		ExperimentID: experimentID,
		Factors:      factors,
		Cells:        map[string]*cellAggregate{},
	}

	// Prime the Cells map with the full cross-product so empty cells
	// are visible to downstream consumers (mean=0, Trials=0).
	for _, cell := range cartesianCells(factors) {
		key := canonicalCellKey(factors, cell)
		view.Cells[key] = &cellAggregate{
			Cell:      cell,
			Successes: 0,
			Trials:    0,
		}
	}

	rows, err := db.QueryContext(ctx, `
		SELECT IFNULL(cell_json, '{}'), score
		FROM ExperimentRuns
		WHERE experiment_id = ? AND score IS NOT NULL
	`, experimentID)
	if err != nil {
		return nil, fmt.Errorf("loadFactorialView: query runs: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var cellJSON string
		var score sql.NullFloat64
		if err := rows.Scan(&cellJSON, &score); err != nil {
			return nil, fmt.Errorf("loadFactorialView: scan: %w", err)
		}
		if !score.Valid {
			continue
		}
		var cell map[string]string
		if err := json.Unmarshal([]byte(cellJSON), &cell); err != nil {
			// A run with a malformed cell_json is opaque to the
			// factorial analyzer; skip rather than crash the whole
			// termination flow. Caller can audit by reading
			// ExperimentRuns directly if needed.
			continue
		}
		// Drop cells that don't pin every declared factor; those
		// rows belong to single-treatment experiments or to ill-formed
		// authoring paths and have no place in the factorial
		// projection.
		complete := true
		for _, f := range factors {
			if _, ok := cell[f.Name]; !ok {
				complete = false
				break
			}
		}
		if !complete {
			continue
		}
		key := canonicalCellKey(factors, cell)
		agg, ok := view.Cells[key]
		if !ok {
			// Unknown cell — could be a level not in factors_json.
			// Skip; the factorial validator should have caught this
			// at authoring time.
			continue
		}
		agg.Trials++
		if score.Float64 >= 0.5 {
			agg.Successes++
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("loadFactorialView: rows: %w", err)
	}
	return view, nil
}

// cartesianCells emits the full cross-product of factor levels as a
// slice of cell maps. Order is deterministic (declaration-order
// outer-most, levels declaration-order inner).
func cartesianCells(factors []factorSpec) []map[string]string {
	if len(factors) == 0 {
		return nil
	}
	out := []map[string]string{{}}
	for _, f := range factors {
		next := make([]map[string]string, 0, len(out)*len(f.Levels))
		for _, partial := range out {
			for _, lvl := range f.Levels {
				cp := make(map[string]string, len(partial)+1)
				for k, v := range partial {
					cp[k] = v
				}
				cp[f.Name] = lvl
				next = append(next, cp)
			}
		}
		out = next
	}
	return out
}

// ──────────────────────────────────────────────────────────────────────────
// ComputeMainEffects
// ──────────────────────────────────────────────────────────────────────────

// ComputeMainEffects returns one MainEffect per (factor, level) for
// the given experiment. For each (factor, level), pools successes and
// trials across all cells where the factor takes that level (the
// marginal posterior), and applies Beta-Binomial inference under the
// rule's prior. ProbBetterThanRef is the Monte Carlo P(level > ref)
// against the FIRST declared level of the factor.
func ComputeMainEffects(ctx context.Context, db *sql.DB, experimentID int) ([]MainEffect, error) {
	view, err := loadFactorialView(ctx, db, experimentID)
	if err != nil {
		return nil, err
	}
	rule := DecisionRule{}.withDefaults()
	return computeMainEffectsFromView(view, rule), nil
}

// ComputeMainEffectsWithRule is the seed-pinnable variant — tests
// pass a fixed RandomSeed so ProbBetterThanRef is deterministic.
func ComputeMainEffectsWithRule(ctx context.Context, db *sql.DB, experimentID int, rule DecisionRule) ([]MainEffect, error) {
	view, err := loadFactorialView(ctx, db, experimentID)
	if err != nil {
		return nil, err
	}
	rule = rule.withDefaults()
	return computeMainEffectsFromView(view, rule), nil
}

func computeMainEffectsFromView(view *factorialView, rule DecisionRule) []MainEffect {
	out := []MainEffect{}
	for _, f := range view.Factors {
		// Per-level pooled (successes, trials).
		pooled := map[string]struct{ s, t int }{}
		for _, cell := range view.Cells {
			lvl := cell.Cell[f.Name]
			p := pooled[lvl]
			p.s += cell.Successes
			p.t += cell.Trials
			pooled[lvl] = p
		}
		ref := f.Levels[0]
		refPost := NewPosterior(rule.PriorAlpha, rule.PriorBeta, pooled[ref].s, pooled[ref].t)
		for i, lvl := range f.Levels {
			p := pooled[lvl]
			lvlPost := NewPosterior(rule.PriorAlpha, rule.PriorBeta, p.s, p.t)
			eff := MainEffect{
				Factor:    f.Name,
				Level:     lvl,
				Posterior: *lvlPost,
				Mean:      lvlPost.Mean(),
			}
			if i == 0 {
				// Reference level is by definition equal to itself.
				eff.ProbBetterThanRef = 0.5
			} else {
				// Pin a per-(factor, level) seed offset so different
				// factors don't collapse onto the same MC samples.
				seed := rule.RandomSeed + int64(hashStringSeed(f.Name+"|"+lvl))
				eff.ProbBetterThanRef = compareWithSeed(lvlPost, refPost, rule.MonteCarloSamples, seed)
			}
			out = append(out, eff)
		}
	}
	return out
}

// hashStringSeed turns a factor/level identifier into an int64 seed
// offset — small, deterministic, and collision-tolerant (we only use
// it to spread MC sample paths across distinct estimates).
func hashStringSeed(s string) uint32 {
	var h uint32 = 2166136261
	for i := 0; i < len(s); i++ {
		h ^= uint32(s[i])
		h *= 16777619
	}
	return h
}
