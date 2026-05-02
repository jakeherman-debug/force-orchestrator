// Package stagegate defines the plug interface and registry for D5.5
// staged-convoy gates. A Gate evaluates whether a ConvoyStage can advance
// from AwaitingGate → GatePassed (or → Failed). Leaf gates (soak_minutes,
// operator_confirm, null) and compound gates (all_of, any_of) all
// implement the same interface — the registry treats them uniformly so
// future leaf types plug in without re-architecting.
//
// The package lives at top-level so it's importable by both
// internal/agents (the convoy-stage-watch dog) and internal/store
// (helpers that need the registered gate-type vocabulary) without
// creating an import cycle.
//
// D5.5 Phase 1 ships the 5 baseline types: soak_minutes, operator_confirm,
// null, all_of, any_of. Phase 3 adds the 4 advanced leaves
// (probe_endpoint, release_label_present, datadog_metric_threshold,
// databricks_query_threshold) — they register against the same registry
// without changing the dispatch surface.
package stagegate

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// MaxNestingDepth is the cap on compound-gate recursion depth. Five
// levels is the spec's pathological-config guardrail — deeper trees are
// almost certainly a planning bug rather than legitimate logic. The
// planner rejects deeper specs at convoy-creation time; the runtime
// enforces the same cap defensively in case a malformed gate spec
// reaches Evaluate.
const MaxNestingDepth = 5

// ErrPending is the sentinel returned by Gate.Evaluate when the gate
// hasn't resolved yet — neither pass nor fail. The convoy-stage-watch
// dog treats this as "leave the stage in AwaitingGate; check again next
// tick." Soak gates return ErrPending while the soak window is open;
// operator-confirm returns ErrPending until the operator clicks the
// dashboard advance button.
var ErrPending = errors.New("stagegate: pending")

// Gate is the plug interface every stage gate type must implement.
//
// Evaluate returns:
//   - passed=true:               gate resolved positively; stage advances
//                                to GatePassed.
//   - passed=false, err=nil:     gate resolved negatively; stage moves
//                                to Failed.
//   - passed=false, err=Pending: gate hasn't resolved yet; stage stays
//                                in AwaitingGate and the dog re-checks
//                                next tick.
//   - err non-nil and non-Pending: a structural error in evaluation
//                                (bad config, dependency failure, etc.).
//                                Surfaced to operator via the dog's
//                                error-mail path.
type Gate interface {
	// Type returns the gate type string used in
	// ConvoyStages.gate_type and in the JSON gate-spec's "type" field.
	// Must be unique across all registered gates.
	Type() string

	// Evaluate is invoked by the convoy-stage-watch dog (or, in P2,
	// by ConvoyReview) for a stage in AwaitingGate. The stage
	// argument carries the parsed gate config plus enough context
	// (timing, convoy id) for the gate to make its decision.
	Evaluate(ctx context.Context, db *sql.DB, stage StageContext) (passed bool, reason string, err error)
}

// StageContext is the minimal subset of a ConvoyStages row + convoy
// context passed to Gate.Evaluate. Decoupling the gate from the full
// store.ConvoyStage type keeps the gate package free of a store import
// (avoiding circular deps) and keeps gates testable in isolation.
type StageContext struct {
	StageID        int
	ConvoyID       int
	StageNum       int
	Status         string // current row status (Open, AwaitingGate, ...)
	GateType       string // matches Gate.Type()
	GateConfig     json.RawMessage
	AllPRsMergedAt time.Time // zero-value if the stage hasn't reached AllPRsMerged yet
	OpenedAt       time.Time
	GateTimeoutMin int
}

// gateSpec is the JSON shape passed to EvaluateGateConfig. Leaf gates
// carry a "config" object; compound gates carry a "gates" array of
// nested gateSpecs. We model both shapes here so the recursion is
// type-safe.
type gateSpec struct {
	Type   string            `json:"type"`
	Config json.RawMessage   `json:"config,omitempty"`
	Gates  []json.RawMessage `json:"gates,omitempty"`
}

// Registry maps gate_type strings to Gate implementations. Compound
// gates ("all_of", "any_of") are registered alongside leaves and look
// up their children via the same registry recursively. The registry is
// goroutine-safe for reads after construction; callers should register
// at startup and treat the registry as read-only thereafter.
type Registry struct {
	gates  map[string]Gate
	logger Logger
}

// Logger is the minimal logging interface the registry uses for
// non-fatal warnings (e.g. compound-with-single-child). Matches the
// dog logger shape so callers can pass the same logger.
type Logger interface {
	Printf(format string, args ...any)
}

type nopLogger struct{}

func (nopLogger) Printf(string, ...any) {}

// NewRegistry creates an empty registry. Callers register Gate
// implementations before invoking Evaluate. Pass nil for logger to
// suppress warnings (tests typically do this).
func NewRegistry() *Registry {
	return &Registry{gates: map[string]Gate{}, logger: nopLogger{}}
}

