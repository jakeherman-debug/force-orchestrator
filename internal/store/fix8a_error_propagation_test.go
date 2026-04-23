package store

import (
	"testing"
)

// Fix #8 Phase A — error-propagation coverage for the three self-heal
// terminators at the store boundary: UpdateBountyStatus, FailBounty.
// (CreateEscalation lives in internal/agents and is tested there.)
//
// Each test seeds a real SQLite DB, drops the relevant table to force
// every subsequent UPDATE/INSERT to fail, then calls the terminator and
// asserts a non-nil error comes back. Pre-Fix #8a the functions returned
// void, so callers had no signal to branch on — these tests are permanent
// regression protection against that defect reopening.

func TestFix8A_UpdateBountyStatus_ReturnsErrorOnDBFault(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()

	res, err := db.Exec(
		`INSERT INTO BountyBoard (parent_id, type, status, payload, created_at)
		 VALUES (0, 'CodeEdit', 'Pending', 'test', datetime('now'))`)
	if err != nil {
		t.Fatalf("seed insert failed: %v", err)
	}
	rawID, _ := res.LastInsertId()
	id := int(rawID)

	// Force a guaranteed UPDATE failure by dropping the table.
	if _, err := db.Exec(`DROP TABLE BountyBoard`); err != nil {
		t.Fatalf("drop table setup failed: %v", err)
	}

	gotErr := UpdateBountyStatus(db, id, "Completed")
	if gotErr == nil {
		t.Fatal("UpdateBountyStatus: expected error after table drop, got nil")
	}
}

func TestFix8A_UpdateBountyStatus_SuccessIsNilError(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()

	res, err := db.Exec(
		`INSERT INTO BountyBoard (parent_id, type, status, payload, created_at)
		 VALUES (0, 'CodeEdit', 'Pending', 'ok', datetime('now'))`)
	if err != nil {
		t.Fatalf("seed insert failed: %v", err)
	}
	rawID, _ := res.LastInsertId()
	id := int(rawID)

	if err := UpdateBountyStatus(db, id, "Completed"); err != nil {
		t.Fatalf("UpdateBountyStatus: unexpected error on happy path: %v", err)
	}

	// Post-condition: status is Completed.
	var status string
	if err := db.QueryRow(`SELECT status FROM BountyBoard WHERE id = ?`, id).Scan(&status); err != nil {
		t.Fatalf("status read failed: %v", err)
	}
	if status != "Completed" {
		t.Errorf("expected status=Completed, got %q", status)
	}
}

func TestFix8A_FailBounty_ReturnsErrorOnDBFault(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()

	res, err := db.Exec(
		`INSERT INTO BountyBoard (parent_id, type, status, payload, created_at)
		 VALUES (0, 'CodeEdit', 'Pending', 'test', datetime('now'))`)
	if err != nil {
		t.Fatalf("seed insert failed: %v", err)
	}
	rawID, _ := res.LastInsertId()
	id := int(rawID)

	if _, err := db.Exec(`DROP TABLE BountyBoard`); err != nil {
		t.Fatalf("drop table setup failed: %v", err)
	}

	gotErr := FailBounty(db, id, "forced failure")
	if gotErr == nil {
		t.Fatal("FailBounty: expected error after table drop, got nil")
	}
}

func TestFix8A_FailBounty_SuccessIsNilError(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()

	res, err := db.Exec(
		`INSERT INTO BountyBoard (parent_id, type, status, payload, created_at)
		 VALUES (0, 'CodeEdit', 'Pending', 'ok', datetime('now'))`)
	if err != nil {
		t.Fatalf("seed insert failed: %v", err)
	}
	rawID, _ := res.LastInsertId()
	id := int(rawID)

	if err := FailBounty(db, id, "normal fail reason"); err != nil {
		t.Fatalf("FailBounty: unexpected error on happy path: %v", err)
	}

	var status, errLog string
	if err := db.QueryRow(`SELECT status, error_log FROM BountyBoard WHERE id = ?`, id).Scan(&status, &errLog); err != nil {
		t.Fatalf("read back failed: %v", err)
	}
	if status != "Failed" {
		t.Errorf("expected status=Failed, got %q", status)
	}
	if errLog != "normal fail reason" {
		t.Errorf("expected error_log=%q, got %q", "normal fail reason", errLog)
	}
}
