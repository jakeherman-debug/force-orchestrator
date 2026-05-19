package main

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"force-orchestrator/internal/daemon/wake"
	"force-orchestrator/internal/forcepath"
	"force-orchestrator/internal/notify"
	"force-orchestrator/internal/store"
)

// installTestNotifyConfig wires a minimal notify.Config so Dispatch
// doesn't return ErrNoConfig in tests. The system_event category is
// included since reconcilePostWake routes through it.
func installTestNotifyConfig(t *testing.T) func() {
	t.Helper()
	cfg, err := notify.ParseConfig([]byte(`
version: 1
categories:
  system_event:
    tier: 2
    default: mail
    description: D12 P2 daemon lifecycle test category
presets:
  default:
    description: defaults
    rules: tier_defaults
`), "test.yaml")
	if err != nil {
		t.Fatalf("ParseConfig: %v", err)
	}
	prev := notify.GetGlobalConfig()
	notify.SetGlobalConfig(cfg)
	return func() { notify.SetGlobalConfig(prev) }
}

// insertLockedTask inserts a BountyBoard row in `Locked` state for the
// reconcile sweep to find. Returns the row id.
func insertLockedTask(t *testing.T, db *sql.DB) int64 {
	t.Helper()
	res, err := db.Exec(`INSERT INTO BountyBoard (type, owner, status, locked_at)
		VALUES (?, ?, 'Locked', ?)`, "TestType", "test-owner", store.NowSQLite())
	if err != nil {
		t.Fatalf("insert Locked task: %v", err)
	}
	id, _ := res.LastInsertId()
	return id
}

// taskStatus returns the current status of a BountyBoard row.
func taskStatus(t *testing.T, db *sql.DB, id int64) string {
	t.Helper()
	var s string
	if err := db.QueryRow(`SELECT status FROM BountyBoard WHERE id = ?`, id).Scan(&s); err != nil {
		t.Fatalf("read task %d status: %v", id, err)
	}
	return s
}

// TestReconcilePostWake_HappyPath asserts the four reconciliation
// steps run cleanly on a healthy DB.
func TestReconcilePostWake_HappyPath(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	restore := installTestNotifyConfig(t)
	defer restore()

	id := insertLockedTask(t, db)

	if err := reconcilePostWake(context.Background(), db); err != nil {
		t.Fatalf("reconcilePostWake: %v", err)
	}

	// Locked task swept back to Pending.
	if got := taskStatus(t, db, id); got != "Pending" {
		t.Errorf("after reconcile: task status = %q, want Pending", got)
	}

	// system_event mail row landed.
	var mailCount int
	if err := db.QueryRow(`SELECT COUNT(*) FROM Fleet_Mail WHERE subject LIKE '[D11/system_event]%'`).Scan(&mailCount); err != nil {
		t.Fatalf("count system_event mail: %v", err)
	}
	if mailCount == 0 {
		t.Errorf("after reconcile: expected at least one system_event mail row, got 0")
	}
}

// TestReconcilePostWake_Idempotent is the correctness gate from the
// spec: running reconcilePostWake N times in a row produces the same
// outcome as running it once. We assert that the BountyBoard state
// stabilises after the first call (no row-state oscillation), which
// is the property the daemon actually relies on.
//
// Note: each call DOES emit a Fleet_Mail row (intentional — operators
// want to see "daemon resumed" pings). That's not a state mutation
// that affects the reconciler's idempotence guarantee, since the next
// reconciler run doesn't read mail rows. The spec's idempotence claim
// is about the reconciliation steps themselves, not the side-effect
// notification.
func TestReconcilePostWake_Idempotent(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	restore := installTestNotifyConfig(t)
	defer restore()

	id1 := insertLockedTask(t, db)
	id2 := insertLockedTask(t, db)

	for i := 0; i < 3; i++ {
		if err := reconcilePostWake(context.Background(), db); err != nil {
			t.Fatalf("reconcilePostWake (run %d): %v", i+1, err)
		}
	}

	// All Locked rows are Pending (nothing got pushed back to Locked).
	if got := taskStatus(t, db, id1); got != "Pending" {
		t.Errorf("task %d status = %q, want Pending", id1, got)
	}
	if got := taskStatus(t, db, id2); got != "Pending" {
		t.Errorf("task %d status = %q, want Pending", id2, got)
	}

	// Subsequent reconciler runs find no Locked rows to release —
	// release count is 0 on calls 2 and 3. We verify that property
	// indirectly by counting any rows still in Locked (must be 0).
	var stillLocked int
	if err := db.QueryRow(`SELECT COUNT(*) FROM BountyBoard WHERE status = 'Locked'`).Scan(&stillLocked); err != nil {
		t.Fatalf("count Locked: %v", err)
	}
	if stillLocked != 0 {
		t.Errorf("after 3x reconcile: %d row(s) still in Locked state — sweep not idempotent", stillLocked)
	}
}

