package store

import (
	"testing"
)

// TestRecordDaemonUpdate_HappyPath: a successful insert returns nil and
// surfaces via ListDaemonUpdateHistory.
func TestRecordDaemonUpdate_HappyPath(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()

	if err := RecordDaemonUpdate(db,
		"abc123", "def456",
		"oldgit", "newgit",
		"jakeh", "success", "ratified via --assume-yes",
	); err != nil {
		t.Fatalf("RecordDaemonUpdate: %v", err)
	}

	entries, err := ListDaemonUpdateHistory(db, 10)
	if err != nil {
		t.Fatalf("ListDaemonUpdateHistory: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}
	got := entries[0]
	if got.OldBinarySHA != "abc123" || got.NewBinarySHA != "def456" {
		t.Errorf("SHA fields wrong: %+v", got)
	}
	if got.OldGitSHA != "oldgit" || got.NewGitSHA != "newgit" {
		t.Errorf("git fields wrong: %+v", got)
	}
	if got.Operator != "jakeh" || got.Outcome != "success" {
		t.Errorf("operator/outcome fields wrong: %+v", got)
	}
	if got.Notes != "ratified via --assume-yes" {
		t.Errorf("notes field wrong: %q", got.Notes)
	}
	if got.TS == "" {
		t.Errorf("ts not populated")
	}
}

// TestRecordDaemonUpdate_AllOutcomes: success, rolled_back, failed all
// land successfully.
func TestRecordDaemonUpdate_AllOutcomes(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()
	for _, outcome := range []string{"success", "rolled_back", "failed"} {
		if err := RecordDaemonUpdate(db, "old", "new", "", "", "op", outcome, ""); err != nil {
			t.Errorf("outcome=%q: %v", outcome, err)
		}
	}
	entries, err := ListDaemonUpdateHistory(db, 10)
	if err != nil {
		t.Fatalf("ListDaemonUpdateHistory: %v", err)
	}
	if len(entries) != 3 {
		t.Fatalf("expected 3 entries, got %d", len(entries))
	}
}

// TestRecordDaemonUpdate_EmptyOutcome_Errors: empty outcome is rejected.
func TestRecordDaemonUpdate_EmptyOutcome_Errors(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()
	if err := RecordDaemonUpdate(db, "old", "new", "", "", "op", "", ""); err == nil {
		t.Fatalf("expected error for empty outcome, got nil")
	}
}

// TestListDaemonUpdateHistory_NewestFirst: entries returned newest-first
// (descending by id).
func TestListDaemonUpdateHistory_NewestFirst(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()

	for i := 0; i < 3; i++ {
		notes := []string{"first", "second", "third"}[i]
		if err := RecordDaemonUpdate(db, "o", "n", "", "", "op", "success", notes); err != nil {
			t.Fatalf("iter %d: %v", i, err)
		}
	}
	entries, err := ListDaemonUpdateHistory(db, 10)
	if err != nil {
		t.Fatalf("ListDaemonUpdateHistory: %v", err)
	}
	if len(entries) != 3 {
		t.Fatalf("expected 3 entries, got %d", len(entries))
	}
	if entries[0].Notes != "third" || entries[2].Notes != "first" {
		t.Errorf("ordering wrong: %v", entries)
	}
}

// TestListDaemonUpdateHistory_LimitDefault: limit <= 0 defaults to 50.
func TestListDaemonUpdateHistory_LimitDefault(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()
	for i := 0; i < 60; i++ {
		_ = RecordDaemonUpdate(db, "o", "n", "", "", "op", "success", "")
	}
	entries, err := ListDaemonUpdateHistory(db, 0)
	if err != nil {
		t.Fatalf("ListDaemonUpdateHistory: %v", err)
	}
	if len(entries) != 50 {
		t.Errorf("expected default limit 50, got %d", len(entries))
	}
}

// TestRecordDaemonUpdate_Idempotent: re-inserting the same logical row
// is allowed (each `force daemon update` invocation is a distinct event).
// We verify the table allows multiple rows with the same SHAs.
func TestRecordDaemonUpdate_Idempotent(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()
	for i := 0; i < 3; i++ {
		if err := RecordDaemonUpdate(db, "same-old", "same-new", "g", "g", "op", "success", "retry"); err != nil {
			t.Fatalf("iter %d: %v", i, err)
		}
	}
	entries, _ := ListDaemonUpdateHistory(db, 10)
	if len(entries) != 3 {
		t.Errorf("expected 3 distinct rows for retried updates, got %d", len(entries))
	}
}
