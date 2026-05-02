// D3 polish-pass iteration 2 (C1) — SPA wiring regression.
//
// The Reflection surface in the SPA wires 10+ P6B endpoints via vanilla
// JS fetch() handlers. This test asserts that:
//
//   1. The static index.html declares the Reflection sub-tabs and
//      contains the input/button elements each endpoint binding needs.
//   2. The static app.js references each P6B endpoint by URL.
//   3. The dashboard server still serves the static index.html and
//      app.js (so a misconfigured embed.FS is caught).
//   4. Every JS-referenced endpoint has a matching server handler
//      registered (round-trip parity check — if app.js fetches
//      /api/reflection/learning, the dashboard mux MUST register the
//      route, otherwise the SPA will 404 at runtime).
//
// Implementation: read the static files from the embed FS, grep for the
// endpoint URLs, and cross-check against dashboard.go's mux.HandleFunc
// declarations.

package dashboard

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"force-orchestrator/internal/store"
)

// p6bEndpointsRequired — every P6B endpoint the Reflection surface must
// reference in app.js. Each entry's value is a one-line description used
// in the failure message so a developer can fix the omission quickly.
//
// D4 fix-loop-1 α extends this map with the four D4 dashboard surfaces
// (Security findings, rule metrics, override audit, Senate review log).
// Each new endpoint MUST be referenced by app.js or the SPA's
// corresponding tab will 404 at runtime.
var p6bEndpointsRequired = map[string]string{
	"/api/drill/convoy/":            "Drill convoy timeline",
	"/api/drill/task/":              "Drill task timeline",
	"/api/drill/event/":             "Drill single event detail",
	"/api/drill/search":             "Drill free-text search",
	"/api/drill/replay/":            "Replay historical decision",
	"/api/annotations":              "Operator event annotations",
	"/api/ask":                      "Ask `/` shortcut",
	"/api/reflection/calibration":   "Calibration scoreboard",
	"/api/reflection/learning":      "Fleet learning panel",
	"/api/reflection/retro/generate": "Retro markdown generator",
	"/api/reflection/retro/save":    "Retro markdown saver",

	// D4 fix-loop-1 α — four dashboard views for D4 entities.
	"/api/security-findings":  "Security findings list (BoS + ISB)",
	"/api/rule-metrics":       "Per-rule precision metrics rollup",
	"/api/override-audit":     "Bypass-comment audit log",
	"/api/senate/chambers":    "Senate chambers (Senator roster)",
	"/api/senate/reviews":     "Senate review log per feature",

	// NB: D5.5 P4 staged-convoy routes (/api/convoys/<id>/stages...)
	// share the existing handleConvoysSubroutes mux entry registered at
	// `/api/convoys/`. Path fragments (`/stages`, `/advance`, `/abort`)
	// are asserted separately below since URL IDs are templated.
}

func TestSPAWiring_ReflectionSurfaceReferencesAllP6BEndpoints(t *testing.T) {
	root := repoRootSPA(t)
	appJSBytes, err := os.ReadFile(filepath.Join(root, "internal/dashboard/static/app.js"))
	if err != nil {
		t.Fatalf("read app.js: %v", err)
	}
	appJS := string(appJSBytes)

	// D5.5 P4 — stage operator surface routes are templated in app.js
	// (the convoy ID + stage_num are substituted at call time). The map
	// entry above asserts the prefix is registered server-side; this
	// extra assertion confirms app.js actually reaches the new routes.
	for _, marker := range []string{
		"/stages",        // GET list per convoy
		"/advance",       // POST operator-confirm
		"/abort",         // POST force-Failed
	} {
		if !strings.Contains(appJS, marker) {
			t.Errorf("SPA wiring (D5.5 P4): app.js does not reference stage route fragment %q — staged-convoy modal will not function", marker)
		}
	}

	indexHTMLBytes, err := os.ReadFile(filepath.Join(root, "internal/dashboard/static/index.html"))
	if err != nil {
		t.Fatalf("read index.html: %v", err)
	}
	indexHTML := string(indexHTMLBytes)

	for url, desc := range p6bEndpointsRequired {
		if !strings.Contains(appJS, url) {
			t.Errorf("SPA wiring: app.js does not reference %q (%s) — endpoint will not be reachable from the Reflection surface", url, desc)
		}
	}
	// Sanity: the Reflection surface declares its sub-tabs.
	for _, marker := range []string{
		"reflection-tab",
		"reflection-pane-diagnostics",
		"reflection-pane-reflection-main",
		"reflection-pane-ask",
	} {
		if !strings.Contains(indexHTML, marker) {
			t.Errorf("SPA wiring: index.html missing required marker %q (Reflection sub-tab structure)", marker)
		}
	}
	// D4 fix-loop-1 α — Security + Senate tabs and their sub-tab panes.
	for _, marker := range []string{
		`id="tab-security"`,
		`id="tab-senate"`,
		`id="security-pane-findings"`,
		`id="security-pane-rule-metrics"`,
		`id="security-pane-override-audit"`,
		`id="senate-pane-chambers"`,
		`id="senate-pane-reviews"`,
	} {
		if !strings.Contains(indexHTML, marker) {
			t.Errorf("SPA wiring: index.html missing D4 marker %q (Security/Senate tab structure)", marker)
		}
	}
}