// TestReconcilePostWake_LockedTaskSweep zeroes in on the
// ReleaseInFlightTasks contract — Locked, UnderReview, and
// UnderCaptainReview rows are all swept back to Pending; rows in any
// other state are left alone.
func TestReconcilePostWake_LockedTaskSweep(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	restore := installTestNotifyConfig(t)
	defer restore()

	cases := []struct {
		state      string
		wantSwept  bool
	}{
		{"Locked", true},
		{"UnderReview", true},
		{"UnderCaptainReview", true},
		{"Pending", false},
		{"Completed", false},
		{"Failed", false},
	}
	ids := make(map[string]int64)
	for _, c := range cases {
		res, err := db.Exec(`INSERT INTO BountyBoard (type, owner, status, locked_at)
			VALUES (?, ?, ?, ?)`, "TestType", "test-owner", c.state, store.NowSQLite())
		if err != nil {
			t.Fatalf("insert %s task: %v", c.state, err)
		}
		id, _ := res.LastInsertId()
		ids[c.state] = id
	}

	if err := reconcilePostWake(context.Background(), db); err != nil {
		t.Fatalf("reconcilePostWake: %v", err)
	}

	for _, c := range cases {
		got := taskStatus(t, db, ids[c.state])
		if c.wantSwept {
			if got != "Pending" {
				t.Errorf("task in %s: post-reconcile status = %q, want Pending (should have been swept)", c.state, got)
			}
		} else {
			if got != c.state {
				t.Errorf("task in %s: post-reconcile status = %q, want %s (should NOT have been swept)", c.state, got, c.state)
			}
		}
	}
}

// TestReconcilePostWake_AttentionMarkerClear documents that
// "agent-attention markers" are not a real concept in this codebase
// (no AgentAttention table — only OperatorAttentionTags, which is
// operator-pinned, not stale-after-sleep). The spec called out a
// step to clear them; we verify the alternative invariant: the
// reconciler does NOT touch OperatorAttentionTags rows (operator
// state is preserved across sleep/wake).
func TestReconcilePostWake_AttentionMarkerClear(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	restore := installTestNotifyConfig(t)
	defer restore()

	ctx := context.Background()
	if err := store.SetAttentionTag(ctx, db, store.AttentionTag{
		OperatorEmail:  "operator@example.com",
		TargetKind:     "convoy",
		TargetID:       "42",
		AttentionLevel: string(store.AttentionFollowing),
	}); err != nil {
		t.Fatalf("SetAttentionTag: %v", err)
	}

	if err := reconcilePostWake(ctx, db); err != nil {
		t.Fatalf("reconcilePostWake: %v", err)
	}

	// Operator attention tag survives reconciliation.
	tags, err := store.ListAttentionTags(ctx, db, "operator@example.com")
	if err != nil {
		t.Fatalf("ListAttentionTags: %v", err)
	}
	if len(tags) != 1 {
		t.Errorf("after reconcile: operator attention tags = %d, want 1 (operator pins must survive sleep/wake)", len(tags))
	}
}

// TestReconcilePostWakeLoop_DispatchesGoingToSleep + Woke confirm the
// goroutine driver routes events to the correct branches.
//
// We don't verify reconcilePostWake's full behaviour here (the unit
// tests above cover that). We just confirm that:
//
//   - GoingToSleep events are consumed without panicking and without
//     calling the reconciler (we'd see no Pending sweep).
//   - Woke events trigger reconcilePostWake (we'd see the Locked row
//     swept).
//   - Channel close cleanly exits the loop.
func TestReconcilePostWakeLoop_RoutesEvents(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	restore := installTestNotifyConfig(t)
	defer restore()

	id := insertLockedTask(t, db)

	events := make(chan wake.Event, 4)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})

	go func() {
		reconcilePostWakeLoop(ctx, db, events)
		close(done)
	}()

	// First fire a GoingToSleep — the row should still be Locked (no
	// reconciliation runs on this branch).
	events <- wake.GoingToSleep
	// Give the goroutine a moment to consume.
	time.Sleep(50 * time.Millisecond)
	if got := taskStatus(t, db, id); got != "Locked" {
		t.Errorf("after GoingToSleep: task status = %q, want still-Locked (sleep branch shouldn't sweep)", got)
	}

	// Now fire Woke — reconcilePostWake runs and sweeps the row.
	events <- wake.Woke
	// Wait for reconciliation. The reconciler is fast (in-memory DB),
	// but we poll up to 2s to avoid flake.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if taskStatus(t, db, id) == "Pending" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if got := taskStatus(t, db, id); got != "Pending" {
		t.Errorf("after Woke: task status = %q, want Pending", got)
	}

	// Channel close must cleanly exit the loop.
	close(events)
	select {
	case <-done:
		// expected
	case <-time.After(2 * time.Second):
		cancel()
		t.Fatalf("reconcilePostWakeLoop did not exit after channel close")
	}
	cancel()
}

