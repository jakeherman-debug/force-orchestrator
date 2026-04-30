package engineering_corps

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"

	"force-orchestrator/internal/store"
)

// seedRunningExperiment seeds a running 2-arm experiment with the
// given (control_successes / control_trials) and
// (treatment_successes / treatment_trials), returning the
// experiment_id. The treatment arm is named "treatment", control is
// "control".
func seedRunningExperiment(t *testing.T, db *sql.DB, name string, ctrlS, ctrlT, treatS, treatT int) int {
	t.Helper()
	res, err := db.Exec(`
		INSERT INTO Experiments (name, hypothesis_text, stakes_tier, subject_agent, assignment_unit, status)
		VALUES (?, ?, 'low', 'captain', 'task', 'running')
	`, name, name+" hypothesis")
	if err != nil {
		t.Fatalf("seed exp: %v", err)
	}
	expID64, _ := res.LastInsertId()
	expID := int(expID64)

	// Two TreatmentSpecs (so treatment_spec_id FK is satisfied).
	specCtrl, _ := db.Exec(`INSERT INTO TreatmentSpecs (spec_hash) VALUES (?)`, fmt.Sprintf("ctrl-%d", expID))
	specCtrlID, _ := specCtrl.LastInsertId()
	specTreat, _ := db.Exec(`INSERT INTO TreatmentSpecs (spec_hash) VALUES (?)`, fmt.Sprintf("treat-%d", expID))
	specTreatID, _ := specTreat.LastInsertId()

	armCtrl, err := db.Exec(`
		INSERT INTO ExperimentTreatments (experiment_id, arm_label, treatment_spec_id, target_cell_weight)
		VALUES (?, 'control', ?, 0.5)
	`, expID, specCtrlID)
	if err != nil {
		t.Fatalf("seed ctrl arm: %v", err)
	}
	ctrlArmID64, _ := armCtrl.LastInsertId()
	armTreat, err := db.Exec(`
		INSERT INTO ExperimentTreatments (experiment_id, arm_label, treatment_spec_id, target_cell_weight)
		VALUES (?, 'treatment', ?, 0.5)
	`, expID, specTreatID)
	if err != nil {
		t.Fatalf("seed treat arm: %v", err)
	}
	treatArmID64, _ := armTreat.LastInsertId()

	insertRuns(t, db, expID, int(ctrlArmID64), ctrlS, ctrlT, "task")
	insertRuns(t, db, expID, int(treatArmID64), treatS, treatT, "task")

	return expID
}

func insertRuns(t *testing.T, db *sql.DB, expID, treatmentID, successes, trials int, kind string) {
	t.Helper()
	for i := 0; i < trials; i++ {
		score := 0.0
		if i < successes {
			score = 1.0
		}
		_, err := db.Exec(`
			INSERT INTO ExperimentRuns
				(experiment_id, treatment_id, natural_unit_kind, natural_unit_id, mode, score)
			VALUES (?, ?, ?, ?, 'paired_real', ?)
		`, expID, treatmentID, kind, i+1, score)
		if err != nil {
			t.Fatalf("seed run: %v", err)
		}
	}
}

