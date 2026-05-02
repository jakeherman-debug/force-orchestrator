package datadog

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"force-orchestrator/internal/store"
)

// ── HTTP-level stubbing helpers ─────────────────────────────────────────────

// stubDoer is the test-only stand-in for an *http.Client. The doer
// returns whatever the test set on the matching field; if doFunc is
// non-nil it overrides the static response for tests that need to
// inspect the request (header asserts, query param asserts, etc.).
type stubDoer struct {
	doFunc func(req *http.Request) (*http.Response, error)
	resp   *http.Response
	err    error
}

func (s *stubDoer) Do(req *http.Request) (*http.Response, error) {
	if s.doFunc != nil {
		return s.doFunc(req)
	}
	return s.resp, s.err
}

// withBody builds an http.Response with the supplied status and body
// the inprocess client can ReadAll/Close. Avoids importing
// io/ioutil at each callsite.
func withBody(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Body:       newStringBody(body),
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Request:    &http.Request{},
	}
}

// newStringBody is a minimal io.ReadCloser around a string. Avoids
// pulling in extra deps for the test file.
func newStringBody(s string) *stringBody {
	return &stringBody{r: strings.NewReader(s)}
}

type stringBody struct {
	r      *strings.Reader
	closed bool
}

func (b *stringBody) Read(p []byte) (int, error) { return b.r.Read(p) }
func (b *stringBody) Close() error               { b.closed = true; return nil }

// newTestClient is the shared in-test factory: wires a stubDoer into
// the unexported newInProcessFromHTTP factory. Equivalent of the
// codeartifact package's newTestClient(api).
func newTestClient(doer httpDoer, baseURL string) Client {
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	return newInProcessFromHTTP(doer, baseURL, "test-api-key", "test-app-key")
}

// ── QueryMetric: happy-path + error mapping ─────────────────────────────────

func TestQueryMetric_HappyPath(t *testing.T) {
	// Datadog returns a single series with three points; we want the
	// LAST one (1.5 at ts=2000ms).
	body := `{
	  "status": "ok",
	  "series": [
	    {
	      "metric": "system.load.1",
	      "pointlist": [[1000.0, 0.5], [1500.0, 1.0], [2000.0, 1.5]]
	    }
	  ]
	}`
	doer := &stubDoer{
		doFunc: func(req *http.Request) (*http.Response, error) {
			// Sanity: headers + path are right.
			if got := req.Header.Get("DD-API-KEY"); got != "test-api-key" {
				t.Errorf("DD-API-KEY header: want test-api-key, got %q", got)
			}
			if got := req.Header.Get("DD-APPLICATION-KEY"); got != "test-app-key" {
				t.Errorf("DD-APPLICATION-KEY header: want test-app-key, got %q", got)
			}
			if got := req.URL.Path; got != "/api/v1/query" {
				t.Errorf("path: want /api/v1/query, got %q", got)
			}
			if req.URL.Query().Get("query") == "" {
				t.Errorf("query param missing")
			}
			return withBody(200, body), nil
		},
	}
	c := newTestClient(doer, "https://api.datadoghq.com")

	val, at, err := c.QueryMetric(context.Background(), "avg:system.load.1{*}", 30*time.Minute)
	if err != nil {
		t.Fatalf("QueryMetric: unexpected error: %v", err)
	}
	if val != 1.5 {
		t.Errorf("value: want 1.5, got %v", val)
	}
	wantAt := time.Unix(0, int64(2000*float64(time.Millisecond))).UTC()
	if !at.Equal(wantAt) {
		t.Errorf("at: want %v, got %v", wantAt, at)
	}
}

func TestQueryMetric_EmptyPointlist_ErrNoData(t *testing.T) {
	body := `{"status":"ok","series":[{"metric":"foo","pointlist":[]}]}`
	doer := &stubDoer{resp: withBody(200, body)}
	c := newTestClient(doer, "https://api.datadoghq.com")
	_, _, err := c.QueryMetric(context.Background(), "avg:foo{*}", time.Minute)
	if !errors.Is(err, ErrNoData) {
		t.Fatalf("expected ErrNoData, got: %v", err)
	}
}

