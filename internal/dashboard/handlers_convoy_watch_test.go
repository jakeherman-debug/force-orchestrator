// internal/dashboard/handlers_convoy_watch_test.go — D11 Phase 2 Sub-task B.
//
// Per-endpoint coverage for /api/convoys/<id>/watch GET, POST, and
// /watch/clear POST. Real in-memory SQLite (CLAUDE.md "never mock the
// database"); httptest for the HTTP shape.

package dashboard

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"database/sql"

	"force-orchestrator/internal/store"
)

// seedRegistryForWatch inserts a couple of categories so GET /watch can
// return a non-empty Categories field. The notify package owns the real
// seeder; here we just need rows in NotificationCategoryRegistry.
func seedRegistryForWatch(t *testing.T, db *sql.DB) {
	t.Helper()
	for _, row := range []struct {
		name        string
		tier        int
		def         string
		description string
	}{
		{"convoy_progress", 2, "mail", "convoy progress updates"},
		{"convoy_complete", 1, "mail+slack", "convoy hits terminal status"},
	} {
		_, err := db.Exec(
			`INSERT INTO NotificationCategoryRegistry (category, tier, yaml_default, description, yaml_version)
			 VALUES (?, ?, ?, ?, 1)`,
			row.name, row.tier, row.def, row.description,
		)
		if err != nil {
			t.Fatalf("seed registry %q: %v", row.name, err)
		}
	}
}

// callConvoyWatch exercises the dispatcher with a synthetic URL of the
// form `/api/convoys/<id>/watch[...]`. We invoke handleConvoyWatch
// directly with the path-tail parts to avoid pulling the whole convoys
// subroute mux into the test (those routes have side effects we don't
// want).
func callConvoyWatch(db *sql.DB, method, urlSuffix string, body string, convoyID int, parts []string) *httptest.ResponseRecorder {
	url := "/api/convoys/" + intStr(convoyID) + "/" + strings.Join(parts, "/") + urlSuffix
	var req *http.Request
	if body == "" {
		req = httptest.NewRequest(method, url, nil)
	} else {
		req = httptest.NewRequest(method, url, strings.NewReader(body))
	}
	rec := httptest.NewRecorder()
	handleConvoyWatch(db, rec, req, convoyID, parts)
	return rec
}

func TestHandleConvoyWatch_GET_NoOverride_ReturnsCategories(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	seedRegistryForWatch(t, db)

	rec := callConvoyWatch(db, http.MethodGet, "", "", 17, []string{"watch"})
	if rec.Code != 200 {
		t.Fatalf("status: %d (body=%s)", rec.Code, rec.Body.String())
	}
	var resp watchGetResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v (body=%s)", err, rec.Body.String())
	}
	if resp.HasOverride {
		t.Errorf("expected has_override=false on fresh convoy, got %+v", resp)
	}
	if len(resp.Categories) != 2 {
		t.Errorf("expected 2 categories from seeded registry, got %d", len(resp.Categories))
	}
	if len(resp.Settings) != 4 {
		t.Errorf("expected 4 settings, got %d: %v", len(resp.Settings), resp.Settings)
	}
}

func TestHandleConvoyWatch_GET_WithOverride_ReturnsParsedRow(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	seedRegistryForWatch(t, db)

	if err := store.UpsertConvoyNotificationOverride(db, store.ConvoyNotificationOverride{
		ConvoyID:   42,
		Mode:       "custom_json",
		CustomJSON: `{"convoy_progress":"slack","*":"off"}`,
		SetBy:      "jake",
		Reason:     "shadow rollout",
	}); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	rec := callConvoyWatch(db, http.MethodGet, "", "", 42, []string{"watch"})
	if rec.Code != 200 {
		t.Fatalf("status: %d (body=%s)", rec.Code, rec.Body.String())
	}
	var resp watchGetResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !resp.HasOverride || resp.Override == nil {
		t.Fatalf("expected has_override=true, got %+v", resp)
	}
	if resp.Override.Mode != "custom_json" {
		t.Errorf("mode mismatch: %q", resp.Override.Mode)
	}
	if resp.Override.CustomJSON["convoy_progress"] != "slack" {
		t.Errorf("custom_json convoy_progress: %v", resp.Override.CustomJSON)
	}
	if resp.Override.SetBy != "jake" || resp.Override.Reason != "shadow rollout" {
		t.Errorf("audit fields mismatch: %+v", resp.Override)
	}
}

func TestHandleConvoyWatch_POST_VerboseMode_PersistsAndAudits(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	body := `{"mode":"verbose","operator":"jake","reason":"chasing flake"}`
	rec := callConvoyWatch(db, http.MethodPost, "", body, 9, []string{"watch"})
	if rec.Code != 200 {
		t.Fatalf("status: %d (body=%s)", rec.Code, rec.Body.String())
	}
	got, err := store.GetConvoyNotificationOverride(db, 9)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Mode != "verbose" || got.SetBy != "jake" || got.Reason != "chasing flake" {
		t.Errorf("row mismatch: %+v", got)
	}
	// Audit row landed.
	entries := store.ListAuditLog(db, 10)
	found := false
	for _, e := range entries {
		if e.Action == "convoy-watch-set" && e.TaskID == 9 && e.Actor == "jake" {
			found = true
			if !strings.Contains(e.Detail, "mode=verbose") || !strings.Contains(e.Detail, "chasing flake") {
				t.Errorf("audit detail missing fields: %q", e.Detail)
			}
		}
	}
	if !found {
		t.Errorf("expected convoy-watch-set audit row, got %+v", entries)
	}
}

