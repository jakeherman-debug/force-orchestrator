package adversarial

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"force-orchestrator/internal/store"
)

func newTestDB(t *testing.T) *sql.DB {
	t.Helper()
	return store.InitHolocronDSN(":memory:")
}

func stubCritic(outcome, version string) CriticFn {
	return func(_ context.Context, _ PrimaryDecision) (CriticOutcome, error) {
		return CriticOutcome{Outcome: outcome, PromptVersion: version}, nil
	}
}

func TestAdversarialPair_AgreementNoSurface(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()

	primary := PrimaryDecision{
		DecisionID:    42,
		Agent:         AgentCouncil,
		Outcome:       `{"approved":true}`,
		Reasoning:    "small diff, good test coverage",
		PromptVersion: "council-v3",
	}
	pair, err := RunAdversarialPairWith(context.Background(), db, primary,
		stubCritic(`{"approved":true}`, "council-critic-v1"))
	if err != nil {
		t.Fatalf("RunAdversarialPairWith: %v", err)
	}
	if !pair.Agreement {
		t.Fatalf("primary and critic both approved — must agree")
	}
	// Surface should be a no-op for agreements.
	if err := SurfaceDisagreementToOperator(context.Background(), db, pair.ID); err != nil {
		t.Fatalf("SurfaceDisagreementToOperator on agreement: %v", err)
	}
	// Confirm no fleet_mail row written.
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM Fleet_Mail WHERE from_agent='adversarial-pairing'`).Scan(&n); err != nil {
		t.Fatalf("count mail: %v", err)
	}
	if n != 0 {
		t.Fatalf("agreement must not write Fleet_Mail; got %d rows", n)
	}
	// Confirm surfaced_at NOT set.
	var surfaced string
	db.QueryRow(`SELECT IFNULL(surfaced_at,'') FROM AdversarialPairings WHERE id=?`, pair.ID).Scan(&surfaced)
	if surfaced != "" {
		t.Fatalf("agreement must not set surfaced_at; got %q", surfaced)
	}
}

func TestAdversarialPair_DisagreementSurfaces(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()

	primary := PrimaryDecision{
		DecisionID:    42,
		Agent:         AgentCouncil,
		Outcome:       `{"approved":true}`,
		Reasoning:    "small diff",
		PromptVersion: "council-v3",
	}
	pair, err := RunAdversarialPairWith(context.Background(), db, primary,
		stubCritic(`{"approved":false,"feedback":"missing test for the boundary case"}`, "council-critic-v1"))
	if err != nil {
		t.Fatalf("RunAdversarialPairWith: %v", err)
	}
	if pair.Agreement {
		t.Fatalf("primary approved but critic rejected — must disagree")
	}

	if err := SurfaceDisagreementToOperator(context.Background(), db, pair.ID); err != nil {
		t.Fatalf("SurfaceDisagreementToOperator: %v", err)
	}

	// Confirm Fleet_Mail row written.
	var n int
	db.QueryRow(`SELECT COUNT(*) FROM Fleet_Mail WHERE from_agent='adversarial-pairing'`).Scan(&n)
	if n != 1 {
		t.Fatalf("disagreement must write 1 Fleet_Mail; got %d rows", n)
	}

	// Confirm surfaced_at populated.
	var surfaced string
	db.QueryRow(`SELECT IFNULL(surfaced_at,'') FROM AdversarialPairings WHERE id=?`, pair.ID).Scan(&surfaced)
	if surfaced == "" {
		t.Fatalf("disagreement must set surfaced_at")
	}
}

func TestAdversarialPair_StoresBothOutcomes(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()

	primary := PrimaryDecision{
		DecisionID:    7,
		Agent:         AgentMedic,
		Outcome:       `{"decision":"requeue"}`,
		PromptVersion: "medic-v2",
	}
	pair, err := RunAdversarialPairWith(context.Background(), db, primary,
		stubCritic(`{"decision":"escalate"}`, "medic-critic-v1"))
	if err != nil {
		t.Fatalf("RunAdversarialPairWith: %v", err)
	}

	loaded, err := LoadPair(context.Background(), db, pair.ID)
	if err != nil {
		t.Fatalf("LoadPair: %v", err)
	}
	if loaded.PrimaryOutcome != `{"decision":"requeue"}` {
		t.Fatalf("PrimaryOutcome not preserved: %q", loaded.PrimaryOutcome)
	}
	if loaded.CriticOutcome != `{"decision":"escalate"}` {
		t.Fatalf("CriticOutcome not preserved: %q", loaded.CriticOutcome)
	}
	if loaded.Agent != AgentMedic {
		t.Fatalf("Agent not preserved: %q", loaded.Agent)
	}
	if loaded.Agreement {
		t.Fatalf("requeue vs escalate must be disagreement")
	}
}

func TestAdversarialPair_PersistsPromptVersions(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()

	primary := PrimaryDecision{
		DecisionID:    7,
		Agent:         AgentConvoyReview,
		Outcome:       `{"finding":"missing test","fix_task":"add test"}`,
		PromptVersion: "convoy-review-v4",
	}
	pair, err := RunAdversarialPairWith(context.Background(), db, primary,
		stubCritic(`{"finding":"missing test","fix_task":"add test"}`, "convoy-review-critic-v1"))
	if err != nil {
		t.Fatalf("RunAdversarialPairWith: %v", err)
	}

	var pp, pc string
	err = db.QueryRow(`SELECT prompt_version_primary, prompt_version_critic FROM AdversarialPairings WHERE id=?`,
		pair.ID).Scan(&pp, &pc)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if pp != "convoy-review-v4" {
		t.Fatalf("prompt_version_primary not persisted: %q", pp)
	}
	if pc != "convoy-review-critic-v1" {
		t.Fatalf("prompt_version_critic not persisted: %q", pc)
	}
}

func TestAdversarialPair_RejectsIdenticalPromptVersions(t *testing.T) {
	// Anti-cheat: critic prompt version equals primary's → fail closed.
	db := newTestDB(t)
	defer db.Close()

	_, err := RunAdversarialPairWith(context.Background(), db,
		PrimaryDecision{
			DecisionID:    1,
			Agent:         AgentCouncil,
			Outcome:       `{"approved":true}`,
			PromptVersion: "council-v3",
		},
		stubCritic(`{"approved":true}`, "council-v3"), // same version
	)
	if !errors.Is(err, ErrIdenticalPromptVersions) {
		t.Fatalf("want ErrIdenticalPromptVersions, got %v", err)
	}
}

func TestAdversarialPair_RejectsEmptyCriticPromptVersion(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()

	_, err := RunAdversarialPairWith(context.Background(), db,
		PrimaryDecision{
			DecisionID:    1,
			Agent:         AgentCouncil,
			Outcome:       `{"approved":true}`,
			PromptVersion: "council-v3",
		},
		stubCritic(`{"approved":true}`, ""), // empty critic version
	)
	if !errors.Is(err, ErrIdenticalPromptVersions) {
		t.Fatalf("empty critic prompt version: want ErrIdenticalPromptVersions, got %v", err)
	}
}

func TestAdversarialPair_RejectsEmptyPrimaryPromptVersion(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()

	_, err := RunAdversarialPairWith(context.Background(), db,
		PrimaryDecision{
			DecisionID:    1,
			Agent:         AgentCouncil,
			Outcome:       `{"approved":true}`,
			PromptVersion: "", // empty primary version
		},
		stubCritic(`{"approved":true}`, "council-critic-v1"),
	)
	if !errors.Is(err, ErrIdenticalPromptVersions) {
		t.Fatalf("empty primary prompt version: want ErrIdenticalPromptVersions, got %v", err)
	}
}

func TestAdversarialPair_SurfaceIdempotent(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()

	pair, err := RunAdversarialPairWith(context.Background(), db,
		PrimaryDecision{
			DecisionID:    42,
			Agent:         AgentCouncil,
			Outcome:       `{"approved":true}`,
			PromptVersion: "council-v3",
		},
		stubCritic(`{"approved":false}`, "council-critic-v1"))
	if err != nil {
		t.Fatalf("RunAdversarialPairWith: %v", err)
	}

	for i := 0; i < 3; i++ {
		if err := SurfaceDisagreementToOperator(context.Background(), db, pair.ID); err != nil {
			t.Fatalf("Surface call #%d: %v", i+1, err)
		}
	}
	var n int
	db.QueryRow(`SELECT COUNT(*) FROM Fleet_Mail WHERE from_agent='adversarial-pairing'`).Scan(&n)
	if n != 1 {
		t.Fatalf("idempotent surface must write exactly 1 mail; got %d", n)
	}
}

func TestAdversarialPair_RejectsMissingDB(t *testing.T) {
	_, err := RunAdversarialPairWith(context.Background(), nil,
		PrimaryDecision{Agent: AgentCouncil, PromptVersion: "v1"},
		stubCritic("x", "v2"))
	if err == nil {
		t.Fatalf("nil db: want error, got nil")
	}
}

func TestAdversarialPair_RejectsNilCritic(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()

	_, err := RunAdversarialPairWith(context.Background(), db,
		PrimaryDecision{Agent: AgentCouncil, PromptVersion: "v1"}, nil)
	if err == nil {
		t.Fatalf("nil critic: want error, got nil")
	}
}

func TestAdversarialPair_RegisterCriticDispatch(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()

	called := 0
	RegisterCritic(AgentCouncil, func(ctx context.Context, p PrimaryDecision) (CriticOutcome, error) {
		called++
		return CriticOutcome{Outcome: `{"approved":false}`, PromptVersion: "stub-critic"}, nil
	})
	defer func() { delete(wiredCritics, AgentCouncil) }()

	pair, err := RunAdversarialPair(context.Background(), db, PrimaryDecision{
		DecisionID:    1,
		Agent:         AgentCouncil,
		Outcome:       `{"approved":true}`,
		PromptVersion: "council-v3",
	})
	if err != nil {
		t.Fatalf("RunAdversarialPair via RegisterCritic: %v", err)
	}
	if pair == nil || called != 1 {
		t.Fatalf("RegisterCritic dispatch broken; called=%d pair=%v", called, pair)
	}
}

func TestAdversarialPair_OutcomesAgree(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{`{"x":1}`, `{"x":1}`, true},
		{`{"x":1}`, `{ "x" : 1 }`, true}, // whitespace tolerant
		{`{"x":1}`, `{"x":2}`, false},
		{"", "", true},
	}
	for _, c := range cases {
		if got := outcomesAgree(c.a, c.b); got != c.want {
			t.Errorf("outcomesAgree(%q,%q) = %v want %v", c.a, c.b, got, c.want)
		}
	}
}
