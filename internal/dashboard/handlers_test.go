package dashboard

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"force-orchestrator/internal/agents"
	"force-orchestrator/internal/store"
)

// ── handleHealthz ─────────────────────────────────────────────────────────────

func TestHandleHealthz_ReturnsOK(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	w := httptest.NewRecorder()
	handleHealthz(w, r)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("unexpected Content-Type: %s", ct)
	}
	var got map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("response is not JSON: %v", err)
	}
	if got["status"] != "ok" {
		t.Errorf("expected status:ok, got %v", got["status"])
	}
	if _, ok := got["ts"]; !ok {
		t.Error("missing ts field")
	}
}

// ── handleStatus ─────────────────────────────────────────────────────────────

func TestHandleStatus_EmptyDB(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	r := httptest.NewRequest(http.MethodGet, "/api/status", nil)
	w := httptest.NewRecorder()
	handleStatus(db)(w, r)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}
	var s DashboardStatus
	if err := json.Unmarshal(w.Body.Bytes(), &s); err != nil {
		t.Fatalf("response is not valid JSON: %v", err)
	}
	if s.Timestamp == "" {
		t.Error("expected non-empty timestamp")
	}
	if s.Tasks == nil {
		t.Error("expected tasks map (not nil)")
	}
	if s.OpenEscalations != 0 || s.HighEscalations != 0 {
		t.Errorf("expected zero escalation counts, got open=%d high=%d", s.OpenEscalations, s.HighEscalations)
	}
}

func TestHandleStatus_WithTasksAndEscalations(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	store.AddBounty(db, 0, "Feature", "task 1")
	store.AddBounty(db, 0, "Feature", "task 2")
	id := store.AddBounty(db, 0, "Feature", "task 3")
	db.Exec(`UPDATE BountyBoard SET status = 'Completed' WHERE id = ?`, id)

	db.Exec(`INSERT INTO Escalations (task_id, severity, message, status) VALUES (1, 'HIGH', 'critical', 'Open')`)
	db.Exec(`INSERT INTO Escalations (task_id, severity, message, status) VALUES (2, 'LOW', 'minor', 'Open')`)

	r := httptest.NewRequest(http.MethodGet, "/api/status", nil)
	w := httptest.NewRecorder()
	handleStatus(db)(w, r)

	var s DashboardStatus
	if err := json.Unmarshal(w.Body.Bytes(), &s); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if s.Tasks["Pending"] != 2 {
		t.Errorf("expected 2 Pending tasks, got %d", s.Tasks["Pending"])
	}
	if s.Tasks["Completed"] != 1 {
		t.Errorf("expected 1 Completed task, got %d", s.Tasks["Completed"])
	}
	if s.OpenEscalations != 2 {
		t.Errorf("expected 2 open escalations, got %d", s.OpenEscalations)
	}
	if s.HighEscalations != 1 {
		t.Errorf("expected 1 high escalation, got %d", s.HighEscalations)
	}
	if s.DaemonRunning {
		t.Error("expected daemon not running in test environment")
	}
}

func TestHandleStatus_Estopped(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	agents.SetEstop(db, true)

	r := httptest.NewRequest(http.MethodGet, "/api/status", nil)
	w := httptest.NewRecorder()
	handleStatus(db)(w, r)

	var s DashboardStatus
	if err := json.Unmarshal(w.Body.Bytes(), &s); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if !s.Estopped {
		t.Error("expected estopped:true")
	}
}

func TestHandleStatus_CORS(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	r := httptest.NewRequest(http.MethodGet, "/api/status", nil)
	w := httptest.NewRecorder()
	handleStatus(db)(w, r)

	if w.Header().Get("Access-Control-Allow-Origin") != "*" {
		t.Error("missing CORS header")
	}
}

