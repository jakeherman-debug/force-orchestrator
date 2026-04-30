package engineering_corps

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"strings"

	"force-orchestrator/internal/agents/capabilities"
	"force-orchestrator/internal/store"
)

// PromotionAuthor — assemble a ratifiable PromotionProposals row from
// a terminated, declared-winner experiment.
//
// Per docs/paired-runs.md § Hand-off to promotion, the proposal must
// carry the full evidence trail:
//   - experiment_id + winner_treatment_id
//   - cell_means at termination
//   - posterior probability at termination
//   - confirm-phase outcome (Phase 5 surface — Phase 3 fills in
//     `confirm_phase_outcome=''` and the operator interprets that as
//     "no confirm phase ran for this stakes tier")
//   - fleet_state_hash at start / end (sourced from the
//     ExperimentOutcomes columns when present)
//   - analysis framework version + metric version
//
// Operator-routing invariant: the row is written with ratified_at='',
// rejected_at='', and a 14-day TTL. Operator ratifies on the
// dashboard; the row never auto-promotes.
//
// Pure SQL — no LLM call. The hypothesis text + winner cell content
// are already deterministically captured at experiment-author time;
// PromotionAuthor's job is to copy + format the evidence, not to
// invent new content.
//
// Inputs (BountyBoard.payload JSON):
//   { "experiment_id": <int> }
//
// Idempotence: if a non-rejected proposal already exists for this
// experiment, the handler logs and completes without writing a
// duplicate. Re-running PromotionAuthor on the same experiment does
// not produce a second open proposal.
type promotionAuthorPayload struct {
	ExperimentID int `json:"experiment_id"`
}

func handlePromotionAuthor(
	ctx context.Context,
	cfg EngineeringCorpsConfig,
	_ *capabilities.Profile,
	agentName string,
	bounty *store.Bounty,
	logger *log.Logger,
) error {
	db := cfg.DB

	var payload promotionAuthorPayload
	if err := strictDecode(bounty.Payload, &payload); err != nil {
		return fmt.Errorf("PromotionAuthor: parse payload: %w", err)
	}
	if payload.ExperimentID <= 0 {
		return fmt.Errorf("PromotionAuthor: payload missing experiment_id")
	}

	// Read terminated experiment + outcome.
	exp, err := loadTerminatedExperimentForPromotion(db, payload.ExperimentID)
	if err != nil {
		return fmt.Errorf("PromotionAuthor: load experiment %d: %w", payload.ExperimentID, err)
	}
	if exp.OutcomeReason != "declared_winner" || exp.WinnerTreatmentID == 0 {
		// Not a winner — clean completion (the monitor shouldn't
		// have queued us, but we're idempotent).
		logger.Printf("[%s] PromotionAuthor #%d: experiment %d is %q, not declared_winner — skipping (no proposal authored)",
			agentName, bounty.ID, payload.ExperimentID, exp.OutcomeReason)
		return store.UpdateBountyStatus(db, bounty.ID, "Completed")
	}

	// Idempotence: don't double-author for the same experiment.
	existing, err := existingOpenProposalID(db, payload.ExperimentID)
	if err != nil {
		return fmt.Errorf("PromotionAuthor: idempotence check: %w", err)
	}
	if existing > 0 {
		logger.Printf("[%s] PromotionAuthor #%d: experiment %d already has open proposal #%d — no duplicate authored",
			agentName, bounty.ID, payload.ExperimentID, existing)
		return store.UpdateBountyStatus(db, bounty.ID, "Completed")
	}

	// Build the evidence summary. Keep this deterministic so the same
	// experiment + outcome always produces the same JSON; that lets
	// reviewers diff proposals across re-authors.
	evidence := map[string]any{
		"experiment_id":             payload.ExperimentID,
		"experiment_name":           exp.Name,
		"hypothesis_text":           exp.Hypothesis,
		"stakes_tier":               exp.StakesTier,
		"subject_agent":             exp.SubjectAgent,
		"winner_treatment_id":       exp.WinnerTreatmentID,
		"winner_arm_label":          exp.WinnerArmLabel,
		"winner_posterior":          exp.WinnerPosterior,
		"winner_effect_estimate":    exp.WinnerEffectEstimate,
		"cell_means":                exp.CellMeans, // map[string]float64
		"analysis_framework":        exp.AnalysisFramework,
		"fleet_state_hash_at_start": exp.FleetStateHashAtStart,
		"fleet_state_hash_at_end":   exp.FleetStateHashAtEnd,
		"confirm_phase_outcome":     exp.ConfirmPhaseOutcome,
	}
	evidenceJSON, err := json.Marshal(evidence)
	if err != nil {
		return fmt.Errorf("PromotionAuthor: marshal evidence: %w", err)
	}

	// Insert the proposal. Authored-by='engineering-corps' is the
	// canonical authoring stamp (paired-runs.md). ratified_at='' and
	// rejected_at='' so the operator gate is preserved. ttl_expires_at
	// = +14 days (paired-runs.md § Hand-off to promotion).
	var proposalID int
	err = db.QueryRowContext(ctx, `
		INSERT INTO PromotionProposals
			(experiment_id, kind, rule_key, proposed_content, evidence_summary_json,
			 authored_by, authored_at, ttl_expires_at)
		VALUES (?, 'promote', '', '', ?, 'engineering-corps', datetime('now'), datetime('now', '+14 days'))
		RETURNING id
	`, payload.ExperimentID, string(evidenceJSON)).Scan(&proposalID)
	if err != nil {
		return fmt.Errorf("PromotionAuthor: insert proposal: %w", err)
	}

	logger.Printf("[%s] PromotionAuthor #%d: authored PromotionProposals #%d for experiment %d (winner=%s posterior=%.4f) — awaiting operator ratification",
		agentName, bounty.ID, proposalID, payload.ExperimentID, exp.WinnerArmLabel, exp.WinnerPosterior)

	if err := store.UpdateBountyStatus(db, bounty.ID, "Completed"); err != nil {
		return fmt.Errorf("PromotionAuthor: complete bounty: %w", err)
	}
	return nil
}

