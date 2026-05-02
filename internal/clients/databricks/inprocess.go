// Package databricks: in-process client backed by net/http calling the
// Databricks SQL Statement Execution API.
//
// Construction is via NewInProcess(ctx, db) — the SystemConfig handle
// supplies the workspace URL and PAT. There is no AWS-style credential
// chain; the PAT is provided directly by the operator and rotated via
// `force config set databricks_token <new>`.
//
// Wire format: the Statement Execution API is async-first but supports
// a synchronous wait of up to 50s. We pass `wait_timeout` as
// `<min(timeout, 50)>s` and `on_wait_timeout=CONTINUE`. If the
// statement is still RUNNING when the wait expires we poll
// GET /api/2.0/sql/statements/<id> until the deadline. State machine:
//
//   PENDING / RUNNING → poll
//   SUCCEEDED         → parse data_array → scalar float64
//   FAILED / CANCELED → ErrTransient (FAILED is usually warehouse-side
//                       — query syntax errors also surface as FAILED;
//                       the caller's gate-failure path treats both as
//                       a fail-closed signal and pages the operator)
//   CLOSED            → ErrTransient (statement abandoned)
//
// Pattern P16 holds: the type below is unexported; callers obtain a
// Client through NewInProcess. Tests construct an instance via the
// package-private newInProcessFromHTTP helper that injects a custom
// base URL pointing at httptest.Server.
package databricks

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"force-orchestrator/internal/store"
)

// Default HTTP timeout. The per-call timeout passed to ExecuteQuery
// dominates this; the Client.Timeout is a belt-and-braces ceiling so a
// stuck connection eventually fails.
const defaultHTTPTimeout = 60 * time.Second

// maxSyncWaitSeconds is the largest value the Databricks Statement
// Execution API accepts for `wait_timeout`. Beyond this we poll.
const maxSyncWaitSeconds = 50

// pollInterval is the gap between polls when the API returns
// PENDING/RUNNING. Conservative — Databricks rate-limits aggressive
// pollers.
const pollInterval = 2 * time.Second

// inProcessClient is the production Client backing. Per Pattern P16,
// the type is unexported; callers obtain it through NewInProcess.
type inProcessClient struct {
	workspaceURL string // no trailing slash
	token        string
	httpClient   *http.Client
}

// NewInProcess returns a Client backed by the Databricks REST API.
// Workspace URL and token come from SystemConfig.
//
// Returns ErrConfig if either workspace_url or token is empty.
func NewInProcess(ctx context.Context, db *sql.DB) (Client, error) {
	if db == nil {
		return nil, fmt.Errorf("%w: nil db handle", ErrConfig)
	}
	workspaceURL := strings.TrimRight(store.GetConfig(db, ConfigKeyWorkspaceURL, ""), "/")
	token := store.GetConfig(db, ConfigKeyToken, "")
	if workspaceURL == "" || token == "" {
		return nil, fmt.Errorf("%w: workspace_url and token required", ErrConfig)
	}
	return &inProcessClient{
		workspaceURL: workspaceURL,
		token:        token,
		httpClient:   &http.Client{Timeout: defaultHTTPTimeout},
	}, nil
}

// newInProcessFromHTTP is the test-only constructor: injects a base URL
// (typically an httptest.Server.URL) and a token. Kept package-private
// so production code cannot accidentally bypass SystemConfig.
func newInProcessFromHTTP(baseURL, token string, hc *http.Client) Client {
	if hc == nil {
		hc = &http.Client{Timeout: defaultHTTPTimeout}
	}
	return &inProcessClient{
		workspaceURL: strings.TrimRight(baseURL, "/"),
		token:        token,
		httpClient:   hc,
	}
}

