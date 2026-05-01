package treatments

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"time"

	"force-orchestrator/internal/holdout"
	"force-orchestrator/internal/store"
)

// SystemConfigApplyMode is the SystemConfig key whose value selects
// log-only vs live behaviour. Operators set this to 'log-only' for an
// emergency rollback (a single SystemConfig write) without a
// re-deploy. Default is 'live' (Phase 2 of D3 onwards).
const SystemConfigApplyMode = "treatments_apply_mode"

// activeApplyMode reads SystemConfig for the live/log-only switch.
// Defaults to ModeLive on a missing or unparseable row — the contract
// is "live unless the operator explicitly disables it." A typo in the
// SystemConfig value (e.g. 'logonly' instead of 'log-only') falls
// through to live; this is intentional, the safer default for an
// experiment-active fleet is to keep recording assignments.
func activeApplyMode(db *sql.DB) string {
	if db == nil {
		return ModeLive
	}
	switch store.GetConfig(db, SystemConfigApplyMode, ModeLive) {
	case ModeLogOnly, "log-only":
		return ModeLogOnly
	default:
		return ModeLive
	}
}

// applyLive runs the live-mode pipeline. Sequence:
//  1. Resolve holdout membership via baseline-2026 (or call.InHoldout
//     if the caller already inherited it from the natural unit's
//     parent).
//  2. If in holdout: short-circuit, no experiment enrollment.
//  3. Otherwise: load every running experiment whose subject_agent +
//     assignment_unit matches, hand the candidate set to the
//     orthogonal-overlap scheduler, and enroll the unit in the
//     maximal non-conflicting subset (paired-runs.md § Orthogonal
//     dimension invariant). Rewrite the CallDescriptor's prompt
//     template / model per each assigned TreatmentSpec.
//
// D3 Phase 4: the Phase 2 "enroll in every match" loop has been
// replaced by SelectOrthogonalEnrollments. Single-experiment
// behaviour is preserved (one candidate → one selection → identical
// effect to the old path); the scheduler only changes outcomes when
// two or more candidates touch overlapping dimensions.
//
// Returns the (possibly rewritten) descriptor and the list of
// assignment records to journal.
func applyLive(ctx context.Context, db *sql.DB, call CallDescriptor) (CallDescriptor, []RunAssignment) {
	now := time.Now().UTC()

	if !call.InHoldout {
		if h, err := holdout.LoadHoldoutByName(ctx, db, holdout.BaselineHoldoutName); err == nil {
			if holdout.IsInHoldoutWithSnapshot(h, call.NaturalUnitKind, call.NaturalUnitID, now) {
				call.InHoldout = true
			}
		}
	}
	if call.InHoldout {
		return call, nil
	}

	candidates, err := loadExperimentDescriptors(ctx, db, call.AgentName, call.NaturalUnitKind)
	if err != nil || len(candidates) == 0 {
		return call, nil
	}
	selected := SelectOrthogonalEnrollments(call.NaturalUnitKind, call.NaturalUnitID, candidates)
	if len(selected) == 0 {
		return call, nil
	}

	var assignments []RunAssignment
	for _, exp := range selected {
		treats, err := loadExperimentTreatments(ctx, db, exp.ID)
		if err != nil || len(treats) == 0 {
			continue
		}
		assigned := pickTreatment(exp.ID, call.NaturalUnitKind, call.NaturalUnitID, treats)
		applyTreatmentToCall(&call, assigned)
		assignments = append(assignments, RunAssignment{
			ExperimentID: exp.ID,
			TreatmentID:  assigned.ID,
			ArmLabel:     assigned.ArmLabel,
		})
		recordExperimentRun(ctx, db, exp.ID, assigned.ID, call, now)
	}
	return call, assignments
}

// treatmentRow joins ExperimentTreatments + TreatmentSpecs into a
// single shape the picker can consume.
type treatmentRow struct {
	ID                 int
	ArmLabel           string
	TargetCellWeight   float64
	PromptTemplateRef  string
	ModelIdentifier    string
}

