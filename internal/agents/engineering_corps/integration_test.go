package engineering_corps

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"

	"force-orchestrator/internal/agents/capabilities"
	"force-orchestrator/internal/clients/librarian"
	"force-orchestrator/internal/clients/metrics"
	"force-orchestrator/internal/store"
)

// TestDispatcher_AllSixTypes_HandlersWired is the post-Phase-3-A
// regression that asserts every EC task type, when dispatched
// through dispatch() directly, reaches a real handler — NOT a
// Phase-1 stub returning ErrNotImplemented.
//
// Per-type fixtures are seeded so each handler has something
// defensible to consume. After dispatch:
//
//   - The bounty is Completed OR Failed (handler terminated).
//   - The error_log (if any) does NOT contain "not implemented"
//     / "ErrNotImplemented" — that string would prove the
//     dispatcher is still routing to a stub.
//
// Until Phase 3-A's work landed, every task type would fail with
// "EC <type> handler stub: handler not implemented". The test
// catches a regression that re-introduces the stub for any type.
func TestDispatcher_AllSixTypes_HandlersWired(t *testing.T) {
	withTempCWD(t)

	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	// Stub Claude for the LLM-driven handlers (ExperimentAuthor,
	// MetricAuthor). SQL-only handlers ignore the stub.
	stubClaudeReturning(t, validAuthorJSON(), nil)

	cfg := EngineeringCorpsConfig{
		Name:      "EC-int-test",
		DB:        db,
		Librarian: librarian.NewInProcess(db),
		Metrics:   metrics.NewInProcess(),
	}
	if err := cfg.validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
	profile, err := capabilities.LoadProfile("engineering-corps")
	if err != nil {
		t.Fatalf("LoadProfile: %v", err)
	}
	logger := newTestLogger()

	for _, taskType := range AllTaskTypes {
		t.Run(taskType, func(t *testing.T) {
			payload := seedAndPayloadFor(t, db, taskType)
			id := store.AddBounty(db, 0, taskType, payload)
			bounty, claimed := store.ClaimBounty(db, taskType, "EC-int-test")
			if !claimed || bounty == nil || bounty.ID != id {
				t.Fatalf("ClaimBounty(%s) failed: bounty=%v claimed=%v", taskType, bounty, claimed)
			}

			dispatch(context.Background(), cfg, profile, "EC-int-test", taskType, bounty, logger.std())

			fresh, err := store.GetBounty(db, id)
			if err != nil {
				t.Fatalf("GetBounty: %v", err)
			}

			errLog := readErrorLog(t, db, id)
			lower := strings.ToLower(errLog)
			if strings.Contains(lower, "not implemented") || strings.Contains(lower, "errnotimplemented") {
				t.Errorf("%s: error_log carries stub sentinel — dispatcher routed to a stub. error_log=%q",
					taskType, errLog)
			}
			if fresh.Status != "Completed" && fresh.Status != "Failed" {
				t.Errorf("%s: status = %q, want Completed or Failed (handler must terminate)",
					taskType, fresh.Status)
			}
		})
	}
}

