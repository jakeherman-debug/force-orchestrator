package claude

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"force-orchestrator/internal/store"
)

// TestContextOverflow_TriggersSummarize seeds a 300 KB prompt against
// a 200 KB cap and verifies the summarizer is invoked. The mock
// summarizer returns a 50 KB string (well under cap) so the call
// proceeds and the runner sees the SUMMARIZED prompt — not the
// original.
func TestContextOverflow_TriggersSummarize(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	SetContextSizeDB(db)
	t.Cleanup(func() { SetContextSizeDB(nil) })

	// Use the default cap (200 KB).
	bigPrompt := strings.Repeat("X", 300_000)
	summarizerCalled := false
	var capturedTarget int

	SetSummarizerForTest(func(ctx context.Context, prompt string, targetBytes int) (string, error) {
		summarizerCalled = true
		capturedTarget = targetBytes
		// Return a 50 KB string — well under the 200 KB cap.
		return strings.Repeat("S", 50_000), nil
	})
	t.Cleanup(ResetSummarizerForTest)

	// Stamp the call ctx with a captain attribution so
	// CheckContextSize uses the captain's per-agent cap (defaults to
	// 200 KB since no override is set).
	ctx := WithClaudeCallContext(context.Background(), "captain", 42,
		[]store.SourceContribution{{SourceTag: "file_read", Bytes: 300_000}})

	// Install a stub runner that records what prompt it received.
	var runnerSawPrompt string
	stub := func(_ context.Context, prompt, _, _, _, _ string, _ int, _ time.Duration) (string, error) {
		runnerSawPrompt = prompt
		return "stub-output", nil
	}
	SetCLIRunner(stub)
	t.Cleanup(ResetCLIRunner)

	// AskClaudeCLIContext concatenates "SYSTEM INSTRUCTIONS:\n%s\n\nUSER PROMPT:\n%s",
	// so we send the bigPrompt as the userPrompt; total is bigPrompt + a few hundred bytes.
	out, err := AskClaudeCLIContext(ctx, "sys", bigPrompt, "", "", "", 1)
	if err != nil {
		t.Fatalf("expected success after summarize, got error: %v", err)
	}
	if out != "stub-output" {
		t.Errorf("expected stub output, got %q", out)
	}

	if !summarizerCalled {
		t.Fatal("expected summarizer to be invoked, but it was not")
	}
	if capturedTarget != 200_000 {
		t.Errorf("expected target=200_000, got %d", capturedTarget)
	}
	// Runner should have received the summarized 50 KB string, NOT the
	// 300 KB original.
	if len(runnerSawPrompt) > 200_000 {
		t.Errorf("runner saw %d bytes — the summarizer's output was not used", len(runnerSawPrompt))
	}
	if !strings.HasPrefix(runnerSawPrompt, "SSSS") {
		t.Errorf("runner did not see the summarized output (first chars: %q)", runnerSawPrompt[:min(20, len(runnerSawPrompt))])
	}

	// PromptByteAttribution should hold rows for this call. Expect
	// 1 row from the stamped contribution (file_read, 300_000) plus
	// 1 row from the post-summarize record (other, 50_000).
	rows, err := store.ListPromptByteAttributionsForTask(db, 42)
	if err != nil {
		t.Fatalf("ListPromptByteAttributionsForTask: %v", err)
	}
	if len(rows) < 1 {
		t.Errorf("expected ≥1 PromptByteAttribution row, got %d", len(rows))
	}
}

// TestContextOverflow_HardRejectOnDoubleOverflow installs a summarizer
// whose result still exceeds the cap. CheckContextSize must return
// ErrContextOverflow (no proceed); the runner must NOT be called.
func TestContextOverflow_HardRejectOnDoubleOverflow(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	SetContextSizeDB(db)
	t.Cleanup(func() { SetContextSizeDB(nil) })

	bigPrompt := strings.Repeat("X", 300_000)
	SetSummarizerForTest(func(ctx context.Context, prompt string, targetBytes int) (string, error) {
		// Returns a still-too-large string (220 KB > 200 KB cap).
		return strings.Repeat("Y", 220_000), nil
	})
	t.Cleanup(ResetSummarizerForTest)

	runnerCalled := false
	stub := func(_ context.Context, _, _, _, _, _ string, _ int, _ time.Duration) (string, error) {
		runnerCalled = true
		return "", nil
	}
	SetCLIRunner(stub)
	t.Cleanup(ResetCLIRunner)

	ctx := WithClaudeCallContext(context.Background(), "captain", 99,
		[]store.SourceContribution{{SourceTag: "file_read", Bytes: 300_000}})

	_, err := AskClaudeCLIContext(ctx, "sys", bigPrompt, "", "", "", 1)
	if !errors.Is(err, ErrContextOverflow) {
		t.Fatalf("expected ErrContextOverflow, got %v", err)
	}
	if runnerCalled {
		t.Error("runner was called despite double-overflow — should have short-circuited")
	}
}

