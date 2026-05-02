package stagegate

import (
	"context"
	"database/sql"
)

// NullGate is the no-op gate for the terminal stage of a convoy. It
// always returns passed=true. The "null gate is allowed only on the
// terminal stage" rule is enforced by the planner (P2) at convoy
// creation time — at runtime the gate just resolves immediately.
//
// Naming note: the gate type registers as "null" (matching the
// docs/roadmap.md gate-type table) so the JSON spec reads:
//
//	{"type": "null", "config": {}}
//
// We intentionally do NOT use the literal JSON null for the spec
// because that would collide with "absent gate" semantics — every
// stage needs a gate_type that the registry can dispatch.
type NullGate struct{}

// Type implements Gate.
func (NullGate) Type() string { return "null" }

// Evaluate implements Gate.
func (NullGate) Evaluate(_ context.Context, _ *sql.DB, _ StageContext) (bool, string, error) {
	return true, "no gate (terminal stage)", nil
}
