package stagegate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestProbeEndpoint_Type(t *testing.T) {
	if (ProbeEndpoint{}).Type() != "probe_endpoint" {
		t.Errorf("Type() = %q, want probe_endpoint", ProbeEndpoint{}.Type())
	}
}

// TestProbeEndpoint_HappyPath: 200 OK from the configured URL → passed.
func TestProbeEndpoint_HappyPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}))
	defer srv.Close()

	cfg, _ := json.Marshal(map[string]any{
		"url":             srv.URL,
		"expected_status": 200,
	})
	g := NewProbeEndpoint()
	passed, reason, err := g.Evaluate(context.Background(), nil, StageContext{GateConfig: cfg})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !passed {
		t.Errorf("expected passed=true, reason=%q", reason)
	}
	if !strings.Contains(reason, "200") {
		t.Errorf("expected reason to mention 200, got %q", reason)
	}
}

// TestProbeEndpoint_StatusMismatch_FailsCleanly: server returns 500, expected
// 200 → passed=false with err=nil (concrete fail, NOT pending).
func TestProbeEndpoint_StatusMismatch_FailsCleanly(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	cfg, _ := json.Marshal(map[string]any{
		"url":             srv.URL,
		"expected_status": 200,
	})
	g := NewProbeEndpoint()
	passed, reason, err := g.Evaluate(context.Background(), nil, StageContext{GateConfig: cfg})
	if err != nil {
		t.Fatalf("expected nil err on status mismatch (concrete fail), got %v", err)
	}
	if passed {
		t.Error("expected passed=false on status mismatch")
	}
	if !strings.Contains(reason, "expected status 200") || !strings.Contains(reason, "got 500") {
		t.Errorf("reason should mention expected/got, got %q", reason)
	}
}

// TestProbeEndpoint_BodyMatch_HappyPath: body regex matches → passed.
func TestProbeEndpoint_BodyMatch_HappyPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"status":"ok","build":"abc123"}`))
	}))
	defer srv.Close()

	cfg, _ := json.Marshal(map[string]any{
		"url":              srv.URL,
		"body_match_regex": `"status":\s*"ok"`,
	})
	g := NewProbeEndpoint()
	passed, _, err := g.Evaluate(context.Background(), nil, StageContext{GateConfig: cfg})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !passed {
		t.Error("expected passed=true with matching body regex")
	}
}

// TestProbeEndpoint_BodyNotMatching_FailsCleanly: body regex misses → fail.
func TestProbeEndpoint_BodyNotMatching_FailsCleanly(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"status":"degraded"}`))
	}))
	defer srv.Close()

	cfg, _ := json.Marshal(map[string]any{
		"url":              srv.URL,
		"body_match_regex": `"status":\s*"ok"`,
	})
	g := NewProbeEndpoint()
	passed, reason, err := g.Evaluate(context.Background(), nil, StageContext{GateConfig: cfg})
	if err != nil {
		t.Fatalf("expected nil err on regex miss, got %v", err)
	}
	if passed {
		t.Error("expected passed=false on regex miss")
	}
	if !strings.Contains(reason, "did not match") {
		t.Errorf("expected reason to mention 'did not match', got %q", reason)
	}
}

// TestProbeEndpoint_NetworkError_Pending: connecting to a port that refuses
// connections → ErrPending, NOT a concrete fail. The convoy-stage-watch dog
// re-checks next tick; a transient outage shouldn't fail the convoy.
func TestProbeEndpoint_NetworkError_Pending(t *testing.T) {
	cfg, _ := json.Marshal(map[string]any{
		// Reserved by RFC 5737 for documentation; routes to nowhere.
		// Combined with the short timeout below, the request fails fast.
		"url":             "http://203.0.113.1:1/probe",
		"timeout_seconds": 1,
	})
	g := NewProbeEndpoint()
	passed, reason, err := g.Evaluate(context.Background(), nil, StageContext{GateConfig: cfg})
	if !errors.Is(err, ErrPending) {
		t.Fatalf("expected ErrPending on network error, got %v", err)
	}
	if passed {
		t.Error("expected passed=false on network error")
	}
	if !strings.Contains(reason, "network error") {
		t.Errorf("expected reason to mention network error, got %q", reason)
	}
}