// statementResponse models the subset of the Statement Execution API's
// response that we consume. See:
// https://docs.databricks.com/api/workspace/statementexecution/executestatement
type statementResponse struct {
	StatementID string `json:"statement_id"`
	Status      struct {
		State string `json:"state"` // PENDING|RUNNING|SUCCEEDED|FAILED|CANCELED|CLOSED
		Error *struct {
			Message string `json:"message"`
			Code    string `json:"error_code"`
		} `json:"error,omitempty"`
	} `json:"status"`
	Manifest struct {
		Schema struct {
			ColumnCount int `json:"column_count"`
			Columns     []struct {
				Name     string `json:"name"`
				TypeName string `json:"type_name"`
			} `json:"columns"`
		} `json:"schema"`
		TotalRowCount int64 `json:"total_row_count"`
	} `json:"manifest"`
	Result *struct {
		DataArray [][]string `json:"data_array"`
		RowCount  int64      `json:"row_count"`
	} `json:"result,omitempty"`
}

// ExecuteQuery runs a SQL statement and returns the scalar result.
func (c *inProcessClient) ExecuteQuery(ctx context.Context, warehouseID, sqlQuery string, timeout time.Duration) (float64, error) {
	if c.workspaceURL == "" || c.token == "" {
		return 0, fmt.Errorf("ExecuteQuery: %w", ErrConfig)
	}
	if warehouseID == "" || sqlQuery == "" {
		return 0, fmt.Errorf("ExecuteQuery: warehouse_id and sql_query are required")
	}
	if timeout <= 0 {
		return 0, fmt.Errorf("ExecuteQuery: %w: timeout must be positive", ErrTimeout)
	}

	// Bound the call by the caller-supplied timeout. The deadline is
	// shared between the initial POST and any polling.
	callCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	syncWait := int(timeout / time.Second)
	if syncWait < 1 {
		syncWait = 1
	}
	if syncWait > maxSyncWaitSeconds {
		syncWait = maxSyncWaitSeconds
	}

	body := map[string]interface{}{
		"warehouse_id":      warehouseID,
		"statement":         sqlQuery,
		"wait_timeout":      fmt.Sprintf("%ds", syncWait),
		"on_wait_timeout":   "CONTINUE",
		"disposition":       "INLINE",
		"format":            "JSON_ARRAY",
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return 0, fmt.Errorf("ExecuteQuery: marshal request: %w", err)
	}

	endpoint := c.workspaceURL + "/api/2.0/sql/statements"
	req, err := http.NewRequestWithContext(callCtx, http.MethodPost, endpoint, strings.NewReader(string(payload)))
	if err != nil {
		return 0, fmt.Errorf("ExecuteQuery: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return 0, mapTransportError("ExecuteQuery", callCtx, err)
	}
	defer resp.Body.Close()

	if mapped := mapHTTPStatus("ExecuteQuery", resp.StatusCode); mapped != nil {
		return 0, mapped
	}

	var sr statementResponse
	if err := json.NewDecoder(resp.Body).Decode(&sr); err != nil {
		return 0, fmt.Errorf("ExecuteQuery: decode response: %w", err)
	}

	// Drive the state machine to terminal.
	final, err := c.waitForCompletion(callCtx, sr)
	if err != nil {
		return 0, err
	}
	return scalarFromResponse("ExecuteQuery", final)
}

// waitForCompletion polls until the statement reaches a terminal state
// or the context deadline fires. Returns the final response on success.
func (c *inProcessClient) waitForCompletion(ctx context.Context, current statementResponse) (statementResponse, error) {
	for {
		switch strings.ToUpper(current.Status.State) {
		case "SUCCEEDED":
			return current, nil
		case "FAILED", "CANCELED", "CLOSED":
			return statementResponse{}, fmt.Errorf("ExecuteQuery: statement %s: %w", current.Status.State, ErrTransient)
		case "PENDING", "RUNNING", "":
			// Fall through to poll.
		default:
			return statementResponse{}, fmt.Errorf("ExecuteQuery: unknown statement state %q: %w", current.Status.State, ErrTransient)
		}

		if current.StatementID == "" {
			// API returned a non-terminal state but no ID — treat as
			// transient rather than infinite-loop.
			return statementResponse{}, fmt.Errorf("ExecuteQuery: non-terminal state %q with empty statement_id: %w", current.Status.State, ErrTransient)
		}

		// Sleep before the next poll, but respect ctx cancellation.
		select {
		case <-ctx.Done():
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return statementResponse{}, fmt.Errorf("ExecuteQuery: %w", ErrTimeout)
			}
			return statementResponse{}, ctx.Err()
		case <-time.After(pollInterval):
		}

		next, err := c.pollStatement(ctx, current.StatementID)
		if err != nil {
			return statementResponse{}, err
		}
		current = next
	}
}

