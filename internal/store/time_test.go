package store

import (
	"regexp"
	"testing"
	"time"
)

// TestNowSQLite_ShapeMatchesDatetimeNow verifies NowSQLite's output shape
// matches what SQLite's `datetime('now')` emits, so the two can be
// compared directly in a WHERE clause without a parse round-trip.
func TestNowSQLite_ShapeMatchesDatetimeNow(t *testing.T) {
	got := NowSQLite()
	// Format: YYYY-MM-DD HH:MM:SS (space separator, no fractional, no TZ).
	shape := regexp.MustCompile(`^\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2}$`)
	if !shape.MatchString(got) {
		t.Errorf("NowSQLite() = %q, want shape YYYY-MM-DD HH:MM:SS", got)
	}
	// Confirm the value parses round-trip through ParseSQLiteTime.
	parsed, err := ParseSQLiteTime(got)
	if err != nil {
		t.Errorf("round-trip ParseSQLiteTime(%q): %v", got, err)
	}
	if parsed.Location() != time.UTC {
		t.Errorf("ParseSQLiteTime loc = %v, want UTC", parsed.Location())
	}
}

// TestNowSQLite_MatchesDBDatetimeNow runs SQLite's `datetime('now')` and
// compares the shape + proximity. Both should produce values within a few
// seconds of each other (the DB call and the Go call are effectively
// simultaneous). If they diverge by hours or days, one side has a TZ bug.
func TestNowSQLite_MatchesDBDatetimeNow(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()

	var sqlNow string
	if err := db.QueryRow(`SELECT datetime('now')`).Scan(&sqlNow); err != nil {
		t.Fatalf("datetime('now'): %v", err)
	}
	goNow := NowSQLite()

	sqlT, err := ParseSQLiteTime(sqlNow)
	if err != nil {
		t.Fatalf("parse SQL now: %v", err)
	}
	goT, err := ParseSQLiteTime(goNow)
	if err != nil {
		t.Fatalf("parse Go now: %v", err)
	}
	diff := goT.Sub(sqlT)
	if diff < 0 {
		diff = -diff
	}
	// Allow ±2s for CI jitter. A TZ bug would put this in the hours range.
	if diff > 2*time.Second {
		t.Errorf("NowSQLite vs datetime('now') diverge by %v — TZ mismatch suspected (sql=%s, go=%s)",
			diff, sqlNow, goNow)
	}
}

// TestParseSQLiteTime_RejectsMalformed confirms the helper surfaces
// errors instead of returning a zero time silently — the hazard behind
// AUDIT-132 (raw time.Parse swallowed parse errors and returned 0
// duration, making a malformed row look "brand new forever").
func TestParseSQLiteTime_RejectsMalformed(t *testing.T) {
	for _, tc := range []string{
		"",
		"not a timestamp",
		"2024-01-02T03:04:05Z", // RFC3339 — rejected (this parser is SQLite-layout only)
		"2024/01/02 03:04:05",
	} {
		if _, err := ParseSQLiteTime(tc); err == nil {
			t.Errorf("ParseSQLiteTime(%q) succeeded; want error", tc)
		}
	}
}
