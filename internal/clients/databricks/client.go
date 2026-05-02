// Package databricks defines the Client interface for the Databricks
// SQL Statement Execution API used by the `databricks_query_threshold`
// staged-convoy gate (docs/roadmap.md § Deliverable 5.5, P3 leaf gates).
//
// The gate runs a SQL query against a configured Databricks SQL
// warehouse and compares the scalar result against a threshold using
// one of the comparator operators (lt|gt|eq|lte|gte). Use cases per
// roadmap:
//
//   - "backfill complete: count of rows with new column populated ==
//     total count"
//   - "no data drift: distribution match within tolerance"
//   - "all migration shards reported success"
//
// Pattern P16
// (internal/audittools/audit_pattern_p16_clients_interfaces_test.go)
// enforces that production code references the Client interface only
// — never a concrete struct. Construction is via the exported
// NewInProcess factory; future siblings (gRPC, mock) follow the same
// shape.
//
// Implementations live as siblings:
//   - inprocess.go — backed by net/http calling the Databricks
//     Statement Execution API
//     (https://docs.databricks.com/api/workspace/statementexecution)
//     with a per-instance bearer token and workspace URL pulled from
//     SystemConfig. Used in production.
//
// Auth model: Force does not cache tokens. The Databricks PAT is read
// from SystemConfig at constructor time; rotation requires `force
// config set databricks_token <new>` and a fresh client. 401/403 from
// the API surface as ErrAuthFailure so the gate fails closed and the
// operator is paged.
package databricks

import (
	"context"
	"errors"
	"time"
)

// Client is the Databricks SQL Statement Execution API interface used
// by the databricks_query_threshold staged-convoy gate.
//
// Routes through the Statement Execution API
// (https://docs.databricks.com/api/workspace/statementexecution).
type Client interface {
	// ExecuteQuery runs a SQL statement against the configured warehouse
	// and returns the first row's first column as a scalar float64.
	//
	// SQL is expected to return a single scalar value (e.g., "SELECT
	// COUNT(*) FROM users WHERE col IS NOT NULL"). Multi-row or multi-
	// column results trigger ErrShapeUnexpected.
	//
	// warehouseID identifies the SQL warehouse; passes through to the
	// API as the JSON body's `warehouse_id` field.
	//
	// timeout is the wall-clock budget for the API call (waits for the
	// statement to complete or times out). The Statement Execution API
	// supports a synchronous wait of up to 50s; longer queries are
	// polled internally until the deadline.
	//
	// Errors:
	//   ErrTransient       — 5xx, network, or warehouse-paused.
	//   ErrAuthFailure     — 401/403.
	//   ErrShapeUnexpected — query result wasn't a single scalar.
	//   ErrTimeout         — API didn't respond within the timeout.
	//   ErrConfig          — workspace URL or token not configured.
	ExecuteQuery(ctx context.Context, warehouseID, sqlQuery string, timeout time.Duration) (value float64, err error)

	// Health probes the Databricks workspace to confirm credentials +
	// reachability without running a query. Implementations call a low-
	// cost endpoint such as GET /api/2.0/preview/scim/v2/Me.
	//
	// Errors mirror ExecuteQuery: ErrAuthFailure on 401/403,
	// ErrTransient on 5xx / network.
	Health(ctx context.Context) error
}

// Sentinel errors. Callers compare with errors.Is.
var (
	// ErrTransient wraps 5xx, network errors, and warehouse-paused
	// states. Callers may retry with backoff.
	ErrTransient = errors.New("databricks: transient (5xx or network)")

	// ErrAuthFailure wraps 401/403 from the API. The gate fails closed
	// and pages the operator; not a retry-class error.
	ErrAuthFailure = errors.New("databricks: auth failure (401/403)")

	// ErrShapeUnexpected is returned when the query result is not a
	// single scalar (multi-row, multi-column, or non-numeric value).
	// The gate treats this as a configuration error in the rule's SQL.
	ErrShapeUnexpected = errors.New("databricks: query result not a single scalar")

	// ErrTimeout is returned when the API or polling loop exceeds the
	// caller-supplied timeout. The gate may retry or escalate.
	ErrTimeout = errors.New("databricks: query exceeded timeout")

	// ErrConfig is returned by NewInProcess when the workspace URL or
	// PAT is missing from SystemConfig. Distinct from ErrAuthFailure so
	// callers can distinguish "operator never configured this" from
	// "the configured creds got rejected by the API."
	ErrConfig = errors.New("databricks: config invalid (workspace URL or token missing)")
)

// SystemConfig keys consumed by NewInProcess. Both are required — there
// is no sensible default for a per-tenant Databricks workspace.
const (
	// ConfigKeyWorkspaceURL is the base URL of the Databricks workspace,
	// e.g. https://upstart-dbx.cloud.databricks.com (no trailing slash).
	ConfigKeyWorkspaceURL = "databricks_workspace_url"

	// ConfigKeyToken is the Databricks personal access token (PAT).
	// Stored encrypted at rest via store.SetConfig conventions; rotated
	// out-of-band by the operator.
	ConfigKeyToken = "databricks_token"
)
