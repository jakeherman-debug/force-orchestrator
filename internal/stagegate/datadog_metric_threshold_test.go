package stagegate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"force-orchestrator/internal/clients/datadog"
)

// stubDatadogClient is a per-test stub that satisfies the datadog.Client
// interface. Tests populate the queryFn / healthFn fields so the gate's
// Evaluate sees whatever scenario the test is exercising — Pattern P16
// stubbing at the interface boundary, no real Datadog API calls.
type stubDatadogClient struct {
	queryFn  func(ctx context.Context, query string, window time.Duration) (float64, time.Time, error)
	healthFn func(ctx context.Context) error
}

func (s *stubDatadogClient) QueryMetric(ctx context.Context, query string, window time.Duration) (float64, time.Time, error) {
	if s.queryFn == nil {
		return 0, time.Time{}, fmt.Errorf("stub: queryFn not set")
	}
	return s.queryFn(ctx, query, window)
}

func (s *stubDatadogClient) Health(ctx context.Context) error {
	if s.healthFn == nil {
		return nil
	}
	return s.healthFn(ctx)
}

// Compile-time check: the stub satisfies the interface.
var _ datadog.Client = (*stubDatadogClient)(nil)

// fixedTime gives every test a predictable timestamp so the reason
// strings carry a stable, comparable RFC3339 value.
var fixedTime = time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)

// makeCfg builds a JSON gate-config blob with the four supported keys.
// Tests pass through whatever shape they need; helpers keep the table
// terse.
func makeCfg(t *testing.T, query, comparator string, threshold float64, windowMin int) json.RawMessage {
	t.Helper()
	cfg, err := json.Marshal(map[string]any{
		"metric_query":          query,
		"comparator":            comparator,
		"threshold":             threshold,
		"sample_window_minutes": windowMin,
	})
	if err != nil {
		t.Fatalf("makeCfg: marshal: %v", err)
	}
	return cfg
}

// stubReturning returns a stub whose QueryMetric returns the given value
// and a nil error. fixedTime is used as the "at" timestamp.
func stubReturning(value float64) *stubDatadogClient {
	return &stubDatadogClient{
		queryFn: func(_ context.Context, _ string, _ time.Duration) (float64, time.Time, error) {
			return value, fixedTime, nil
		},
	}
}

// stubError returns a stub whose QueryMetric returns the given error.
func stubError(e error) *stubDatadogClient {
	return &stubDatadogClient{
		queryFn: func(_ context.Context, _ string, _ time.Duration) (float64, time.Time, error) {
			return 0, time.Time{}, e
		},
	}
}

func TestDatadogMetricThreshold_Type(t *testing.T) {
	if (DatadogMetricThreshold{}).Type() != "datadog_metric_threshold" {
		t.Errorf("Type() = %q, want datadog_metric_threshold", DatadogMetricThreshold{}.Type())
	}
}

// LT happy path: error rate stayed below the threshold.
func TestDatadogMetricThreshold_LT_Passes(t *testing.T) {
	g := NewDatadogMetricThreshold(stubReturning(0.0005))
	cfg := makeCfg(t, "avg:errors{*}.as_rate()", "lt", 0.001, 30)
	passed, reason, err := g.Evaluate(context.Background(), nil, StageContext{GateConfig: cfg})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !passed {
		t.Errorf("expected passed=true, reason=%q", reason)
	}
	if !strings.Contains(reason, "passed") {
		t.Errorf("expected reason to mention 'passed', got %q", reason)
	}
}

// LT clean fail: value above the threshold under a "less-than" comparator
// is a concrete fail (not pending).
func TestDatadogMetricThreshold_LT_Fails(t *testing.T) {
	g := NewDatadogMetricThreshold(stubReturning(0.002))
	cfg := makeCfg(t, "avg:errors{*}.as_rate()", "lt", 0.001, 30)
	passed, reason, err := g.Evaluate(context.Background(), nil, StageContext{GateConfig: cfg})
	if err != nil {
		t.Fatalf("expected nil err on threshold miss, got %v", err)
	}
	if passed {
		t.Error("expected passed=false on threshold miss")
	}
	if !strings.Contains(reason, "failed") {
		t.Errorf("expected reason to mention 'failed', got %q", reason)
	}
}

