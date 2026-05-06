// handlers_dashboard_config_write_test.go — D11 Phase 3 sub-task B.
//
// Coverage:
//
//   - Per-tab + display set: happy path for each individual field and
//     all-together.
//   - Validation rejection for invalid theme, invalid density, invalid
//     refresh_seconds, invalid pagination, unknown tab, unknown
//     default_sort tab.
//   - Method-gating (405 for non-POST).
//   - /clear paths actually clear the SystemConfig keys.
//   - Audit-trail row lands for every successful mutation (action,
//     actor, detail).
package dashboard

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	dashconfig "force-orchestrator/internal/dashboard/config"
	"force-orchestrator/internal/store"
)

// dashConfigPostJSON is a tiny ergonomic wrapper for the write tests.
// Matches the shape used by the rest of the dashboard test suite.
func dashConfigPostJSON(t *testing.T, h http.HandlerFunc, path, bodyJSON string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(bodyJSON))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	h(rr, req)
	return rr
}

// TestHandleDashboardConfigTab_HappyPath_AllFields — POST sets all three
// keys atomically and the resolver picks them up.
func TestHandleDashboardConfigTab_HappyPath_AllFields(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	defer dashInstallConfig(t)()

	rr := dashConfigPostJSON(t, handleDashboardConfigTabWrite(db),
		"/api/dashboard/config/tab/tasks",
		`{"visible":false,"order":42,"refresh_seconds":120,"operator":"jake"}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", rr.Code, rr.Body.String())
	}

	if got := store.GetConfig(db, dashconfig.ConfigKeyTabVisiblePrefix+"tasks", ""); got != "0" {
		t.Errorf("visible key = %q, want \"0\"", got)
	}
	if got := store.GetConfig(db, dashconfig.ConfigKeyTabOrderPrefix+"tasks", ""); got != "42" {
		t.Errorf("order key = %q, want \"42\"", got)
	}
	if got := store.GetConfig(db, dashconfig.ConfigKeyTabRefreshPrefix+"tasks", ""); got != "120" {
		t.Errorf("refresh key = %q, want \"120\"", got)
	}

	// Audit row landed.
	var auditCount int
	db.QueryRow(`SELECT COUNT(*) FROM AuditLog WHERE actor=? AND action=?`, "jake", "dashboard_tab_set").Scan(&auditCount)
	if auditCount != 1 {
		t.Errorf("audit rows = %d, want 1", auditCount)
	}

	// Resolved view picks up the override.
	resolved, err := dashconfig.ResolveTabConfig(db, "tasks")
	if err != nil {
		t.Fatalf("ResolveTabConfig: %v", err)
	}
	if resolved.Visible || resolved.Order != 42 || resolved.RefreshSeconds != 120 {
		t.Errorf("resolved tab = %+v, want {visible=false order=42 refresh=120}", resolved)
	}
}

// TestHandleDashboardConfigTab_HappyPath_PerField — each field can be
// set independently; setting one doesn't clobber the others.
func TestHandleDashboardConfigTab_HappyPath_PerField(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	defer dashInstallConfig(t)()

	cases := []struct {
		name string
		body string
		key  string
		want string
	}{
		{"visible only", `{"visible":true}`, dashconfig.ConfigKeyTabVisiblePrefix + "convoys", "1"},
		{"order only", `{"order":7}`, dashconfig.ConfigKeyTabOrderPrefix + "convoys", "7"},
		{"refresh only", `{"refresh_seconds":15}`, dashconfig.ConfigKeyTabRefreshPrefix + "convoys", "15"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rr := dashConfigPostJSON(t, handleDashboardConfigTabWrite(db),
				"/api/dashboard/config/tab/convoys", tc.body)
			if rr.Code != http.StatusOK {
				t.Fatalf("status %d body %s", rr.Code, rr.Body.String())
			}
			if got := store.GetConfig(db, tc.key, ""); got != tc.want {
				t.Errorf("key %s = %q, want %q", tc.key, got, tc.want)
			}
		})
	}
}

// TestHandleDashboardConfigTab_RejectInvalidRefresh — refresh_seconds
// must be in (0, 3600].
func TestHandleDashboardConfigTab_RejectInvalidRefresh(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	defer dashInstallConfig(t)()

	for _, body := range []string{
		`{"refresh_seconds":0}`,
		`{"refresh_seconds":-1}`,
		`{"refresh_seconds":3601}`,
		`{"refresh_seconds":99999}`,
	} {
		rr := dashConfigPostJSON(t, handleDashboardConfigTabWrite(db),
			"/api/dashboard/config/tab/tasks", body)
		if rr.Code != http.StatusBadRequest {
			t.Errorf("body=%s: status = %d, want 400", body, rr.Code)
		}
	}
	// Confirm no key was written.
	if got := store.GetConfig(db, dashconfig.ConfigKeyTabRefreshPrefix+"tasks", ""); got != "" {
		t.Errorf("refresh key = %q, want unset", got)
	}
}

// TestHandleDashboardConfigTab_RejectInvalidOrder — order must be > 0.
func TestHandleDashboardConfigTab_RejectInvalidOrder(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	defer dashInstallConfig(t)()

	for _, body := range []string{`{"order":0}`, `{"order":-1}`} {
		rr := dashConfigPostJSON(t, handleDashboardConfigTabWrite(db),
			"/api/dashboard/config/tab/tasks", body)
		if rr.Code != http.StatusBadRequest {
			t.Errorf("body=%s: status = %d, want 400", body, rr.Code)
		}
	}
}

// TestHandleDashboardConfigTab_RejectUnknownTab — tab id must exist in
// the YAML registry.
func TestHandleDashboardConfigTab_RejectUnknownTab(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	defer dashInstallConfig(t)()

	rr := dashConfigPostJSON(t, handleDashboardConfigTabWrite(db),
		"/api/dashboard/config/tab/nonexistent", `{"visible":true}`)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400; body=%s", rr.Code, rr.Body.String())
	}
}

// TestHandleDashboardConfigTab_RejectEmptyBody — operator sent a POST
// with no fields → 400 (no silent no-op).
func TestHandleDashboardConfigTab_RejectEmptyBody(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	defer dashInstallConfig(t)()

	rr := dashConfigPostJSON(t, handleDashboardConfigTabWrite(db),
		"/api/dashboard/config/tab/tasks", `{}`)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400; body=%s", rr.Code, rr.Body.String())
	}
}

// TestHandleDashboardConfigTab_MethodGated — non-POST → 405.
func TestHandleDashboardConfigTab_MethodGated(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	defer dashInstallConfig(t)()

	for _, method := range []string{http.MethodGet, http.MethodPut, http.MethodDelete, http.MethodPatch} {
		req := httptest.NewRequest(method, "/api/dashboard/config/tab/tasks", strings.NewReader(`{}`))
		rr := httptest.NewRecorder()
		handleDashboardConfigTabWrite(db)(rr, req)
		if rr.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s: status = %d, want 405", method, rr.Code)
		}
	}
}

// TestHandleDashboardConfigTab_Clear — /clear wipes all three keys.
func TestHandleDashboardConfigTab_Clear(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	defer dashInstallConfig(t)()

	// Pre-populate.
	store.SetConfig(db, dashconfig.ConfigKeyTabVisiblePrefix+"logs", "0")
	store.SetConfig(db, dashconfig.ConfigKeyTabOrderPrefix+"logs", "9")
	store.SetConfig(db, dashconfig.ConfigKeyTabRefreshPrefix+"logs", "60")

	rr := dashConfigPostJSON(t, handleDashboardConfigTabWrite(db),
		"/api/dashboard/config/tab/logs/clear", `{"operator":"jake"}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rr.Code, rr.Body.String())
	}
	for _, key := range []string{
		dashconfig.ConfigKeyTabVisiblePrefix + "logs",
		dashconfig.ConfigKeyTabOrderPrefix + "logs",
		dashconfig.ConfigKeyTabRefreshPrefix + "logs",
	} {
		if got := store.GetConfig(db, key, ""); got != "" {
			t.Errorf("key %s = %q after clear, want empty", key, got)
		}
	}
	var auditCount int
	db.QueryRow(`SELECT COUNT(*) FROM AuditLog WHERE actor=? AND action=?`, "jake", "dashboard_tab_clear").Scan(&auditCount)
	if auditCount != 1 {
		t.Errorf("audit rows = %d, want 1", auditCount)
	}
}

