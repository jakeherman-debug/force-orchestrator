package store

import (
	"strings"
	"testing"
)

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