func loadExperimentTreatments(ctx context.Context, db *sql.DB, experimentID int) ([]treatmentRow, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT t.id, t.arm_label, IFNULL(t.target_cell_weight, 0),
		       IFNULL(s.prompt_template_ref, ''),
		       IFNULL(s.model_identifier, '')
		FROM ExperimentTreatments t
		LEFT JOIN TreatmentSpecs s ON s.id = t.treatment_spec_id
		WHERE t.experiment_id = ?
		ORDER BY t.id
	`, experimentID)
	if err != nil {
		return nil, fmt.Errorf("loadExperimentTreatments query: %w", err)
	}
	defer rows.Close()
	var out []treatmentRow
	for rows.Next() {
		var tr treatmentRow
		if err := rows.Scan(&tr.ID, &tr.ArmLabel, &tr.TargetCellWeight, &tr.PromptTemplateRef, &tr.ModelIdentifier); err != nil {
			return nil, fmt.Errorf("loadExperimentTreatments scan: %w", err)
		}
		out = append(out, tr)
	}
	return out, rows.Err()
}

// pickTreatment maps (experiment, kind, id) onto exactly one arm via
// stable hash + cumulative-weight buckets. Identical inputs always
// produce the same arm — the determinism guarantee from
// paired-runs.md § Sticky task retries.
func pickTreatment(experimentID int, kind string, unitID int, treats []treatmentRow) treatmentRow {
	if len(treats) == 1 {
		return treats[0]
	}
	totalWeight := 0.0
	for _, t := range treats {
		if t.TargetCellWeight > 0 {
			totalWeight += t.TargetCellWeight
		}
	}
	if totalWeight <= 0 {
		// Equal-weight fallback when no target_cell_weight is set.
		idx := int(hashUint64(experimentID, kind, unitID) % uint64(len(treats)))
		return treats[idx]
	}
	v := hashFraction(experimentID, kind, unitID) * totalWeight
	cum := 0.0
	for _, t := range treats {
		w := t.TargetCellWeight
		if w <= 0 {
			continue
		}
		cum += w
		if v < cum {
			return t
		}
	}
	return treats[len(treats)-1]
}

func applyTreatmentToCall(call *CallDescriptor, t treatmentRow) {
	if t.PromptTemplateRef != "" {
		call.PromptTemplate = t.PromptTemplateRef
	}
	if t.ModelIdentifier != "" {
		call.Model = t.ModelIdentifier
	}
}

func recordExperimentRun(ctx context.Context, db *sql.DB, experimentID, treatmentID int, call CallDescriptor, now time.Time) {
	// Idempotent on (experiment_id, treatment_id, natural_unit_*) —
	// a unit re-hitting the same experiment via a Medic re-queue
	// keeps its original assignment. No INSERT OR IGNORE — the index
	// would be the cleaner shape, but ExperimentRuns has no UNIQUE
	// in the schema today; we issue an INSERT only when no prior
	// row exists.
	var existing int
	err := db.QueryRowContext(ctx, `
		SELECT id FROM ExperimentRuns
		WHERE experiment_id = ?
		  AND treatment_id = ?
		  AND natural_unit_kind = ?
		  AND natural_unit_id = ?
		LIMIT 1
	`, experimentID, treatmentID, call.NaturalUnitKind, call.NaturalUnitID).Scan(&existing)
	if err == nil {
		return
	}
	if err != sql.ErrNoRows {
		return
	}
	db.ExecContext(ctx, `
		INSERT INTO ExperimentRuns
			(experiment_id, treatment_id, natural_unit_kind, natural_unit_id,
			 mode, agent_name, assigned_at)
		VALUES (?, ?, ?, ?, 'paired_real', ?, ?)
	`, experimentID, treatmentID, call.NaturalUnitKind, call.NaturalUnitID,
		call.AgentName, now.Format("2006-01-02 15:04:05"))
}

// writeLogRow journals one TreatmentApplyLog row capturing the
// post-modification call descriptor and the assignments produced.
// Fail-open: a journal write failure must not break the agent's
// hot-path call (the live-mode caller's pattern is "log error and
// proceed").
func writeLogRow(ctx context.Context, db *sql.DB, call CallDescriptor, assignments []RunAssignment, mode string) error {
	if assignments == nil {
		assignments = []RunAssignment{}
	}
	body, err := json.Marshal(assignments)
	if err != nil {
		return fmt.Errorf("marshal assignments: %w", err)
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
		string(body),
		mode,
	)
	if err != nil {
		return fmt.Errorf("write TreatmentApplyLog: %w", err)
	}
	return nil
}

// hashFraction maps (experiment_id, kind, unit_id) onto a stable
// float in [0, 1). Symmetrically equivalent to internal/holdout's
// hashFraction but with the experiment id as the salt — ensures the
// same unit lands in different buckets across different experiments.
func hashFraction(experimentID int, kind string, unitID int) float64 {
	v := hashUint64(experimentID, kind, unitID)
	return float64(v) / 18446744073709551616.0
}

func hashUint64(experimentID int, kind string, unitID int) uint64 {
	key := fmt.Sprintf("%d:%s:%d", experimentID, kind, unitID)
	sum := sha256.Sum256([]byte(key))
	return binary.BigEndian.Uint64(sum[:8])
}
