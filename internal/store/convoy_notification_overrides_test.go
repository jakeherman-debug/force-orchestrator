// internal/store/convoy_notification_overrides_test.go — D11 Phase 2 Sub-tasks B + C.
//
// Round-trip + idempotence + ListActive (excludes closed) + MarkClosed
// coverage for ConvoyNotificationOverrides accessors. Real in-memory
// SQLite via InitHolocronDSN — never mocked, per CLAUDE.md.

package store

import (
	"database/sql"
	"errors"
	"strings"
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

// TestMarkConvoyOverrideClosed_HappyPath stamps an explicit timestamp
// onto an existing override row.
func TestMarkConvoyOverrideClosed_HappyPath(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()

	if _, err := db.Exec(`INSERT INTO ConvoyNotificationOverrides
		(convoy_id, mode, custom_json, set_at, set_by, reason)
		VALUES (42, 'verbose', '{}', datetime('now'), 'op', 'test')`); err != nil {
		t.Fatalf("seed: %v", err)
	}

	if err := MarkConvoyOverrideClosed(db, 42, "2026-01-01 00:00:00"); err != nil {
		t.Fatalf("MarkConvoyOverrideClosed: %v", err)
	}

	var stamp string
	if err := db.QueryRow(
		`SELECT IFNULL(convoy_closed_at, '') FROM ConvoyNotificationOverrides WHERE convoy_id = 42`,
	).Scan(&stamp); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if stamp != "2026-01-01 00:00:00" {
		t.Errorf("expected explicit timestamp written, got %q", stamp)
	}
}

// TestMarkConvoyOverrideClosed_StampsCurrentWhenEmpty confirms the
// helper substitutes NowSQLite when ts == "". The exact value depends
// on the system clock so we only assert (a) the column is non-empty
// and (b) it parses as a SQLite timestamp.
func TestMarkConvoyOverrideClosed_StampsCurrentWhenEmpty(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()

	if _, err := db.Exec(`INSERT INTO ConvoyNotificationOverrides
		(convoy_id, mode, custom_json, set_at, set_by, reason)
		VALUES (43, 'quiet', '{}', datetime('now'), 'op', '')`); err != nil {
		t.Fatalf("seed: %v", err)
	}

	if err := MarkConvoyOverrideClosed(db, 43, ""); err != nil {
		t.Fatalf("MarkConvoyOverrideClosed: %v", err)
	}

	var stamp string
	if err := db.QueryRow(
		`SELECT IFNULL(convoy_closed_at, '') FROM ConvoyNotificationOverrides WHERE convoy_id = 43`,
	).Scan(&stamp); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if stamp == "" {
		t.Fatal("expected NowSQLite() to be substituted, got empty string")
	}
	if _, err := ParseSQLiteTime(stamp); err != nil {
		t.Errorf("expected SQLite-shaped timestamp, got %q (parse err: %v)", stamp, err)
	}
}

// TestMarkConvoyOverrideClosed_NoRowSilentNoOp confirms calling on a
// convoy with no override row is a silent success (UPDATE affects 0
// rows but doesn't error). This is the happy-path the convoy
// terminal-transition hook relies on — most convoys never set an
// override, so the hook fires for every convoy regardless.
func TestMarkConvoyOverrideClosed_NoRowSilentNoOp(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()

	if err := MarkConvoyOverrideClosed(db, 999, ""); err != nil {
		t.Fatalf("expected no error on convoy with no override row, got %v", err)
	}

	var count int
	db.QueryRow(`SELECT COUNT(*) FROM ConvoyNotificationOverrides WHERE convoy_id = 999`).Scan(&count)
	if count != 0 {
		t.Errorf("UPDATE on non-existent row should not have inserted, got count=%d", count)
	}
}

// TestMarkConvoyOverrideClosed_Idempotent re-stamps the same row
// twice. The second call slides the boundary forward; both calls
// succeed and the row's stamp matches the second call's input.
func TestMarkConvoyOverrideClosed_Idempotent(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()

	if _, err := db.Exec(`INSERT INTO ConvoyNotificationOverrides
		(convoy_id, mode, custom_json, set_at, set_by, reason)
		VALUES (44, 'verbose', '{}', datetime('now'), 'op', '')`); err != nil {
		t.Fatalf("seed: %v", err)
	}

	first := "2026-01-01 00:00:00"
	second := "2026-02-01 00:00:00"
	if err := MarkConvoyOverrideClosed(db, 44, first); err != nil {
		t.Fatalf("first stamp: %v", err)
	}
	if err := MarkConvoyOverrideClosed(db, 44, second); err != nil {
		t.Fatalf("second stamp: %v", err)
	}

	var stamp string
	db.QueryRow(`SELECT IFNULL(convoy_closed_at, '') FROM ConvoyNotificationOverrides WHERE convoy_id = 44`).Scan(&stamp)
	if !strings.HasPrefix(stamp, "2026-02") {
		t.Errorf("expected second stamp to win, got %q", stamp)
	}
}
