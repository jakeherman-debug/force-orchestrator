// D3 P6A.10 — Pattern P29: briefing prose cites real evidence.
//
// Audit posture: assert the production renderer (briefing_renderer.go)
// uses the deterministic synthesiseBriefingText path that only emits
// IDs sourced from the input. The runtime fuzz of synthetic
// hallucinated rows lives in the briefing renderer test suite (which
// has access to the live DB). This audit is the static guard.
package audittools

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPattern_P29_BriefingCitesRealEvidence(t *testing.T) {
	root := repoRootP29(t)
	src, err := os.ReadFile(filepath.Join(root, "internal/agents/briefing_renderer.go"))
	if err != nil {
		t.Fatalf("read briefing_renderer.go: %v", err)
	}
	body := string(src)

	// 1. The renderer must contain a deterministic synthesis function.
	//    Pattern P29 contract: the synthesis only references input IDs.
	if !strings.Contains(body, "func synthesiseBriefingText(") {
		t.Errorf("Pattern P29: briefing_renderer.go missing synthesiseBriefingText — the deterministic-citation contract is broken")
	}

	// 2. The renderer must NOT call out to claude.AskClaudeCLI without a
	//    redaction wrapper. Until 6B's full Haiku integration lands,
	//    P29 protects against an early ad-hoc LLM-prose path.
	if strings.Contains(body, "AskClaudeCLI") && !strings.Contains(body, "// safe-llm: P29") {
		t.Errorf("Pattern P29: AskClaudeCLI used without P29 safe-llm marker; risk of unverified ID hallucination")
	}

	// 3. RenderBriefing must emit prompt_version from briefing_prompts.PromptVersion.
	if !strings.Contains(body, "briefing_prompts.PromptVersion") {
		t.Errorf("Pattern P29: prompt_version not stamped from briefing_prompts.PromptVersion")
	}
}

// TestPattern_P29_PromptInCode — the prompt template MUST live in code,
// not in a SystemConfig row. Code-stored prompts version with the
// binary; DB-stored prompts drift.
func TestPattern_P29_PromptInCode(t *testing.T) {
	root := repoRootP29(t)
	dir := filepath.Join(root, "internal/agents/briefing_prompts")
	if _, err := os.Stat(dir); err != nil {
		t.Errorf("briefing_prompts/ directory missing")
		return
	}
	src, err := os.ReadFile(filepath.Join(dir, "v1.go"))
	if err != nil {
		t.Errorf("briefing_prompts/v1.go missing: %v", err)
		return
	}
	body := string(src)
	for _, want := range []string{"PromptVersion", "PromptTemplate", "FallbackBriefing"} {
		if !strings.Contains(body, want) {
			t.Errorf("Pattern P29: %s missing from briefing_prompts/v1.go", want)
		}
	}
}

func repoRootP29(t *testing.T) string {
	t.Helper()
	wd, _ := os.Getwd()
	for d := wd; d != "/" && d != ""; d = filepath.Dir(d) {
		if _, err := os.Stat(filepath.Join(d, "go.mod")); err == nil {
			return d
		}
	}
	t.Fatalf("repo root not found from %s", wd)
	return ""
}
