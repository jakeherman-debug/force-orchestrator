package store

import (
	"strings"
	"testing"
	"time"
)

// TestPromptByteAttribution_SourceTagsPopulated assembles a multi-source
// prompt and verifies the byte counts recorded sum to the total prompt
// length. Per-source rows are present and the breakdown reads back via
// ListPromptByteAttributionsForTask.
func TestPromptByteAttribution_SourceTagsPopulated(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()

	taskID := 42
	agent := "captain"
	callTS := NowSQLite()

	systemPrompt := "You are the Captain agent. Review the convoy diff and respond JSON-only."
	claudeMD := "## CLAUDE.md\nAlways prefer self-healing over escalation."
	taskPayload := "Convoy 5 task — implement OAuth on the api repo."
	scopeGuard := "[SCOPE GUARD — DO NOT MODIFY]\n- secrets.yaml\n- ci/deploy.yml\n---"
	fileRead := strings.Repeat("git diff content ", 32) // a real file_read slice

	contributions := []SourceContribution{
		{SourceTag: "fleet_rules", Bytes: len(systemPrompt)},
		{SourceTag: "claude_md", Bytes: len(claudeMD)},
		{SourceTag: "task_payload", Bytes: len(taskPayload)},
		{SourceTag: "scope_guard", Bytes: len(scopeGuard)},
		{SourceTag: "file_read", Bytes: len(fileRead)},
	}
	totalExpected := 0
	for _, c := range contributions {
		totalExpected += c.Bytes
	}

	if err := RecordSourceTags(db, taskID, agent, callTS, contributions); err != nil {
		t.Fatalf("RecordSourceTags: %v", err)
	}

	// Idempotence — re-recording the same callTS adds new rows, not silently
	// dedups (each call is a distinct event); document the contract here.
	got, err := ListPromptByteAttributionsForTask(db, taskID)
	if err != nil {
		t.Fatalf("ListPromptByteAttributionsForTask: %v", err)
	}
	if len(got) != len(contributions) {
		t.Fatalf("expected %d rows, got %d", len(contributions), len(got))
	}

	// Verify the sum matches.
	gotTotal := 0
	bySource := map[string]int{}
	for _, r := range got {
		gotTotal += r.Bytes
		bySource[r.SourceTag] += r.Bytes
		if r.AgentName != agent {
			t.Errorf("row %d: agent_name = %q, want %q", r.ID, r.AgentName, agent)
		}
		if r.TaskID != taskID {
			t.Errorf("row %d: task_id = %d, want %d", r.ID, r.TaskID, taskID)
		}
		if r.CallTimestamp != callTS {
			t.Errorf("row %d: call_timestamp = %q, want %q", r.ID, r.CallTimestamp, callTS)
		}
	}
	if gotTotal != totalExpected {
		t.Errorf("total bytes: got %d, want %d", gotTotal, totalExpected)
	}
	// Spot-check a couple of source tags.
	if bySource["scope_guard"] != len(scopeGuard) {
		t.Errorf("scope_guard bytes: got %d, want %d", bySource["scope_guard"], len(scopeGuard))
	}
	if bySource["file_read"] != len(fileRead) {
		t.Errorf("file_read bytes: got %d, want %d", bySource["file_read"], len(fileRead))
	}
}

// TestRecordSourceTags_ZeroByteContributionDropped verifies a 0-byte
// entry produces no row (no signal value), but a malformed entry
// (non-zero bytes + empty tag) returns an error.
func TestRecordSourceTags_ZeroByteContributionDropped(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()

	err := RecordSourceTags(db, 1, "captain", NowSQLite(), []SourceContribution{
		{SourceTag: "claude_md", Bytes: 0}, // dropped
		{SourceTag: "task_payload", Bytes: 50},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got, _ := ListPromptByteAttributionsForTask(db, 1)
	if len(got) != 1 {
		t.Errorf("expected 1 row (0-byte dropped), got %d", len(got))
	}

	if err := RecordSourceTags(db, 2, "captain", NowSQLite(), []SourceContribution{
		{SourceTag: "", Bytes: 50}, // malformed: bytes>0 with empty tag
	}); err == nil {
		t.Error("expected error for empty source_tag with non-zero bytes, got nil")
	}
}

// TestRecordSourceTags_RequiresAgentName rejects the empty agent name.
func TestRecordSourceTags_RequiresAgentName(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()

	if err := RecordSourceTags(db, 1, "", NowSQLite(), []SourceContribution{
		{SourceTag: "task_payload", Bytes: 10},
	}); err == nil {
		t.Error("expected error for empty agent_name, got nil")
	}
}

// TestPromptByteAttributionByAgent_RollingWindow verifies the
// dashboard's rolling-window aggregation correctly groups by agent and
// reports per-source bytes + distinct call counts.
func TestPromptByteAttributionByAgent_RollingWindow(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()

	// Two calls for captain, one for medic. Each call produces multiple
	// source rows sharing one call_timestamp.
	captainCall1 := NowSQLite()
	if err := RecordSourceTags(db, 1, "captain", captainCall1, []SourceContribution{
		{SourceTag: "claude_md", Bytes: 1000},
		{SourceTag: "file_read", Bytes: 4000},
	}); err != nil {
		t.Fatalf("captain call1: %v", err)
	}
	// Force a different timestamp for the second call so DISTINCT counts ==2.
	time.Sleep(1100 * time.Millisecond)
	captainCall2 := NowSQLite()
	if err := RecordSourceTags(db, 1, "captain", captainCall2, []SourceContribution{
		{SourceTag: "claude_md", Bytes: 500},
		{SourceTag: "task_payload", Bytes: 2000},
	}); err != nil {
		t.Fatalf("captain call2: %v", err)
	}

	medicCall := NowSQLite()
	if err := RecordSourceTags(db, 2, "medic", medicCall, []SourceContribution{
		{SourceTag: "fleet_rules", Bytes: 800},
	}); err != nil {
		t.Fatalf("medic call: %v", err)
	}

	got, err := PromptByteAttributionByAgent(db, "")
	if err != nil {
		t.Fatalf("PromptByteAttributionByAgent: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 agents, got %d (%+v)", len(got), got)
	}

	// got is sorted by agent_name ASC: captain, medic.
	if got[0].AgentName != "captain" {
		t.Fatalf("expected captain first, got %q", got[0].AgentName)
	}
	if got[0].Calls != 2 {
		t.Errorf("captain calls: got %d, want 2", got[0].Calls)
	}
	if got[0].TotalBytes != 1000+4000+500+2000 {
		t.Errorf("captain total: got %d, want %d", got[0].TotalBytes, 1000+4000+500+2000)
	}
	// claude_md should aggregate across both calls
	if got[0].BySource["claude_md"] != 1500 {
		t.Errorf("captain claude_md: got %d, want 1500", got[0].BySource["claude_md"])
	}
	// Ordered descending by bytes — file_read (4000) should be first
	if len(got[0].Ordered) == 0 || got[0].Ordered[0].SourceTag != "file_read" {
		t.Errorf("captain ordered: expected file_read first, got %+v", got[0].Ordered)
	}

	if got[1].AgentName != "medic" || got[1].Calls != 1 || got[1].TotalBytes != 800 {
		t.Errorf("medic mismatch: %+v", got[1])
	}
}