// strictDecode is the small JSON-unmarshal helper this package's
// non-LLM handlers use. It calls DisallowUnknownFields and rejects
// trailing tokens (Fix #8.5 strict-decode invariant).
func strictDecode(payload string, out any) error {
	if strings.TrimSpace(payload) == "" || strings.TrimSpace(payload) == "{}" {
		return nil
	}
	dec := json.NewDecoder(strings.NewReader(payload))
	dec.DisallowUnknownFields()
	if err := dec.Decode(out); err != nil {
		return err
	}
	if dec.More() {
		return fmt.Errorf("trailing tokens after first value")
	}
	return nil
}

// promotionInputRow is the projection PromotionAuthor reads.
type promotionInputRow struct {
	ID                    int
	Name                  string
	Hypothesis            string
	StakesTier            string
	SubjectAgent          string
	AnalysisFramework     string
	OutcomeReason         string
	WinnerTreatmentID     int
	WinnerArmLabel        string
	WinnerPosterior       float64
	WinnerEffectEstimate  float64
	CellMeans             map[string]float64
	FleetStateHashAtStart string
	FleetStateHashAtEnd   string
	ConfirmPhaseOutcome   string
}

func loadTerminatedExperimentForPromotion(db *sql.DB, experimentID int) (*promotionInputRow, error) {
	var row promotionInputRow
	row.ID = experimentID
	var status string
	err := db.QueryRow(`
		SELECT status, name, IFNULL(hypothesis_text, ''), IFNULL(stakes_tier, ''),
		       IFNULL(subject_agent, ''), IFNULL(analysis_framework_version, '')
		FROM Experiments WHERE id = ?
	`, experimentID).Scan(&status, &row.Name, &row.Hypothesis, &row.StakesTier, &row.SubjectAgent, &row.AnalysisFramework)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("experiment %d not found", experimentID)
	}
	if err != nil {
		return nil, err
	}
	if status != "terminated" {
		return nil, fmt.Errorf("experiment %d status=%q (must be terminated)", experimentID, status)
	}

	var cellMeansJSON string
	err = db.QueryRow(`
		SELECT termination_reason, IFNULL(winner_treatment_id, 0),
		       IFNULL(winner_posterior, 0), IFNULL(winner_effect_estimate, 0),
		       IFNULL(cell_means_json, '{}'),
		       IFNULL(fleet_state_hash_at_start, ''), IFNULL(fleet_state_hash_at_end, ''),
		       IFNULL(confirm_phase_outcome, '')
		FROM ExperimentOutcomes WHERE experiment_id = ?
	`, experimentID).Scan(
		&row.OutcomeReason, &row.WinnerTreatmentID,
		&row.WinnerPosterior, &row.WinnerEffectEstimate,
		&cellMeansJSON, &row.FleetStateHashAtStart, &row.FleetStateHashAtEnd, &row.ConfirmPhaseOutcome,
	)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("experiment %d has no outcome row", experimentID)
	}
	if err != nil {
		return nil, err
	}
	row.CellMeans = map[string]float64{}
	_ = json.Unmarshal([]byte(cellMeansJSON), &row.CellMeans)

	if row.WinnerTreatmentID > 0 {
		_ = db.QueryRow(`SELECT IFNULL(arm_label,'') FROM ExperimentTreatments WHERE id=?`,
			row.WinnerTreatmentID).Scan(&row.WinnerArmLabel)
	}
	return &row, nil
}

// existingOpenProposalID returns the id of any PromotionProposals
// row for this experiment that has neither been ratified nor rejected
// (i.e. operator has not yet acted). Returns 0 if none.
func existingOpenProposalID(db *sql.DB, experimentID int) (int, error) {
	var id int
	err := db.QueryRow(`
		SELECT id FROM PromotionProposals
		WHERE experiment_id = ?
		  AND IFNULL(ratified_at, '') = ''
		  AND IFNULL(rejected_at, '') = ''
		ORDER BY id LIMIT 1
	`, experimentID).Scan(&id)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	return id, nil
}