func TestQueryMetric_EmptySeries_ErrNoData(t *testing.T) {
	// An empty top-level "series" array is the more common
	// "no-data-yet" shape — the metric isn't emitting at all.
	body := `{"status":"ok","series":[]}`
	doer := &stubDoer{resp: withBody(200, body)}
	c := newTestClient(doer, "https://api.datadoghq.com")
	_, _, err := c.QueryMetric(context.Background(), "avg:foo{*}", time.Minute)
	if !errors.Is(err, ErrNoData) {
		t.Fatalf("expected ErrNoData, got: %v", err)
	}
}

func TestQueryMetric_5xx_ErrTransient(t *testing.T) {
	doer := &stubDoer{resp: withBody(503, "service unavailable")}
	c := newTestClient(doer, "https://api.datadoghq.com")
	_, _, err := c.QueryMetric(context.Background(), "avg:foo{*}", time.Minute)
	if !errors.Is(err, ErrTransient) {
		t.Fatalf("expected ErrTransient, got: %v", err)
	}
}

func TestQueryMetric_401_ErrAuthFailure(t *testing.T) {
	doer := &stubDoer{resp: withBody(401, `{"errors":["Forbidden"]}`)}
	c := newTestClient(doer, "https://api.datadoghq.com")
	_, _, err := c.QueryMetric(context.Background(), "avg:foo{*}", time.Minute)
	if !errors.Is(err, ErrAuthFailure) {
		t.Fatalf("expected ErrAuthFailure, got: %v", err)
	}
}

func TestQueryMetric_403_ErrAuthFailure(t *testing.T) {
	doer := &stubDoer{resp: withBody(403, `{"errors":["Forbidden"]}`)}
	c := newTestClient(doer, "https://api.datadoghq.com")
	_, _, err := c.QueryMetric(context.Background(), "avg:foo{*}", time.Minute)
	if !errors.Is(err, ErrAuthFailure) {
		t.Fatalf("expected ErrAuthFailure, got: %v", err)
	}
}

func TestQueryMetric_NetworkError_ErrTransient(t *testing.T) {
	doer := &stubDoer{err: errors.New("connection refused")}
	c := newTestClient(doer, "https://api.datadoghq.com")
	_, _, err := c.QueryMetric(context.Background(), "avg:foo{*}", time.Minute)
	if !errors.Is(err, ErrTransient) {
		t.Fatalf("expected ErrTransient, got: %v", err)
	}
}

func TestQueryMetric_MalformedJSON_ErrTransient(t *testing.T) {
	doer := &stubDoer{resp: withBody(200, `{"status":"ok","series":[{`)}
	c := newTestClient(doer, "https://api.datadoghq.com")
	_, _, err := c.QueryMetric(context.Background(), "avg:foo{*}", time.Minute)
	if !errors.Is(err, ErrTransient) {
		t.Fatalf("expected ErrTransient on malformed JSON, got: %v", err)
	}
}

func TestQueryMetric_StatusErrorInBody_ErrTransient(t *testing.T) {
	// Datadog occasionally signals query-side errors via the response
	// body even on a 200. Map those to transient so the gate retries.
	body := `{"status":"error","error":"Internal error","series":[]}`
	doer := &stubDoer{resp: withBody(200, body)}
	c := newTestClient(doer, "https://api.datadoghq.com")
	_, _, err := c.QueryMetric(context.Background(), "avg:foo{*}", time.Minute)
	if !errors.Is(err, ErrTransient) {
		t.Fatalf("expected ErrTransient, got: %v", err)
	}
}

func TestQueryMetric_429_ErrTransient(t *testing.T) {
	// Rate-limit responses are non-auth 4xx and get bucketed as
	// transient — the gate retries on the next dog tick.
	doer := &stubDoer{resp: withBody(429, `{"errors":["rate limit"]}`)}
	c := newTestClient(doer, "https://api.datadoghq.com")
	_, _, err := c.QueryMetric(context.Background(), "avg:foo{*}", time.Minute)
	if !errors.Is(err, ErrTransient) {
		t.Fatalf("expected ErrTransient on 429, got: %v", err)
	}
}

