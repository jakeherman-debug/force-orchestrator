package stagegate

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"force-orchestrator/internal/clients/datadog"
)

// DatadogMetricThreshold is the D5.5 P3 W2 δ advanced leaf gate that
// confirms a deployed change is "actually working" by polling a Datadog
// metric and asserting its latest value compares correctly against an
// operator-supplied threshold. It's the natural successor to probe_endpoint
// for stages that need stronger evidence than "the /health URL says ok":
// error rate stayed below 0.1% over a 30-min sample window, p95 latency
// stayed flat, request throughput didn't collapse after a stage merge.
//
// Config shape (the JSON object stored in ConvoyStages.gate_config_json):
//
//	{
//	  "metric_query":           "avg:trace.http.request.errors{env:prod}.as_rate()",
//	  "comparator":             "lt",     // lt | gt | eq | lte | gte
//	  "threshold":              0.001,
//	  "sample_window_minutes":  30
//	}
//
// Evaluation contract:
//   - ErrNoData from the client (empty series in window) → ErrPending. The
//     metric may not be emitting yet, or the operator's query has a typo;
//     either way the gate can't decide, so the dog re-checks next tick.
//   - ErrTransient (5xx / network) → ErrPending. Same reasoning as
//     probe_endpoint's network errors: a flapping API shouldn't fail the
//     convoy.
//   - ErrAuthFailure → passed=false with err=nil. Auth misconfiguration is
//     operator-actionable and won't self-heal on retry; keep failing the
//     gate (and surfacing the reason) until the operator rotates keys.
//   - Any other client error → structural error.
//   - Comparator evaluates negatively → passed=false with err=nil. The
//     metric came back "out of bounds"; the stage moves to Failed.
//   - Comparator evaluates positively → passed=true.
//
// Test discipline: stubbed at the datadog.Client interface boundary
// (Pattern P16). No real Datadog API calls happen during go test.
type DatadogMetricThreshold struct {
	client datadog.Client
}

// NewDatadogMetricThreshold constructs the gate with an injected
// datadog.Client. Production wires the in-process implementation
// (datadog.NewInProcess); tests pass a stub satisfying the same interface.
// Nil clients are caught at Evaluate-time (a structural error) rather than
// panicking at construction so the daemon's "skip registration when client
// is nil" wiring path stays observable.
func NewDatadogMetricThreshold(client datadog.Client) DatadogMetricThreshold {
	return DatadogMetricThreshold{client: client}
}

// Type implements Gate.
func (DatadogMetricThreshold) Type() string { return "datadog_metric_threshold" }

// Evaluate implements Gate.
func (d DatadogMetricThreshold) Evaluate(ctx context.Context, _ *sql.DB, stage StageContext) (bool, string, error) {
	var cfg struct {
		MetricQuery         string  `json:"metric_query"`
		Comparator          string  `json:"comparator"`
		Threshold           float64 `json:"threshold"`
		SampleWindowMinutes int     `json:"sample_window_minutes"`
	}
	if err := json.Unmarshal(stage.GateConfig, &cfg); err != nil {
		return false, "", fmt.Errorf("datadog_metric_threshold: parse config: %w", err)
	}
	if cfg.MetricQuery == "" {
		return false, "", fmt.Errorf("datadog_metric_threshold: metric_query required")
	}
	if cfg.Comparator == "" {
		return false, "", fmt.Errorf("datadog_metric_threshold: comparator required")
	}
	if !validComparator(cfg.Comparator) {
		return false, "", fmt.Errorf("datadog_metric_threshold: invalid comparator %q; must be one of lt, gt, eq, lte, gte", cfg.Comparator)
	}
	if cfg.SampleWindowMinutes <= 0 {
		return false, "", fmt.Errorf("datadog_metric_threshold: sample_window_minutes must be positive, got %d", cfg.SampleWindowMinutes)
	}
	if d.client == nil {
		// Defensive: NewDatadogMetricThreshold doesn't panic on nil so
		// the daemon can skip registration cleanly when Datadog isn't
		// configured, but reaching Evaluate with a nil client means the
		// gate WAS registered with no client — that's a wiring bug.
		return false, "", fmt.Errorf("datadog_metric_threshold: nil client (gate registered without datadog.Client)")
	}

	window := time.Duration(cfg.SampleWindowMinutes) * time.Minute
	value, at, err := d.client.QueryMetric(ctx, cfg.MetricQuery, window)
	switch {
	case errors.Is(err, datadog.ErrNoData):
		// No data in the window — gate can't evaluate; treat as pending so
		// the dog re-checks next tick. The metric may not yet be emitting,
		// or the operator's query has a typo; either way the gate isn't
		// going to silently decide for us.
		return false, fmt.Sprintf("datadog: no data for %q over last %v", cfg.MetricQuery, window), ErrPending
	case errors.Is(err, datadog.ErrTransient):
		return false, fmt.Sprintf("datadog: transient error: %v", err), ErrPending
	case errors.Is(err, datadog.ErrAuthFailure):
		// Auth failure is operator-actionable; don't retry, fail the gate.
		// The reason field carries the operator-readable signal.
		return false, fmt.Sprintf("datadog: auth failure: %v", err), nil
	case err != nil:
		return false, "", fmt.Errorf("datadog_metric_threshold: query: %w", err)
	}

	if !compareValue(cfg.Comparator, value, cfg.Threshold) {
		return false, fmt.Sprintf("datadog: %q = %.4f at %s; %s %.4f failed", cfg.MetricQuery, value, at.Format(time.RFC3339), cfg.Comparator, cfg.Threshold), nil
	}
	return true, fmt.Sprintf("datadog: %q = %.4f at %s; %s %.4f passed", cfg.MetricQuery, value, at.Format(time.RFC3339), cfg.Comparator, cfg.Threshold), nil
}

// validComparator + compareValue are defined in comparator.go (shared
// across datadog_metric_threshold + databricks_query_threshold; canonical
// home for threshold-gate comparator helpers, including the 1e-9 eq
// tolerance documented there).

// RegisterDatadogGate registers the datadog_metric_threshold gate against
// the provided registry. Slice δ owns this helper; slice ε will add a
// parallel RegisterDatabricksGate, and Wave 3 ζ composes both at the
// daemon registration site (so this helper signature is safe to evolve).
//
// If client is nil (e.g., environments without Datadog config) we skip
// registration silently — the daemon's registration site is responsible
// for logging that observation; this helper just no-ops to keep the
// downstream registry clean of a half-wired gate.
//
// Panics on duplicate registration (Registry.Register's contract).
func RegisterDatadogGate(r *Registry, client datadog.Client) {
	if client == nil {
		return
	}
	r.Register(NewDatadogMetricThreshold(client))
}
