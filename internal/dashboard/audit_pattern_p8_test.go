package dashboard

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"

	"force-orchestrator/internal/store"
)

// TestPattern_P8_DashboardBindsAllInterfaces_ServesWildcardCORS verifies the
// exposure surface described in AUDIT Pattern P8:
//
//   - AUDIT-001: dashboard binds 0.0.0.0 with no auth and wildcard CORS
//   - AUDIT-002: stored XSS via unsanitized marked.parse() on mail bodies
//   - AUDIT-003: marked.min.js loaded from CDN with no SRI integrity pin
//   - AUDIT-053: SSE endpoints emit Access-Control-Allow-Origin: *
//   - AUDIT-054: POST handlers reuse wildcard-CORS helper with no size limit
//   - AUDIT-064: critical-escalation banner not surfaced (indirectly exposed
//     here because the same no-auth /api/status is world-readable)
//   - AUDIT-100: combined with 0.0.0.0 bind, world-readable worktrees chain
//
// This test asserts the SAFE posture. It is expected to FAIL against
// current HEAD — that is the point. Once the hardening PR lands, flipping
// the dashboard to 127.0.0.1, Origin-gated CORS, and a bundled + SRI'd
// marked (or DOMPurify wrap) will make this test pass.
func TestPattern_P8_DashboardBindsAllInterfaces_ServesWildcardCORS(t *testing.T) {
	_, thisFile, _, _ := runtime.Caller(0)
	pkgDir := filepath.Dir(thisFile)

	// ── Static check A: bind address in dashboard.go ─────────────────────
	// SAFE posture: bind 127.0.0.1 explicitly. CURRENT: fmt.Sprintf(":%d", port).
	dashBytes, err := os.ReadFile(filepath.Join(pkgDir, "dashboard.go"))
	if err != nil {
		t.Fatalf("read dashboard.go: %v", err)
	}
	dashSrc := string(dashBytes)

	// Match all-interface bind expressions of the form `":%d"` or `":PORT"`
	// (i.e. a colon followed directly by a port with no host).
	allIfaceBind := regexp.MustCompile(`fmt\.Sprintf\(\s*"\s*:%d"`)
	if allIfaceBind.MatchString(dashSrc) {
		t.Errorf("AUDIT-001: dashboard.go binds all interfaces (matched `fmt.Sprintf(\":%%d\"...)`); expected 127.0.0.1 bind")
	}
	if !strings.Contains(dashSrc, "127.0.0.1") {
		t.Errorf("AUDIT-001: dashboard.go does not contain `127.0.0.1` — bind is not loopback-gated")
	}

	// ── Static check B: jsonCORS writes wildcard Access-Control-Allow-Origin ─
	handlersBytes, err := os.ReadFile(filepath.Join(pkgDir, "handlers.go"))
	if err != nil {
		t.Fatalf("read handlers.go: %v", err)
	}
	handlersSrc := string(handlersBytes)

	// Locate the jsonCORS function body.
	jsonCORSRe := regexp.MustCompile(`(?s)func\s+jsonCORS\s*\([^)]*\)\s*\{[^}]*\}`)
	match := jsonCORSRe.FindString(handlersSrc)
	if match == "" {
		t.Fatalf("AUDIT-053/054: could not locate jsonCORS in handlers.go")
	}
	if strings.Contains(match, `"Access-Control-Allow-Origin", "*"`) {
		t.Errorf("AUDIT-053/054: jsonCORS sets wildcard Access-Control-Allow-Origin — any origin can read/drive the API.\nFunction body:\n%s", match)
	}

	// ── Static check C: index.html loads marked.min.js from CDN with no SRI ─
	indexBytes, err := os.ReadFile(filepath.Join(pkgDir, "static", "index.html"))
	if err != nil {
		t.Fatalf("read static/index.html: %v", err)
	}
	indexSrc := string(indexBytes)

	// Find the <script ...> tag that loads marked(.min).js, if any.
	markedTagRe := regexp.MustCompile(`(?i)<script[^>]*src=["'][^"']*marked(?:\.min)?\.js[^"']*["'][^>]*>`)
	tag := markedTagRe.FindString(indexSrc)
	if tag == "" {
		t.Fatalf("AUDIT-003: could not locate marked.js script tag in index.html")
	}
	if strings.Contains(tag, "cdn.jsdelivr.net") {
		t.Errorf("AUDIT-003: marked.min.js loaded from cdn.jsdelivr.net (unbundled supply-chain dependency).\nTag: %s", tag)
	}
	if !strings.Contains(tag, "integrity=") {
		t.Errorf("AUDIT-003: marked.js script tag has no `integrity=` SRI attribute — CDN compromise = same-origin RCE.\nTag: %s", tag)
	}

	// ── Static check D: app.js uses marked.parse without DOMPurify wrap ──
	appBytes, err := os.ReadFile(filepath.Join(pkgDir, "static", "app.js"))
	if err != nil {
		t.Fatalf("read static/app.js: %v", err)
	}
	appSrc := string(appBytes)

	if !strings.Contains(appSrc, "marked.parse(") {
		t.Fatalf("AUDIT-002: could not find `marked.parse(` in app.js; test assumption broken")
	}
	// Look for DOMPurify.sanitize wrapping anywhere in the file.
	if !strings.Contains(appSrc, "DOMPurify.sanitize(") {
		t.Errorf("AUDIT-002: app.js calls marked.parse() with no DOMPurify.sanitize() wrap anywhere in the file — stored XSS on mail bodies")
	} else {
		// If present, require that the specific marked.parse at the mail
		// modal site (around line 1349) is itself wrapped. Grab ~200 chars
		// of context around the first marked.parse( occurrence.
		idx := strings.Index(appSrc, "marked.parse(")
		start := idx - 120
		if start < 0 {
			start = 0
		}
		end := idx + 120
		if end > len(appSrc) {
			end = len(appSrc)
		}
		window := appSrc[start:end]
		if !strings.Contains(window, "DOMPurify.sanitize(") {
			t.Errorf("AUDIT-002: marked.parse() near mail-modal render is not DOMPurify-wrapped.\nWindow:\n%s", window)
		}
	}

	// ── Dynamic check: real handler writes wildcard CORS header ──────────
	// Exercise handleStatus against an in-memory DB and inspect response headers.
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	req := httptest.NewRequest(http.MethodGet, "/api/status", nil)
	w := httptest.NewRecorder()
	handleStatus(db)(w, req)

	if got := w.Header().Get("Access-Control-Allow-Origin"); got == "*" {
		t.Errorf("AUDIT-001/053: handleStatus response sets Access-Control-Allow-Origin=* (got %q) — no origin gating", got)
	}
}