func TestQueryMetric_NullPointSkipped(t *testing.T) {
	// Datadog emits JSON null for missing samples mid-window; we walk
	// back to the most recent non-null point and return that.
	// pointlist: [[1000, 0.5], [2000, null], [3000, null]]  → returns 0.5 @1000.
	body := `{
	  "status": "ok",
	  "series": [
	    {
	      "metric": "foo",
	      "pointlist": [[1000.0, 0.5], [2000.0, null], [3000.0, null]]
	    }
	  ]
	}`
	doer := &stubDoer{resp: withBody(200, body)}
	c := newTestClient(doer, "https://api.datadoghq.com")
	val, at, err := c.QueryMetric(context.Background(), "avg:foo{*}", time.Minute)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if val != 0.5 {
		t.Errorf("value: want 0.5, got %v", val)
	}
	wantAt := time.Unix(0, int64(1000*float64(time.Millisecond))).UTC()
	if !at.Equal(wantAt) {
		t.Errorf("at: want %v, got %v", wantAt, at)
	}
}

func TestQueryMetric_EmptyQueryRejected(t *testing.T) {
	c := newTestClient(&stubDoer{}, "https://api.datadoghq.com")
	_, _, err := c.QueryMetric(context.Background(), "", time.Minute)
	if err == nil {
		t.Fatal("expected an error for empty query, got nil")
	}
}

func TestQueryMetric_ZeroWindowRejected(t *testing.T) {
	c := newTestClient(&stubDoer{}, "https://api.datadoghq.com")
	_, _, err := c.QueryMetric(context.Background(), "avg:foo{*}", 0)
	if err == nil {
		t.Fatal("expected an error for zero window, got nil")
	}
}

// ── Health ──────────────────────────────────────────────────────────────────

func TestHealth_200_Nil(t *testing.T) {
	doer := &stubDoer{
		doFunc: func(req *http.Request) (*http.Response, error) {
			if got := req.URL.Path; got != "/api/v1/validate" {
				t.Errorf("Health path: want /api/v1/validate, got %q", got)
			}
			return withBody(200, `{"valid":true}`), nil
		},
	}
	c := newTestClient(doer, "https://api.datadoghq.com")
	if err := c.Health(context.Background()); err != nil {
		t.Fatalf("Health: unexpected error: %v", err)
	}
}

func TestHealth_401_ErrAuthFailure(t *testing.T) {
	doer := &stubDoer{resp: withBody(401, `{"errors":["Bad API key"]}`)}
	c := newTestClient(doer, "https://api.datadoghq.com")
	err := c.Health(context.Background())
	if !errors.Is(err, ErrAuthFailure) {
		t.Fatalf("expected ErrAuthFailure, got: %v", err)
	}
}

func TestHealth_403_ErrAuthFailure(t *testing.T) {
	doer := &stubDoer{resp: withBody(403, `{"errors":["Forbidden"]}`)}
	c := newTestClient(doer, "https://api.datadoghq.com")
	err := c.Health(context.Background())
	if !errors.Is(err, ErrAuthFailure) {
		t.Fatalf("expected ErrAuthFailure, got: %v", err)
	}
}

func TestHealth_5xx_ErrTransient(t *testing.T) {
	doer := &stubDoer{resp: withBody(502, "bad gateway")}
	c := newTestClient(doer, "https://api.datadoghq.com")
	err := c.Health(context.Background())
	if !errors.Is(err, ErrTransient) {
		t.Fatalf("expected ErrTransient, got: %v", err)
	}
}

func TestHealth_NetworkError_ErrTransient(t *testing.T) {
	doer := &stubDoer{err: errors.New("dial tcp: i/o timeout")}
	c := newTestClient(doer, "https://api.datadoghq.com")
	err := c.Health(context.Background())
	if !errors.Is(err, ErrTransient) {
		t.Fatalf("expected ErrTransient, got: %v", err)
	}
}

// ── NewInProcess: SystemConfig validation ──────────────────────────────────

func TestNewInProcess_MissingAPIKey_ErrConfig(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	// app_key is set, api_key is not.
	store.SetConfig(db, ConfigKeyAPPKey, "appkey-only")
	if _, err := NewInProcess(context.Background(), db); !errors.Is(err, ErrConfig) {
		t.Fatalf("expected ErrConfig when api_key missing, got: %v", err)
	}
}

func TestNewInProcess_MissingAppKey_ErrConfig(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	// api_key is set, app_key is not.
	store.SetConfig(db, ConfigKeyAPIKey, "apikey-only")
	if _, err := NewInProcess(context.Background(), db); !errors.Is(err, ErrConfig) {
		t.Fatalf("expected ErrConfig when app_key missing, got: %v", err)
	}
}

