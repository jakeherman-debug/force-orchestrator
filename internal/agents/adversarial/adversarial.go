// Package adversarial implements adversarial-pair decision evaluation
// for high-stakes auto-execute layers (Council approve/reject, Medic
// requeue/shard/cleanup/escalate, ConvoyReview fix-task spawn).
//
// The shape:
//   - The primary agent (Council, Medic, ConvoyReview) makes a decision
//     using its production prompt.
//   - A critic LLM call runs in parallel with a DIFFERENT prompt (the
//     critic's framing is "evaluate whether this decision is correct";
//     anti-cheat invariant: critic's prompt_version MUST differ from
//     the primary's, otherwise the pair is a sham — same model + same
//     prompt produces nearly identical outputs).
//   - Both outcomes are persisted to the AdversarialPairings table with
//     prompt_version_primary / prompt_version_critic populated.
//   - On disagreement, SurfaceDisagreementToOperator writes a Fleet_Mail
//     row + dashboard banner so the operator can adjudicate.
//
// Package surface (filled in by sub-agent B):
//   - pair.go     — RunAdversarialPair, SurfaceDisagreementToOperator
//   - council.go  — wires Council's approval/rejection paths
//   - medic.go    — wires Medic's requeue/shard/cleanup/escalate dispatch
//   - convoy.go   — wires ConvoyReview's fix-task spawn
package adversarial

import (
	"context"
	"database/sql"
	"errors"
)

// Agent identifies the primary auto-execute layer being evaluated.
// Used as the AdversarialPairings.agent column value.
type Agent string

const (
	AgentCouncil       Agent = "council"
	AgentMedic         Agent = "medic"
	AgentConvoyReview  Agent = "convoy_review"
)

// PrimaryDecision is the outcome the primary agent produced. Both
// `Outcome` (the structured decision JSON) and `PromptVersion` (the
// prompt revision that produced it) are required — the prompt_version
// is the anti-cheat axis (critic must use a different one).
type PrimaryDecision struct {
	// DecisionID is the upstream task / bounty ID being evaluated.
	DecisionID int64

	// Agent is which auto-execute layer made the decision.
	Agent Agent

	// Outcome is the primary's structured decision payload (typically
	// JSON: e.g. {"approved":true,"feedback":"..."} for Council;
	// {"decision":"requeue",...} for Medic).
	Outcome string

	// Reasoning is the natural-language reasoning the primary produced
	// alongside Outcome. Fed to the critic (wrapped via WrapUserContent
	// per Pattern P12) so the critic can evaluate the chain of thought,
	// not just the conclusion.
	Reasoning string

	// PromptVersion is the prompt revision tag (e.g. "council-v3") that
	// produced Outcome. MUST differ from the critic's prompt version
	// (anti-cheat). Persisted to AdversarialPairings.prompt_version_primary.
	PromptVersion string
}

// Pair is the AdversarialPairings row materialized as a Go value. Used
// by SurfaceDisagreementToOperator and by audit / replay tooling.
type Pair struct {
	ID                     int64
	DecisionID             int64
	Agent                  Agent
	PrimaryOutcome         string
	CriticOutcome          string
	PromptVersionPrimary   string
	PromptVersionCritic    string
	Agreement              bool
	SurfacedAt             string
	OperatorResolution     string
	CreatedAt              string
}

// ErrIdenticalPromptVersions is the anti-cheat sentinel. Returned by
// RunAdversarialPair when the critic's prompt-version tag is empty or
// matches the primary's. A pair where both arms used the same prompt is
// not adversarial; D3 P5 rejects it at write time rather than allowing
// sham agreements to inflate the agreement-rate metric.
var ErrIdenticalPromptVersions = errors.New("adversarial: critic prompt version must differ from primary")

// RunAdversarialPair is the production entry point that loads the
// agent's wired CriticFn and delegates to RunAdversarialPairWith. The
// wiring (which CriticFn each agent uses) lives in council.go,
// medic.go, convoy.go.
//
// When called for an Agent value with no wired critic, returns
// ErrIdenticalPromptVersions (fail-closed contract). Tests should call
// RunAdversarialPairWith directly with a stub CriticFn.
func RunAdversarialPair(ctx context.Context, db *sql.DB, primary PrimaryDecision) (*Pair, error) {
	critic, ok := wiredCritics[primary.Agent]
	if !ok || critic == nil {
		return nil, ErrIdenticalPromptVersions
	}
	return RunAdversarialPairWith(ctx, db, primary, critic)
}

// SurfaceDisagreementToOperator is a convenience wrapper that surfaces
// to the canonical "operator" inbox.
func SurfaceDisagreementToOperator(ctx context.Context, db *sql.DB, pairID int64) error {
	return SurfaceDisagreementToOperatorWith(ctx, db, pairID, "operator")
}

// wiredCritics is populated by council.go / medic.go / convoy.go via
// RegisterCritic. Keeping this as a package-level var (vs a registry
// struct) is intentional — these are static, set once at package init,
// and reading without a lock from RunAdversarialPair is safe by
// construction.
var wiredCritics = map[Agent]CriticFn{}

// RegisterCritic wires a CriticFn for an Agent. Called from
// council.go / medic.go / convoy.go's init() once each. Re-registering
// for the same Agent is allowed (lets tests inject deterministic
// stubs); production registration is one-time.
func RegisterCritic(agent Agent, fn CriticFn) {
	wiredCritics[agent] = fn
}
