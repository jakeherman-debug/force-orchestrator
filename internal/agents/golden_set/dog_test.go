package golden_set

import (
	"context"
	"testing"

	"force-orchestrator/internal/store"
)

type stubGate struct {
	estop bool
	spend bool
}

func (s stubGate) IsEstopped() bool       { return s.estop }
func (s stubGate) SpendCapExceeded() bool { return s.spend }

func TestRunWeeklyEvaluatorDog_HappyPath(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	seedFixtures(t, db, "council", []Fixture{
		{Input: `{"x":1}`, ExpectedOutput: `{"approved":true}`, Source: SourceAutoCleanShipping},
	})
	seedFixtures(t, db, "medic", []Fixture{
		{Input: `{"err":"x"}`, ExpectedOutput: `{"decision":"requeue"}`, Source: SourceOperatorCurated},
	})

	out, err := RunWeeklyEvaluatorDog(context.Background(), db,
		EvaluatorByAgent{
			"council": echoExpected(),
			"medic":   echoExpected(),
		},
		PromptVersionByAgent{
			"council": "council-v3",
			"medic":   "medic-v2",
		},
		stubGate{},
	)
	if err != nil {
		t.Fatalf("dog: %v", err)
	}
	if out["council"] != 1 || out["medic"] != 1 {
		t.Fatalf("dog: per-agent counts wrong: %+v", out)
	}
}

func TestRunWeeklyEvaluatorDog_HonorsEstop(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	seedFixtures(t, db, "council", []Fixture{
		{Input: `x`, ExpectedOutput: `y`, Source: SourceAutoCleanShipping},
	})
	_, err := RunWeeklyEvaluatorDog(context.Background(), db,
		EvaluatorByAgent{"council": echoExpected()},
		PromptVersionByAgent{"council": "council-v3"},
		stubGate{estop: true},
	)
	if err == nil {
		t.Fatalf("estop active: want error")
	}
	var n int
	db.QueryRow(`SELECT COUNT(*) FROM GoldenSetEvaluations`).Scan(&n)
	if n != 0 {
		t.Fatalf("estop active: dog must not write evaluations; got %d", n)
	}
}

func TestRunWeeklyEvaluatorDog_HonorsSpendCap(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	seedFixtures(t, db, "council", []Fixture{
		{Input: `x`, ExpectedOutput: `y`, Source: SourceAutoCleanShipping},
	})
	_, err := RunWeeklyEvaluatorDog(context.Background(), db,
		EvaluatorByAgent{"council": echoExpected()},
		PromptVersionByAgent{"council": "council-v3"},
		stubGate{spend: true},
	)
	if err == nil {
		t.Fatalf("spend cap exceeded: want error")
	}
}

func TestRunWeeklyEvaluatorDog_OneAgentFailureDoesNotHaltOthers(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	// Only "medic" has fixtures; "council" returns ErrNoFixtures
	// during the cycle. The dog should record 0 for council and 1 for medic.
	seedFixtures(t, db, "medic", []Fixture{
		{Input: `x`, ExpectedOutput: `y`, Source: SourceOperatorCurated},
	})
	out, err := RunWeeklyEvaluatorDog(context.Background(), db,
		EvaluatorByAgent{
			"council": echoExpected(),
			"medic":   echoExpected(),
		},
		PromptVersionByAgent{
			"council": "council-v3",
			"medic":   "medic-v2",
		},
		stubGate{},
	)
	if err != nil {
		t.Fatalf("partial-failure dog: %v", err)
	}
	if out["council"] != 0 {
		t.Fatalf("council had no fixtures; want 0, got %d", out["council"])
	}
	if out["medic"] != 1 {
		t.Fatalf("medic should still evaluate; got %d", out["medic"])
	}
}

func TestRunWeeklyEvaluatorDog_SkipsAgentsWithEmptyVersion(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	seedFixtures(t, db, "council", []Fixture{
		{Input: `x`, ExpectedOutput: `y`, Source: SourceAutoCleanShipping},
	})
	out, err := RunWeeklyEvaluatorDog(context.Background(), db,
		EvaluatorByAgent{"council": echoExpected()},
		PromptVersionByAgent{}, // no version
		stubGate{},
	)
	if err != nil {
		t.Fatalf("dog: %v", err)
	}
	if _, ok := out["council"]; ok {
		t.Fatalf("agent with empty prompt-version must be skipped")
	}
}
