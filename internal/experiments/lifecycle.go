// Package experiments implements the single-treatment experiment
// lifecycle: author → ratify → enrol units → terminate → outcome →
// promotion proposal. Phase 2 of D3 ships this as a single-treatment
// surface (no factorial); Phase 4 of D3 will extend with factorial
// dimensions and an orthogonal-overlap scheduler.
//
// The lifecycle is operator-routed: AuthorFromYAML produces a row
// in 'authored' state, and Ratify is the gate that flips it to
// 'running'. Both hops are recorded in AuditLog.
package experiments

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"

	"force-orchestrator/internal/analysis"
	"force-orchestrator/internal/store"
)

// Status values used by the lifecycle. The Experiments schema's
// status enum is the union of these plus 'authored' (the schema
// default) — see paired-runs.md § Data Model.
const (
	StatusAuthored   = "authored"
	StatusRunning    = "running"
	StatusConfirming = "confirming"
	StatusTerminated = "terminated"
)

// Manifest is the YAML schema accepted by AuthorFromYAML. Required
// fields are enforced by validateManifest after unmarshal.
type Manifest struct {
	Name                     string `yaml:"name"`
	Hypothesis               string `yaml:"hypothesis"`
	MinPracticalEffect       float64 `yaml:"min_practical_effect"`
	StakesTier               string `yaml:"stakes_tier"`
	SubjectAgent             string `yaml:"subject_agent"`
	AssignmentUnit           string `yaml:"assignment_unit"`
	AnalysisFrameworkVersion string `yaml:"analysis_framework_version"`
	DurationCapHours         int     `yaml:"duration_cap_hours"`
	BudgetUSD                float64 `yaml:"budget_usd"`
	HardCapUSD               float64 `yaml:"hard_cap_usd"`
	Treatments               []ManifestTreatment `yaml:"treatments"`
	Metrics                  []ManifestMetric    `yaml:"metrics"`
	Promote                  *ManifestPromotion  `yaml:"promote"`
}

// ManifestTreatment is one arm of the experiment.
type ManifestTreatment struct {
	ArmLabel          string  `yaml:"arm_label"`
	PromptTemplateRef string  `yaml:"prompt_template_ref"`
	Model             string  `yaml:"model"`
	TargetCellWeight  float64 `yaml:"target_cell_weight"`
}

// ManifestMetric pins one metric the experiment tracks. Exactly one
// must be is_primary=true; that metric drives winner declaration.
type ManifestMetric struct {
	MetricName    string `yaml:"metric_name"`
	MetricVersion string `yaml:"metric_version"`
	Direction     string `yaml:"direction"`
	IsPrimary     bool   `yaml:"is_primary"`
}

// ManifestPromotion is the optional rule-promotion block. When
// present and the terminated experiment has a declared winner,
// MaybePromoteRule mints a PromotionProposal with this content.
type ManifestPromotion struct {
	RuleKey         string `yaml:"rule_key"`
	ProposedContent string `yaml:"proposed_content"`
}

func validateManifest(m Manifest) error {
	if strings.TrimSpace(m.Name) == "" {
		return errors.New("manifest: name is required")
	}
	if strings.TrimSpace(m.Hypothesis) == "" {
		return errors.New("manifest: hypothesis is required (paired-runs.md § Pre-registration)")
	}
	if strings.TrimSpace(m.SubjectAgent) == "" {
		return errors.New("manifest: subject_agent is required")
	}
	if strings.TrimSpace(m.AssignmentUnit) == "" {
		return errors.New("manifest: assignment_unit is required")
	}
	if len(m.Treatments) < 2 {
		return fmt.Errorf("manifest: at least two treatments required (got %d) — single-treatment experiments still need a control arm", len(m.Treatments))
	}
	primaries := 0
	for _, mm := range m.Metrics {
		if mm.IsPrimary {
			primaries++
		}
	}
	if primaries != 1 {
		return fmt.Errorf("manifest: exactly one metric must be is_primary=true (got %d)", primaries)
	}
	return nil
}

