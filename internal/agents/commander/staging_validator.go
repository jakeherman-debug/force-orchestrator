// Package commander hosts Commander-side helpers that don't fit cleanly in
// the parent `agents` package. Today the only resident is the staged-convoy
// JSON-output validator (D5.5 P2): it parses the optional staging-mode JSON
// shape the Commander emits when planning a multi-stage convoy, and rejects
// malformed shapes before any DB writes happen.
//
// The validator is pure — it takes a parsed StagingPlan + a stagegate
// Registry, and returns nil on success or a descriptive error on the first
// violation. Callers in runCommanderTask invoke it after extracting the JSON
// from the LLM's response. On validation failure the convoy creation is
// rejected with the error surfaced to the operator.
//
// Why a sibling package and not the parent agents package? Three reasons:
//
//  1. The validator is a small, testable surface; isolating it lets it be
//     exercised by short table-driven tests without dragging the rest of
//     the agents package's transitive deps (telemetry, claude CLI, etc.).
//  2. Pattern P-StagingPromotionConfirm (D5.5 anti-cheat #2) and the
//     staging_strategy gate live here — the parent agents package can
//     import these without creating a circular dep through internal/store.
//  3. The forward-compat hooks for merge_parallel/stacked are easier to
//     evolve when the recognised-but-rejected list is in one place.
package commander

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"force-orchestrator/internal/stagegate"
	"force-orchestrator/internal/store"
)

// StagingPlan is the structured shape of the Commander's optional
// staged-mode JSON output. It mirrors the schema described in
// docs/roadmap.md § "Deliverable 5.5 — Staged Convoys" → "Commander
// integration." Single-mode planning continues to emit a bare
// []TaskPlan; staged-mode wraps the task list with stage metadata.
//
// The JSON shape is:
//
//	{
//	  "staging_mode": "staged",
//	  "staging_strategy": "strict",
//	  "stages": [
//	    {
//	      "stage_num": 1,
//	      "intent": "Add nullable user_account_status column + migration",
//	      "tasks": [{"id":1,"repo":"api","task":"...","blocked_by":[]}],
//	      "gate": {"type": "soak_minutes", "config": {"minutes": 60}}
//	    },
//	    ...
//	  ]
//	}
//
// `staging_mode == "single"` is also accepted by the validator — the
// Commander may emit it explicitly to make the choice durable in the
// proposal record. In that case `stages` is ignored and the legacy
// task-array path takes over.
type StagingPlan struct {
	StagingMode     string             `json:"staging_mode"`
	StagingStrategy string             `json:"staging_strategy"`
	Stages          []StagingPlanStage `json:"stages"`
}

// StagingPlanStage is one stage in a StagingPlan. Tasks within a stage
// follow the same []TaskPlan shape used by single-mode plans, so the
// downstream insertConvoyAndTasks path can consume them unchanged after
// the validator has stamped stage_num through to BountyBoard.stage_id.
type StagingPlanStage struct {
	StageNum int                `json:"stage_num"`
	Intent   string             `json:"intent"`
	Tasks    []store.TaskPlan   `json:"tasks"`
	Gate     *StagingPlanGate   `json:"gate"` // nil → no gate (terminal stage only)
}

// StagingPlanGate is the gate spec for one stage. Type is the gate's
// registered type string (must match a stagegate.Registry entry). Config
// is forwarded verbatim to the gate's config-parser; for compound gates
// (all_of, any_of) it carries a `gates` array of nested specs.
//
// For round-trip JSON purposes Config is parsed as a generic map so the
// validator can reach into compound payloads (children list) without
// re-parsing a json.RawMessage. The same map is re-marshalled by
// MarshalGateJSON when the validator returns a sanitised gate spec to
// the caller.
type StagingPlanGate struct {
	Type   string                 `json:"type"`
	Config map[string]interface{} `json:"config,omitempty"`
	// Gates is populated for compound gates ("all_of", "any_of"). Each
	// element is itself a *StagingPlanGate; we model nested compounds
	// recursively so the depth-cap walk doesn't need to re-marshal at
	// each level.
	Gates []*StagingPlanGate `json:"gates,omitempty"`
}

