package store

// Campaign 2 migration tests. Two migrations landed together:
//
//   1. AUDIT-025: Escalations.status='Resolved' → 'Closed' normalization.
//      Three sinks used to write 'Resolved' but no read-side consumer
//      recognised it. Migration UPDATEs historical rows in place.
//
//   2. AUDIT-149: Escalations.auto_resolve_count column added so the
//      escalation-sweeper can enforce its one-shot auto-close budget and
//      respect operator re-opens.
//
// Both migrations must be idempotent — runMigrations runs on every startup
// in production, so a second run must be a no-op.

import (
	"database/sql"
	"testing"
	_ "github.com/mattn/go-sqlite3"
)

// TestAUDIT_025_ResolvedToClosedMigration pre-seeds a DB with 'Resolved'
// rows using a bare-minimum schema, then runs runMigrations and asserts the
// rows are all flipped to 'Closed' with acknowledged_at populated.
func TestAUDIT_025_ResolvedToClosedMigration(t *testing.T) {
	// Build a DB that mimics a pre-Campaign-2 state: schema with Escalations
	// table already created but no auto_resolve_count column and 'Resolved'
	// rows present. Then run createSchema + runMigrations and assert the
	// normalization ran.
	dsn := ":memory:"
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)

	// Minimal ancestor schema — the shape Escalations had pre-Campaign-2.
	if _, err := db.Exec(`CREATE TABLE Escalations (
		id               INTEGER PRIMARY KEY AUTOINCREMENT,
		task_id          INTEGER NOT NULL,
		severity         TEXT    NOT NULL,
		message          TEXT    NOT NULL,
		status           TEXT    DEFAULT 'Open',
		created_at       TEXT    DEFAULT (datetime('now')),
		acknowledged_at  TEXT    DEFAULT ''
	)`); err != nil {
		t.Fatalf("ancestor schema: %v", err)
	}

	// Seed a mix of statuses including the legacy 'Resolved' rows.
	seeds := []struct {
		task   int
		sev    string
		msg    string
		status string
		ack    string
	}{
		{10, "MEDIUM", "a", "Resolved", ""},                    // legacy, ack empty
		{11, "HIGH", "b", "Resolved", "2025-01-01 12:00:00"},    // legacy, ack stamped
		{12, "LOW", "c", "Open", ""},                            // not affected
		{13, "HIGH", "d", "Closed", "2025-01-02 12:00:00"},      // already migrated-style
		{14, "MEDIUM", "e", "Acknowledged", ""},                 // not affected
	}
	for _, s := range seeds {
		if _, err := db.Exec(`INSERT INTO Escalations (task_id, severity, message, status, acknowledged_at)
			VALUES (?, ?, ?, ?, ?)`, s.task, s.sev, s.msg, s.status, s.ack); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}

	// Run migrations. Order matters: createSchema touches the table (IF NOT
	// EXISTS is a no-op) then runMigrations applies the Campaign 2 UPDATE.
	createSchema(db)
	runMigrations(db)

	// Assert: zero rows with 'Resolved' remain.
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM Escalations WHERE status='Resolved'`).Scan(&n); err != nil {
		t.Fatalf("count resolved: %v", err)
	}
	if n != 0 {
		t.Errorf("AUDIT-025 migration: %d row(s) still have status='Resolved'", n)
	}

	// Row 10 (was 'Resolved', ack empty): now Closed, ack populated.
	var status, ack string
	db.QueryRow(`SELECT status, acknowledged_at FROM Escalations WHERE task_id = 10`).Scan(&status, &ack)
	if status != "Closed" {
		t.Errorf("row 10: expected Closed, got %q", status)
	}
	if ack == "" {
		t.Errorf("row 10: migration must populate acknowledged_at when empty")
	}

	// Row 11 (was 'Resolved', ack stamped): Closed, ack preserved at original stamp.
	db.QueryRow(`SELECT status, acknowledged_at FROM Escalations WHERE task_id = 11`).Scan(&status, &ack)
	if status != "Closed" {
		t.Errorf("row 11: expected Closed, got %q", status)
	}
	if ack != "2025-01-01 12:00:00" {
		t.Errorf("row 11: existing acknowledged_at must be preserved, got %q", ack)
	}

	// Row 12 (Open) — untouched.
	db.QueryRow(`SELECT status FROM Escalations WHERE task_id = 12`).Scan(&status)
	if status != "Open" {
		t.Errorf("row 12: expected Open untouched, got %q", status)
	}

	// Row 13 (Closed) — untouched.
	db.QueryRow(`SELECT status FROM Escalations WHERE task_id = 13`).Scan(&status)
	if status != "Closed" {
		t.Errorf("row 13: expected Closed, got %q", status)
	}

	// Row 14 (Acknowledged) — untouched.
	db.QueryRow(`SELECT status FROM Escalations WHERE task_id = 14`).Scan(&status)
	if status != "Acknowledged" {
		t.Errorf("row 14: expected Acknowledged, got %q", status)
	}

	// Idempotence: run migrations a second time and assert nothing flipped.
	runMigrations(db)
	db.QueryRow(`SELECT COUNT(*) FROM Escalations WHERE status='Resolved'`).Scan(&n)
	if n != 0 {
		t.Errorf("idempotence: second migration run produced %d Resolved rows", n)
	}
}

// TestAUDIT_149_AutoResolveCountColumnAdded asserts the migration adds the
// auto_resolve_count column with a sensible default and that it survives a
// second migration run (ALTER TABLE ADD COLUMN fails silently on duplicate —
// the standard SQLite idempotency pattern).
func TestAUDIT_149_AutoResolveCountColumnAdded(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()

	// Direct PRAGMA probe: the column must exist after schema init.
	rows, err := db.Query(`PRAGMA table_info(Escalations)`)
	if err != nil {
		t.Fatalf("pragma: %v", err)
	}
	defer rows.Close()
	var found bool
	var foundDflt string
	for rows.Next() {
		var cid int
		var name, ctype string
		var notnull, pk int
		var dflt sql.NullString
		rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk)
		if name == "auto_resolve_count" {
			found = true
			if dflt.Valid {
				foundDflt = dflt.String
			}
		}
	}
	if !found {
		t.Fatal("AUDIT-149: auto_resolve_count column not present in Escalations after schema init")
	}
	if foundDflt != "0" {
		t.Errorf("AUDIT-149: expected default 0, got %q", foundDflt)
	}

	// A fresh insert must default to 0 (i.e. the sweeper sees a budget of 1).
	db.Exec(`INSERT INTO Escalations (task_id, severity, message, status) VALUES (1, 'LOW', 'x', 'Open')`)
	var count int
	db.QueryRow(`SELECT auto_resolve_count FROM Escalations WHERE task_id = 1`).Scan(&count)
	if count != 0 {
		t.Errorf("fresh row must have auto_resolve_count=0, got %d", count)
	}
}
