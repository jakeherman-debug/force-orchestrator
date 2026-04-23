package agents

import (
	"sync"
	"testing"

	"force-orchestrator/internal/store"
)

// TestCreateEscalation_ConcurrentCallers exercises the race window Fix #3
// closed on Escalations. Before the fix, three concurrent self-healing
// paths (ConvoyReview loop cap + Captain exhaustion + inquisitor boot-
// triage) could each INSERT a fresh Open row for the same task, spamming
// the operator inbox. Fix #3 introduced the partial UNIQUE
// idx_escalations_open_task on Escalations(task_id) WHERE status='Open',
// backed by an ON CONFLICT DO UPDATE that merges severity/message.
//
// Post-fix: 50 goroutines → exactly 1 Open row per task_id.
func TestCreateEscalation_ConcurrentCallers(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	// Seed a task row so UpdateBountyStatus inside CreateEscalation has
	// something to hit (the webhook path is skipped because the row has no
	// external subscribers in the in-memory DB).
	res, err := db.Exec(`INSERT INTO BountyBoard (type, status, payload) VALUES ('CodeEdit', 'Pending', 'seed')`)
	if err != nil {
		t.Fatalf("seed bounty: %v", err)
	}
	taskID64, _ := res.LastInsertId()
	taskID := int(taskID64)

	const goroutines = 50
	var wg sync.WaitGroup
	start := make(chan struct{})
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		sev := store.SeverityLow
		if i%3 == 0 {
			sev = store.SeverityMedium
		}
		if i%7 == 0 {
			sev = store.SeverityHigh
		}
		i := i
		go func() {
			defer wg.Done()
			<-start
			_, _ = CreateEscalation(db, taskID, sev, "concurrent call")
			_ = i
		}()
	}
	close(start)
	wg.Wait()

	var openCount int
	if err := db.QueryRow(`SELECT COUNT(*) FROM Escalations WHERE task_id = ? AND status = 'Open'`, taskID).
		Scan(&openCount); err != nil {
		t.Fatalf("count open escalations: %v", err)
	}
	if openCount != 1 {
		t.Fatalf("Fix #3 regression: expected exactly 1 Open escalation for task %d "+
			"after %d concurrent CreateEscalation calls, got %d — the partial UNIQUE "+
			"idx_escalations_open_task + ON CONFLICT merge is missing or broken.",
			taskID, goroutines, openCount)
	}

	// The merged row should carry the highest severity seen (HIGH, since at
	// least one caller fed HIGH). The test passes the Low/Medium/High mix
	// through the CASE expression in CreateEscalation.
	var mergedSev string
	if err := db.QueryRow(`SELECT severity FROM Escalations WHERE task_id = ? AND status = 'Open'`, taskID).
		Scan(&mergedSev); err != nil {
		t.Fatalf("select merged severity: %v", err)
	}
	if mergedSev != string(store.SeverityHigh) {
		t.Fatalf("Fix #3: merged severity should be HIGH (highest seen) — got %q", mergedSev)
	}
}

// TestCreateEscalation_NoDuplicatesAcrossSeparateTasks regression-guards the
// index predicate: two distinct tasks must each be able to hold one Open
// escalation simultaneously.
func TestCreateEscalation_NoDuplicatesAcrossSeparateTasks(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	// Seed two tasks.
	r1, _ := db.Exec(`INSERT INTO BountyBoard (type, status, payload) VALUES ('CodeEdit', 'Pending', 'a')`)
	r2, _ := db.Exec(`INSERT INTO BountyBoard (type, status, payload) VALUES ('CodeEdit', 'Pending', 'b')`)
	id1, _ := r1.LastInsertId()
	id2, _ := r2.LastInsertId()
	if id1 == id2 || id1 == 0 || id2 == 0 {
		t.Fatalf("seed task ids collide: %d %d", id1, id2)
	}

	_, _ = CreateEscalation(db, int(id1), store.SeverityLow, "first")
	_, _ = CreateEscalation(db, int(id2), store.SeverityMedium, "second")

	var count int
	db.QueryRow(`SELECT COUNT(*) FROM Escalations WHERE status = 'Open'`).Scan(&count)
	if count != 2 {
		t.Fatalf("expected 2 Open rows across 2 distinct tasks, got %d — the "+
			"partial UNIQUE's WHERE status='Open' predicate is under-scoped.", count)
	}
}

// TestCreateEscalation_TerminalDoesNotBlockNewOpen verifies that once an
// escalation is Acknowledged/Closed, a new Open row for the same task is
// allowed — the partial predicate is `status='Open'`, not just `task_id`.
func TestCreateEscalation_TerminalDoesNotBlockNewOpen(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	res, _ := db.Exec(`INSERT INTO BountyBoard (type, status, payload) VALUES ('CodeEdit', 'Pending', 'seed')`)
	id, _ := res.LastInsertId()
	taskID := int(id)

	first, err := CreateEscalation(db, taskID, store.SeverityLow, "first")
	if err != nil {
		t.Fatalf("first CreateEscalation: %v", err)
	}
	if first == 0 {
		t.Fatalf("first CreateEscalation returned 0")
	}
	// Move the first escalation out of Open.
	db.Exec(`UPDATE Escalations SET status = 'Acknowledged' WHERE id = ?`, first)

	// Task needs to be re-queued before a new escalation (CreateEscalation
	// also runs UpdateBountyStatus(Escalated); UpdateBountyStatus clears owner
	// but keeps the row). Simulate that by resetting status directly.
	db.Exec(`UPDATE BountyBoard SET status = 'Pending' WHERE id = ?`, taskID)

	second, err := CreateEscalation(db, taskID, store.SeverityHigh, "second")
	if err != nil {
		t.Fatalf("second CreateEscalation: %v", err)
	}
	if second == 0 {
		t.Fatalf("second CreateEscalation returned 0 — partial UNIQUE mis-scoped, "+
			"may be blocking even Acknowledged rows")
	}
	if second == first {
		t.Fatalf("second escalation reused Acknowledged id %d — expected a new Open row", first)
	}

	var openCount int
	db.QueryRow(`SELECT COUNT(*) FROM Escalations WHERE task_id = ? AND status = 'Open'`, taskID).Scan(&openCount)
	if openCount != 1 {
		t.Fatalf("expected 1 Open escalation post-retry, got %d", openCount)
	}
}
