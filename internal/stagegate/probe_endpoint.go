package stagegate

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"time"
)

// ProbeEndpoint is the D5.5 P3 advanced leaf gate that confirms a deployed
// change is "actually working" by probing an HTTP endpoint after the soak
// window. It's the natural successor to soak_minutes for stages that need
// stronger evidence than "wait and pray": the gate calls the configured URL,
// asserts the response status (and optionally a regex match against the
// body), and only flips GatePassed when the probe succeeds.
//
// Config shape (the JSON object stored in ConvoyStages.gate_config_json):
//
//	{
//	  "url":                  "https://api.example.com/health",
//	  "method":               "GET",                        // optional, default GET
//	  "expected_status":      200,                          // optional, default 200
//	  "body_match_regex":     "\"status\":\\s*\"ok\"",      // optional, no match requirement if empty
//	  "timeout_seconds":      30,                           // optional, default 30s
//	  "target_env":           "prod",                       // informational; the URL itself encodes the env
//	  "headers":              {"Authorization": "Bearer x"} // optional
//	}
//
// Evaluation contract:
//   - Network errors (DNS, connection refused, timeout) → ErrPending so the
//     dog re-checks next tick. A flapping endpoint shouldn't permanently
//     fail the convoy; the operator-confirm escape hatch handles the case
//     where the endpoint is genuinely broken.
//   - Status mismatch / body regex miss → passed=false with err=nil. The
//     stage moves to Failed; this is a "the change deployed but is broken"
//     signal that demands operator attention.
//   - Bad config (empty URL, invalid regex) → structural error.
type ProbeEndpoint struct {
	// httpClient is the HTTP client used for probes. Construction via
	// NewProbeEndpoint installs a default *http.Client; tests inject a
	// client pointed at httptest.NewServer.
	httpClient *http.Client
}

// NewProbeEndpoint returns a ProbeEndpoint with a default HTTP client.
// Production callers register this via Registry.Register.
func NewProbeEndpoint() ProbeEndpoint {
	return ProbeEndpoint{httpClient: &http.Client{}}
}

// NewProbeEndpointWithClient is the test-helper constructor that lets a
// test inject a pre-configured *http.Client (for example the one returned
// by httptest.NewServer's Client() method).
func NewProbeEndpointWithClient(c *http.Client) ProbeEndpoint {
	return ProbeEndpoint{httpClient: c}
}

// Type implements Gate.
func (ProbeEndpoint) Type() string { return "probe_endpoint" }

// Evaluate implements Gate.
func (p ProbeEndpoint) Evaluate(ctx context.Context, _ *sql.DB, stage StageContext) (bool, string, error) {
	var cfg struct {
		URL            string            `json:"url"`
		Method         string            `json:"method"`
		ExpectedStatus int               `json:"expected_status"`
		BodyMatchRegex string            `json:"body_match_regex"`
		TimeoutSeconds int               `json:"timeout_seconds"`
		TargetEnv      string            `json:"target_env"`
		Headers        map[string]string `json:"headers"`
	}
	if err := json.Unmarshal(stage.GateConfig, &cfg); err != nil {
		return false, "", fmt.Errorf("probe_endpoint: parse config: %w", err)
	}
	if cfg.URL == "" {
		return false, "", fmt.Errorf("probe_endpoint: url required")
	}
	method := cfg.Method
	if method == "" {
		method = "GET"
	}
	if cfg.ExpectedStatus == 0 {
		cfg.ExpectedStatus = 200
	}
	timeout := time.Duration(cfg.TimeoutSeconds) * time.Second
	if timeout == 0 {
		timeout = 30 * time.Second
	}

	// Pre-compile the body-match regex BEFORE issuing the request so a
	// bad regex returns a structural error without burning network round-
	// trips. If the regex is empty we skip body matching entirely.
	var bodyRe *regexp.Regexp
	if cfg.BodyMatchRegex != "" {
		re, err := regexp.Compile(cfg.BodyMatchRegex)
		if err != nil {
			return false, "", fmt.Errorf("probe_endpoint: compile regex: %w", err)
		}
		bodyRe = re
	}

	reqCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, method, cfg.URL, nil)
	if err != nil {
		return false, "", fmt.Errorf("probe_endpoint: build request: %w", err)
	}
	for k, v := range cfg.Headers {
		req.Header.Set(k, v)
	}

	client := p.httpClient
	if client == nil {
		client = &http.Client{Timeout: timeout}
	}
	resp, err := client.Do(req)
	if err != nil {
		// Network errors (DNS, refused, timeout) → ErrPending. The dog
		// retries next tick; a transient blip shouldn't fail the stage.
		return false, fmt.Sprintf("probe_endpoint: network error: %v", err), ErrPending
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != cfg.ExpectedStatus {
		return false, fmt.Sprintf("probe_endpoint: expected status %d, got %d", cfg.ExpectedStatus, resp.StatusCode), nil
	}
	if bodyRe != nil && !bodyRe.Match(body) {
		return false, fmt.Sprintf("probe_endpoint: body did not match regex %q", cfg.BodyMatchRegex), nil
	}
	envSuffix := ""
	if cfg.TargetEnv != "" {
		envSuffix = fmt.Sprintf(" (env=%s)", cfg.TargetEnv)
	}
	return true, fmt.Sprintf("probe_endpoint: %s %s → %d%s", method, cfg.URL, resp.StatusCode, envSuffix), nil
}