func TestHandleStatus_ActiveConvoys(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	db.Exec(`INSERT INTO Convoys (name, status, created_at) VALUES ('convoy-1', 'Active', datetime('now'))`)
	db.Exec(`INSERT INTO Convoys (name, status, created_at) VALUES ('convoy-2', 'Active', datetime('now'))`)
	db.Exec(`INSERT INTO Convoys (name, status, created_at) VALUES ('convoy-3', 'Completed', datetime('now'))`)

	r := httptest.NewRequest(http.MethodGet, "/api/status", nil)
	w := httptest.NewRecorder()
	handleStatus(db)(w, r)

	var s DashboardStatus
	if err := json.Unmarshal(w.Body.Bytes(), &s); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if s.ActiveConvoys != 2 {
		t.Errorf("expected 2 active convoys, got %d", s.ActiveConvoys)
	}
}

// ── handleTasks ───────────────────────────────────────────────────────────────

func TestHandleTasks_EmptyDB(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	r := httptest.NewRequest(http.MethodGet, "/api/tasks", nil)
	w := httptest.NewRecorder()
	handleTasks(db)(w, r)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}
	var tasks []DashboardTask
	if err := json.Unmarshal(w.Body.Bytes(), &tasks); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if len(tasks) != 0 {
		t.Errorf("expected empty slice, got %d tasks", len(tasks))
	}
}

func TestHandleTasks_ReturnsAllTasks(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	store.AddBounty(db, 0, "Feature", "task A")
	id := store.AddBounty(db, 0, "Bug", "task B")
	db.Exec(`UPDATE BountyBoard SET status = 'Failed' WHERE id = ?`, id)

	r := httptest.NewRequest(http.MethodGet, "/api/tasks", nil)
	w := httptest.NewRecorder()
	handleTasks(db)(w, r)

	var tasks []DashboardTask
	if err := json.Unmarshal(w.Body.Bytes(), &tasks); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if len(tasks) != 2 {
		t.Errorf("expected 2 tasks, got %d", len(tasks))
	}
}

func TestHandleTasks_StatusFilter(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	store.AddBounty(db, 0, "Feature", "task pending")
	id := store.AddBounty(db, 0, "Bug", "task failed")
	db.Exec(`UPDATE BountyBoard SET status = 'Failed' WHERE id = ?`, id)

	r := httptest.NewRequest(http.MethodGet, "/api/tasks?status=Pending", nil)
	w := httptest.NewRecorder()
	handleTasks(db)(w, r)

	var tasks []DashboardTask
	if err := json.Unmarshal(w.Body.Bytes(), &tasks); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if len(tasks) != 1 {
		t.Errorf("expected 1 Pending task, got %d", len(tasks))
	}
	if tasks[0].Status != "Pending" {
		t.Errorf("unexpected status: %s", tasks[0].Status)
	}
}

func TestHandleTasks_PayloadTruncation(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	longPayload := strings.Repeat("x", 400)
	store.AddBounty(db, 0, "Feature", longPayload)

	r := httptest.NewRequest(http.MethodGet, "/api/tasks", nil)
	w := httptest.NewRecorder()
	handleTasks(db)(w, r)

	var tasks []DashboardTask
	if err := json.Unmarshal(w.Body.Bytes(), &tasks); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if len(tasks) == 0 {
		t.Fatal("expected a task")
	}
	if len(tasks[0].Payload) >= 400 {
		t.Errorf("expected payload to be truncated, got length %d", len(tasks[0].Payload))
	}
	if !strings.HasSuffix(tasks[0].Payload, "…") {
		t.Error("expected truncated payload to end with ellipsis")
	}
}

func TestHandleTasks_CORS(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	r := httptest.NewRequest(http.MethodGet, "/api/tasks", nil)
	w := httptest.NewRecorder()
	handleTasks(db)(w, r)

	if w.Header().Get("Access-Control-Allow-Origin") != "*" {
		t.Error("missing CORS header")
	}
}

// ── task cancel (Cancelled status) ───────────────────────────────────────────

