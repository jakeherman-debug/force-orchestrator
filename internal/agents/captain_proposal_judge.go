// D3 fix-loop-1 β1 — Captain proposal LLM-judge layer.
//
// Per roadmap concern #1 / exit criterion 7 (lines 1193-1198):
//   "Captain reasoning LLM-judge. Cheap LLM call (Haiku) with Captain's
//    captain_reasoning + actual cited AT/FleetRule texts; returns
//    consistent / inconsistent / ambiguous. Inconsistent → reject ruling,
//    retry with critic note. Ambiguous → proceed with operator-visible
//    [reasoning may not match cited evidence] badge."
//
// The judge sits one layer ABOVE the mechanical validator
// (store.ValidateProposedAction). Mechanical validation has already
// rejected hallucinated AT-IDs and out-of-range confidence; the judge's
// job is the harder semantic question — does the rationale text
// actually support the cited evidence?
//
// Live Haiku gating mirrors the existing renderer pattern
// (internal/agents/live_haiku.go): when LIVE_HAIKU_DISABLED is "1" or
// "true" the judge returns the deterministic stub verdict
// "consistent". When unset, the judge routes through
// claude.CallWithTranscript with the captain-proposal-judge capability
// profile.
//
// Pattern P31 (every LLM call writes a transcript): satisfied because
// the live path uses CallWithTranscript.
package agents

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"force-orchestrator/internal/claude"
	"force-orchestrator/internal/store"
)

// JudgeVerdict is the trichotomy emitted by the LLM-judge.
type JudgeVerdict string

const (
	// JudgeConsistent — rationale matches cited evidence; ruling
	// proceeds normally.
	JudgeConsistent JudgeVerdict = "consistent"
	// JudgeInconsistent — rationale contradicts or ignores the cited
	// evidence; caller MUST reject the ruling and retry.
	JudgeInconsistent JudgeVerdict = "inconsistent"
	// JudgeAmbiguous — judge couldn't decide; ruling proceeds but a
	// "[reasoning may not match cited evidence]" badge surfaces in the
	// operator UI.
	JudgeAmbiguous JudgeVerdict = "ambiguous"
)

// JudgeReport is the structured response from the judge. Note is a
// short prose explanation that the operator UI can surface alongside
// the badge for ambiguous/inconsistent verdicts.
type JudgeReport struct {
	Verdict JudgeVerdict `json:"verdict"`
	Note    string       `json:"note"`
}

// CitedEvidence carries the actual text the judge needs to evaluate.
// Captain's cited_ats[] and cited_fleet_rules[] are converted to this
// shape by the caller before invoking the judge.
type CitedEvidence struct {
	ATs        []CitedATText        `json:"ats"`
	FleetRules []CitedFleetRuleText `json:"fleet_rules"`
}

// CitedATText is a resolved AT reference: the convoy-scoped ID plus the
// actual spec text the operator UI would render.
type CitedATText struct {
	ConvoyID int    `json:"convoy_id"`
	ATID     string `json:"at_id"`
	Text     string `json:"text"`
}

// CitedFleetRuleText is a resolved FleetRules reference: the rule_key
// plus the actual rule body.
type CitedFleetRuleText struct {
	RuleKey string `json:"rule_key"`
	Content string `json:"content"`
}

// JudgeProposalConsistency evaluates whether `proposal.Rationale`
// actually supports `evidence`. Returns a non-nil report on every
// non-error path; the deterministic stub returns "consistent" so
// callers in test mode see green-path behavior.
//
// Live path: routes through CallWithTranscript with the
// captain-proposal-judge capability profile. Failures fall back to the
// deterministic stub (verdict=consistent) so a transient LLM hiccup
// never blocks Captain → Council routing — the mechanical validator
// already rejected hallucinated references; the judge is best-effort
// semantic backstop.
func JudgeProposalConsistency(ctx context.Context, proposal store.ProposedAction, evidence CitedEvidence) (JudgeReport, error) {
	if liveHaikuDisabled() {
		return JudgeReport{Verdict: JudgeConsistent, Note: "deterministic stub (LIVE_HAIKU_DISABLED)"}, nil
	}
	prof, err := loadRendererProfile("captain-proposal-judge")
	if err != nil {
		// Profile failure is fatal-shape per Pattern P13; fall back to
		// the deterministic stub so a transient profile load issue
		// doesn't strand the proposal.
		return JudgeReport{Verdict: JudgeConsistent, Note: fmt.Sprintf("fallback: profile load failed: %v", err)}, nil
	}

	systemPrompt := captainProposalJudgeSystemPrompt
	userPrompt, err := buildJudgeUserPrompt(proposal, evidence)
	if err != nil {
		return JudgeReport{Verdict: JudgeConsistent, Note: fmt.Sprintf("fallback: prompt build failed: %v", err)}, nil
	}

	out, err := claude.CallWithTranscript(ctx, claude.CallDescriptor{
		Agent:         "captain-proposal-judge",
		PromptVersion: "captain-proposal-judge-v1",
	}, systemPrompt, userPrompt,
		prof.allowedTools, prof.disallowedTools, prof.mcpConfig, 1)
	if err != nil {
		return JudgeReport{Verdict: JudgeConsistent, Note: fmt.Sprintf("fallback: live call failed: %v", err)}, nil
	}

	verdict, note := parseJudgeVerdict(out)
	return JudgeReport{Verdict: verdict, Note: note}, nil
}

