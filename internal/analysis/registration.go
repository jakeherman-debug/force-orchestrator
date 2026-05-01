package analysis

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
)

// BayesianBetaBinomialVersion is the canonical version string under
// which the framework is registered. Bumped only when the algorithm
// changes (prior shape, decision rule, MC implementation).
const BayesianBetaBinomialVersion = "2026-04-29"

// BayesianBetaBinomialName is the framework's name as recorded in
// AnalysisFrameworks; ExperimentMetrics rows reference it via the
// `analysis_framework_version` column on Experiments.
const BayesianBetaBinomialName = "bayesian-beta-binomial"

// BayesianBetaBinomialFactorialVersion is the canonical version
// string under which the factorial extension is registered. Bumped
// only when the factorial algorithm changes (decomposition shape,
// interaction prior, decision rule). Decoupled from the
// single-treatment version so a single-treatment analyzer change
// doesn't force a factorial-row rewrite (and vice versa).
const BayesianBetaBinomialFactorialVersion = "2026-04-30"

// BayesianBetaBinomialFactorialName is the factorial framework's
// name as recorded in AnalysisFrameworks. Mirrors the single-treatment
// shape (sibling row, separate version) rather than mutating the
// existing row — the AnalysisFrameworks contract is that published
// versions are immutable.
const BayesianBetaBinomialFactorialName = "bayesian-beta-binomial-factorial"

// bayesianBetaBinomialParams is the parameter manifest stored on the
// AnalysisFrameworks row. It documents the prior, decision rule, and
// minimum sample size so a future replay can reconstruct the exact
// math even if the source file moved.
var bayesianBetaBinomialParams = map[string]any{
	"name":                  BayesianBetaBinomialName,
	"version":               BayesianBetaBinomialVersion,
	"family":                "bayesian-beta-binomial",
	"prior_alpha":           1.0,
	"prior_beta":            1.0,
	"min_samples_per_arm":   30,
	"winner_threshold":      0.95,
	"monte_carlo_samples":   200000,
	"sql_or_code_ref":       "internal/analysis/bayesian_beta_binomial.go",
	"description":           "Beta-Binomial conjugate posteriors per arm; P(treatment > control) by Monte Carlo over the joint; equal-tail Beta quantile credible intervals via Lentz/bisection on I_x(a,b).",
	"published_by":          "operator:jake.herman@upstart.com",
	"reproducibility_note":  "Algorithm is deterministic given the seed embedded in DecisionRule.RandomSeed; tests pin a fixed seed and production uses the same constant so two reads of the same observed data return the same decision.",
}

// RegisterBayesianBetaBinomial inserts the framework row into
// AnalysisFrameworks if it does not already exist. The schema enforces
// `version TEXT PRIMARY KEY`, so a re-call with the same version is
// idempotent — the second call sees the row, computes the same hash,
// and returns nil without rewriting it.
//
// A re-call with the same version BUT a different parameter manifest
// is an error: the framework's contract is that published versions
// are immutable. The caller must bump BayesianBetaBinomialVersion if
// the algorithm changes.
func RegisterBayesianBetaBinomial(ctx context.Context, db *sql.DB) error {
	if db == nil {
		return fmt.Errorf("RegisterBayesianBetaBinomial: db is nil")
	}
	body, err := json.Marshal(bayesianBetaBinomialParams)
	if err != nil {
		return fmt.Errorf("RegisterBayesianBetaBinomial: marshal params: %w", err)
	}
	hash := sha256Hex(string(body))

	var existingHash string
	err = db.QueryRowContext(ctx,
		`SELECT IFNULL(config_hash, '') FROM AnalysisFrameworks WHERE version = ?`,
		BayesianBetaBinomialVersion,
	).Scan(&existingHash)
	switch {
	case err == sql.ErrNoRows:
		// Not yet present — insert.
	case err != nil:
		return fmt.Errorf("RegisterBayesianBetaBinomial: lookup existing: %w", err)
	default:
		if existingHash == hash {
			return nil
		}
		return fmt.Errorf("RegisterBayesianBetaBinomial: version %q already registered with a different config_hash (existing=%s, new=%s); bump the version constant before changing the manifest",
			BayesianBetaBinomialVersion, existingHash, hash)
	}

	_, err = db.ExecContext(ctx, `
		INSERT INTO AnalysisFrameworks
			(version, config_content, config_hash, algorithm_git_sha,
			 published_at, published_by, description)
		VALUES (?, ?, ?, ?, datetime('now'), ?, ?)
	`,
		BayesianBetaBinomialVersion,
		string(body),
		hash,
		"", // populated on demand by a future operator command
		bayesianBetaBinomialParams["published_by"],
		bayesianBetaBinomialParams["description"],
	)
	if err != nil {
		return fmt.Errorf("RegisterBayesianBetaBinomial: insert: %w", err)
	}
	return nil
}

