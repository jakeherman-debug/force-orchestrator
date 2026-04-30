package engineering_corps

import (
	"context"
	"strings"
	"testing"

	"force-orchestrator/internal/store"
)

// TestHandleHoldoutMonitor_HappyPath_NoActiveHoldouts seeds an empty
// GlobalHoldouts table, claims a HoldoutMonitor bounty, and asserts
// the handler completes the bounty (heartbeat succeeds even when no
// holdouts exist).
func TestHandleHoldoutMonitor_HappyPath_NoActiveHoldouts(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	cfg := EngineeringCorpsConfig{Name: "EC-test", DB: db}
	logger := newTestLogger()

	id := store.AddBounty(db, 0, TaskTypeHoldoutMonitor, "{}")
	bounty, claimed := store.ClaimBounty(db, TaskTypeHoldoutMonitor, "EC-test")
	if !claimed || bounty == nil || bounty.ID != id {
		t.Fatalf("ClaimBounty failed: bounty=%v claimed=%v", bounty, claimed)
	}

	err := handleHoldoutMonitor(context.Background(), cfg, nil, "EC-test", bounty, logger.std())
	if err != nil {
		t.Fatalf("handleHoldoutMonitor: %v", err)
	}

	fresh, gerr := store.GetBounty(db, id)
	if gerr != nil {
		t.Fatalf("GetBounty: %v", gerr)
	}
	if fresh.Status != "Completed" {
		t.Errorf("status = %q, want Completed", fresh.Status)
	}
	if !strings.Contains(logger.dump(), "no model deprecation detected") {
		t.Errorf("expected debug heartbeat in log; got %q", logger.dump())
	}
}

// TestHandleHoldoutMonitor_HappyPath_WithActiveHoldout seeds one
// active holdout and asserts the handler counts it correctly.
func TestHandleHoldoutMonitor_HappyPath_WithActiveHoldout(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	if _, err := db.Exec(`
		INSERT INTO GlobalHoldouts (name, plateau_fraction, retired_at)
		VALUES ('baseline-2026', 0.02, '')
	`); err != nil {
		t.Fatalf("seed holdout: %v", err)
	}

	cfg := EngineeringCorpsConfig{Name: "EC-test", DB: db}
	logger := newTestLogger()

	id := store.AddBounty(db, 0, TaskTypeHoldoutMonitor, "{}")
	bounty, _ := store.ClaimBounty(db, TaskTypeHoldoutMonitor, "EC-test")

	err := handleHoldoutMonitor(context.Background(), cfg, nil, "EC-test", bounty, logger.std())
	if err != nil {
		t.Fatalf("handleHoldoutMonitor: %v", err)
	}

	fresh, _ := store.GetBounty(db, id)
	if fresh.Status != "Completed" {
		t.Errorf("status = %q, want Completed", fresh.Status)
	}
	if !strings.Contains(logger.dump(), "1 active holdout") {
		t.Errorf("expected count=1 in log; got %q", logger.dump())
	}
}

// TestHandleHoldoutMonitor_OperatorRoutingPreserved asserts the
// handler does NOT auto-substitute models or otherwise mutate
// holdout state — full availability watch lives in P5/P6 and the
// minimal P3 surface should not pre-empt that.
func TestHandleHoldoutMonitor_OperatorRoutingPreserved(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	if _, err := db.Exec(`
		INSERT INTO GlobalHoldouts (name, plateau_fraction)
		VALUES ('baseline-2026', 0.02)
	`); err != nil {
		t.Fatalf("seed holdout: %v", err)
	}
	if _, err := db.Exec(`
		INSERT INTO ModelAvailability (model_id, last_checked_at)
		VALUES ('claude-opus-test', '')
	`); err != nil {
		t.Fatalf("seed model: %v", err)
	}

	cfg := EngineeringCorpsConfig{Name: "EC-test", DB: db}
	logger := newTestLogger()

	id := store.AddBounty(db, 0, TaskTypeHoldoutMonitor, "{}")
	bounty, _ := store.ClaimBounty(db, TaskTypeHoldoutMonitor, "EC-test")

	if err := handleHoldoutMonitor(context.Background(), cfg, nil, "EC-test", bounty, logger.std()); err != nil {
		t.Fatalf("handleHoldoutMonitor: %v", err)
	}

	// No deprecation rows must have been written by this handler.
	var depCount int
	if err := db.QueryRow(`SELECT COUNT(*) FROM ModelAvailability WHERE IFNULL(deprecation_detected_at,'') != ''`).Scan(&depCount); err != nil {
		t.Fatalf("count deprecations: %v", err)
	}
	if depCount != 0 {
		t.Errorf("HoldoutMonitor must not auto-substitute / record deprecation in P3 (operator-routed in P5/P6); got %d deprecation rows", depCount)
	}

	// No PromotionProposals or operator-routed mail rows from this path either.
	var ppCount int
	if err := db.QueryRow(`SELECT COUNT(*) FROM PromotionProposals`).Scan(&ppCount); err != nil {
		t.Fatalf("count proposals: %v", err)
	}
	if ppCount != 0 {
		t.Errorf("HoldoutMonitor must not author PromotionProposals in P3; got %d", ppCount)
	}

	// Heartbeat completed.
	fresh, _ := store.GetBounty(db, id)
	if fresh.Status != "Completed" {
		t.Errorf("status = %q, want Completed", fresh.Status)
	}
}
