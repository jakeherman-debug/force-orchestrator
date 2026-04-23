package agents

// Fix #6 — Medic-requeue cap tests.
//
// These tests verify the bounded Astromech→Council→Medic→Astromech loop:
//
//   - applyMedicRequeue increments medic_requeue_count and honors the LLM's
//     requeue decision while count < maxMedicRequeues.
//   - Once count >= maxMedicRequeues, the next requeue attempt is FORCED to
//     escalate regardless of the LLM's decision — the task reaches a
//     terminal state in bounded cycles.
//   - The counter is per-task, so distinct tasks each get a fresh budget.
//   - End-to-end: looping "requeue" Medic calls against the same task
//     converges to an escalation in ≤ maxMedicRequeues+1 calls.

import (
	"database/sql"
	"io"
	"log"
	"testing"

	"force-orchestrator/internal/store"
)

// seedMedicTestTask inserts a parent CodeEdit task + its MedicReview bounty
// and returns both IDs. Keeps each integration test focused on the Medic
// control-flow rather than the DB fixture setup.
func seedMedicTestTask(t *testing.T, db *sql.DB) (parentID, medicID int) {
	t.Helper()
	pid, err := store.AddConvoyTask(db, 0, "test", "do the thing", 0, 0, "Failed")
	if err != nil {
		t.Fatalf("AddConvoyTask: %v", err)
	}
	mid := store.QueueMedicReview(db,
		&store.Bounty{ID: pid, TargetRepo: "test", ConvoyID: 0, Priority: 0},
		"test", "triage this")
	return pid, mid
}

// TestApplyMedicRequeue_CapFiresAt2 — integration test.
// Two honored requeues followed by a third attempt that is forced to
// escalate. Proves the cap bounds the loop at maxMedicRequeues.
func TestApplyMedicRequeue_CapFiresAt2(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	parentID, medicID := seedMedicTestTask(t, db)
	parent, _ := store.GetBounty(db, parentID)
	medic, _ := store.GetBounty(db, medicID)
	logger := log.New(io.Discard, "", 0)

	// Cycles 1 and 2 — both honored. Task returns to Pending, counter
	// increments, no escalation is created.
	for i := 1; i <= maxMedicRequeues; i++ {
		applyMedicRequeue(db, "medic", medic, parent, medicDecision{
			Decision: "requeue", Reason: "try again", Guidance: "do X",
		}, logger)

		got := store.GetMedicRequeueCount(db, parentID)
		if got != i {
			t.Fatalf("cycle %d: medic_requeue_count=%d, want %d", i, got, i)
		}
		b, _ := store.GetBounty(db, parentID)
		if b.Status != "Pending" {
			t.Fatalf("cycle %d: parent status=%q, want Pending", i, b.Status)
		}
		if escalations := countEscalations(t, db, parentID, "Open"); escalations != 0 {
			t.Fatalf("cycle %d: unexpected %d Open escalation(s)", i, escalations)
		}
	}

	// Cycle 3 — cap hit. Requeue must be refused; task escalates instead.
	// The counter must NOT have advanced: the cap is enforced by
	// short-circuiting to applyMedicEscalate BEFORE the increment, so a
	// third honored requeue cannot have happened.
	applyMedicRequeue(db, "medic", medic, parent, medicDecision{
		Decision: "requeue", Reason: "and again", Guidance: "do X differently",
	}, logger)

	if got := store.GetMedicRequeueCount(db, parentID); got != maxMedicRequeues {
		t.Fatalf("after cap hit: medic_requeue_count=%d, want %d (no further increment)", got, maxMedicRequeues)
	}
	if escalations := countEscalations(t, db, parentID, "Open"); escalations != 1 {
		t.Fatalf("after cap hit: want 1 Open escalation, got %d", escalations)
	}
}