func TestDatadogMetricThreshold_GT_Passes(t *testing.T) {
	g := NewDatadogMetricThreshold(stubReturning(150.0))
	cfg := makeCfg(t, "avg:throughput{*}", "gt", 100.0, 15)
	passed, _, err := g.Evaluate(context.Background(), nil, StageContext{GateConfig: cfg})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !passed {
		t.Error("expected passed=true with value > threshold under gt")
	}
}

// GTE: value == threshold should pass under gte. Boundary case.
func TestDatadogMetricThreshold_GTE_Passes_Equal(t *testing.T) {
	g := NewDatadogMetricThreshold(stubReturning(100.0))
	cfg := makeCfg(t, "avg:throughput{*}", "gte", 100.0, 15)
	passed, _, err := g.Evaluate(context.Background(), nil, StageContext{GateConfig: cfg})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !passed {
		t.Error("expected passed=true with value == threshold under gte")
	}
}

// LTE: value == threshold should pass under lte.
func TestDatadogMetricThreshold_LTE_Passes_Equal(t *testing.T) {
	g := NewDatadogMetricThreshold(stubReturning(0.001))
	cfg := makeCfg(t, "avg:errors{*}", "lte", 0.001, 30)
	passed, _, err := g.Evaluate(context.Background(), nil, StageContext{GateConfig: cfg})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !passed {
		t.Error("expected passed=true with value == threshold under lte")
	}
}

// EQ within 1e-9 tolerance — values that differ by less than the float
// epsilon are treated as equal.
func TestDatadogMetricThreshold_EQ_WithinTolerance_Passes(t *testing.T) {
	// 1.0 + 1e-12 should still compare equal to 1.0 under eq.
	g := NewDatadogMetricThreshold(stubReturning(1.0 + 1e-12))
	cfg := makeCfg(t, "avg:flat{*}", "eq", 1.0, 5)
	passed, _, err := g.Evaluate(context.Background(), nil, StageContext{GateConfig: cfg})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !passed {
		t.Error("expected passed=true within EQ tolerance")
	}
}

// EQ outside tolerance — clean fail.
func TestDatadogMetricThreshold_EQ_OutsideTolerance_Fails(t *testing.T) {
	g := NewDatadogMetricThreshold(stubReturning(1.5))
	cfg := makeCfg(t, "avg:flat{*}", "eq", 1.0, 5)
	passed, reason, err := g.Evaluate(context.Background(), nil, StageContext{GateConfig: cfg})
	if err != nil {
		t.Fatalf("expected nil err on eq miss, got %v", err)
	}
	if passed {
		t.Error("expected passed=false outside EQ tolerance")
	}
	if !strings.Contains(reason, "failed") {
		t.Errorf("expected reason to mention 'failed', got %q", reason)
	}
}

// ErrNoData → ErrPending. The dog re-checks next tick.
func TestDatadogMetricThreshold_NoData_Pending(t *testing.T) {
	g := NewDatadogMetricThreshold(stubError(datadog.ErrNoData))
	cfg := makeCfg(t, "avg:silent{*}", "lt", 0.001, 30)
	passed, reason, err := g.Evaluate(context.Background(), nil, StageContext{GateConfig: cfg})
	if !errors.Is(err, ErrPending) {
		t.Fatalf("expected ErrPending on ErrNoData, got %v", err)
	}
	if passed {
		t.Error("expected passed=false on ErrNoData")
	}
	if !strings.Contains(reason, "no data") {
		t.Errorf("expected reason to mention 'no data', got %q", reason)
	}
}