func TestHandleTasksSubroutes_Cancel(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	id := store.AddBounty(db, 0, "Feature", "some task")

	r := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/api/tasks/%d/cancel", id), nil)
	w := httptest.NewRecorder()
	handleTasksSubroutes(db)(w, r)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var status string
	db.QueryRow(`SELECT status FROM BountyBoard WHERE id = ?`, id).Scan(&status)
	if status != "Cancelled" {
		t.Errorf("expected status Cancelled, got %s", status)
	}
}

func TestHandleTasksSubroutes_CancelCompleted(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	id := store.AddBounty(db, 0, "Feature", "done task")
	db.Exec(`UPDATE BountyBoard SET status = 'Completed' WHERE id = ?`, id)

	r := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/api/tasks/%d/cancel", id), nil)
	w := httptest.NewRecorder()
	handleTasksSubroutes(db)(w, r)

	if w.Code != http.StatusConflict {
		t.Errorf("expected 409, got %d", w.Code)
	}
}

// ── handleTasksSubroutes — retry ─────────────────────────────────────────────

func TestHandleTasksSubroutes_Retry(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	id := store.AddBounty(db, 0, "Feature", "fix something")
	db.Exec(`UPDATE BountyBoard SET status = 'Failed', error_log = 'oops', retry_count = 3 WHERE id = ?`, id)

	r := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/api/tasks/%d/retry", id), nil)
	w := httptest.NewRecorder()
	handleTasksSubroutes(db)(w, r)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var status string
	db.QueryRow(`SELECT status FROM BountyBoard WHERE id = ?`, id).Scan(&status)
	if status != "Pending" {
		t.Errorf("expected status Pending after retry, got %s", status)
	}
}

func TestHandleTasksSubroutes_RetryEscalated(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	id := store.AddBounty(db, 0, "Feature", "escalated task")
	db.Exec(`UPDATE BountyBoard SET status = 'Escalated' WHERE id = ?`, id)

	r := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/api/tasks/%d/retry", id), nil)
	w := httptest.NewRecorder()
	handleTasksSubroutes(db)(w, r)

	var status string
	db.QueryRow(`SELECT status FROM BountyBoard WHERE id = ?`, id).Scan(&status)
	if status != "Pending" {
		t.Errorf("expected Escalated task to be reset to Pending, got %s", status)
	}
}

func TestHandleTasksSubroutes_RetryMethodNotAllowed(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	r := httptest.NewRequest(http.MethodGet, "/api/tasks/1/retry", nil)
	w := httptest.NewRecorder()
	handleTasksSubroutes(db)(w, r)

	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404 for GET on action route, got %d", w.Code)
	}
}

func TestHandleTasksSubroutes_BadID(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	r := httptest.NewRequest(http.MethodPost, "/api/tasks/abc/retry", nil)
	w := httptest.NewRecorder()
	handleTasksSubroutes(db)(w, r)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", w.Code)
	}
}

func TestHandleTasksSubroutes_UnknownAction(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	r := httptest.NewRequest(http.MethodPost, "/api/tasks/1/unknown-action", nil)
	w := httptest.NewRecorder()
	handleTasksSubroutes(db)(w, r)

	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", w.Code)
	}
}

func TestHandleTasksSubroutes_DoesNotResetLocked(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	id := store.AddBounty(db, 0, "Feature", "active task")
	db.Exec(`UPDATE BountyBoard SET status = 'Locked', owner = 'astromech-1' WHERE id = ?`, id)

	r := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/api/tasks/%d/retry", id), nil)
	w := httptest.NewRecorder()
	handleTasksSubroutes(db)(w, r)

	// Retry only affects Failed/Escalated — Locked task stays Locked
	var status string
	db.QueryRow(`SELECT status FROM BountyBoard WHERE id = ?`, id).Scan(&status)
	if status != "Locked" {
		t.Errorf("expected Locked task to remain Locked, got %s", status)
	}
}

// ── handleEscalationsSubroutes — ack ─────────────────────────────────────────

