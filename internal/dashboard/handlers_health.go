// D3 P6A.2 — /api/dashboard/health.
//
// Returns the most recent DashboardHealthHeartbeats row plus a fresh/stale
// classification. The SPA uses this on every page load to decide whether
// to display the yellow heartbeat banner.
package dashboard

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"time"
)

func handleDashboardHealth(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		jsonCORS(w)

		row, err := LatestHeartbeat(db)
		if errors.Is(err, sql.ErrNoRows) {
			// No heartbeat yet — the goroutine hasn't ticked. Treat as
			// stale; the operator should see the banner so they know
			// the dashboard process just started.
			out := HeartbeatStatus{
				Fresh:   false,
				Message: "no heartbeat yet — dashboard just started",
			}
			_ = json.NewEncoder(w).Encode(out)
			return
		}
		if err != nil {
			http.Error(w, `{"error":"heartbeat lookup failed"}`, http.StatusInternalServerError)
			return
		}

		out := EvaluateHeartbeat(row, time.Now())
		_ = json.NewEncoder(w).Encode(out)
	}
}
