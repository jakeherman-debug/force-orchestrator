// Package treatments implements the experiment-treatment ingress for
// the paired-runs system.
//
// Apply is the single hot-path entry: every Claude CLI / git op call
// routes through it before invoking the underlying tool. The live
// vs log-only behaviour is selected at runtime by SystemConfig key
// `treatments_apply_mode` — default 'live' (Phase 2 onwards),
// 'log-only' is the emergency-rollback escape hatch.
//
// Live behaviour (Phase 2+):
//
//   1. Resolve holdout membership against baseline-2026.
//      Members short-circuit: their CallDescriptor is returned
//      unchanged with the InHoldout flag set, no experiment
//      enrollment.
//   2. For non-holdout units, query active experiments matching
//      (subject_agent, assignment_unit) and deterministically assign
//      one arm per experiment. The arm's TreatmentSpec rewrites the
//      descriptor (prompt template, model).
//   3. Journal a TreatmentApplyLog row capturing the FINAL post-
//      modification descriptor and the list of assignments, tagged
//      mode='live'.
//
// Log-only behaviour (rollback):
//
//   The descriptor is returned unchanged and a journal row is
//   written with mode='log_only'. No holdout / experiment lookup,
//   no ExperimentRuns mutation.
package treatments

import (
	"context"
	"database/sql"
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
// call kind; live mode composes their assignments in id order.
// Log-only mode emits an empty []RunAssignment for every call.
type RunAssignment struct {
	ExperimentID int
	TreatmentID  int
	ArmLabel     string
	Notes        string
}

// Mode discriminates log-only (emergency rollback) from live.
const (
	ModeLogOnly = "log_only"
	ModeLive    = "live"
)

// Apply is the ingress for the entire treatment system. Every LLM /
// subprocess call in the fleet routes through it.
//
// Behaviour is selected at runtime by SystemConfig
// `treatments_apply_mode`:
//   - 'live' (default Phase 2+): holdout check + experiment
//     enrollment + descriptor rewrite + journal.
//   - 'log-only' (operator rollback): pass-through + journal only.
//
// Returns:
//   - the (possibly rewritten) CallDescriptor the caller should
//     actually invoke.
//   - the list of RunAssignment records produced (empty in log-only
//     mode and in live mode when no experiment slotted the unit).
//   - nil error in the steady state. A journal write failure
//     surfaces as the returned error; the caller's pattern is
//     `_, _, _ = treatments.Apply(...)` for fail-open behaviour.
func Apply(ctx context.Context, db *sql.DB, call CallDescriptor) (CallDescriptor, []RunAssignment, error) {
	if db == nil {
		return call, nil, nil
	}

	mode := activeApplyMode(db)

	var assignments []RunAssignment
	if mode == ModeLive {
		call, assignments = applyLive(ctx, db, call)
	}

	if err := writeLogRow(ctx, db, call, assignments, mode); err != nil {
		return call, assignments, err
	}
	return call, assignments, nil
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