// TestContextOverflow_NoSummarizerInstalled verifies the fail-closed
// path when overflow happens but no summarizer is available.
func TestContextOverflow_NoSummarizerInstalled(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	SetContextSizeDB(db)
	t.Cleanup(func() { SetContextSizeDB(nil) })

	// Ensure no summarizer is installed.
	ResetSummarizerForTest()

	bigPrompt := strings.Repeat("X", 300_000)

	stub := func(_ context.Context, _, _, _, _, _ string, _ int, _ time.Duration) (string, error) {
		t.Error("runner called — should have short-circuited on overflow")
		return "", nil
	}
	SetCLIRunner(stub)
	t.Cleanup(ResetCLIRunner)

	ctx := WithClaudeCallContext(context.Background(), "captain", 1,
		[]store.SourceContribution{{SourceTag: "file_read", Bytes: 300_000}})
	_, err := AskClaudeCLIContext(ctx, "sys", bigPrompt, "", "", "", 1)
	if !errors.Is(err, ErrContextOverflow) {
		t.Fatalf("expected ErrContextOverflow with no summarizer, got %v", err)
	}
}

// TestContextOverflow_UnderCap_NoSummarizerCall verifies the happy
// path: an under-cap prompt does NOT invoke the summarizer.
func TestContextOverflow_UnderCap_NoSummarizerCall(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	SetContextSizeDB(db)
	t.Cleanup(func() { SetContextSizeDB(nil) })

	called := false
	SetSummarizerForTest(func(ctx context.Context, prompt string, targetBytes int) (string, error) {
		called = true
		return prompt, nil
	})
	t.Cleanup(ResetSummarizerForTest)

	stub := func(_ context.Context, _, _, _, _, _ string, _ int, _ time.Duration) (string, error) {
		return "ok", nil
	}
	SetCLIRunner(stub)
	t.Cleanup(ResetCLIRunner)

	ctx := WithClaudeCallContext(context.Background(), "captain", 7,
		[]store.SourceContribution{{SourceTag: "task_payload", Bytes: 500}})
	if _, err := AskClaudeCLIContext(ctx, "sys", "small prompt", "", "", "", 1); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if called {
		t.Error("summarizer was called for under-cap prompt")
	}
}

// TestContextOverflow_PerAgentOverride verifies that a SystemConfig
// per-agent override (agent_max_prompt_bytes_<agent>) is honoured by
// the cap check.
func TestContextOverflow_PerAgentOverride(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	SetContextSizeDB(db)
	t.Cleanup(func() { SetContextSizeDB(nil) })

	// Lower the captain cap to 1 KB; a 5 KB prompt now overflows.
	store.SetConfig(db, "agent_max_prompt_bytes_captain", "1024")

	summarizerCalled := false
	SetSummarizerForTest(func(ctx context.Context, prompt string, targetBytes int) (string, error) {
		summarizerCalled = true
		return strings.Repeat("S", 500), nil // well under 1 KB
	})
	t.Cleanup(ResetSummarizerForTest)

	stub := func(_ context.Context, _, _, _, _, _ string, _ int, _ time.Duration) (string, error) {
		return "ok", nil
	}
	SetCLIRunner(stub)
	t.Cleanup(ResetCLIRunner)

	medPrompt := strings.Repeat("X", 5000)
	ctx := WithClaudeCallContext(context.Background(), "captain", 1,
		[]store.SourceContribution{{SourceTag: "file_read", Bytes: 5000}})
	if _, err := AskClaudeCLIContext(ctx, "sys", medPrompt, "", "", "", 1); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !summarizerCalled {
		t.Error("summarizer was not called despite per-agent cap override")
	}
}