// TestApplyMedicRequeue_CapIsPerTask — integration test.
// Task A's counter does not leak into Task B: each distinct task has its
// own medic_requeue_count budget.
func TestApplyMedicRequeue_CapIsPerTask(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	parentA, medicA := seedMedicTestTask(t, db)
	parentB, medicB := seedMedicTestTask(t, db)
	pa, _ := store.GetBounty(db, parentA)
	pb, _ := store.GetBounty(db, parentB)
	ma, _ := store.GetBounty(db, medicA)
	mb, _ := store.GetBounty(db, medicB)
	logger := log.New(io.Discard, "", 0)

	// Drive task A to the cap.
	for i := 0; i < maxMedicRequeues; i++ {
		applyMedicRequeue(db, "medic", ma, pa, medicDecision{
			Decision: "requeue", Reason: "A", Guidance: "g",
		}, logger)
	}
	if got := store.GetMedicRequeueCount(db, parentA); got != maxMedicRequeues {
		t.Fatalf("task A: counter=%d, want %d", got, maxMedicRequeues)
	}

	// Task B's counter must still be zero — the cap is per-task.
	if got := store.GetMedicRequeueCount(db, parentB); got != 0 {
		t.Fatalf("task B: counter leaked = %d, want 0", got)
	}

	// Requeuing task B once should still be honored.
	applyMedicRequeue(db, "medic", mb, pb, medicDecision{
		Decision: "requeue", Reason: "B", Guidance: "g",
	}, logger)
	if got := store.GetMedicRequeueCount(db, parentB); got != 1 {
		t.Fatalf("task B: first requeue counter=%d, want 1", got)
	}
	b, _ := store.GetBounty(db, parentB)
	if b.Status != "Pending" {
		t.Fatalf("task B: first requeue status=%q, want Pending", b.Status)
	}
	if escalations := countEscalations(t, db, parentB, "Open"); escalations != 0 {
		t.Fatalf("task B: first requeue — unexpected %d escalation(s)", escalations)
	}
}

// TestApplyMedicRequeue_AdversarialLLM — e2e test covering the full
// Astromech→Council→Medic→Astromech loop terminating at the cap regardless
// of what the Medic LLM returns. We simulate a stuck LLM that always wants
// "requeue" and verify the control-flow converges to an escalation in a
// bounded number of cycles.
func TestApplyMedicRequeue_AdversarialLLM(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	parentID, medicID := seedMedicTestTask(t, db)
	parent, _ := store.GetBounty(db, parentID)
	medic, _ := store.GetBounty(db, medicID)
	logger := log.New(io.Discard, "", 0)

	// Adversarial decision: every single cycle, the LLM wants another
	// requeue. A correctly-capped system must still escalate in bounded
	// time.
	adversarial := medicDecision{
		Decision: "requeue",
		Reason:   "just one more try",
		Guidance: "try harder",
	}

	// Run the loop for 3x the cap to prove it doesn't walk past the cap
	// even under repeated re-invocation.
	const trials = maxMedicRequeues * 3
	for i := 0; i < trials; i++ {
		applyMedicRequeue(db, "medic", medic, parent, adversarial, logger)
	}

	// Exactly maxMedicRequeues honored requeues — the rest are refused.
	if got := store.GetMedicRequeueCount(db, parentID); got != maxMedicRequeues {
		t.Fatalf("adversarial loop: medic_requeue_count=%d, want %d (cap)", got, maxMedicRequeues)
	}

	// Every call past the cap tries to post an escalation. Fix #3's partial
	// UNIQUE idx_escalations_open_task(task_id) WHERE status='Open' collapses
	// repeated Open rows for the same task into a single upserted row — so
	// `trials - maxMedicRequeues` attempted inserts yield exactly 1 Open row.
	// The severity CASE in CreateEscalation ensures the merged row rides the
	// max observed severity (which stays MEDIUM in this scenario).
	if got := countEscalations(t, db, parentID, "Open"); got != 1 {
		t.Fatalf("adversarial loop: Open escalation count=%d, want 1 (Fix #3 partial UNIQUE collapses repeats)", got)
	}
}

// countEscalations is a test helper that counts Escalations rows for a task
// in a given status. Local to this file so we don't pollute the package
// surface.
func countEscalations(t *testing.T, db *sql.DB, taskID int, status string) int {
	t.Helper()
	var n int
	err := db.QueryRow(`SELECT COUNT(*) FROM Escalations WHERE task_id = ? AND status = ?`, taskID, status).Scan(&n)
	if err != nil {
		t.Fatalf("count escalations: %v", err)
	}
	return n
}
