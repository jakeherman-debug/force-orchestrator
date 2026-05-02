package stagegate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"force-orchestrator/internal/clients/databricks"
)

// stubDatabricksClient is the test fake for databricks.Client. It records
// the warehouse id, SQL, and timeout it was called with so tests can
// assert pass-through, and returns a configurable (value, err) pair so
// each error class (transient, timeout, auth, shape, etc.) can be
// exercised independently.
type stubDatabricksClient struct {
	value         float64
	err           error
	healthErr     error
	gotWarehouse  string
	gotSQL        string
	gotTimeout    time.Duration
	executeCalls  int
}

func (s *stubDatabricksClient) ExecuteQuery(_ context.Context, warehouseID, sqlQuery string, timeout time.Duration) (float64, error) {
	s.executeCalls++
	s.gotWarehouse = warehouseID
	s.gotSQL = sqlQuery
	s.gotTimeout = timeout
	return s.value, s.err
}

func (s *stubDatabricksClient) Health(_ context.Context) error { return s.healthErr }

// helper: build a JSON gate-config blob with the supplied fields.
func dqtCfg(t *testing.T, m map[string]any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal cfg: %v", err)
	}
	return b
}

// TestDatabricksQueryThreshold_Type — Type() returns the registered name.
func TestDatabricksQueryThreshold_Type(t *testing.T) {
	if (DatabricksQueryThreshold{}).Type() != "databricks_query_threshold" {
		t.Errorf("Type() = %q, want databricks_query_threshold", DatabricksQueryThreshold{}.Type())
	}
}

// TestDatabricksQueryThreshold_LT_Passes — value < threshold → passed.
func TestDatabricksQueryThreshold_LT_Passes(t *testing.T) {
	stub := &stubDatabricksClient{value: 0.005}
	g := NewDatabricksQueryThreshold(stub)
	cfg := dqtCfg(t, map[string]any{
		"sql_query":    "SELECT error_rate FROM metrics",
		"comparator":   "lt",
		"threshold":    0.01,
		"warehouse_id": "wh-abc",
	})
	passed, reason, err := g.Evaluate(context.Background(), nil, StageContext{GateConfig: cfg})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !passed {
		t.Errorf("expected passed=true, reason=%q", reason)
	}
	if !strings.Contains(reason, "passed") {
		t.Errorf("expected reason to mention passed, got %q", reason)
	}
}

// TestDatabricksQueryThreshold_LT_Fails — value not < threshold → fail-clean.
func TestDatabricksQueryThreshold_LT_Fails(t *testing.T) {
	stub := &stubDatabricksClient{value: 0.05}
	g := NewDatabricksQueryThreshold(stub)
	cfg := dqtCfg(t, map[string]any{
		"sql_query":    "SELECT error_rate FROM metrics",
		"comparator":   "lt",
		"threshold":    0.01,
		"warehouse_id": "wh-abc",
	})
	passed, reason, err := g.Evaluate(context.Background(), nil, StageContext{GateConfig: cfg})
	if err != nil {
		t.Fatalf("expected nil err on threshold miss, got %v", err)
	}
	if passed {
		t.Error("expected passed=false on threshold miss")
	}
	if !strings.Contains(reason, "failed") {
		t.Errorf("expected reason to mention failed, got %q", reason)
	}
}

// TestDatabricksQueryThreshold_GT_Passes — value > threshold → passed.
func TestDatabricksQueryThreshold_GT_Passes(t *testing.T) {
	stub := &stubDatabricksClient{value: 1500000}
	g := NewDatabricksQueryThreshold(stub)
	cfg := dqtCfg(t, map[string]any{
		"sql_query":    "SELECT COUNT(*) FROM users",
		"comparator":   "gt",
		"threshold":    1000000,
		"warehouse_id": "wh-abc",
	})
	passed, _, err := g.Evaluate(context.Background(), nil, StageContext{GateConfig: cfg})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !passed {
		t.Error("expected passed=true with 1.5M > 1M")
	}
}

