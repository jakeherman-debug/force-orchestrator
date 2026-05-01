package dashboard

// JIRA-from-UI — handler validation matrix tests for
// POST /api/feature/from-jira.
//
// LIVE_HAIKU_DISABLED is pinned to "1" by testmain_test.go so
// agents.QueueFeatureFromJira routes through its deterministic stub
// branch — no live MCP contact, no flaky tests.
//
// Coverage matrix:
//   - GET method  → 405
//   - body invalid JSON → 400
//   - empty ticket_id → 400
//   - malformed ticket_id (lowercase, no dash, weird chars) → 400
//   - priority out of range (negative, 10) → 400
//   - non-bool plan_only via type-mismatched JSON → 400 (decoder rejects)
//   - happy path: 200 with task_id + summary; row queued
//   - happy path with priority + plan_only

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHandleFeatureFromJira_RejectsGet(t *testing.T) {
	db := openDashTestDB(t)
	r := httptest.NewRequest(http.MethodGet, "/api/feature/from-jira", nil)
	w := httptest.NewRecorder()
	handleFeatureFromJira(db)(w, r)
	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("status: got %d want 405; body=%s", w.Code, w.Body.String())
	}
}

func TestHandleFeatureFromJira_InvalidJSON(t *testing.T) {
	db := openDashTestDB(t)
	r := httptest.NewRequest(http.MethodPost, "/api/feature/from-jira",
		bytes.NewReader([]byte("not-json{")))
	w := httptest.NewRecorder()
	handleFeatureFromJira(db)(w, r)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status: got %d want 400; body=%s", w.Code, w.Body.String())
	}
}

func TestHandleFeatureFromJira_EmptyTicket(t *testing.T) {
	db := openDashTestDB(t)
	body, _ := json.Marshal(map[string]any{"ticket_id": "", "priority": 0})
	r := httptest.NewRequest(http.MethodPost, "/api/feature/from-jira", bytes.NewReader(body))
	w := httptest.NewRecorder()
	handleFeatureFromJira(db)(w, r)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status: got %d want 400; body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "ticket_id required") {
		t.Errorf("expected 'ticket_id required' message, got %s", w.Body.String())
	}
}

func TestHandleFeatureFromJira_MalformedTicket(t *testing.T) {
	db := openDashTestDB(t)
	for _, badTicket := range []string{
		"abc-123",  // lowercase
		"ABC123",   // missing dash
		"ABC-",     // no number
		"-123",     // no project key
		"ABC-12X",  // letter in number
		"ABC 123",  // space
		"ABC-123;", // injection-shaped suffix
	} {
		body, _ := json.Marshal(map[string]any{"ticket_id": badTicket, "priority": 0})
		r := httptest.NewRequest(http.MethodPost, "/api/feature/from-jira", bytes.NewReader(body))
		w := httptest.NewRecorder()
		handleFeatureFromJira(db)(w, r)
		if w.Code != http.StatusBadRequest {
			t.Errorf("ticket=%q: status got %d want 400; body=%s",
				badTicket, w.Code, w.Body.String())
		}
	}
}

func TestHandleFeatureFromJira_PriorityOutOfRange(t *testing.T) {
	db := openDashTestDB(t)
	for _, p := range []int{-1, 10, 99, -100} {
		body, _ := json.Marshal(map[string]any{"ticket_id": "ABC-1", "priority": p})
		r := httptest.NewRequest(http.MethodPost, "/api/feature/from-jira", bytes.NewReader(body))
		w := httptest.NewRecorder()
		handleFeatureFromJira(db)(w, r)
		if w.Code != http.StatusBadRequest {
			t.Errorf("priority=%d: status got %d want 400; body=%s",
				p, w.Code, w.Body.String())
		}
	}
}

func TestHandleFeatureFromJira_NonBoolPlanOnlyRejected(t *testing.T) {
	db := openDashTestDB(t)
	// Hand-rolled body so plan_only is a non-bool — the JSON decoder
	// rejects this at the boundary before the handler even sees it.
	body := []byte(`{"ticket_id":"ABC-123","priority":0,"plan_only":"yes"}`)
	r := httptest.NewRequest(http.MethodPost, "/api/feature/from-jira", bytes.NewReader(body))
	w := httptest.NewRecorder()
	handleFeatureFromJira(db)(w, r)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status: got %d want 400; body=%s", w.Code, w.Body.String())
	}
}

func TestHandleFeatureFromJira_HappyPath(t *testing.T) {
	db := openDashTestDB(t)
	body, _ := json.Marshal(map[string]any{
		"ticket_id": "ABC-123",
		"priority":  0,
		"plan_only": false,
	})
	r := httptest.NewRequest(http.MethodPost, "/api/feature/from-jira", bytes.NewReader(body))
	w := httptest.NewRecorder()
	handleFeatureFromJira(db)(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200; body=%s", w.Code, w.Body.String())
	}
	var resp featureFromJiraResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v body=%s", err, w.Body.String())
	}
	if resp.TaskID == 0 {
		t.Errorf("task_id: got 0, want non-zero")
	}
	if resp.Summary == "" {
		t.Errorf("summary: got empty; want stub-shape body")
	}

	// Verify the row landed.
	var taskType, payload string
	row := db.QueryRow(`SELECT type, payload FROM BountyBoard WHERE id = ?`, resp.TaskID)
	if err := row.Scan(&taskType, &payload); err != nil {
		t.Fatalf("query queued row: %v", err)
	}
	if taskType != "Feature" {
		t.Errorf("type: got %q want Feature", taskType)
	}
	if !strings.Contains(payload, "[JIRA: ABC-123]") {
		t.Errorf("payload missing ticket marker: %q", payload)
	}
}

func TestHandleFeatureFromJira_HappyPathPriorityAndPlanOnly(t *testing.T) {
	db := openDashTestDB(t)
	body, _ := json.Marshal(map[string]any{
		"ticket_id": "TEAM-99",
		"priority":  3,
		"plan_only": true,
	})
	r := httptest.NewRequest(http.MethodPost, "/api/feature/from-jira", bytes.NewReader(body))
	w := httptest.NewRecorder()
	handleFeatureFromJira(db)(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200; body=%s", w.Code, w.Body.String())
	}
	var resp featureFromJiraResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}

	var priority int
	var payload string
	row := db.QueryRow(`SELECT priority, payload FROM BountyBoard WHERE id = ?`, resp.TaskID)
	if err := row.Scan(&priority, &payload); err != nil {
		t.Fatalf("query: %v", err)
	}
	if priority != 3 {
		t.Errorf("priority: got %d want 3", priority)
	}
	if !strings.HasPrefix(payload, "[PLAN_ONLY]\n") {
		t.Errorf("plan-only payload: got %q want [PLAN_ONLY]\\n prefix", payload)
	}
	if !strings.Contains(payload, "[JIRA: TEAM-99]") {
		t.Errorf("payload missing ticket marker: %q", payload)
	}
}

func TestHandleFeatureFromJira_PriorityZeroIsLegal(t *testing.T) {
	// priority=0 is the "leave default" sentinel, matching the CLI's
	// --priority handling. Distinct from the "out of range" rejection.
	db := openDashTestDB(t)
	body, _ := json.Marshal(map[string]any{"ticket_id": "ABC-1", "priority": 0})
	r := httptest.NewRequest(http.MethodPost, "/api/feature/from-jira", bytes.NewReader(body))
	w := httptest.NewRecorder()
	handleFeatureFromJira(db)(w, r)
	if w.Code != http.StatusOK {
		t.Errorf("priority=0 should be legal; got %d body=%s", w.Code, w.Body.String())
	}
}
