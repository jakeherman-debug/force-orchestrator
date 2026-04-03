package telemetry

import (
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"force-orchestrator/internal/store"
	"force-orchestrator/internal/util"
)

// ── NewSessionID ──────────────────────────────────────────────────────────────

func TestNewSessionID(t *testing.T) {
	id := NewSessionID()
	if len(id) != 16 {
		t.Errorf("expected 16-char hex session ID, got %q (len=%d)", id, len(id))
	}
	id2 := NewSessionID()
	if id == id2 {
		t.Error("expected unique session IDs")
	}
}

// ── EmitEvent ─────────────────────────────────────────────────────────────────

func TestEmitEvent_NoFile(t *testing.T) {
	// Should not panic even with no telemetry file initialized
	EmitEvent(TelemetryEvent{
		EventType: "test_event",
		Payload:   map[string]any{"key": "value"},
	})
}

func TestEmitEvent_WithFile(t *testing.T) {
	dir := t.TempDir()
	orig, _ := os.Getwd()
	os.Chdir(dir)
	defer func() {
		os.Chdir(orig)
		telemetryMu.Lock()
		if telemetryFile != nil {
			telemetryFile.Close()
			telemetryFile = nil
		}
		telemetryMu.Unlock()
	}()

	InitTelemetry()

	EmitEvent(TelemetryEvent{
		EventType: "test_with_file",
		Agent:     "R2-D2",
		TaskID:    42,
		Payload:   map[string]any{"key": "value"},
	})

	// holonet.jsonl should contain the event
	data, err := os.ReadFile("holonet.jsonl")
	if err != nil {
		t.Fatalf("could not read holonet.jsonl: %v", err)
	}
	if !strings.Contains(string(data), "test_with_file") {
		t.Errorf("expected event in holonet.jsonl, got: %s", data)
	}
}

func TestEmitEvent_WithOTLPEndpoint(t *testing.T) {
	dir := t.TempDir()
	orig, _ := os.Getwd()
	os.Chdir(dir)
	defer func() {
		os.Chdir(orig)
		telemetryMu.Lock()
		if telemetryFile != nil {
			telemetryFile.Close()
			telemetryFile = nil
		}
		otlpEndpoint = ""
		otlpHTTPClient = nil
		telemetryMu.Unlock()
	}()

	InitTelemetry()

	// Configure OTLP with a dead endpoint — the goroutine will fail to POST
	// but the code path that launches it IS covered.
	telemetryMu.Lock()
	otlpEndpoint = "http://127.0.0.1:19999/v1/logs" // nothing listening
	otlpHTTPClient = &http.Client{Timeout: 50 * time.Millisecond}
	telemetryMu.Unlock()

	EmitEvent(TelemetryEvent{
		EventType: "test_otlp_launch",
		Agent:     "R2-D2",
		TaskID:    1,
		SessionID: "abc123",
	})

	// Let the goroutine attempt (and fail) its HTTP POST
	time.Sleep(100 * time.Millisecond)
}

func TestEmitEvent_MarshalError(t *testing.T) {
	// Channels cannot be marshaled → json.Marshal returns an error → early return
	event := TelemetryEvent{
		EventType: "test",
		Payload:   map[string]any{"bad": make(chan int)},
	}
	oldFile := telemetryFile
	defer func() { telemetryFile = oldFile }()
	telemetryFile = nil
	// Must not panic
	EmitEvent(event)
}

// ── Event constructors ────────────────────────────────────────────────────────

func TestEventTaskClaimed(t *testing.T) {
	e := EventTaskClaimed("sess1", "R2-D2", 42, "api", "add endpoint")
	if e.EventType != "task_claimed" {
		t.Errorf("expected task_claimed, got %q", e.EventType)
	}
	if e.TaskID != 42 {
		t.Errorf("expected task_id=42, got %d", e.TaskID)
	}
	if e.Agent != "R2-D2" {
		t.Errorf("expected agent R2-D2, got %q", e.Agent)
	}
}