// TestDatabricksQueryThreshold_GTE_Equal_Passes — equality satisfies >=.
func TestDatabricksQueryThreshold_GTE_Equal_Passes(t *testing.T) {
	stub := &stubDatabricksClient{value: 100}
	g := NewDatabricksQueryThreshold(stub)
	cfg := dqtCfg(t, map[string]any{
		"sql_query":    "SELECT shard_count FROM migration",
		"comparator":   "gte",
		"threshold":    100,
		"warehouse_id": "wh-abc",
	})
	passed, _, err := g.Evaluate(context.Background(), nil, StageContext{GateConfig: cfg})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !passed {
		t.Error("expected passed=true with 100 >= 100")
	}
}

// TestDatabricksQueryThreshold_EQ_WithinTolerance_Passes — eq tolerates the
// 1e-9 ULP fuzz that SQL aggregates routinely introduce.
func TestDatabricksQueryThreshold_EQ_WithinTolerance_Passes(t *testing.T) {
	// 1e-12 difference — well under the 1e-9 tolerance.
	stub := &stubDatabricksClient{value: 42.0 + 1e-12}
	g := NewDatabricksQueryThreshold(stub)
	cfg := dqtCfg(t, map[string]any{
		"sql_query":    "SELECT total FROM ledger",
		"comparator":   "eq",
		"threshold":    42.0,
		"warehouse_id": "wh-abc",
	})
	passed, reason, err := g.Evaluate(context.Background(), nil, StageContext{GateConfig: cfg})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !passed {
		t.Errorf("expected passed=true within tolerance, reason=%q", reason)
	}
}

// TestDatabricksQueryThreshold_Transient_Pending — 5xx / network → ErrPending.
func TestDatabricksQueryThreshold_Transient_Pending(t *testing.T) {
	stub := &stubDatabricksClient{err: fmt.Errorf("warehouse paused: %w", databricks.ErrTransient)}
	g := NewDatabricksQueryThreshold(stub)
	cfg := dqtCfg(t, map[string]any{
		"sql_query":    "SELECT 1",
		"comparator":   "lt",
		"threshold":    1,
		"warehouse_id": "wh-abc",
	})
	passed, reason, err := g.Evaluate(context.Background(), nil, StageContext{GateConfig: cfg})
	if !errors.Is(err, ErrPending) {
		t.Fatalf("expected ErrPending on transient, got %v", err)
	}
	if passed {
		t.Error("expected passed=false on transient")
	}
	if !strings.Contains(reason, "transient") {
		t.Errorf("expected reason to mention transient, got %q", reason)
	}
}

// TestDatabricksQueryThreshold_Timeout_Pending — query timeout → ErrPending.
func TestDatabricksQueryThreshold_Timeout_Pending(t *testing.T) {
	stub := &stubDatabricksClient{err: fmt.Errorf("ExecuteQuery: %w", databricks.ErrTimeout)}
	g := NewDatabricksQueryThreshold(stub)
	cfg := dqtCfg(t, map[string]any{
		"sql_query":       "SELECT slow()",
		"comparator":      "lt",
		"threshold":       1,
		"warehouse_id":    "wh-abc",
		"timeout_seconds": 1,
	})
	passed, reason, err := g.Evaluate(context.Background(), nil, StageContext{GateConfig: cfg})
	if !errors.Is(err, ErrPending) {
		t.Fatalf("expected ErrPending on timeout, got %v", err)
	}
	if passed {
		t.Error("expected passed=false on timeout")
	}
	if !strings.Contains(reason, "timeout") {
		t.Errorf("expected reason to mention timeout, got %q", reason)
	}
}

