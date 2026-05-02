// Package datadog: in-process client backed by Datadog's v1 HTTPS API.
//
// Construction is via NewInProcess(ctx, db) — SystemConfig supplies
// datadog_api_key / datadog_app_key / datadog_base_url. Force does
// NOT cache a token here: every QueryMetric call sets the
// DD-API-KEY + DD-APPLICATION-KEY headers fresh from the in-memory
// snapshot read at construction. Rotation happens via daemon restart
// (re-read SystemConfig).
//
// Pattern P16: only `Client`, NewInProcess, the sentinels, and the
// ConfigKey* / DefaultBaseURL constants are exported. The
// inProcessClient struct is unexported; callers obtain it through the
// NewInProcess factory.
package datadog

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"force-orchestrator/internal/store"
)

// httpDoer is the minimal http surface this package needs. Defining
// it as an interface (rather than calling *http.Client directly) lets
// the unit tests stub the network at the API boundary — no real
// Datadog calls in CI. Pattern P16 still holds because the interface
// lives inside this package and the production type embedding is the
// concrete *http.Client.
type httpDoer interface {
	Do(req *http.Request) (*http.Response, error)
}

// inProcessClient is the production Client backing. Per Pattern P16,
// the type is unexported; callers obtain it through NewInProcess.
type inProcessClient struct {
	apiKey     string
	appKey     string
	baseURL    string
	httpClient httpDoer
}

// NewInProcess returns a Client that talks to Datadog's HTTPS API.
//
// Configuration (read at construction; rotation happens via daemon
// restart):
//   - SystemConfig.datadog_api_key  — Datadog API key  (required)
//   - SystemConfig.datadog_app_key  — Datadog APP key  (required for
//     the v1 query API)
//   - SystemConfig.datadog_base_url — defaults to https://api.datadoghq.com
//
// Returns ErrConfig if api_key or app_key is empty.
func NewInProcess(ctx context.Context, db *sql.DB) (Client, error) {
	apiKey := ""
	appKey := ""
	baseURL := DefaultBaseURL

	if db != nil {
		apiKey = store.GetConfig(db, ConfigKeyAPIKey, "")
		appKey = store.GetConfig(db, ConfigKeyAPPKey, "")
		if v := store.GetConfig(db, ConfigKeyBaseURL, ""); v != "" {
			baseURL = v
		}
	}

	if apiKey == "" {
		return nil, fmt.Errorf("%w: datadog_api_key is required", ErrConfig)
	}
	if appKey == "" {
		return nil, fmt.Errorf("%w: datadog_app_key is required", ErrConfig)
	}

	return &inProcessClient{
		apiKey:     apiKey,
		appKey:     appKey,
		baseURL:    baseURL,
		httpClient: &http.Client{Timeout: 30 * time.Second},
	}, nil
}

// newInProcessFromHTTP is the test-only constructor that injects a
// pre-built httpDoer stub plus an explicit base URL (typically the
// httptest.Server URL). Kept package-private so production code
// cannot accidentally bypass the credential read path.
func newInProcessFromHTTP(doer httpDoer, baseURL, apiKey, appKey string) Client {
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	return &inProcessClient{
		apiKey:     apiKey,
		appKey:     appKey,
		baseURL:    baseURL,
		httpClient: doer,
	}
}

// queryAPIResponse mirrors the relevant subset of Datadog's
// `GET /api/v1/query` response payload. Only the fields the gate needs
// are surfaced — the SDK has dozens more (units, scope, expression,
// metadata) and we deliberately do not depend on them so a future
// API revision that adds fields stays compatible.
//
// Reference shape (from Datadog docs):
//
//	{
//	  "status": "ok",
//	  "series": [
//	    {
//	      "metric": "system.load.1",
//	      "pointlist": [[1612345678000.0, 0.42], [1612345738000.0, 0.55]]
//	    }
//	  ]
//	}
//
// `pointlist` entries are [unix-ms, value]. Datadog occasionally
// returns a JSON null for value when a point is missing; we treat
// those as "skip" and walk backwards to the most recent non-null.
type queryAPIResponse struct {
	Status string                 `json:"status"`
	Error  string                 `json:"error,omitempty"`
	Series []queryAPISeriesEntry  `json:"series"`
}

type queryAPISeriesEntry struct {
	Metric    string          `json:"metric,omitempty"`
	Pointlist [][]json.Number `json:"pointlist"`
}

