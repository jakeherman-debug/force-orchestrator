package graph_test

import (
	"context"
	"errors"
	"testing"

	"force-orchestrator/internal/clients/graph"
)

func TestInProcess_StubReturnsErrNotImplemented(t *testing.T) {
	c := graph.NewInProcess()
	ctx := context.Background()

	sym := graph.Symbol{Repo: "force", Name: "auth.Login", Kind: "func"}
	if _, err := c.Consumers(ctx, sym); !errors.Is(err, graph.ErrNotImplemented) {
		t.Errorf("Consumers: expected ErrNotImplemented, got %v", err)
	}
	if _, err := c.Definers(ctx, sym); !errors.Is(err, graph.ErrNotImplemented) {
		t.Errorf("Definers: expected ErrNotImplemented, got %v", err)
	}
	if _, err := c.BlastRadius(ctx, sym); !errors.Is(err, graph.ErrNotImplemented) {
		t.Errorf("BlastRadius: expected ErrNotImplemented, got %v", err)
	}
	if _, err := c.IndexHealth(ctx); !errors.Is(err, graph.ErrNotImplemented) {
		t.Errorf("IndexHealth: expected ErrNotImplemented, got %v", err)
	}
}

func TestMock_FixturedConsumers(t *testing.T) {
	m := graph.NewMock()
	target := graph.Symbol{Name: "auth.Login"}
	caller := graph.Symbol{Name: "handlers.LoginHandler"}
	m.ConsumersByName["auth.Login"] = []graph.Consumer{{Symbol: caller, Via: "direct"}}

	got, err := m.Consumers(context.Background(), target)
	if err != nil {
		t.Fatalf("Consumers: %v", err)
	}
	if len(got) != 1 || got[0].Symbol.Name != "handlers.LoginHandler" {
		t.Errorf("Consumers unexpected: %+v", got)
	}

	if _, err := m.Consumers(context.Background(), graph.Symbol{Name: "absent"}); !errors.Is(err, graph.ErrSymbolNotFound) {
		t.Errorf("expected ErrSymbolNotFound on miss, got %v", err)
	}
}
