// spa_saved_filters_test.go — D11 Phase 3 sub-task C SPA wiring regression.
//
// The saved-filter-pill row, save-modal, and export-modal are wired in
// index.html + app.js. These tests pin:
//
//   - Saved-filter pill row exists for at least one tab in index.html.
//   - GET /api/dashboard/saved-filter is referenced in app.js.
//   - The save-modal markup exists + the JS posts to the create endpoint.
//   - The export button calls /api/dashboard/saved-filter/export.
//   - Delete + DELETE endpoint reference (right-click flow).
//
// If any wiring drops, the SPA's saved-filter UI silently no-ops at
// runtime, which is the same failure mode the D11 P2 NotificationsTab
// wiring regression catches.
package dashboard

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestSPA_SavedFilterPills_Wired — the pill row + GET endpoint must be
// referenced. Without these the operator never sees the saved filters
// they have stored.
func TestSPA_SavedFilterPills_Wired(t *testing.T) {
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

	// (1) Pill row scaffolding.
	for _, marker := range []string{
		`saved-filter-bar`,
		`saved-filter-pills-convoys`,
		`saved-filter-pills-tasks`,
	} {
		if !strings.Contains(indexHTML, marker) {
			t.Errorf("SPA wiring (D11 P3-C): index.html missing %q — saved-filter pills cannot render", marker)
		}
	}

	// (2) GET endpoint referenced in app.js.
	if !strings.Contains(appJS, "/api/dashboard/saved-filter?tab=") {
		t.Errorf(`SPA wiring (D11 P3-C): app.js does not reference "GET /api/dashboard/saved-filter?tab=" — pills will never load`)
	}

	// (3) loadSavedFiltersForTab + renderSavedFilterPills are defined.
	for _, fn := range []string{
		`function loadSavedFiltersForTab(`,
		`function renderSavedFilterPills(`,
		`function applySavedFilter(`,
	} {
		if !strings.Contains(appJS, fn) {
			t.Errorf("SPA wiring (D11 P3-C): app.js missing %q", fn)
		}
	}

	// (4) Pills get re-loaded on switchTab — the override hook must be
	// present so a tab change refreshes the pill row.
	if !strings.Contains(appJS, "loadSavedFiltersForTab(name)") {
		t.Errorf("SPA wiring (D11 P3-C): app.js switchTab override missing loadSavedFiltersForTab(name) hook")
	}
}

// TestSPA_SaveFilterModal_Wired — the save-modal must exist + the
// submit button must POST to the create endpoint.
func TestSPA_SaveFilterModal_Wired(t *testing.T) {
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

	for _, marker := range []string{
		`id="saved-filter-save-modal"`,
		`id="saved-filter-name"`,
		`id="saved-filter-desc"`,
		`data-action="submitSaveFilter"`,
		// Sweep B (CSP refactor) replaced inline onclick="..." with
		// data-action / data-arg attributes. The save-current-filter
		// buttons in tasks + convoys tabs are now wired via the dispatcher;
		// the per-tab arg lives in data-arg.
		`data-action="openSaveFilterModal" data-arg="convoys"`,
		`data-action="openSaveFilterModal" data-arg="tasks"`,
	} {
		if !strings.Contains(indexHTML, marker) {
			t.Errorf("SPA wiring (D11 P3-C): index.html missing save-modal marker %q", marker)
		}
	}

	for _, fn := range []string{
		`function openSaveFilterModal(`,
		`function submitSaveFilter(`,
		`function closeSaveFilterModal(`,
	} {
		if !strings.Contains(appJS, fn) {
			t.Errorf("SPA wiring (D11 P3-C): app.js missing %q", fn)
		}
	}

	// The submit must POST to /api/dashboard/saved-filter (no query
	// string — that's the create endpoint).
	if !strings.Contains(appJS, `fetch('/api/dashboard/saved-filter'`) {
		t.Errorf(`SPA wiring (D11 P3-C): app.js does not POST to "/api/dashboard/saved-filter" — Save button will not persist`)
	}
	if !strings.Contains(appJS, `method: 'POST'`) {
		t.Errorf(`SPA wiring (D11 P3-C): app.js missing 'method: POST' literal`)
	}

	// DELETE wiring (right-click → confirm → DELETE).
	if !strings.Contains(appJS, `'/api/dashboard/saved-filter/'`) || !strings.Contains(appJS, `method: 'DELETE'`) {
		t.Errorf(`SPA wiring (D11 P3-C): app.js missing DELETE wiring for /api/dashboard/saved-filter/<id>`)
	}
}

// TestSPA_ExportSavedFilters_Wired — the export button must call the
// export endpoint and surface the path in the modal.
func TestSPA_ExportSavedFilters_Wired(t *testing.T) {
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

	for _, marker := range []string{
		`id="saved-filter-export-modal"`,
		// Sweep B (CSP refactor) replaced the inline onclick="..." with
		// data-action="..." driven by the delegated dispatcher in app.js.
		`data-action="openExportSavedFiltersModal"`,
		`data-action="submitExportSavedFilters"`,
	} {
		if !strings.Contains(indexHTML, marker) {
			t.Errorf("SPA wiring (D11 P3-C): index.html missing export marker %q", marker)
		}
	}

	for _, fn := range []string{
		`function openExportSavedFiltersModal(`,
		`function submitExportSavedFilters(`,
	} {
		if !strings.Contains(appJS, fn) {
			t.Errorf("SPA wiring (D11 P3-C): app.js missing %q", fn)
		}
	}

	// Submit must POST to /api/dashboard/saved-filter/export.
	if !strings.Contains(appJS, `'/api/dashboard/saved-filter/export'`) {
		t.Errorf(`SPA wiring (D11 P3-C): app.js does not call the export endpoint`)
	}
}
