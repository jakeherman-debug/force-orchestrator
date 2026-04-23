package agents

import (
	"strings"
	"testing"

	"force-orchestrator/internal/store"
)

func TestBuildScopeGuardedPayload_NoRejectedFiles_PlainFeedback(t *testing.T) {
	ruling := store.CaptainRuling{Feedback: "please fix the error handling"}
	got := buildScopeGuardedPayload("original task", ruling, 1)

	if strings.Contains(got, scopeGuardMarker) {
		t.Errorf("no rejected_files should leave guard block absent; got %q", got)
	}
	if !strings.Contains(got, "original task") {
		t.Error("must preserve the original payload body")
	}
	if !strings.Contains(got, "CAPTAIN FEEDBACK (attempt 1/") {
		t.Error("must append captain feedback line")
	}
}

func TestBuildScopeGuardedPayload_WithRejectedFiles_PrependsGuard(t *testing.T) {
	ruling := store.CaptainRuling{
		Feedback:      "you touched dashboard.go but the task was claude.go only",
		RejectedFiles: []string{"internal/dashboard/dashboard.go", "internal/dashboard/static/app.js"},
	}
	got := buildScopeGuardedPayload("Fix rateLimitPatterns regex", ruling, 2)

	if !strings.HasPrefix(got, scopeGuardMarker) {
		t.Errorf("guarded payload must start with %q; got %q", scopeGuardMarker, got[:60])
	}
	for _, f := range ruling.RejectedFiles {
		if !strings.Contains(got, f) {
			t.Errorf("guard must list rejected file %q; got %q", f, got)
		}
	}
	if !strings.Contains(got, "Fix rateLimitPatterns regex") {
		t.Error("must preserve original task body after guard")
	}
	if !strings.Contains(got, "CAPTAIN FEEDBACK (attempt 2/") {
		t.Error("must include feedback line with attempt count")
	}
}

func TestBuildScopeGuardedPayload_Idempotent_NoAccumulation(t *testing.T) {
	ruling1 := store.CaptainRuling{
		Feedback:      "first rejection",
		RejectedFiles: []string{"internal/dashboard/dashboard.go"},
	}
	ruling2 := store.CaptainRuling{
		Feedback:      "second rejection with different file",
		RejectedFiles: []string{"internal/claude/claude.go"},
	}

	first := buildScopeGuardedPayload("original task", ruling1, 1)
	second := buildScopeGuardedPayload(first, ruling2, 2)

	guardCount := strings.Count(second, scopeGuardMarker)
	if guardCount != 1 {
		t.Errorf("repeated rejections must not accumulate guard blocks; got %d", guardCount)
	}
	if strings.Contains(second, "internal/dashboard/dashboard.go") {
		t.Error("new guard must replace the old file list, not merge it")
	}
	if !strings.Contains(second, "internal/claude/claude.go") {
		t.Error("new guard must list the latest rejected file")
	}
	if !strings.Contains(second, "original task") {
		t.Error("original body must survive strip-and-rebuild")
	}
}

func TestBuildScopeGuardedPayload_SkipsBlankFilePaths(t *testing.T) {
	ruling := store.CaptainRuling{
		Feedback:      "mixed rejections",
		RejectedFiles: []string{"", "  ", "real/file.go"},
	}
	got := buildScopeGuardedPayload("task body", ruling, 1)
	bulletLines := strings.Count(got, "\n  - ")
	if bulletLines != 1 {
		t.Errorf("expected 1 file bullet, got %d (payload=%q)", bulletLines, got)
	}
	if !strings.Contains(got, "real/file.go") {
		t.Error("non-blank file should be listed")
	}
}

func TestStripScopeGuard_NoGuard_ReturnsPayloadUnchanged(t *testing.T) {
	payload := "ordinary task description"
	if stripScopeGuard(payload) != payload {
		t.Errorf("payload without guard should be unchanged")
	}
}

func TestStripScopeGuard_WithGuard_RemovesHeader(t *testing.T) {
	ruling := store.CaptainRuling{RejectedFiles: []string{"f.go"}, Feedback: "x"}
	guarded := buildScopeGuardedPayload("BODY", ruling, 1)
	if stripped := stripScopeGuard(guarded); !strings.HasPrefix(stripped, "BODY") {
		t.Errorf("strip should return payload starting with the original body; got %q", stripped[:40])
	}
}