// ParseStagingPlan attempts to interpret a raw JSON byte slice as a
// staged-mode plan. Returns the parsed plan with `staged=true` if the
// JSON is an object whose `staging_mode` field is "staged"; returns
// `staged=false` (and nil plan) if the JSON is anything else (a bare
// task array, an explicit `staging_mode: "single"`, etc.). Returns an
// error only on JSON syntax errors that would prevent fall-back parsing.
//
// The two-phase parse — peek at staging_mode, then full-parse — keeps
// the legacy single-stage path completely undisturbed: an LLM that
// emits a bare array continues to flow through json.Unmarshal into
// []TaskPlan as before.
func ParseStagingPlan(raw []byte) (plan *StagingPlan, staged bool, err error) {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" {
		return nil, false, nil
	}
	// Bare arrays cannot be staged plans by definition — short-circuit
	// so an LLM that emits the legacy shape never trips a peek-error.
	if strings.HasPrefix(trimmed, "[") {
		return nil, false, nil
	}
	var peek struct {
		StagingMode string `json:"staging_mode"`
	}
	if pErr := json.Unmarshal([]byte(trimmed), &peek); pErr != nil {
		// Not a staged-mode object — caller will try the legacy
		// []TaskPlan path. Don't surface this as an error; the
		// legacy path will produce its own clear failure if the
		// JSON is also not a valid task array.
		return nil, false, nil
	}
	if peek.StagingMode != string(store.StagingModeStaged) {
		return nil, false, nil
	}
	// We've committed to staged-mode shape — surface real parse errors.
	var p StagingPlan
	if uErr := json.Unmarshal([]byte(trimmed), &p); uErr != nil {
		return nil, true, fmt.Errorf("parse staging plan: %w", uErr)
	}
	return &p, true, nil
}

// ValidateStagingPlan parses the Commander's emitted staged-mode plan
// and validates structural invariants:
//
//   - staging_mode is "single" or "staged"
//   - staging_strategy is "strict" (only supported value in D5.5;
//     "merge_parallel" and "stacked" are forward-compat-recognised but
//     rejected with explicit "not yet supported" errors)
//   - if staged: stages list non-empty, stage_num 1-indexed contiguous,
//     intent non-empty per stage
//   - tasks list non-empty per stage
//   - gate spec on each stage is well-formed:
//   - leaf gates: type is registered in the passed-in registry
//   - compound gates: children non-empty, depth ≤ MaxNestingDepth
//   - non-terminal stages may not carry gate=null (per spec: null gate
//     is allowed only on the terminal stage)
//
// Returns nil on success. Returns the FIRST violation encountered as
// an error, with a message that names the field/stage and explains
// what was wrong, so the operator-facing FailBounty surface can print
// it without reformatting.
//
// gateRegistry is the registry the runtime will dispatch through; the
// validator uses it for type-name lookup so callers can pre-register
// custom gates (P3 advanced leaves, future test stubs) without the
// validator hard-coding the type list.
func ValidateStagingPlan(plan StagingPlan, gateRegistry *stagegate.Registry) error {
	if gateRegistry == nil {
		return fmt.Errorf("ValidateStagingPlan: gateRegistry must not be nil")
	}

	// Mode + strategy.
	switch plan.StagingMode {
	case string(store.StagingModeSingle):
		// Single mode: stages are ignored, no further validation.
		// staging_strategy is permitted but not required; if set,
		// it must still be in the recognised set.
		if plan.StagingStrategy != "" {
			if err := validateStagingStrategy(plan.StagingStrategy); err != nil {
				return err
			}
		}
		return nil
	case string(store.StagingModeStaged):
		// fall through to staged-mode validation
	case "":
		return fmt.Errorf("staging_mode required (got empty); use \"single\" or \"staged\"")
	default:
		return fmt.Errorf("unknown staging_mode %q (allowed: %q, %q)", plan.StagingMode,
			store.StagingModeSingle, store.StagingModeStaged)
	}

	if err := validateStagingStrategy(plan.StagingStrategy); err != nil {
		return err
	}

	// Staged-mode body.
	if len(plan.Stages) == 0 {
		return fmt.Errorf("staged-mode plan has empty stages list (must declare at least one stage)")
	}
	for i, s := range plan.Stages {
		if s.StageNum != i+1 {
			return fmt.Errorf("stages must be 1-indexed contiguous: stage at index %d has stage_num=%d (expected %d)", i, s.StageNum, i+1)
		}
		if strings.TrimSpace(s.Intent) == "" {
			return fmt.Errorf("stage %d: intent must be non-empty (the Commander reasons about why each stage is independently safe)", s.StageNum)
		}
		if len(s.Tasks) == 0 {
			return fmt.Errorf("stage %d: tasks list is empty (every stage must carry at least one CodeEdit task)", s.StageNum)
		}

		isTerminal := i == len(plan.Stages)-1
		if s.Gate == nil {
			// null gate allowed only on terminal stage.
			if !isTerminal {
				return fmt.Errorf("stage %d: gate=null is allowed only on the terminal stage; non-terminal stages must declare a gate (anti-cheat: no null-gate on non-terminal)", s.StageNum)
			}
			continue
		}
		if err := validateGateSpec(s.Gate, gateRegistry, 0); err != nil {
			return fmt.Errorf("stage %d: gate spec invalid: %w", s.StageNum, err)
		}
	}
	return nil
}

