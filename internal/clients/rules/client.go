// Package rules defines the client interface for the FleetRules
// service — promoted-from-experiment behaviour rules that change how
// agents act (e.g. "Captain rejects PRs > 800 LoC"). FleetRules is
// the post-D3 mechanism by which experiments graduate from "we tested
// it on a paired run" to "this is now the fleet default."
//
// Implementation timeline:
//   - D0 (this commit): interface definition + ErrNotImplemented stubs.
//   - D3 (paired-runs + Engineering Corps deliverable): the real
//     in-process implementation lands here, sourced from the
//     FleetRules table the deliverable introduces. Agents (Captain,
//     Council, Medic) consume this interface in the same way they
//     read CLAUDE.md fragments today.
//   - Later: gRPC backing for shared multi-tenant operation.
//
// Pattern P16 (audit_pattern_p16_clients_interfaces_test.go) enforces
// that production agent code references the Client interface only.
package rules

import (
	"context"
	"errors"
)

// Client is the contract between agents and the FleetRules service.
// The shape mirrors what an LLM-invoking agent needs at claim time:
// "give me the rules in my category right now."
type Client interface {
	// ActiveRules returns every rule active for the given agent and
	// category. Categories are stable per-agent (e.g. "captain-scope",
	// "council-quality", "medic-triage"). Returns an empty slice (not
	// nil) when no rules apply.
	ActiveRules(ctx context.Context, agent, category string) ([]Rule, error)

	// RuleByKey looks up a single rule by its stable key. Returns
	// ErrRuleNotFound on miss.
	RuleByKey(ctx context.Context, ruleKey string) (Rule, error)

	// PromoteFromExperiment transitions a winning experiment into a
	// permanent rule. The supplied experiment must already have a
	// recorded outcome; D3 enforces the global-holdout precondition.
	PromoteFromExperiment(ctx context.Context, experimentID int, p PromotionRequest) (Rule, error)

	// Retire turns off a rule. The history is preserved; subsequent
	// ActiveRules calls in the same category no longer return it.
	Retire(ctx context.Context, ruleKey, reason string) error
}

// Rule is the in-memory shape of a FleetRules row.
type Rule struct {
	ID            int
	Key           string // stable identifier (e.g. "captain-scope-800loc")
	Agent         string // owning agent (e.g. "captain")
	Category      string // bucket within the agent (e.g. "scope-cap")
	Body          string // free-form rule text — what the agent prepends to its system prompt
	PromotedFrom  int    // experiment ID that produced this rule, 0 if hand-authored
	ActivatedAt   string // SQLite datetime
	RetiredAt     string // empty when active
	RetireReason  string // empty when active
}

// PromotionRequest carries the fields needed to graduate a winning
// experiment into a rule. D3 owns the validation invariants; D0 only
// fixes the shape.
type PromotionRequest struct {
	Key      string
	Agent    string
	Category string
	Body     string
}

var (
	// ErrRuleNotFound — RuleByKey or Retire called with an unknown key.
	ErrRuleNotFound = errors.New("rules: rule not found")

	// ErrInvalidPromotion — D3 rejected a promotion (e.g. holdout
	// violation, no recorded outcome on the experiment).
	ErrInvalidPromotion = errors.New("rules: promotion request rejected")

	// ErrNotImplemented — D0 stub guard.
	ErrNotImplemented = errors.New("rules: not implemented (D3 deliverable)")
)