// ErrTransient → ErrPending. 5xx / network errors don't fail the convoy.
func TestDatadogMetricThreshold_Transient_Pending(t *testing.T) {
	g := NewDatadogMetricThreshold(stubError(fmt.Errorf("wrap: %w", datadog.ErrTransient)))
	cfg := makeCfg(t, "avg:errors{*}", "lt", 0.001, 30)
	passed, reason, err := g.Evaluate(context.Background(), nil, StageContext{GateConfig: cfg})
	if !errors.Is(err, ErrPending) {
		t.Fatalf("expected ErrPending on ErrTransient, got %v", err)
	}
	if passed {
		t.Error("expected passed=false on ErrTransient")
	}
	if !strings.Contains(reason, "transient") {
		t.Errorf("expected reason to mention 'transient', got %q", reason)
	}
}

// ErrAuthFailure → passed=false with err=nil. Operator-actionable; the
// reason carries the signal so the dashboard and notify path surface it.
func TestDatadogMetricThreshold_AuthFailure_Fails(t *testing.T) {
	g := NewDatadogMetricThreshold(stubError(fmt.Errorf("wrap: %w", datadog.ErrAuthFailure)))
	cfg := makeCfg(t, "avg:errors{*}", "lt", 0.001, 30)
	passed, reason, err := g.Evaluate(context.Background(), nil, StageContext{GateConfig: cfg})
	if err != nil {
		t.Fatalf("expected nil err on auth failure (operator-actionable), got %v", err)
	}
	if passed {
		t.Error("expected passed=false on auth failure")
	}
	if !strings.Contains(reason, "auth failure") {
		t.Errorf("expected reason to mention 'auth failure', got %q", reason)
	}
}

// Any non-sentinel error from the client surfaces as a structural error.
func TestDatadogMetricThreshold_OtherError_PropagatesError(t *testing.T) {
	g := NewDatadogMetricThreshold(stubError(errors.New("unexpected boom")))
	cfg := makeCfg(t, "avg:errors{*}", "lt", 0.001, 30)
	_, _, err := g.Evaluate(context.Background(), nil, StageContext{GateConfig: cfg})
	if err == nil {
		t.Fatal("expected error on unexpected client error")
	}
	if errors.Is(err, ErrPending) {
		t.Errorf("unexpected client error should not be ErrPending, got %v", err)
	}
	if !strings.Contains(err.Error(), "query") {
		t.Errorf("expected error to mention 'query', got %v", err)
	}
}

// Empty metric_query → structural error caught before any client call.
func TestDatadogMetricThreshold_MissingQuery_Errors(t *testing.T) {
	g := NewDatadogMetricThreshold(stubReturning(0))
	cfg, _ := json.Marshal(map[string]any{
		"comparator":            "lt",
		"threshold":             0.001,
		"sample_window_minutes": 30,
	})
	_, _, err := g.Evaluate(context.Background(), nil, StageContext{GateConfig: cfg})
	if err == nil {
		t.Fatal("expected error on missing metric_query")
	}
	if !strings.Contains(err.Error(), "metric_query required") {
		t.Errorf("expected error to mention 'metric_query required', got %v", err)
	}
}

// Empty comparator → structural error.
func TestDatadogMetricThreshold_MissingComparator_Errors(t *testing.T) {
	g := NewDatadogMetricThreshold(stubReturning(0))
	cfg, _ := json.Marshal(map[string]any{
		"metric_query":          "avg:errors{*}",
		"threshold":             0.001,
		"sample_window_minutes": 30,
	})
	_, _, err := g.Evaluate(context.Background(), nil, StageContext{GateConfig: cfg})
	if err == nil {
		t.Fatal("expected error on missing comparator")
	}
	if !strings.Contains(err.Error(), "comparator required") {
		t.Errorf("expected error to mention 'comparator required', got %v", err)
	}
}

// Garbage comparator → structural error with a clear "must be one of"
// message so the operator can fix their gate config.
func TestDatadogMetricThreshold_InvalidComparator_Errors(t *testing.T) {
	g := NewDatadogMetricThreshold(stubReturning(0))
	cfg := makeCfg(t, "avg:errors{*}", "between", 0.001, 30)
	_, _, err := g.Evaluate(context.Background(), nil, StageContext{GateConfig: cfg})
	if err == nil {
		t.Fatal("expected error on invalid comparator")
	}
	if !strings.Contains(err.Error(), "invalid comparator") {
		t.Errorf("expected error to mention 'invalid comparator', got %v", err)
	}
}

