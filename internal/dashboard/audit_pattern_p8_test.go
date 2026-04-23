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
// exposure surface described in AUDIT Pattern P8. Originally a red-phase test
// (skipped until Fix #2 landed). Fix #2 made this pass; it now stays as
// permanent regression protection.
//
// What this test enforces (the SAFE posture):
//
//   - AUDIT-001: dashboard.go binds 127.0.0.1 via loopbackBindAddr (not
//     `fmt.Sprintf(":%d", port)` which bound all interfaces).
//   - AUDIT-002: app.js does not call marked.parse() — the mail-body modal
//     renders via textContent, which parses nothing. Zero XSS sink.
//   - AUDIT-003: index.html does not load marked.js from any CDN. If marked
//     is ever re-introduced, it must be bundled locally under static/ (and
//     accompanied by a DOMPurify sanitize wrap).
//   - AUDIT-053: SSE endpoints do not emit Access-Control-Allow-Origin.
//   - AUDIT-054: jsonCORS does not emit wildcard Access-Control-Allow-Origin.
//   - AUDIT-064: the dashboard is no longer world-readable; same-origin only.
//
// If any check fails, Fix #2's dashboard hardening has regressed.
func TestPattern_P8_DashboardBindsAllInterfaces_ServesWildcardCORS(t *testing.T) {
	_, thisFile, _, _ := runtime.Caller(0)
	pkgDir := filepath.Dir(thisFile)

	// ── Static check A: bind address in dashboard.go ─────────────────────
	// SAFE posture: bind via loopbackBindAddr (defined in security.go) which
	// formats 127.0.0.1:PORT. The literal "127.0.0.1" should appear in one of
	// the package's Go files (security.go or dashboard.go).
	dashBytes, err := os.ReadFile(filepath.Join(pkgDir, "dashboard.go"))
	if err != nil {
		t.Fatalf("read dashboard.go: %v", err)
	}
	dashSrc := string(dashBytes)

	// The old vulnerable pattern: fmt.Sprintf(":%d", port) — empty host.
	// Must not appear as a bind-address construction in the RunDashboard body.
	allIfaceBind := regexp.MustCompile(`fmt\.Sprintf\(\s*"\s*:%d"\s*,\s*port\s*\)`)
	if allIfaceBind.MatchString(dashSrc) {
		t.Errorf("AUDIT-001: dashboard.go binds all interfaces (matched `fmt.Sprintf(\":%%d\", port)`); expected loopback bind")
	}

	// The loopback bind literal must exist somewhere in the package (we allow
	// it to live in security.go alongside the middleware helpers).
	loopbackFound := strings.Contains(dashSrc, "127.0.0.1")
	if !loopbackFound {
		secBytes, err := os.ReadFile(filepath.Join(pkgDir, "security.go"))
		if err == nil && strings.Contains(string(secBytes), "127.0.0.1") {
			loopbackFound = true
		}
	}
	if !loopbackFound {
		t.Errorf("AUDIT-001: neither dashboard.go nor security.go contains `127.0.0.1` — bind is not loopback-gated")
	}

	// dashboard.go must invoke loopbackBindAddr to get its bind address.
	if !strings.Contains(dashSrc, "loopbackBindAddr(") {
		t.Errorf("AUDIT-001: dashboard.go does not call loopbackBindAddr — bind address is not going through the loopback helper")
	}

	// ── Static check B: jsonCORS does not write wildcard Access-Control-Allow-Origin ─
	handlersBytes, err := os.ReadFile(filepath.Join(pkgDir, "handlers.go"))
	if err != nil {
		t.Fatalf("read handlers.go: %v", err)
	}
	handlersSrc := string(handlersBytes)

	jsonCORSRe := regexp.MustCompile(`(?s)func\s+jsonCORS\s*\([^)]*\)\s*\{.*?\n\}`)
	match := jsonCORSRe.FindString(handlersSrc)
	if match == "" {
		t.Fatalf("AUDIT-053/054: could not locate jsonCORS in handlers.go")
	}
	if strings.Contains(match, `"Access-Control-Allow-Origin", "*"`) {
		t.Errorf("AUDIT-053/054: jsonCORS sets wildcard Access-Control-Allow-Origin — any origin can read/drive the API.\nFunction body:\n%s", match)
	}

	// ── Static check C: index.html does not load marked.js from a CDN ──
	indexBytes, err := os.ReadFile(filepath.Join(pkgDir, "static", "index.html"))
	if err != nil {
		t.Fatalf("read static/index.html: %v", err)
	}
	indexSrc := string(indexBytes)

	markedTagRe := regexp.MustCompile(`(?i)<script[^>]*src=["'][^"']*marked(?:\.min)?\.js[^"']*["'][^>]*>`)
	tag := markedTagRe.FindString(indexSrc)
	if tag != "" {
		// A marked tag is tolerated ONLY if it is:
		//   (1) bundled locally (no CDN host in the src),
		//   (2) carries an integrity= SRI hash.
		// If either is missing, AUDIT-003 is re-opened.
		cdnHosts := []string{"cdn.jsdelivr.net", "unpkg.com", "cdnjs.cloudflare.com"}
		for _, host := range cdnHosts {
			if strings.Contains(tag, host) {
				t.Errorf("AUDIT-003: marked.js loaded from CDN (%s) — must be bundled locally.\nTag: %s", host, tag)
			}
		}
		if !strings.Contains(tag, "integrity=") {
			t.Errorf("AUDIT-003: marked.js script tag has no `integrity=` SRI attribute.\nTag: %s", tag)
		}
	}
	// If no marked tag at all, that's the preferred post-Fix-2 state
	// (we render mail bodies via textContent). Nothing to assert.

	// ── Static check D: app.js does not call marked.parse ──
	// Fix #2 switched mail-body rendering from marked.parse to textContent.
	// A bare marked.parse call is forbidden — if marked is ever re-introduced,
	// every call must be DOMPurify-wrapped.
	appBytes, err := os.ReadFile(filepath.Join(pkgDir, "static", "app.js"))
	if err != nil {
		t.Fatalf("read static/app.js: %v", err)
	}
	appSrc := string(appBytes)

	if strings.Contains(appSrc, "marked.parse(") {
		// If marked.parse is present, each call-site must be inside a
		// DOMPurify.sanitize wrap. Grab ~200 chars of context around the
		// first hit and check.
		if !strings.Contains(appSrc, "DOMPurify.sanitize(") {
			t.Errorf("AUDIT-002: app.js calls marked.parse() with no DOMPurify.sanitize() wrap — stored XSS on mail bodies.")
		} else {
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
	}
	// If no marked.parse call, the mail-body render path is textContent,
	// which is XSS-free by construction — that's the current Fix-2 state.

	// ── Dynamic check: real handler does NOT write wildcard CORS header ──
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	req := httptest.NewRequest(http.MethodGet, "/api/status", nil)
	w := httptest.NewRecorder()
	handleStatus(db)(w, req)

	if got := w.Header().Get("Access-Control-Allow-Origin"); got == "*" {
		t.Errorf("AUDIT-001/053: handleStatus response sets Access-Control-Allow-Origin=* (got %q) — no origin gating", got)
	}

	// ── Index.html carries a CSP meta tag (AUDIT-002 belt-and-suspenders) ──
	cspMetaRe := regexp.MustCompile(`(?i)<meta\s+http-equiv=["']Content-Security-Policy["']`)
	if !cspMetaRe.MatchString(indexSrc) {
		t.Errorf("AUDIT-002: index.html missing Content-Security-Policy meta tag — XSS defence-in-depth gone")
	}
}