// TestHandleDashboardConfigDisplay_HappyPath_AllFields — set every
// display key in one POST.
func TestHandleDashboardConfigDisplay_HappyPath_AllFields(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	defer dashInstallConfig(t)()

	body := `{
		"theme":"dark",
		"density":"compact",
		"default_sort":{"tasks":"updated_desc"},
		"per_table_pagination":100,
		"operator":"jake"
	}`
	rr := dashConfigPostJSON(t, handleDashboardConfigDisplayWrite(db),
		"/api/dashboard/config/display", body)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
	}
	if got := store.GetConfig(db, dashconfig.ConfigKeyDisplayTheme, ""); got != "dark" {
		t.Errorf("theme = %q, want dark", got)
	}
	if got := store.GetConfig(db, dashconfig.ConfigKeyDisplayDensity, ""); got != "compact" {
		t.Errorf("density = %q, want compact", got)
	}
	if got := store.GetConfig(db, dashconfig.ConfigKeyDisplayPagSize, ""); got != "100" {
		t.Errorf("pagination = %q, want 100", got)
	}
	if got := store.GetConfig(db, dashconfig.ConfigKeyDisplaySortPfx+"tasks", ""); got != "updated_desc" {
		t.Errorf("default_sort[tasks] = %q, want updated_desc", got)
	}
	var auditCount int
	db.QueryRow(`SELECT COUNT(*) FROM AuditLog WHERE actor=? AND action=?`, "jake", "dashboard_display_set").Scan(&auditCount)
	if auditCount != 1 {
		t.Errorf("audit rows = %d, want 1", auditCount)
	}
}

