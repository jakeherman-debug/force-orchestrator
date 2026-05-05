// internal/store/convoy_notification_overrides_test.go — D11 Phase 2 Sub-task B.
//
// Round-trip + idempotence + ListActive (excludes closed) + MarkClosed
// coverage for ConvoyNotificationOverrides accessors. Real in-memory
// SQLite via InitHolocronDSN — never mocked, per CLAUDE.md.

package store

import (
	"database/sql"
	"errors"
	"testing"
)

func TestUpsertConvoyNotificationOverride_RoundTrip(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()

	ov := ConvoyNotificationOverride{
		ConvoyID:   42,
		Mode:       "verbose",
		CustomJSON: "{}",
		SetBy:      "jake",
		Reason:     "tracking ZDM migration",
	}
	if err := UpsertConvoyNotificationOverride(db, ov); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	got, err := GetConvoyNotificationOverride(db, 42)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.ConvoyID != 42 || got.Mode != "verbose" || got.SetBy != "jake" {
		t.Fatalf("round-trip mismatch: %+v", got)
	}
	if got.Reason != "tracking ZDM migration" {
		t.Fatalf("reason mismatch: %q", got.Reason)
	}
	if got.SetAt == "" {
		t.Fatalf("set_at not stamped")
	}
	if got.ConvoyClosedAt != "" {
		t.Fatalf("convoy_closed_at unexpectedly set on insert: %q", got.ConvoyClosedAt)
	}
}