// seedAndPayloadFor seeds any DB rows the handler will read on its
// happy / non-stub path AND returns the BountyBoard.payload that
// drives the handler's execution.
//
// The goal is realism, not happy-path success: each handler must
// reach real logic (no ErrNotImplemented stub fall-through). Some
// fixtures land on Completed; others land on Failed with a real
// validation error — both prove the stub is gone.
func seedAndPayloadFor(t *testing.T, db *sql.DB, taskType string) string {
	t.Helper()
	switch taskType {
	case TaskTypeExperimentAuthor:
		// Override-path payload — no proposal_id, just the hypothesis
		// + rule_key. The stubbed Claude returns a valid manifest;
		// AuthorFromManifest writes the row in `authored` state.
		return `{"hypothesis_text":"Captain rejection rate is rising","rule_key":"captain-strict-flag"}`

	case TaskTypeExperimentMonitor:
		// Empty payload triggers the heartbeat shape (scan all
		// running experiments). With zero running experiments this
		// is a clean no-op Completed.
		return `{}`

	case TaskTypePromotionAuthor:
		// Seed a terminated experiment with a declared winner.
		expID, _ := seedTerminatedWinnerInline(t, db, "exp-int-promote")
		return fmt.Sprintf(`{"experiment_id":%d}`, expID)

	case TaskTypeDemotionAuthor:
		// Seed a stale ratified promotion so the handler authors a
		// demotion proposal.
		res, _ := db.Exec(`INSERT INTO Experiments (name, status) VALUES ('exp-int-demo', 'terminated')`)
		expID, _ := res.LastInsertId()
		db.Exec(`
			INSERT INTO PromotionProposals
				(experiment_id, kind, rule_key, proposed_content, evidence_summary_json,
				 authored_by, authored_at, ratified_at, ttl_expires_at)
			VALUES (?, 'promote', 'rule-int-demo', '', '{}', 'engineering-corps',
			        datetime('now', '-100 days'),
			        datetime('now', '-90 days'),
			        datetime('now', '+14 days'))
		`, expID)
		return `{}`

	case TaskTypeMetricAuthor:
		// Stubbed Claude returns the experiment-author JSON which
		// won't strict-decode into metricAuthorResponse — that's OK,
		// the handler will Fail with a parse error (NOT
		// ErrNotImplemented), proving the stub is gone.
		return `{"hypothesis_text":"X is rising","metric_name":"x-rate"}`

	case TaskTypeHoldoutMonitor:
		return `{}`

	default:
		t.Fatalf("seedAndPayloadFor: unknown task type %q", taskType)
		return ""
	}
}

// seedTerminatedWinnerInline mirrors seedTerminatedWinner from
// promotion_author_test.go but lives here to avoid cross-test
// dependencies. Returns experimentID + winnerTreatmentID.
func seedTerminatedWinnerInline(t *testing.T, db *sql.DB, name string) (int, int) {
	t.Helper()
	res, err := db.Exec(`
		INSERT INTO Experiments (name, hypothesis_text, stakes_tier, subject_agent, assignment_unit,
		                          analysis_framework_version, status, terminated_at, termination_reason)
		VALUES (?, ?, 'low', 'captain', 'task', '2026-04-23', 'terminated', datetime('now'), 'declared_winner')
	`, name, name+" hyp")
	if err != nil {
		t.Fatalf("seed exp: %v", err)
	}
	expID64, _ := res.LastInsertId()
	expID := int(expID64)

	specCtrl, _ := db.Exec(`INSERT INTO TreatmentSpecs (spec_hash) VALUES (?)`, fmt.Sprintf("ctrl-int-%d", expID))
	specCtrlID, _ := specCtrl.LastInsertId()
	specTreat, _ := db.Exec(`INSERT INTO TreatmentSpecs (spec_hash) VALUES (?)`, fmt.Sprintf("treat-int-%d", expID))
	specTreatID, _ := specTreat.LastInsertId()
	db.Exec(`INSERT INTO ExperimentTreatments (experiment_id, arm_label, treatment_spec_id) VALUES (?, 'control', ?)`, expID, specCtrlID)
	armRes, _ := db.Exec(`INSERT INTO ExperimentTreatments (experiment_id, arm_label, treatment_spec_id) VALUES (?, 'treatment', ?)`, expID, specTreatID)
	winnerID64, _ := armRes.LastInsertId()
	winnerID := int(winnerID64)

	db.Exec(`
		INSERT INTO ExperimentOutcomes
			(experiment_id, termination_reason, winner_treatment_id, winner_posterior,
			 winner_effect_estimate, cell_means_json,
			 fleet_state_hash_at_start, fleet_state_hash_at_end, confirm_phase_outcome)
		VALUES (?, 'declared_winner', ?, 0.99, 0.5, '{"control":0.2,"treatment":0.7}', 'a', 'b', '')
	`, expID, winnerID)
	return expID, winnerID
}

// readErrorLog reads BountyBoard.error_log for a row. Returns "" on
// error (defensive — the test asserts negative content; an empty
// string never matches the stub sentinel).
func readErrorLog(t *testing.T, db *sql.DB, id int) string {
	t.Helper()
	var s string
	_ = db.QueryRow(`SELECT IFNULL(error_log,'') FROM BountyBoard WHERE id=?`, id).Scan(&s)
	return s
}