// ValidateReleaseLabelGateForRepos enforces the D5.5 P3 γ planner-time
// per-repo pattern check for stages that use release_label_present
// (including nested under all_of / any_of compounds). For each stage
// whose gate tree contains a release_label_present leaf, every repo
// touched by that stage's tasks must have a non-empty
// Repositories.release_label_pattern; if any repo lacks the pattern
// the function returns an explicit "configure pattern or pick a
// different gate" error so the operator gets actionable text instead
// of a runtime gate-misconfigured surprise.
//
// Why a separate function (rather than rolling into ValidateStagingPlan):
//
//   - The pattern check needs DB access — passing a *sql.DB into
//     ValidateStagingPlan would force every existing test that calls
//     it to seed a real DB, polluting the structural-only validation
//     with a runtime dependency.
//   - The check runs only once per convoy at planning time. Bundling
//     it into ValidateStagingPlan would burn DB queries on every
//     plan even when the gate type doesn't need them.
//   - Callers compose the two: structural validation first (cheap,
//     fail-fast), then DB-backed pattern check. Failing the structural
//     pass means the plan never reaches the DB step.
//
// Returns nil when the plan uses no release_label_present gates OR
// when every release_label_present-touched repo has a non-empty
// pattern. Returns an error naming the offending stage + repo on the
// first violation (deterministic ordering — we sort the offending
// repo set so the error message is stable across runs for the same
// plan + DB).
func ValidateReleaseLabelGateForRepos(plan StagingPlan, db *sql.DB) error {
	if db == nil {
		return fmt.Errorf("ValidateReleaseLabelGateForRepos: db must not be nil")
	}
	if plan.StagingMode != string(store.StagingModeStaged) {
		// Single-mode plans don't carry stage-level gates; nothing to
		// check. (Single-mode never uses release_label_present because
		// the whole plan is one stage with no gate.)
		return nil
	}
	for _, s := range plan.Stages {
		if s.Gate == nil {
			continue
		}
		if !gateTreeUsesReleaseLabel(s.Gate) {
			continue
		}
		// Collect the unique repo set this stage touches via its tasks.
		// A multi-task stage may hit several repos; each must carry
		// the pattern.
		repoSet := map[string]struct{}{}
		for _, task := range s.Tasks {
			if task.Repo == "" {
				continue
			}
			repoSet[task.Repo] = struct{}{}
		}
		repos := make([]string, 0, len(repoSet))
		for r := range repoSet {
			repos = append(repos, r)
		}
		sort.Strings(repos)

		var missing []string
		for _, repo := range repos {
			pattern, err := store.GetRepositoryReleaseLabelPattern(db, repo)
			if err != nil {
				return fmt.Errorf("stage %d: release_label_present gate: lookup release_label_pattern for repo %q: %w", s.StageNum, repo, err)
			}
			if pattern == "" {
				missing = append(missing, repo)
			}
		}
		if len(missing) > 0 {
			// Operator-actionable message: name the offending stage,
			// list the bare repos, hint at the two ways to resolve.
			return fmt.Errorf("stage %d uses release_label_present gate but repo %s has no release_label_pattern configured. Either set the pattern via 'force config set repository.<name>.release_label_pattern <regex>' or pick a different gate.",
				s.StageNum, strings.Join(missing, ", "))
		}
	}
	return nil
}

