package shadow

import (
	"context"
	"errors"
	"testing"
)

// TestShadowSession_FieldsCompile is a smoke test that pins the
// ShadowSession field set so downstream packages (adversarial,
// golden_set, agent call sites) can rely on these names while sub-agent
// A is filling in the real implementation. If A renames a field, this
// test fails fast and forces an explicit downstream update.
func TestShadowSession_FieldsCompile(t *testing.T) {
	s := &ShadowSession{
		ExperimentID:    7,
		RunID:           42,
		AgentName:       "astromech-1",
		WorktreePath:    "/tmp/shadow",
		GhRecordingPath: "/tmp/shadow/gh.jsonl",
	}
	if s.ExperimentID != 7 || s.RunID != 42 || s.AgentName != "astromech-1" {
		t.Fatalf("ShadowSession field round-trip broken: %+v", s)
	}
	if s.WorktreePath == "" || s.GhRecordingPath == "" {
		t.Fatalf("ShadowSession path fields not preserved: %+v", s)
	}
}

func TestSetupShadowWorktree_Stub_ReturnsErrShadowNotConfigured(t *testing.T) {
	// Before sub-agent A lands, SetupShadowWorktree returns the
	// not-configured sentinel. This test pins the contract so callers
	// can errors.Is against it.
	_, err := SetupShadowWorktree(context.Background(), nil, 1)
	if !errors.Is(err, ErrShadowNotConfigured) {
		t.Fatalf("SetupShadowWorktree stub: want ErrShadowNotConfigured, got %v", err)
	}
}

func TestCleanupShadowWorktree_Stub_Idempotent(t *testing.T) {
	if err := CleanupShadowWorktree(context.Background(), nil, nil); err != nil {
		t.Fatalf("CleanupShadowWorktree stub: want nil, got %v", err)
	}
	// Second call must also be a clean no-op.
	if err := CleanupShadowWorktree(context.Background(), nil, nil); err != nil {
		t.Fatalf("CleanupShadowWorktree stub second call: want nil, got %v", err)
	}
}
