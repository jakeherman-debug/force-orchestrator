package agents

import (
	"context"
	"log"
	"os"
	"strings"
	"testing"

	"force-orchestrator/internal/store"
)

// TestEmitCaptainProposal_ApproveWritesPayload — happy path: an approve
// ruling produces a structurally-valid proposed_action_json with action
// "approve" and confidence > 0.5.
func TestEmitCaptainProposal_ApproveWritesPayload(t *testing.T) {
	t.Setenv("LIVE_HAIKU_DISABLED", "1")
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	taskID := store.AddBounty(db, 0, "CodeEdit", "demo task")
	b := &store.Bounty{ID: taskID, ConvoyID: 0, TargetRepo: "demo"}
	ruling := store.CaptainRuling{Decision: "approve", Feedback: ""}
	logger := log.New(os.Stderr, "test ", log.LstdFlags)

	emitCaptainProposal(context.Background(), db, "captain-test", b, ruling, logger)

	got, ok, err := store.GetProposedAction(db, taskID)
	if err != nil {
		t.Fatalf("GetProposedAction: %v", err)
	}
	if !ok {
		t.Fatal("expected proposal payload, got empty")
	}
	if got.Action != "approve" {
		t.Errorf("expected action=approve, got %q", got.Action)
	}
	if got.ClassificationConfidence < 0.5 {
		t.Errorf("expected confidence >= 0.5 for approve, got %f", got.ClassificationConfidence)
	}
	// P23: cited arrays must be present (not nil).
	if got.CitedATs == nil {
		t.Error("expected non-nil CitedATs (P23)")
	}
	if got.CitedFleetRules == nil {
		t.Error("expected non-nil CitedFleetRules (P23)")
	}
}

// TestEmitCaptainProposal_RejectWritesPayload — reject ruling carries
// the feedback as rationale.
func TestEmitCaptainProposal_RejectWritesPayload(t *testing.T) {
	t.Setenv("LIVE_HAIKU_DISABLED", "1")
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	taskID := store.AddBounty(db, 0, "CodeEdit", "demo task")
	b := &store.Bounty{ID: taskID, ConvoyID: 0, TargetRepo: "demo"}
	ruling := store.CaptainRuling{
		Decision: "reject",
		Feedback: "diff touches files outside scope",
	}
	logger := log.New(os.Stderr, "test ", log.LstdFlags)

	emitCaptainProposal(context.Background(), db, "captain-test", b, ruling, logger)

	got, _, _ := store.GetProposedAction(db, taskID)
	if got.Action != "reject" {
		t.Errorf("expected action=reject, got %q", got.Action)
	}
	if !strings.Contains(got.Rationale, "diff touches files outside scope") {
		t.Errorf("expected feedback in rationale, got %q", got.Rationale)
	}
}

// TestEmitCaptainProposal_EscalateWritesPayload — escalate ruling.
func TestEmitCaptainProposal_EscalateWritesPayload(t *testing.T) {
	t.Setenv("LIVE_HAIKU_DISABLED", "1")
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	taskID := store.AddBounty(db, 0, "CodeEdit", "demo task")
	b := &store.Bounty{ID: taskID, ConvoyID: 0, TargetRepo: "demo"}
	ruling := store.CaptainRuling{Decision: "escalate", Feedback: "convoy plan diverged"}
	logger := log.New(os.Stderr, "test ", log.LstdFlags)

	emitCaptainProposal(context.Background(), db, "captain-test", b, ruling, logger)

	got, _, _ := store.GetProposedAction(db, taskID)
	if got.Action != "escalate" {
		t.Errorf("expected action=escalate, got %q", got.Action)
	}
}

// TestEmitCaptainProposal_AuditTrailLogged — verifies the
// captain-proposal-emit + captain-proposal-judge audit rows land.
func TestEmitCaptainProposal_AuditTrailLogged(t *testing.T) {
	t.Setenv("LIVE_HAIKU_DISABLED", "1")
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	taskID := store.AddBounty(db, 0, "CodeEdit", "demo task")
	b := &store.Bounty{ID: taskID, ConvoyID: 0, TargetRepo: "demo"}
	ruling := store.CaptainRuling{Decision: "approve"}
	logger := log.New(os.Stderr, "test ", log.LstdFlags)

	emitCaptainProposal(context.Background(), db, "captain-test", b, ruling, logger)

	rows, err := db.Query(`SELECT action FROM AuditLog WHERE task_id = ? ORDER BY id`, taskID)
	if err != nil {
		t.Fatalf("AuditLog query: %v", err)
	}
	defer rows.Close()
	var actions []string
	for rows.Next() {
		var a string
		if err := rows.Scan(&a); err != nil {
			t.Fatal(err)
		}
		actions = append(actions, a)
	}
	hasEmit, hasJudge := false, false
	for _, a := range actions {
		if a == "captain-proposal-emit" {
			hasEmit = true
		}
		if a == "captain-proposal-judge" {
			hasJudge = true
		}
	}
	if !hasEmit {
		t.Errorf("expected captain-proposal-emit audit row, got %v", actions)
	}
	if !hasJudge {
		t.Errorf("expected captain-proposal-judge audit row, got %v", actions)
	}
}

// TestMapDecisionToProposedAction — verb mapping is total.
func TestMapDecisionToProposedAction(t *testing.T) {
	cases := map[string]string{
		"approve":  "approve",
		"reject":   "reject",
		"escalate": "escalate",
		"fix":      "fix",
		"":         "escalate",
		"yeet":     "escalate",
	}
	for in, want := range cases {
		if got := mapDecisionToProposedAction(in); got != want {
			t.Errorf("mapDecisionToProposedAction(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestCaptainConfidenceFromDecision_RangeAlwaysValid — the helper
// always returns a value in [0.0, 1.0] so SetProposedAction's mechanical
// validator never rejects it.
func TestCaptainConfidenceFromDecision_RangeAlwaysValid(t *testing.T) {
	for _, d := range []string{"approve", "reject", "escalate", "fix", "", "weird"} {
		c := captainConfidenceFromDecision(d)
		if c < 0.0 || c > 1.0 {
			t.Errorf("captainConfidenceFromDecision(%q) = %f, out of range", d, c)
		}
	}
}