// gateTreeUsesReleaseLabel returns true when the gate spec (or any of
// its descendants, via all_of/any_of compounds) is a
// release_label_present leaf. Walks at most stagegate.MaxNestingDepth
// levels — the structural validator has already verified the tree is
// within the cap, so we don't need a fresh depth guard here.
func gateTreeUsesReleaseLabel(g *StagingPlanGate) bool {
	if g == nil {
		return false
	}
	if g.Type == "release_label_present" {
		return true
	}
	if !isCompoundType(g.Type) {
		return false
	}
	children := g.Gates
	if len(children) == 0 {
		children = childrenFromConfig(g.Config)
	}
	for _, child := range children {
		if gateTreeUsesReleaseLabel(child) {
			return true
		}
	}
	return false
}

// validateStagingStrategy enforces the D5.5 "only strict is supported"
// rule. Returns explicit "not yet supported" errors for merge_parallel
// and stacked so the operator-facing surface explains the deferral
// instead of dropping a generic "unknown strategy" message.
func validateStagingStrategy(strategy string) error {
	switch strategy {
	case "":
		return fmt.Errorf("staging_strategy required (got empty); use \"strict\" — \"merge_parallel\" and \"stacked\" are forward-compat-recognised but not yet implemented in D5.5")
	case store.StagingStrategyStrict:
		return nil
	case store.StagingStrategyMergeParallel:
		return fmt.Errorf("staging_strategy=merge_parallel is forward-compat-recognized but not yet implemented in D5.5; only \"strict\" is supported. Wait for D6+ to enable parallel-stage execution.")
	case store.StagingStrategyStacked:
		return fmt.Errorf("staging_strategy=stacked is forward-compat-recognized but not yet implemented in D5.5; only \"strict\" is supported. Wait for D6+ to enable parallel-stage execution.")
	default:
		return fmt.Errorf("unknown staging_strategy %q (recognised: %q, %q, %q)",
			strategy, store.StagingStrategyStrict, store.StagingStrategyMergeParallel, store.StagingStrategyStacked)
	}
}

