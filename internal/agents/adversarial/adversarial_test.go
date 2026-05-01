package adversarial

import (
	"context"
	"errors"
	"testing"
)

// TestPrimaryDecision_FieldsCompile pins the PrimaryDecision shape so
// sub-agent B's implementation can change behavior but not field names
// without breaking call sites in jedi_council / medic / convoy_review.
func TestPrimaryDecision_FieldsCompile(t *testing.T) {
	d := PrimaryDecision{
		DecisionID:    99,
		Agent:         AgentCouncil,
		Outcome:       `{"approved":true}`,
		Reasoning:     "diff is small and well-scoped",
		PromptVersion: "council-v3",
	}
	if d.DecisionID != 99 || d.Agent != AgentCouncil {
		t.Fatalf("PrimaryDecision round-trip broken: %+v", d)
	}
	if d.PromptVersion == "" {
		t.Fatalf("PromptVersion must be load-bearing — empty value defeats the anti-cheat axis")
	}
}

// TestPair_FieldsCompile pins the Pair shape so audit / replay code can
// SELECT * INTO Pair{...} without fearing rename churn.
func TestPair_FieldsCompile(t *testing.T) {
	p := Pair{
		ID:                   1,
		DecisionID:           42,
		Agent:                AgentMedic,
		PrimaryOutcome:       `{"decision":"requeue"}`,
		CriticOutcome:        `{"decision":"escalate"}`,
		PromptVersionPrimary: "medic-v2",
		PromptVersionCritic:  "medic-critic-v1",
		Agreement:            false,
	}
	if p.PromptVersionPrimary == p.PromptVersionCritic {
		t.Fatalf("Pair anti-cheat invariant: prompt versions must differ when populated")
	}
}

func TestRunAdversarialPair_Stub_ReturnsErrIdenticalPromptVersions(t *testing.T) {
	// Skeleton stub: critic prompt version absent → fails closed.
	_, err := RunAdversarialPair(context.Background(), nil, PrimaryDecision{
		DecisionID:    1,
		Agent:         AgentCouncil,
		Outcome:       `{"approved":true}`,
		PromptVersion: "council-v3",
	})
	if !errors.Is(err, ErrIdenticalPromptVersions) {
		t.Fatalf("RunAdversarialPair stub: want ErrIdenticalPromptVersions, got %v", err)
	}
}

func TestSurfaceDisagreementToOperator_Stub_NoOp(t *testing.T) {
	if err := SurfaceDisagreementToOperator(context.Background(), nil, 1); err != nil {
		t.Fatalf("SurfaceDisagreementToOperator stub: want nil, got %v", err)
	}
}

func TestAgent_Constants(t *testing.T) {
	// Anchor the three agent values matching AdversarialPairings.agent
	// CHECK-style enumeration in the schema's documented value set.
	if string(AgentCouncil) != "council" || string(AgentMedic) != "medic" || string(AgentConvoyReview) != "convoy_review" {
		t.Fatalf("Agent constants drift from schema-documented values: council=%q medic=%q convoy_review=%q",
			AgentCouncil, AgentMedic, AgentConvoyReview)
	}
}
