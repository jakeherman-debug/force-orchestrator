// D3 P6A.2 — `force dashboard status` CLI.
//
// Reads the most recent DashboardHealthHeartbeats row directly from the
// holocron DB (does NOT touch the live dashboard process) and exits 0
// (fresh, < 60s) or 1 (stale, >= 60s). Cron / monitoring scripts can
// surface a silent dashboard restart by polling this command.
package main

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"time"

	"force-orchestrator/internal/dashboard"
)

// cmdDashboardStatus is the entry point for `force dashboard status`.
// Returns the exit code so main.go can route through os.Exit at the
// appropriate moment.
func cmdDashboardStatus(db *sql.DB) int {
	row, err := dashboard.LatestHeartbeat(db)
	if errors.Is(err, sql.ErrNoRows) {
		fmt.Fprintln(os.Stderr, "STALE — no heartbeat ever recorded (dashboard process never ran or DB drifted)")
		return 1
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}

	status := dashboard.EvaluateHeartbeat(row, time.Now())
	if status.Fresh {
		fmt.Printf("OK — %s (pid=%d bind=%s in_flight=%d)\n",
			status.Message, row.ProcessPID, row.BindAddr, row.InFlightRequest)
		return 0
	}
	fmt.Fprintf(os.Stderr, "%s (pid=%d bind=%s)\n",
		status.Message, row.ProcessPID, row.BindAddr)
	return 1
}
