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

	"force-orchestrator/internal/forcepath"
	"force-orchestrator/internal/store"
	"force-orchestrator/internal/util"
)

// TelemetryEvent is a structured event written to holonet.jsonl.
// Every field is optional except Timestamp and EventType.
// Set FORCE_OTEL_LOGS_URL to also ship events to an OTLP HTTP/JSON log endpoint
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
	telemetryFile  *os.File
	telemetryMu    sync.Mutex
	telemetryLog   *log.Logger
	otlpEndpoint   string       // set from FORCE_OTEL_LOGS_URL at init time
	otlpHTTPClient *http.Client // shared, reused across goroutines
	// otlpInFlight tracks async OTLP POST goroutines launched by EmitEvent so
	// tests can wait for them to drain before resetting the globals. Without
	// this wait, -race flags a read-vs-write on otlpEndpoint / otlpHTTPClient
	// between the deferred cleanup and the tail of sendOTLPLog (AUDIT prep
	// note: pre-existing race predates Fix #10; fixed here because Fix #10
	// is the first change to touch the OTLP path).
	otlpInFlight sync.WaitGroup
)

func InitTelemetry() {
	// Sweep-F: holonet.jsonl resolves through forcepath
	// (~/.force/holonet.jsonl by default; overridable via FORCE_DIR).
	// Pre-canonical builds wrote to "./holonet.jsonl" in CWD, which
	// invisibly bifurcated event streams when daemon/CLI ran from
	// different directories.
	path := forcepath.HolonetEventStream()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		telemetryLog = log.New(os.Stderr, "[telemetry] ", log.LstdFlags)
		telemetryLog.Printf("WARNING: could not open %s: %v", path, err)
	} else {
		telemetryFile = f
	}
	telemetryLog = log.New(os.Stderr, "[telemetry] ", log.LstdFlags)

	if rawURL := os.Getenv("FORCE_OTEL_LOGS_URL"); rawURL != "" {
		// Validate at init. An attacker with env access must not be able
		// to redirect every task_claimed payload preview to an arbitrary
		// endpoint — in particular not to 169.254.169.254 (AWS/GCP
		// metadata) or a 10.x.x.x service on the daemon host.
		if err := store.ValidateOutboundURL(rawURL); err != nil {
			telemetryLog.Printf("REFUSING FORCE_OTEL_LOGS_URL=%q: %v", rawURL, err)
			return
		}
		otlpEndpoint = rawURL
		otlpHTTPClient = newOTLPClient()
		telemetryLog.Printf("OTLP export enabled → %s", otlpEndpoint)
	}
}

// newOTLPClient builds the http.Client used for OTLP POSTs. The CheckRedirect
// policy revalidates the target on every hop — an attacker who somehow got
// the env var past init (e.g., DNS result changed after startup) cannot
// 302-bounce the telemetry stream to an internal address.
func newOTLPClient() *http.Client {
	return &http.Client{
		Timeout: 5 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return fmt.Errorf("otlp: too many redirects")
			}
			return store.ValidateOutboundURL(req.URL.String())
		},
	}
}

// EmitEvent writes a structured event to holonet.jsonl and optionally to an
// OTLP HTTP endpoint if FORCE_OTEL_LOGS_URL is set. Thread-safe.
//
// Outbound-channel hardening (Fix #10): event payloads are redacted via
// store.RedactSecrets before either the holonet.jsonl write or the OTLP
// POST, so ghp_/Bearer/url-basic-auth tokens that leaked into a task
// payload preview don't escape via the telemetry stream.
func EmitEvent(event TelemetryEvent) {
	if event.Timestamp.IsZero() {
		event.Timestamp = time.Now()
	}
	event = redactEventPayload(event)

	data, err := json.Marshal(event)
	if err != nil {
		return
	}

	telemetryMu.Lock()
	if telemetryFile != nil {
		fmt.Fprintf(telemetryFile, "%s\n", data)
	}
	endpoint := otlpEndpoint
	client := otlpHTTPClient
	telemetryMu.Unlock()

	if endpoint != "" && client != nil {
		otlpInFlight.Add(1)
		go func() {
			defer otlpInFlight.Done()
			sendOTLPLog(event, data, endpoint, client)
		}()
	}
}

// WaitForOTLPDrain blocks until every in-flight OTLP POST goroutine has
// returned. Production code never needs this — EmitEvent is fire-and-
// forget — but tests that reset otlpEndpoint / otlpHTTPClient must wait
// to avoid racing the teardown against sendOTLPLog's tail.
func WaitForOTLPDrain() { otlpInFlight.Wait() }

// redactEventPayload walks the event's Payload map and scrubs string values
// through store.RedactSecrets. Non-string values pass through unchanged —
// structured fields like ints, durations, and ID lists cannot encode a
// secret. The function is cheap on events with no payload.
func redactEventPayload(e TelemetryEvent) TelemetryEvent {
	if e.Payload == nil {
		return e
	}
	out := make(map[string]any, len(e.Payload))
	for k, v := range e.Payload {
		switch vv := v.(type) {
		case string:
			out[k] = store.RedactSecrets(vv)
		case []string:
			rs := make([]string, len(vv))
			for i, s := range vv {
				rs[i] = store.RedactSecrets(s)
			}
			out[k] = rs
		default:
			out[k] = v
		}
	}
	e.Payload = out
	return e
}

// sendOTLPLog ships a single event to an OTLP HTTP/JSON logs endpoint.
// The OTLP Logs JSON format: https://opentelemetry.io/docs/specs/otlp/#otlphttp
//
// endpoint and client are captured by the caller under telemetryMu so a
// concurrent test teardown cannot null the globals mid-POST (pre-existing
// race — see otlpInFlight).
func sendOTLPLog(event TelemetryEvent, rawEvent []byte, endpoint string, client *http.Client) {
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
								"body":           map[string]any{"stringValue": store.RedactSecrets(string(rawEvent))},
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

	resp, err := client.Post(endpoint, "application/json", bytes.NewReader(payload))
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

// EventSpendCapExceeded is emitted by the spend-burn-watch dog when the
// trailing-hour spend crosses a threshold. kind is "soft_cap" (operator
// warning) or "auto_estop" (fleet halt).
func EventSpendCapExceeded(hourlySpend, threshold float64, kind string) TelemetryEvent {
	return TelemetryEvent{
		EventType: "spend_cap_exceeded",
		Payload: map[string]any{
			"hourly_spend_usd": hourlySpend,
			"threshold_usd":    threshold,
			"kind":             kind,
		},
	}
}
