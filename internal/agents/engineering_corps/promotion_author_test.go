package engineering_corps

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"force-orchestrator/internal/store"
)

// seedTerminatedWinner seeds a terminated experiment with a declared
// winner outcome row. Returns experimentID + winnerTreatmentID.
func seedTerminatedWinner(t *testing.T, db *sql.DB, name string) (int, int) {
	t.Helper()
	res, err := db.Exec(`
		INSERT INTO Experiments (name, hypothesis_text, stakes_tier, subject_agent, assignment_unit,
		                          analysis_framework_version, status, terminated_at, termination_reason)
		VALUES (?, ?, 'low', 'captain', 'task', '2026-04-23', 'terminated', datetime('now'), 'declared_winner')
	`, name, name+" hypothesis text")
	if err != nil {
		t.Fatalf("seed exp: %v", err)
	}
	expID64, _ := res.LastInsertId()
	expID := int(expID64)

	specCtrl, _ := db.Exec(`INSERT INTO TreatmentSpecs (spec_hash) VALUES (?)`, fmt.Sprintf("ctrl-%d", expID))
	specCtrlID, _ := specCtrl.LastInsertId()
	specTreat, _ := db.Exec(`INSERT INTO TreatmentSpecs (spec_hash) VALUES (?)`, fmt.Sprintf("treat-%d", expID))
	specTreatID, _ := specTreat.LastInsertId()

	db.Exec(`INSERT INTO ExperimentTreatments (experiment_id, arm_label, treatment_spec_id) VALUES (?, 'control', ?)`, expID, specCtrlID)
	armRes, _ := db.Exec(`INSERT INTO ExperimentTreatments (experiment_id, arm_label, treatment_spec_id) VALUES (?, 'treatment', ?)`, expID, specTreatID)
	winnerID64, _ := armRes.LastInsertId()
	winnerID := int(winnerID64)

	cellMeans := `{"control": 0.20, "treatment": 0.96}`
	if _, err := db.Exec(`
		INSERT INTO ExperimentOutcomes
			(experiment_id, termination_reason, winner_treatment_id, winner_posterior,
			 winner_effect_estimate, cell_means_json,
			 fleet_state_hash_at_start, fleet_state_hash_at_end, confirm_phase_outcome)
		VALUES (?, 'declared_winner', ?, 0.99, 0.76, ?, 'state-A', 'state-B', '')
	`, expID, winnerID, cellMeans); err != nil {
		t.Fatalf("seed outcome: %v", err)
	}
	return expID, winnerID
}

