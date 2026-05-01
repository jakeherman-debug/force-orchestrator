package agents

import (
	"context"
	"testing"

	"force-orchestrator/internal/store"
)

// TestJudgeProposalConsistency_DeterministicStub — under
// LIVE_HAIKU_DISABLED (set by testmain), the judge always returns
// "consistent" so unit tests don't burn LLM calls.
func TestJudgeProposalConsistency_DeterministicStub(t *testing.T) {
	t.Setenv("LIVE_HAIKU_DISABLED", "1")

	proposal := store.ProposedAction{
		Action: "approve",
		CitedATs: []store.CitedAT{
			{ConvoyID: 1, ATID: "AT-001"},
		},
		CitedFleetRules:          []string{"rule.foo"},
		ClassificationConfidence: 0.8,
		Rationale:                "satisfies AT-001",
	}
	evidence := CitedEvidence{
		ATs: []CitedATText{
			{ConvoyID: 1, ATID: "AT-001", Text: "must implement helper X"},
		},
		FleetRules: []CitedFleetRuleText{
			{RuleKey: "rule.foo", Content: "do the foo"},
		},
	}
	rep, err := JudgeProposalConsistency(context.Background(), proposal, evidence)
	if err != nil {
		t.Fatalf("JudgeProposalConsistency: %v", err)
	}
	if rep.Verdict != JudgeConsistent {
		t.Errorf("expected consistent stub verdict, got %q", rep.Verdict)
	}
}

// TestParseJudgeVerdict_JSONShape — judge response in JSON envelope.
func TestParseJudgeVerdict_JSONShape(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want JudgeVerdict
	}{
		{"consistent_json", `{"verdict":"consistent","note":"ok"}`, JudgeConsistent},
		{"inconsistent_json", `{"verdict":"inconsistent","note":"contradicts AT-001"}`, JudgeInconsistent},
		{"ambiguous_json", `{"verdict":"ambiguous","note":"unclear"}`, JudgeAmbiguous},
		{"inconsistent_with_fenced_json", "```json\n{\"verdict\":\"inconsistent\",\"note\":\"x\"}\n```", JudgeInconsistent},
		{"plain_consistent", "consistent — rationale matches cited AT-001", JudgeConsistent},
		{"plain_inconsistent", "this is inconsistent with the evidence", JudgeInconsistent},
		{"plain_ambiguous", "looks ambiguous to me", JudgeAmbiguous},
		{"empty", "", JudgeAmbiguous},
		{"unparseable", "I'm sorry I cannot help with this", JudgeAmbiguous},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, _ := parseJudgeVerdict(tc.raw)
			if got != tc.want {
				t.Errorf("parseJudgeVerdict(%q) = %q, want %q", tc.raw, got, tc.want)
			}
		})
	}
}

// TestParseJudgeVerdict_NotePropagated — JSON note field is preserved
// for the operator UI.
func TestParseJudgeVerdict_NotePropagated(t *testing.T) {
	_, note := parseJudgeVerdict(`{"verdict":"inconsistent","note":"AT-001 says X but rationale says Y"}`)
	if note != "AT-001 says X but rationale says Y" {
		t.Errorf("note not preserved: got %q", note)
	}
}

// TestBuildJudgeUserPrompt_IncludesEvidence — the prompt embeds the
// cited evidence so the judge has the actual texts in front of it.
func TestBuildJudgeUserPrompt_IncludesEvidence(t *testing.T) {
	proposal := store.ProposedAction{
		Action:                   "approve",
		Rationale:                "satisfies AT-007",
		ClassificationConfidence: 0.9,
	}
	evidence := CitedEvidence{
		ATs: []CitedATText{
			{ConvoyID: 42, ATID: "AT-007", Text: "REQUIRED: implement feature Z"},
		},
		FleetRules: []CitedFleetRuleText{
			{RuleKey: "rule.beep", Content: "beep before boop"},
		},
	}
	prompt, err := buildJudgeUserPrompt(proposal, evidence)
	if err != nil {
		t.Fatalf("buildJudgeUserPrompt: %v", err)
	}
	for _, want := range []string{"AT-007", "REQUIRED: implement feature Z", "rule.beep", "beep before boop", "satisfies AT-007"} {
		if !contains(prompt, want) {
			t.Errorf("prompt missing %q\n--- prompt ---\n%s", want, prompt)
		}
	}
}

func contains(s, substr string) bool {
	for i := 0; i+len(substr) <= len(s); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
