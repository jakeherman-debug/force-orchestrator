// spa_wiring_d11p3b_test.go — D11 Phase 3 sub-task B SPA-wiring regression.
//
// Asserts that:
//
//   1. app.js fetches /api/dashboard/config and references it as the
//      source of truth for tab visibility / ordering / refresh.
//   2. The SPA applies theme + density classes to <body>.
//   3. The refresh-interval reading code references a tab's
//      refresh_seconds field (no hardcoded 12000ms / 5000ms anymore for
//      per-tab content polling).
//   4. The CSS exposes the theme / density classes the JS toggles.
//   5. The new write endpoints are registered server-side.
//
// Same shape as the existing TestSPA_NotificationsTab_Wired regression
// — grep-and-assert against the static files + dashboard.go mux.

package dashboard

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestSPA_TabConfigRespected_Wired — app.js references cfg.tabs as the
// source of truth for visibility + ordering rather than a hardcoded
// list.
func TestSPA_TabConfigRespected_Wired(t *testing.T) {
	root := repoRootSPA(t)
	appJSBytes, err := os.ReadFile(filepath.Join(root, "internal/dashboard/static/app.js"))
	if err != nil {
		t.Fatalf("read app.js: %v", err)
	}
	appJS := string(appJSBytes)

	// (1) The fetch must hit /api/dashboard/config.
	if !strings.Contains(appJS, "/api/dashboard/config") {
		t.Errorf("SPA wiring (D11 P3-B): app.js does not fetch /api/dashboard/config — tab config is ignored")
	}
	// (2) A loadDashboardConfig() function must exist and be called at
	//     boot.
	if !strings.Contains(appJS, "function loadDashboardConfig(") {
		t.Errorf("SPA wiring (D11 P3-B): app.js missing function loadDashboardConfig(")
	}
	if !strings.Contains(appJS, "loadDashboardConfig()") {
		t.Errorf("SPA wiring (D11 P3-B): app.js never calls loadDashboardConfig() at boot")
	}
	// (3) The visibility-driven render path must reference cfg.tabs and
	//     the visible / order fields (substring match — exact wording
	//     can drift, but the field names are stable across the
	//     write-endpoint contract).
	for _, marker := range []string{
		"dashCfg.tabs",
		"applyDashTabVisibilityAndOrder",
		".visible",
		".order",
	} {
		if !strings.Contains(appJS, marker) {
			t.Errorf("SPA wiring (D11 P3-B): app.js missing marker %q — tab visibility/order is not driven by config", marker)
		}
	}
}

// TestSPA_ThemeDensity_Applied — app.js applies theme-* and density-*
// classes to <body>; the CSS exposes those classes.
func TestSPA_ThemeDensity_Applied(t *testing.T) {
	root := repoRootSPA(t)
	appJSBytes, err := os.ReadFile(filepath.Join(root, "internal/dashboard/static/app.js"))
	if err != nil {
		t.Fatalf("read app.js: %v", err)
	}
	appJS := string(appJSBytes)
	cssBytes, err := os.ReadFile(filepath.Join(root, "internal/dashboard/static/style.css"))
	if err != nil {
		t.Fatalf("read style.css: %v", err)
	}
	css := string(cssBytes)

	// (1) JS adds the classes on <body>.
	for _, marker := range []string{
		"document.body.classList",
		"theme-light",
		"theme-dark",
		"density-compact",
		"density-comfortable",
		"prefers-color-scheme",
	} {
		if !strings.Contains(appJS, marker) {
			t.Errorf("SPA wiring (D11 P3-B): app.js missing theme/density marker %q", marker)
		}
	}
	// (2) CSS exposes the body-class selectors.
	for _, marker := range []string{
		"body.theme-light",
		"body.theme-dark",
		"body.density-compact",
		"body.density-comfortable",
	} {
		if !strings.Contains(css, marker) {
			t.Errorf("SPA wiring (D11 P3-B): style.css missing theme/density selector %q", marker)
		}
	}
	// (3) applyDashTheme + applyDashDensity must exist in app.js.
	for _, fn := range []string{
		"function applyDashTheme(",
		"function applyDashDensity(",
	} {
		if !strings.Contains(appJS, fn) {
			t.Errorf("SPA wiring (D11 P3-B): app.js missing %q", fn)
		}
	}
}

// TestSPA_RefreshInterval_FromConfig — the per-tab refresh interval is
// read from cfg.tabs[i].refresh_seconds, not a hardcoded ms literal.
func TestSPA_RefreshInterval_FromConfig(t *testing.T) {
	root := repoRootSPA(t)
	appJSBytes, err := os.ReadFile(filepath.Join(root, "internal/dashboard/static/app.js"))
	if err != nil {
		t.Fatalf("read app.js: %v", err)
	}
	appJS := string(appJSBytes)

	// (1) dashTabRefreshSeconds reads refresh_seconds.
	if !strings.Contains(appJS, "function dashTabRefreshSeconds(") {
		t.Errorf("SPA wiring (D11 P3-B): app.js missing dashTabRefreshSeconds — interval is not config-driven")
	}
	if !strings.Contains(appJS, ".refresh_seconds") {
		t.Errorf("SPA wiring (D11 P3-B): app.js does not reference .refresh_seconds field")
	}
	// (2) rearmDashTabRefresh exists and is called from switchTab.
	if !strings.Contains(appJS, "function rearmDashTabRefresh(") {
		t.Errorf("SPA wiring (D11 P3-B): app.js missing rearmDashTabRefresh")
	}
	if !strings.Contains(appJS, "rearmDashTabRefresh(name)") {
		t.Errorf("SPA wiring (D11 P3-B): switchTab does not call rearmDashTabRefresh(name) — interval not re-armed on tab switch")
	}
}

// TestSPA_DashConfigWriteEndpoints_Registered — the dashboard mux
// registers the new write routes; app.js (or operator tooling) can hit
// them.
func TestSPA_DashConfigWriteEndpoints_Registered(t *testing.T) {
	root := repoRootSPA(t)
	dashSrcBytes, err := os.ReadFile(filepath.Join(root, "internal/dashboard/dashboard.go"))
	if err != nil {
		t.Fatalf("read dashboard.go: %v", err)
	}
	dashSrc := string(dashSrcBytes)

	for _, route := range []string{
		"/api/dashboard/config/tab/",
		"/api/dashboard/config/display",
		"/api/dashboard/config/display/clear",
	} {
		if !strings.Contains(dashSrc, `"`+route+`"`) {
			t.Errorf("SPA wiring (D11 P3-B): dashboard.go does not register route %q", route)
		}
	}
	// And the handler functions exist.
	for _, fn := range []string{
		"handleDashboardConfigTabWrite",
		"handleDashboardConfigDisplayWrite",
	} {
		if !strings.Contains(dashSrc, fn) {
			t.Errorf("SPA wiring (D11 P3-B): dashboard.go does not reference %q", fn)
		}
	}
}
