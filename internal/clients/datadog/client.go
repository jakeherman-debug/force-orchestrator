// Package datadog defines the client interface for Datadog metric
// queries used by the D5.5 P3 `datadog_metric_threshold` staged-convoy
// gate.
//
// The interface is small and matches the operations the gate needs:
// a single point-in-time scalar lookup (QueryMetric) plus a cheap
// reachability/auth probe (Health) used by the convoy-stage-watch dog
// before walking a long batch of stages.
//
// Roadmap reference (docs/roadmap.md § "Deliverable 5.5" → "Advanced
// leaf gates (P3)"):
//
//	datadog_metric_threshold | {metric_query: string, comparator:
//	  lt|gt|eq|lte|gte, threshold: float, sample_window_minutes: int}
//	  | Queries Datadog API for time-series metrics. Use cases:
//	  "error rate stayed below 0.1% for 30 min", "p95 latency didn't
//	  increase by >10% over baseline", "request throughput stayed flat
//	  after stage 1 merged."
//
// Pattern P16
// (internal/audittools/audit_pattern_p16_clients_interfaces_test.go)
// enforces that production agent code references the Client interface
// only — never a concrete struct type. Construction is via the
// exported NewInProcess factory; future siblings (gRPC, mock) follow
// the same shape.
//
// Implementations live as siblings:
//   - inprocess.go — backed by the Datadog v1 timeseries-query HTTPS
//     API (https://docs.datadoghq.com/api/latest/metrics/#query-timeseries-points).
//     SystemConfig keys (datadog_api_key / datadog_app_key /
//     datadog_base_url) are read at construction; rotation happens via
//     daemon restart.
//
// Test discipline: tests stub the HTTP boundary
// (httptest.Server / RoundTripper); no real Datadog API calls.
package datadog

import (
	"context"
	"errors"
	"time"
)

// Client is the Datadog metrics-query interface used by the
// `datadog_metric_threshold` staged-convoy gate.
//
// Implementations route through Datadog's v1 timeseries-query API
// (https://docs.datadoghq.com/api/latest/metrics/#query-timeseries-points).
//
// In-process implementation lives at inprocess.go; tests stub at the
// Client interface boundary (Pattern P16) so no real network calls
// happen during go test.
type Client interface {
	// QueryMetric returns the latest scalar value for a Datadog metric
	// query over the given window. The returned value is the LAST point
	// in the series (most recent), not an aggregate — the caller is
	// expected to express any aggregation directly in the query string
	// (e.g., "avg:foo.bar{...}.rollup(avg, 1800)").
	//
	// window is the lookback window; the API call uses [now-window, now].
	//
	// Returns ErrNoData if Datadog returns an empty series (operator
	// typo, metric not yet emitting, etc.) — caller treats as "gate
	// can't evaluate", which routes to ErrPending in the gate body.
	//
	// Returns ErrTransient on 5xx / network errors — gate retries on
	// next dog tick.
	//
	// Returns ErrAuthFailure on 401/403 — caller surfaces an operator
	// alert (the API key is misconfigured or expired).
	QueryMetric(ctx context.Context, query string, window time.Duration) (value float64, at time.Time, err error)

	// Health probes the Datadog API to confirm credentials +
	// reachability. Used by the convoy-stage-watch dog as a precheck
	// before calling QueryMetric on a long batch of stages. Backed by
	// the GET /api/v1/validate endpoint (only the API key is required).
	//
	// Returns nil on a 200 response.
	// Returns ErrAuthFailure on 401/403.
	// Returns ErrTransient on 5xx / network errors.
	Health(ctx context.Context) error
}

// Sentinel errors. Callers compare with errors.Is.
var (
	// ErrNoData wraps an empty-series response from the Datadog query
	// API. The `datadog_metric_threshold` gate treats this as
	// "indeterminate" and defers (ErrPending) — the metric may not
	// be emitting yet, or the operator's query has a typo.
	ErrNoData = errors.New("datadog: no data for query")

	// ErrTransient wraps 5xx / network errors. The gate retries on the
	// next dog tick; not an auth-class condition.
	ErrTransient = errors.New("datadog: transient (5xx or network)")

	// ErrAuthFailure wraps 401/403 responses. The caller surfaces an
	// operator alert because the configured API or APP key is missing,
	// expired, or insufficiently scoped.
	ErrAuthFailure = errors.New("datadog: auth failure (401/403)")

	// ErrConfig is returned by NewInProcess when SystemConfig is
	// missing required keys (datadog_api_key, datadog_app_key) or when
	// they are present but malformed.
	ErrConfig = errors.New("datadog: config invalid (missing API key, etc.)")
)

// SystemConfig keys consumed by NewInProcess. The api_key + app_key
// pair is required for the v1 query API; base_url defaults to
// Datadog's US1 region but can be overridden for EU / Gov / staging
// (api.datadoghq.eu, api.ddog-gov.com, etc.).
const (
	ConfigKeyAPIKey  = "datadog_api_key"
	ConfigKeyAPPKey  = "datadog_app_key"
	ConfigKeyBaseURL = "datadog_base_url"
)

// DefaultBaseURL is Datadog's US1-region API host. Mirrors the value
// used by the official `datadog-api-client-go` SDK when no override is
// supplied.
const DefaultBaseURL = "https://api.datadoghq.com"
