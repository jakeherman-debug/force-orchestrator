package agents

import (
	"context"
	"fmt"

	"force-orchestrator/internal/agents/adversarial"
	"force-orchestrator/internal/agents/capabilities"
	"force-orchestrator/internal/claude"
)

// adversarial_wiring.go wires production CriticFns for the three D3
// Phase 5 adversarial-pair subjects: Council, Medic, and ConvoyReview.
//
// Each CriticFn:
//   * loads the agent's `<name>-critic` capabilities profile (Pattern P13;
//     LoadProfile fails closed)
//   * builds a critic-side prompt that frames the question as "find
//     reasons the primary's decision is wrong"
//   * wraps the primary's reasoning + outcome via WrapUserContent
//     (Pattern P12) so prompt-injection sentinels are honored
//   * shells out via claude.AskClaudeCLIContext (Pattern: ctx-aware
//     LLM calls only) and parses the response via strictJSONUnmarshal
//   * returns a CriticOutcome with a DIFFERENT prompt-version key
//     than the primary's (anti-cheat: pair runner rejects identical
//     versions at write time)
//
// These critics are NOT registered automatically — call
// EnableAdversarialPairing(ctx) once at daemon startup to opt-in.
// Leaving registration manual lets the operator A/B-test the pairing
// itself before flipping it on for the whole fleet.

// EnableAdversarialPairing wires production critics for Council,
// Medic, and ConvoyReview. Idempotent — re-calling overwrites with
// the same closures.
//
// On capability-profile load failure, returns the error WITHOUT
// registering anything; callers should treat that as a hard
// daemon-startup error (a critic that can't load its profile is a
// misconfigured fleet).
func EnableAdversarialPairing(ctx context.Context) error {
	councilCritic, err := capabilities.LoadProfile("council-critic")
	if err != nil {
		return fmt.Errorf("EnableAdversarialPairing: council-critic profile: %w", err)
	}
	medicCritic, err := capabilities.LoadProfile("medic-critic")
	if err != nil {
		return fmt.Errorf("EnableAdversarialPairing: medic-critic profile: %w", err)
	}
	convoyReviewCritic, err := capabilities.LoadProfile("convoy-review-critic")
	if err != nil {
		return fmt.Errorf("EnableAdversarialPairing: convoy-review-critic profile: %w", err)
	}

	adversarial.RegisterCritic(adversarial.AgentCouncil, makeCouncilCritic(councilCritic))
	adversarial.RegisterCritic(adversarial.AgentMedic, makeMedicCritic(medicCritic))
	adversarial.RegisterCritic(adversarial.AgentConvoyReview, makeConvoyReviewCritic(convoyReviewCritic))
	return nil
}

// councilCriticPromptVersion / medicCriticPromptVersion /
// convoyReviewCriticPromptVersion are the prompt-version tags
// persisted to AdversarialPairings.prompt_version_critic. They MUST
// differ from the primary's prompt-version tag — the pair runner
// rejects identical versions as sham pairings.
const (
	councilCriticPromptVersion       = "council-critic-v1"
	medicCriticPromptVersion         = "medic-critic-v1"
	convoyReviewCriticPromptVersion  = "convoy-review-critic-v1"
)

// makeCouncilCritic returns a CriticFn that runs the council-critic
// prompt: given a Council "approved" decision, find reasons it should
// have been "rejected" (and vice versa). The critic answers in the
// SAME structured JSON shape as the primary so outcomesAgree can
// directly compare.
func makeCouncilCritic(profile *capabilities.Profile) adversarial.CriticFn {
	return func(ctx context.Context, primary adversarial.PrimaryDecision) (adversarial.CriticOutcome, error) {
		systemPrompt := `You are the Council Critic — an adversarial reviewer paired with the Council.
Your job is to find REASONS THE COUNCIL'S DECISION MIGHT BE WRONG.

The Council just produced a decision on a code-review task. You see
their decision and reasoning. Your task: re-evaluate from the opposite
framing. If they approved, look for quality issues, scope creep,
missing tests, hidden coupling, regressions. If they rejected, look
for whether the rejection is over-strict or whether the diff actually
satisfies the task.

Respond with the SAME JSON shape the Council uses: {"approved":bool,"feedback":"..."}.
The "approved" field is REQUIRED. Your goal is honest disagreement
when warranted, not contrarianism for its own sake — agreeing is fine
when the Council's decision is genuinely correct.`

		userPrompt := fmt.Sprintf(
			"COUNCIL_PRIMARY_OUTCOME:\n%s\n\nCOUNCIL_PRIMARY_REASONING:\n%s",
			WrapUserContent("primary_outcome", primary.Outcome),
			WrapUserContent("primary_reasoning", primary.Reasoning),
		)

		mcpConfig, _ := profile.MCPConfigArg()
		response, err := claude.CallWithTranscript(ctx, claude.CallDescriptor{
			Agent:         "council-critic",
			PromptVersion: councilCriticPromptVersion,
		}, systemPrompt, userPrompt,
			profile.AllowedToolsArg(), profile.DisallowedToolsArg(), mcpConfig, 3)
		if err != nil {
			return adversarial.CriticOutcome{}, fmt.Errorf("council-critic: %w", err)
		}
		clean := claude.ExtractJSON(response)
		// Validate the response is parseable JSON (no schema validation
		// here — the primary and critic both produce CouncilRuling-shaped
		// JSON; outcomesAgree compares textually).
		if clean == "" {
			return adversarial.CriticOutcome{}, fmt.Errorf("council-critic: empty response")
		}
		return adversarial.CriticOutcome{
			Outcome:       clean,
			PromptVersion: councilCriticPromptVersion,
		}, nil
	}
}