// QueryMetric calls GET /api/v1/query with the supplied window and
// returns the latest non-null point in the first series.
func (c *inProcessClient) QueryMetric(ctx context.Context, query string, window time.Duration) (float64, time.Time, error) {
	if query == "" {
		return 0, time.Time{}, fmt.Errorf("QueryMetric: query is required")
	}
	if window <= 0 {
		return 0, time.Time{}, fmt.Errorf("QueryMetric: window must be positive, got %s", window)
	}

	now := time.Now().UTC()
	from := now.Add(-window)

	endpoint, err := url.Parse(c.baseURL)
	if err != nil {
		return 0, time.Time{}, fmt.Errorf("QueryMetric: parse base URL: %w", err)
	}
	endpoint.Path = "/api/v1/query"
	q := endpoint.Query()
	q.Set("from", strconv.FormatInt(from.Unix(), 10))
	q.Set("to", strconv.FormatInt(now.Unix(), 10))
	q.Set("query", query)
	endpoint.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return 0, time.Time{}, fmt.Errorf("QueryMetric: build request: %w", err)
	}
	req.Header.Set("DD-API-KEY", c.apiKey)
	req.Header.Set("DD-APPLICATION-KEY", c.appKey)
	req.Header.Set("Accept", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		// Network-level failure: connection refused, DNS, TLS, EOF
		// during read, ctx-cancelled, etc. All map to transient so the
		// gate retries on the next dog tick rather than escalating.
		return 0, time.Time{}, fmt.Errorf("QueryMetric: %w: %v", ErrTransient, err)
	}
	defer resp.Body.Close()

	if err := mapHTTPStatus("QueryMetric", resp.StatusCode); err != nil {
		return 0, time.Time{}, err
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, time.Time{}, fmt.Errorf("QueryMetric: %w: read body: %v", ErrTransient, err)
	}

	var parsed queryAPIResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		// Malformed JSON from a 2xx is almost always a transient
		// upstream blip (proxy returned an HTML error page with a 200,
		// for example). Map to transient — the gate's next tick will
		// retry.
		return 0, time.Time{}, fmt.Errorf("QueryMetric: %w: malformed response: %v", ErrTransient, err)
	}

	// Datadog signals a query-side problem with `status:"error"` even
	// on a 200. Treat that as transient — the typical cause is a
	// per-org rate limit or a backend hiccup that retries cleanly.
	if parsed.Status != "" && parsed.Status != "ok" {
		return 0, time.Time{}, fmt.Errorf("QueryMetric: %w: api status %q (%s)", ErrTransient, parsed.Status, parsed.Error)
	}

	if len(parsed.Series) == 0 {
		return 0, time.Time{}, fmt.Errorf("QueryMetric: %w (query=%q)", ErrNoData, query)
	}

	// Walk the first series back-to-front and return the most recent
	// point with a non-null value. Datadog represents missing points
	// as JSON null inside the [ts, value] pair; json.Number leaves
	// those as empty strings.
	series := parsed.Series[0]
	for i := len(series.Pointlist) - 1; i >= 0; i-- {
		pair := series.Pointlist[i]
		if len(pair) < 2 {
			continue
		}
		valStr := pair[1].String()
		if valStr == "" || valStr == "null" {
			continue
		}
		val, err := pair[1].Float64()
		if err != nil {
			continue
		}
		tsStr := pair[0].String()
		tsMs, err := strconv.ParseFloat(tsStr, 64)
		if err != nil {
			// Malformed timestamp on an otherwise-good point — fall
			// back to "now" rather than discarding the value.
			return val, now, nil
		}
		at := time.Unix(0, int64(tsMs*float64(time.Millisecond))).UTC()
		return val, at, nil
	}

	return 0, time.Time{}, fmt.Errorf("QueryMetric: %w: series had no non-null points", ErrNoData)
}

// Health calls GET /api/v1/validate. Only DD-API-KEY is required for
// this endpoint, but we send DD-APPLICATION-KEY too so a misconfigured
// app_key is caught at probe time rather than first QueryMetric call.
func (c *inProcessClient) Health(ctx context.Context) error {
	endpoint, err := url.Parse(c.baseURL)
	if err != nil {
		return fmt.Errorf("Health: parse base URL: %w", err)
	}
	endpoint.Path = "/api/v1/validate"

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return fmt.Errorf("Health: build request: %w", err)
	}
	req.Header.Set("DD-API-KEY", c.apiKey)
	req.Header.Set("DD-APPLICATION-KEY", c.appKey)
	req.Header.Set("Accept", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("Health: %w: %v", ErrTransient, err)
	}
	defer resp.Body.Close()
	// Drain the body so the connection can be reused; we don't need
	// the payload for a 200 (Datadog returns {"valid":true}).
	_, _ = io.Copy(io.Discard, resp.Body)

	return mapHTTPStatus("Health", resp.StatusCode)
}

// mapHTTPStatus turns a status code into our sentinel error scheme.
// 2xx → nil, 401/403 → ErrAuthFailure, 5xx → ErrTransient, every
// other non-2xx → ErrTransient (covers 408 Request Timeout, 429 Rate
// Limit, etc. — all of which the gate can safely retry).
func mapHTTPStatus(op string, status int) error {
	switch {
	case status >= 200 && status < 300:
		return nil
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return fmt.Errorf("%s: %w (status=%d)", op, ErrAuthFailure, status)
	case status >= 500 && status < 600:
		return fmt.Errorf("%s: %w (status=%d)", op, ErrTransient, status)
	default:
		// 4xx other than 401/403 (400 Bad Request, 404, 408, 429, ...)
		// — treat as transient. The gate will re-evaluate; if the
		// query is genuinely malformed the operator sees the same
		// error repeatedly and can fix it.
		return fmt.Errorf("%s: %w (status=%d)", op, ErrTransient, status)
	}
}
