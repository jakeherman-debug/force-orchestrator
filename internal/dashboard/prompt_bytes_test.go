package dashboard

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"force-orchestrator/internal/store"
)

// TestHandlePromptBytes_AggregatesByAgent fixtures a multi-source,
// multi-agent attribution set and asserts the dashboard handler
// returns the per-agent breakdown ordered descending by bytes.
func TestHandlePromptBytes_AggregatesByAgent(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	// captain: 2 calls totalling 7000 bytes split file_read 5000 + claude_md 2000
	if err := store.RecordSourceTags(db, 1, "captain", store.NowSQLite(), []store.SourceContribution{
		{SourceTag: "file_read", Bytes: 3000},
		{SourceTag: "claude_md", Bytes: 1000},
	}); err != nil {
		t.Fatalf("captain rec1: %v", err)
	}
	if err := store.RecordSourceTags(db, 1, "captain", store.NowSQLite(), []store.SourceContribution{
		{SourceTag: "file_read", Bytes: 2000},
		{SourceTag: "claude_md", Bytes: 1000},
	}); err != nil {
		t.Fatalf("captain rec2: %v", err)
	}
	// medic: 1 call, 800 bytes fleet_rules
	if err := store.RecordSourceTags(db, 2, "medic", store.NowSQLite(), []store.SourceContribution{
		{SourceTag: "fleet_rules", Bytes: 800},
	}); err != nil {
		t.Fatalf("medic rec: %v", err)
	}

	r := httptest.NewRequest(http.MethodGet, "/api/prompt-bytes", nil)
	w := httptest.NewRecorder()
	handlePromptBytes(db)(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("unexpected Content-Type: %s", ct)
	}

	var resp struct {
		WindowSince string `json:"window_since"`
		WindowHours int    `json:"window_hours"`
		Agents      []struct {
			Agent      string `json:"agent"`
			Calls      int    `json:"calls"`
			TotalBytes int    `json:"total_bytes"`
			BySource   []struct {
				SourceTag string `json:"source_tag"`
				Bytes     int    `json:"bytes"`
				Pct       int    `json:"pct"`
			} `json:"by_source"`
		} `json:"agents"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v\nbody=%s", err, w.Body.String())
	}
	if resp.WindowHours != 168 {
		t.Errorf("expected default window 168h, got %d", resp.WindowHours)
	}
	if len(resp.Agents) != 2 {
		t.Fatalf("expected 2 agents, got %d", len(resp.Agents))
	}

	// captain first (alpha sort by name)
	captain := resp.Agents[0]
	if captain.Agent != "captain" {
		t.Errorf("expected captain first, got %q", captain.Agent)
	}
	if captain.TotalBytes != 7000 {
		t.Errorf("captain total: got %d, want 7000", captain.TotalBytes)
	}
	// file_read should be the top source (5000 > claude_md 2000)
	if len(captain.BySource) == 0 || captain.BySource[0].SourceTag != "file_read" {
		t.Errorf("expected file_read first, got %+v", captain.BySource)
	}
	// pct math: 5000/7000 ≈ 71%
	if captain.BySource[0].Pct < 60 || captain.BySource[0].Pct > 80 {
		t.Errorf("file_read pct out of range: got %d", captain.BySource[0].Pct)
	}

	medic := resp.Agents[1]
	if medic.Agent != "medic" || medic.TotalBytes != 800 {
		t.Errorf("medic mismatch: %+v", medic)
	}
}

// TestHandlePromptBytes_RespectsSinceHoursOverride verifies the query
// parameter clamps to 1..720 hours.
func TestHandlePromptBytes_RespectsSinceHoursOverride(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	r := httptest.NewRequest(http.MethodGet, "/api/prompt-bytes?since_hours=24", nil)
	w := httptest.NewRecorder()
	handlePromptBytes(db)(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var resp struct {
		WindowHours int `json:"window_hours"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.WindowHours != 24 {
		t.Errorf("expected window_hours=24, got %d", resp.WindowHours)
	}

	// Bogus value falls back to default.
	r = httptest.NewRequest(http.MethodGet, "/api/prompt-bytes?since_hours=-5", nil)
	w = httptest.NewRecorder()
	handlePromptBytes(db)(w, r)
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode neg: %v", err)
	}
	if resp.WindowHours != 168 {
		t.Errorf("expected fallback 168h, got %d", resp.WindowHours)
	}
}

// TestHandlePromptBytes_EmptyDB returns an empty-but-valid JSON body
// (no panic, no nil-slice nil JSON).
func TestHandlePromptBytes_EmptyDB(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	r := httptest.NewRequest(http.MethodGet, "/api/prompt-bytes", nil)
	w := httptest.NewRecorder()
	handlePromptBytes(db)(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if !json.Valid(w.Body.Bytes()) {
		t.Errorf("response body is not valid JSON: %q", w.Body.String())
	}
	var resp struct {
		Agents []any `json:"agents"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Agents) != 0 {
		t.Errorf("expected empty agents, got %d", len(resp.Agents))
	}
}
