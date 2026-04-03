package telemetry

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"sync"
	"time"

	"force-orchestrator/internal/store"
	"force-orchestrator/internal/util"
)

// TelemetryEvent is a structured event written to holonet.jsonl.
// Every field is optional except Timestamp and EventType.
// Set GT_OTEL_LOGS_URL to also ship events to an OTLP HTTP/JSON log endpoint
// (e.g. http://localhost:4318/v1/logs for a local Grafana Alloy or OTel Collector).
type TelemetryEvent struct {
	Timestamp time.Time      `json:"timestamp"`
	SessionID string         `json:"session_id,omitempty"`
	Agent     string         `json:"agent,omitempty"`
	TaskID    int            `json:"task_id,omitempty"`
	EventType string         `json:"event_type"`
	Payload   map[string]any `json:"payload,omitempty"`
}

// NewSessionID generates a random ID to correlate all events from a single agent session.
func NewSessionID() string {
	b := make([]byte, 8)
	rand.Read(b)
	return fmt.Sprintf("%x", b)
}

var (
	telemetryFile   *os.File
	telemetryMu     sync.Mutex
	telemetryLog    *log.Logger
	otlpEndpoint    string       // set from GT_OTEL_LOGS_URL at init time
	otlpHTTPClient  *http.Client // shared, reused across goroutines
)

func InitTelemetry() {
	f, err := os.OpenFile("holonet.jsonl", os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		telemetryLog = log.New(os.Stderr, "[telemetry] ", log.LstdFlags)
		telemetryLog.Printf("WARNING: could not open holonet.jsonl: %v", err)
	} else {
		telemetryFile = f
	}
	telemetryLog = log.New(os.Stderr, "[telemetry] ", log.LstdFlags)

	if url := os.Getenv("GT_OTEL_LOGS_URL"); url != "" {
		otlpEndpoint = url
		otlpHTTPClient = &http.Client{Timeout: 5 * time.Second}
		telemetryLog.Printf("OTLP export enabled → %s", otlpEndpoint)
	}
}

// EmitEvent writes a structured event to holonet.jsonl and optionally to an
// OTLP HTTP endpoint if GT_OTEL_LOGS_URL is set. Thread-safe.
func EmitEvent(event TelemetryEvent) {
	if event.Timestamp.IsZero() {
		event.Timestamp = time.Now()
	}

	data, err := json.Marshal(event)
	if err != nil {
		return
	}

	telemetryMu.Lock()
	if telemetryFile != nil {
		fmt.Fprintf(telemetryFile, "%s\n", data)
	}
	telemetryMu.Unlock()

	if otlpEndpoint != "" {
		go sendOTLPLog(event, data)
	}
}

// sendOTLPLog ships a single event to an OTLP HTTP/JSON logs endpoint.
// The OTLP Logs JSON format: https://opentelemetry.io/docs/specs/otlp/#otlphttp
func sendOTLPLog(event TelemetryEvent, rawEvent []byte) {
	// Build attribute list from event fields
	attrs := []map[string]any{
		{"key": "event.type", "value": map[string]any{"stringValue": event.EventType}},
	}
	if event.Agent != "" {
		attrs = append(attrs, map[string]any{"key": "agent", "value": map[string]any{"stringValue": event.Agent}})
	}
	if event.SessionID != "" {
		attrs = append(attrs, map[string]any{"key": "session.id", "value": map[string]any{"stringValue": event.SessionID}})
	}
	if event.TaskID != 0 {
		attrs = append(attrs, map[string]any{"key": "task.id", "value": map[string]any{"intValue": event.TaskID}})
	}

	body := map[string]any{
		"resourceLogs": []map[string]any{
			{
				"resource": map[string]any{
					"attributes": []map[string]any{
						{"key": "service.name", "value": map[string]any{"stringValue": "force-orchestrator"}},
					},
				},
				"scopeLogs": []map[string]any{
					{
						"scope": map[string]any{"name": "force-orchestrator"},
						"logRecords": []map[string]any{
							{
								"timeUnixNano":   fmt.Sprintf("%d", event.Timestamp.UnixNano()),
								"severityNumber": 9, // INFO
								"severityText":   "INFO",
								"body":           map[string]any{"stringValue": string(rawEvent)},
								"attributes":     attrs,
							},
						},
					},
				},
			},
		},
	}

	payload, err := json.Marshal(body)
	if err != nil {
		return
	}

	resp, err := otlpHTTPClient.Post(otlpEndpoint, "application/json", bytes.NewReader(payload))
	if err != nil {
		// Silent failure — telemetry should never crash the agent
		return
	}
	resp.Body.Close()
}

