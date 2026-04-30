package engineering_corps

import (
	"context"
	"database/sql"
	"fmt"
	"testing"

	"force-orchestrator/internal/store"
)

// seedRatifiedPromotion seeds a PromotionProposals row for the given
// experiment_id with kind='promote' and ratified_at set `daysAgo`
// days ago. Returns the proposal's id.
func seedRatifiedPromotion(t *testing.T, db *sql.DB, expID int, ruleKey string, daysAgo int) int {
	t.Helper()
	res, err := db.Exec(`
		INSERT INTO PromotionProposals
			(experiment_id, kind, rule_key, proposed_content, evidence_summary_json,
			 authored_by, authored_at, ratified_at, ttl_expires_at)
		VALUES (?, 'promote', ?, 'rule body', '{}', 'engineering-corps',
		        datetime('now', '-' || ? || ' days'),
		        datetime('now', '-' || ? || ' days'),
		        datetime('now', '+14 days'))
	`, expID, ruleKey, daysAgo+1, daysAgo)
	if err != nil {
		t.Fatalf("seed promotion: %v", err)
	}
	id64, _ := res.LastInsertId()
	return int(id64)
}

// TestHandleDemotionAuthor_HappyPath_StaleProposal: a 60-day-old
// ratified promotion produces one demotion proposal.
func TestHandleDemotionAuthor_HappyPath_StaleProposal(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	// Seed an experiment row so FK semantics are at least implicit.
	res, _ := db.Exec(`INSERT INTO Experiments (name, status) VALUES ('exp1', 'terminated')`)
	expID64, _ := res.LastInsertId()
	expID := int(expID64)

	pid := seedRatifiedPromotion(t, db, expID, "captain-flag-stale-1", 60)
	_ = pid

	cfg := EngineeringCorpsConfig{Name: "EC-test", DB: db}
	logger := newTestLogger()
	bid := store.AddBounty(db, 0, TaskTypeDemotionAuthor, `{}`)
	bounty, _ := store.ClaimBounty(db, TaskTypeDemotionAuthor, "EC-test")

	if err := handleDemotionAuthor(context.Background(), cfg, nil, "EC-test", bounty, logger.std()); err != nil {
		t.Fatalf("handler: %v", err)
	}

	var demoCount int
	db.QueryRow(`SELECT COUNT(*) FROM PromotionProposals WHERE kind='demote'`).Scan(&demoCount)
	if demoCount != 1 {
		t.Errorf("expected 1 demotion, got %d", demoCount)
	}

	var kind, authoredBy, ratifiedAt string
	db.QueryRow(`SELECT kind, authored_by, IFNULL(ratified_at,'') FROM PromotionProposals WHERE kind='demote'`).Scan(&kind, &authoredBy, &ratifiedAt)
	if kind != "demote" {
		t.Errorf("kind = %q, want demote", kind)
	}
	if authoredBy != "engineering-corps" {
		t.Errorf("authored_by = %q, want engineering-corps", authoredBy)
	}
	if ratifiedAt != "" {
		t.Errorf("ratified_at must be empty (operator-routed); got %q", ratifiedAt)
	}

	fresh, _ := store.GetBounty(db, bid)
	if fresh.Status != "Completed" {
		t.Errorf("bounty status = %q, want Completed", fresh.Status)
	}
}

// TestHandleDemotionAuthor_FreshProposalSkipped: a 5-day-old
// promotion does NOT trigger a demotion (within retention window).
func TestHandleDemotionAuthor_FreshProposalSkipped(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	res, _ := db.Exec(`INSERT INTO Experiments (name, status) VALUES ('exp1', 'terminated')`)
	expID64, _ := res.LastInsertId()
	seedRatifiedPromotion(t, db, int(expID64), "fresh-rule", 5)

	cfg := EngineeringCorpsConfig{Name: "EC-test", DB: db}
	logger := newTestLogger()
	store.AddBounty(db, 0, TaskTypeDemotionAuthor, `{}`)
	bounty, _ := store.ClaimBounty(db, TaskTypeDemotionAuthor, "EC-test")

	if err := handleDemotionAuthor(context.Background(), cfg, nil, "EC-test", bounty, logger.std()); err != nil {
		t.Fatalf("handler: %v", err)
	}

	var demoCount int
	db.QueryRow(`SELECT COUNT(*) FROM PromotionProposals WHERE kind='demote'`).Scan(&demoCount)
	if demoCount != 0 {
		t.Errorf("expected 0 demotions for a 5-day-old promotion; got %d", demoCount)
	}
}

