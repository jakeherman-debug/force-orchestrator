// Package experiments defines the client interface for the paired-runs
// / Engineering Corps treatment-application service.
//
// Implementation timeline:
//   - D0 (this commit): interface definition + ErrNotImplemented stubs.
//   - D3 (paired-runs + Engineering Corps deliverable): the real
//     in-process implementation lands here. Engineering Corps's hot
//     path (`treatments.Apply`) goes through experiments.Client from
//     day 1 — every Claude CLI / git op routes through Apply so a
//     treatment can rewrite the call before it lands.
//   - Later: a gRPC backing if Engineering Corps becomes a multi-tenant
//     shared service.
//
// Pattern P16 (audit_pattern_p16_clients_interfaces_test.go) enforces
// that production agent code references the Client interface only.
package experiments

import (
	"context"
	"errors"
)

// Client is the contract between agents and the experiments service.
// CallDescriptor / Assignment / Outcome are the load-bearing data
// types; their fields are provisional until D3 freezes the schema.
type Client interface {
	// Apply rewrites a CallDescriptor according to any active
	// experiment treatments and records the assignments it made. The
	// returned descriptor is what the caller actually invokes; the
	// returned []Assignment records "this experiment slotted this
	// arm into this run" so D3's metrics service can score later.
	//
	// In the D0 stub this returns ErrNotImplemented so callers see a
	// real error rather than the no-treatment fall-through.
	Apply(ctx context.Context, call CallDescriptor) (CallDescriptor, []Assignment, error)

	// Outcome returns the recorded outcome for an experiment run. D3
	// uses this for the post-run metrics rollup; the D-side tooling
	// (force experiments report) reads it for the operator's dashboard.
	Outcome(ctx context.Context, experimentID int) (Outcome, error)

	// Register declares a new experiment. The body is bounded by D3's
	// global-holdout invariant — the registered experiment cannot
	// spend more than its allotted budget across paired runs.
	Register(ctx context.Context, exp ExperimentDecl) (int, error)

	// Cancel stops a running experiment. Subsequent Apply calls
	// against the same key fall through unmodified.
	Cancel(ctx context.Context, experimentID int, reason string) error
}

// CallDescriptor describes the operation about to be performed —
// "claude -p with this prompt" or "git push origin <branch>". The
// descriptor is opaque enough that an experiment can rewrite it
// (e.g. swap the prompt template or the model).
type CallDescriptor struct {
	Kind    string            // "claude" | "git" | "gh" | future kinds
	Subject string            // e.g. "captain-review" for a claude call
	Args    []string          // CLI args (claude/git/gh)
	Inputs  map[string]string // structured per-kind inputs (e.g. "model", "prompt", "system")
	Repo    string            // repo name (where applicable)
	TaskID  int               // BountyBoard.id (where applicable)
}

// Assignment records that one experiment slotted one arm into the
// supplied call. Multiple experiments may register against the same
// call kind; D3's mixer ensures their assignments compose.
type Assignment struct {
	ExperimentID int
	ArmKey       string // e.g. "control" / "treatment-A"
	Notes        string // free-form, for the audit trail
}

// Outcome is the post-run rollup for an experiment. Score is the
// aggregate value of the metric the experiment optimises for; D3's
// metrics.Client computes it.
type Outcome struct {
	ExperimentID  int
	ArmKey        string
	RunCount      int
	Score         float64 // metric-defined units; comparable across arms within one experiment
	BudgetSpentUS float64 // dollars consumed
}

// ExperimentDecl describes a new experiment at registration time.
type ExperimentDecl struct {
	Key            string  // human-readable identifier (e.g. "captain-prompt-v3")
	Hypothesis     string  // why we expect treatment to win
	Arms           []string
	BudgetUSCap    float64 // hard ceiling on spend across the experiment's lifetime
	OwningTeam     string
}

var (
	// ErrExperimentNotFound — Outcome / Cancel called for an unknown ID.
	ErrExperimentNotFound = errors.New("experiments: experiment not found")

	// ErrBudgetExhausted — Apply refused to assign a treatment because
	// the experiment's BudgetUSCap is exhausted.
	ErrBudgetExhausted = errors.New("experiments: experiment budget exhausted")

	// ErrNotImplemented — D0 stub guard. D3 fills in the real bodies.
	ErrNotImplemented = errors.New("experiments: not implemented (D3 deliverable)")
)
