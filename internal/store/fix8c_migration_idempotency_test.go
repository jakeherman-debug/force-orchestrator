package store

import (
	"testing"
)

// TestFix8c_MigrationIdempotency is the Fix #8c / Campaign 4 acceptance gate
// for schema invariant 5 in the per-campaign checklist: running
// `InitHolocronDSN(":memory:")` twice (equivalent to `createSchema` +
// `runMigrations` twice) must not error. It's the behavioural companion to
// the AUDIT-077-specific test in audit_schema_time_test.go — that test
// targets the DROP COLUMN; this one sweeps the whole migration block.
//
// We can't re-run `InitHolocronDSN` against a closed handle, but we can
// re-run `createSchema` + `runMigrations` against the same open connection
// N times. Each pass must be a no-op (same PRAGMA table shapes, no
// exceptions, no loss of rows).
func TestFix8c_MigrationIdempotency(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()

	// Seed a BountyBoard row so the post-fix created_at backfill has
	// something to repair across runs, and a row with a valid created_at
	// so we can confirm legitimate values are preserved.
	_, err := db.Exec(
		`INSERT INTO BountyBoard (id, type, status, payload, created_at) VALUES
		 (100, 'CodeEdit', 'Pending', 'a', ''),
		 (101, 'CodeEdit', 'Pending', 'b', '2024-01-02 03:04:05')`,
	)
	if err != nil {
		t.Fatalf("seed BountyBoard: %v", err)
	}

	// Snapshot the current table shape for BountyBoard and every other
	// table we care about, then re-run migrations 3 times and confirm the
	// shape is unchanged and no errors surface.
	type tableShape struct {
		columns map[string]bool
	}
	tables := []string{
		"BountyBoard", "Escalations", "Convoys", "Repositories",
		"AskBranchPRs", "ConvoyAskBranches", "TaskHistory", "Fleet_Mail",
		"Dogs", "FleetMemory", "PRReviewComments", "TaskNotes",
		"ProposedConvoys", "FeatureBlockers", "ConvoyHolds", "AuditLog",
		"SystemConfig", "TaskDependencies", "Agents",
	}
	snapshots := map[string]tableShape{}
	for _, tbl := range tables {
		snapshots[tbl] = tableShape{columns: columnsOf(t, ":memory:", tbl)}
	}
	// Wait — columnsOf opens a NEW in-memory DB, which defeats the purpose.
	// Use a local helper against the current handle.
	readCols := func(table string) map[string]bool {
		rows, err := db.Query(`SELECT name FROM pragma_table_info(?)`, table)
		if err != nil {
			t.Fatalf("pragma_table_info(%s): %v", table, err)
		}
		defer rows.Close()
		cs := map[string]bool{}
		for rows.Next() {
			var n string
			rows.Scan(&n)
			cs[n] = true
		}
		return cs
	}
	for _, tbl := range tables {
		snapshots[tbl] = tableShape{columns: readCols(tbl)}
	}

	for pass := 1; pass <= 3; pass++ {
		createSchema(db)
		runMigrations(db)
		for _, tbl := range tables {
			got := readCols(tbl)
			want := snapshots[tbl].columns
			if len(got) != len(want) {
				t.Errorf("pass %d: table %q column count drifted: before=%v after=%v", pass, tbl, want, got)
				continue
			}
			for col := range want {
				if !got[col] {
					t.Errorf("pass %d: table %q lost column %q", pass, tbl, col)
				}
			}
			for col := range got {
				if !want[col] {
					t.Errorf("pass %d: table %q grew column %q unexpectedly", pass, tbl, col)
				}
			}
		}
	}

	// Seed row 100 (empty created_at) should have been re-stamped by the
	// Fix #8c backfill; seed row 101 (valid timestamp) must be preserved.
	var ts100, ts101 string
	if err := db.QueryRow(`SELECT created_at FROM BountyBoard WHERE id=100`).Scan(&ts100); err != nil {
		t.Fatalf("read row 100: %v", err)
	}
	if err := db.QueryRow(`SELECT created_at FROM BountyBoard WHERE id=101`).Scan(&ts101); err != nil {
		t.Fatalf("read row 101: %v", err)
	}
	if ts100 == "" {
		t.Errorf("Fix #8c backfill: row 100 still has empty created_at after 3 migration passes")
	}
	if ts101 != "2024-01-02 03:04:05" {
		t.Errorf("Fix #8c backfill preservation: row 101 created_at drifted from the seeded value: got %q, want %q",
			ts101, "2024-01-02 03:04:05")
	}

	// The DROP COLUMN idempotency gate (AUDIT-077). Having run migrations
	// 3 times, blocked_by must remain absent.
	bbCols := readCols("BountyBoard")
	if bbCols["blocked_by"] {
		t.Errorf("AUDIT-077: BountyBoard.blocked_by reappeared after 3 migration passes")
	}
}
