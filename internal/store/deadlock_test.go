package store

import (
	"testing"
	"time"
)

// runWithDeadline runs f in a goroutine and fails the test if it does not
// complete within timeout. Use this to catch deadlocks in DB-heavy functions.
func runWithDeadline(t *testing.T, timeout time.Duration, f func()) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		f()
	}()
	select {
	case <-done:
	case <-time.After(timeout):
		t.Fatalf("function did not complete within %v — possible deadlock (MaxOpenConns(1) violation)", timeout)
	}
}

// ── RecoverStaleConvoys ───────────────────────────────────────────────────────

// TestRecoverStaleConvoys_NoDeadlock_WithData is the primary regression test for
// the MaxOpenConns(1) deadlock: RecoverStaleConvoys must close its outer rows
// before calling AutoRecoverConvoy (which opens its own queries).
func TestRecoverStaleConvoys_NoDeadlock_WithData(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()

	// Two Failed convoys with all tasks Completed — AutoRecoverConvoy will run
	// inner queries for each, exercising the sequential two-cursor pattern.
	for i := 0; i < 2; i++ {
		id, err := CreateConvoy(db, "test-convoy-"+string(rune('a'+i)))
		if err != nil {
			t.Fatalf("CreateConvoy: %v", err)
		}
		db.Exec(`UPDATE Convoys SET status = 'Failed' WHERE id = ?`, id)
		taskID, _ := AddConvoyTask(db, 0, "repo", "payload", id, 0, "Pending")
		db.Exec(`UPDATE BountyBoard SET status = 'Completed' WHERE id = ?`, taskID)
	}

	// Must complete without deadlocking.
	runWithDeadline(t, 3*time.Second, func() {
		RecoverStaleConvoys(db)
	})

	// Both convoys should now be Active (no Failed/Escalated tasks → auto-recovered).
	var failedCount int
	db.QueryRow(`SELECT COUNT(*) FROM Convoys WHERE status = 'Failed'`).Scan(&failedCount)
	if failedCount != 0 {
		t.Errorf("expected 0 Failed convoys after recovery, got %d", failedCount)
	}
}

func TestRecoverStaleConvoys_MixedConvoys(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()

	// Convoy 1: Active — untouched (AutoRecoverConvoy returns early for non-Failed).
	activeID, _ := CreateConvoy(db, "active-convoy")
	_ = activeID

	// Convoy 2: Failed with a Failed task — stays Failed (problem task present).
	blockedID, _ := CreateConvoy(db, "blocked-convoy")
	db.Exec(`UPDATE Convoys SET status = 'Failed' WHERE id = ?`, blockedID)
	problemTask, _ := AddConvoyTask(db, 0, "repo", "failed task", blockedID, 0, "Pending")
	db.Exec(`UPDATE BountyBoard SET status = 'Failed' WHERE id = ?`, problemTask)

	// Convoy 3: Failed, no Failed/Escalated tasks — recovers to Active.
	recoverID, _ := CreateConvoy(db, "recoverable-convoy")
	db.Exec(`UPDATE Convoys SET status = 'Failed' WHERE id = ?`, recoverID)
	taskR, _ := AddConvoyTask(db, 0, "repo", "done", recoverID, 0, "Pending")
	db.Exec(`UPDATE BountyBoard SET status = 'Completed' WHERE id = ?`, taskR)

	runWithDeadline(t, 3*time.Second, func() {
		RecoverStaleConvoys(db)
	})

	check := func(id int, want string) {
		t.Helper()
		var got string
		db.QueryRow(`SELECT status FROM Convoys WHERE id = ?`, id).Scan(&got)
		if got != want {
			t.Errorf("convoy %d: want status %q, got %q", id, want, got)
		}
	}
	check(activeID, "Active")
	check(blockedID, "Failed")
	check(recoverID, "Active")
}

func TestRecoverStaleConvoys_EmptyDB(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()

	runWithDeadline(t, 1*time.Second, func() {
		RecoverStaleConvoys(db)
	})
}

// ── AutoRecoverConvoy ─────────────────────────────────────────────────────────

func TestAutoRecoverConvoy_RecoverWhenNoFailedTasks(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()

	id, _ := CreateConvoy(db, "convoy-a")
	db.Exec(`UPDATE Convoys SET status = 'Failed' WHERE id = ?`, id)
	t1, _ := AddConvoyTask(db, 0, "repo", "task1", id, 0, "Pending")
	t2, _ := AddConvoyTask(db, 0, "repo", "task2", id, 0, "Pending")
	db.Exec(`UPDATE BountyBoard SET status = 'Completed' WHERE id IN (?, ?)`, t1, t2)

	runWithDeadline(t, 3*time.Second, func() {
		AutoRecoverConvoy(db, id, nil)
	})

	var status string
	db.QueryRow(`SELECT status FROM Convoys WHERE id = ?`, id).Scan(&status)
	if status != "Active" {
		t.Errorf("expected convoy Active after all tasks Completed, got %q", status)
	}
}

func TestAutoRecoverConvoy_StaysFailedWhenFailedTasksExist(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()

	id, _ := CreateConvoy(db, "convoy-b")
	db.Exec(`UPDATE Convoys SET status = 'Failed' WHERE id = ?`, id)
	done, _ := AddConvoyTask(db, 0, "repo", "done", id, 0, "Pending")
	db.Exec(`UPDATE BountyBoard SET status = 'Completed' WHERE id = ?`, done)
	broken, _ := AddConvoyTask(db, 0, "repo", "broken", id, 0, "Pending")
	db.Exec(`UPDATE BountyBoard SET status = 'Failed' WHERE id = ?`, broken)

	runWithDeadline(t, 3*time.Second, func() {
		AutoRecoverConvoy(db, id, nil)
	})

	var status string
	db.QueryRow(`SELECT status FROM Convoys WHERE id = ?`, id).Scan(&status)
	if status != "Failed" {
		t.Errorf("expected convoy to stay Failed with a Failed task present, got %q", status)
	}
}

// ── ConvoyProgress ────────────────────────────────────────────────────────────

func TestConvoyProgress_WithCancelledTasks(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()

	id, _ := CreateConvoy(db, "convoy-c")

	t1, _ := AddConvoyTask(db, 0, "repo", "done1", id, 0, "Pending")
	t2, _ := AddConvoyTask(db, 0, "repo", "done2", id, 0, "Pending")
	t3, _ := AddConvoyTask(db, 0, "repo", "cancelled", id, 0, "Pending")
	db.Exec(`UPDATE BountyBoard SET status = 'Completed' WHERE id IN (?, ?)`, t1, t2)
	db.Exec(`UPDATE BountyBoard SET status = 'Cancelled' WHERE id = ?`, t3)

	completed, total := ConvoyProgress(db, id)
	if total != 2 {
		t.Errorf("expected total=2 (excluding Cancelled), got %d", total)
	}
	if completed != 2 {
		t.Errorf("expected completed=2, got %d", completed)
	}
}
