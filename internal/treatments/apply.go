// Package treatments implements the experiment-treatment ingress for
// the paired-runs system.
//
// In log-only mode (D3 Phase 1) every Claude CLI / git op call routes
// through Apply, which records the call descriptor + assignment intent
// to TreatmentApplyLog WITHOUT mutating the call. The returned
// CallDescriptor is byte-identical to the input. Phase 2 of D3 flips
// this to live pass-through (active experiment treatments rewrite the
// call); Phase 1 ships the wiring + the audit trail so the live flip
// is a config change, not a code change.
package treatments

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
)

// CallDescriptor describes the operation about to be performed —
// "claude -p with this prompt" or "git push origin <branch>". The
// descriptor is opaque enough that an experiment can rewrite it
// (e.g. swap the prompt template or the model). Mirrors the shape
// in internal/clients/experiments/client.go's CallDescriptor; the
// duplicate is intentional — internal/treatments is the runtime
// implementation, internal/clients/experiments is the cross-agent
// service interface (D0 stub layer).
type CallDescriptor struct {
	AgentName       string
	NaturalUnitKind string // 'feature' | 'convoy' | 'task'
	NaturalUnitID   int
	PromptTemplate  string // ref, e.g. 'captain/default@HEAD'
	Model           string
	InHoldout       bool // inherited from the natural unit
}

// RunAssignment records that one experiment slotted one arm into the
// supplied call. Multiple experiments may register against the same
// call kind; D3's mixer ensures their assignments compose. Phase 1's
// log-only mode produces an empty []RunAssignment for every call.
type RunAssignment struct {
	ExperimentID int
	TreatmentID  int
	ArmLabel     string
	Notes        string
}

// Mode discriminates Phase 1 (log-only) from Phase 2+ (live).
const (
	ModeLogOnly = "log_only"
	ModeLive    = "live"
)

// Apply is the ingress for the entire treatment system. Every LLM /
// subprocess call in the fleet routes through it.
//
// Phase 1 contract (log-only):
//   - Returns the input CallDescriptor unchanged (no mutation).
//   - Returns an empty []RunAssignment.
//   - Records one row in TreatmentApplyLog tagged mode='log_only'.
//   - Returns nil error in the steady state. A DB write failure is
//     logged and swallowed (fail-open: an experiment audit failure
//     must not break the agent's actual call).
func Apply(ctx context.Context, db *sql.DB, call CallDescriptor) (CallDescriptor, []RunAssignment, error) {
	assignments := []RunAssignment{}

	if db == nil {
		return call, assignments, nil
	}

	assignmentsJSON, err := json.Marshal(assignments)
	if err != nil {
		// JSON-encoding an empty slice cannot fail; the branch is here
		// for defense-in-depth against future struct additions.
		return call, assignments, fmt.Errorf("marshal assignments: %w", err)
	}

	_, err = db.ExecContext(ctx, `
		INSERT INTO TreatmentApplyLog
			(applied_at, agent_name, natural_unit_kind, natural_unit_id,
			 prompt_template, model, in_holdout, assignments_json, mode)
		VALUES (datetime('now'), ?, ?, ?, ?, ?, ?, ?, ?)
	`,
		call.AgentName,
		call.NaturalUnitKind,
		call.NaturalUnitID,
		call.PromptTemplate,
		call.Model,
		boolToInt(call.InHoldout),
		string(assignmentsJSON),
		ModeLogOnly,
	)
	if err != nil {
		// Fail-open: log to the daemon log via the returned error so
		// the operator sees drift, but do NOT propagate to the agent's
		// hot path. The caller's pattern is `_, _, _ = treatments.Apply(...)`
		// for log-only mode.
		return call, assignments, fmt.Errorf("write TreatmentApplyLog: %w", err)
	}

	return call, assignments, nil
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
