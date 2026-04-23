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
// Fix #8 Phase A closes the headline defect on UpdateBountyStatus by
// changing the signature to return error. This test, post-fix, acts as
// permanent regression protection:
//
//  1. STATIC: reflect on the exported function and assert its signature
//     DOES return an error. If a future refactor drops the error return
//     (or wraps the function to re-hide it), this half fails loudly.
//
//  2. EMPIRICAL: seed a bounty, drop the BountyBoard table to force every
//     subsequent UPDATE to fail, call UpdateBountyStatus, and assert the
//     returned error is non-nil. Callers now have a signal to branch on.
func TestPattern_P1_UpdateBountyStatusSwallowsDBError(t *testing.T) {
	// ── Part 1: STATIC signature check ────────────────────────────────────
	// Post-Fix #8a, UpdateBountyStatus's declared signature is
	//   func(db *sql.DB, id int, newStatus string) error
	// NumOut() == 1 and the return type implements error.
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
		t.Errorf("AUDIT-P1 regression (AUDIT-022, AUDIT-070): UpdateBountyStatus has no error return "+
			"(NumOut=%d, returnsError=%v). Fix #8a explicitly added an error return to close this "+
			"audit — do not remove it without updating CLAUDE.md's 'no silent failures' invariant.",
			numOut, returnsError)
	}

	// ── Part 2: EMPIRICAL demonstration ───────────────────────────────────
	// Seed a real SQLite DB, insert a bounty, drop the table, call the
	// mutator, and confirm the caller receives an error.
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

	// Call the audited function. Post-Fix #8a it returns error.
	gotErr := UpdateBountyStatus(db, id, "Completed")
	if gotErr == nil {
		t.Errorf("AUDIT-P1 empirical regression: UpdateBountyStatus swallowed DB failure. "+
			"Expected a non-nil error; got nil. The ~200-call-site silent-failure blast "+
			"radius described in AUDIT-022/-070 has re-opened.")
	}

	// Keep sql import referenced even if tests are refactored.
	var _ *sql.DB = db
}