func TestEventTaskCompleted(t *testing.T) {
	e := EventTaskCompleted("sess1", "BB-8", 7)
	if e.EventType != "task_completed" {
		t.Errorf("expected task_completed, got %q", e.EventType)
	}
}

func TestEventTaskFailed(t *testing.T) {
	e := EventTaskFailed("sess1", "R2-D2", 5, "compilation error")
	if e.EventType != "task_failed" {
		t.Errorf("expected task_failed, got %q", e.EventType)
	}
	if e.Payload["reason"] != "compilation error" {
		t.Errorf("expected reason in payload, got %v", e.Payload)
	}
}

func TestEventTaskEscalated(t *testing.T) {
	e := EventTaskEscalated("sess1", "R2-D2", 3, store.SeverityHigh, "needs human input")
	if e.EventType != "task_escalated" {
		t.Errorf("expected task_escalated, got %q", e.EventType)
	}
}

func TestEventTaskSharded(t *testing.T) {
	e := EventTaskSharded("sess1", "R2-D2", 10)
	if e.EventType != "task_sharded" {
		t.Errorf("expected task_sharded, got %q", e.EventType)
	}
}

func TestEventCouncilRuling_Approved(t *testing.T) {
	e := EventCouncilRuling("sess1", "Yoda", 5, true, "")
	if e.EventType != "council_approved" {
		t.Errorf("expected council_approved, got %q", e.EventType)
	}
}

func TestEventCouncilRuling_Rejected(t *testing.T) {
	e := EventCouncilRuling("sess1", "Yoda", 5, false, "missing tests")
	if e.EventType != "council_rejected" {
		t.Errorf("expected council_rejected, got %q", e.EventType)
	}
}

func TestEventEstop(t *testing.T) {
	eActive := EventEstop(true)
	if eActive.EventType != "estop_activated" {
		t.Errorf("expected estop_activated, got %q", eActive.EventType)
	}
	eCleared := EventEstop(false)
	if eCleared.EventType != "estop_cleared" {
		t.Errorf("expected estop_cleared, got %q", eCleared.EventType)
	}
}

func TestEventInfraFailure(t *testing.T) {
	e := EventInfraFailure("sess1", "R2-D2", 3, 2, "git error")
	if e.EventType != "infra_failure" {
		t.Errorf("expected infra_failure, got %q", e.EventType)
	}
}

func TestEventInquisitorReset(t *testing.T) {
	e := EventInquisitorReset([]int{1, 2, 3})
	if e.EventType != "inquisitor_reset" {
		t.Errorf("expected inquisitor_reset, got %q", e.EventType)
	}
	if e.Payload["count"] != 3 {
		t.Errorf("expected count=3, got %v", e.Payload["count"])
	}
}

func TestEventTaskDoneSignal(t *testing.T) {
	e := EventTaskDoneSignal("sess1", "R2-D2", 8)
	if e.EventType != "task_done_signal" {
		t.Errorf("expected task_done_signal, got %q", e.EventType)
	}
}

func TestEventRateLimited(t *testing.T) {
	e := EventRateLimited("sess1", "BB-8", 4, 2, 30*time.Second)
	if e.EventType != "rate_limited" {
		t.Errorf("expected rate_limited, got %q", e.EventType)
	}
}

func TestEventStallDetected(t *testing.T) {
	e := EventStallDetected(5, "R2-D2", "api", 25.5)
	if e.EventType != "stall_detected" {
		t.Errorf("expected stall_detected, got %q", e.EventType)
	}
}

// ── InitTelemetry ─────────────────────────────────────────────────────────────

