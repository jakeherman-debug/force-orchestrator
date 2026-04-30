package engineering_corps

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"strings"

	"force-orchestrator/internal/agents/capabilities"
	"force-orchestrator/internal/analysis"
	"force-orchestrator/internal/experiments"
	"force-orchestrator/internal/store"
)

// ExperimentMonitor — the SQL-only watcher that decides when a
// running experiment terminates and queues the PromotionAuthor
// follow-up.
//
// Per docs/paired-runs.md § Engineering Corps + § Emergency stop, the
// monitor's job is to:
//
//   1. Read the experiment's runs (per-arm successes / trials).
//   2. Run the Bayesian framework over the (treatment, control) pair.
//   3. Compare the posterior against:
//        a. The declare-winner threshold (e.g. 0.95).
//        b. The emergency-stop threshold (>0.9 of "treatment is worse")
//           AFTER min_runs_for_kill (default 20) runs.
//   4. Terminate the experiment with the appropriate
//      ExperimentOutcomes.termination_reason.
//   5. On declared_winner, queue an ECPromotionAuthor follow-up
//      bounty so the next operator-routed step (assemble ratifiable
//      proposal) executes.
//
// Operator-routing invariant: the monitor declares winners but does
// NOT auto-ratify. PromotionAuthor writes the PromotionProposals row
// with ratified_at='', leaving the operator-on-dashboard ratification
// gate intact.
//
// Inputs (BountyBoard.payload JSON):
//   { "experiment_id": <int> }
// If experiment_id is missing or zero, the handler scans every
// running experiment (heartbeat shape — useful as a periodic dog).
//
// No LLM call. No worktree. Pure SQL + the analysis framework.
type experimentMonitorPayload struct {
	ExperimentID int `json:"experiment_id"`
}

// thresholds — kept as package-level vars so tests can flex them. The
// production defaults track docs/paired-runs.md.
var (
	// declareWinnerPosterior is the posterior probability above which
	// a non-control arm is declared the winner.
	declareWinnerPosterior = 0.95

	// minRunsForKill is the minimum trial count below which we never
	// declare an outcome — paired-runs.md "min_runs_for_kill=20".
	minRunsForKill = 20
)

func handleExperimentMonitor(
	ctx context.Context,
	cfg EngineeringCorpsConfig,
	_ *capabilities.Profile,
	agentName string,
	bounty *store.Bounty,
	logger *log.Logger,
) error {
	db := cfg.DB

	var payload experimentMonitorPayload
	// Strict JSON decode (Fix #8.5) — empty payload is allowed; it's
	// treated as "scan every running experiment". A malformed payload
	// fails the bounty rather than silently scanning.
	if len(bounty.Payload) > 0 && bounty.Payload != "{}" {
		dec := json.NewDecoder(strings.NewReader(bounty.Payload))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&payload); err != nil {
			return fmt.Errorf("ExperimentMonitor: parse payload: %w", err)
		}
		if dec.More() {
			return fmt.Errorf("ExperimentMonitor: trailing tokens in payload")
		}
	}

	candidates, err := loadMonitorCandidates(db, payload.ExperimentID)
	if err != nil {
		return fmt.Errorf("ExperimentMonitor: load candidates: %w", err)
	}

	terminated := 0
	winnersQueued := 0
	for _, expID := range candidates {
		decision, err := evaluateExperiment(ctx, db, expID)
		if err != nil {
			logger.Printf("[%s] ExperimentMonitor #%d: experiment %d evaluate failed: %v", agentName, bounty.ID, expID, err)
			continue
		}
		if decision == nil {
			// Not enough data yet OR still within thresholds.
			continue
		}
		// Drive termination through the canonical experiments helper
		// — it writes ExperimentOutcomes atomically with the status
		// flip (CAS via UPDATE..WHERE status IN running/confirming).
		if err := experiments.Terminate(ctx, db, expID, decision.Reason); err != nil {
			logger.Printf("[%s] ExperimentMonitor #%d: terminate exp %d (%s) failed: %v", agentName, bounty.ID, expID, decision.Reason, err)
			continue
		}
		terminated++
		logger.Printf("[%s] ExperimentMonitor #%d: experiment %d terminated reason=%s posterior=%.4f",
			agentName, bounty.ID, expID, decision.Reason, decision.Posterior)

		// On declared_winner, queue the next-stage handler so it can
		// assemble the ratifiable PromotionProposal. PromotionAuthor
		// is operator-routed (the proposal lands unratified). We DO
		// NOT auto-ratify here.
		if decision.Reason == "declared_winner" {
			authorPayload, _ := json.Marshal(map[string]int{"experiment_id": expID})
			store.AddBounty(db, bounty.ID, TaskTypePromotionAuthor, string(authorPayload))
			winnersQueued++
		}
	}

	logger.Printf("[%s] ExperimentMonitor #%d: scanned %d running experiment(s), terminated %d, queued %d promotion author task(s)",
		agentName, bounty.ID, len(candidates), terminated, winnersQueued)

	if err := store.UpdateBountyStatus(db, bounty.ID, "Completed"); err != nil {
		return fmt.Errorf("ExperimentMonitor: complete bounty: %w", err)
	}
	return nil
}

