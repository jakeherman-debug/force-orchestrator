package agents

import (
	"errors"
	"testing"

	"force-orchestrator/internal/store"
)

// ── BootTriage ────────────────────────────────────────────────────────────────

func TestBootTriage_Reset(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	withStubCLIRunner(t, `{"decision":"RESET","reason":"agent is stuck in a loop"}`, nil)

	verdict := BootTriage(db, 1, "R2-D2", "api", 45, "")
	if verdict.Decision != BootReset {
		t.Errorf("expected BootReset, got %q", verdict.Decision)
	}
	if verdict.Reason == "" {
		t.Error("expected non-empty reason")
	}
}

func TestBootTriage_Escalate(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	withStubCLIRunner(t, `{"decision":"ESCALATE","reason":"auth failure, needs human"}`, nil)

	verdict := BootTriage(db, 2, "BB-8", "frontend", 120, "auth error: 401")
	if verdict.Decision != BootEscalate {
		t.Errorf("expected BootEscalate, got %q", verdict.Decision)
	}
}

func TestBootTriage_Warn(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	withStubCLIRunner(t, `{"decision":"WARN","reason":"task appears stalled but may recover"}`, nil)

	verdict := BootTriage(db, 3, "C-3PO", "backend", 20, "")
	if verdict.Decision != BootWarn {
		t.Errorf("expected BootWarn, got %q", verdict.Decision)
	}
}

func TestBootTriage_Ignore(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	withStubCLIRunner(t, `{"decision":"IGNORE","reason":"agent is making steady progress"}`, nil)

	verdict := BootTriage(db, 4, "R2-D2", "api", 10, "")
	if verdict.Decision != BootIgnore {
		t.Errorf("expected BootIgnore, got %q", verdict.Decision)
	}
}

func TestBootTriage_CLIError_DefaultsToWarn(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	withStubCLIRunner(t, "", errors.New("claude CLI not available"))

	verdict := BootTriage(db, 5, "R2-D2", "api", 30, "")
	if verdict.Decision != BootWarn {
		t.Errorf("expected BootWarn on CLI error, got %q", verdict.Decision)
	}
	if verdict.Reason == "" {
		t.Error("expected non-empty reason on CLI error")
	}
}

func TestBootTriage_MalformedJSON_DefaultsToWarn(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	withStubCLIRunner(t, `this is not json at all`, nil)

	verdict := BootTriage(db, 6, "R2-D2", "api", 30, "")
	if verdict.Decision != BootWarn {
		t.Errorf("expected BootWarn on malformed JSON, got %q", verdict.Decision)
	}
}

func TestBootTriage_UnknownDecision_DefaultsToWarn(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	withStubCLIRunner(t, `{"decision":"DESTROY","reason":"chaos"}`, nil)

	verdict := BootTriage(db, 7, "R2-D2", "api", 30, "")
	if verdict.Decision != BootWarn {
		t.Errorf("expected BootWarn for unknown decision, got %q", verdict.Decision)
	}
}

func TestBootTriage_JSONInMarkdownFence(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	// Claude sometimes wraps responses in markdown fences
	withStubCLIRunner(t, "```json\n{\"decision\":\"RESET\",\"reason\":\"stuck\"}\n```", nil)

	verdict := BootTriage(db, 8, "R2-D2", "api", 60, "repeated error")
	if verdict.Decision != BootReset {
		t.Errorf("expected BootReset from fenced JSON, got %q", verdict.Decision)
	}
}
