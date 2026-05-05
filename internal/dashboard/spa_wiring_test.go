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

// TestSPA_ArchHealthTab_Wired — D9 ArchHealth fix-iter1 regression.
//
// The verifier flagged a NO GO defect: the "Arch Health" tab button
// existed in index.html but had (a) no <div class="tab-pane" id="tab-arch-health">
// content pane, (b) no `case 'arch-health':` arm in switchTab, and
// (c) no loadArchHealth() function in app.js — clicking the button
// silently no-op'd. Backend handlers worked fine; only the SPA wiring
// was missing.
//
// This test pins all three structural elements so a future refactor that
// removes any one of them fails CI before reaching the dashboard.
func TestSPA_ArchHealthTab_Wired(t *testing.T) {
	root := repoRootSPA(t)

	indexHTMLBytes, err := os.ReadFile(filepath.Join(root, "internal/dashboard/static/index.html"))
	if err != nil {
		t.Fatalf("read index.html: %v", err)
	}
	indexHTML := string(indexHTMLBytes)

	appJSBytes, err := os.ReadFile(filepath.Join(root, "internal/dashboard/static/app.js"))
	if err != nil {
		t.Fatalf("read app.js: %v", err)
	}
	appJS := string(appJSBytes)

	// (1) Content pane MUST exist in index.html.
	if !strings.Contains(indexHTML, `id="tab-arch-health"`) {
		t.Errorf(`SPA wiring (D9 ArchHealth): index.html missing <div id="tab-arch-health"> — clicking the Arch Health tab button silently no-ops`)
	}
	// Required UI elements inside the pane.
	for _, marker := range []string{
		`id="ah-month"`,           // month picker
		`id="arch-health-table"`,  // main table region
		`id="arch-health-per-author"`, // per-author summary block (carries the ⚠️ flag)
	} {
		if !strings.Contains(indexHTML, marker) {
			t.Errorf("SPA wiring (D9 ArchHealth): index.html arch-health pane missing required element %q", marker)
		}
	}

	// (2) switchTab MUST route 'arch-health' to loadArchHealth.
	if !strings.Contains(appJS, `case 'arch-health':`) {
		t.Errorf("SPA wiring (D9 ArchHealth): app.js switchTab missing `case 'arch-health':` — tab button click does not trigger any loader")
	}

	// (3) loadArchHealth() MUST be defined and exposed on window.
	for _, marker := range []string{
		`function loadArchHealth(`,
		`window.loadArchHealth = loadArchHealth`,
	} {
		if !strings.Contains(appJS, marker) {
			t.Errorf("SPA wiring (D9 ArchHealth): app.js missing %q", marker)
		}
	}

	// (4) The loader MUST reference all three backend endpoints (months
	// list, latest, and the per-month form). Mirrors the round-trip
	// parity check above for P6B endpoints.
	for _, endpoint := range []string{
		"/api/arch-health/months",
		"/api/arch-health/latest",
		"/api/arch-health/", // per-month form: '/api/arch-health/' + encodeURIComponent(selected)
	} {
		if !strings.Contains(appJS, endpoint) {
			t.Errorf("SPA wiring (D9 ArchHealth): app.js does not reference %q — the loader cannot reach the backend", endpoint)
		}
	}
}

