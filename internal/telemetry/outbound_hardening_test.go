package telemetry

import (
	"os"
	"strings"
	"testing"

	"force-orchestrator/internal/store"
)

// TestInitTelemetry_RejectsLinkLocalOTLPURL asserts that setting
// FORCE_OTEL_LOGS_URL to a link-local literal (the cloud-metadata
// endpoint lives at 169.254.169.254) causes InitTelemetry to REFUSE
// the URL and leave OTLP export disabled. An operator with env-var
// access must not be able to redirect every task_claimed payload
// preview to an internal endpoint (AUDIT-017).
func TestInitTelemetry_RejectsLinkLocalOTLPURL(t *testing.T) {
	dir := t.TempDir()
	orig, _ := os.Getwd()
	os.Chdir(dir)
	t.Setenv("FORCE_OTEL_LOGS_URL", "http://169.254.169.254/v1/logs")
	defer func() {
		os.Chdir(orig)
		WaitForOTLPDrain()
		telemetryMu.Lock()
		if telemetryFile != nil {
			telemetryFile.Close()
			telemetryFile = nil
		}
		otlpEndpoint = ""
		otlpHTTPClient = nil
		telemetryMu.Unlock()
	}()

	// Ensure any previous test state is cleared.
	telemetryMu.Lock()
	otlpEndpoint = ""
	otlpHTTPClient = nil
	telemetryMu.Unlock()

	InitTelemetry()

	if otlpEndpoint != "" {
		t.Errorf("AUDIT-017: link-local URL was accepted; otlpEndpoint=%q", otlpEndpoint)
	}
	if otlpHTTPClient != nil {
		t.Errorf("AUDIT-017: otlpHTTPClient should be nil when URL is rejected")
	}
}

// TestRedactEventPayload_ScrubsPATFromStringField exercises
// redactEventPayload directly — a PAT embedded in a string-valued
// payload field must be scrubbed before emission. This is the
// acceptance gate for Fix #10's telemetry redaction.
func TestRedactEventPayload_ScrubsPATFromStringField(t *testing.T) {
	const fake = "ghp_telemetrySecretValue123456"
	e := TelemetryEvent{
		EventType: "task_claimed",
		Payload: map[string]any{
			"payload_preview": "work on task; token=" + fake + " thanks",
			"numeric_field":   42,
			"task_ids":        []string{"a", fake, "c"},
		},
	}
	out := redactEventPayload(e)

	if s, _ := out.Payload["payload_preview"].(string); strings.Contains(s, fake) {
		t.Errorf("string field not redacted: %q", s)
	}
	if n, _ := out.Payload["numeric_field"].(int); n != 42 {
		t.Errorf("numeric field mutated: got %v", out.Payload["numeric_field"])
	}
	if ss, _ := out.Payload["task_ids"].([]string); len(ss) != 3 || strings.Contains(ss[1], fake) {
		t.Errorf("[]string field not redacted per-element: %v", ss)
	}
}

// TestRedactSecrets_ReExportedByStore is a belt-and-suspenders check
// that the store.RedactSecrets symbol (which the telemetry redaction
// delegates to) is exported. If this package stops compiling, the
// telemetry's redaction path is broken.
func TestRedactSecrets_ReExportedByStore(t *testing.T) {
	got := store.RedactSecrets("ghp_abcdefghij1234567890")
	if !strings.Contains(got, "[REDACTED]") {
		t.Errorf("store.RedactSecrets unreachable from telemetry package: got %q", got)
	}
}