func TestInitTelemetry_CreatesFile(t *testing.T) {
	dir := t.TempDir()
	orig, _ := os.Getwd()
	os.Chdir(dir)
	defer func() {
		os.Chdir(orig)
		// Reset global telemetry state
		telemetryMu.Lock()
		if telemetryFile != nil {
			telemetryFile.Close()
			telemetryFile = nil
		}
		telemetryMu.Unlock()
	}()

	InitTelemetry()

	// holonet.jsonl should be created
	if _, statErr := os.Stat("holonet.jsonl"); statErr != nil {
		t.Error("expected holonet.jsonl to be created by InitTelemetry")
	}

	// Should be able to emit an event after init
	EmitEvent(TelemetryEvent{EventType: "test_after_init"})
}

func TestInitTelemetry_OTLPEnabled(t *testing.T) {
	dir := t.TempDir()
	orig, _ := os.Getwd()
	os.Chdir(dir)
	t.Setenv("GT_OTEL_LOGS_URL", "http://localhost:4318/v1/logs")
	defer func() {
		os.Chdir(orig)
		telemetryMu.Lock()
		if telemetryFile != nil {
			telemetryFile.Close()
			telemetryFile = nil
		}
		otlpEndpoint = ""
		otlpHTTPClient = nil
		telemetryMu.Unlock()
	}()

	InitTelemetry()

	if otlpEndpoint == "" {
		t.Error("expected otlpEndpoint to be set when GT_OTEL_LOGS_URL is configured")
	}
	if otlpHTTPClient == nil {
		t.Error("expected otlpHTTPClient to be initialized")
	}
}

func TestInitTelemetry_ErrorPath(t *testing.T) {
	// Save & restore global telemetry state
	oldFile, oldLog, oldEndpoint, oldClient := telemetryFile, telemetryLog, otlpEndpoint, otlpHTTPClient
	defer func() {
		telemetryFile = oldFile
		telemetryLog = oldLog
		otlpEndpoint = oldEndpoint
		otlpHTTPClient = oldClient
	}()
	telemetryFile = nil

	orig, _ := os.Getwd()
	defer os.Chdir(orig)
	tmp := t.TempDir()
	os.Chdir(tmp)

	// Create "holonet.jsonl" as a directory so os.OpenFile fails with EISDIR
	if err := os.MkdirAll(filepath.Join(tmp, "holonet.jsonl"), 0755); err != nil {
		t.Fatal(err)
	}
	InitTelemetry()
	if telemetryLog == nil {
		t.Error("expected telemetryLog to be set even in error path")
	}
}

// ── sendOTLPLog ───────────────────────────────────────────────────────────────

func TestSendOTLPLog_WithServer(t *testing.T) {
	// Create a minimal HTTP test server that accepts OTLP logs
	received := make(chan struct{}, 1)
	server := &http.Server{}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/logs", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		received <- struct{}{}
	})
	server.Handler = mux

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skip("cannot listen on loopback: " + err.Error())
	}
	go server.Serve(ln)
	defer server.Close()

	addr := ln.Addr().String()
	event := TelemetryEvent{
		EventType: "test",
		Agent:     "R2-D2",
		TaskID:    1,
		SessionID: "s1",
		Timestamp: time.Now(),
	}
	rawEvent, _ := json.Marshal(event)

	client := &http.Client{Timeout: 2 * time.Second}
	oldClient := otlpHTTPClient
	oldEndpoint := otlpEndpoint
	otlpHTTPClient = client
	otlpEndpoint = "http://" + addr + "/v1/logs"
	defer func() {
		otlpHTTPClient = oldClient
		otlpEndpoint = oldEndpoint
	}()

	sendOTLPLog(event, rawEvent)

	select {
	case <-received:
		// success — resp.Body.Close() was called
	case <-time.After(2 * time.Second):
		t.Error("timeout waiting for OTLP server to receive request")
	}
}

// ── truncateStr ───────────────────────────────────────────────────────────────

func TestTruncateStr(t *testing.T) {
	if got := util.TruncateStr("hello world", 5); got != "hello…" {
		t.Errorf("got %q", got)
	}
	if got := util.TruncateStr("hi", 10); got != "hi" {
		t.Errorf("got %q", got)
	}
	if got := util.TruncateStr("", 5); got != "" {
		t.Errorf("got %q", got)
	}
}