// TestHandleDashboardConfigDisplay_HappyPath_PerField — each field
// settable independently.
func TestHandleDashboardConfigDisplay_HappyPath_PerField(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	defer dashInstallConfig(t)()

	cases := []struct {
		name string
		body string
		key  string
		want string
	}{
		{"theme only", `{"theme":"system"}`, dashconfig.ConfigKeyDisplayTheme, "system"},
		{"density only", `{"density":"comfortable"}`, dashconfig.ConfigKeyDisplayDensity, "comfortable"},
		{"pagination only", `{"per_table_pagination":25}`, dashconfig.ConfigKeyDisplayPagSize, "25"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rr := dashConfigPostJSON(t, handleDashboardConfigDisplayWrite(db),
				"/api/dashboard/config/display", tc.body)
			if rr.Code != http.StatusOK {
				t.Fatalf("status %d body %s", rr.Code, rr.Body.String())
			}
			if got := store.GetConfig(db, tc.key, ""); got != tc.want {
				t.Errorf("key %s = %q, want %q", tc.key, got, tc.want)
			}
		})
	}
}

// TestHandleDashboardConfigDisplay_RejectInvalidTheme — theme must be
// in the closed enum.
func TestHandleDashboardConfigDisplay_RejectInvalidTheme(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	defer dashInstallConfig(t)()

	for _, body := range []string{`{"theme":"cerulean"}`, `{"theme":""}`, `{"theme":"DARK"}`} {
		rr := dashConfigPostJSON(t, handleDashboardConfigDisplayWrite(db),
			"/api/dashboard/config/display", body)
		if rr.Code != http.StatusBadRequest {
			t.Errorf("body=%s: status = %d, want 400", body, rr.Code)
		}
	}
}

