package agents

import (
	"context"
	"strings"
	"testing"

	"force-orchestrator/internal/store"
)

func TestCooldownScheduler_Lifecycle(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	ctx := context.Background()

	id, err := ScheduleCooldown(ctx, db, "council_approve", 47)
	if err != nil {
		t.Fatalf("schedule: %v", err)
	}
	pending, _ := ListPendingCooldowns(ctx, db)
	if len(pending) != 1 || pending[0].ID != id {
		t.Fatalf("pending mismatch: %+v", pending)
	}

	// Pause.
	if err := PauseCooldown(ctx, db, id, "op@example.com"); err != nil {
		t.Fatalf("pause: %v", err)
	}
	// Resume requires rationale.
	if err := ResumeCooldown(ctx, db, id, "no"); err == nil {
		t.Errorf("resume with short rationale was accepted")
	}
	if err := ResumeCooldown(ctx, db, id, "verified diff is clean"); err != nil {
		t.Fatalf("resume: %v", err)
	}

	// Cancel.
	id2, _ := ScheduleCooldown(ctx, db, "medic_auto_fix", 99)
	if err := CancelCooldown(ctx, db, id2); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	pending2, _ := ListPendingCooldowns(ctx, db)
	for _, p := range pending2 {
		if p.ID == id2 {
			t.Errorf("cancelled cooldown still pending: %+v", p)
		}
	}

	// MarkExecuted.
	if err := MarkCooldownExecuted(ctx, db, id); err != nil {
		t.Fatalf("mark executed: %v", err)
	}
}

func TestCooldownScheduler_PauseIdempotent(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	ctx := context.Background()

	id, _ := ScheduleCooldown(ctx, db, "council_approve", 47)
	if err := PauseCooldown(ctx, db, id, "op@example.com"); err != nil {
		t.Fatalf("first pause: %v", err)
	}
	// Second pause is a no-op (paused_at != '' filter rejects it).
	if err := PauseCooldown(ctx, db, id, "op@example.com"); err != nil {
		t.Fatalf("second pause: %v", err)
	}
}

func TestCooldownScheduler_ResumeRequiresRationale(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	ctx := context.Background()

	id, _ := ScheduleCooldown(ctx, db, "council_approve", 47)
	_ = PauseCooldown(ctx, db, id, "op@example.com")

	err := ResumeCooldown(ctx, db, id, "")
	if err == nil || !strings.Contains(err.Error(), "rationale") {
		t.Errorf("expected rationale-required error, got %v", err)
	}
}