// SetLogger swaps the warning logger. Returns the registry so callers
// can chain.
func (r *Registry) SetLogger(l Logger) *Registry {
	if l == nil {
		r.logger = nopLogger{}
	} else {
		r.logger = l
	}
	return r
}

// Register adds a Gate to the registry. Panics on duplicate
// registration — duplicates are always a wiring bug, and a panic at
// startup is preferable to a silent override that surfaces months
// later as the wrong gate type evaluating a stage. Callers register
// once at daemon startup; the panic fires before any work begins.
func (r *Registry) Register(g Gate) {
	if g == nil {
		panic("stagegate: Register(nil)")
	}
	t := g.Type()
	if t == "" {
		panic("stagegate: Gate.Type() returned empty string")
	}
	if _, exists := r.gates[t]; exists {
		panic(fmt.Sprintf("stagegate: duplicate registration of %q", t))
	}
	r.gates[t] = g
}

// Lookup returns the Gate registered for the given type string and
// whether the lookup hit. Compound gates use this to dispatch to
// their children.
func (r *Registry) Lookup(gateType string) (Gate, bool) {
	g, ok := r.gates[gateType]
	return g, ok
}

// Names returns the sorted set of registered gate type names. Useful
// for tests and dashboards.
func (r *Registry) Names() []string {
	out := make([]string, 0, len(r.gates))
	for k := range r.gates {
		out = append(out, k)
	}
	return out
}

// EvaluateGateConfig is the entry point for the convoy-stage-watch dog
// (and, in P2, for ConvoyReview). It takes a parsed gate spec — the
// JSON shape stored in ConvoyStages.gate_config_json wrapped with the
// gate's type — and dispatches through the registry. Compound gates
// recurse through this same method, threading the depth counter.
//
// gateSpecJSON is the JSON object:
//
//	{"type": "soak_minutes", "config": {"minutes": 60}}
//	{"type": "all_of", "gates": [<gateSpec>, <gateSpec>]}
//
// depth is the current recursion level; callers from outside the
// registry pass 0. A depth >= MaxNestingDepth returns an error.
func (r *Registry) EvaluateGateConfig(ctx context.Context, db *sql.DB, stage StageContext, gateSpecJSON json.RawMessage, depth int) (passed bool, reason string, err error) {
	if depth >= MaxNestingDepth {
		return false, "", fmt.Errorf("stagegate: nesting depth %d exceeds cap %d", depth, MaxNestingDepth)
	}
	if len(gateSpecJSON) == 0 {
		return false, "", fmt.Errorf("stagegate: empty gate spec")
	}
	var spec gateSpec
	if uErr := json.Unmarshal(gateSpecJSON, &spec); uErr != nil {
		return false, "", fmt.Errorf("stagegate: parse gate spec: %w", uErr)
	}
	if spec.Type == "" {
		return false, "", fmt.Errorf("stagegate: gate spec missing type")
	}
	gate, ok := r.Lookup(spec.Type)
	if !ok {
		return false, "", fmt.Errorf("stagegate: unknown gate type %q", spec.Type)
	}

	// Build the inner StageContext. Leaf gates see Config; compound
	// gates see the raw spec (so they can read the Gates array).
	inner := stage
	inner.GateType = spec.Type
	if isCompound(spec.Type) {
		// Compound gates read the original spec to access the children
		// list. We pass through gateSpecJSON so the compound's
		// Evaluate can re-parse and recurse.
		inner.GateConfig = gateSpecJSON
	} else {
		// Leaf gates only need the config object.
		if len(spec.Config) == 0 {
			inner.GateConfig = json.RawMessage("{}")
		} else {
			inner.GateConfig = spec.Config
		}
	}

	// Compound gates need the registry + depth to recurse. Rather
	// than threading them through StageContext (which would couple
	// every leaf gate to recursion machinery), we type-assert to the
	// compound interface here and pass them explicitly.
	if c, isC := gate.(compoundGate); isC {
		return c.evaluateCompound(ctx, db, inner, r, depth)
	}
	return gate.Evaluate(ctx, db, inner)
}

// compoundGate is the package-private interface implemented by all_of
// and any_of. It threads the registry + depth through so children can
// recurse via EvaluateGateConfig. Leaf gates implement plain Gate;
// compound gates implement both Gate (so the registry treats them
// uniformly) and compoundGate (so EvaluateGateConfig can dispatch with
// recursion machinery).
type compoundGate interface {
	Gate
	evaluateCompound(ctx context.Context, db *sql.DB, stage StageContext, registry *Registry, depth int) (passed bool, reason string, err error)
}

// isCompound returns true for the registered compound gate type
// names. Kept tight on purpose — compound types are a closed set and
// the only place outside the gate impls that needs to know whether a
// type is compound is the dispatcher above.
func isCompound(gateType string) bool {
	return gateType == "all_of" || gateType == "any_of"
}