// TestHandleDemotionAuthor_OperatorRoutingPreserved: written
// demotion is unratified and unrejected.
func TestHandleDemotionAuthor_OperatorRoutingPreserved(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	res, _ := db.Exec(`INSERT INTO Experiments (name, status) VALUES ('exp1', 'terminated')`)
	expID64, _ := res.LastInsertId()
	seedRatifiedPromotion(t, db, int(expID64), "rule-route", 60)

	cfg := EngineeringCorpsConfig{Name: "EC-test", DB: db}
	logger := newTestLogger()
	store.AddBounty(db, 0, TaskTypeDemotionAuthor, `{}`)
	bounty, _ := store.ClaimBounty(db, TaskTypeDemotionAuthor, "EC-test")

	_ = handleDemotionAuthor(context.Background(), cfg, nil, "EC-test", bounty, logger.std())

	var ratifiedCount, rejectedCount int
	db.QueryRow(`SELECT COUNT(*) FROM PromotionProposals WHERE kind='demote' AND IFNULL(ratified_at,'') != ''`).Scan(&ratifiedCount)
	db.QueryRow(`SELECT COUNT(*) FROM PromotionProposals WHERE kind='demote' AND IFNULL(rejected_at,'') != ''`).Scan(&rejectedCount)
	if ratifiedCount != 0 || rejectedCount != 0 {
		t.Errorf("operator gate broken: ratified=%d rejected=%d", ratifiedCount, rejectedCount)
	}
}

// TestHandleDemotionAuthor_Idempotent: re-running over the same
// stale window does not produce duplicates.
func TestHandleDemotionAuthor_Idempotent(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	res, _ := db.Exec(`INSERT INTO Experiments (name, status) VALUES ('exp1', 'terminated')`)
	expID64, _ := res.LastInsertId()
	seedRatifiedPromotion(t, db, int(expID64), "rule-idem", 60)

	cfg := EngineeringCorpsConfig{Name: "EC-test", DB: db}
	logger := newTestLogger()

	for i := 0; i < 3; i++ {
		store.AddBounty(db, 0, TaskTypeDemotionAuthor, `{}`)
		bounty, _ := store.ClaimBounty(db, TaskTypeDemotionAuthor, "EC-test")
		if err := handleDemotionAuthor(context.Background(), cfg, nil, "EC-test", bounty, logger.std()); err != nil {
			t.Fatalf("handler #%d: %v", i, err)
		}
	}

	var demoCount int
	db.QueryRow(`SELECT COUNT(*) FROM PromotionProposals WHERE kind='demote'`).Scan(&demoCount)
	if demoCount != 1 {
		t.Errorf("idempotence broken: got %d demotions, want 1", demoCount)
	}
}

// TestHandleDemotionAuthor_PayloadOverridesStaleDays: payload may
// supply a custom stale_days override.
func TestHandleDemotionAuthor_PayloadOverridesStaleDays(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	res, _ := db.Exec(`INSERT INTO Experiments (name, status) VALUES ('exp1', 'terminated')`)
	expID64, _ := res.LastInsertId()
	seedRatifiedPromotion(t, db, int(expID64), "rule-custom", 10)

	cfg := EngineeringCorpsConfig{Name: "EC-test", DB: db}
	logger := newTestLogger()
	// Lower the threshold so a 10-day-old promotion qualifies.
	store.AddBounty(db, 0, TaskTypeDemotionAuthor, fmt.Sprintf(`{"stale_days":%d}`, 7))
	bounty, _ := store.ClaimBounty(db, TaskTypeDemotionAuthor, "EC-test")

	if err := handleDemotionAuthor(context.Background(), cfg, nil, "EC-test", bounty, logger.std()); err != nil {
		t.Fatalf("handler: %v", err)
	}

	var demoCount int
	db.QueryRow(`SELECT COUNT(*) FROM PromotionProposals WHERE kind='demote'`).Scan(&demoCount)
	if demoCount != 1 {
		t.Errorf("custom stale_days=7 should have triggered demotion; got %d", demoCount)
	}
}
