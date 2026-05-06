package store

import (
	"testing"
	"time"
)

// TestRecordDaemonStart_HappyPath: a single recorded start surfaces in
// ListDaemonStarts and counts toward RecentStartCount.
func TestRecordDaemonStart_HappyPath(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()

	if err := RecordDaemonStart(db, "binsha", "gitsha", 12345); err != nil {
		t.Fatalf("RecordDaemonStart: %v", err)
	}

	entries, err := ListDaemonStarts(db, 10)
	if err != nil {
		t.Fatalf("ListDaemonStarts: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}
	got := entries[0]
	if got.BinarySHA != "binsha" || got.GitSHA != "gitsha" || got.PID != 12345 || got.Outcome != "started" {
		t.Errorf("fields wrong: %+v", got)
	}
}

// TestRecentStartCount_Window: only rows within the window are counted.
func TestRecentStartCount_Window(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()

	// 3 recent starts (datetime('now')) — all within any reasonable window.
	for i := 0; i < 3; i++ {
		if err := RecordDaemonStart(db, "bin", "git", i); err != nil {
			t.Fatalf("iter %d: %v", i, err)
		}
	}
	// One artificially-aged row outside the 5-minute window.
	if _, err := db.Exec(`INSERT INTO DaemonStartLog (ts, binary_sha, git_sha, pid, outcome)
		VALUES (datetime('now', '-10 minutes'), 'bin', 'git', 99, 'started')`); err != nil {
		t.Fatalf("backdate insert: %v", err)
	}

	n, err := RecentStartCount(db, 5*time.Minute)
	if err != nil {
		t.Fatalf("RecentStartCount: %v", err)
	}
	if n != 3 {
		t.Errorf("expected 3 (excluding backdated row), got %d", n)
	}
}

// TestRecentStartCount_AbortedNotCounted: rows with outcome='crash_loop_aborted'
// don't count toward the budget — they're observability for the operator.
func TestRecentStartCount_AbortedNotCounted(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()

	if err := RecordDaemonStart(db, "bin", "git", 1); err != nil {
		t.Fatalf("RecordDaemonStart: %v", err)
	}
	if err := RecordDaemonStartAborted(db, "bin", "git", 2); err != nil {
		t.Fatalf("RecordDaemonStartAborted: %v", err)
	}

	n, err := RecentStartCount(db, 5*time.Minute)
	if err != nil {
		t.Fatalf("RecentStartCount: %v", err)
	}
	if n != 1 {
		t.Errorf("expected 1 (only 'started' counted), got %d", n)
	}

	all, _ := ListDaemonStarts(db, 10)
	if len(all) != 2 {
		t.Errorf("expected ListDaemonStarts to surface both rows, got %d", len(all))
	}
}

// TestClearDaemonStartLog: truncate empties the table and the budget
// resets to zero.
func TestClearDaemonStartLog(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()
	for i := 0; i < 5; i++ {
		_ = RecordDaemonStart(db, "bin", "git", i)
	}
	pre, _ := RecentStartCount(db, 5*time.Minute)
	if pre != 5 {
		t.Fatalf("seed count wrong: %d", pre)
	}
	n, err := ClearDaemonStartLog(db)
	if err != nil {
		t.Fatalf("ClearDaemonStartLog: %v", err)
	}
	if n != 5 {
		t.Errorf("expected 5 rows deleted, got %d", n)
	}
	post, _ := RecentStartCount(db, 5*time.Minute)
	if post != 0 {
		t.Errorf("post-clear count = %d, want 0", post)
	}
}

// TestClearDaemonStartLog_Idempotent: running twice is a no-op the
// second time and does not fail.
func TestClearDaemonStartLog_Idempotent(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()
	_ = RecordDaemonStart(db, "bin", "git", 1)
	if _, err := ClearDaemonStartLog(db); err != nil {
		t.Fatalf("first clear: %v", err)
	}
	n, err := ClearDaemonStartLog(db)
	if err != nil {
		t.Fatalf("second clear: %v", err)
	}
	if n != 0 {
		t.Errorf("expected 0 rows on idempotent re-run, got %d", n)
	}
}

// TestRecordDaemonStart_Idempotent: each invocation creates a distinct
// row even with identical inputs (each daemon boot is a separate event).
func TestRecordDaemonStart_Idempotent(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()
	for i := 0; i < 3; i++ {
		if err := RecordDaemonStart(db, "samebin", "samegit", 42); err != nil {
			t.Fatalf("iter %d: %v", i, err)
		}
	}
	entries, _ := ListDaemonStarts(db, 10)
	if len(entries) != 3 {
		t.Errorf("expected 3 distinct rows, got %d", len(entries))
	}
}
