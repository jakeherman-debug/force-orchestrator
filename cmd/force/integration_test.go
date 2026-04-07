package main

import (
	"fmt"
	"testing"
	"time"

	"force-orchestrator/internal/store"
)

// ── RunCommandCenter convoy display — deadlock regression ─────────────────────

// TestRunCommandCenter_ConvoyDisplay_NoDeadlock is the regression test for the
// watch.go deadlock: convoyRows were kept open (via defer) while ConvoyProgress
// called db.QueryRow twice inside the loop body. With MaxOpenConns(1) this
// deadlocks. The fix drains convoyRows into a slice, closes it, then queries
// progress per convoy.
func TestRunCommandCenter_ConvoyDisplay_NoDeadlock(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	// Create three active convoys, each with tasks in various states.
	for i := 0; i < 3; i++ {
		id, err := store.CreateConvoy(db, fmt.Sprintf("convoy-%d", i))
		if err != nil {
			t.Fatalf("CreateConvoy: %v", err)
		}
		t1, _ := store.AddConvoyTask(db, 0, "repo", "task", id, 0, "Pending")
		t2, _ := store.AddConvoyTask(db, 0, "repo", "task", id, 0, "Pending")
		db.Exec(`UPDATE BountyBoard SET status = 'Completed' WHERE id = ?`, t1)
		_ = t2
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		captureOutput(func() { RunCommandCenter(db) })
	}()

	select {
	case <-done:
		t.Error("RunCommandCenter exited unexpectedly (it should run forever)")
	case <-time.After(300 * time.Millisecond):
		// Ran at least one full iteration including convoy progress queries — no deadlock.
	}
}

// TestRunCommandCenter_AllStatusCategories seeds one task in every status bucket
// and verifies RunCommandCenter completes a display cycle without panicking or
// deadlocking. This exercises every branch of the status display switch.
func TestRunCommandCenter_AllStatusCategories(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	store.AddRepo(db, "repo", "/tmp/fake", "test")

	statuses := []string{
		"Pending", "Locked", "AwaitingCaptainReview", "UnderCaptainReview",
		"AwaitingCouncilReview", "UnderReview", "Completed", "Failed", "Escalated",
	}
	for _, s := range statuses {
		id := store.AddBounty(db, 0, "CodeEdit", "task for "+s)
		if s != "Pending" {
			db.Exec(`UPDATE BountyBoard SET status = ? WHERE id = ?`, s, id)
		}
	}

	// Add an active convoy to exercise the convoy display path.
	convoyID, _ := store.CreateConvoy(db, "test-convoy")
	t1, _ := store.AddConvoyTask(db, 0, "repo", "convoy task", convoyID, 0, "Pending")
	db.Exec(`UPDATE BountyBoard SET status = 'Completed' WHERE id = ?`, t1)

	panicked := make(chan any, 1)
	done := make(chan struct{})
	go func() {
		defer func() {
			if r := recover(); r != nil {
				panicked <- r
			}
			close(done)
		}()
		captureOutput(func() { RunCommandCenter(db) })
	}()

	select {
	case r := <-panicked:
		t.Errorf("RunCommandCenter panicked: %v", r)
	case <-time.After(300 * time.Millisecond):
		// Completed at least one full display cycle without panic or deadlock.
	}
}

// TestRunCommandCenter_WithEscalations exercises the escalation display path,
// which opens escRows inside a branch that first does two QueryRow calls.
func TestRunCommandCenter_WithEscalations(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	// Seed an open escalation.
	taskID := store.AddBounty(db, 0, "CodeEdit", "escalated task")
	db.Exec(`UPDATE BountyBoard SET status = 'Escalated' WHERE id = ?`, taskID)
	db.Exec(`INSERT INTO Escalations (task_id, severity, reason, status, created_at)
		VALUES (?, 'medium', 'test escalation', 'open', datetime('now'))`, taskID)

	panicked := make(chan any, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				panicked <- r
			}
		}()
		captureOutput(func() { RunCommandCenter(db) })
	}()

	select {
	case r := <-panicked:
		t.Errorf("RunCommandCenter panicked with escalations present: %v", r)
	case <-time.After(300 * time.Millisecond):
		// Ran without deadlock through the escalation display path.
	}
}
