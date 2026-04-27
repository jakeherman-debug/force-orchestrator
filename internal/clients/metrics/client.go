// Package metrics defines the client interface for the metrics service
// — the pluggable scoring layer that turns a recorded run into a
// numerical outcome. Engineering Corps uses this to score paired runs
// (treatment vs control); experiments.Outcome reads from it; the
// fleet dashboard renders score-over-time charts on top of it.
//
// Implementation timeline:
//   - D0 (this commit): interface definition + ErrNotImplemented stubs.
//   - D3 (paired-runs + Engineering Corps deliverable): the real
//     in-process implementation lands here, sourced from the Metrics /
//     MetricVersions tables the deliverable introduces.
//   - Later: gRPC backing for shared multi-tenant operation.
//
// Pattern P16 (audit_pattern_p16_clients_interfaces_test.go) enforces
// that production agent code references the Client interface only.
package metrics

import (
	"context"
	"errors"
)

// Client is the contract between Engineering Corps / experiments code
// and the metrics service. The shape is built around "register a
// versioned metric definition, then score a run against it later."
type Client interface {
	// RegisterMetric records a new metric definition. Versioning is
	// load-bearing — the metric body (the function that turns a run
	// into a number) can change over time, but historical scores
	// against an older version stay comparable.
	RegisterMetric(ctx context.Context, metric MetricVersion) error

	// Score returns the recorded score for the given run / metric
	// pair. Returns ErrNoScore when no row was written (the metric
	// hasn't run against this run yet).
	Score(ctx context.Context, runID int, metricName, version string) (float64, error)

	// RecordScore writes a score for the given run / metric pair.
	// D3's metric runners call this after they compute their value.
	RecordScore(ctx context.Context, runID int, metricName, version string, score float64) error

	// ListMetrics returns every registered metric (across versions)
	// for the operator's dashboard.
	ListMetrics(ctx context.Context) ([]MetricVersion, error)
}

// MetricVersion describes one (name, version) pair: the body the D3
// runner evaluates and the units the resulting score is in.
type MetricVersion struct {
	Name        string
	Version     string  // semver-ish; D3 owns the format
	Description string
	Units       string  // free-form, for the dashboard label
	Body        string  // the metric definition (e.g. SQL fragment, Go expr key)
	OwningTeam  string
}

var (
	// ErrNoScore — Score called against a (run, metric, version)
	// triple that has no recorded value.
	ErrNoScore = errors.New("metrics: no score recorded for this run")

	// ErrMetricExists — RegisterMetric called for a (Name, Version)
	// pair that already exists. Versions are immutable; bump Version
	// to publish a change.
	ErrMetricExists = errors.New("metrics: metric version already registered")

	// ErrNotImplemented — D0 stub guard.
	ErrNotImplemented = errors.New("metrics: not implemented (D3 deliverable)")
)
