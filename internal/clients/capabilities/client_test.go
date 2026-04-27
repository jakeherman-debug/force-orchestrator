package capabilities_test

import (
	"context"
	"errors"
	"testing"

	"force-orchestrator/internal/clients/capabilities"
)

// TestInProcess_StubReturnsErrNotImplemented pins the D0 contract:
// every method on the placeholder backing must return
// ErrNotImplemented so callers see a real error instead of the
// no-treatment fall-through. D1 replaces these bodies with real ones.
func TestInProcess_StubReturnsErrNotImplemented(t *testing.T) {
	c := capabilities.NewInProcess()
	ctx := context.Background()

	if _, err := c.LoadProfile(ctx, "Astromech-1"); !errors.Is(err, capabilities.ErrNotImplemented) {
		t.Errorf("LoadProfile: expected ErrNotImplemented, got %v", err)
	}
	if _, err := c.AllowedTools(ctx, "Astromech-1"); !errors.Is(err, capabilities.ErrNotImplemented) {
		t.Errorf("AllowedTools: expected ErrNotImplemented, got %v", err)
	}
	if _, err := c.DisallowedTools(ctx, "Astromech-1"); !errors.Is(err, capabilities.ErrNotImplemented) {
		t.Errorf("DisallowedTools: expected ErrNotImplemented, got %v", err)
	}
	if _, err := c.MCPConfigPath(ctx, "Astromech-1"); !errors.Is(err, capabilities.ErrNotImplemented) {
		t.Errorf("MCPConfigPath: expected ErrNotImplemented, got %v", err)
	}
}

func TestMock_DefaultLookup(t *testing.T) {
	m := capabilities.NewMock()
	m.Profiles["Yoda"] = &capabilities.Profile{
		AgentName:    "Yoda",
		AllowedTools: []string{"Read", "Edit"},
	}
	got, err := m.LoadProfile(context.Background(), "Yoda")
	if err != nil {
		t.Fatalf("LoadProfile: %v", err)
	}
	if got.AgentName != "Yoda" || len(got.AllowedTools) != 2 {
		t.Errorf("unexpected profile: %+v", got)
	}
	if _, err := m.LoadProfile(context.Background(), "Unknown"); !errors.Is(err, capabilities.ErrProfileNotFound) {
		t.Errorf("expected ErrProfileNotFound for unknown, got %v", err)
	}
}
