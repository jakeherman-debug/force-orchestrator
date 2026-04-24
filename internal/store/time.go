package store

import "time"

// sqliteTimeLayout is the shape of SQLite's `datetime('now')` output — space
// separator, no TZ, no fractional seconds. It is NOT RFC3339-parseable by
// `time.Time.UnmarshalText`, which is the hazard behind AUDIT-131 (that
// UnmarshalText branch fell through to the ParseInLocation fallback on every
// value but still shipped).
const sqliteTimeLayout = "2006-01-02 15:04:05"

// NowSQLite returns the current UTC wall-clock time formatted to match
// SQLite's `datetime('now')` output. Use this helper in Go-side comparisons
// against any column that was written with `datetime('now')` so the
// comparison is apples-to-apples UTC on both sides (Fix #8c, AUDIT-146 /
// AUDIT-147). Callers that need a `time.Time` value (not a string) should
// use `time.Now().UTC()` directly; this helper only matters when the caller
// is formatting a SQLite-comparable timestamp.
func NowSQLite() string {
	return time.Now().UTC().Format(sqliteTimeLayout)
}

// ParseSQLiteTime parses a `datetime('now')` string into a UTC-located
// time.Time. Callers that previously called `time.Parse(layout, s)` and
// got an implicit-UTC result (the Go stdlib default when the layout has no
// TZ specifier) should migrate to this helper so the UTC assumption is
// spelled out. Returns the zero time.Time and an error on a malformed
// string — callers MUST check the error (AUDIT-131, AUDIT-147).
func ParseSQLiteTime(s string) (time.Time, error) {
	return time.ParseInLocation(sqliteTimeLayout, s, time.UTC)
}
