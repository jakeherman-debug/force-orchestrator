package store

import (
	"testing"
)

// TestFetchDigestStats_NoUpdatedAtColumn is a regression test for the
// daily-digest dog failure (fleet mail #160). BountyBoard has no updated_at
// column; FetchDigestStats must not reference it.
func TestFetchDigestStats_NoUpdatedAtColumn(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()

	// Calling FetchDigestStats on an empty DB must not error —
	// the old query would panic here with "no such column: updated_at".
	stats, err := FetchDigestStats(db)
	if err != nil {
		t.Fatalf("FetchDigestStats returned error: %v", err)
	}
	if stats.Completed != 0 || stats.Failed != 0 || stats.Escalated != 0 {
		t.Errorf("expected zero counts on empty DB, got %+v", stats)
	}
}

// TestFetchDigestStats_CountsTerminalTasksViaHistory verifies that tasks
// completed within the last 24 hours are counted via TaskHistory, not via a
// non-existent updated_at column on BountyBoard.
func TestFetchDigestStats_CountsTerminalTasksViaHistory(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()

	// One Completed task with a recent TaskHistory entry.
	tid1 := AddBounty(db, 0, "Feature", "task-1")
	UpdateBountyStatus(db, tid1, "Completed")
	RecordTaskHistory(db, tid1, "R2-D2", "sess1", "done", "Completed")

	// One Failed task.
	tid2 := AddBounty(db, 0, "Feature", "task-2")
	UpdateBountyStatus(db, tid2, "Failed")
	RecordTaskHistory(db, tid2, "BB-8", "sess2", "oops", "Failed")

	// One Pending task — must NOT appear in terminal counts.
	tid3 := AddBounty(db, 0, "Feature", "task-3")
	_ = tid3

	stats, err := FetchDigestStats(db)
	if err != nil {
		t.Fatalf("FetchDigestStats: %v", err)
	}
	if stats.Completed != 1 {
		t.Errorf("Completed: want 1, got %d", stats.Completed)
	}
	if stats.Failed != 1 {
		t.Errorf("Failed: want 1, got %d", stats.Failed)
	}
	if stats.Escalated != 0 {
		t.Errorf("Escalated: want 0, got %d", stats.Escalated)
	}
	if stats.Pending != 1 {
		t.Errorf("Pending: want 1, got %d", stats.Pending)
	}
}
