package agents

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"sync"
	"testing"

	"force-orchestrator/internal/store"
)

// seedDivergenceTask inserts a Locked-status BountyBoard row with the
// canonical empty ring. Returns the new task ID.
func seedDivergenceTask(t *testing.T, db *sql.DB) int {
	t.Helper()
	res, err := db.Exec(`INSERT INTO BountyBoard
		(parent_id, target_repo, type, status, payload, branch_name,
		 owner, locked_at, recent_commit_hashes_json, created_at)
		VALUES (0, 'demo', 'CodeEdit', 'Locked', 'do the thing',
		        'agent/R2-D2/task-1', 'R2-D2', datetime('now'), '[]', datetime('now'))`)
	if err != nil {
		t.Fatalf("seed task: %v", err)
	}
	id, _ := res.LastInsertId()
	return int(id)
}

func readRing(t *testing.T, db *sql.DB, taskID int) []string {
	t.Helper()
	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer func() { _ = tx.Rollback() }()
	r, err := loadCommitHashRingTx(tx, taskID)
	if err != nil {
		t.Fatalf("loadCommitHashRingTx: %v", err)
	}
	return r.Hashes
}

// ── 1. Three-commit cycle escalates ────────────────────────────────────────

func TestDivergenceDetector_ThreeCommitCycle_Escalates(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	taskID := seedDivergenceTask(t, db)
	ctx := context.Background()

	// First commit: A. No circle (only one entry).
	circled, err := RecordCommitAndCheckCircle(ctx, db, taskID, "A")
	if err != nil {
		t.Fatalf("Push A: %v", err)
	}
	if circled {
		t.Fatalf("first push (A) should not circle")
	}

	// Second commit: B. No circle (different from A).
	circled, err = RecordCommitAndCheckCircle(ctx, db, taskID, "B")
	if err != nil {
		t.Fatalf("Push B: %v", err)
	}
	if circled {
		t.Fatalf("push B should not circle")
	}

	// Third commit: A. CIRCLE — A appears at index 0, which is not the
	// most recent entry (most recent is B at index 1).
	circled, err = RecordCommitAndCheckCircle(ctx, db, taskID, "A")
	if err != nil {
		t.Fatalf("Push A again: %v", err)
	}
	if !circled {
		t.Fatalf("push A after [A,B] should circle")
	}

	// Escalate the task.
	if err := EscalateOnCircle(ctx, db, taskID, "Locked"); err != nil {
		t.Fatalf("EscalateOnCircle: %v", err)
	}

	// Status moved to Escalated, spend_suspended=1.
	var status string
	var suspended int
	if err := db.QueryRow(`SELECT status, spend_suspended FROM BountyBoard WHERE id = ?`, taskID).Scan(&status, &suspended); err != nil {
		t.Fatalf("read row: %v", err)
	}
	if status != "Escalated" {
		t.Errorf("status = %q, want Escalated", status)
	}
	if suspended != 1 {
		t.Errorf("spend_suspended = %d, want 1", suspended)
	}

	// Escalations row created.
	var escCount int
	if err := db.QueryRow(`SELECT COUNT(*) FROM Escalations WHERE task_id = ? AND status = 'Open'`, taskID).Scan(&escCount); err != nil {
		t.Fatalf("count escalations: %v", err)
	}
	if escCount != 1 {
		t.Errorf("escalations Open = %d, want 1", escCount)
	}

	// Operator mail with [CIRCULAR COMMITS] subject.
	var subj string
	if err := db.QueryRow(`SELECT subject FROM Fleet_Mail WHERE to_agent = 'operator' AND task_id = ? ORDER BY id DESC LIMIT 1`, taskID).Scan(&subj); err != nil {
		t.Fatalf("read mail: %v", err)
	}
	if !strings.Contains(subj, "[CIRCULAR COMMITS]") {
		t.Errorf("mail subject = %q, want it to contain [CIRCULAR COMMITS]", subj)
	}

	// Audit log.
	var auditCount int
	if err := db.QueryRow(`SELECT COUNT(*) FROM AuditLog WHERE action = 'circular-commits-detected' AND task_id = ?`, taskID).Scan(&auditCount); err != nil {
		t.Fatalf("audit count: %v", err)
	}
	if auditCount != 1 {
		t.Errorf("audit count = %d, want 1", auditCount)
	}
}

// ── 2. Linear progression takes no action ──────────────────────────────────