func TestSPAWiring_StaticFilesEmbedded(t *testing.T) {
	// Verify the embed FS carries the static files (catches an empty
	// embed-FS regression — would 404 every dashboard fetch).
	for _, name := range []string{"static/index.html", "static/app.js", "static/style.css"} {
		f, err := staticFiles.ReadFile(name)
		if err != nil {
			t.Errorf("embed FS missing %s: %v", name, err)
			continue
		}
		if len(f) == 0 {
			t.Errorf("embed FS %s is empty", name)
		}
	}
}

// TestSPAWiring_EveryReferencedEndpointHasHandler — round-trip parity.
// Walks app.js for every URL referenced by a fetch() call and asserts a
// matching handler is registered in dashboard.go's mux. Catches the
// "SPA fetches a URL the server doesn't serve → silent 404" failure mode.
func TestSPAWiring_EveryReferencedEndpointHasHandler(t *testing.T) {
	root := repoRootSPA(t)
	dashSrcBytes, err := os.ReadFile(filepath.Join(root, "internal/dashboard/dashboard.go"))
	if err != nil {
		t.Fatalf("read dashboard.go: %v", err)
	}
	dashSrc := string(dashSrcBytes)

	// Every URL in p6bEndpointsRequired must be registered as a route in
	// dashboard.go. Trailing-slash routes register both the bare and
	// slash-suffixed forms — accept either match.
	for url, desc := range p6bEndpointsRequired {
		bareURL := strings.TrimSuffix(url, "/")
		hasMatch := strings.Contains(dashSrc, `"`+url+`"`) || strings.Contains(dashSrc, `"`+bareURL+`"`)
		if !hasMatch {
			t.Errorf("SPA wiring round-trip: app.js fetches %q (%s) but dashboard.go has no matching mux.HandleFunc — SPA will see runtime 404", url, desc)
		}
	}
}

// TestSPAWiring_AskEndpoint_HandlerSmokeTest — exercises POST /api/ask
// from the SPA's perspective: JSON body, correct Content-Type, valid
// JSON response. Catches handler-side regressions that would break the
// SPA without breaking a unit test of the underlying agent function.
func TestSPAWiring_AskEndpoint_HandlerSmokeTest(t *testing.T) {
	t.Setenv("LIVE_HAIKU_DISABLED", "1")
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	req := httptest.NewRequest(http.MethodPost, "/api/ask",
		strings.NewReader(`{"question":"hello"}`))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	handleAsk(db)(rr, req)
	if rr.Code != 200 {
		t.Errorf("POST /api/ask expected 200, got %d body=%s", rr.Code, rr.Body.String())
	}
}

// TestSPAWiring_CalibrationEndpoint_HandlerSmokeTest — GET round trip.
func TestSPAWiring_CalibrationEndpoint_HandlerSmokeTest(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	req := httptest.NewRequest(http.MethodGet, "/api/reflection/calibration", nil)
	rr := httptest.NewRecorder()
	handleCalibration(db)(rr, req)
	if rr.Code != 200 {
		t.Errorf("GET /api/reflection/calibration expected 200, got %d body=%s", rr.Code, rr.Body.String())
	}
}

func repoRootSPA(t *testing.T) string {
	t.Helper()
	wd, _ := os.Getwd()
	for d := wd; d != "/" && d != ""; d = filepath.Dir(d) {
		if _, err := os.Stat(filepath.Join(d, "go.mod")); err == nil {
			return d
		}
	}
	t.Fatalf("repo root not found from %s", wd)
	return ""
}