func TestHandleConvoyWatch_POST_CustomJSON_ValidatesShape(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	cases := []struct {
		name     string
		body     string
		wantCode int
	}{
		{
			name:     "valid custom_json",
			body:     `{"mode":"custom_json","custom_json":"{\"convoy_progress\":\"slack\",\"*\":\"off\"}","operator":"jake","reason":"why"}`,
			wantCode: 200,
		},
		{
			name:     "malformed json",
			body:     `{"mode":"custom_json","custom_json":"not-json","operator":"jake","reason":"why"}`,
			wantCode: 400,
		},
		{
			name:     "invalid setting value",
			body:     `{"mode":"custom_json","custom_json":"{\"x\":\"loud\"}","operator":"jake","reason":"why"}`,
			wantCode: 400,
		},
		{
			name:     "missing custom_json",
			body:     `{"mode":"custom_json","operator":"jake","reason":"why"}`,
			wantCode: 400,
		},
		{
			name:     "empty map",
			body:     `{"mode":"custom_json","custom_json":"{}","operator":"jake","reason":"why"}`,
			wantCode: 400,
		},
	}
	for i, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			convoyID := 100 + i
			rec := callConvoyWatch(db, http.MethodPost, "", c.body, convoyID, []string{"watch"})
			if rec.Code != c.wantCode {
				t.Errorf("got %d want %d (body=%s)", rec.Code, c.wantCode, rec.Body.String())
			}
		})
	}
}

func TestHandleConvoyWatch_POST_RejectsBadInputs(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	cases := []struct {
		name     string
		body     string
		wantCode int
	}{
		{"missing operator", `{"mode":"verbose","reason":"x"}`, 400},
		{"missing reason", `{"mode":"verbose","operator":"jake"}`, 400},
		{"invalid mode", `{"mode":"loud","operator":"jake","reason":"x"}`, 400},
		{"malformed body", `not json`, 400},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := callConvoyWatch(db, http.MethodPost, "", c.body, 1, []string{"watch"})
			if rec.Code != c.wantCode {
				t.Errorf("got %d want %d (body=%s)", rec.Code, c.wantCode, rec.Body.String())
			}
		})
	}
}

func TestHandleConvoyWatch_POSTClear_DeletesRowAndAudits(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	if err := store.UpsertConvoyNotificationOverride(db, store.ConvoyNotificationOverride{
		ConvoyID: 5, Mode: "verbose", SetBy: "jake", Reason: "x",
	}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	body := `{"operator":"jake","reason":"no longer needed"}`
	rec := callConvoyWatch(db, http.MethodPost, "", body, 5, []string{"watch", "clear"})
	if rec.Code != 200 {
		t.Fatalf("status: %d (body=%s)", rec.Code, rec.Body.String())
	}
	_, err := store.GetConvoyNotificationOverride(db, 5)
	if !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("expected ErrNoRows after clear, got %v", err)
	}
	entries := store.ListAuditLog(db, 10)
	found := false
	for _, e := range entries {
		if e.Action == "convoy-watch-clear" && e.TaskID == 5 {
			found = true
		}
	}
	if !found {
		t.Errorf("expected convoy-watch-clear audit row, got %+v", entries)
	}
}

func TestHandleConvoyWatch_POSTClear_RejectsMissingOperator(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	rec := callConvoyWatch(db, http.MethodPost, "", `{"reason":"x"}`, 1, []string{"watch", "clear"})
	if rec.Code != 400 {
		t.Errorf("got %d want 400 (body=%s)", rec.Code, rec.Body.String())
	}
}

func TestHandleConvoyWatch_MethodNotAllowed(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	// PUT to /watch
	rec := callConvoyWatch(db, http.MethodPut, "", `{}`, 1, []string{"watch"})
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("got %d want 405 on PUT /watch (body=%s)", rec.Code, rec.Body.String())
	}
	// GET on /watch/clear
	rec2 := callConvoyWatch(db, http.MethodGet, "", "", 1, []string{"watch", "clear"})
	if rec2.Code != http.StatusMethodNotAllowed {
		t.Errorf("got %d want 405 on GET /watch/clear (body=%s)", rec2.Code, rec2.Body.String())
	}
}

func TestHandleConvoyWatch_NonWatchPathReturnsFalse(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	// Send something that isn't /watch — handler must return false so
	// the dispatcher can fall through to its default 404.
	req := httptest.NewRequest(http.MethodGet, "/api/convoys/1/stages", nil)
	rec := httptest.NewRecorder()
	if handleConvoyWatch(db, rec, req, 1, []string{"stages"}) {
		t.Errorf("handleConvoyWatch returned true for non-watch path")
	}
}