// TestHandleDashboardConfigDisplay_RejectInvalidDensity — density must
// be in the closed enum.
func TestHandleDashboardConfigDisplay_RejectInvalidDensity(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	defer dashInstallConfig(t)()

	for _, body := range []string{`{"density":"snug"}`, `{"density":""}`} {
		rr := dashConfigPostJSON(t, handleDashboardConfigDisplayWrite(db),
			"/api/dashboard/config/display", body)
		if rr.Code != http.StatusBadRequest {
			t.Errorf("body=%s: status = %d, want 400", body, rr.Code)
		}
	}
}

// TestHandleDashboardConfigDisplay_RejectInvalidPagination — pagination
// out of [10, 500] is rejected.
func TestHandleDashboardConfigDisplay_RejectInvalidPagination(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	defer dashInstallConfig(t)()

	for _, body := range []string{
		`{"per_table_pagination":0}`,
		`{"per_table_pagination":-5}`,
		`{"per_table_pagination":9}`,
		`{"per_table_pagination":501}`,
		`{"per_table_pagination":99999}`,
	} {
		rr := dashConfigPostJSON(t, handleDashboardConfigDisplayWrite(db),
			"/api/dashboard/config/display", body)
		if rr.Code != http.StatusBadRequest {
			t.Errorf("body=%s: status = %d, want 400", body, rr.Code)
		}
	}
}

// TestHandleDashboardConfigDisplay_RejectUnknownSortTab — default_sort
// referencing an unregistered tab is rejected.
func TestHandleDashboardConfigDisplay_RejectUnknownSortTab(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	defer dashInstallConfig(t)()

	rr := dashConfigPostJSON(t, handleDashboardConfigDisplayWrite(db),
		"/api/dashboard/config/display",
		`{"default_sort":{"madeup":"asc"}}`)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400; body=%s", rr.Code, rr.Body.String())
	}
}

// TestHandleDashboardConfigDisplay_MethodGated — non-POST → 405.
func TestHandleDashboardConfigDisplay_MethodGated(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	defer dashInstallConfig(t)()

	for _, method := range []string{http.MethodGet, http.MethodPut, http.MethodDelete} {
		req := httptest.NewRequest(method, "/api/dashboard/config/display", strings.NewReader(`{}`))
		rr := httptest.NewRecorder()
		handleDashboardConfigDisplayWrite(db)(rr, req)
		if rr.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s: status = %d, want 405", method, rr.Code)
		}
	}
}

// TestHandleDashboardConfigDisplay_Clear — /clear wipes every display
// key including per-tab sort overrides.
func TestHandleDashboardConfigDisplay_Clear(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	defer dashInstallConfig(t)()

	// Pre-populate every key.
	store.SetConfig(db, dashconfig.ConfigKeyDisplayTheme, "dark")
	store.SetConfig(db, dashconfig.ConfigKeyDisplayDensity, "compact")
	store.SetConfig(db, dashconfig.ConfigKeyDisplayPagSize, "75")
	store.SetConfig(db, dashconfig.ConfigKeyDisplaySortPfx+"tasks", "abc")
	store.SetConfig(db, dashconfig.ConfigKeyDisplaySortPfx+"convoys", "xyz")

	rr := dashConfigPostJSON(t, handleDashboardConfigDisplayWrite(db),
		"/api/dashboard/config/display/clear", `{"operator":"jake"}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rr.Code, rr.Body.String())
	}
	for _, key := range []string{
		dashconfig.ConfigKeyDisplayTheme,
		dashconfig.ConfigKeyDisplayDensity,
		dashconfig.ConfigKeyDisplayPagSize,
		dashconfig.ConfigKeyDisplaySortPfx + "tasks",
		dashconfig.ConfigKeyDisplaySortPfx + "convoys",
	} {
		if got := store.GetConfig(db, key, ""); got != "" {
			t.Errorf("key %s = %q after clear, want empty", key, got)
		}
	}
	var auditCount int
	db.QueryRow(`SELECT COUNT(*) FROM AuditLog WHERE actor=? AND action=?`, "jake", "dashboard_display_clear").Scan(&auditCount)
	if auditCount != 1 {
		t.Errorf("audit rows = %d, want 1", auditCount)
	}
}

// TestHandleDashboardConfigDisplay_RejectEmptyBody — POST without any
// mutable fields → 400.
func TestHandleDashboardConfigDisplay_RejectEmptyBody(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	defer dashInstallConfig(t)()

	rr := dashConfigPostJSON(t, handleDashboardConfigDisplayWrite(db),
		"/api/dashboard/config/display", `{}`)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400; body=%s", rr.Code, rr.Body.String())
	}
}

// TestHandleDashboardConfigTab_AuditPayloadShape — the audit detail
// records the tab + which fields landed.
func TestHandleDashboardConfigTab_AuditPayloadShape(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	defer dashInstallConfig(t)()

	rr := dashConfigPostJSON(t, handleDashboardConfigTabWrite(db),
		"/api/dashboard/config/tab/tasks",
		`{"visible":true,"refresh_seconds":12,"operator":"jake"}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("status %d", rr.Code)
	}
	var detail string
	db.QueryRow(`SELECT detail FROM AuditLog WHERE actor=? AND action=? ORDER BY id DESC LIMIT 1`,
		"jake", "dashboard_tab_set").Scan(&detail)
	if !strings.Contains(detail, "tab=tasks") {
		t.Errorf("audit detail %q missing tab=tasks", detail)
	}
	if !strings.Contains(detail, "visible") || !strings.Contains(detail, "refresh_seconds") {
		t.Errorf("audit detail %q missing field markers", detail)
	}
}