// AuthorFromYAML parses the manifest at yamlPath, validates it, and
// inserts the experiment + its treatments + its metrics into the
// database in a single transaction. Returns the new Experiments.id.
//
// The experiment's status starts at 'authored'; it cannot be enrolled
// against until Ratify flips it to 'running'.
func AuthorFromYAML(ctx context.Context, db *sql.DB, yamlPath string) (int, error) {
	body, err := os.ReadFile(yamlPath)
	if err != nil {
		return 0, fmt.Errorf("AuthorFromYAML: read %s: %w", yamlPath, err)
	}
	return AuthorFromBytes(ctx, db, body)
}

// AuthorFromBytes is the byte-shape sibling of AuthorFromYAML — used
// by tests that build a manifest in memory and by future callers
// that author from EC-emitted templates.
func AuthorFromBytes(ctx context.Context, db *sql.DB, raw []byte) (int, error) {
	var m Manifest
	if err := yaml.Unmarshal(raw, &m); err != nil {
		return 0, fmt.Errorf("AuthorFromYAML: parse: %w", err)
	}
	return AuthorFromManifest(ctx, db, m)
}

// AuthorFromManifest writes the experiment rows for a parsed manifest.
// The single-transaction shape avoids leaving an Experiments row
// without its arm / metric children if a downstream insert fails.
func AuthorFromManifest(ctx context.Context, db *sql.DB, m Manifest) (int, error) {
	if err := validateManifest(m); err != nil {
		return 0, err
	}
	if m.AnalysisFrameworkVersion == "" {
		m.AnalysisFrameworkVersion = analysis.BayesianBetaBinomialVersion
	}
	if m.StakesTier == "" {
		m.StakesTier = "low"
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("AuthorFromManifest: begin tx: %w", err)
	}
	defer tx.Rollback()

	res, err := tx.ExecContext(ctx, `
		INSERT INTO Experiments
			(name, hypothesis_text, min_practical_effect, stakes_tier,
			 subject_agent, assignment_unit, analysis_framework_version,
			 status, duration_cap_hours, budget_usd, hard_cap_usd,
			 created_by, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, datetime('now'))
	`,
		m.Name, m.Hypothesis, m.MinPracticalEffect, m.StakesTier,
		m.SubjectAgent, m.AssignmentUnit, m.AnalysisFrameworkVersion,
		StatusAuthored, m.DurationCapHours, m.BudgetUSD, m.HardCapUSD,
		"engineering-corps",
	)
	if err != nil {
		return 0, fmt.Errorf("AuthorFromManifest: insert experiment: %w", err)
	}
	expID64, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("AuthorFromManifest: experiment id: %w", err)
	}
	expID := int(expID64)

	for _, tr := range m.Treatments {
		// Salt the spec_hash with expID so re-authoring the same
		// manifest twice produces distinct rows. The cross-experiment
		// dedup property (paired-runs.md § Data Model — "identical
		// treatments across experiments share rows") is deferred to a
		// later phase that handles the lookup-or-insert dance properly.
		specHash := contentHash(fmt.Sprintf("%d|%s|%s|%s", expID, tr.ArmLabel, tr.PromptTemplateRef, tr.Model))
		var specID int
		err := tx.QueryRowContext(ctx, `
			INSERT INTO TreatmentSpecs (spec_hash, prompt_template_ref, model_identifier)
			VALUES (?, ?, ?)
			RETURNING id
		`, specHash, tr.PromptTemplateRef, tr.Model).Scan(&specID)
		if err != nil {
			return 0, fmt.Errorf("AuthorFromManifest: insert treatment spec %q: %w", tr.ArmLabel, err)
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO ExperimentTreatments
				(experiment_id, arm_label, treatment_spec_id, target_cell_weight)
			VALUES (?, ?, ?, ?)
		`, expID, tr.ArmLabel, specID, tr.TargetCellWeight); err != nil {
			return 0, fmt.Errorf("AuthorFromManifest: insert treatment %q: %w", tr.ArmLabel, err)
		}
	}

	for _, mm := range m.Metrics {
		isPrimary := 0
		if mm.IsPrimary {
			isPrimary = 1
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO ExperimentMetrics
				(experiment_id, metric_name, metric_version, direction, is_primary)
			VALUES (?, ?, ?, ?, ?)
		`, expID, mm.MetricName, mm.MetricVersion, mm.Direction, isPrimary); err != nil {
			return 0, fmt.Errorf("AuthorFromManifest: insert metric %q: %w", mm.MetricName, err)
		}
	}

	// Stash the optional promotion block as JSON on the experiment's
	// termination_reason field — repurposed at termination time.
	// (Phase 2 stays small here; a dedicated PromotionTemplate table
	// can land in Phase 4 if the audit trail matures.)
	if m.Promote != nil {
		body, _ := json.Marshal(m.Promote)
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO SystemConfig (key, value) VALUES (?, ?)
			ON CONFLICT(key) DO UPDATE SET value = excluded.value
		`, fmt.Sprintf("experiment_promote_%d", expID), string(body)); err != nil {
			return 0, fmt.Errorf("AuthorFromManifest: cache promote block: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("AuthorFromManifest: commit: %w", err)
	}
	return expID, nil
}

// Ratify is the operator-routed gate from 'authored' to 'running'.
// Requires a non-empty operatorEmail. Records an AuditLog row.
//
// Conditional update (CAS): if status is not currently 'authored',
// no row is updated and the call returns an error. This prevents
// re-ratifying an already-running experiment and is the shape Fix #8d
// codifies (UpdateBountyStatusFrom-style CAS for any conditional
// state mutation).
func Ratify(ctx context.Context, db *sql.DB, experimentID int, operatorEmail string) error {
	if strings.TrimSpace(operatorEmail) == "" {
		return errors.New("Ratify: operatorEmail is required (operator-routed gate, paired-runs.md § Pre-registration)")
	}
	res, err := db.ExecContext(ctx, `
		UPDATE Experiments
		SET status = ?, ratified_at = datetime('now'), ratified_by = ?, started_at = datetime('now')
		WHERE id = ? AND status = ?
	`, StatusRunning, operatorEmail, experimentID, StatusAuthored)
	if err != nil {
		return fmt.Errorf("Ratify: update: %w", err)
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("Ratify: rows affected: %w", err)
	}
	if rows == 0 {
		return fmt.Errorf("Ratify: experiment %d not in 'authored' state — refusing to flip", experimentID)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO AuditLog (actor, action, task_id, detail)
		VALUES (?, 'experiment.ratify', ?, ?)
	`, operatorEmail, experimentID, fmt.Sprintf("Ratified experiment %d to running", experimentID)); err != nil {
		return fmt.Errorf("Ratify: audit log: %w", err)
	}
	return nil
}