// TestDatabricksQueryThreshold_AuthFailure_Fails — 401/403 → fail-clean (not
// pending). Auth failure is operator-actionable; retrying won't help.
func TestDatabricksQueryThreshold_AuthFailure_Fails(t *testing.T) {
	stub := &stubDatabricksClient{err: fmt.Errorf("HTTP 401: %w", databricks.ErrAuthFailure)}
	g := NewDatabricksQueryThreshold(stub)
	cfg := dqtCfg(t, map[string]any{
		"sql_query":    "SELECT 1",
		"comparator":   "lt",
		"threshold":    1,
		"warehouse_id": "wh-abc",
	})
	passed, reason, err := g.Evaluate(context.Background(), nil, StageContext{GateConfig: cfg})
	if err != nil {
		t.Fatalf("expected nil err on auth failure (concrete fail), got %v", err)
	}
	if passed {
		t.Error("expected passed=false on auth failure")
	}
	if !strings.Contains(reason, "auth failure") {
		t.Errorf("expected reason to mention auth failure, got %q", reason)
	}
}

// TestDatabricksQueryThreshold_ShapeUnexpected_Fails — multi-row / non-scalar
// → fail-clean. Operator must fix the SQL.
func TestDatabricksQueryThreshold_ShapeUnexpected_Fails(t *testing.T) {
	stub := &stubDatabricksClient{err: fmt.Errorf("3 rows (expected 1): %w", databricks.ErrShapeUnexpected)}
	g := NewDatabricksQueryThreshold(stub)
	cfg := dqtCfg(t, map[string]any{
		"sql_query":    "SELECT * FROM users",
		"comparator":   "lt",
		"threshold":    1,
		"warehouse_id": "wh-abc",
	})
	passed, reason, err := g.Evaluate(context.Background(), nil, StageContext{GateConfig: cfg})
	if err != nil {
		t.Fatalf("expected nil err on shape unexpected, got %v", err)
	}
	if passed {
		t.Error("expected passed=false on shape unexpected")
	}
	if !strings.Contains(reason, "not scalar") {
		t.Errorf("expected reason to mention 'not scalar', got %q", reason)
	}
}

// TestDatabricksQueryThreshold_OtherError_PropagatesError — non-sentinel
// errors propagate as structural errors (caller gets a wrapped error,
// not ErrPending and not a fail-clean).
func TestDatabricksQueryThreshold_OtherError_PropagatesError(t *testing.T) {
	stub := &stubDatabricksClient{err: errors.New("something genuinely weird")}
	g := NewDatabricksQueryThreshold(stub)
	cfg := dqtCfg(t, map[string]any{
		"sql_query":    "SELECT 1",
		"comparator":   "lt",
		"threshold":    1,
		"warehouse_id": "wh-abc",
	})
	_, _, err := g.Evaluate(context.Background(), nil, StageContext{GateConfig: cfg})
	if err == nil {
		t.Fatal("expected error on unclassified failure")
	}
	if errors.Is(err, ErrPending) {
		t.Errorf("unclassified error must not collapse to ErrPending; got %v", err)
	}
	if !strings.Contains(err.Error(), "query") {
		t.Errorf("expected error to mention 'query', got %v", err)
	}
}

// TestDatabricksQueryThreshold_MissingQuery_Errors — empty sql_query is a
// structural config error, not a runtime fail.
func TestDatabricksQueryThreshold_MissingQuery_Errors(t *testing.T) {
	stub := &stubDatabricksClient{value: 1}
	g := NewDatabricksQueryThreshold(stub)
	cfg := dqtCfg(t, map[string]any{
		"comparator":   "lt",
		"threshold":    1,
		"warehouse_id": "wh-abc",
	})
	_, _, err := g.Evaluate(context.Background(), nil, StageContext{GateConfig: cfg})
	if err == nil {
		t.Fatal("expected error on empty sql_query")
	}
	if !strings.Contains(err.Error(), "sql_query required") {
		t.Errorf("expected error to mention 'sql_query required', got %v", err)
	}
	if stub.executeCalls != 0 {
		t.Errorf("expected no client call on missing query, got %d", stub.executeCalls)
	}
}

