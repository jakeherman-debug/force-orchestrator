package store

import (
	"testing"
)

func TestAddConvoyTaskIdempotent_NewKey_Inserts(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()

	id, existed, err := AddConvoyTaskIdempotent(db, "rebase-conflict:branch:agent/R2-D2/task-1",
		0, "api", "payload", 1, 5, "Pending")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if existed {
		t.Error("expected existed=false on first insert")
	}
	if id == 0 {
		t.Error("expected non-zero task ID")
	}
}

func TestAddConvoyTaskIdempotent_ExistingNonTerminal_ReusesID(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()

	key := "rebase-conflict:branch:agent/R2-D2/task-1"
	first, existed, err := AddConvoyTaskIdempotent(db, key, 0, "api", "payload", 1, 5, "Pending")
	if err != nil {
		t.Fatalf("first insert: %v", err)
	}
	if existed {
		t.Error("first call should not report existed")
	}

	second, existed, err := AddConvoyTaskIdempotent(db, key, 0, "api", "different payload", 1, 5, "Pending")
	if err != nil {
		t.Fatalf("second insert: %v", err)
	}
	if !existed {
		t.Error("second call should report existed=true")
	}
	if second != first {
		t.Errorf("expected second call to return first ID %d, got %d", first, second)
	}

	var rowCount int
	db.QueryRow(`SELECT COUNT(*) FROM BountyBoard WHERE idempotency_key = ?`, key).Scan(&rowCount)
	if rowCount != 1 {
		t.Errorf("expected exactly 1 row with key, got %d", rowCount)
	}
}

func TestAddConvoyTaskIdempotent_TerminalStatusAllowsNewInsert(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()

	key := "rebase-conflict:branch:agent/R2-D2/task-1"
	first, _, _ := AddConvoyTaskIdempotent(db, key, 0, "api", "payload", 1, 5, "Pending")

	for _, terminal := range []string{"Completed", "Cancelled", "Failed"} {
		db.Exec(`UPDATE BountyBoard SET status = ? WHERE id = ?`, terminal, first)

		newID, existed, err := AddConvoyTaskIdempotent(db, key, 0, "api", "retry", 1, 5, "Pending")
		if err != nil {
			t.Fatalf("insert after %s: %v", terminal, err)
		}
		if existed {
			t.Errorf("after %s, should allow new insert, got existed=true", terminal)
		}
		if newID == first {
			t.Errorf("expected a new ID when prior task was %s, got same ID %d", terminal, first)
		}
		// Mark the new one terminal too so the next loop iteration is clean.
		db.Exec(`UPDATE BountyBoard SET status = 'Completed' WHERE id = ?`, newID)
	}
}

func TestAddConvoyTaskIdempotent_EmptyKey_Errors(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()

	_, _, err := AddConvoyTaskIdempotent(db, "", 0, "api", "payload", 1, 5, "Pending")
	if err == nil {
		t.Fatal("expected error for empty key")
	}
}

func TestAddConvoyTaskIdempotent_DifferentKeys_BothInsert(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()

	id1, _, _ := AddConvoyTaskIdempotent(db, "rebase-conflict:branch:a", 0, "api", "p1", 1, 5, "Pending")
	id2, _, _ := AddConvoyTaskIdempotent(db, "rebase-conflict:branch:b", 0, "api", "p2", 1, 5, "Pending")
	if id1 == id2 || id1 == 0 || id2 == 0 {
		t.Errorf("expected two distinct IDs, got %d and %d", id1, id2)
	}
}
