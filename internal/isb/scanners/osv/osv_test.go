// Package osv unit tests.
//
// These tests stub the HTTP transport rather than the osv-scanner
// library: that way we exercise the actual lockfile-parser → query →
// hydrate pipeline (catching wrapper bugs) without ever touching
// api.osv.dev. Slice δ in Wave 2 will add an integration-style test
// against a tiny golden lockfile + recorded fixture set.
//
// Test strategy:
//
//   - TestOSV_ScanLockfile_NoVulns: a synthetic gemfile with one safe
//     dep; the stub OSV transport returns "no vulns".
//   - TestOSV_ScanLockfile_HighSeverityVuln: same gemfile, OSV returns
//     a hydrated vulnerability with CVSS 9.8.
//   - TestOSV_UnsupportedEcosystem: a basename osv-scanner doesn't
//     parse → wrapped ErrUnsupportedLockfile.
//   - TestOSV_MalformedLockfile: garbage bytes in a recognised
//     basename → non-nil error from Parse, no findings.
//   - TestOSV_EmptyContent: zero-length content → empty findings, no
//     error.
//   - TestOSV_SeverityBuckets: extractSeverity table-driven sanity.
//   - TestOSV_OSVErrorPropagated: network error from stub transport →
//     wrapped error.
package osv

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/google/osv-scanner/pkg/models"
)

// stubTransport is a tiny http.RoundTripper that routes requests to a
// per-URL handler. Each handler returns the bytes the test wants OSV
// to "respond" with; missing URLs error out so a forgotten stub
// surfaces immediately.
type stubTransport struct {
	handlers map[string]func() (int, []byte, error)
	calls    int
}

func newStubTransport() *stubTransport {
	return &stubTransport{handlers: map[string]func() (int, []byte, error){}}
}

func (s *stubTransport) on(urlPrefix string, fn func() (int, []byte, error)) {
	s.handlers[urlPrefix] = fn
}

func (s *stubTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	s.calls++
	for prefix, fn := range s.handlers {
		if strings.HasPrefix(req.URL.String(), prefix) {
			status, body, err := fn()
			if err != nil {
				return nil, err
			}
			return &http.Response{
				StatusCode: status,
				Body:       io.NopCloser(strings.NewReader(string(body))),
				Header:     http.Header{},
				Request:    req,
			}, nil
		}
	}
	return nil, errors.New("stubTransport: no handler for " + req.URL.String())
}

// stubBatchedNoVulns is the OSV batched-query response for "every dep
// is safe." It has one entry per query with an empty Vulns slice.
func stubBatchedNoVulns(numQueries int) []byte {
	type minimal struct {
		ID string `json:"id"`
	}
	type result struct {
		Vulns []minimal `json:"vulns"`
	}
	type resp struct {
		Results []result `json:"results"`
	}
	r := resp{Results: make([]result, numQueries)}
	for i := range r.Results {
		r.Results[i].Vulns = []minimal{}
	}
	b, _ := json.Marshal(r)
	return b
}

// stubBatchedOneVuln is the OSV batched-query response for "first dep
// has one CVE." numQueries is the slice length to return; vulnIdx is
// which slot gets the vulnerability.
func stubBatchedOneVuln(numQueries int, vulnIdx int, vulnID string) []byte {
	type minimal struct {
		ID string `json:"id"`
	}
	type result struct {
		Vulns []minimal `json:"vulns"`
	}
	type resp struct {
		Results []result `json:"results"`
	}
	r := resp{Results: make([]result, numQueries)}
	for i := range r.Results {
		r.Results[i].Vulns = []minimal{}
	}
	r.Results[vulnIdx].Vulns = []minimal{{ID: vulnID}}
	b, _ := json.Marshal(r)
	return b
}

// stubVulnDetail is the response for GET /vulns/<ID> — a hydrated
// Vulnerability record. Severity score-string format mirrors what
// real OSV ships for CVSS_V3 entries.
func stubVulnDetail(id, summary string, severityScore string) []byte {
	v := models.Vulnerability{
		ID:      id,
		Summary: summary,
		Severity: []models.Severity{
			{Type: models.SeverityCVSSV3, Score: severityScore},
		},
	}
	b, _ := json.Marshal(v)
	return b
}

// gemfileLockOneDep is a minimally-valid Gemfile.lock with one direct
// dep. osv-scanner's gemfile-lock parser accepts this shape.
const gemfileLockOneDep = `GEM
  remote: https://rubygems.org/
  specs:
    redis (5.0.0)

PLATFORMS
  ruby

DEPENDENCIES
  redis

BUNDLED WITH
   2.4.0
`