// Negative or zero window → structural error. A non-positive window is a
// planner bug; the planner should have caught it but the runtime is the
// defensive backstop.
func TestDatadogMetricThreshold_NegativeWindow_Errors(t *testing.T) {
	g := NewDatadogMetricThreshold(stubReturning(0))
	cfg := makeCfg(t, "avg:errors{*}", "lt", 0.001, -5)
	_, _, err := g.Evaluate(context.Background(), nil, StageContext{GateConfig: cfg})
	if err == nil {
		t.Fatal("expected error on negative window")
	}
	if !strings.Contains(err.Error(), "sample_window_minutes must be positive") {
		t.Errorf("expected error to mention 'sample_window_minutes must be positive', got %v", err)
	}
}

func TestDatadogMetricThreshold_ZeroWindow_Errors(t *testing.T) {
	g := NewDatadogMetricThreshold(stubReturning(0))
	cfg := makeCfg(t, "avg:errors{*}", "lt", 0.001, 0)
	_, _, err := g.Evaluate(context.Background(), nil, StageContext{GateConfig: cfg})
	if err == nil {
		t.Fatal("expected error on zero window")
	}
	if !strings.Contains(err.Error(), "sample_window_minutes must be positive") {
		t.Errorf("expected error to mention 'sample_window_minutes must be positive', got %v", err)
	}
}

// Nil client at Evaluate → structural error. The constructor doesn't
// panic on nil so the daemon can skip registration cleanly when Datadog
// isn't configured; reaching Evaluate with a nil client is a wiring bug.
func TestDatadogMetricThreshold_NilClient_Errors(t *testing.T) {
	g := NewDatadogMetricThreshold(nil)
	cfg := makeCfg(t, "avg:errors{*}", "lt", 0.001, 30)
	_, _, err := g.Evaluate(context.Background(), nil, StageContext{GateConfig: cfg})
	if err == nil {
		t.Fatal("expected error on nil client")
	}
	if !strings.Contains(err.Error(), "nil client") {
		t.Errorf("expected error to mention 'nil client', got %v", err)
	}
}

// Bad JSON in the gate config → parse error.
func TestDatadogMetricThreshold_BadJSON_Errors(t *testing.T) {
	g := NewDatadogMetricThreshold(stubReturning(0))
	_, _, err := g.Evaluate(context.Background(), nil, StageContext{GateConfig: json.RawMessage("not json")})
	if err == nil {
		t.Fatal("expected error on bad JSON")
	}
	if !strings.Contains(err.Error(), "parse config") {
		t.Errorf("expected error to mention 'parse config', got %v", err)
	}
}

// Sample window passes through to the client unchanged (covers the int
// → time.Duration conversion).
func TestDatadogMetricThreshold_WindowPassthrough(t *testing.T) {
	var seenWindow time.Duration
	stub := &stubDatadogClient{
		queryFn: func(_ context.Context, _ string, w time.Duration) (float64, time.Time, error) {
			seenWindow = w
			return 0.0005, fixedTime, nil
		},
	}
	g := NewDatadogMetricThreshold(stub)
	cfg := makeCfg(t, "avg:errors{*}", "lt", 0.001, 45)
	if _, _, err := g.Evaluate(context.Background(), nil, StageContext{GateConfig: cfg}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if seenWindow != 45*time.Minute {
		t.Errorf("expected window=45m, got %v", seenWindow)
	}
}

// RegisterDatadogGate wires the gate into a fresh registry when client
// is non-nil, and skips registration cleanly when it's nil.
func TestRegisterDatadogGate(t *testing.T) {
	r := NewRegistry()
	RegisterDatadogGate(r, stubReturning(0))
	if _, ok := r.Lookup("datadog_metric_threshold"); !ok {
		t.Error("expected datadog_metric_threshold to be registered")
	}

	r2 := NewRegistry()
	RegisterDatadogGate(r2, nil)
	if _, ok := r2.Lookup("datadog_metric_threshold"); ok {
		t.Error("expected nil-client path to skip registration")
	}
}