// TestProbeEndpoint_TimeoutExceeded_Pending: server hangs longer than the
// configured timeout → request returns a deadline error → ErrPending.
func TestProbeEndpoint_TimeoutExceeded_Pending(t *testing.T) {
	// Server sleeps 500ms before responding; we give it a 100ms timeout.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
			return
		case <-time.After(500 * time.Millisecond):
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer srv.Close()

	cfg, _ := json.Marshal(map[string]any{
		"url":             srv.URL,
		"timeout_seconds": 1, // must be int per spec; we override the http.Client
	})
	// Use a client whose own timeout enforces the 100ms limit (the JSON
	// `timeout_seconds: 1` resolves to a 1-sec context cap; the client
	// timeout below trips first).
	g := NewProbeEndpointWithClient(&http.Client{Timeout: 100 * time.Millisecond})
	passed, reason, err := g.Evaluate(context.Background(), nil, StageContext{GateConfig: cfg})
	if !errors.Is(err, ErrPending) {
		t.Fatalf("expected ErrPending on timeout, got %v (reason=%q)", err, reason)
	}
	if passed {
		t.Error("expected passed=false on timeout")
	}
}

// TestProbeEndpoint_UnconfiguredURL_Errors: empty url is a structural config
// error, not a runtime pending — the planner should have caught this.
func TestProbeEndpoint_UnconfiguredURL_Errors(t *testing.T) {
	cfg := json.RawMessage(`{}`)
	g := NewProbeEndpoint()
	_, _, err := g.Evaluate(context.Background(), nil, StageContext{GateConfig: cfg})
	if err == nil {
		t.Fatal("expected error on empty url")
	}
	if errors.Is(err, ErrPending) {
		t.Errorf("empty url should be a structural error, not ErrPending; got %v", err)
	}
	if !strings.Contains(err.Error(), "url required") {
		t.Errorf("expected error to mention 'url required', got %v", err)
	}
}

// TestProbeEndpoint_InvalidRegex_Errors: an unparseable regex is a structural
// error surfaced before any HTTP call.
func TestProbeEndpoint_InvalidRegex_Errors(t *testing.T) {
	cfg, _ := json.Marshal(map[string]any{
		"url":              "http://example.invalid/",
		"body_match_regex": `[unterminated`,
	})
	g := NewProbeEndpoint()
	_, _, err := g.Evaluate(context.Background(), nil, StageContext{GateConfig: cfg})
	if err == nil {
		t.Fatal("expected error on invalid regex")
	}
	if errors.Is(err, ErrPending) {
		t.Errorf("invalid regex should be structural, not ErrPending; got %v", err)
	}
	if !strings.Contains(err.Error(), "compile regex") {
		t.Errorf("expected error to mention 'compile regex', got %v", err)
	}
}

// TestProbeEndpoint_Headers_PassedThrough: the Headers map populates the
// outbound request's headers verbatim.
func TestProbeEndpoint_Headers_PassedThrough(t *testing.T) {
	var seenAuth, seenTrace string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenAuth = r.Header.Get("Authorization")
		seenTrace = r.Header.Get("X-Trace-Id")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	cfg, _ := json.Marshal(map[string]any{
		"url": srv.URL,
		"headers": map[string]string{
			"Authorization": "Bearer secret",
			"X-Trace-Id":    "probe-1",
		},
	})
	g := NewProbeEndpoint()
	if _, _, err := g.Evaluate(context.Background(), nil, StageContext{GateConfig: cfg}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if seenAuth != "Bearer secret" {
		t.Errorf("Authorization header not passed: got %q", seenAuth)
	}
	if seenTrace != "probe-1" {
		t.Errorf("X-Trace-Id header not passed: got %q", seenTrace)
	}
}

// TestProbeEndpoint_DefaultMethodAndStatus: method/expected_status default
// to GET/200 when the JSON config omits them.
func TestProbeEndpoint_DefaultMethodAndStatus(t *testing.T) {
	var seenMethod string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenMethod = r.Method
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	cfg, _ := json.Marshal(map[string]any{"url": srv.URL})
	g := NewProbeEndpoint()
	passed, _, err := g.Evaluate(context.Background(), nil, StageContext{GateConfig: cfg})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !passed {
		t.Error("expected passed=true with all defaults")
	}
	if seenMethod != "GET" {
		t.Errorf("expected default method GET, got %q", seenMethod)
	}
}

// guard against unused imports / types if a future test removes them.
var _ = fmt.Sprintf
