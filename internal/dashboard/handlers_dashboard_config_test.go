// handlers_dashboard_config_test.go — D11 Phase 3 substrate.
//
// GET /api/dashboard/config happy path + method-gating.
package dashboard

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	dashconfig "force-orchestrator/internal/dashboard/config"
	"force-orchestrator/internal/store"
)

// dashInstallConfig installs a small synthetic dashboard config so the
// handler test doesn't depend on the canonical config/dashboard.yaml
// existing on disk.
func dashInstallConfig(t *testing.T) func() {
	t.Helper()
	cfg, err := dashconfig.ParseConfig([]byte(`
version: 1
tabs:
  - id: tasks
    visible: true
    order: 1
    refresh_seconds: 5
  - id: convoys
    visible: true
    order: 2
    refresh_seconds: 10
  - id: logs
    visible: false
    order: 3
    refresh_seconds: 5
display:
  theme: light
  density: comfortable
  default_sort:
    tasks: created_at_desc
  per_table_pagination: 50
saved_filters: []
`), "test.yaml")
	if err != nil {
		t.Fatalf("dashconfig.ParseConfig: %v", err)
	}
	prev := dashconfig.GetGlobalConfig()
	dashconfig.SetGlobalConfig(cfg)
	return func() { dashconfig.SetGlobalConfig(prev) }
}

// TestHandleDashboardConfig_HappyPath — GET returns the resolved
// config (YAML defaults + SystemConfig overrides composed) as a JSON
// document with the documented shape.
func TestHandleDashboardConfig_HappyPath(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	defer dashInstallConfig(t)()

	// Apply one operator override per axis to confirm composition.
	store.SetConfig(db, dashconfig.ConfigKeyTabVisiblePrefix+"logs", "1")
	store.SetConfig(db, dashconfig.ConfigKeyTabRefreshPrefix+"tasks", "42")
	store.SetConfig(db, dashconfig.ConfigKeyDisplayTheme, "dark")

	req := httptest.NewRequest(http.MethodGet, "/api/dashboard/config", nil)
	rr := httptest.NewRecorder()
	handleDashboardConfig(db)(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", rr.Code, rr.Body.String())
	}

	var resp struct {
		Tabs    []dashconfig.TabConfig   `json:"tabs"`
		Display dashconfig.DisplayConfig `json:"display"`
		// Re-shape SavedFilters as raw JSON slice so we can assert
		// "is an array, not null" without depending on the inner shape.
		SavedFilters []map[string]any `json:"saved_filters"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode body %q: %v", rr.Body.String(), err)
	}
	if len(resp.Tabs) != 3 {
		t.Fatalf("Tabs: got %d, want 3", len(resp.Tabs))
	}
	// tabs are returned in resolved-order ascending.
	wantOrder := []string{"tasks", "convoys", "logs"}
	for i, want := range wantOrder {
		if resp.Tabs[i].ID != want {
			t.Errorf("Tabs[%d].ID = %q, want %q (full=%+v)", i, resp.Tabs[i].ID, want, resp.Tabs)
		}
	}
	// Override-driven assertions.
	for _, tab := range resp.Tabs {
		switch tab.ID {
		case "tasks":
			if tab.RefreshSeconds != 42 {
				t.Errorf("tasks.RefreshSeconds = %d, want 42", tab.RefreshSeconds)
			}
		case "logs":
			if !tab.Visible {
				t.Errorf("logs.Visible = false, want true (override)")
			}
		}
	}
	if string(resp.Display.Theme) != "dark" {
		t.Errorf("Display.Theme = %q, want dark (override)", resp.Display.Theme)
	}
	if string(resp.Display.Density) != "comfortable" {
		t.Errorf("Display.Density = %q, want comfortable (YAML default)", resp.Display.Density)
	}
	// Saved filters MUST serialise as [] not null; the SPA expects an
	// array shape unconditionally.
	if resp.SavedFilters == nil {
		t.Error("SavedFilters: serialised as null; want empty array")
	}
}

// TestHandleDashboardConfig_RejectsNonGET — POST/PUT/DELETE all
// return 405 with a JSON error body.
func TestHandleDashboardConfig_RejectsNonGET(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	defer dashInstallConfig(t)()

	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch} {
		req := httptest.NewRequest(method, "/api/dashboard/config", nil)
		rr := httptest.NewRecorder()
		handleDashboardConfig(db)(rr, req)
		if rr.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s: status = %d, want 405; body=%s", method, rr.Code, rr.Body.String())
		}
	}
}

// TestHandleDashboardConfig_NoConfigInstalled — handler surfaces
// dashconfig.ErrNoConfig as a 500 with a readable JSON error body.
func TestHandleDashboardConfig_NoConfigInstalled(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	prev := dashconfig.GetGlobalConfig()
	dashconfig.SetGlobalConfig(nil)
	defer dashconfig.SetGlobalConfig(prev)

	req := httptest.NewRequest(http.MethodGet, "/api/dashboard/config", nil)
	rr := httptest.NewRecorder()
	handleDashboardConfig(db)(rr, req)
	if rr.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", rr.Code)
	}
}
