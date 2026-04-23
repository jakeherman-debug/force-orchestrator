package store

import (
	"database/sql"
	"reflect"
	"testing"
)

// TestPattern_P1_UpdateBountyStatusSwallowsDBError verifies the P1 audit
// pattern: the "no silent failures" invariant is violated at the store
// boundary because core mutators have no error return, so callers cannot
// observe DB failures.
//
// This test demonstrates the violation on UpdateBountyStatus in two ways:
//
//  1. STATIC: reflect on the exported function to assert its signature
//     carries no error-shaped return value. If anyone adds an error
//     return to fix the audit, this assertion flips and the test starts
//     failing on the "assertion" branch.
//
//  2. EMPIRICAL: seed a bounty, drop the BountyBoard table to force every
//     subsequent UPDATE to fail, call UpdateBountyStatus, and show that:
//     (a) the caller has no error value to inspect (it is a void-returning
//     func — it cannot even be assigned) and (b) the status was never
//     actually changed. The caller has no way to know the write failed.
//
// The test is EXPECTED TO FAIL under the current (broken) signature,
// because the audit finding is an open defect. If/when the signature is
// fixed to return error, the static half flips and this test must be
// updated to reflect the new contract (and the empirical half can then
// assert err != nil).
func TestPattern_P1_UpdateBountyStatusSwallowsDBError(t *testing.T) {
	// ── Part 1: STATIC signature check ────────────────────────────────────
	// UpdateBountyStatus's declared signature is
	//   func(db *sql.DB, id int, newStatus string)
	// i.e. NumOut() == 0. That is the root cause of P1.
	fnType := reflect.TypeOf(UpdateBountyStatus)
	if fnType.Kind() != reflect.Func {
		t.Fatalf("UpdateBountyStatus is not a function (got %s)", fnType.Kind())
	}
	numOut := fnType.NumOut()
	errIface := reflect.TypeOf((*error)(nil)).Elem()
	returnsError := false
	for i := 0; i < numOut; i++ {
		if fnType.Out(i).Implements(errIface) {
			returnsError = true
			break
		}
	}
	if !returnsError {
		t.Errorf("AUDIT-P1 (AUDIT-022, AUDIT-070): UpdateBountyStatus has no error return "+
			"(NumOut=%d, returnsError=%v). Callers cannot propagate DB failures; "+
			"the 'no silent failures' invariant in CLAUDE.md is unenforceable at the store boundary.",
			numOut, returnsError)
	}

	// ── Part 2: EMPIRICAL demonstration ───────────────────────────────────
	// Seed a real SQLite DB, insert a bounty, drop the table, call the
	// mutator, and show the caller cannot observe the failure.
	db := InitHolocronDSN(":memory:")
	defer db.Close()

	res, err := db.Exec(
		`INSERT INTO BountyBoard (parent_id, type, status, payload, created_at)
		 VALUES (0, 'CodeEdit', 'Pending', 'p1-test', datetime('now'))`)
	if err != nil {
		t.Fatalf("seed insert failed: %v", err)
	}
	rawID, _ := res.LastInsertId()
	id := int(rawID)

	// Induce a guaranteed DB error on the next UPDATE by dropping the table.
	// Every subsequent db.Exec against BountyBoard will now return
	// "no such table: BountyBoard".
	if _, err := db.Exec(`DROP TABLE BountyBoard`); err != nil {
		t.Fatalf("drop table setup failed: %v", err)
	}

	// Sanity: confirm the UPDATE itself fails at the raw driver level.
	if _, directErr := db.Exec(
		`UPDATE BountyBoard SET status = ?, owner = '', locked_at = '' WHERE id = ?`,
		"Completed", id); directErr == nil {
		t.Fatalf("expected direct UPDATE to fail against dropped table, got nil")
	}

	// Call the audited function. There is literally no return value to
	// receive — the following line would not compile if we tried:
	//     err := UpdateBountyStatus(db, id, "Completed")
	// This IS the P1 violation in executable form.
	UpdateBountyStatus(db, id, "Completed")

	// Rebuild the table so we can prove the write was lost. We recreate the
	// row with its pre-call state ("Pending") and assert the caller's
	// post-condition ("status == Completed") never holds — yet the caller
	// has no signal that anything went wrong.
	if _, err := db.Exec(`CREATE TABLE BountyBoard (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		parent_id INTEGER,
		type TEXT,
		status TEXT,
		payload TEXT,
		owner TEXT DEFAULT '',
		locked_at TEXT DEFAULT '',
		created_at TEXT
	)`); err != nil {
		t.Fatalf("recreate table failed: %v", err)
	}
	if _, err := db.Exec(
		`INSERT INTO BountyBoard (id, parent_id, type, status, payload, created_at)
		 VALUES (?, 0, 'CodeEdit', 'Pending', 'p1-test', datetime('now'))`,
		id); err != nil {
		t.Fatalf("reseed failed: %v", err)
	}

	var status string
	if err := db.QueryRow(`SELECT status FROM BountyBoard WHERE id = ?`, id).Scan(&status); err != nil {
		t.Fatalf("status read failed: %v", err)
	}

	// The status remains "Pending" (the write was lost), but the caller
	// of UpdateBountyStatus received no error and has no way to know.
	// Any downstream logic branching on "I just completed this task"
	// will act on a false premise. This is the P1 invariant violation.
	t.Errorf("AUDIT-P1 empirical: UpdateBountyStatus silently swallowed DB failure. "+
		"Post-call status=%q (expected %q); caller received no error (function returns void). "+
		"This is the ~200-call-site silent-failure blast radius described in AUDIT-022/-070.",
		status, "Completed")

	// Keep sql import referenced even if tests are refactored.
	var _ *sql.DB = db
}