// loadMonitorCandidates returns the experiment IDs the monitor will
// evaluate this run. If filterID is non-zero, only that experiment is
// returned (bounded to running/confirming statuses); otherwise every
// running/confirming experiment is returned.
func loadMonitorCandidates(db *sql.DB, filterID int) ([]int, error) {
	if filterID > 0 {
		var status string
		err := db.QueryRow(`SELECT status FROM Experiments WHERE id = ?`, filterID).Scan(&status)
		if err == sql.ErrNoRows {
			return nil, fmt.Errorf("experiment %d not found", filterID)
		}
		if err != nil {
			return nil, err
		}
		if status != experiments.StatusRunning && status != experiments.StatusConfirming {
			// Not eligible — log nothing, return empty so the heartbeat completes.
			return nil, nil
		}
		return []int{filterID}, nil
	}

	rows, err := db.Query(`
		SELECT id FROM Experiments
		WHERE status IN (?, ?)
		ORDER BY id
	`, experiments.StatusRunning, experiments.StatusConfirming)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []int
	for rows.Next() {
		var id int
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// monitorDecision is the in-memory output of evaluateExperiment. A
// nil return means "no termination yet" (within thresholds OR
// insufficient data).
type monitorDecision struct {
	Reason    string  // declared_winner|inconclusive|emergency_stop
	Posterior float64
}

// evaluateExperiment reads the experiment's runs, identifies the
// control + best treatment, runs the Bayesian framework, and returns
// a termination decision when thresholds are crossed.
//
// Sample-size guard: emergency-stop AND declared_winner both require
// trials >= minRunsForKill (per paired-runs.md "small samples cannot
// trigger emergency-stop"). Below that, the function returns nil and
// the caller skips termination.
func evaluateExperiment(ctx context.Context, db *sql.DB, experimentID int) (*monitorDecision, error) {
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
		return nil, err
	}
	defer rows.Close()
	var arms []armSummary
	for rows.Next() {
		var a armSummary
		if err := rows.Scan(&a.ID, &a.Label, &a.Trials, &a.Successes); err != nil {
			return nil, err
		}
		arms = append(arms, a)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(arms) < 2 {
		return nil, nil
	}

	// Identify control: arm_label='control' wins; else lowest id.
	controlIdx := 0
	for i, a := range arms {
		if a.Label == "control" {
			controlIdx = i
			break
		}
	}
	control := arms[controlIdx]

	// Sample-size gate for either decision.
	if control.Trials < minRunsForKill {
		return nil, nil
	}

	bestPosterior := 0.0
	bestArmTrials := 0
	bestEmergencyPosterior := 0.0
	bestEmergencyTrials := 0
	for i, a := range arms {
		if i == controlIdx {
			continue
		}
		if a.Trials < minRunsForKill {
			continue
		}
		// Winner direction: treatment > control.
		d, err := analysis.DecideOutcome(
			analysis.ObservedCounts{Successes: a.Successes, Trials: a.Trials},
			analysis.ObservedCounts{Successes: control.Successes, Trials: control.Trials},
			analysis.DecisionRule{},
		)
		if err == nil && d.Winner == "treatment" && d.Confidence > bestPosterior {
			bestPosterior = d.Confidence
			bestArmTrials = a.Trials
		}
		// Emergency-stop direction: control > treatment (treatment
		// worse). DecideOutcome returns Winner="control" when the
		// control wins; we use that confidence as P(treatment worse).
		if err == nil && d.Winner == "control" && d.Confidence > bestEmergencyPosterior {
			bestEmergencyPosterior = d.Confidence
			bestEmergencyTrials = a.Trials
		}
	}

	// Emergency-stop has priority over declared_winner: a "treatment
	// is worse" signal must terminate immediately even if some other
	// arm crosses the winner threshold (the worst-arm signal trumps).
	if bestEmergencyPosterior > 0.9 && bestEmergencyTrials >= minRunsForKill {
		return &monitorDecision{Reason: "emergency_stop", Posterior: bestEmergencyPosterior}, nil
	}
	if bestPosterior >= declareWinnerPosterior && bestArmTrials >= minRunsForKill {
		return &monitorDecision{Reason: "declared_winner", Posterior: bestPosterior}, nil
	}
	return nil, nil
}