// TestOSV_ScanLockfile_NoVulns — happy path: a parseable lockfile, OSV
// returns no vulns → empty findings, no error.
func TestOSV_ScanLockfile_NoVulns(t *testing.T) {
	stub := newStubTransport()
	stub.on("https://api.osv.dev/v1/querybatch", func() (int, []byte, error) {
		return 200, stubBatchedNoVulns(1), nil
	})

	client := NewInProcess(WithHTTPClient(&http.Client{Transport: stub}))

	findings, err := client.ScanLockfile(context.Background(), "Gemfile.lock", []byte(gemfileLockOneDep))
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if len(findings) != 0 {
		t.Fatalf("expected 0 findings, got %d: %+v", len(findings), findings)
	}
	if stub.calls == 0 {
		t.Errorf("expected at least one stub HTTP call, got 0")
	}
}

// TestOSV_ScanLockfile_HighSeverityVuln — a parseable lockfile, OSV
// returns one CVSS-9.8 vuln on the first dep → one Finding with
// severity=HIGH (well, CRITICAL given the score), populated fields.
func TestOSV_ScanLockfile_HighSeverityVuln(t *testing.T) {
	stub := newStubTransport()
	stub.on("https://api.osv.dev/v1/querybatch", func() (int, []byte, error) {
		return 200, stubBatchedOneVuln(1, 0, "CVE-2023-12345"), nil
	})
	stub.on("https://api.osv.dev/v1/vulns/CVE-2023-12345", func() (int, []byte, error) {
		return 200, stubVulnDetail("CVE-2023-12345", "redis arbitrary code execution", "9.8"), nil
	})

	client := NewInProcess(WithHTTPClient(&http.Client{Transport: stub}))

	findings, err := client.ScanLockfile(context.Background(), "Gemfile.lock", []byte(gemfileLockOneDep))
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if len(findings) != 1 {
		t.Fatalf("expected 1 finding, got %d: %+v", len(findings), findings)
	}
	f := findings[0]
	if f.PackageName != "redis" {
		t.Errorf("PackageName: got %q want redis", f.PackageName)
	}
	if f.PackageVersion != "5.0.0" {
		t.Errorf("PackageVersion: got %q want 5.0.0", f.PackageVersion)
	}
	if f.OSVID != "CVE-2023-12345" {
		t.Errorf("OSVID: got %q", f.OSVID)
	}
	if f.Severity != "CRITICAL" {
		t.Errorf("Severity: got %q want CRITICAL", f.Severity)
	}
	if !strings.Contains(f.Summary, "arbitrary code execution") {
		t.Errorf("Summary: %q", f.Summary)
	}
	if !strings.Contains(f.URL, "CVE-2023-12345") {
		t.Errorf("URL: %q", f.URL)
	}
	if f.Ecosystem == "" {
		t.Errorf("Ecosystem empty (lockfile parser should set it)")
	}
}

// TestOSV_UnsupportedEcosystem — a basename osv-scanner doesn't claim
// (e.g. plain Gemfile, not Gemfile.lock; pyproject.toml). We use
// "Gemfile" since osv-scanner v1's parser-table only contains
// Gemfile.lock, not Gemfile.
func TestOSV_UnsupportedEcosystem(t *testing.T) {
	stub := newStubTransport()
	// No handler — if scan reaches HTTP, it'll fail loud.

	client := NewInProcess(WithHTTPClient(&http.Client{Transport: stub}))

	_, err := client.ScanLockfile(context.Background(), "Gemfile", []byte("source 'https://rubygems.org'\ngem 'redis'\n"))
	if err == nil {
		t.Fatal("expected error on unsupported lockfile, got nil")
	}
	if !errors.Is(err, ErrUnsupportedLockfile) {
		t.Errorf("expected ErrUnsupportedLockfile sentinel, got %v", err)
	}
	if stub.calls != 0 {
		t.Errorf("expected 0 HTTP calls on unsupported lockfile, got %d", stub.calls)
	}
}

// TestOSV_MalformedLockfile — garbage in a recognised basename. Some
// extractors return errors with empty Packages → wrapper returns
// (nil, err). Others tolerate noise and return Packages anyway.
// Either way the wrapper must NOT panic.
func TestOSV_MalformedLockfile(t *testing.T) {
	stub := newStubTransport()
	// Provide a permissive batched-query handler so partial-parse
	// paths that DO reach the network don't fail with "no handler".
	stub.on("https://api.osv.dev/v1/querybatch", func() (int, []byte, error) {
		return 200, stubBatchedNoVulns(0), nil
	})

	client := NewInProcess(WithHTTPClient(&http.Client{Transport: stub}))

	// `package-lock.json` parser expects JSON; non-JSON is hard fail.
	_, err := client.ScanLockfile(context.Background(), "package-lock.json", []byte("this is not json"))
	if err == nil {
		t.Fatal("expected error on malformed package-lock.json, got nil")
	}
	if !strings.Contains(err.Error(), "package-lock.json") {
		t.Errorf("expected error to reference filename, got: %v", err)
	}
}