func TestNewInProcess_NilDB_ErrConfig(t *testing.T) {
	// A nil DB short-circuits to no api_key/app_key, which must
	// surface as ErrConfig — we never want a silent "Datadog client
	// constructed against zero-value creds" path.
	if _, err := NewInProcess(context.Background(), nil); !errors.Is(err, ErrConfig) {
		t.Fatalf("expected ErrConfig with nil db, got: %v", err)
	}
}

func TestNewInProcess_HappyPath(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	store.SetConfig(db, ConfigKeyAPIKey, "real-api")
	store.SetConfig(db, ConfigKeyAPPKey, "real-app")
	store.SetConfig(db, ConfigKeyBaseURL, "https://api.datadoghq.eu")

	c, err := NewInProcess(context.Background(), db)
	if err != nil {
		t.Fatalf("NewInProcess: %v", err)
	}
	if c == nil {
		t.Fatal("NewInProcess returned nil client with no error")
	}
}

func TestNewInProcess_DefaultBaseURL(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	store.SetConfig(db, ConfigKeyAPIKey, "real-api")
	store.SetConfig(db, ConfigKeyAPPKey, "real-app")
	// No explicit base URL → should default to DefaultBaseURL.

	c, err := NewInProcess(context.Background(), db)
	if err != nil {
		t.Fatalf("NewInProcess: %v", err)
	}
	// We can't introspect the unexported baseURL from outside the
	// package without a leak, so smoke-test by asserting the concrete
	// type's baseURL field is the default.
	ipc, ok := c.(*inProcessClient)
	if !ok {
		t.Fatalf("expected *inProcessClient, got %T", c)
	}
	if ipc.baseURL != DefaultBaseURL {
		t.Errorf("baseURL: want %q, got %q", DefaultBaseURL, ipc.baseURL)
	}
}

// ── End-to-end: real httptest.Server round-trip ─────────────────────────────

// TestQueryMetric_HTTPTestServer drives the full http.DefaultClient
// path through a real httptest.Server so the production code path
// (request build → headers → JSON parse) is exercised at least once
// without any network mocks. Catches regressions the doer-stub tests
// would miss.
func TestQueryMetric_HTTPTestServer(t *testing.T) {
	body := `{
	  "status":"ok",
	  "series":[
	    {"metric":"e2e","pointlist":[[1700000000000.0, 42.0]]}
	  ]
	}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/query" {
			http.Error(w, "wrong path", http.StatusNotFound)
			return
		}
		if r.Header.Get("DD-API-KEY") == "" {
			http.Error(w, "missing api key", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		_, _ = fmt.Fprint(w, body)
	}))
	defer srv.Close()

	// Build a Client backed by the real http.DefaultClient pointed at
	// the test server. We bypass NewInProcess to avoid needing the
	// SystemConfig dance, but still exercise the full HTTPS round-trip
	// shape.
	c := newInProcessFromHTTP(http.DefaultClient, srv.URL, "test-api-key", "test-app-key")

	val, _, err := c.QueryMetric(context.Background(), "avg:e2e{*}", time.Minute)
	if err != nil {
		t.Fatalf("QueryMetric: %v", err)
	}
	if val != 42.0 {
		t.Errorf("value: want 42.0, got %v", val)
	}
}

func TestHealth_HTTPTestServer_OK(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/validate" {
			http.Error(w, "wrong path", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		_, _ = fmt.Fprint(w, `{"valid":true}`)
	}))
	defer srv.Close()

	c := newInProcessFromHTTP(http.DefaultClient, srv.URL, "k", "a")
	if err := c.Health(context.Background()); err != nil {
		t.Fatalf("Health: %v", err)
	}
}

// ── Compile-time guards ─────────────────────────────────────────────────────

// TestInProcessSatisfiesClient guards Pattern P16: the unexported
// inProcessClient must implement Client. P16 also enforces this from
// the audittools side.
func TestInProcessSatisfiesClient(t *testing.T) {
	var _ Client = newInProcessFromHTTP(&stubDoer{}, "", "", "")
}

// Sanity: the test stub satisfies httpDoer.
var _ httpDoer = (*stubDoer)(nil)