// TestHandleDashboardConfigTab_ResolverIntegration — the SystemConfig
// keys we wrote actually flow through ResolveAllTabs in the order
// specified by the override.
func TestHandleDashboardConfigTab_ResolverIntegration(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	defer dashInstallConfig(t)()

	// Reorder convoys above tasks (YAML has tasks=1, convoys=2).
	rr := dashConfigPostJSON(t, handleDashboardConfigTabWrite(db),
		"/api/dashboard/config/tab/convoys", `{"order":1}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("status %d", rr.Code)
	}
	rr = dashConfigPostJSON(t, handleDashboardConfigTabWrite(db),
		"/api/dashboard/config/tab/tasks", `{"order":2}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("status %d", rr.Code)
	}
	tabs, err := dashconfig.ResolveAllTabs(db)
	if err != nil {
		t.Fatalf("ResolveAllTabs: %v", err)
	}
	if len(tabs) < 2 || tabs[0].ID != "convoys" || tabs[1].ID != "tasks" {
		ids := make([]string, len(tabs))
		for i, t := range tabs {
			ids[i] = t.ID
		}
		t.Errorf("resolved order = %v, want [convoys, tasks, ...]", ids)
	}
}

// TestHandleDashboardConfigDisplay_GetEndpointReflectsOverrides — the
// GET endpoint surfaces what we POSTed (round-trip parity).
func TestHandleDashboardConfigDisplay_GetEndpointReflectsOverrides(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	defer dashInstallConfig(t)()

	rr := dashConfigPostJSON(t, handleDashboardConfigDisplayWrite(db),
		"/api/dashboard/config/display",
		`{"theme":"dark","density":"compact"}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("set status %d body=%s", rr.Code, rr.Body.String())
	}
	getReq := httptest.NewRequest(http.MethodGet, "/api/dashboard/config", nil)
	getRR := httptest.NewRecorder()
	handleDashboardConfig(db)(getRR, getReq)
	if getRR.Code != http.StatusOK {
		t.Fatalf("get status %d body=%s", getRR.Code, getRR.Body.String())
	}
	var resp struct {
		Display struct {
			Theme   string `json:"theme"`
			Density string `json:"density"`
		} `json:"display"`
	}
	if err := json.Unmarshal(getRR.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Display.Theme != "dark" || resp.Display.Density != "compact" {
		t.Errorf("display = %+v, want theme=dark density=compact", resp.Display)
	}
}