func TestUpsertConvoyNotificationOverride_Idempotent(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()

	ov := ConvoyNotificationOverride{ConvoyID: 7, Mode: "quiet", SetBy: "ops"}
	if err := UpsertConvoyNotificationOverride(db, ov); err != nil {
		t.Fatalf("first upsert: %v", err)
	}
	// Second upsert with a different mode should overwrite, not duplicate.
	ov2 := ConvoyNotificationOverride{ConvoyID: 7, Mode: "verbose", SetBy: "ops2", Reason: "changed mind"}
	if err := UpsertConvoyNotificationOverride(db, ov2); err != nil {
		t.Fatalf("second upsert: %v", err)
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM ConvoyNotificationOverrides WHERE convoy_id = 7`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Fatalf("idempotence: expected 1 row, got %d", n)
	}
	got, err := GetConvoyNotificationOverride(db, 7)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Mode != "verbose" || got.SetBy != "ops2" || got.Reason != "changed mind" {
		t.Fatalf("upsert overwrite mismatch: %+v", got)
	}
}

func TestUpsertConvoyNotificationOverride_RejectsInvalidMode(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()
	err := UpsertConvoyNotificationOverride(db, ConvoyNotificationOverride{
		ConvoyID: 1, Mode: "bogus", SetBy: "x",
	})
	if err == nil {
		t.Fatalf("expected error for invalid mode, got nil")
	}
}

func TestUpsertConvoyNotificationOverride_RejectsZeroConvoyID(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()
	err := UpsertConvoyNotificationOverride(db, ConvoyNotificationOverride{
		ConvoyID: 0, Mode: "verbose", SetBy: "x",
	})
	if err == nil {
		t.Fatalf("expected error for convoy_id=0, got nil")
	}
}

func TestGetConvoyNotificationOverride_MissingReturnsErrNoRows(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()
	_, err := GetConvoyNotificationOverride(db, 999)
	if !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("expected sql.ErrNoRows, got %v", err)
	}
}

func TestClearConvoyNotificationOverride(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()

	if err := UpsertConvoyNotificationOverride(db, ConvoyNotificationOverride{
		ConvoyID: 5, Mode: "quiet", SetBy: "jake",
	}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if err := ClearConvoyNotificationOverride(db, 5); err != nil {
		t.Fatalf("clear: %v", err)
	}
	_, err := GetConvoyNotificationOverride(db, 5)
	if !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("expected sql.ErrNoRows after clear, got %v", err)
	}

	// Idempotent: clear-twice is a no-op.
	if err := ClearConvoyNotificationOverride(db, 5); err != nil {
		t.Fatalf("second clear: %v", err)
	}
	// Clearing a never-existed convoy is a no-op too.
	if err := ClearConvoyNotificationOverride(db, 12345); err != nil {
		t.Fatalf("clear-missing: %v", err)
	}
}

func TestListActiveConvoyNotificationOverrides_ExcludesClosed(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()

	for _, ov := range []ConvoyNotificationOverride{
		{ConvoyID: 1, Mode: "verbose", SetBy: "jake", Reason: "active"},
		{ConvoyID: 2, Mode: "quiet", SetBy: "jake", Reason: "soon-to-close"},
		{ConvoyID: 3, Mode: "verbose", SetBy: "jake", Reason: "still-active"},
	} {
		if err := UpsertConvoyNotificationOverride(db, ov); err != nil {
			t.Fatalf("upsert convoy=%d: %v", ov.ConvoyID, err)
		}
	}

	// Close convoy 2.
	if err := MarkConvoyOverrideClosed(db, 2, NowSQLite()); err != nil {
		t.Fatalf("mark closed: %v", err)
	}

	rows, err := ListActiveConvoyNotificationOverrides(db)
	if err != nil {
		t.Fatalf("list active: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("expected 2 active rows, got %d: %+v", len(rows), rows)
	}
	for _, r := range rows {
		if r.ConvoyID == 2 {
			t.Fatalf("closed convoy 2 leaked into active list: %+v", r)
		}
		if r.ConvoyClosedAt != "" {
			t.Fatalf("active row carries non-empty convoy_closed_at: %+v", r)
		}
	}
}

func TestMarkConvoyOverrideClosed_StampsConvoyClosedAt(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()

	if err := UpsertConvoyNotificationOverride(db, ConvoyNotificationOverride{
		ConvoyID: 99, Mode: "verbose", SetBy: "jake",
	}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	closedAt := NowSQLite()
	if err := MarkConvoyOverrideClosed(db, 99, closedAt); err != nil {
		t.Fatalf("mark closed: %v", err)
	}
	got, err := GetConvoyNotificationOverride(db, 99)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.ConvoyClosedAt != closedAt {
		t.Fatalf("convoy_closed_at = %q, want %q", got.ConvoyClosedAt, closedAt)
	}
}

func TestMarkConvoyOverrideClosed_RejectsEmptyClosedAt(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()
	err := MarkConvoyOverrideClosed(db, 1, "")
	if err == nil {
		t.Fatalf("expected error for empty closedAt, got nil")
	}
}

func TestMarkConvoyOverrideClosed_NoRowIsNoOp(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()
	// No row for convoy 555 — MarkClosed must not error (cleanup dog
	// may sweep convoys that never had an override).
	if err := MarkConvoyOverrideClosed(db, 555, NowSQLite()); err != nil {
		t.Fatalf("mark missing: %v", err)
	}
}

// Re-upserting an override on a convoy whose previous override was
// closed should reset convoy_closed_at to NULL — operator may turn the
// override back on after the convoy was prematurely marked closed.
func TestUpsertConvoyNotificationOverride_ReopensClosedRow(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()
	if err := UpsertConvoyNotificationOverride(db, ConvoyNotificationOverride{
		ConvoyID: 33, Mode: "verbose", SetBy: "jake",
	}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if err := MarkConvoyOverrideClosed(db, 33, NowSQLite()); err != nil {
		t.Fatalf("mark closed: %v", err)
	}
	// Re-upsert.
	if err := UpsertConvoyNotificationOverride(db, ConvoyNotificationOverride{
		ConvoyID: 33, Mode: "quiet", SetBy: "jake", Reason: "actually still going",
	}); err != nil {
		t.Fatalf("re-upsert: %v", err)
	}
	got, err := GetConvoyNotificationOverride(db, 33)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.ConvoyClosedAt != "" {
		t.Fatalf("re-upsert did not clear convoy_closed_at: %+v", got)
	}
	if got.Mode != "quiet" {
		t.Fatalf("re-upsert mode mismatch: %+v", got)
	}
}