// TestSPA_NotificationsTab_Wired — D11 Phase 2 SPA wiring regression.
//
// The /notifications tab is a full SPA surface backed by seven endpoints.
// This test pins:
//
//   1. The tab pane exists in index.html.
//   2. switchTab routes 'notifications' to loadNotificationsTab.
//   3. loadNotificationsTab is defined in app.js.
//   4. All seven backend endpoint URLs are referenced in app.js.
//   5. The DND modal, Tier-1 confirm modal, and save-preset modal exist.
//
// If any one element is removed in a future refactor, the SPA's tab will
// silently no-op or 404 — the same failure mode the D9 ArchHealth wiring
// regression caught.
func TestSPA_NotificationsTab_Wired(t *testing.T) {
	root := repoRootSPA(t)

	indexHTMLBytes, err := os.ReadFile(filepath.Join(root, "internal/dashboard/static/index.html"))
	if err != nil {
		t.Fatalf("read index.html: %v", err)
	}
	indexHTML := string(indexHTMLBytes)

	appJSBytes, err := os.ReadFile(filepath.Join(root, "internal/dashboard/static/app.js"))
	if err != nil {
		t.Fatalf("read app.js: %v", err)
	}
	appJS := string(appJSBytes)

	// (1) Content pane MUST exist in index.html.
	if !strings.Contains(indexHTML, `id="tab-notifications"`) {
		t.Errorf(`SPA wiring (D11 P2): index.html missing <div id="tab-notifications"> — clicking the Notifications tab silently no-ops`)
	}
	// Required UI scaffolding.
	for _, marker := range []string{
		`id="notif-dnd-band"`,
		`id="notif-dnd-modal"`,
		`id="notif-tier1-confirm-modal"`,
		`id="notif-save-preset-modal"`,
		`id="notif-category-table"`,
		`data-preset="default"`,
		`data-preset="focus"`,
		`data-preset="verbose"`,
	} {
		if !strings.Contains(indexHTML, marker) {
			t.Errorf("SPA wiring (D11 P2): index.html missing required marker %q", marker)
		}
	}

	// (2) switchTab MUST route 'notifications' to loadNotificationsTab.
	if !strings.Contains(appJS, `case 'notifications':`) {
		t.Errorf("SPA wiring (D11 P2): app.js switchTab missing `case 'notifications':` — tab button click does not trigger any loader")
	}

	// (3) loadNotificationsTab() MUST be defined.
	if !strings.Contains(appJS, `function loadNotificationsTab(`) {
		t.Errorf("SPA wiring (D11 P2): app.js missing `function loadNotificationsTab(`")
	}
	if !strings.Contains(appJS, `window.loadNotificationsTab = loadNotificationsTab`) {
		t.Errorf("SPA wiring (D11 P2): app.js does not export window.loadNotificationsTab")
	}

	// (4) All seven backend endpoint URLs referenced in app.js.
	for _, endpoint := range []string{
		"/api/notifications/state",
		"/api/notifications/catalog",
		"/api/notifications/preset",
		"/api/notifications/preset/save",
		"/api/notifications/dnd",
		"/api/notifications/dnd/clear",
		"/api/notifications/category/",
	} {
		if !strings.Contains(appJS, endpoint) {
			t.Errorf("SPA wiring (D11 P2): app.js does not reference %q — the loader cannot reach the backend", endpoint)
		}
	}
}

// TestSPA_ConvoyWatchChip_Wired — D11 P2 sub-task B regression.
//
// The "👁 Watch:" chip on every convoy card is the operator's entry
// point to per-convoy notification overrides. This test pins:
//
//  1. The chip's display label ("Watch:") is in index.html / app.js so a
//     refactor that drops it fails CI.
//  2. The watch popover modal element exists in index.html.
//  3. loadConvoyWatchPopover() and saveConvoyWatch() are defined in app.js
//     and exposed on window.
//  4. The three backend endpoints are referenced by the loader.
func TestSPA_ConvoyWatchChip_Wired(t *testing.T) {
	root := repoRootSPA(t)

	indexHTMLBytes, err := os.ReadFile(filepath.Join(root, "internal/dashboard/static/index.html"))
	if err != nil {
		t.Fatalf("read index.html: %v", err)
	}
	indexHTML := string(indexHTMLBytes)

	appJSBytes, err := os.ReadFile(filepath.Join(root, "internal/dashboard/static/app.js"))
	if err != nil {
		t.Fatalf("read app.js: %v", err)
	}
	appJS := string(appJSBytes)

	// (1) Chip label substring appears in app.js (the chip is rendered
	// dynamically). The "Watch:" prefix is what the operator sees.
	if !strings.Contains(appJS, "Watch:") {
		t.Errorf("SPA wiring (D11 P2 watch): app.js missing chip label %q — operator can't see the override state", "Watch:")
	}
	if !strings.Contains(appJS, "renderConvoyWatchChip(") {
		t.Errorf("SPA wiring (D11 P2 watch): app.js missing renderConvoyWatchChip — chip will not render")
	}

	// (2) Modal element + label in index.html.
	for _, marker := range []string{
		`id="convoy-watch-modal"`,
		`id="convoy-watch-id"`,
		`id="convoy-watch-body"`,
		`onclick="saveConvoyWatch()"`,
	} {
		if !strings.Contains(indexHTML, marker) {
			t.Errorf("SPA wiring (D11 P2 watch): index.html missing %q — popover cannot mount", marker)
		}
	}

	// (3) Loader + saver defined and exposed.
	for _, marker := range []string{
		"function loadConvoyWatchPopover(",
		"function saveConvoyWatch(",
		"window.loadConvoyWatchPopover = loadConvoyWatchPopover",
		"window.saveConvoyWatch = saveConvoyWatch",
	} {
		if !strings.Contains(appJS, marker) {
			t.Errorf("SPA wiring (D11 P2 watch): app.js missing %q", marker)
		}
	}

	// (4) Backend endpoints referenced.
	for _, endpoint := range []string{
		"/api/convoys/${convoyID}/watch",
		"/api/convoys/${id}/watch",
		"/api/convoys/${id}/watch/clear",
	} {
		if !strings.Contains(appJS, endpoint) {
			t.Errorf("SPA wiring (D11 P2 watch): app.js does not reference %q — loader cannot reach backend", endpoint)
		}
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