// TestDatabricksQueryThreshold_MissingWarehouseID_Errors — empty warehouse_id
// is a structural config error.
func TestDatabricksQueryThreshold_MissingWarehouseID_Errors(t *testing.T) {
	stub := &stubDatabricksClient{value: 1}
	g := NewDatabricksQueryThreshold(stub)
	cfg := dqtCfg(t, map[string]any{
		"sql_query":  "SELECT 1",
		"comparator": "lt",
		"threshold":  1,
	})
	_, _, err := g.Evaluate(context.Background(), nil, StageContext{GateConfig: cfg})
	if err == nil {
		t.Fatal("expected error on empty warehouse_id")
	}
	if !strings.Contains(err.Error(), "warehouse_id required") {
		t.Errorf("expected error to mention 'warehouse_id required', got %v", err)
	}
}

// TestDatabricksQueryThreshold_InvalidComparator_Errors — comparator outside
// {lt,gt,eq,lte,gte} is a structural error.
func TestDatabricksQueryThreshold_InvalidComparator_Errors(t *testing.T) {
	stub := &stubDatabricksClient{value: 1}
	g := NewDatabricksQueryThreshold(stub)
	cfg := dqtCfg(t, map[string]any{
		"sql_query":    "SELECT 1",
		"comparator":   "neq",
		"threshold":    1,
		"warehouse_id": "wh-abc",
	})
	_, _, err := g.Evaluate(context.Background(), nil, StageContext{GateConfig: cfg})
	if err == nil {
		t.Fatal("expected error on invalid comparator")
	}
	if !strings.Contains(err.Error(), "invalid comparator") {
		t.Errorf("expected error to mention 'invalid comparator', got %v", err)
	}
	// Also sanity-check the "comparator missing" branch separately, since
	// they share a code path neighborhood.
	cfg2 := dqtCfg(t, map[string]any{
		"sql_query":    "SELECT 1",
		"threshold":    1,
		"warehouse_id": "wh-abc",
	})
	_, _, err = g.Evaluate(context.Background(), nil, StageContext{GateConfig: cfg2})
	if err == nil || !strings.Contains(err.Error(), "comparator required") {
		t.Errorf("expected 'comparator required' on missing op, got %v", err)
	}
}

// TestDatabricksQueryThreshold_NilClient_Errors — constructing the gate
// without a Databricks client must surface a structural error rather
// than nil-deref panicking.
func TestDatabricksQueryThreshold_NilClient_Errors(t *testing.T) {
	g := NewDatabricksQueryThreshold(nil)
	cfg := dqtCfg(t, map[string]any{
		"sql_query":    "SELECT 1",
		"comparator":   "lt",
		"threshold":    1,
		"warehouse_id": "wh-abc",
	})
	_, _, err := g.Evaluate(context.Background(), nil, StageContext{GateConfig: cfg})
	if err == nil {
		t.Fatal("expected error on nil client")
	}
	if !strings.Contains(err.Error(), "nil client") {
		t.Errorf("expected error to mention 'nil client', got %v", err)
	}
}

// TestDatabricksQueryThreshold_DefaultTimeout_AppliedWhenZero — when
// timeout_seconds is omitted (or zero), the gate substitutes the 60s
// default and passes that through to the client.
func TestDatabricksQueryThreshold_DefaultTimeout_AppliedWhenZero(t *testing.T) {
	stub := &stubDatabricksClient{value: 1}
	g := NewDatabricksQueryThreshold(stub)
	cfg := dqtCfg(t, map[string]any{
		"sql_query":    "SELECT 1",
		"comparator":   "gte",
		"threshold":    0,
		"warehouse_id": "wh-abc",
		// timeout_seconds intentionally omitted.
	})
	if _, _, err := g.Evaluate(context.Background(), nil, StageContext{GateConfig: cfg}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if stub.gotTimeout != 60*time.Second {
		t.Errorf("expected default 60s timeout, got %v", stub.gotTimeout)
	}
	if stub.gotWarehouse != "wh-abc" {
		t.Errorf("expected warehouse_id pass-through, got %q", stub.gotWarehouse)
	}
	if stub.gotSQL != "SELECT 1" {
		t.Errorf("expected sql_query pass-through, got %q", stub.gotSQL)
	}
}
