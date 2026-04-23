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

// TestBuildScopeGuardedPayload_FiltersHallucinatedInScopeFiles is the direct
// regression test for task 449's failure mode. The Captain LLM listed
// internal/claude/claude.go in rejected_files even though the task body
// literally asks to modify it. Without this filter, Medic sees payload+guard
// as contradictory and escalates — an entirely unnecessary operator burden.
func TestBuildScopeGuardedPayload_FiltersHallucinatedInScopeFiles(t *testing.T) {
	ruling := store.CaptainRuling{
		Feedback: "dashboard changes are out of scope",
		// Captain's LLM returned BOTH truly out-of-scope files AND the task's
		// actual target — this is the hallucination we have to defend against.
		RejectedFiles: []string{
			"internal/dashboard/dashboard.go",
			"internal/claude/claude.go",
		},
	}
	body := "In internal/claude/claude.go, extend the rateLimitPatterns regex to match new substrings."

	got := buildScopeGuardedPayload(body, ruling, 1)
	if !strings.Contains(got, "internal/dashboard/dashboard.go") {
		t.Error("truly out-of-scope file must remain in guard")
	}
	if strings.Contains(got, "- internal/claude/claude.go\n") {
		t.Error("in-scope file (mentioned in task body) must be filtered from guard — otherwise the next attempt is unsatisfiable")
	}
}

// TestBuildScopeGuardedPayload_AllRejectsHallucinated_NoGuard covers the
// worst case: every file the Captain claimed was out-of-scope is actually
// referenced in the task body. The guard would be empty; don't build one at
// all. This avoids emitting a stub "[SCOPE GUARD — DO NOT MODIFY]" block
// with zero entries, which would confuse both agents and future stripping.
func TestBuildScopeGuardedPayload_AllRejectsHallucinated_NoGuard(t *testing.T) {
	ruling := store.CaptainRuling{
		Feedback:      "your approach broke the tests",
		RejectedFiles: []string{"internal/claude/claude.go", "internal/claude/claude_test.go"},
	}
	body := "In internal/claude/claude.go and internal/claude/claude_test.go, add a regex."

	got := buildScopeGuardedPayload(body, ruling, 1)
	if strings.Contains(got, scopeGuardMarker) {
		t.Error("when every rejected file is in-scope, no guard block should be emitted")
	}
	if !strings.Contains(got, "CAPTAIN FEEDBACK") {
		t.Error("must still append the feedback line")
	}
}

func TestFilterHallucinatedRejections(t *testing.T) {
	body := "Modify internal/claude/claude.go and test it in internal/claude/claude_test.go."
	got := filterHallucinatedRejections(
		[]string{"internal/claude/claude.go", "internal/dashboard/app.js", "", "  ", "internal/claude/claude_test.go"},
		body,
	)
	want := []string{"internal/dashboard/app.js"}
	if len(got) != len(want) || (len(got) > 0 && got[0] != want[0]) {
		t.Errorf("filterHallucinatedRejections = %v, want %v", got, want)
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
