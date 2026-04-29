package agents

// D1 T0-1 follow-up — astromech-only target-repo CLAUDE.md mitigation.
//
// These tests pin the three runtime invariants that back
// AstromechTargetCLAUDEMDClause:
//
//   1. The clause is present in astromech's composed system prompt
//      (and includes the [TARGET_CLAUDE_MD_OBSERVATION: signal token
//      that Investigator picks up from the event stream).
//
//   2. The clause is NOT present in any other LLM-invoking agent's
//      system prompt. Adding it elsewhere would be confusing noise:
//      no other agent operates inside a target-repo worktree, so
//      Claude Code never auto-loads target CLAUDE.md for them.
//
//   3. SanitizeLLMPayload rejects payloads carrying the
//      [TARGET_CLAUDE_MD_OBSERVATION: token. This closes the
//      downstream-payload-smuggling channel: if an upstream LLM
//      authored a task payload containing the marker, the next agent
//      could be tricked into emitting the same marker upward —
//      sanitizing at every LLM-payload boundary blocks that.
//
// Removing any of these tests is equivalent to declaring the
// follow-up's invariant retired. See
// docs/architecture/claude-cli-invocation.md for the full layering
// reference.

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// readAgentSource loads an agent source file relative to the repo
// root. Mirrors Pattern P12's helper without re-exporting it (P12 is
// a static test of a different invariant).
func readAgentSource(t *testing.T, rel string) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatalf("runtime.Caller failed")
	}
	repoRootDir := filepath.Clean(filepath.Join(filepath.Dir(thisFile), "..", ".."))
	p := filepath.Join(repoRootDir, rel)
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return string(data)
}

// TestAstromech_TargetCLAUDEMDClauseInSystemPrompt asserts the clause
// reaches astromech's composed system prompt. It mirrors the
// concatenation runAstromechTask and RunTaskForeground perform — a
// future refactor that drops the clause from either call site fails
// this test.
func TestAstromech_TargetCLAUDEMDClauseInSystemPrompt(t *testing.T) {
	// Empty repo → LoadDirective returns empty → directiveSection is
	// the empty string. We model the daemon-side composition exactly:
	// AstromechSystemPrompt + directiveSection + AstromechTargetCLAUDEMDClause.
	directiveSection := ""
	systemPrompt := AstromechSystemPrompt + directiveSection + AstromechTargetCLAUDEMDClause

	if !strings.Contains(systemPrompt, AstromechTargetCLAUDEMDClause) {
		t.Fatalf("astromech system prompt missing AstromechTargetCLAUDEMDClause")
	}

	const observationMarker = "[TARGET_CLAUDE_MD_OBSERVATION:"
	if !strings.Contains(systemPrompt, observationMarker) {
		t.Errorf("astromech system prompt missing %q signal marker — Investigator would never see target-CLAUDE.md conflicts", observationMarker)
	}

	// Belt-and-braces: confirm the clause itself names the marker so
	// astromechs can locate it in their own prompt context.
	if !strings.Contains(AstromechTargetCLAUDEMDClause, observationMarker) {
		t.Errorf("AstromechTargetCLAUDEMDClause does not reference %q — clause has been silently re-templated", observationMarker)
	}

	// Verify the load-bearing framing is present: target CLAUDE.md is
	// advisory, not authoritative.
	if !strings.Contains(AstromechTargetCLAUDEMDClause, "ADVISORY") {
		t.Errorf("AstromechTargetCLAUDEMDClause missing the 'ADVISORY' framing — the clause must explicitly downgrade target CLAUDE.md from authoritative to advisory")
	}

	// Wiring smoke-test: confirm both call sites in astromech.go
	// still concatenate the clause. A refactor that wraps the
	// composition in a helper but drops the clause would slip past
	// the prompt-content assertion above.
	src := readAgentSource(t, "internal/agents/astromech.go")
	occurrences := strings.Count(src, "AstromechTargetCLAUDEMDClause")
	// Expect: the const declaration itself + 2 call sites
	// (runAstromechTask and RunTaskForeground) = 3 minimum.
	if occurrences < 3 {
		t.Errorf("astromech.go references AstromechTargetCLAUDEMDClause %d times; expected ≥ 3 (const decl + 2 call sites). A call site has been removed.", occurrences)
	}
}