// bayesianBetaBinomialFactorialParams is the parameter manifest for
// the factorial framework row. The `decomposition` key matches
// paired-runs.md § Factorial Scoring (`main_effects_plus_2way`); the
// `max_interaction_order` mirrors the YAML default. `algorithm_ref`
// names the Go entry points so a future replay can reproduce the
// math from the source tree alone.
var bayesianBetaBinomialFactorialParams = map[string]any{
	"name":                  BayesianBetaBinomialFactorialName,
	"version":               BayesianBetaBinomialFactorialVersion,
	"family":                "bayesian-beta-binomial",
	"prior_alpha":           1.0,
	"prior_beta":            1.0,
	"min_samples_per_arm":   30,
	"winner_threshold":      0.95,
	"monte_carlo_samples":   200000,
	"decomposition":         "main_effects_plus_2way",
	"max_interaction_order": 2,
	"sql_or_code_ref":       "internal/analysis/factorial_analysis.go",
	"description":           "Factorial Beta-Binomial: per-(factor,level) marginal posteriors as main effects; per-(factor_a,factor_b,level_a,level_b) cell-level 2-way interaction contrasts with Monte Carlo P(|interaction| > min_practical_effect); decision rule declares best cell when no interactions cross WinnerThreshold, else flags 'significant_interaction'.",
	"published_by":          "operator:jake.herman@upstart.com",
	"reproducibility_note":  "Algorithm is deterministic given DecisionRule.RandomSeed; per-(factor,level) and per-interaction MC sample paths are seeded by hashing the identifier into an offset, so different rows don't collapse onto identical sample paths.",
}

// RegisterBayesianBetaBinomialFactorial inserts the factorial
// framework row into AnalysisFrameworks. Mirrors
// RegisterBayesianBetaBinomial: idempotent on identical re-call,
// errors if the same version is re-registered with a different
// manifest hash. The factorial row is a SIBLING of the single-
// treatment row (separate version PK) — they cohabit
// AnalysisFrameworks because both are valid analyzer choices for
// different experiment kinds (single vs factorial).
func RegisterBayesianBetaBinomialFactorial(ctx context.Context, db *sql.DB) error {
	if db == nil {
		return fmt.Errorf("RegisterBayesianBetaBinomialFactorial: db is nil")
	}
	body, err := json.Marshal(bayesianBetaBinomialFactorialParams)
	if err != nil {
		return fmt.Errorf("RegisterBayesianBetaBinomialFactorial: marshal params: %w", err)
	}
	hash := sha256Hex(string(body))

	var existingHash string
	err = db.QueryRowContext(ctx,
		`SELECT IFNULL(config_hash, '') FROM AnalysisFrameworks WHERE version = ?`,
		BayesianBetaBinomialFactorialVersion,
	).Scan(&existingHash)
	switch {
	case err == sql.ErrNoRows:
		// Not yet present — insert.
	case err != nil:
		return fmt.Errorf("RegisterBayesianBetaBinomialFactorial: lookup existing: %w", err)
	default:
		if existingHash == hash {
			return nil
		}
		return fmt.Errorf("RegisterBayesianBetaBinomialFactorial: version %q already registered with a different config_hash (existing=%s, new=%s); bump the version constant before changing the manifest",
			BayesianBetaBinomialFactorialVersion, existingHash, hash)
	}

	_, err = db.ExecContext(ctx, `
		INSERT INTO AnalysisFrameworks
			(version, config_content, config_hash, algorithm_git_sha,
			 published_at, published_by, description)
		VALUES (?, ?, ?, ?, datetime('now'), ?, ?)
	`,
		BayesianBetaBinomialFactorialVersion,
		string(body),
		hash,
		"", // populated on demand by a future operator command
		bayesianBetaBinomialFactorialParams["published_by"],
		bayesianBetaBinomialFactorialParams["description"],
	)
	if err != nil {
		return fmt.Errorf("RegisterBayesianBetaBinomialFactorial: insert: %w", err)
	}
	return nil
}

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}
