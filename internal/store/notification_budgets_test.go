package store

import (
	"context"
	"testing"
)

func TestRespectNotificationBudget_HighStakesPunchesThrough(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()
	ctx := context.Background()

	// Set an aggressive budget so non-high stakes would bounce.
	if err := SetNotificationBudget(ctx, db, "op@example.com", "captain", "email", 0, 60, true); err != nil {
		t.Fatalf("seed budget: %v", err)
	}

	// High-stakes — always allowed regardless of budget state.
	allowed, err := RespectNotificationBudget(ctx, db, "op@example.com", "captain", "email", `{"x":1}`, StakesHigh)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !allowed {
		t.Errorf("high-stakes was suppressed; want allowed=true")
	}
}

func TestRespectNotificationBudget_NoRowDefaultsOpen(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()
	ctx := context.Background()

	allowed, err := RespectNotificationBudget(ctx, db, "op@example.com", "captain", "email", `{}`, StakesMedium)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !allowed {
		t.Errorf("no budget row should default-open")
	}
}

func TestRespectNotificationBudget_BudgetExhaustedDigests(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()
	ctx := context.Background()

	// Budget: 0 per 60 min for the modal channel (digest enabled).
	if err := SetNotificationBudget(ctx, db, "op@example.com", "captain", "modal", 0, 60, true); err != nil {
		t.Fatalf("seed budget: %v", err)
	}

	// Medium-stakes — should land in digest.
	allowed, err := RespectNotificationBudget(ctx, db, "op@example.com", "captain", "modal", `{"msg":"x"}`, StakesMedium)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if allowed {
		t.Errorf("budget-exhausted medium-stakes should not be allowed")
	}

	// Verify the digest row exists.
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM OperatorNotificationDigest WHERE operator_email = 'op@example.com' AND source = 'captain' AND channel = 'modal'`).Scan(&n); err != nil {
		t.Fatalf("count digest: %v", err)
	}
	if n == 0 {
		t.Errorf("expected at least one digest row; got none")
	}
}

func TestRespectNotificationBudget_DigestDisabledDrops(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()
	ctx := context.Background()

	if err := SetNotificationBudget(ctx, db, "op@example.com", "investigator", "email", 0, 60, false); err != nil {
		t.Fatalf("seed budget: %v", err)
	}

	allowed, err := RespectNotificationBudget(ctx, db, "op@example.com", "investigator", "email", `{}`, StakesLow)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if allowed {
		t.Errorf("budget-exhausted low-stakes with digest=off should be dropped")
	}
}

func TestSetNotificationBudget_Idempotence(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()
	ctx := context.Background()

	// Insert.
	if err := SetNotificationBudget(ctx, db, "op@example.com", "captain", "email", 5, 60, true); err != nil {
		t.Fatalf("first set: %v", err)
	}
	// Re-insert with new values — should upsert.
	if err := SetNotificationBudget(ctx, db, "op@example.com", "captain", "email", 10, 30, false); err != nil {
		t.Fatalf("second set: %v", err)
	}
	budgets, err := ListNotificationBudgets(ctx, db, "op@example.com")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(budgets) != 1 {
		t.Fatalf("expected 1 row after upsert; got %d", len(budgets))
	}
	got := budgets[0]
	if got.MaxPerPeriod != 10 || got.PeriodMinutes != 30 || got.DigestRemainder {
		t.Errorf("upsert did not overwrite: %+v", got)
	}
}

func TestFlushPendingDigests(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()
	ctx := context.Background()

	// Force a budget-exhausted emit so a digest row lands.
	if err := SetNotificationBudget(ctx, db, "op@example.com", "captain", "modal", 0, 60, true); err != nil {
		t.Fatalf("seed budget: %v", err)
	}
	if _, err := RespectNotificationBudget(ctx, db, "op@example.com", "captain", "modal", `{"k":1}`, StakesMedium); err != nil {
		t.Fatalf("emit: %v", err)
	}

	n, err := FlushPendingDigests(ctx, db, "op@example.com", "9999-12-31")
	if err != nil {
		t.Fatalf("flush: %v", err)
	}
	if n == 0 {
		t.Errorf("flushed 0 rows; expected at least one")
	}
	// Idempotence: re-flushing flushes nothing more.
	n2, _ := FlushPendingDigests(ctx, db, "op@example.com", "9999-12-31")
	if n2 != 0 {
		t.Errorf("second flush touched %d rows; want 0 (already flushed)", n2)
	}
}