// TestReconcilePostWakeLoop_CtxCancel confirms ctx cancellation also
// exits the loop cleanly.
func TestReconcilePostWakeLoop_CtxCancel(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	events := make(chan wake.Event, 4)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})

	go func() {
		reconcilePostWakeLoop(ctx, db, events)
		close(done)
	}()

	cancel()
	select {
	case <-done:
		// expected
	case <-time.After(2 * time.Second):
		t.Fatalf("reconcilePostWakeLoop did not exit after ctx cancel")
	}
}

// TestReconcilePostWake_DBDeadReturnsSentinel verifies that a closed
// DB returns the ErrPostWakeDBDead sentinel so the goroutine driver
// can log.Fatalf on it. The loop driver itself (which calls
// log.Fatalf) is exercised at compile time only — we never let it
// run in tests.
func TestReconcilePostWake_DBDeadReturnsSentinel(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	restore := installTestNotifyConfig(t)
	defer restore()

	// Close the DB so PingContext fails.
	db.Close()

	err := reconcilePostWake(context.Background(), db)
	if err == nil {
		t.Fatalf("reconcilePostWake on closed DB: got nil error, want ErrPostWakeDBDead")
	}
	if !errors.Is(err, ErrPostWakeDBDead) {
		t.Errorf("reconcilePostWake on closed DB: err = %v, want ErrPostWakeDBDead", err)
	}
}

// TestReconcilePostWakeLoop_GoingToSleepSnapshots is the D12 P3
// integration: a GoingToSleep event triggers store.SnapshotHolocron
// against the live DB. We use a file-backed DB so VACUUM INTO has
// real bytes to copy, and FORCE_DIR pointed at a temp dir so the
// snapshot lands somewhere we can stat. The reconcile branch on
// Woke also runs but is not the focus here.
func TestReconcilePostWakeLoop_GoingToSleepSnapshots(t *testing.T) {
	// Point FORCE_DIR + holocron at a temp dir. forcepath caches its
	// resolved Dir() result across the entire test binary, so we MUST
	// reset the cache after setting FORCE_DIR and on cleanup — otherwise
	// a sibling test that called forcepath.Dir() first pins ~/.force/
	// and our snapshot lands in the operator's real backups directory.
	tmpDir := t.TempDir()
	prevDir := os.Getenv("FORCE_DIR")
	os.Setenv("FORCE_DIR", tmpDir)
	forcepath.ResetDirCacheForTests()
	defer func() {
		if prevDir == "" {
			os.Unsetenv("FORCE_DIR")
		} else {
			os.Setenv("FORCE_DIR", prevDir)
		}
		forcepath.ResetDirCacheForTests()
	}()

	dbPath := tmpDir + "/holocron.db"
	db := store.InitHolocronDSN(dbPath + "?_busy_timeout=5000&_journal_mode=WAL")
	defer db.Close()

	events := make(chan wake.Event, 2)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})

	go func() {
		reconcilePostWakeLoop(ctx, db, events)
		close(done)
	}()

	events <- wake.GoingToSleep

	// Poll for the snapshot file under tmpDir/backups/.
	backupsDir := tmpDir + "/backups"
	deadline := time.Now().Add(2 * time.Second)
	var found bool
	for time.Now().Before(deadline) {
		entries, err := os.ReadDir(backupsDir)
		if err == nil {
			for _, e := range entries {
				if !e.IsDir() && strings.HasPrefix(e.Name(), "snapshot-pre-sleep-") {
					found = true
					break
				}
			}
		}
		if found {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !found {
		t.Errorf("GoingToSleep did not produce a snapshot under %s within 2s", backupsDir)
	}

	close(events)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		cancel()
		t.Fatalf("reconcilePostWakeLoop did not exit after channel close")
	}
}