// TestHandlePromotionAuthor_HappyPath: terminated experiment with
// declared_winner produces a single PromotionProposals row with the
// full evidence summary; ratified_at remains '' (operator gate).
func TestHandlePromotionAuthor_HappyPath(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	expID, winnerID := seedTerminatedWinner(t, db, "exp-promote")

	cfg := EngineeringCorpsConfig{Name: "EC-test", DB: db}
	logger := newTestLogger()
	payload := fmt.Sprintf(`{"experiment_id":%d}`, expID)
	bid := store.AddBounty(db, 0, TaskTypePromotionAuthor, payload)
	bounty, _ := store.ClaimBounty(db, TaskTypePromotionAuthor, "EC-test")

	if err := handlePromotionAuthor(context.Background(), cfg, nil, "EC-test", bounty, logger.std()); err != nil {
		t.Fatalf("handler: %v", err)
	}

	// Exactly one open proposal authored.
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM PromotionProposals WHERE experiment_id=?`, expID).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Fatalf("expected 1 proposal, got %d", n)
	}

	var kind, authoredBy, evidenceJSON, ratifiedAt, ttl string
	if err := db.QueryRow(`
		SELECT kind, authored_by, IFNULL(evidence_summary_json,'{}'),
		       IFNULL(ratified_at,''), IFNULL(ttl_expires_at,'')
		FROM PromotionProposals WHERE experiment_id=?
	`, expID).Scan(&kind, &authoredBy, &evidenceJSON, &ratifiedAt, &ttl); err != nil {
		t.Fatalf("read proposal: %v", err)
	}
	if kind != "promote" {
		t.Errorf("kind = %q, want promote", kind)
	}
	if authoredBy != "engineering-corps" {
		t.Errorf("authored_by = %q, want engineering-corps", authoredBy)
	}
	if ratifiedAt != "" {
		t.Errorf("ratified_at must be empty (operator-routed); got %q", ratifiedAt)
	}
	if ttl == "" {
		t.Errorf("ttl_expires_at must be set (+14 days)")
	}

	// Evidence summary contains the load-bearing fields.
	var ev map[string]any
	if err := json.Unmarshal([]byte(evidenceJSON), &ev); err != nil {
		t.Fatalf("unmarshal evidence: %v", err)
	}
	for _, key := range []string{"experiment_id", "winner_treatment_id", "winner_posterior",
		"cell_means", "analysis_framework", "fleet_state_hash_at_start",
		"fleet_state_hash_at_end", "hypothesis_text", "stakes_tier"} {
		if _, ok := ev[key]; !ok {
			t.Errorf("evidence summary missing key %q (have %v)", key, keysOf(ev))
		}
	}
	if got := int(ev["winner_treatment_id"].(float64)); got != winnerID {
		t.Errorf("winner_treatment_id = %d, want %d", got, winnerID)
	}

	fresh, _ := store.GetBounty(db, bid)
	if fresh.Status != "Completed" {
		t.Errorf("bounty status = %q, want Completed", fresh.Status)
	}
}

// TestHandlePromotionAuthor_OperatorRoutingPreserved is the explicit
// "no auto-ratify" guard — even when the proposal is created the
// operator gate must be preserved.
func TestHandlePromotionAuthor_OperatorRoutingPreserved(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	expID, _ := seedTerminatedWinner(t, db, "exp-route")

	cfg := EngineeringCorpsConfig{Name: "EC-test", DB: db}
	logger := newTestLogger()
	payload := fmt.Sprintf(`{"experiment_id":%d}`, expID)
	store.AddBounty(db, 0, TaskTypePromotionAuthor, payload)
	bounty, _ := store.ClaimBounty(db, TaskTypePromotionAuthor, "EC-test")

	if err := handlePromotionAuthor(context.Background(), cfg, nil, "EC-test", bounty, logger.std()); err != nil {
		t.Fatalf("handler: %v", err)
	}

	var ratifiedCount, rejectedCount int
	db.QueryRow(`SELECT COUNT(*) FROM PromotionProposals WHERE IFNULL(ratified_at,'') != ''`).Scan(&ratifiedCount)
	db.QueryRow(`SELECT COUNT(*) FROM PromotionProposals WHERE IFNULL(rejected_at,'') != ''`).Scan(&rejectedCount)
	if ratifiedCount != 0 || rejectedCount != 0 {
		t.Errorf("operator gate broken: ratified=%d rejected=%d (both must be 0)", ratifiedCount, rejectedCount)
	}

	// Should also not have made any FleetRules edits.
	var ruleCount int
	db.QueryRow(`SELECT COUNT(*) FROM FleetRules WHERE created_by='engineering-corps'`).Scan(&ruleCount)
	if ruleCount != 0 {
		t.Errorf("PromotionAuthor must not write FleetRules directly; got %d rule rows", ruleCount)
	}
}

// TestHandlePromotionAuthor_Idempotent: re-running on the same
// experiment must not produce a duplicate open proposal.
func TestHandlePromotionAuthor_Idempotent(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	expID, _ := seedTerminatedWinner(t, db, "exp-idem")

	cfg := EngineeringCorpsConfig{Name: "EC-test", DB: db}
	logger := newTestLogger()

	for i := 0; i < 3; i++ {
		payload := fmt.Sprintf(`{"experiment_id":%d}`, expID)
		store.AddBounty(db, 0, TaskTypePromotionAuthor, payload)
		bounty, _ := store.ClaimBounty(db, TaskTypePromotionAuthor, "EC-test")
		if err := handlePromotionAuthor(context.Background(), cfg, nil, "EC-test", bounty, logger.std()); err != nil {
			t.Fatalf("handler #%d: %v", i, err)
		}
	}

	var n int
	db.QueryRow(`SELECT COUNT(*) FROM PromotionProposals WHERE experiment_id=?`, expID).Scan(&n)
	if n != 1 {
		t.Errorf("idempotence broken: got %d proposals, want 1", n)
	}
}

// TestHandlePromotionAuthor_NotTerminated: refuses to author for a
// still-running experiment.
func TestHandlePromotionAuthor_NotTerminated(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	res, _ := db.Exec(`
		INSERT INTO Experiments (name, status) VALUES ('exp-running', 'running')
	`)
	expID64, _ := res.LastInsertId()

	cfg := EngineeringCorpsConfig{Name: "EC-test", DB: db}
	logger := newTestLogger()
	payload := fmt.Sprintf(`{"experiment_id":%d}`, expID64)
	store.AddBounty(db, 0, TaskTypePromotionAuthor, payload)
	bounty, _ := store.ClaimBounty(db, TaskTypePromotionAuthor, "EC-test")

	err := handlePromotionAuthor(context.Background(), cfg, nil, "EC-test", bounty, logger.std())
	if err == nil {
		t.Fatalf("expected error for still-running experiment, got nil")
	}
	if !strings.Contains(err.Error(), "must be terminated") {
		t.Errorf("error should mention terminated; got %v", err)
	}
}

// TestHandlePromotionAuthor_PayloadParseError: malformed payload
// fails the bounty cleanly.
func TestHandlePromotionAuthor_PayloadParseError(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	cfg := EngineeringCorpsConfig{Name: "EC-test", DB: db}
	logger := newTestLogger()
	store.AddBounty(db, 0, TaskTypePromotionAuthor, `{"experiment_id":"oops"}`)
	bounty, _ := store.ClaimBounty(db, TaskTypePromotionAuthor, "EC-test")

	err := handlePromotionAuthor(context.Background(), cfg, nil, "EC-test", bounty, logger.std())
	if err == nil {
		t.Fatalf("expected parse error, got nil")
	}
}

// keysOf returns the keys of a string-keyed map for debug output.
func keysOf(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
