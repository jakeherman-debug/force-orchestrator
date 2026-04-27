package rules_test

import (
	"context"
	"errors"
	"testing"

	"force-orchestrator/internal/clients/rules"
)

func TestInProcess_StubReturnsErrNotImplemented(t *testing.T) {
	c := rules.NewInProcess()
	ctx := context.Background()

	if _, err := c.ActiveRules(ctx, "captain", "scope"); !errors.Is(err, rules.ErrNotImplemented) {
		t.Errorf("ActiveRules: expected ErrNotImplemented, got %v", err)
	}
	if _, err := c.RuleByKey(ctx, "key"); !errors.Is(err, rules.ErrNotImplemented) {
		t.Errorf("RuleByKey: expected ErrNotImplemented, got %v", err)
	}
	if _, err := c.PromoteFromExperiment(ctx, 1, rules.PromotionRequest{}); !errors.Is(err, rules.ErrNotImplemented) {
		t.Errorf("PromoteFromExperiment: expected ErrNotImplemented, got %v", err)
	}
	if err := c.Retire(ctx, "key", "reason"); !errors.Is(err, rules.ErrNotImplemented) {
		t.Errorf("Retire: expected ErrNotImplemented, got %v", err)
	}
}

func TestMock_PromoteAndActiveRules(t *testing.T) {
	m := rules.NewMock()
	r, err := m.PromoteFromExperiment(context.Background(), 42, rules.PromotionRequest{
		Key: "captain-scope-800loc", Agent: "captain", Category: "scope-cap",
		Body: "reject PRs > 800 LoC",
	})
	if err != nil {
		t.Fatalf("Promote: %v", err)
	}
	if r.PromotedFrom != 42 {
		t.Errorf("PromotedFrom = %d, want 42", r.PromotedFrom)
	}
	active, err := m.ActiveRules(context.Background(), "captain", "scope-cap")
	if err != nil {
		t.Fatalf("ActiveRules: %v", err)
	}
	if len(active) != 1 || active[0].Key != "captain-scope-800loc" {
		t.Errorf("ActiveRules unexpected: %+v", active)
	}
}

func TestMock_RetireRemovesFromActive(t *testing.T) {
	m := rules.NewMock()
	_, _ = m.PromoteFromExperiment(context.Background(), 1, rules.PromotionRequest{
		Key: "k", Agent: "a", Category: "c", Body: "b",
	})
	if err := m.Retire(context.Background(), "k", "test"); err != nil {
		t.Fatalf("Retire: %v", err)
	}
	active, _ := m.ActiveRules(context.Background(), "a", "c")
	if len(active) != 0 {
		t.Errorf("retired rule still appears in ActiveRules: %+v", active)
	}
}
