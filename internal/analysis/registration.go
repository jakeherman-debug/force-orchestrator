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

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}
