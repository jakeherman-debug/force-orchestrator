package stagegate

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// AllOf is the boolean-AND compound gate: passes when every child
// gate passes. Fails (short-circuits) on the first child failure.
// Stays pending while ANY child is pending and no child has failed.
//
// Children can themselves be compounds, allowing nested logic. The
// MaxNestingDepth cap (5) prevents pathological trees.
//
// Config: {"gates": [<gateSpec>, <gateSpec>, ...]}
//
// Edge cases:
//   - Empty children list → returns an error. The planner rejects
//     this at convoy-creation time; the runtime check is defensive.
//   - Single child → allowed; emits a registry-level warning that
//     the compound is equivalent to its child. Per spec, the
//     planner emits the warning but doesn't reject the spec.
type AllOf struct{}

// Type implements Gate.
func (AllOf) Type() string { return "all_of" }

// Evaluate implements Gate. The dispatcher in EvaluateGateConfig
// routes compound gates through evaluateCompound instead, but Gate
// requires this method on the interface — return an error if a
// caller invokes Evaluate directly without going through the
// registry, since the compound would have no way to recurse.
func (AllOf) Evaluate(_ context.Context, _ *sql.DB, _ StageContext) (bool, string, error) {
	return false, "", errors.New("all_of: must be evaluated via Registry.EvaluateGateConfig (compound gates require registry context)")
}

// evaluateCompound implements compoundGate.
func (AllOf) evaluateCompound(ctx context.Context, db *sql.DB, stage StageContext, registry *Registry, depth int) (bool, string, error) {
	children, err := parseCompoundChildren("all_of", stage.GateConfig)
	if err != nil {
		return false, "", err
	}
	if len(children) == 1 {
		registry.logger.Printf("stagegate: all_of with single child is equivalent to that child (stage_id=%d)", stage.StageID)
	}
	var pendingReasons []string
	for i, child := range children {
		passed, reason, cErr := registry.EvaluateGateConfig(ctx, db, stage, child, depth+1)
		if errors.Is(cErr, ErrPending) {
			pendingReasons = append(pendingReasons, fmt.Sprintf("[%d]: %s", i, reason))
			continue
		}
		if cErr != nil {
			return false, "", fmt.Errorf("all_of child %d: %w", i, cErr)
		}
		if !passed {
			// Short-circuit on first concrete fail.
			return false, fmt.Sprintf("all_of child %d failed: %s", i, reason), nil
		}
	}
	if len(pendingReasons) > 0 {
		return false, fmt.Sprintf("all_of pending: %s", strings.Join(pendingReasons, "; ")), ErrPending
	}
	return true, fmt.Sprintf("all_of: all %d children passed", len(children)), nil
}

// AnyOf is the boolean-OR compound gate: passes as soon as ANY child
// passes (short-circuits). Fails only when ALL children fail. Stays
// pending while at least one child is pending and no child has
// passed.
//
// Config: {"gates": [<gateSpec>, <gateSpec>, ...]}
//
// Edge cases mirror AllOf — empty children rejected, single-child
// warned-about-but-allowed.
type AnyOf struct{}

// Type implements Gate.
func (AnyOf) Type() string { return "any_of" }

// Evaluate implements Gate (see AllOf.Evaluate for why this returns
// an error).
func (AnyOf) Evaluate(_ context.Context, _ *sql.DB, _ StageContext) (bool, string, error) {
	return false, "", errors.New("any_of: must be evaluated via Registry.EvaluateGateConfig (compound gates require registry context)")
}

// evaluateCompound implements compoundGate.
func (AnyOf) evaluateCompound(ctx context.Context, db *sql.DB, stage StageContext, registry *Registry, depth int) (bool, string, error) {
	children, err := parseCompoundChildren("any_of", stage.GateConfig)
	if err != nil {
		return false, "", err
	}
	if len(children) == 1 {
		registry.logger.Printf("stagegate: any_of with single child is equivalent to that child (stage_id=%d)", stage.StageID)
	}
	var failReasons []string
	var pendingReasons []string
	for i, child := range children {
		passed, reason, cErr := registry.EvaluateGateConfig(ctx, db, stage, child, depth+1)
		if errors.Is(cErr, ErrPending) {
			pendingReasons = append(pendingReasons, fmt.Sprintf("[%d]: %s", i, reason))
			continue
		}
		if cErr != nil {
			return false, "", fmt.Errorf("any_of child %d: %w", i, cErr)
		}
		if passed {
			// Short-circuit on first pass.
			return true, fmt.Sprintf("any_of child %d passed: %s", i, reason), nil
		}
		failReasons = append(failReasons, fmt.Sprintf("[%d]: %s", i, reason))
	}
	// No child passed concretely. If any are still pending, stay
	// pending — they may yet pass. Only fail when ALL children have
	// returned a concrete fail.
	if len(pendingReasons) > 0 {
		return false, fmt.Sprintf("any_of pending: %s", strings.Join(pendingReasons, "; ")), ErrPending
	}
	return false, fmt.Sprintf("any_of: all %d children failed: %s", len(children), strings.Join(failReasons, "; ")), nil
}

// parseCompoundChildren extracts the children list from a compound
// gate's spec. The spec shape is:
//
//	{"type": "all_of", "gates": [<gateSpec>, ...]}
//	{"type": "any_of", "gates": [<gateSpec>, ...]}
//
// Returns an error on empty children (the planner rejects empty
// children at convoy-creation time; this is the runtime defense).
func parseCompoundChildren(compoundType string, raw json.RawMessage) ([]json.RawMessage, error) {
	var spec struct {
		Type  string            `json:"type"`
		Gates []json.RawMessage `json:"gates"`
	}
	if err := json.Unmarshal(raw, &spec); err != nil {
		return nil, fmt.Errorf("%s: parse spec: %w", compoundType, err)
	}
	if len(spec.Gates) == 0 {
		return nil, fmt.Errorf("%s: empty children list (planner should have rejected this at planning time)", compoundType)
	}
	return spec.Gates, nil
}