// pollStatement issues GET /api/2.0/sql/statements/<id>.
func (c *inProcessClient) pollStatement(ctx context.Context, id string) (statementResponse, error) {
	endpoint := c.workspaceURL + "/api/2.0/sql/statements/" + url.PathEscape(id)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return statementResponse{}, fmt.Errorf("pollStatement: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return statementResponse{}, mapTransportError("pollStatement", ctx, err)
	}
	defer resp.Body.Close()

	if mapped := mapHTTPStatus("pollStatement", resp.StatusCode); mapped != nil {
		return statementResponse{}, mapped
	}

	var sr statementResponse
	if err := json.NewDecoder(resp.Body).Decode(&sr); err != nil {
		return statementResponse{}, fmt.Errorf("pollStatement: decode response: %w", err)
	}
	return sr, nil
}

// scalarFromResponse extracts the single scalar float64 from a
// SUCCEEDED response, returning ErrShapeUnexpected on any non-scalar
// shape or non-numeric value.
func scalarFromResponse(op string, sr statementResponse) (float64, error) {
	if sr.Result == nil || len(sr.Result.DataArray) == 0 {
		return 0, fmt.Errorf("%s: empty result set: %w", op, ErrShapeUnexpected)
	}
	if len(sr.Result.DataArray) > 1 {
		return 0, fmt.Errorf("%s: %d rows (expected 1): %w", op, len(sr.Result.DataArray), ErrShapeUnexpected)
	}
	row := sr.Result.DataArray[0]
	if len(row) != 1 {
		return 0, fmt.Errorf("%s: %d columns (expected 1): %w", op, len(row), ErrShapeUnexpected)
	}
	v, err := strconv.ParseFloat(strings.TrimSpace(row[0]), 64)
	if err != nil {
		return 0, fmt.Errorf("%s: non-numeric scalar %q: %w", op, row[0], ErrShapeUnexpected)
	}
	return v, nil
}

// Health probes the workspace via the SCIM Me endpoint — cheap, no
// warehouse cost, exercises the same auth path as the SQL API.
func (c *inProcessClient) Health(ctx context.Context) error {
	if c.workspaceURL == "" || c.token == "" {
		return fmt.Errorf("Health: %w", ErrConfig)
	}
	endpoint := c.workspaceURL + "/api/2.0/preview/scim/v2/Me"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return fmt.Errorf("Health: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return mapTransportError("Health", ctx, err)
	}
	defer resp.Body.Close()
	// Drain so the connection is reusable.
	_, _ = io.Copy(io.Discard, resp.Body)

	if mapped := mapHTTPStatus("Health", resp.StatusCode); mapped != nil {
		return mapped
	}
	return nil
}

// mapHTTPStatus converts a non-2xx HTTP status to a sentinel-wrapped
// error. Returns nil for 2xx.
func mapHTTPStatus(op string, status int) error {
	switch {
	case status >= 200 && status < 300:
		return nil
	case status == http.StatusUnauthorized, status == http.StatusForbidden:
		return fmt.Errorf("%s: HTTP %d: %w", op, status, ErrAuthFailure)
	case status >= 500:
		return fmt.Errorf("%s: HTTP %d: %w", op, status, ErrTransient)
	default:
		// 4xx other than 401/403 — treat as transient so the gate fails
		// closed; the underlying message reaches the audit log via the
		// %w wrap above.
		return fmt.Errorf("%s: HTTP %d: %w", op, status, ErrTransient)
	}
}

// mapTransportError converts net/http transport-layer errors to
// sentinels. Context-deadline-exceeded becomes ErrTimeout; everything
// else is ErrTransient.
func mapTransportError(op string, ctx context.Context, err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return fmt.Errorf("%s: %w", op, ErrTimeout)
	}
	if errors.Is(err, context.Canceled) {
		return fmt.Errorf("%s: %w", op, ctx.Err())
	}
	return fmt.Errorf("%s: %w: %v", op, ErrTransient, err)
}