// TestHandleExperimentMonitor_HappyPath_DeclareWinner: treatment
// massively dominates control after >=minRunsForKill trials per arm —
// the monitor should terminate and queue a PromotionAuthor follow-up.
func TestHandleExperimentMonitor_HappyPath_DeclareWinner(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	// 50 trials each arm; treatment 95% / control 20%.
	expID := seedRunningExperiment(t, db, "exp-winner", 10, 50, 48, 50)

	cfg := EngineeringCorpsConfig{Name: "EC-test", DB: db}
	logger := newTestLogger()

	payload := fmt.Sprintf(`{"experiment_id":%d}`, expID)
	bid := store.AddBounty(db, 0, TaskTypeExperimentMonitor, payload)
	bounty, _ := store.ClaimBounty(db, TaskTypeExperimentMonitor, "EC-test")

	if err := handleExperimentMonitor(context.Background(), cfg, nil, "EC-test", bounty, logger.std()); err != nil {
		t.Fatalf("handler: %v", err)
	}

	// Experiment should have been terminated.
	var status, reason string
	if err := db.QueryRow(`SELECT status, IFNULL(termination_reason,'') FROM Experiments WHERE id=?`, expID).Scan(&status, &reason); err != nil {
		t.Fatalf("read exp: %v", err)
	}
	if status != "terminated" {
		t.Errorf("status = %q, want terminated", status)
	}
	if reason != "declared_winner" {
		t.Errorf("termination_reason = %q, want declared_winner", reason)
	}

	// Outcome row written.
	var outcomeReason string
	if err := db.QueryRow(`SELECT termination_reason FROM ExperimentOutcomes WHERE experiment_id=?`, expID).Scan(&outcomeReason); err != nil {
		t.Fatalf("read outcome: %v", err)
	}
	if outcomeReason != "declared_winner" {
		t.Errorf("outcome reason = %q, want declared_winner", outcomeReason)
	}

	// PromotionAuthor follow-up queued.
	var promoCount int
	if err := db.QueryRow(`SELECT COUNT(*) FROM BountyBoard WHERE type=? AND parent_id=?`,
		TaskTypePromotionAuthor, bid).Scan(&promoCount); err != nil {
		t.Fatalf("read follow-up: %v", err)
	}
	if promoCount != 1 {
		t.Errorf("expected 1 PromotionAuthor follow-up, got %d", promoCount)
	}

	fresh, _ := store.GetBounty(db, bid)
	if fresh.Status != "Completed" {
		t.Errorf("bounty status = %q, want Completed", fresh.Status)
	}
}

// TestHandleExperimentMonitor_BelowMinRuns: when an arm has < 20
// runs (minRunsForKill), the monitor must NOT terminate even when
// the apparent effect is large.
func TestHandleExperimentMonitor_BelowMinRuns(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	// Only 10 trials per arm — below the 20-run gate.
	expID := seedRunningExperiment(t, db, "exp-tiny", 1, 10, 10, 10)

	cfg := EngineeringCorpsConfig{Name: "EC-test", DB: db}
	logger := newTestLogger()
	payload := fmt.Sprintf(`{"experiment_id":%d}`, expID)
	store.AddBounty(db, 0, TaskTypeExperimentMonitor, payload)
	bounty, _ := store.ClaimBounty(db, TaskTypeExperimentMonitor, "EC-test")

	if err := handleExperimentMonitor(context.Background(), cfg, nil, "EC-test", bounty, logger.std()); err != nil {
		t.Fatalf("handler: %v", err)
	}

	var status string
	if err := db.QueryRow(`SELECT status FROM Experiments WHERE id=?`, expID).Scan(&status); err != nil {
		t.Fatalf("read: %v", err)
	}
	if status != "running" {
		t.Errorf("status = %q, want running (sample-size guard)", status)
	}
}

// TestHandleExperimentMonitor_EmergencyStop: control dominates
// treatment with high posterior — emergency_stop fires, no
// PromotionAuthor queued (we don't promote losers).
func TestHandleExperimentMonitor_EmergencyStop(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	// Treatment is dramatically worse than control.
	expID := seedRunningExperiment(t, db, "exp-bad", 48, 50, 5, 50)

	cfg := EngineeringCorpsConfig{Name: "EC-test", DB: db}
	logger := newTestLogger()
	payload := fmt.Sprintf(`{"experiment_id":%d}`, expID)
	bid := store.AddBounty(db, 0, TaskTypeExperimentMonitor, payload)
	bounty, _ := store.ClaimBounty(db, TaskTypeExperimentMonitor, "EC-test")

	if err := handleExperimentMonitor(context.Background(), cfg, nil, "EC-test", bounty, logger.std()); err != nil {
		t.Fatalf("handler: %v", err)
	}

	var reason string
	if err := db.QueryRow(`SELECT IFNULL(termination_reason,'') FROM Experiments WHERE id=?`, expID).Scan(&reason); err != nil {
		t.Fatalf("read: %v", err)
	}
	if reason != "emergency_stop" {
		t.Errorf("termination_reason = %q, want emergency_stop", reason)
	}

	var promoCount int
	db.QueryRow(`SELECT COUNT(*) FROM BountyBoard WHERE type=? AND parent_id=?`,
		TaskTypePromotionAuthor, bid).Scan(&promoCount)
	if promoCount != 0 {
		t.Errorf("expected NO PromotionAuthor on emergency_stop; got %d", promoCount)
	}
}

