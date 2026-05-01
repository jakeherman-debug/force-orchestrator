package dashboard

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"force-orchestrator/internal/store"
)

// TestDashboardHeartbeat — D3 P6A.2 — exercises the heartbeat goroutine
// end-to-end: it ticks at the requested interval, writes rows into
// DashboardHealthHeartbeats, and LatestHeartbeat returns the freshest row.
func TestDashboardHeartbeat(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// 50ms tick so the test resolves quickly.
	startHeartbeatWithInterval(ctx, db, "127.0.0.1:8080", 50*time.Millisecond)

	// Wait for at least three ticks (initial tick + two interval ticks).
	deadline := time.Now().Add(2 * time.Second)
	for {
		var n int
		_ = db.QueryRow(`SELECT COUNT(*) FROM DashboardHealthHeartbeats`).Scan(&n)
		if n >= 3 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("heartbeat goroutine did not tick three times within 2s; row count=%d", n)
		}
		time.Sleep(20 * time.Millisecond)
	}

	row, err := LatestHeartbeat(db)
	if err != nil {
		t.Fatalf("LatestHeartbeat: %v", err)
	}
	if row.BindAddr != "127.0.0.1:8080" {
		t.Errorf("BindAddr = %q, want 127.0.0.1:8080", row.BindAddr)
	}
	if row.ProcessPID == 0 {
		t.Errorf("ProcessPID is 0; expected the test process pid")
	}

	// Fresh classification: the row was just written.
	status := EvaluateHeartbeat(row, time.Now())
	if !status.Fresh {
		t.Errorf("just-written heartbeat classified as stale: %+v", status)
	}

	// Stale classification: artificially age the row.
	stale := EvaluateHeartbeat(row, time.Now().Add(2*time.Minute))
	if stale.Fresh {
		t.Errorf("aged-by-2min heartbeat classified as fresh: %+v", stale)
	}
}

// TestHeartbeatStartsImmediately — first heartbeat is written before the
// interval elapses. The brief calls this out: the dashboard must look
// fresh on startup so reload doesn't show the banner spuriously.
func TestHeartbeatStartsImmediately(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// 1-hour interval — if the immediate tick doesn't fire, the row count
	// stays at 0 and the test fails.
	startHeartbeatWithInterval(ctx, db, "127.0.0.1:8080", time.Hour)

	deadline := time.Now().Add(2 * time.Second)
	for {
		var n int
		_ = db.QueryRow(`SELECT COUNT(*) FROM DashboardHealthHeartbeats`).Scan(&n)
		if n >= 1 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("immediate-tick contract violated: no row after 2s")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestHeartbeatHonoursContextCancellation — when ctx is cancelled the
// goroutine returns and stops writing rows. Critical for the
// daemon-context-threading invariant.
func TestHeartbeatHonoursContextCancellation(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	ctx, cancel := context.WithCancel(context.Background())
	startHeartbeatWithInterval(ctx, db, "127.0.0.1:8080", 30*time.Millisecond)

	// Wait for first ticks.
	time.Sleep(120 * time.Millisecond)
	cancel()
	time.Sleep(50 * time.Millisecond)

	var before int
	_ = db.QueryRow(`SELECT COUNT(*) FROM DashboardHealthHeartbeats`).Scan(&before)
	// Wait long enough that, if still running, several more rows would land.
	time.Sleep(250 * time.Millisecond)
	var after int
	_ = db.QueryRow(`SELECT COUNT(*) FROM DashboardHealthHeartbeats`).Scan(&after)

	// Allow at most one row to leak (the one-final-tick case if cancellation
	// raced with a tick). Anything beyond that means the goroutine ignored
	// the cancel.
	if after-before > 1 {
		t.Errorf("heartbeat goroutine did not stop on ctx cancel: rows added after cancel = %d", after-before)
	}
}

// TestDashboardHealthHandler — /api/dashboard/health returns a JSON
// payload with `fresh` true when the heartbeat is fresh, false when
// stale. Stale is forced via a manual SQL update to keep the test fast.
func TestDashboardHealthHandler(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	// No row → "no heartbeat yet" branch.
	t.Run("no rows", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/dashboard/health", nil)
		rr := httptest.NewRecorder()
		handleDashboardHealth(db)(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("got status %d, want 200", rr.Code)
		}
		var payload HeartbeatStatus
		if err := json.Unmarshal(rr.Body.Bytes(), &payload); err != nil {
			t.Fatalf("json unmarshal: %v", err)
		}
		if payload.Fresh {
			t.Errorf("no-rows path classified as fresh")
		}
	})

	// Insert a fresh row.
	t.Run("fresh row", func(t *testing.T) {
		_, err := db.Exec(`INSERT INTO DashboardHealthHeartbeats (ticked_at, process_pid, bind_addr) VALUES (?, ?, ?)`,
			store.NowSQLite(), 12345, "127.0.0.1:9999")
		if err != nil {
			t.Fatalf("seed insert: %v", err)
		}
		req := httptest.NewRequest(http.MethodGet, "/api/dashboard/health", nil)
		rr := httptest.NewRecorder()
		handleDashboardHealth(db)(rr, req)
		var payload HeartbeatStatus
		if err := json.Unmarshal(rr.Body.Bytes(), &payload); err != nil {
			t.Fatalf("json unmarshal: %v", err)
		}
		if !payload.Fresh {
			t.Errorf("just-inserted heartbeat classified stale: %+v", payload)
		}
		if payload.ProcessPID != 12345 {
			t.Errorf("ProcessPID=%d, want 12345", payload.ProcessPID)
		}
	})

	// Force stale by reaching back 5 minutes.
	t.Run("stale row", func(t *testing.T) {
		_, err := db.Exec(`UPDATE DashboardHealthHeartbeats SET ticked_at = datetime('now', '-5 minutes')
			WHERE id = (SELECT MAX(id) FROM DashboardHealthHeartbeats)`)
		if err != nil {
			t.Fatalf("age update: %v", err)
		}
		req := httptest.NewRequest(http.MethodGet, "/api/dashboard/health", nil)
		rr := httptest.NewRecorder()
		handleDashboardHealth(db)(rr, req)
		var payload HeartbeatStatus
		if err := json.Unmarshal(rr.Body.Bytes(), &payload); err != nil {
			t.Fatalf("json unmarshal: %v", err)
		}
		if payload.Fresh {
			t.Errorf("aged-by-5min heartbeat classified fresh: %+v", payload)
		}
		if payload.AgeSeconds < 60 {
			t.Errorf("AgeSeconds=%d, want >=60", payload.AgeSeconds)
		}
	})
}

// TestHeartbeatInFlightTracking — the in-flight counter increments on
// each request and decrements on completion. The next heartbeat row
// captures the live snapshot.
func TestHeartbeatInFlightTracking(t *testing.T) {
	// Reset the counter (test isolation).
	for inFlightSnapshot() > 0 {
		DecInFlight()
	}
	if got := inFlightSnapshot(); got != 0 {
		t.Fatalf("baseline in_flight=%d, want 0", got)
	}
	IncInFlight()
	IncInFlight()
	if got := inFlightSnapshot(); got != 2 {
		t.Fatalf("after 2 incs, in_flight=%d, want 2", got)
	}
	DecInFlight()
	if got := inFlightSnapshot(); got != 1 {
		t.Fatalf("after dec, in_flight=%d, want 1", got)
	}
	DecInFlight()
}
