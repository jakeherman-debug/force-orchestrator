package main

// JIRA-from-UI — smoke test for cmdAddJira after the helper extraction.
//
// The bulk of the fetch + payload logic lives in
// internal/agents.QueueFeatureFromJira (and is tested there with the
// LIVE_HAIKU_DISABLED stub branch). This test asserts the CLI surface
// itself still:
//   - parses --priority + --plan-only argv flags
//   - prints a "Fetching <ticket>..." progress line
//   - prints a "Jira ticket <ticket> added to the Fleet as task #N" line
//     (and the plan-only suffix when applicable)
//   - queues a Feature row with the right payload + priority
//
// LIVE_HAIKU_DISABLED is set per-test so QueueFeatureFromJira routes
// through its deterministic stub branch and never touches live MCP.

import (
	"strings"
	"testing"

	"force-orchestrator/internal/store"
)

func TestCmdAddJira_HappyPath(t *testing.T) {
	t.Setenv("LIVE_HAIKU_DISABLED", "1")
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	out := captureOutput(func() {
		cmdAddJira(db, []string{"ABC-123"})
	})
	if !strings.Contains(out, "Fetching Jira ticket ABC-123") {
		t.Errorf("missing 'Fetching' line; out=%q", out)
	}
	if !strings.Contains(out, "Jira ticket ABC-123 added to the Fleet as task #") {
		t.Errorf("missing success line; out=%q", out)
	}
	if strings.Contains(out, "Commander will plan only") {
		t.Errorf("plan-only suffix should NOT be present without --plan-only; out=%q", out)
	}

	var taskType, payload string
	row := db.QueryRow(`SELECT type, payload FROM BountyBoard ORDER BY id DESC LIMIT 1`)
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

func TestCmdAddJira_PriorityAndPlanOnly(t *testing.T) {
	t.Setenv("LIVE_HAIKU_DISABLED", "1")
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	out := captureOutput(func() {
		cmdAddJira(db, []string{"--priority", "7", "--plan-only", "TEAM-9"})
	})
	if !strings.Contains(out, "Commander will plan only") {
		t.Errorf("missing plan-only suffix; out=%q", out)
	}
	if !strings.Contains(out, "TEAM-9") {
		t.Errorf("missing ticket id in success line; out=%q", out)
	}

	var priority int
	var payload string
	row := db.QueryRow(`SELECT priority, payload FROM BountyBoard ORDER BY id DESC LIMIT 1`)
	if err := row.Scan(&priority, &payload); err != nil {
		t.Fatalf("query queued row: %v", err)
	}
	if priority != 7 {
		t.Errorf("priority: got %d want 7", priority)
	}
	if !strings.HasPrefix(payload, "[PLAN_ONLY]\n") {
		t.Errorf("plan-only payload: got %q want [PLAN_ONLY]\\n prefix", payload)
	}
	if !strings.Contains(payload, "[JIRA: TEAM-9]") {
		t.Errorf("payload missing ticket marker: %q", payload)
	}
}