// TestHandleExperimentMonitor_OperatorRoutingPreserved: a
// declared_winner queues PromotionAuthor but DOES NOT directly
// write a ratified PromotionProposals row (operator ratifies via
// the dashboard).
func TestHandleExperimentMonitor_OperatorRoutingPreserved(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	expID := seedRunningExperiment(t, db, "exp-route", 10, 50, 48, 50)

	cfg := EngineeringCorpsConfig{Name: "EC-test", DB: db}
	logger := newTestLogger()
	payload := fmt.Sprintf(`{"experiment_id":%d}`, expID)
	store.AddBounty(db, 0, TaskTypeExperimentMonitor, payload)
	bounty, _ := store.ClaimBounty(db, TaskTypeExperimentMonitor, "EC-test")

	_ = handleExperimentMonitor(context.Background(), cfg, nil, "EC-test", bounty, logger.std())

	// Monitor is forbidden from writing PromotionProposals directly —
	// PromotionAuthor (separate handler) is responsible. Either way,
	// any proposal that DID land must NOT carry a ratified_at.
	var ratifiedCount int
	if err := db.QueryRow(`SELECT COUNT(*) FROM PromotionProposals WHERE IFNULL(ratified_at,'') != ''`).Scan(&ratifiedCount); err != nil {
		t.Fatalf("count ratified: %v", err)
	}
	if ratifiedCount != 0 {
		t.Errorf("PromotionProposals.ratified_at must remain '' (operator-routed); got %d ratified rows", ratifiedCount)
	}
}

// TestHandleExperimentMonitor_PayloadParseError: a malformed
// payload fails the bounty cleanly (no panic).
func TestHandleExperimentMonitor_PayloadParseError(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	cfg := EngineeringCorpsConfig{Name: "EC-test", DB: db}
	logger := newTestLogger()
	store.AddBounty(db, 0, TaskTypeExperimentMonitor, `{"experiment_id":"not-a-number"}`)
	bounty, _ := store.ClaimBounty(db, TaskTypeExperimentMonitor, "EC-test")

	err := handleExperimentMonitor(context.Background(), cfg, nil, "EC-test", bounty, logger.std())
	if err == nil {
		t.Fatalf("expected parse error, got nil")
	}
	if !strings.Contains(err.Error(), "parse payload") {
		t.Errorf("error should mention parse; got %v", err)
	}
}

// TestHandleExperimentMonitor_HeartbeatScansAll: empty payload
// scans every running experiment.
func TestHandleExperimentMonitor_HeartbeatScansAll(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	// Two running experiments — one ready to terminate, one not.
	winner := seedRunningExperiment(t, db, "exp-A", 10, 50, 48, 50)
	notReady := seedRunningExperiment(t, db, "exp-B", 5, 10, 5, 10)

	cfg := EngineeringCorpsConfig{Name: "EC-test", DB: db}
	logger := newTestLogger()
	store.AddBounty(db, 0, TaskTypeExperimentMonitor, `{}`)
	bounty, _ := store.ClaimBounty(db, TaskTypeExperimentMonitor, "EC-test")

	if err := handleExperimentMonitor(context.Background(), cfg, nil, "EC-test", bounty, logger.std()); err != nil {
		t.Fatalf("handler: %v", err)
	}

	var winnerStatus, notReadyStatus string
	db.QueryRow(`SELECT status FROM Experiments WHERE id=?`, winner).Scan(&winnerStatus)
	db.QueryRow(`SELECT status FROM Experiments WHERE id=?`, notReady).Scan(&notReadyStatus)
	if winnerStatus != "terminated" {
		t.Errorf("winner experiment status = %q, want terminated", winnerStatus)
	}
	if notReadyStatus != "running" {
		t.Errorf("not-ready experiment status = %q, want running", notReadyStatus)
	}
}