// TestNonAstromechAgents_DoNotIncludeTargetCLAUDEMDClause is a
// table-driven defense against a future agent inheriting the
// astromech-specific clause. Each entry names an agent and the
// system-prompt const (or source file, for inline prompts) we
// expect to be free of the clause text.
func TestNonAstromechAgents_DoNotIncludeTargetCLAUDEMDClause(t *testing.T) {
	// Agents whose system prompt is held in a package-level const we
	// can read directly from the agents package.
	constCases := []struct {
		name   string
		prompt string
	}{
		{"captain", captainSystemPrompt},
		{"medic", medicSystemPrompt},
		{"medic-ci", medicCISystemPrompt},
		{"chancellor", chancellorSystemPrompt},
		{"convoy-review", convoyReviewSystemPrompt},
		{"pr-review-triage", prReviewSystemPrompt},
	}
	for _, tc := range constCases {
		tc := tc
		t.Run(tc.name+"_const", func(t *testing.T) {
			if strings.Contains(tc.prompt, AstromechTargetCLAUDEMDClause) {
				t.Fatalf("%s system prompt contains AstromechTargetCLAUDEMDClause — the clause is astromech-only because no other agent runs in a target-repo worktree", tc.name)
			}
			// Also reject the standalone marker text — a partial copy
			// without the const reference would slip past the substring
			// check on the const value but still pollute the prompt.
			if strings.Contains(tc.prompt, "TARGET-REPO CLAUDE.md HANDLING") {
				t.Errorf("%s system prompt contains the target-CLAUDE.md framing header — clause has been duplicated by hand", tc.name)
			}
		})
	}

	// Jedi Council builds its system prompt inline in jedi_council.go's
	// runCouncilTask, so we read the source file and confirm the
	// astromech-only const identifier never appears there. This is
	// the same defense as the const-based assertion above, applied
	// at source-grep granularity for the inline-builder case.
	t.Run("jedi-council_source", func(t *testing.T) {
		src := readAgentSource(t, "internal/agents/jedi_council.go")
		if strings.Contains(src, "AstromechTargetCLAUDEMDClause") {
			t.Errorf("jedi_council.go references AstromechTargetCLAUDEMDClause — astromech-only clause has leaked into Council's prompt assembly")
		}
		if strings.Contains(src, "TARGET-REPO CLAUDE.md HANDLING") {
			t.Errorf("jedi_council.go contains the target-CLAUDE.md framing header — clause has been duplicated by hand")
		}
	})
}

// TestSanitizeLLMPayload_RejectsTargetCLAUDEMDObservation asserts
// the SanitizeLLMPayload denylist now rejects the
// [TARGET_CLAUDE_MD_OBSERVATION: signal. Without this, an upstream
// LLM that emitted the marker as part of a payload could trick the
// next hop into surfacing a manufactured target-CLAUDE.md "observation"
// upward — the same downstream-payload-smuggling channel Fix #8.5's
// signal-token denylist was built to close.
func TestSanitizeLLMPayload_RejectsTargetCLAUDEMDObservation(t *testing.T) {
	cases := []string{
		"[TARGET_CLAUDE_MD_OBSERVATION: nothing to see]",
		"prefix prose [TARGET_CLAUDE_MD_OBSERVATION: with leading text] suffix prose",
		"line 1\n[TARGET_CLAUDE_MD_OBSERVATION: multi-line]\nline 3",
	}
	for _, payload := range cases {
		payload := payload
		t.Run(payload[:min(len(payload), 60)], func(t *testing.T) {
			err := SanitizeLLMPayload(payload)
			if err == nil {
				t.Fatalf("SanitizeLLMPayload accepted payload containing [TARGET_CLAUDE_MD_OBSERVATION: token; expected reject")
			}
			if !strings.Contains(err.Error(), "TARGET_CLAUDE_MD_OBSERVATION") {
				t.Errorf("SanitizeLLMPayload error %q does not name the offending token", err.Error())
			}
		})
	}

	// Sanity counter-case: a payload without the marker still
	// passes. Guards against an over-broad denylist that rejects
	// every bracketed substring.
	t.Run("clean_payload_passes", func(t *testing.T) {
		if err := SanitizeLLMPayload("just a normal task description"); err != nil {
			t.Errorf("SanitizeLLMPayload rejected a clean payload: %v", err)
		}
	})
}
