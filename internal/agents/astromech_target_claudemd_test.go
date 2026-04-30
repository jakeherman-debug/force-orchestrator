package agents

// D1 T0-1 follow-up — astromech-only target-repo CLAUDE.md mitigation.
//
// D3-P1 follow-up C: the legacy AstromechTargetCLAUDEMDClause Go const
// was removed. The clause now lives as FleetRules row
// 'astromech-target-claude-md-advisory' and is concatenated into
// astromech's system prompt at runtime via AppendFleetRulesToPrompt.
// These tests query the FleetRules-rendered prompt rather than the
// (now-deleted) const.
//
// The three runtime invariants under test:
//
//   1. The clause is present in astromech's composed system prompt
//      (and includes the [TARGET_CLAUDE_MD_OBSERVATION: signal token
//      that Investigator picks up from the event stream).
//
//   2. The clause is NOT present in any other LLM-invoking agent's
//      system prompt. Adding it elsewhere would be confusing noise:
//      no other agent operates inside a target-repo worktree, so
//      Claude Code never auto-loads target CLAUDE.md for them. The
//      FleetRules row's agent_scope='astromech' is the structural
//      enforcement; this test is the regression layer that catches a
//      future scope-broadening or const-revival.
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
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"force-orchestrator/internal/store"
)

const targetClaudeMDObservationMarker = "[TARGET_CLAUDE_MD_OBSERVATION:"

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

// astromechAdvisoryFromFleetRules bootstraps a fresh in-memory store,
// runs the FleetRules bootstrap, and returns the assembled astromech
// agent-prompt string. This is the runtime path the daemon takes via
// AppendFleetRulesToPrompt — testing against it is more honest than
// inspecting a Go const.
func astromechAdvisoryFromFleetRules(t *testing.T) string {
	t.Helper()
	db := store.InitHolocronDSN(":memory:")
	t.Cleanup(func() { db.Close() })
	ctx := context.Background()
	if _, err := store.BootstrapFleetRules(ctx, db, ""); err != nil {
		t.Fatalf("bootstrap FleetRules: %v", err)
	}
	prompt, err := AssemblePerAgentPrompt(ctx, db, "astromech")
	if err != nil {
		t.Fatalf("assemble astromech prompt: %v", err)
	}
	return prompt
}

// TestAstromech_TargetCLAUDEMDClauseInSystemPrompt asserts the
// FleetRules-injected clause reaches astromech's composed system
// prompt. The bootstrap path is exactly the one SpawnAstromech uses
// at runtime via AppendFleetRulesToPrompt(ctx, db, "astromech", …).
func TestAstromech_TargetCLAUDEMDClauseInSystemPrompt(t *testing.T) {
	advisory := astromechAdvisoryFromFleetRules(t)

	if !strings.Contains(advisory, "TARGET-REPO CLAUDE.md HANDLING") {
		t.Fatalf("astromech FleetRules-rendered prompt missing the target-CLAUDE.md advisory framing header — the FleetRules row has been silently retired or scoped away from astromech")
	}

	if !strings.Contains(advisory, targetClaudeMDObservationMarker) {
		t.Errorf("astromech FleetRules-rendered prompt missing %q signal marker — Investigator would never see target-CLAUDE.md conflicts", targetClaudeMDObservationMarker)
	}

	// Verify the load-bearing framing is present: target CLAUDE.md is
	// advisory, not authoritative.
	if !strings.Contains(advisory, "ADVISORY") {
		t.Errorf("astromech FleetRules-rendered prompt missing the 'ADVISORY' framing — the clause must explicitly downgrade target CLAUDE.md from authoritative to advisory")
	}

	// Wiring smoke-test: confirm both call sites in astromech.go
	// still call AppendFleetRulesToPrompt with the "astromech" scope.
	// A refactor that wraps the composition in a helper but drops
	// the FleetRules injection would slip past the content assertion
	// above (the prompt would simply lack the clause).
	src := readAgentSource(t, "internal/agents/astromech.go")
	occurrences := strings.Count(src, `AppendFleetRulesToPrompt(ctx, db, "astromech"`)
	// Expect: 2 call sites (runAstromechTask and RunTaskForeground).
	if occurrences < 2 {
		t.Errorf("astromech.go calls AppendFleetRulesToPrompt(ctx, db, \"astromech\", …) %d times; expected ≥ 2 (runAstromechTask + RunTaskForeground). A call site has been removed.", occurrences)
	}

	// And confirm the dead const isn't being reintroduced as a symbol.
	// Comments referencing the historic name are allowed (they document
	// the migration); a `const AstromechTargetCLAUDEMDClause = …` decl
	// or a use-site `+ AstromechTargetCLAUDEMDClause` is not.
	if strings.Contains(src, "const AstromechTargetCLAUDEMDClause") ||
		strings.Contains(src, "+ AstromechTargetCLAUDEMDClause") {
		t.Errorf("astromech.go re-introduced the legacy AstromechTargetCLAUDEMDClause const — D3-P1 follow-up C removed this in favour of the FleetRules row. Restore the FleetRules path instead of reviving the const.")
	}
}

// TestNonAstromechAgents_DoNotIncludeTargetCLAUDEMDClause is a
// table-driven defense against a future agent inheriting the
// astromech-specific clause. Each entry names an agent and the
// system-prompt content (assembled at runtime by querying FleetRules
// with the agent's scope) we expect to be free of the clause text.
func TestNonAstromechAgents_DoNotIncludeTargetCLAUDEMDClause(t *testing.T) {
	// Static agents whose system prompt is held in a package-level
	// const PLUS the FleetRules-injected per-agent extras. The const
	// MUST NOT contain the clause; the FleetRules row's
	// agent_scope='astromech' filters it out from non-astromech
	// extras automatically.
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
			if strings.Contains(tc.prompt, "TARGET-REPO CLAUDE.md HANDLING") {
				t.Errorf("%s system prompt const contains the target-CLAUDE.md framing header — clause has been duplicated by hand", tc.name)
			}
		})
	}

	// FleetRules-side assertion: assembling the per-agent prompt for
	// every non-astromech agent MUST NOT yield the advisory clause.
	// agent_scope='astromech' is the structural mechanism; this is
	// the regression layer that catches a future scope broadening
	// (e.g., agent_scope='all' would silently leak the clause).
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	ctx := context.Background()
	if _, err := store.BootstrapFleetRules(ctx, db, ""); err != nil {
		t.Fatalf("bootstrap FleetRules: %v", err)
	}
	for _, tc := range constCases {
		tc := tc
		t.Run(tc.name+"_fleetrules", func(t *testing.T) {
			prompt, err := AssemblePerAgentPrompt(ctx, db, tc.name)
			if err != nil {
				t.Fatalf("assemble %s prompt: %v", tc.name, err)
			}
			if strings.Contains(prompt, "TARGET-REPO CLAUDE.md HANDLING") {
				t.Errorf("FleetRules-rendered %s prompt contains the target-CLAUDE.md framing header — agent_scope filter has broken or the row's scope was widened beyond 'astromech'", tc.name)
			}
		})
	}

	// Jedi Council builds its system prompt inline in jedi_council.go's
	// runCouncilTask, so we read the source file and confirm the
	// removed const identifier never reappears (revival guard).
	t.Run("jedi-council_source", func(t *testing.T) {
		src := readAgentSource(t, "internal/agents/jedi_council.go")
		if strings.Contains(src, "AstromechTargetCLAUDEMDClause") {
			t.Errorf("jedi_council.go references the legacy AstromechTargetCLAUDEMDClause const — astromech-only clause has been revived by hand")
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
