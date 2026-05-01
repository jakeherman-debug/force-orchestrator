package dashboard

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"force-orchestrator/internal/store"
)

func TestDrillConvoyHandler(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	_, _ = db.Exec(`INSERT INTO Convoys (id, name, status) VALUES (10, 'c10', 'Active')`)
	_, _ = db.Exec(`INSERT INTO BountyBoard (id, type, payload, status, convoy_id) VALUES (10, 'a', 'p', 'P', 10)`)
	_, _ = db.Exec(`INSERT INTO LLMCallTranscripts (task_id, agent, prompt_version, call_started_at, system_prompt, user_prompt) VALUES (10, 'captain', 'v1', '2026-01-01 10:00:00', 's', 'u')`)

	req := httptest.NewRequest(http.MethodGet, "/api/drill/convoy/10", nil)
	rr := httptest.NewRecorder()
	handleDrillConvoy(db)(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status %d body %s", rr.Code, rr.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	events, ok := resp["events"].([]any)
	if !ok || len(events) == 0 {
		t.Fatalf("expected events array, got %v", resp)
	}
}

func TestDrillConvoySpendHandler(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	_, _ = db.Exec(`INSERT INTO Convoys (id, name) VALUES (11, 'c11')`)
	_, _ = db.Exec(`INSERT INTO BountyBoard (id, type, payload, status, convoy_id) VALUES (11, 'a', 'p', 'P', 11)`)
	_, _ = db.Exec(`INSERT INTO LLMCallTranscripts (task_id, agent, prompt_version, call_started_at, system_prompt, user_prompt, cost_usd) VALUES (11, 'captain', 'v1', '2026-01-01 10:00:00', 's', 'u', 0.05)`)

	req := httptest.NewRequest(http.MethodGet, "/api/drill/convoy/11/spend", nil)
	rr := httptest.NewRecorder()
	handleDrillConvoy(db)(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status %d body %s", rr.Code, rr.Body.String())
	}
	var resp map[string]any
	json.Unmarshal(rr.Body.Bytes(), &resp)
	if _, ok := resp["spend"]; !ok {
		t.Errorf("expected spend key, got %v", resp)
	}
}

func TestDrillTaskHandler(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	_, _ = db.Exec(`INSERT INTO TaskHistory (task_id, attempt, agent, session_id, claude_output, outcome, created_at) VALUES (44, 1, 'astromech', 's', 'o', 'Completed', '2026-01-01 09:00:00')`)
	req := httptest.NewRequest(http.MethodGet, "/api/drill/task/44", nil)
	rr := httptest.NewRecorder()
	handleDrillTask(db)(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status %d", rr.Code)
	}
}

func TestDrillEventHandler(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	_, _ = db.Exec(`INSERT INTO LLMCallTranscripts (id, task_id, agent, prompt_version, call_started_at, system_prompt, user_prompt) VALUES (88, 1, 'captain', 'v1', '2026-01-01 10:00:00', 's', 'u')`)
	req := httptest.NewRequest(http.MethodGet, "/api/drill/event/llm_call/88", nil)
	rr := httptest.NewRecorder()
	handleDrillEvent(db)(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status %d body %s", rr.Code, rr.Body.String())
	}
	var resp map[string]any
	json.Unmarshal(rr.Body.Bytes(), &resp)
	if resp["agent"] != "captain" {
		t.Errorf("expected captain, got %v", resp)
	}
}

func TestDrillEventInvalidKindNotFound(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	req := httptest.NewRequest(http.MethodGet, "/api/drill/event/invented/1", nil)
	rr := httptest.NewRecorder()
	handleDrillEvent(db)(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", rr.Code)
	}
}

func TestDrillConvoyMethodNotAllowed(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	req := httptest.NewRequest(http.MethodPost, "/api/drill/convoy/1", nil)
	rr := httptest.NewRecorder()
	handleDrillConvoy(db)(rr, req)
	if rr.Code != http.StatusMethodNotAllowed {
		t.Errorf("expected 405, got %d", rr.Code)
	}
}
