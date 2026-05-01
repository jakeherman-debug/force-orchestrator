// Factorial extension of the Bayesian Beta-Binomial framework — D3
// Phase 4. Single-treatment math (NewPosterior, ComparePosteriors,
// DecideOutcome) lives in bayesian_beta_binomial.go and is unchanged.
// This file adds:
//
//   - ComputeMainEffects: per-(factor, level) marginal posteriors
//     (pool successes/trials across all cells where the factor takes
//     that level, then apply Beta-Binomial inference).
//   - Compute2WayInteractions: per-(factor_a, factor_b, level_a,
//     level_b) cell-level interaction estimates with a Monte Carlo
//     P(|interaction| > min_practical_effect) on the joint posterior.
//
// The decision-rule layer lands in a follow-up commit.
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
	"errors"
	"fmt"
	"math"
	"math/rand"
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

// Interaction2Way is one cell-level contrast for an ordered (factor_a,
// factor_b) pair. The 2-way interaction
//
//	[mean(D1=a, D2=b) - mean(D1=a', D2=b)] -
//	[mean(D1=a, D2=b') - mean(D1=a', D2=b')]
//
// is non-zero when the effect of factor_a depends on factor_b's
// level — i.e. main-effects don't compose additively. We store one
// row per (level_a, level_b) cell so 3+-level factors retain the full
// interaction surface (not just a 2x2 contrast scalar). For 2x2 the
// only non-degenerate row is at (level_a = NON-reference, level_b =
// NON-reference); rows where either level matches the reference
// collapse algebraically to zero but are still emitted so the
// surface is complete.
type Interaction2Way struct {
	FactorA             string
	FactorB             string
	LevelA              string
	LevelB              string
	InteractionEstimate float64 // posterior-mean of the 2-way contrast (see formula above)
	PosteriorAlpha      float64 // pooled successes for the (level_a, level_b) cell + prior_alpha
	PosteriorBeta       float64 // pooled failures  for the (level_a, level_b) cell + prior_beta
	ProbNonzero         float64 // P(|interaction| > min_practical_effect) under joint posterior, Monte Carlo
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

// ──────────────────────────────────────────────────────────────────────────
// Compute2WayInteractions
// ──────────────────────────────────────────────────────────────────────────

// Compute2WayInteractions returns one Interaction2Way row per ordered
// (factor_a, factor_b, level_a, level_b) tuple for the given
// experiment, and persists results to ExperimentInteractions (one
// row per (experiment_id, factor_a, factor_b, level_a, level_b),
// re-computed on each call — the table is the analysis layer's
// scratchpad, not an audit log).
//
// For each (factor_a, factor_b) pair, only ordered pairs (i < j) are
// emitted: the (b, a) reflection is algebraically identical and would
// double the table for no information gain.
//
// InteractionEstimate is the posterior-mean of the 2-way contrast
// computed against the FIRST declared level of each factor as the
// reference (a' = factors[i].Levels[0], b' = factors[j].Levels[0]).
// ProbNonzero is the Monte Carlo probability that |contrast| exceeds
// the experiment's min_practical_effect (or, when zero, an internal
// floor of 0.10 — see the rationale in Compute2WayInteractionsWithRule).
func Compute2WayInteractions(ctx context.Context, db *sql.DB, experimentID int) ([]Interaction2Way, error) {
	return Compute2WayInteractionsWithRule(ctx, db, experimentID, DecisionRule{})
}

// Compute2WayInteractionsWithRule is the seed-pinnable variant for
// tests. Persists exactly the rows it returns into
// ExperimentInteractions (replacing any prior rows for this
// experiment).
func Compute2WayInteractionsWithRule(ctx context.Context, db *sql.DB, experimentID int, rule DecisionRule) ([]Interaction2Way, error) {
	view, err := loadFactorialView(ctx, db, experimentID)
	if err != nil {
		return nil, err
	}
	rule = rule.withDefaults()

	// minEffect is the practical-significance floor for the
	// "interaction is non-zero" test. We compare the absolute value
	// of the joint-posterior contrast against this floor. Default of
	// 0.10 captures "the effect of one factor changes by 10pp+ when
	// the other factor flips" — a meaningful operator-facing
	// definition of "real interaction" — while suppressing the
	// sample-noise crossings that show up on small-N additive cells
	// (the contrast variance is ~4 × p(1-p)/n; at n=200/cell, the
	// noise floor on |contrast| is ≈ 0.07 std, so a 0.10 threshold
	// has comfortable margin). Operator-level overrides land via
	// Experiments.min_practical_effect; we read it best-effort.
	minEffect := readMinPracticalEffect(ctx, db, experimentID)
	if minEffect <= 0 {
		minEffect = 0.10
	}

	out := []Interaction2Way{}
	for i := 0; i < len(view.Factors); i++ {
		for j := i + 1; j < len(view.Factors); j++ {
			fa := view.Factors[i]
			fb := view.Factors[j]
			refA := fa.Levels[0]
			refB := fb.Levels[0]
			for _, la := range fa.Levels {
				for _, lb := range fb.Levels {
					row := compute2WayCell(view, fa.Name, fb.Name, la, lb, refA, refB, rule, minEffect)
					out = append(out, row)
				}
			}
		}
	}

	if err := persistInteractions(ctx, db, experimentID, out); err != nil {
		return nil, err
	}
	return out, nil
}

func compute2WayCell(view *factorialView, fa, fb, la, lb, refA, refB string, rule DecisionRule, minEffect float64) Interaction2Way {
	// Pool successes/trials for the four (la/refA × lb/refB) corner
	// cells, marginalising over all OTHER factors.
	type pooledCell struct{ s, t int }
	corners := map[string]*pooledCell{
		"la_lb":     {},
		"refA_lb":   {},
		"la_refB":   {},
		"refA_refB": {},
	}
	for _, cell := range view.Cells {
		isLA := cell.Cell[fa] == la
		isRefA := cell.Cell[fa] == refA
		isLB := cell.Cell[fb] == lb
		isRefB := cell.Cell[fb] == refB
		switch {
		case isLA && isLB:
			corners["la_lb"].s += cell.Successes
			corners["la_lb"].t += cell.Trials
		case isRefA && isLB:
			corners["refA_lb"].s += cell.Successes
			corners["refA_lb"].t += cell.Trials
		}
		switch {
		case isLA && isRefB:
			corners["la_refB"].s += cell.Successes
			corners["la_refB"].t += cell.Trials
		case isRefA && isRefB:
			corners["refA_refB"].s += cell.Successes
			corners["refA_refB"].t += cell.Trials
		}
	}
	// Note: when la == refA OR lb == refB the contrast collapses
	// algebraically to zero. We still emit the row (so the table
	// surface is complete for 3+-level factors) but the estimate and
	// ProbNonzero will read as zero / below threshold.

	// Posteriors for the four corners.
	pLaLb := NewPosterior(rule.PriorAlpha, rule.PriorBeta, corners["la_lb"].s, corners["la_lb"].t)
	pRefALb := NewPosterior(rule.PriorAlpha, rule.PriorBeta, corners["refA_lb"].s, corners["refA_lb"].t)
	pLaRefB := NewPosterior(rule.PriorAlpha, rule.PriorBeta, corners["la_refB"].s, corners["la_refB"].t)
	pRefARefB := NewPosterior(rule.PriorAlpha, rule.PriorBeta, corners["refA_refB"].s, corners["refA_refB"].t)

	// Posterior-mean of the interaction contrast.
	estimate := (pLaLb.Mean() - pRefALb.Mean()) - (pLaRefB.Mean() - pRefARefB.Mean())

	// Monte Carlo P(|contrast| > minEffect) — sample each posterior
	// independently, count the fraction of joint draws crossing
	// threshold. Pin a per-(fa, fb, la, lb) seed offset so different
	// rows don't collapse onto identical sample paths.
	seed := rule.RandomSeed + int64(hashStringSeed(fa+"|"+fb+"|"+la+"|"+lb))
	probNonzero := monteCarloProbNonzero(pLaLb, pRefALb, pLaRefB, pRefARefB, minEffect, rule.MonteCarloSamples, seed)

	// Persistence-side posterior shape: pooled successes/failures
	// for the (la, lb) cell (the canonical "anchor" cell). The
	// schema columns posterior_alpha / posterior_beta are
	// documented per-row as the (level_a, level_b) cell posterior;
	// the contrast itself lives in interaction_estimate.
	return Interaction2Way{
		FactorA:             fa,
		FactorB:             fb,
		LevelA:              la,
		LevelB:              lb,
		InteractionEstimate: estimate,
		PosteriorAlpha:      pLaLb.Alpha,
		PosteriorBeta:       pLaLb.Beta,
		ProbNonzero:         probNonzero,
	}
}

// monteCarloProbNonzero estimates P(|contrast| > minEffect) by
// drawing N joint samples from the four corner posteriors,
// computing the contrast on each sample, and counting hits. Uses a
// fixed seed for determinism.
func monteCarloProbNonzero(pLaLb, pRefALb, pLaRefB, pRefARefB *BetaBinomialPosterior, minEffect float64, samples int, seed int64) float64 {
	if samples <= 0 {
		samples = 200000
	}
	rng := rand.New(rand.NewSource(seed))
	hits := 0
	for i := 0; i < samples; i++ {
		x1 := sampleBeta(rng, pLaLb.Alpha, pLaLb.Beta)
		x2 := sampleBeta(rng, pRefALb.Alpha, pRefALb.Beta)
		x3 := sampleBeta(rng, pLaRefB.Alpha, pLaRefB.Beta)
		x4 := sampleBeta(rng, pRefARefB.Alpha, pRefARefB.Beta)
		c := (x1 - x2) - (x3 - x4)
		if math.Abs(c) > minEffect {
			hits++
		}
	}
	return float64(hits) / float64(samples)
}

func readMinPracticalEffect(ctx context.Context, db *sql.DB, experimentID int) float64 {
	var mpe sql.NullFloat64
	err := db.QueryRowContext(ctx,
		`SELECT min_practical_effect FROM Experiments WHERE id = ?`,
		experimentID,
	).Scan(&mpe)
	if err != nil || !mpe.Valid {
		return 0
	}
	return mpe.Float64
}

// persistInteractions replaces any existing ExperimentInteractions
// rows for the experiment with the freshly-computed surface. Wrapped
// in a single transaction so a partial failure leaves the table in
// the prior state (rather than half-overwritten).
func persistInteractions(ctx context.Context, db *sql.DB, experimentID int, rows []Interaction2Way) error {
	if db == nil {
		return errors.New("persistInteractions: db is nil")
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("persistInteractions: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx,
		`DELETE FROM ExperimentInteractions WHERE experiment_id = ?`,
		experimentID,
	); err != nil {
		return fmt.Errorf("persistInteractions: clear prior rows: %w", err)
	}

	stmt, err := tx.PrepareContext(ctx, `
		INSERT INTO ExperimentInteractions
			(experiment_id, factor_a, factor_b, level_a, level_b,
			 interaction_estimate, posterior_alpha, posterior_beta,
			 posterior_prob_nonzero, computed_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, datetime('now'))
	`)
	if err != nil {
		return fmt.Errorf("persistInteractions: prepare: %w", err)
	}
	defer stmt.Close()

	for _, r := range rows {
		if _, err := stmt.ExecContext(ctx,
			experimentID,
			r.FactorA, r.FactorB,
			r.LevelA, r.LevelB,
			r.InteractionEstimate,
			r.PosteriorAlpha, r.PosteriorBeta,
			r.ProbNonzero,
		); err != nil {
			return fmt.Errorf("persistInteractions: insert (%s,%s,%s,%s): %w", r.FactorA, r.FactorB, r.LevelA, r.LevelB, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("persistInteractions: commit: %w", err)
	}
	return nil
}