func TestHandleEscalationsSubroutes_Ack(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	taskID := store.AddBounty(db, 0, "Feature", "some task")
	db.Exec(`INSERT INTO Escalations (task_id, severity, message, status) VALUES (?, 'LOW', 'problem', 'Open')`, taskID)
	var escID int
	db.QueryRow(`SELECT id FROM Escalations WHERE task_id = ?`, taskID).Scan(&escID)

	r := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/api/escalations/%d/ack", escID), nil)
	w := httptest.NewRecorder()
	handleEscalationsSubroutes(db)(w, r)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var status string
	db.QueryRow(`SELECT status FROM Escalations WHERE id = ?`, escID).Scan(&status)
	if status != "Acknowledged" {
		t.Errorf("expected escalation to be Acknowledged, got %s", status)
	}
}

func TestHandleEscalationsSubroutes_ZeroID(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	r := httptest.NewRequest(http.MethodPost, "/api/escalations/0/ack", nil)
	w := httptest.NewRecorder()
	handleEscalationsSubroutes(db)(w, r)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", w.Code)
	}
}

func TestHandleEscalationsSubroutes_UnknownAction(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	r := httptest.NewRequest(http.MethodPost, "/api/escalations/1/unknown", nil)
	w := httptest.NewRecorder()
	handleEscalationsSubroutes(db)(w, r)

	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", w.Code)
	}
}

// ── handleHolonetStream ───────────────────────────────────────────────────────

func TestHandleHolonetStream_MissingFile(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "nonexistent.jsonl")

	r := httptest.NewRequest(http.MethodGet, "/api/events", nil)
	w := httptest.NewRecorder()
	handleHolonetStream(logPath)(w, r)

	body := w.Body.String()
	if !strings.Contains(body, "holonet.jsonl not found") {
		t.Errorf("expected error SSE event, got: %s", body)
	}
}

func TestHandleHolonetStream_ContextCancelReturns(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "events.jsonl")
	if err := os.WriteFile(logPath, []byte(""), 0644); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // pre-cancel so handler exits on first context check

	r := httptest.NewRequest(http.MethodGet, "/api/events", nil).WithContext(ctx)
	w := httptest.NewRecorder()

	done := make(chan struct{})
	go func() {
		handleHolonetStream(logPath)(w, r)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("handleHolonetStream did not return after context cancellation")
	}
}

func TestHandleHolonetStream_SSEHeaders(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "events.jsonl")
	if err := os.WriteFile(logPath, []byte(""), 0644); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	r := httptest.NewRequest(http.MethodGet, "/api/events", nil).WithContext(ctx)
	w := httptest.NewRecorder()
	handleHolonetStream(logPath)(w, r)

	if ct := w.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("expected text/event-stream, got %s", ct)
	}
	if w.Header().Get("Cache-Control") != "no-cache" {
		t.Error("expected Cache-Control: no-cache")
	}
	if w.Header().Get("Access-Control-Allow-Origin") != "*" {
		t.Error("missing CORS header")
	}
}

// ── handleAdd — auto-classification ──────────────────────────────────────────

func TestHandleAdd_AutoTypeInsertsClassifying(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	body := `{"type":"auto","payload":"classify this task","repo":"","priority":0}`
	r := httptest.NewRequest(http.MethodPost, "/api/add", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handleAdd(db)(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var status string
	db.QueryRow(`SELECT status FROM BountyBoard WHERE id = 1`).Scan(&status)
	if status != "Classifying" {
		t.Errorf("expected status=Classifying for auto type, got %q", status)
	}
}

func TestHandleAdd_EmptyTypeInsertsClassifying(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	body := `{"type":"","payload":"no type specified","repo":"","priority":0}`
	r := httptest.NewRequest(http.MethodPost, "/api/add", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handleAdd(db)(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var status string
	db.QueryRow(`SELECT status FROM BountyBoard WHERE id = 1`).Scan(&status)
	if status != "Classifying" {
		t.Errorf("expected status=Classifying for empty type, got %q", status)
	}
}