// EnrollUnit assigns a natural unit to one of the experiment's arms,
// recording an ExperimentRuns row. Idempotent: a second call for the
// same (experiment, kind, id) returns the same treatment_id and does
// not insert a duplicate row.
func EnrollUnit(ctx context.Context, db *sql.DB, experimentID int, kind string, unitID int) (int, error) {
	// Check existing assignment.
	var existing int
	err := db.QueryRowContext(ctx, `
		SELECT treatment_id FROM ExperimentRuns
		WHERE experiment_id = ? AND natural_unit_kind = ? AND natural_unit_id = ?
		ORDER BY id LIMIT 1
	`, experimentID, kind, unitID).Scan(&existing)
	switch {
	case err == nil:
		return existing, nil
	case err != sql.ErrNoRows:
		return 0, fmt.Errorf("EnrollUnit: lookup existing: %w", err)
	}

	treats, err := loadTreatments(ctx, db, experimentID)
	if err != nil {
		return 0, err
	}
	if len(treats) == 0 {
		return 0, fmt.Errorf("EnrollUnit: experiment %d has no treatments", experimentID)
	}
	picked := pickArm(experimentID, kind, unitID, treats)
	if _, err := db.ExecContext(ctx, `
		INSERT INTO ExperimentRuns
			(experiment_id, treatment_id, natural_unit_kind, natural_unit_id,
			 mode, agent_name, assigned_at)
		VALUES (?, ?, ?, ?, 'paired_real', '', ?)
	`, experimentID, picked.ID, kind, unitID, store.NowSQLite()); err != nil {
		return 0, fmt.Errorf("EnrollUnit: insert run: %w", err)
	}
	return picked.ID, nil
}

// armRow is the in-memory shape of an ExperimentTreatments row used
// by the picker.
type armRow struct {
	ID               int
	ArmLabel         string
	TargetCellWeight float64
}

