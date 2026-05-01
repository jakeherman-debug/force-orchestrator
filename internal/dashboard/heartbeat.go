// Package dashboard heartbeat (D3 P6A.2).
//
// A separate goroutine in the dashboard process inserts into
// DashboardHealthHeartbeats(ticked_at, process_pid, bind_addr,
// in_flight_requests) every 30s. On every page render, the SPA queries
// /api/dashboard/health and shows a yellow banner if the most recent row
// is older than 60s — i.e., the dashboard process may have just
// restarted.
//
// `force dashboard status` (CLI) reads the same row and exits 0 (fresh)
// or 1 (stale) so cron / monitoring scripts can spot a silent restart.
package dashboard

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"os"
	"sync/atomic"
	"time"

	"force-orchestrator/internal/store"
)

const (
	// HeartbeatInterval is how often the heartbeat goroutine ticks. The brief
	// fixes this at 30s; the constant is exported for tests that want to
	// tighten the loop.
	HeartbeatInterval = 30 * time.Second
	// HeartbeatStaleAfter is how old a heartbeat row may be before the
	// dashboard banner flips to "stale". 2× the tick is the brief's rule of
	// thumb (60s).
	HeartbeatStaleAfter = 60 * time.Second
)

// inFlightRequests tracks the count of currently-in-flight HTTP requests.
// The middleware bumps it before/after each handler call; the heartbeat
// goroutine snapshots it into the latest row. atomic so the tick goroutine
// and request goroutines never race on it.
var inFlightRequests int64

// IncInFlight is called by the security middleware before delegating to
// the next handler. Pairs with DecInFlight on response.
func IncInFlight() { atomic.AddInt64(&inFlightRequests, 1) }

// DecInFlight is called when an HTTP request completes (deferred from
// IncInFlight's call site).
func DecInFlight() { atomic.AddInt64(&inFlightRequests, -1) }

// inFlightSnapshot returns the current in-flight count. Used by the
// heartbeat goroutine.
func inFlightSnapshot() int64 { return atomic.LoadInt64(&inFlightRequests) }

// StartHeartbeat spawns the heartbeat goroutine. The goroutine respects
// ctx cancellation per the daemon-context-threading invariant. Errors
// during the insert are logged with a [DASHBOARD-HEARTBEAT] prefix and
// the loop continues — a transient SQLite error must not silence the
// banner permanently.
func StartHeartbeat(ctx context.Context, db *sql.DB, bindAddr string) {
	go heartbeatLoop(ctx, db, bindAddr, HeartbeatInterval)
}

// startHeartbeatWithInterval is the test-friendly entry point.
func startHeartbeatWithInterval(ctx context.Context, db *sql.DB, bindAddr string, interval time.Duration) {
	go heartbeatLoop(ctx, db, bindAddr, interval)
}

func heartbeatLoop(ctx context.Context, db *sql.DB, bindAddr string, interval time.Duration) {
	pid := os.Getpid()
	// Tick once immediately on startup so the dashboard is "fresh" even
	// before the first interval elapses.
	if err := writeHeartbeat(db, pid, bindAddr); err != nil {
		log.Printf("[DASHBOARD-HEARTBEAT] initial insert failed: %v", err)
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := writeHeartbeat(db, pid, bindAddr); err != nil {
				log.Printf("[DASHBOARD-HEARTBEAT] tick insert failed: %v", err)
			}
		}
	}
}

func writeHeartbeat(db *sql.DB, pid int, bindAddr string) error {
	_, err := db.Exec(`INSERT INTO DashboardHealthHeartbeats
		(ticked_at, process_pid, bind_addr, in_flight_requests)
		VALUES (?, ?, ?, ?)`,
		store.NowSQLite(), pid, bindAddr, inFlightSnapshot())
	return err
}

// HeartbeatRow is the shape of a single DashboardHealthHeartbeats row.
type HeartbeatRow struct {
	ID              int64
	TickedAt        time.Time
	ProcessPID      int
	BindAddr        string
	InFlightRequest int
}

// LatestHeartbeat returns the most recent heartbeat row, or
// (HeartbeatRow{}, sql.ErrNoRows) if the table has never been written
// to. The CLI command `force dashboard status` consumes this.
func LatestHeartbeat(db *sql.DB) (HeartbeatRow, error) {
	var (
		row      HeartbeatRow
		tickedAt string
	)
	err := db.QueryRow(`SELECT id, ticked_at, IFNULL(process_pid, 0),
			IFNULL(bind_addr, ''), IFNULL(in_flight_requests, 0)
		FROM DashboardHealthHeartbeats
		ORDER BY id DESC LIMIT 1`).
		Scan(&row.ID, &tickedAt, &row.ProcessPID, &row.BindAddr, &row.InFlightRequest)
	if err != nil {
		return HeartbeatRow{}, err
	}
	parsed, perr := store.ParseSQLiteTime(tickedAt)
	if perr != nil {
		return HeartbeatRow{}, fmt.Errorf("parse ticked_at %q: %w", tickedAt, perr)
	}
	row.TickedAt = parsed
	return row, nil
}

// HeartbeatStatus is the JSON payload returned by /api/dashboard/health.
type HeartbeatStatus struct {
	Fresh           bool   `json:"fresh"`
	LastTickedAt    string `json:"last_ticked_at"`
	AgeSeconds      int64  `json:"age_seconds"`
	ProcessPID      int    `json:"process_pid"`
	BindAddr        string `json:"bind_addr"`
	InFlightRequest int    `json:"in_flight_requests"`
	Message         string `json:"message"`
}

// EvaluateHeartbeat builds the user-facing status from a HeartbeatRow.
// Centralised so the API handler and the CLI emit identical wording.
func EvaluateHeartbeat(row HeartbeatRow, now time.Time) HeartbeatStatus {
	age := now.Sub(row.TickedAt)
	if age < 0 {
		age = 0
	}
	fresh := age <= HeartbeatStaleAfter
	msg := fmt.Sprintf("ticked %ds ago", int(age.Seconds()))
	if !fresh {
		msg = fmt.Sprintf("STALE — last tick %ds ago (threshold %ds)",
			int(age.Seconds()), int(HeartbeatStaleAfter.Seconds()))
	}
	return HeartbeatStatus{
		Fresh:           fresh,
		LastTickedAt:    row.TickedAt.UTC().Format(time.RFC3339),
		AgeSeconds:      int64(age.Seconds()),
		ProcessPID:      row.ProcessPID,
		BindAddr:        row.BindAddr,
		InFlightRequest: row.InFlightRequest,
		Message:         msg,
	}
}
