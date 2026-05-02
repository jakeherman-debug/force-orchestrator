package stagegate

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"force-orchestrator/internal/store"
)

func TestOperatorConfirm_Type(t *testing.T) {
	g := OperatorConfirm{}
	if g.Type() != "operator_confirm" {
		t.Errorf("Type() = %q, want operator_confirm", g.Type())
	}
}

func TestOperatorConfirm_NoConfirm_Pending(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	g := OperatorConfirm{}
	stage := StageContext{
		ConvoyID:   42,
		StageNum:   1,
		GateConfig: json.RawMessage(`{"prompt":"deploy looks healthy?"}`),
	}
	passed, reason, err := g.Evaluate(context.Background(), db, stage)
	if !errors.Is(err, ErrPending) {
		t.Fatalf("expected ErrPending, got %v", err)
	}
	if passed {
		t.Error("expected passed=false while awaiting operator")
	}
	if !strings.Contains(reason, "operator confirm") {
		t.Errorf("expected reason to mention operator confirm, got %q", reason)
	}
	if !strings.Contains(reason, "deploy looks healthy") {
		t.Errorf("expected reason to include prompt, got %q", reason)
	}
}

func TestOperatorConfirm_NoConfirm_NoPrompt_Pending(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	g := OperatorConfirm{}
	// Empty config → no prompt; gate still pending until operator click.
	stage := StageContext{
		ConvoyID:   1,
		StageNum:   1,
		GateConfig: json.RawMessage(`{}`),
	}
	_, reason, err := g.Evaluate(context.Background(), db, stage)
	if !errors.Is(err, ErrPending) {
		t.Fatalf("expected ErrPending, got %v", err)
	}
	if !strings.Contains(reason, "awaiting operator confirm") {
		t.Errorf("got reason %q", reason)
	}
}

func TestOperatorConfirm_Confirmed_Passed(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	// Operator click writes this key — simulate with SetConfig.
	store.SetConfig(db, "stage_advance_42_1", "jake.herman:2026-05-01T12:00:00Z")

	g := OperatorConfirm{}
	stage := StageContext{
		ConvoyID:   42,
		StageNum:   1,
		GateConfig: json.RawMessage(`{"prompt":"all clear?"}`),
	}
	passed, reason, err := g.Evaluate(context.Background(), db, stage)
	if err != nil {
		t.Fatalf("expected nil err, got %v", err)
	}
	if !passed {
		t.Error("expected passed=true after operator confirm")
	}
	if !strings.Contains(reason, "jake.herman") {
		t.Errorf("expected reason to include the operator value, got %q", reason)
	}
}

func TestOperatorConfirm_KeyScopedPerStage(t *testing.T) {
	// Confirm for convoy=42 stage=1 must NOT pass convoy=42 stage=2 —
	// each stage holds its own SystemConfig key.
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	store.SetConfig(db, "stage_advance_42_1", "operator-x")

	g := OperatorConfirm{}
	other := StageContext{
		ConvoyID:   42,
		StageNum:   2,
		GateConfig: json.RawMessage(`{}`),
	}
	_, _, err := g.Evaluate(context.Background(), db, other)
	if !errors.Is(err, ErrPending) {
		t.Errorf("expected stage 2 to remain pending; got err=%v", err)
	}
}