// TestOSV_EmptyContent — zero-length content returns (nil, nil) without
// touching the parser or the network. A SUPPLY-005 caller passing
// `cm.After == nil` (lock file deleted in this commit) goes here.
func TestOSV_EmptyContent(t *testing.T) {
	stub := newStubTransport()
	client := NewInProcess(WithHTTPClient(&http.Client{Transport: stub}))

	findings, err := client.ScanLockfile(context.Background(), "Gemfile.lock", nil)
	if err != nil {
		t.Fatalf("unexpected err on empty content: %v", err)
	}
	if len(findings) != 0 {
		t.Errorf("expected 0 findings on empty content, got %d", len(findings))
	}
	if stub.calls != 0 {
		t.Errorf("expected 0 HTTP calls on empty content, got %d", stub.calls)
	}
}

// TestOSV_OSVErrorPropagated — stub transport returns a 500 on the
// batched endpoint → wrapper returns a wrapped error.
func TestOSV_OSVErrorPropagated(t *testing.T) {
	stub := newStubTransport()
	stub.on("https://api.osv.dev/v1/querybatch", func() (int, []byte, error) {
		return 500, []byte(`internal server error`), nil
	})

	client := NewInProcess(WithHTTPClient(&http.Client{Transport: stub}))

	_, err := client.ScanLockfile(context.Background(), "Gemfile.lock", []byte(gemfileLockOneDep))
	if err == nil {
		t.Fatal("expected wrapped error on OSV 500, got nil")
	}
	if !strings.Contains(err.Error(), "osv:") {
		t.Errorf("expected error prefixed with 'osv:', got: %v", err)
	}
}

// TestOSV_EmptyPath — empty path parameter is a programmer error;
// reject early.
func TestOSV_EmptyPath(t *testing.T) {
	client := NewInProcess()
	_, err := client.ScanLockfile(context.Background(), "", []byte("anything"))
	if err == nil {
		t.Fatal("expected error on empty path")
	}
}

// TestOSV_SeverityBuckets — extractSeverity on synthetic vulns,
// asserting the FIRST.org-mapped buckets are stable.
func TestOSV_SeverityBuckets(t *testing.T) {
	cases := []struct {
		name  string
		score string
		want  string
	}{
		{"critical_9.8", "9.8", "CRITICAL"},
		{"critical_9.0_edge", "9.0", "CRITICAL"},
		{"high_8.9", "8.9", "HIGH"},
		{"high_7.0_edge", "7.0", "HIGH"},
		{"medium_6.9", "6.9", "MEDIUM"},
		{"medium_4.0_edge", "4.0", "MEDIUM"},
		{"low_3.9", "3.9", "LOW"},
		{"low_0.1_edge", "0.1", "LOW"},
		{"unknown_zero", "0", "UNKNOWN"},
		{"unknown_empty", "", "UNKNOWN"},
		{"plain_critical_token", "CRITICAL", "CRITICAL"},
		{"plain_moderate_token", "MODERATE", "MEDIUM"},
		// CVSS-vector form without an embedded numeric — we can't
		// reliably score, treat as unknown.
		{"cvss_vector_only", "CVSS:3.1/AV:N/AC:L/PR:N/UI:N", "UNKNOWN"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := models.Vulnerability{
				ID:       "CVE-X-" + tc.name,
				Severity: []models.Severity{{Type: models.SeverityCVSSV3, Score: tc.score}},
			}
			got := extractSeverity(v)
			if got != tc.want {
				t.Errorf("extractSeverity(%q) = %q, want %q", tc.score, got, tc.want)
			}
		})
	}
}

// TestOSV_SeverityHighestWins — when a vulnerability has multiple
// severity records, we pick the highest bucket.
func TestOSV_SeverityHighestWins(t *testing.T) {
	v := models.Vulnerability{
		ID: "CVE-multi",
		Severity: []models.Severity{
			{Type: models.SeverityCVSSV3, Score: "5.0"},  // MEDIUM
			{Type: models.SeverityCVSSV3, Score: "9.5"},  // CRITICAL
			{Type: models.SeverityCVSSV3, Score: "7.5"},  // HIGH
		},
	}
	got := extractSeverity(v)
	if got != "CRITICAL" {
		t.Errorf("expected CRITICAL (highest of 5.0/9.5/7.5), got %q", got)
	}
}
