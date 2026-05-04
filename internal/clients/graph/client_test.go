package graph_test

import (
	"context"
	"errors"
	"testing"

	"force-orchestrator/internal/clients/graph"
)

// TestInProcess_NilDBReturnsErrIndexNotReady documents the fail-safe
// behaviour of NewInProcess(nil): every read returns ErrIndexNotReady so
// daemon-startup paths that haven't opened the DB yet fail explicitly
// rather than panicking. (D8 T2 replaces the D0-era ErrNotImplemented
// guards now that the in-process backing has a real implementation.)
func TestInProcess_NilDBReturnsErrIndexNotReady(t *testing.T) {
	c := graph.NewInProcess(nil)
	ctx := context.Background()

	sym := graph.Symbol{Repo: "force", Name: "auth.Login", Kind: "func"}
	if _, err := c.Consumers(ctx, sym); !errors.Is(err, graph.ErrIndexNotReady) {
		t.Errorf("Consumers: expected ErrIndexNotReady on nil-db client, got %v", err)
	}
	if _, err := c.Definers(ctx, sym); !errors.Is(err, graph.ErrIndexNotReady) {
		t.Errorf("Definers: expected ErrIndexNotReady on nil-db client, got %v", err)
	}
	if _, err := c.BlastRadius(ctx, sym); !errors.Is(err, graph.ErrIndexNotReady) {
		t.Errorf("BlastRadius: expected ErrIndexNotReady on nil-db client, got %v", err)
	}
	if _, err := c.BlastRadiusForModifications(ctx, []graph.SymbolModification{
		{Repo: "force", FilePath: "auth/login.go", SymbolPath: "auth.Login"},
	}); !errors.Is(err, graph.ErrIndexNotReady) {
		t.Errorf("BlastRadiusForModifications: expected ErrIndexNotReady on nil-db client, got %v", err)
	}
	if _, err := c.IndexHealth(ctx); !errors.Is(err, graph.ErrIndexNotReady) {
		t.Errorf("IndexHealth: expected ErrIndexNotReady on nil-db client, got %v", err)
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
