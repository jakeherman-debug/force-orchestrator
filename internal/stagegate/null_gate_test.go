package stagegate

import (
	"context"
	"testing"
)

func TestNullGate_Type(t *testing.T) {
	g := NullGate{}
	if g.Type() != "null" {
		t.Errorf("Type() = %q, want null", g.Type())
	}
}

func TestNullGate_AlwaysPasses(t *testing.T) {
	g := NullGate{}
	passed, reason, err := g.Evaluate(context.Background(), nil, StageContext{})
	if err != nil {
		t.Fatalf("null gate must never error, got %v", err)
	}
	if !passed {
		t.Error("null gate must always pass")
	}
	if reason == "" {
		t.Error("null gate should provide a reason for the audit trail")
	}
}
