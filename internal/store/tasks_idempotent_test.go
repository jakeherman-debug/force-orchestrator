package store

import (
	"sync"
	"testing"
)

// TestAddConvoyTaskIdempotent_ConcurrentCallers exercises 50 goroutines
// simultaneously calling AddConvoyTaskIdempotent with the same key. Fix #3
// added the partial UNIQUE idx_bounty_idem + ON CONFLICT DO NOTHING so
// exactly one row lands. Without the fix, the SELECT-then-INSERT race was
// observable at 2–50 duplicates per run. Kept as permanent regression cover.
func TestAddConvoyTaskIdempotent_ConcurrentCallers(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()

	// Seed a convoy row so the parent FK-equivalent has a target.
	if _, err := db.Exec(`INSERT INTO Convoys (id, name, status) VALUES (1, 'race-convoy', 'Active')`); err != nil {
		t.Fatalf("seed convoy: %v", err)
	}

	const (
		goroutines = 50
		key        = "rebase-conflict:branch:agent/R2-D2/race"
	)

	var wg sync.WaitGroup
	start := make(chan struct{})
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			<-start
			_, _, err := AddConvoyTaskIdempotent(db, key, 0, "api", "payload", 1, 5, "Pending")
			if err != nil {
				t.Errorf("unexpected error: %v", err)
			}
		}()
	}
	close(start)
	wg.Wait()

	var rows int
	if err := db.QueryRow(`SELECT COUNT(*) FROM BountyBoard WHERE idempotency_key = ?`, key).Scan(&rows); err != nil {
		t.Fatalf("count query: %v", err)
	}
	if rows != 1 {
		t.Fatalf("expected exactly 1 row for idempotency_key=%q after %d concurrent inserts, got %d",
			key, goroutines, rows)
	}
}

// TestAddConvoyTaskIdempotent_ConcurrentCallersReturnSameID exercises the
// post-conflict fallback: every goroutine should receive a usable id, and
// they should all resolve to the same row (the winner of the race).
func TestAddConvoyTaskIdempotent_ConcurrentCallersReturnSameID(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()

	if _, err := db.Exec(`INSERT INTO Convoys (id, name, status) VALUES (1, 'race-id-convoy', 'Active')`); err != nil {
		t.Fatalf("seed convoy: %v", err)
	}

	const goroutines = 50
	key := "convoy-review:42"

	var (
		wg  sync.WaitGroup
		mu  sync.Mutex
		ids []int
	)
	start := make(chan struct{})
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			<-start
			id, _, err := AddConvoyTaskIdempotent(db, key, 0, "api", `{"convoy_id":42}`, 1, 5, "Pending")
			if err != nil {
				t.Errorf("insert: %v", err)
				return
			}
			mu.Lock()
			ids = append(ids, id)
			mu.Unlock()
		}()
	}
	close(start)
	wg.Wait()

	if len(ids) != goroutines {
		t.Fatalf("expected %d ids collected, got %d", goroutines, len(ids))
	}
	// Every goroutine should have seen the same id (the row that won the race).
	first := ids[0]
	if first <= 0 {
		t.Fatalf("first id is zero/negative: %d", first)
	}
	for _, id := range ids[1:] {
		if id != first {
			t.Fatalf("AddConvoyTaskIdempotent returned divergent ids under race: %d vs %d", first, id)
		}
	}

	var rows int
	db.QueryRow(`SELECT COUNT(*) FROM BountyBoard WHERE idempotency_key = ?`, key).Scan(&rows)
	if rows != 1 {
		t.Fatalf("expected exactly 1 row for key %q, got %d", key, rows)
	}
}

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