func loadTreatments(ctx context.Context, db *sql.DB, experimentID int) ([]armRow, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT id, arm_label, IFNULL(target_cell_weight, 0)
		FROM ExperimentTreatments
		WHERE experiment_id = ?
		ORDER BY id
	`, experimentID)
	if err != nil {
		return nil, fmt.Errorf("loadTreatments: %w", err)
	}
	defer rows.Close()
	var out []armRow
	for rows.Next() {
		var a armRow
		if err := rows.Scan(&a.ID, &a.ArmLabel, &a.TargetCellWeight); err != nil {
			return nil, fmt.Errorf("loadTreatments scan: %w", err)
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// pickArm is the canonical hash-bucket arm picker, identical in
// shape to internal/treatments.pickTreatment so live-Apply and
// EnrollUnit produce the same arm for the same (experiment, unit).
func pickArm(experimentID int, kind string, unitID int, treats []armRow) armRow {
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

func hashUint64(experimentID int, kind string, unitID int) uint64 {
	key := fmt.Sprintf("%d:%s:%d", experimentID, kind, unitID)
	sum := sha256.Sum256([]byte(key))
	return binary.BigEndian.Uint64(sum[:8])
}

func hashFraction(experimentID int, kind string, unitID int) float64 {
	return float64(hashUint64(experimentID, kind, unitID)) / 18446744073709551616.0
}

// Terminate transitions a running experiment to 'terminated',
// computes its outcome via the configured analysis framework, and
// writes the ExperimentOutcomes row. Idempotent on the experiment's
// status — a re-call against an already-terminated experiment errors
// rather than rewriting the outcome.
func Terminate(ctx context.Context, db *sql.DB, experimentID int, reason string) error {
	if strings.TrimSpace(reason) == "" {
		reason = "operator_closed"
	}

	// Status CAS — only running/confirming experiments can be terminated.
	res, err := db.ExecContext(ctx, `
		UPDATE Experiments
		SET status = ?, terminated_at = datetime('now'), termination_reason = ?
		WHERE id = ? AND status IN (?, ?)
	`, StatusTerminated, reason, experimentID, StatusRunning, StatusConfirming)
	if err != nil {
		return fmt.Errorf("Terminate: update: %w", err)
	}
	if rows, _ := res.RowsAffected(); rows == 0 {
		return fmt.Errorf("Terminate: experiment %d is not running/confirming — refusing to flip", experimentID)
	}

	outcome, err := computeOutcome(ctx, db, experimentID)
	if err != nil {
		return fmt.Errorf("Terminate: compute outcome: %w", err)
	}
	cellMeans, _ := json.Marshal(outcome.CellMeans)
	if _, err := db.ExecContext(ctx, `
		INSERT INTO ExperimentOutcomes
			(experiment_id, terminated_at, termination_reason,
			 winner_treatment_id, winner_posterior, winner_effect_estimate,
			 cell_means_json)
		VALUES (?, datetime('now'), ?, ?, ?, ?, ?)
	`, experimentID, outcome.TerminationReason,
		outcome.WinnerTreatmentID, outcome.WinnerPosterior, outcome.WinnerEffectEstimate,
		string(cellMeans),
	); err != nil {
		return fmt.Errorf("Terminate: insert outcome: %w", err)
	}
	return nil
}

// outcomeResult is the in-memory shape of a computed outcome — the
// caller flattens it into ExperimentOutcomes columns.
type outcomeResult struct {
	TerminationReason    string
	WinnerTreatmentID    int
	WinnerPosterior      float64
	WinnerEffectEstimate float64
	CellMeans            map[string]float64
}

// computeOutcome reads the experiment's runs, groups by treatment
// (arm), interprets ExperimentRuns.score as a Bernoulli outcome
// (1=success, 0=failure), and runs the Bayesian framework over the
// two-arm comparison. With more than two arms, the framework is
// applied pairwise treatment-vs-control and the highest-confidence
// winner reported.
//
// "Control" is identified by arm_label='control'; if no arm carries
// that label, the lowest-id arm is used.
func computeOutcome(ctx context.Context, db *sql.DB, experimentID int) (outcomeResult, error) {
	type armSummary struct {
		ID        int
		Label     string
		Trials    int
		Successes int
	}
	rows, err := db.QueryContext(ctx, `
		SELECT t.id, t.arm_label, COUNT(r.id), IFNULL(SUM(CASE WHEN r.score >= 0.5 THEN 1 ELSE 0 END), 0)
		FROM ExperimentTreatments t
		LEFT JOIN ExperimentRuns r ON r.treatment_id = t.id
		WHERE t.experiment_id = ?
		GROUP BY t.id
		ORDER BY t.id
	`, experimentID)
	if err != nil {
		return outcomeResult{}, fmt.Errorf("computeOutcome: query: %w", err)
	}
	defer rows.Close()
	var arms []armSummary
	for rows.Next() {
		var a armSummary
		if err := rows.Scan(&a.ID, &a.Label, &a.Trials, &a.Successes); err != nil {
			return outcomeResult{}, fmt.Errorf("computeOutcome: scan: %w", err)
		}
		arms = append(arms, a)
	}
	if err := rows.Err(); err != nil {
		return outcomeResult{}, fmt.Errorf("computeOutcome: rows: %w", err)
	}

	cellMeans := map[string]float64{}
	for _, a := range arms {
		if a.Trials == 0 {
			cellMeans[a.Label] = 0
		} else {
			cellMeans[a.Label] = float64(a.Successes) / float64(a.Trials)
		}
	}

	// Identify control.
	controlIdx := -1
	for i, a := range arms {
		if a.Label == "control" {
			controlIdx = i
			break
		}
	}
	if controlIdx < 0 && len(arms) > 0 {
		controlIdx = 0
	}
	if controlIdx < 0 {
		return outcomeResult{
			TerminationReason: "inconclusive",
			CellMeans:         cellMeans,
		}, nil
	}

	bestPosterior := 0.0
	winnerID := 0
	winnerEffect := 0.0
	winnerLabel := ""
	control := arms[controlIdx]
	for i, a := range arms {
		if i == controlIdx {
			continue
		}
		d, err := analysis.DecideOutcome(
			analysis.ObservedCounts{Successes: a.Successes, Trials: a.Trials},
			analysis.ObservedCounts{Successes: control.Successes, Trials: control.Trials},
			analysis.DecisionRule{},
		)
		if err != nil {
			continue
		}
		if d.Winner == "treatment" && d.Confidence > bestPosterior {
			bestPosterior = d.Confidence
			winnerID = a.ID
			winnerLabel = a.Label
			winnerEffect = cellMeans[a.Label] - cellMeans[control.Label]
		}
	}

	reason := "inconclusive"
	if winnerID > 0 {
		reason = "declared_winner"
	}
	if winnerLabel == "" {
		// Confirm the control didn't outright win (a non-treatment arm
		// could be best). For this Phase 2 surface we only declare
		// non-control arms as winners; a control "win" is recorded as
		// inconclusive (operator decides whether to retire the
		// treatment in a follow-up).
		_ = control
	}
	return outcomeResult{
		TerminationReason:    reason,
		WinnerTreatmentID:    winnerID,
		WinnerPosterior:      bestPosterior,
		WinnerEffectEstimate: winnerEffect,
		CellMeans:            cellMeans,
	}, nil
}

// MaybePromoteRule mints a PromotionProposal IFF the terminated
// experiment has a declared winner AND the manifest specified a
// promotion block. Returns the new proposal's id (or 0 if no
// proposal was minted).
func MaybePromoteRule(ctx context.Context, db *sql.DB, experimentID int) (int, error) {
	var status string
	var outcomeReason sql.NullString
	if err := db.QueryRowContext(ctx, `SELECT status FROM Experiments WHERE id = ?`, experimentID).Scan(&status); err != nil {
		return 0, fmt.Errorf("MaybePromoteRule: load experiment: %w", err)
	}
	if status != StatusTerminated {
		return 0, nil
	}
	var winnerTreatmentID int
	var winnerPosterior float64
	var cellMeansJSON string
	err := db.QueryRowContext(ctx, `
		SELECT termination_reason, IFNULL(winner_treatment_id, 0),
		       IFNULL(winner_posterior, 0), IFNULL(cell_means_json, '{}')
		FROM ExperimentOutcomes
		WHERE experiment_id = ?
	`, experimentID).Scan(&outcomeReason, &winnerTreatmentID, &winnerPosterior, &cellMeansJSON)
	if err != nil {
		return 0, nil
	}
	if !outcomeReason.Valid || outcomeReason.String != "declared_winner" || winnerTreatmentID == 0 {
		return 0, nil
	}
	var promoteJSON string
	err = db.QueryRowContext(ctx, `SELECT value FROM SystemConfig WHERE key = ?`,
		fmt.Sprintf("experiment_promote_%d", experimentID)).Scan(&promoteJSON)
	if err == sql.ErrNoRows || promoteJSON == "" {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("MaybePromoteRule: read promote block: %w", err)
	}
	var p ManifestPromotion
	if err := json.Unmarshal([]byte(promoteJSON), &p); err != nil {
		return 0, fmt.Errorf("MaybePromoteRule: unmarshal promote: %w", err)
	}
	evidence := map[string]any{
		"experiment_id":          experimentID,
		"winner_treatment_id":    winnerTreatmentID,
		"winner_posterior":       winnerPosterior,
		"cell_means_json":        cellMeansJSON,
		"analysis_framework":     analysis.BayesianBetaBinomialName,
		"analysis_version":       analysis.BayesianBetaBinomialVersion,
	}
	evidenceJSON, _ := json.Marshal(evidence)

	var proposalID int
	err = db.QueryRowContext(ctx, `
		INSERT INTO PromotionProposals
			(experiment_id, kind, rule_key, proposed_content, evidence_summary_json,
			 authored_by, authored_at, ttl_expires_at)
		VALUES (?, 'promote', ?, ?, ?, 'engineering-corps', datetime('now'), datetime('now', '+14 days'))
		RETURNING id
	`, experimentID, p.RuleKey, p.ProposedContent, string(evidenceJSON)).Scan(&proposalID)
	if err != nil {
		return 0, fmt.Errorf("MaybePromoteRule: insert proposal: %w", err)
	}
	return proposalID, nil
}

// Status reads the experiment's current status — useful for CLI
// status commands and for tests asserting transitions.
type Status struct {
	ID                int
	Name              string
	Status            string
	StakesTier        string
	SubjectAgent      string
	AssignmentUnit    string
	EnrollmentByArm   map[string]int
	OutcomeReason     string
	WinnerTreatmentID int
	WinnerPosterior   float64
}

// GetStatus reads the lifecycle state for one experiment, plus
// per-arm enrollment counts and outcome (if terminated).
func GetStatus(ctx context.Context, db *sql.DB, experimentID int) (Status, error) {
	var s Status
	err := db.QueryRowContext(ctx, `
		SELECT id, name, status, IFNULL(stakes_tier, ''),
		       IFNULL(subject_agent, ''), IFNULL(assignment_unit, '')
		FROM Experiments WHERE id = ?
	`, experimentID).Scan(&s.ID, &s.Name, &s.Status, &s.StakesTier, &s.SubjectAgent, &s.AssignmentUnit)
	if err != nil {
		return Status{}, fmt.Errorf("GetStatus: load: %w", err)
	}
	rows, err := db.QueryContext(ctx, `
		SELECT t.arm_label, COUNT(r.id)
		FROM ExperimentTreatments t
		LEFT JOIN ExperimentRuns r ON r.treatment_id = t.id
		WHERE t.experiment_id = ?
		GROUP BY t.id
	`, experimentID)
	if err == nil {
		defer rows.Close()
		s.EnrollmentByArm = map[string]int{}
		for rows.Next() {
			var arm string
			var n int
			if err := rows.Scan(&arm, &n); err == nil {
				s.EnrollmentByArm[arm] = n
			}
		}
		if rErr := rows.Err(); rErr != nil {
			return s, fmt.Errorf("GetStatus: enrollment rows: %w", rErr)
		}
	}
	if s.Status == StatusTerminated {
		var reason sql.NullString
		var winner int
		var posterior float64
		_ = db.QueryRowContext(ctx, `
			SELECT termination_reason, IFNULL(winner_treatment_id, 0), IFNULL(winner_posterior, 0)
			FROM ExperimentOutcomes WHERE experiment_id = ?
		`, experimentID).Scan(&reason, &winner, &posterior)
		if reason.Valid {
			s.OutcomeReason = reason.String
		}
		s.WinnerTreatmentID = winner
		s.WinnerPosterior = posterior
	}
	return s, nil
}

// ──────────────────────────────────────────────────────────────────────────
// helpers
// ──────────────────────────────────────────────────────────────────────────

func contentHash(s string) string {
	sum := sha256.Sum256([]byte(s))
	// Truncate — TreatmentSpecs.spec_hash is just a uniqueness key, no
	// security-sensitive interpretation.
	return hex.EncodeToString(sum[:16])
}