func makeMedicCritic(profile *capabilities.Profile) adversarial.CriticFn {
	return func(ctx context.Context, primary adversarial.PrimaryDecision) (adversarial.CriticOutcome, error) {
		systemPrompt := `You are the Medic Critic — an adversarial reviewer paired with Medic triage.
Your job is to find REASONS THE MEDIC DECISION MIGHT BE WRONG.

Medic just produced a triage decision (requeue / shard / cleanup /
escalate) on a failing task. You see their decision and reasoning.
Re-evaluate: was the chosen action the smallest right action? Was
escalate too eager? Was requeue too patient? Was the shard breakdown
correct?

Respond with the SAME JSON shape Medic uses:
{"decision":"requeue|shard|cleanup|escalate","reasoning":"...","guidance":"...","shards":[...],"cleanup_target_branch":"...","cleanup_agents":[...],"escalation":"..."}.
The "decision" field is REQUIRED.`

		userPrompt := fmt.Sprintf(
			"MEDIC_PRIMARY_OUTCOME:\n%s\n\nMEDIC_PRIMARY_REASONING:\n%s",
			WrapUserContent("primary_outcome", primary.Outcome),
			WrapUserContent("primary_reasoning", primary.Reasoning),
		)

		mcpConfig, _ := profile.MCPConfigArg()
		response, err := claude.CallWithTranscript(ctx, claude.CallDescriptor{
			Agent:         "medic-critic",
			PromptVersion: medicCriticPromptVersion,
		}, systemPrompt, userPrompt,
			profile.AllowedToolsArg(), profile.DisallowedToolsArg(), mcpConfig, 3)
		if err != nil {
			return adversarial.CriticOutcome{}, fmt.Errorf("medic-critic: %w", err)
		}
		clean := claude.ExtractJSON(response)
		if clean == "" {
			return adversarial.CriticOutcome{}, fmt.Errorf("medic-critic: empty response")
		}
		return adversarial.CriticOutcome{
			Outcome:       clean,
			PromptVersion: medicCriticPromptVersion,
		}, nil
	}
}

func makeConvoyReviewCritic(profile *capabilities.Profile) adversarial.CriticFn {
	return func(ctx context.Context, primary adversarial.PrimaryDecision) (adversarial.CriticOutcome, error) {
		systemPrompt := `You are the ConvoyReview Critic — an adversarial reviewer paired with ConvoyReview.
Your job is to find REASONS THE PROPOSED FIX TASK WON'T ACTUALLY CLOSE THE GAP.

ConvoyReview just decided to spawn a fix task for a finding. You see
the finding + the proposed fix-task description. Re-evaluate: would
the proposed fix actually resolve the finding? Is it spawning busywork
that won't change the convoy's pass/fail status? Is the finding
itself a true gap or a false positive?

Respond with the SAME JSON shape ConvoyReview uses for findings:
{"finding":"...","fix_task":"...","fingerprint":"..."}, plus an
explicit field "would_close_gap": bool indicating your verdict.`

		userPrompt := fmt.Sprintf(
			"CONVOY_REVIEW_PRIMARY_OUTCOME:\n%s\n\nCONVOY_REVIEW_PRIMARY_REASONING:\n%s",
			WrapUserContent("primary_outcome", primary.Outcome),
			WrapUserContent("primary_reasoning", primary.Reasoning),
		)

		mcpConfig, _ := profile.MCPConfigArg()
		response, err := claude.CallWithTranscript(ctx, claude.CallDescriptor{
			Agent:         "convoy-review-critic",
			PromptVersion: convoyReviewCriticPromptVersion,
		}, systemPrompt, userPrompt,
			profile.AllowedToolsArg(), profile.DisallowedToolsArg(), mcpConfig, 3)
		if err != nil {
			return adversarial.CriticOutcome{}, fmt.Errorf("convoy-review-critic: %w", err)
		}
		clean := claude.ExtractJSON(response)
		if clean == "" {
			return adversarial.CriticOutcome{}, fmt.Errorf("convoy-review-critic: empty response")
		}
		return adversarial.CriticOutcome{
			Outcome:       clean,
			PromptVersion: convoyReviewCriticPromptVersion,
		}, nil
	}
}