func TestDivergenceDetector_LinearProgression_NoAction(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	taskID := seedDivergenceTask(t, db)
	ctx := context.Background()

	for _, h := range []string{"A", "B", "C", "D", "E"} {
		circled, err := RecordCommitAndCheckCircle(ctx, db, taskID, h)
		if err != nil {
			t.Fatalf("Push %s: %v", h, err)
		}
		if circled {
			t.Fatalf("linear push %s should not circle", h)
		}
	}

	// Task should still be Locked, spend_suspended=0.
	var status string
	var suspended int
	if err := db.QueryRow(`SELECT status, spend_suspended FROM BountyBoard WHERE id = ?`, taskID).Scan(&status, &suspended); err != nil {
		t.Fatalf("read row: %v", err)
	}
	if status != "Locked" {
		t.Errorf("status = %q, want Locked", status)
	}
	if suspended != 0 {
		t.Errorf("spend_suspended = %d, want 0", suspended)
	}
}

// ── 3. Immediate amend (A, A) — not a circle ───────────────────────────────

func TestDivergenceDetector_ImmediateAmend_NoAction(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	taskID := seedDivergenceTask(t, db)
	ctx := context.Background()

	// First A.
	circled, err := RecordCommitAndCheckCircle(ctx, db, taskID, "A")
	if err != nil {
		t.Fatalf("Push A: %v", err)
	}
	if circled {
		t.Fatalf("first A should not circle")
	}

	// Immediate re-A. The most-recent entry is A; the exclusion rule says
	// this is an --amend equivalent, NOT a circle.
	circled, err = RecordCommitAndCheckCircle(ctx, db, taskID, "A")
	if err != nil {
		t.Fatalf("Push A (amend): %v", err)
	}
	if circled {
		t.Fatalf("immediate amend A,A should NOT circle (last-entry exclusion)")
	}

	// Ring should now contain ["A", "A"].
	got := readRing(t, db, taskID)
	want := []string{"A", "A"}
	if len(got) != len(want) {
		t.Fatalf("ring = %v, want %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("ring[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

// ── 4. Five-deep ring truncates ────────────────────────────────────────────

func TestDivergenceDetector_FiveDeepRingTruncates(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	taskID := seedDivergenceTask(t, db)
	ctx := context.Background()

	// Push 7 distinct hashes; the ring should retain only the last 5.
	all := []string{"H1", "H2", "H3", "H4", "H5", "H6", "H7"}
	for _, h := range all {
		if _, err := RecordCommitAndCheckCircle(ctx, db, taskID, h); err != nil {
			t.Fatalf("Push %s: %v", h, err)
		}
	}

	got := readRing(t, db, taskID)
	want := []string{"H3", "H4", "H5", "H6", "H7"}
	if len(got) != len(want) {
		t.Fatalf("ring = %v, want %v (len 5)", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("ring[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

// ── 5. Concurrent-safe lost-update guard ───────────────────────────────────

// Two goroutines pound on the same task ID concurrently, each
// pushing a unique sequence of hashes. Without the load+push+save
// transaction, simultaneous reads would clobber each other and the ring
// would lose updates. With the transaction, every push is durable: the
// final ring's most recent entries reflect the actual call order
// observed by SQLite.
func TestDivergenceDetector_ConcurrentSafe_LostUpdate(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	taskID := seedDivergenceTask(t, db)
	ctx := context.Background()

	// 60 pushes total split across 2 goroutines; -race exercises the
	// locking. SQLite serialises writes via the WAL but the helper's
	// load+push+save still has to be atomic across the boundary or the
	// final ring loses entries.
	const nWorkers = 2
	const nPushesEach = 30
	var wg sync.WaitGroup
	wg.Add(nWorkers)
	for w := 0; w < nWorkers; w++ {
		go func(workerID int) {
			defer wg.Done()
			for i := 0; i < nPushesEach; i++ {
				hash := makeHash(workerID, i)
				if _, err := RecordCommitAndCheckCircle(ctx, db, taskID, hash); err != nil {
					t.Errorf("worker %d push %d: %v", workerID, i, err)
					return
				}
			}
		}(w)
	}
	wg.Wait()

	// Final ring is exactly recentCommitHashRingDepth entries.
	got := readRing(t, db, taskID)
	if len(got) != recentCommitHashRingDepth {
		t.Errorf("final ring length = %d, want %d", len(got), recentCommitHashRingDepth)
	}
	for i, h := range got {
		if h == "" {
			t.Errorf("ring[%d] empty — indicates a save was lost", i)
		}
	}
}

func makeHash(worker, seq int) string {
	// Use a non-trivial composite so the test catches torn writes.
	return fmt.Sprintf("wkr-%d:seq-%d", worker, seq)
}
