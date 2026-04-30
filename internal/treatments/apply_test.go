package treatments

import (
	"context"
	"testing"

	"force-orchestrator/internal/store"
)

func TestApply_LogOnlyPassThrough(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	ctx := context.Background()

	in := CallDescriptor{
		AgentName:       "captain",
		NaturalUnitKind: "task",
		NaturalUnitID:   42,
		PromptTemplate:  "captain/default@HEAD",
		Model:           "claude-opus-4-7",
		InHoldout:       false,
	}
	out, assignments, err := Apply(ctx, db, in)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if out != in {
		t.Errorf("Apply mutated CallDescriptor: got %+v, want %+v", out, in)
	}
	if len(assignments) != 0 {
		t.Errorf("log-only mode returned %d assignments; expected 0", len(assignments))
	}
}

func TestApply_LogRowWritten(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	ctx := context.Background()

	in := CallDescriptor{
		AgentName:       "council",
		NaturalUnitKind: "task",
		NaturalUnitID:   7,
		PromptTemplate:  "council/default@HEAD",
		Model:           "claude-sonnet-4-6",
		InHoldout:       true,
	}
	if _, _, err := Apply(ctx, db, in); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	var (
		count       int
		agent       string
		holdout     int
		mode        string
		assignments string
	)
	err := db.QueryRow(`
		SELECT COUNT(*), MAX(agent_name), MAX(in_holdout), MAX(mode), MAX(assignments_json)
		FROM TreatmentApplyLog
	`).Scan(&count, &agent, &holdout, &mode, &assignments)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if count != 1 {
		t.Errorf("TreatmentApplyLog row count: got %d, want 1", count)
	}
	if agent != "council" {
		t.Errorf("agent_name: got %q, want %q", agent, "council")
	}
	if holdout != 1 {
		t.Errorf("in_holdout: got %d, want 1", holdout)
	}
	if mode != ModeLogOnly {
		t.Errorf("mode: got %q, want %q", mode, ModeLogOnly)
	}
	if assignments != "[]" {
		t.Errorf("assignments_json: got %q, want %q (log-only emits empty slice)", assignments, "[]")
	}
}

func TestApply_NoSideEffectsOnExperimentRuns(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	ctx := context.Background()

	in := CallDescriptor{
		AgentName:       "medic",
		NaturalUnitKind: "task",
		NaturalUnitID:   1,
		PromptTemplate:  "medic/default@HEAD",
	}
	if _, _, err := Apply(ctx, db, in); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	// Live experiment runs are NOT touched by log-only mode.
	var runs int
	if err := db.QueryRow(`SELECT COUNT(*) FROM ExperimentRuns`).Scan(&runs); err != nil {
		t.Fatalf("scan ExperimentRuns: %v", err)
	}
	if runs != 0 {
		t.Errorf("log-only Apply created %d ExperimentRuns rows; expected 0", runs)
	}
}

func TestApply_NilDBNoPanic(t *testing.T) {
	// Pre-DB callers (very early daemon boot, tests using a CallDescriptor
	// builder) MUST be tolerated by Apply: nil db → return input
	// unmodified, no panic.
	in := CallDescriptor{AgentName: "test"}
	out, assignments, err := Apply(context.Background(), nil, in)
	if err != nil {
		t.Fatalf("Apply(nil db): %v", err)
	}
	if out != in {
		t.Errorf("Apply(nil db) mutated descriptor")
	}
	if len(assignments) != 0 {
		t.Errorf("Apply(nil db) returned %d assignments", len(assignments))
	}
}