// validateGateSpec recursively validates a gate spec. depth is the
// current recursion depth; the cap matches stagegate.MaxNestingDepth so
// the validator and the runtime agree on what "too deep" means.
//
// Compound gates (all_of, any_of) require non-empty children. Leaf
// gates require their type be registered in gateRegistry. Unknown
// types are rejected — silently accepting unregistered types would
// produce a stage that the runtime can never advance.
func validateGateSpec(g *StagingPlanGate, gateRegistry *stagegate.Registry, depth int) error {
	if g == nil {
		return fmt.Errorf("gate spec is nil")
	}
	if depth >= stagegate.MaxNestingDepth {
		return fmt.Errorf("gate nesting depth %d exceeds cap %d (compound trees deeper than %d are almost certainly a planning bug)", depth, stagegate.MaxNestingDepth, stagegate.MaxNestingDepth)
	}
	if strings.TrimSpace(g.Type) == "" {
		return fmt.Errorf("gate spec missing type")
	}
	if _, ok := gateRegistry.Lookup(g.Type); !ok {
		return fmt.Errorf("unknown gate type %q (not registered in stagegate.Registry)", g.Type)
	}
	if isCompoundType(g.Type) {
		// Compound gates carry children in `gates`. Some legacy specs
		// nested children under `config.gates`; we accept either shape
		// for forward-compat with the docs example.
		children := g.Gates
		if len(children) == 0 {
			children = childrenFromConfig(g.Config)
		}
		if len(children) == 0 {
			return fmt.Errorf("compound gate %q has empty children list (compound with zero children can never resolve)", g.Type)
		}
		for i, child := range children {
			if err := validateGateSpec(child, gateRegistry, depth+1); err != nil {
				return fmt.Errorf("compound gate %q child %d: %w", g.Type, i, err)
			}
		}
	}
	return nil
}

// isCompoundType returns true for gate types that recurse on children.
// Kept aligned with stagegate.isCompound (which is package-private).
func isCompoundType(gateType string) bool {
	return gateType == "all_of" || gateType == "any_of"
}

// childrenFromConfig extracts a `gates` array nested inside a config
// object. Some legacy planner outputs put compound children under
// `config.gates` rather than the top-level `gates`; we accept both
// shapes so the validator's error messages are about real bugs, not
// presentation-layer drift.
func childrenFromConfig(cfg map[string]interface{}) []*StagingPlanGate {
	if cfg == nil {
		return nil
	}
	rawGates, ok := cfg["gates"]
	if !ok {
		return nil
	}
	arr, ok := rawGates.([]interface{})
	if !ok {
		return nil
	}
	out := make([]*StagingPlanGate, 0, len(arr))
	for _, item := range arr {
		// Round-trip through JSON to populate the typed shape; cheaper
		// than hand-walking interface{} maps and keeps the validator
		// agnostic to the original parser's choice of map type.
		buf, err := json.Marshal(item)
		if err != nil {
			return nil
		}
		var g StagingPlanGate
		if uErr := json.Unmarshal(buf, &g); uErr != nil {
			return nil
		}
		out = append(out, &g)
	}
	return out
}

// ToStageSpecs converts a validated StagingPlan into the slice of
// store.StagedStageSpec values that store.CreateStagedConvoy expects.
// Caller must pass a plan that ValidateStagingPlan accepted; this
// conversion does no further validation.
//
// The gate spec is re-marshalled to JSON and stored verbatim in
// ConvoyStages.gate_config_json so the runtime gate dispatcher (which
// works against json.RawMessage) can consume it without re-parsing
// through the typed StagingPlanGate shape.
func (p StagingPlan) ToStageSpecs() ([]store.StagedStageSpec, error) {
	out := make([]store.StagedStageSpec, 0, len(p.Stages))
	for _, s := range p.Stages {
		spec := store.StagedStageSpec{
			StageNum: s.StageNum,
			Intent:   s.Intent,
		}
		if s.Gate != nil {
			spec.GateType = s.Gate.Type
			// Persist the full gate spec object as gate_config_json so
			// the runtime can re-dispatch through Registry.EvaluateGateConfig
			// (which expects {"type": ..., "config": ..., "gates": ...}).
			gateJSON, err := json.Marshal(s.Gate)
			if err != nil {
				return nil, fmt.Errorf("ToStageSpecs: marshal stage %d gate: %w", s.StageNum, err)
			}
			spec.GateConfigJSON = string(gateJSON)
		}
		out = append(out, spec)
	}
	return out, nil
}
