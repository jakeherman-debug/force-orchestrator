package agents

import (
	"context"
	"errors"
	"testing"

	"force-orchestrator/internal/store"
)

func TestCounterProposalRouter_WholeThing(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	ctx := context.Background()

	br, _ := RenderBriefing(ctx, db, "captain_proposal", 7, 70)

	// Too short.
	_, err := RouteCounterProposal(ctx, db, br.ID, "captain_proposal", CounterProposalWholeThing, "no")
	if !errors.Is(err, ErrWholeThingTextTooShort) {
		t.Errorf("err=%v, want ErrWholeThingTextTooShort", err)
	}

	// Valid.
	newID, err := RouteCounterProposal(ctx, db, br.ID, "captain_proposal", CounterProposalWholeThing,
		"this proposal conflicts with AT-008")
	if err != nil {
		t.Fatalf("route: %v", err)
	}
	if newID != 0 {
		t.Errorf("whole_thing should not spawn a task; got newID=%d", newID)
	}

	// BriefingRenders row updated.
	br2, _ := RenderBriefing(ctx, db, "captain_proposal", 7, 70)
	if br2.CounterProposalKind != string(CounterProposalWholeThing) {
		t.Errorf("counter_kind=%q, want whole_thing", br2.CounterProposalKind)
	}
	if br2.OperatorDecision != "rejected" {
		t.Errorf("operator_decision=%q, want rejected", br2.OperatorDecision)
	}
}

func TestCounterProposalRouter_DifferentApproach(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	ctx := context.Background()

	br, _ := RenderBriefing(ctx, db, "captain_proposal", 8, 70)

	// Too short.
	_, err := RouteCounterProposal(ctx, db, br.ID, "captain_proposal", CounterProposalDifferentApproach, "short draft")
	if !errors.Is(err, ErrDifferentApproachTooShort) {
		t.Errorf("err=%v, want ErrDifferentApproachTooShort", err)
	}

	// Valid: 60-char draft.
	draft := "Use a sliding window rate-limiter on the rule_key endpoint instead of a fixed cap."
	newID, err := RouteCounterProposal(ctx, db, br.ID, "captain_proposal", CounterProposalDifferentApproach, draft)
	if err != nil {
		t.Fatalf("route: %v", err)
	}
	if newID == 0 {
		t.Errorf("different_approach should spawn a task; newID=0")
	}

	var n int
	_ = db.QueryRow(`SELECT COUNT(*) FROM BountyBoard WHERE id = ?`, newID).Scan(&n)
	if n != 1 {
		t.Errorf("counter-proposal task not created (id=%d)", newID)
	}
}

func TestCounterProposalRouter_Defer(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	ctx := context.Background()

	br, _ := RenderBriefing(ctx, db, "captain_proposal", 9, 70)

	// Defer accepts empty text.
	newID, err := RouteCounterProposal(ctx, db, br.ID, "captain_proposal", CounterProposalDefer, "")
	if err != nil {
		t.Fatalf("route defer: %v", err)
	}
	if newID == 0 {
		t.Errorf("defer should spawn an Investigator task; newID=0")
	}
}

func TestCounterProposalRouter_UnknownKind(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	ctx := context.Background()
	_, err := RouteCounterProposal(ctx, db, 0, "captain_proposal", CounterProposalKind("hopeful"), "")
	if !errors.Is(err, ErrCounterKindUnknown) {
		t.Errorf("err=%v, want ErrCounterKindUnknown", err)
	}
}
