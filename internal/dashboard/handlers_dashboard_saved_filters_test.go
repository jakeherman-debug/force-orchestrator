// handlers_dashboard_saved_filters_test.go — D11 Phase 3 sub-task C.
//
// Coverage for all four WRITE / READ endpoints:
//
//	GET    /api/dashboard/saved-filter?tab=...
//	POST   /api/dashboard/saved-filter
//	DELETE /api/dashboard/saved-filter/<id>
//	POST   /api/dashboard/saved-filter/export
//
// Plus audit-trail row assertion + anti-cheat (yaml-source rows cannot
// be deleted at runtime).
package dashboard

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	dashconfig "force-orchestrator/internal/dashboard/config"
	"force-orchestrator/internal/store"
)

// dashSavedFilterInstallConfig installs the same synthetic config used
// by the substrate test. Tabs: tasks, convoys, logs.
func dashSavedFilterInstallConfig(t *testing.T) func() {
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
    visible: true
    order: 3
    refresh_seconds: 5
display:
  theme: light
  density: comfortable
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

// ── GET /api/dashboard/saved-filter ──────────────────────────────────────────

func TestHandleDashboardSavedFilter_List_HappyPath(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	defer dashSavedFilterInstallConfig(t)()

	_, _ = db.Exec(
		`INSERT INTO DashboardSavedFilters (name, tab, filter_json, source, created_by)
		 VALUES ('My active', 'convoys', '{"status":["Active"]}', 'dashboard', 'jake'),
		        ('Pinned',    'convoys', '{"status":["Active"]}', 'yaml',      'yaml'),
		        ('elsewhere', 'tasks',   '{"status":["Pending"]}', 'dashboard', 'jake')`,
	)
	req := httptest.NewRequest(http.MethodGet, "/api/dashboard/saved-filter?tab=convoys", nil)
	rr := httptest.NewRecorder()
	handleDashboardSavedFilter(db)(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	var resp struct {
		Tab     string                       `json:"tab"`
		Filters []dashconfig.SavedFilterRow  `json:"filters"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v body=%s", err, rr.Body.String())
	}
	if resp.Tab != "convoys" {
		t.Errorf("tab = %q", resp.Tab)
	}
	if len(resp.Filters) != 2 {
		t.Fatalf("filters len = %d, want 2 (only convoys-tab rows)", len(resp.Filters))
	}
	// yaml-source row sorted first.
	if resp.Filters[0].Source != "yaml" {
		t.Errorf("first source = %q, want yaml (yaml-source sorts first)", resp.Filters[0].Source)
	}
}

func TestHandleDashboardSavedFilter_List_RequiresTab(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	defer dashSavedFilterInstallConfig(t)()
	req := httptest.NewRequest(http.MethodGet, "/api/dashboard/saved-filter", nil)
	rr := httptest.NewRecorder()
	handleDashboardSavedFilter(db)(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rr.Code)
	}
}

func TestHandleDashboardSavedFilter_RejectsNonGETPOST(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	defer dashSavedFilterInstallConfig(t)()
	for _, m := range []string{http.MethodPut, http.MethodPatch, http.MethodDelete} {
		req := httptest.NewRequest(m, "/api/dashboard/saved-filter", nil)
		rr := httptest.NewRecorder()
		handleDashboardSavedFilter(db)(rr, req)
		if rr.Code != http.StatusMethodNotAllowed {
			t.Errorf("method %s: status = %d, want 405", m, rr.Code)
		}
	}
}

// ── POST /api/dashboard/saved-filter (create) ────────────────────────────────

func TestHandleDashboardSavedFilter_Create_HappyPath(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	defer dashSavedFilterInstallConfig(t)()

	body := `{"name":"My active","tab":"convoys","description":"hot stuff",
		"filter":{"status":["Active","DraftPROpen"]},
		"sort_by":"id","sort_dir":"desc","operator":"jake"}`
	req := httptest.NewRequest(http.MethodPost, "/api/dashboard/saved-filter", bytes.NewBufferString(body))
	rr := httptest.NewRecorder()
	handleDashboardSavedFilter(db)(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
	}

	rows, err := dashconfig.ListSavedFilters(db, "convoys")
	if err != nil || len(rows) != 1 {
		t.Fatalf("after create: rows=%d err=%v", len(rows), err)
	}
	if rows[0].Source != "dashboard" {
		t.Errorf("source = %q, want dashboard", rows[0].Source)
	}
	if rows[0].SortDir != "desc" {
		t.Errorf("sort_dir = %q", rows[0].SortDir)
	}

	// Audit trail.
	var found bool
	for _, e := range store.ListAuditLog(db, 10) {
		if e.Action == "dashboard_saved_filter_create" && e.Actor == "jake" && strings.Contains(e.Detail, "name=My active") {
			found = true
		}
	}
	if !found {
		t.Errorf("no dashboard_saved_filter_create audit row for actor=jake")
	}
}

func TestHandleDashboardSavedFilter_Create_RejectsEmptyName(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	defer dashSavedFilterInstallConfig(t)()
	body := `{"name":"","tab":"convoys","filter":{"status":["Active"]}}`
	req := httptest.NewRequest(http.MethodPost, "/api/dashboard/saved-filter", bytes.NewBufferString(body))
	rr := httptest.NewRecorder()
	handleDashboardSavedFilter(db)(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rr.Code)
	}
}

func TestHandleDashboardSavedFilter_Create_RejectsEmptyFilter(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	defer dashSavedFilterInstallConfig(t)()
	body := `{"name":"foo","tab":"convoys","filter":{}}`
	req := httptest.NewRequest(http.MethodPost, "/api/dashboard/saved-filter", bytes.NewBufferString(body))
	rr := httptest.NewRecorder()
	handleDashboardSavedFilter(db)(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rr.Code)
	}
}

func TestHandleDashboardSavedFilter_Create_RejectsUnknownTab(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	defer dashSavedFilterInstallConfig(t)()
	body := `{"name":"foo","tab":"nope","filter":{"status":["Active"]}}`
	req := httptest.NewRequest(http.MethodPost, "/api/dashboard/saved-filter", bytes.NewBufferString(body))
	rr := httptest.NewRecorder()
	handleDashboardSavedFilter(db)(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "unknown tab") {
		t.Errorf("body missing 'unknown tab': %s", rr.Body.String())
	}
}

func TestHandleDashboardSavedFilter_Create_RejectsBadSortDir(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	defer dashSavedFilterInstallConfig(t)()
	body := `{"name":"foo","tab":"convoys","filter":{"status":["Active"]},"sort_dir":"sideways"}`
	req := httptest.NewRequest(http.MethodPost, "/api/dashboard/saved-filter", bytes.NewBufferString(body))
	rr := httptest.NewRecorder()
	handleDashboardSavedFilter(db)(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rr.Code)
	}
}

func TestHandleDashboardSavedFilter_Create_RejectsYAMLNameCollision(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	defer dashSavedFilterInstallConfig(t)()
	_, _ = db.Exec(
		`INSERT INTO DashboardSavedFilters (name, tab, filter_json, source, created_by)
		 VALUES ('Pinned', 'convoys', '{"x":["y"]}', 'yaml', 'yaml')`,
	)
	body := `{"name":"Pinned","tab":"convoys","filter":{"status":["Active"]}}`
	req := httptest.NewRequest(http.MethodPost, "/api/dashboard/saved-filter", bytes.NewBufferString(body))
	rr := httptest.NewRecorder()
	handleDashboardSavedFilter(db)(rr, req)
	if rr.Code != http.StatusConflict {
		t.Errorf("status = %d, want 409 (yaml-source collision)", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "yaml-source") {
		t.Errorf("body missing 'yaml-source' hint: %s", rr.Body.String())
	}
}

func TestHandleDashboardSavedFilter_Create_RejectsDuplicateDashboardName(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	defer dashSavedFilterInstallConfig(t)()
	body := `{"name":"dup","tab":"convoys","filter":{"status":["Active"]}}`
	req := httptest.NewRequest(http.MethodPost, "/api/dashboard/saved-filter", bytes.NewBufferString(body))
	rr := httptest.NewRecorder()
	handleDashboardSavedFilter(db)(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("first create: status = %d", rr.Code)
	}
	req2 := httptest.NewRequest(http.MethodPost, "/api/dashboard/saved-filter", bytes.NewBufferString(body))
	rr2 := httptest.NewRecorder()
	handleDashboardSavedFilter(db)(rr2, req2)
	if rr2.Code != http.StatusConflict {
		t.Errorf("second create: status = %d, want 409", rr2.Code)
	}
}

// ── DELETE /api/dashboard/saved-filter/<id> ──────────────────────────────────

func TestHandleDashboardSavedFilter_Delete_HappyPath(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	defer dashSavedFilterInstallConfig(t)()

	res, _ := db.Exec(
		`INSERT INTO DashboardSavedFilters (name, tab, filter_json, source, created_by)
		 VALUES ('to-delete', 'convoys', '{"x":["y"]}', 'dashboard', 'jake')`,
	)
	id, _ := res.LastInsertId()

	body := `{"operator":"jake","reason":"no longer needed"}`
	req := httptest.NewRequest(http.MethodDelete, fmt.Sprintf("/api/dashboard/saved-filter/%d", id), bytes.NewBufferString(body))
	rr := httptest.NewRecorder()
	handleDashboardSavedFilterByID(db)(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
	}
	rows, _ := dashconfig.ListSavedFilters(db, "convoys")
	if len(rows) != 0 {
		t.Errorf("rows after delete = %d, want 0", len(rows))
	}
	var sawAudit bool
	for _, e := range store.ListAuditLog(db, 10) {
		if e.Action == "dashboard_saved_filter_delete" && strings.Contains(e.Detail, "no longer needed") {
			sawAudit = true
		}
	}
	if !sawAudit {
		t.Errorf("no delete audit row")
	}
}

// TestHandleDashboardSavedFilter_Delete_RejectsYAMLSource — anti-cheat.
// Yaml-source rows cannot be deleted via the dashboard. Operator must
// commit a YAML edit + restart the daemon.
func TestHandleDashboardSavedFilter_Delete_RejectsYAMLSource(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	defer dashSavedFilterInstallConfig(t)()
	res, _ := db.Exec(
		`INSERT INTO DashboardSavedFilters (name, tab, filter_json, source, created_by)
		 VALUES ('yaml-pinned', 'convoys', '{"x":["y"]}', 'yaml', 'yaml')`,
	)
	id, _ := res.LastInsertId()
	req := httptest.NewRequest(http.MethodDelete, fmt.Sprintf("/api/dashboard/saved-filter/%d", id), nil)
	rr := httptest.NewRecorder()
	handleDashboardSavedFilterByID(db)(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (yaml-source delete refused)", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "yaml-source") {
		t.Errorf("body missing 'yaml-source' hint: %s", rr.Body.String())
	}
	// Row must still exist.
	rows, _ := dashconfig.ListSavedFilters(db, "convoys")
	if len(rows) != 1 {
		t.Errorf("rows after rejected delete = %d, want 1", len(rows))
	}
}

func TestHandleDashboardSavedFilter_Delete_RejectsMissingID(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	defer dashSavedFilterInstallConfig(t)()
	req := httptest.NewRequest(http.MethodDelete, "/api/dashboard/saved-filter/9999", nil)
	rr := httptest.NewRecorder()
	handleDashboardSavedFilterByID(db)(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rr.Code)
	}
}

func TestHandleDashboardSavedFilter_Delete_RejectsBadID(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	defer dashSavedFilterInstallConfig(t)()
	req := httptest.NewRequest(http.MethodDelete, "/api/dashboard/saved-filter/notanint", nil)
	rr := httptest.NewRecorder()
	handleDashboardSavedFilterByID(db)(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rr.Code)
	}
}

// ── POST /api/dashboard/saved-filter/export ──────────────────────────────────

func TestHandleDashboardSavedFilter_Export_HappyPath(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	defer dashSavedFilterInstallConfig(t)()

	_, _ = db.Exec(
		`INSERT INTO DashboardSavedFilters (name, tab, description, filter_json, sort_by, sort_dir, source, created_by)
		 VALUES ('My active', 'convoys', 'hot', '{"status":["Active","DraftPROpen"]}', 'id', 'desc', 'dashboard', 'jake'),
		        ('Pinned',    'convoys', 'meh', '{"status":["Active"]}',                '',   '',     'yaml',      'yaml')`,
	)
	body := `{"operator":"jake"}`
	req := httptest.NewRequest(http.MethodPost, "/api/dashboard/saved-filter/export", bytes.NewBufferString(body))
	rr := httptest.NewRecorder()
	handleDashboardSavedFilterByID(db)(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
	}
	var resp struct {
		OK      bool   `json:"ok"`
		Path    string `json:"path"`
		Content string `json:"content"`
		Count   int    `json:"count"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v body=%s", err, rr.Body.String())
	}
	if !resp.OK {
		t.Errorf("ok = false")
	}
	if resp.Count != 1 {
		t.Errorf("count = %d, want 1 (only dashboard-source rows)", resp.Count)
	}
	if resp.Path == "" {
		t.Fatal("path empty")
	}
	// File must exist on disk and contain the dashboard-source filter
	// but NOT the yaml-source one.
	data, err := os.ReadFile(resp.Path)
	if err != nil {
		t.Fatalf("read exported file: %v", err)
	}
	body2 := string(data)
	if !strings.Contains(body2, "name: ") || !strings.Contains(body2, "My active") {
		t.Errorf("exported YAML missing 'My active' filter:\n%s", body2)
	}
	if strings.Contains(body2, "Pinned") {
		t.Errorf("exported YAML contains yaml-source filter (should not):\n%s", body2)
	}
	if !strings.Contains(body2, "saved_filters:") {
		t.Errorf("exported YAML missing saved_filters: header:\n%s", body2)
	}
	// Round-trip: pasting under a fresh YAML must parse cleanly.
	roundTrip := baseFilterRoundTripYAML + body2
	if _, perr := dashconfig.ParseConfig([]byte(roundTrip), "round-trip"); perr != nil {
		t.Errorf("exported YAML does not round-trip parse: %v\n%s", perr, body2)
	}
	// Audit row.
	var sawAudit bool
	for _, e := range store.ListAuditLog(db, 10) {
		if e.Action == "dashboard_saved_filter_export" && strings.Contains(e.Detail, "count=1") {
			sawAudit = true
		}
	}
	if !sawAudit {
		t.Errorf("no export audit row")
	}
}

func TestHandleDashboardSavedFilter_Export_EmptyDashboardSource(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	defer dashSavedFilterInstallConfig(t)()
	body := `{"operator":"jake"}`
	req := httptest.NewRequest(http.MethodPost, "/api/dashboard/saved-filter/export", bytes.NewBufferString(body))
	rr := httptest.NewRecorder()
	handleDashboardSavedFilterByID(db)(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	var resp struct {
		Count   int    `json:"count"`
		Content string `json:"content"`
	}
	_ = json.Unmarshal(rr.Body.Bytes(), &resp)
	if resp.Count != 0 {
		t.Errorf("count = %d, want 0", resp.Count)
	}
	if !strings.Contains(resp.Content, "saved_filters:") {
		t.Errorf("body missing saved_filters: header even when empty")
	}
}

func TestHandleDashboardSavedFilter_Export_RejectsNonPOST(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	defer dashSavedFilterInstallConfig(t)()
	req := httptest.NewRequest(http.MethodGet, "/api/dashboard/saved-filter/export", nil)
	rr := httptest.NewRecorder()
	handleDashboardSavedFilterByID(db)(rr, req)
	if rr.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", rr.Code)
	}
}

// baseFilterRoundTripYAML — minimal valid dashboard.yaml prefix used by
// the export round-trip test. The parser requires version + tabs +
// display before the saved_filters block; we declare a tab named
// "convoys" so the export's `tab: convoys` reference is valid.
const baseFilterRoundTripYAML = `
version: 1
tabs:
  - id: convoys
    visible: true
    order: 1
    refresh_seconds: 5
display:
  theme: light
  density: comfortable
  per_table_pagination: 50
`