// TestAgentMaxPromptBytes_Defaults exercises the lookup chain.
func TestAgentMaxPromptBytes_Defaults(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	// Empty config — falls through to defaultMaxPromptBytes.
	if v := AgentMaxPromptBytes(db, "captain"); v != defaultMaxPromptBytes {
		t.Errorf("default cap: got %d, want %d", v, defaultMaxPromptBytes)
	}

	// Default override.
	store.SetConfig(db, "agent_max_prompt_bytes_default", "150000")
	if v := AgentMaxPromptBytes(db, "captain"); v != 150_000 {
		t.Errorf("default override: got %d, want 150000", v)
	}

	// Per-agent override beats default override.
	store.SetConfig(db, "agent_max_prompt_bytes_captain", "75000")
	if v := AgentMaxPromptBytes(db, "captain"); v != 75_000 {
		t.Errorf("captain override: got %d, want 75000", v)
	}
	if v := AgentMaxPromptBytes(db, "medic"); v != 150_000 {
		t.Errorf("medic still uses default override: got %d, want 150000", v)
	}

	// Negative / unparseable values fall back to default.
	store.SetConfig(db, "agent_max_prompt_bytes_captain", "-1")
	if v := AgentMaxPromptBytes(db, "captain"); v != 150_000 {
		t.Errorf("negative override: got %d, want default 150000", v)
	}
}

// TestEmitContextOverflowMail_DedupedPerAgentPerDay verifies the
// "first overflow per agent per day" dedup contract.
func TestEmitContextOverflowMail_DedupedPerAgentPerDay(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	emitContextOverflowMail(db, "captain", 1, 300_000, 200_000, "file_read=200KB(60%)")
	emitContextOverflowMail(db, "captain", 2, 305_000, 200_000, "file_read=205KB(60%)")
	emitContextOverflowMail(db, "medic", 3, 300_000, 200_000, "file_read=200KB(60%)")

	mails := store.ListMail(db, "operator")
	captainCount := 0
	medicCount := 0
	for _, m := range mails {
		if strings.Contains(m.Subject, "agent=captain") {
			captainCount++
		}
		if strings.Contains(m.Subject, "agent=medic") {
			medicCount++
		}
	}
	if captainCount != 1 {
		t.Errorf("captain mail count: got %d, want 1 (deduped)", captainCount)
	}
	if medicCount != 1 {
		t.Errorf("medic mail count: got %d, want 1", medicCount)
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// TestAskClaudeCLIContext_RecordsAttributionFromContext is the
// end-to-end glue test: the caller stamps a multi-source context, the
// ingress check records a PromptByteAttribution row per source. This
// is the integration covered by the roadmap's
// TestPromptByteAttribution_SourceTagsPopulated requirement, exercised
// through the actual claude.AskClaudeCLIContext entry point rather
// than directly against the store helper.
func TestAskClaudeCLIContext_RecordsAttributionFromContext(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	SetContextSizeDB(db)
	t.Cleanup(func() { SetContextSizeDB(nil) })

	stub := func(_ context.Context, _, _, _, _, _ string, _ int, _ time.Duration) (string, error) {
		return "ok", nil
	}
	SetCLIRunner(stub)
	t.Cleanup(ResetCLIRunner)

	ctx := WithClaudeCallContext(context.Background(), "captain", 100, []store.SourceContribution{
		{SourceTag: "fleet_rules", Bytes: 1000},
		{SourceTag: "claude_md", Bytes: 500},
		{SourceTag: "task_payload", Bytes: 250},
		{SourceTag: "file_read", Bytes: 4000},
	})
	if _, err := AskClaudeCLIContext(ctx, "sys", "user", "", "", "", 1); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	rows, err := store.ListPromptByteAttributionsForTask(db, 100)
	if err != nil {
		t.Fatalf("ListPromptByteAttributionsForTask: %v", err)
	}
	if len(rows) != 4 {
		t.Fatalf("expected 4 rows (one per source), got %d (%+v)", len(rows), rows)
	}
	bySource := map[string]int{}
	for _, r := range rows {
		bySource[r.SourceTag] = r.Bytes
		if r.AgentName != "captain" {
			t.Errorf("expected agent=captain, got %q", r.AgentName)
		}
	}
	if bySource["fleet_rules"] != 1000 {
		t.Errorf("fleet_rules: got %d, want 1000", bySource["fleet_rules"])
	}
	if bySource["file_read"] != 4000 {
		t.Errorf("file_read: got %d, want 4000", bySource["file_read"])
	}
}
