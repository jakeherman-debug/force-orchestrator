package stagegate

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"force-orchestrator/internal/clients/databricks"
)

// DatabricksQueryThreshold is the D5.5 P3 advanced leaf gate that runs a SQL
// query against a configured Databricks SQL warehouse and compares the
// scalar result against a numeric threshold using a comparator
// (lt|gt|eq|lte|gte). It's the natural bridge between staged convoys and
// real data correctness: rather than waiting on a soak window or polling
// HTTP, the gate asks the data lake "are the rows we expect there?".
//
// Roadmap motivating uses (docs/roadmap.md § Deliverable 5.5):
//   - Backfill complete: count of rows with new column populated == total.
//   - Migration shards reported success: aggregate status table.
//   - No data drift: distribution match within tolerance.
//
// Config shape (the JSON object stored in ConvoyStages.gate_config_json):
//
//	{
//	  "sql_query":        "SELECT COUNT(*) FROM users WHERE col IS NOT NULL",
//	  "comparator":       "gte",
//	  "threshold":        1000000,
//	  "warehouse_id":     "abc123def456",
//	  "timeout_seconds":  60
//	}
//
// Evaluation contract:
//   - Transient API errors (5xx, network, warehouse paused) →
//     ErrPending. The dog re-checks next tick; a flapping warehouse
//     shouldn't permanently fail the convoy.
//   - Statement-execution timeout → ErrPending. Same reasoning: long
//     queries may finish on the next tick when the warehouse is warm.
//   - 401/403 from Databricks → passed=false with err=nil. Auth failure
//     is operator-actionable (rotate the PAT); the convoy should fail
//     and page the operator rather than silently retry forever.
//   - Query result not a single scalar (multi-row, multi-col, non-numeric)
//     → passed=false with err=nil. Operator must fix the SQL — retrying
//     won't change anything.
//   - Threshold comparison fails → passed=false with err=nil (concrete
//     "data not yet ready" signal).
//   - Other errors (config parsing, unexpected client failures) →
//     structural error.
type DatabricksQueryThreshold struct {
	client databricks.Client
}

// NewDatabricksQueryThreshold returns a gate wrapping the supplied
// Databricks client. Production callers wire this with a databricks
// in-process client; tests pass a stub implementing databricks.Client.
func NewDatabricksQueryThreshold(client databricks.Client) DatabricksQueryThreshold {
	return DatabricksQueryThreshold{client: client}
}

// Type implements Gate.
func (DatabricksQueryThreshold) Type() string { return "databricks_query_threshold" }

// Evaluate implements Gate.
func (d DatabricksQueryThreshold) Evaluate(ctx context.Context, _ *sql.DB, stage StageContext) (bool, string, error) {
	var cfg struct {
		SQLQuery       string  `json:"sql_query"`
		Comparator     string  `json:"comparator"`
		Threshold      float64 `json:"threshold"`
		WarehouseID    string  `json:"warehouse_id"`
		TimeoutSeconds int     `json:"timeout_seconds"`
	}
	if err := json.Unmarshal(stage.GateConfig, &cfg); err != nil {
		return false, "", fmt.Errorf("databricks_query_threshold: parse config: %w", err)
	}
	if cfg.SQLQuery == "" {
		return false, "", fmt.Errorf("databricks_query_threshold: sql_query required")
	}
	if cfg.WarehouseID == "" {
		return false, "", fmt.Errorf("databricks_query_threshold: warehouse_id required")
	}
	if cfg.Comparator == "" {
		return false, "", fmt.Errorf("databricks_query_threshold: comparator required")
	}
	if !validComparator(cfg.Comparator) {
		return false, "", fmt.Errorf("databricks_query_threshold: invalid comparator %q", cfg.Comparator)
	}
	timeout := time.Duration(cfg.TimeoutSeconds) * time.Second
	if timeout == 0 {
		timeout = 60 * time.Second
	}
	if d.client == nil {
		return false, "", fmt.Errorf("databricks_query_threshold: nil client")
	}

	value, err := d.client.ExecuteQuery(ctx, cfg.WarehouseID, cfg.SQLQuery, timeout)
	switch {
	case errors.Is(err, databricks.ErrTransient):
		return false, fmt.Sprintf("databricks: transient: %v", err), ErrPending
	case errors.Is(err, databricks.ErrTimeout):
		return false, fmt.Sprintf("databricks: timeout: %v", err), ErrPending
	case errors.Is(err, databricks.ErrAuthFailure):
		// Auth failure is operator-actionable (rotate the PAT); fail the
		// gate concretely rather than retrying forever.
		return false, fmt.Sprintf("databricks: auth failure: %v", err), nil
	case errors.Is(err, databricks.ErrShapeUnexpected):
		// SQL returned non-scalar; operator must fix the query.
		return false, fmt.Sprintf("databricks: result not scalar: %v", err), nil
	case err != nil:
		return false, "", fmt.Errorf("databricks_query_threshold: query: %w", err)
	}

	if !compareValue(cfg.Comparator, value, cfg.Threshold) {
		return false, fmt.Sprintf("databricks: query → %.4f; %s %.4f failed", value, cfg.Comparator, cfg.Threshold), nil
	}
	return true, fmt.Sprintf("databricks: query → %.4f; %s %.4f passed", value, cfg.Comparator, cfg.Threshold), nil
}