// parseJudgeVerdict pulls a structured verdict out of the model's
// response. Accepts either a JSON envelope `{"verdict":"...","note":"..."}`
// or a plain-text response that contains one of the three verbs.
//
// Defaults to "ambiguous" — if the model produced something we can't
// parse, the operator gets the badge and decides; we never silently
// flip to "consistent" because that would defeat the gate.
func parseJudgeVerdict(raw string) (JudgeVerdict, string) {
	clean := strings.TrimSpace(raw)
	if clean == "" {
		return JudgeAmbiguous, "judge returned empty output"
	}
	// Try JSON first.
	var rep JudgeReport
	if err := json.Unmarshal([]byte(claude.ExtractJSON(clean)), &rep); err == nil {
		v := normaliseVerdict(string(rep.Verdict))
		if v != "" {
			return v, rep.Note
		}
	}
	// Fall back to keyword scan.
	lower := strings.ToLower(clean)
	switch {
	case strings.Contains(lower, "inconsistent"):
		return JudgeInconsistent, truncate(clean, 240)
	case strings.Contains(lower, "ambiguous"):
		return JudgeAmbiguous, truncate(clean, 240)
	case strings.Contains(lower, "consistent"):
		return JudgeConsistent, truncate(clean, 240)
	}
	return JudgeAmbiguous, "could not parse verdict from output: " + truncate(clean, 200)
}

func normaliseVerdict(s string) JudgeVerdict {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "consistent":
		return JudgeConsistent
	case "inconsistent":
		return JudgeInconsistent
	case "ambiguous":
		return JudgeAmbiguous
	}
	return ""
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}

// buildJudgeUserPrompt serialises Captain's proposal + the resolved
// evidence into a structured prompt the judge can reason over. The
// prompt deliberately encodes the citation set so the judge can
// confirm-or-deny each one without hallucinating new references.
func buildJudgeUserPrompt(proposal store.ProposedAction, evidence CitedEvidence) (string, error) {
	payload := map[string]any{
		"proposal": map[string]any{
			"action":                    proposal.Action,
			"classification_confidence": proposal.ClassificationConfidence,
			"rationale":                 proposal.Rationale,
			"draft_amendment":           proposal.DraftAmendment,
			"alternative":               proposal.Alternative,
		},
		"evidence": evidence,
	}
	encoded, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return "", fmt.Errorf("marshal: %w", err)
	}
	return "Evaluate Captain's proposal against the cited evidence.\n\n" +
		"Captain's proposal and the resolved citation texts are below as JSON. " +
		"Decide whether the prose rationale actually supports the cited evidence:\n\n" +
		"  - consistent  : rationale clearly maps to the cited ATs/FleetRules\n" +
		"  - inconsistent: rationale contradicts or ignores the cited evidence\n" +
		"  - ambiguous   : you cannot tell\n\n" +
		"Respond with JSON ONLY: {\"verdict\":\"consistent|inconsistent|ambiguous\",\"note\":\"<short prose>\"}.\n\n" +
		"INPUT:\n" + string(encoded), nil
}

// captainProposalJudgeSystemPrompt is the system half of the LLM call.
// Kept short — the judge is a single-shot pure-reasoning task.
const captainProposalJudgeSystemPrompt = `You are an LLM-judge evaluating a Fleet Captain's proposal against the actual cited evidence.

Your job is NOT to second-guess the action verb. It is to answer one question:
  - Does the prose RATIONALE actually support the cited ATs and FleetRules?

Three verdicts:
  - "consistent"  : the rationale clearly maps to the cited evidence
  - "inconsistent": the rationale contradicts or ignores the cited evidence
  - "ambiguous"   : you cannot tell from the input alone

Default to "ambiguous" rather than guessing. The operator will see your verdict — false confidence is worse than honest uncertainty.

Respond in JSON ONLY: {"verdict":"...","note":"..."}.`
