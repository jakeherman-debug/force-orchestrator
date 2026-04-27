package experiments_test

import (
	"context"
	"errors"
	"testing"

	"force-orchestrator/internal/clients/experiments"
)

func TestInProcess_StubReturnsErrNotImplemented(t *testing.T) {
	c := experiments.NewInProcess()
	ctx := context.Background()

	call := experiments.CallDescriptor{Kind: "claude", Subject: "captain-review"}
	if _, _, err := c.Apply(ctx, call); !errors.Is(err, experiments.ErrNotImplemented) {
		t.Errorf("Apply: expected ErrNotImplemented, got %v", err)
	}
	if _, err := c.Outcome(ctx, 1); !errors.Is(err, experiments.ErrNotImplemented) {
		t.Errorf("Outcome: expected ErrNotImplemented, got %v", err)
	}
	if _, err := c.Register(ctx, experiments.ExperimentDecl{Key: "foo"}); !errors.Is(err, experiments.ErrNotImplemented) {
		t.Errorf("Register: expected ErrNotImplemented, got %v", err)
	}
	if err := c.Cancel(ctx, 1, "test"); !errors.Is(err, experiments.ErrNotImplemented) {
		t.Errorf("Cancel: expected ErrNotImplemented, got %v", err)
	}
}

func TestMock_PassThroughApply(t *testing.T) {
	m := experiments.NewMock()
	call := experiments.CallDescriptor{Kind: "claude", Subject: "test"}
	got, assigns, err := m.Apply(context.Background(), call)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if got.Kind != "claude" || got.Subject != "test" {
		t.Errorf("Apply rewrote call unexpectedly: %+v", got)
	}
	if len(assigns) != 0 {
		t.Errorf("default Apply should produce zero assignments, got %d", len(assigns))
	}
}

func TestMock_RegisterIDIncrements(t *testing.T) {
	m := experiments.NewMock()
	id1, _ := m.Register(context.Background(), experiments.ExperimentDecl{Key: "a"})
	id2, _ := m.Register(context.Background(), experiments.ExperimentDecl{Key: "b"})
	if id1 != 1 || id2 != 2 {
		t.Errorf("expected sequential IDs 1,2; got %d,%d", id1, id2)
	}
}