// Standard event constructors — use these instead of building TelemetryEvent inline.

func EventTaskClaimed(sessionID, agent string, taskID int, repo, payload string) TelemetryEvent {
	return TelemetryEvent{
		SessionID: sessionID, Agent: agent, TaskID: taskID,
		EventType: "task_claimed",
		Payload:   map[string]any{"repo": repo, "payload_preview": util.TruncateStr(payload, 120)},
	}
}

func EventTaskCompleted(sessionID, agent string, taskID int) TelemetryEvent {
	return TelemetryEvent{
		SessionID: sessionID, Agent: agent, TaskID: taskID,
		EventType: "task_completed",
	}
}

func EventTaskFailed(sessionID, agent string, taskID int, reason string) TelemetryEvent {
	return TelemetryEvent{
		SessionID: sessionID, Agent: agent, TaskID: taskID,
		EventType: "task_failed",
		Payload:   map[string]any{"reason": reason},
	}
}

func EventTaskEscalated(sessionID, agent string, taskID int, severity store.EscalationSeverity, message string) TelemetryEvent {
	return TelemetryEvent{
		SessionID: sessionID, Agent: agent, TaskID: taskID,
		EventType: "task_escalated",
		Payload:   map[string]any{"severity": string(severity), "message": message},
	}
}

func EventTaskSharded(sessionID, agent string, taskID int) TelemetryEvent {
	return TelemetryEvent{
		SessionID: sessionID, Agent: agent, TaskID: taskID,
		EventType: "task_sharded",
	}
}

func EventCouncilRuling(sessionID, agent string, taskID int, approved bool, feedback string) TelemetryEvent {
	eventType := "council_approved"
	if !approved {
		eventType = "council_rejected"
	}
	return TelemetryEvent{
		SessionID: sessionID, Agent: agent, TaskID: taskID,
		EventType: eventType,
		Payload:   map[string]any{"feedback": feedback},
	}
}

func EventEstop(active bool) TelemetryEvent {
	eventType := "estop_activated"
	if !active {
		eventType = "estop_cleared"
	}
	return TelemetryEvent{EventType: eventType}
}

func EventInfraFailure(sessionID, agent string, taskID, count int, reason string) TelemetryEvent {
	return TelemetryEvent{
		SessionID: sessionID, Agent: agent, TaskID: taskID,
		EventType: "infra_failure",
		Payload:   map[string]any{"count": count, "reason": util.TruncateStr(reason, 200)},
	}
}

func EventInquisitorReset(taskIDs []int) TelemetryEvent {
	return TelemetryEvent{
		EventType: "inquisitor_reset",
		Payload:   map[string]any{"task_ids": taskIDs, "count": len(taskIDs)},
	}
}

func EventTaskDoneSignal(sessionID, agent string, taskID int) TelemetryEvent {
	return TelemetryEvent{
		SessionID: sessionID, Agent: agent, TaskID: taskID,
		EventType: "task_done_signal",
	}
}

func EventRateLimited(sessionID, agent string, taskID, hitCount int, backoff time.Duration) TelemetryEvent {
	return TelemetryEvent{
		SessionID: sessionID, Agent: agent, TaskID: taskID,
		EventType: "rate_limited",
		Payload:   map[string]any{"hit_count": hitCount, "backoff_s": backoff.Seconds()},
	}
}

func EventStallDetected(taskID int, agent, repo string, lockedMinutes float64) TelemetryEvent {
	return TelemetryEvent{
		TaskID:    taskID,
		EventType: "stall_detected",
		Payload:   map[string]any{"agent": agent, "repo": repo, "locked_minutes": lockedMinutes},
	}
}

