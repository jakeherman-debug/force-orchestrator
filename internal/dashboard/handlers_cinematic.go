// D3 P6A.9 — Cinematic API.
package dashboard

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"time"

	"force-orchestrator/internal/agents"
)

func handlePulseCinematic(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		jsonCORS(w)
		// Auto-detect or accept a `since` query param.
		var since time.Time
		if s := r.URL.Query().Get("since"); s != "" {
			t, err := time.Parse(time.RFC3339, s)
			if err == nil {
				since = t
			}
		}
		if since.IsZero() {
			detected, ok := agents.DetectSleepStartedAt(r.Context(), db)
			if !ok {
				_ = json.NewEncoder(w).Encode(map[string]any{"sleep_detected": false})
				return
			}
			since = detected
		}
		out, err := agents.BuildCinematic(r.Context(), db, since)
		if err != nil {
			http.Error(w, `{"error":"build cinematic failed"}`, http.StatusInternalServerError)
			return
		}
		_ = json.NewEncoder(w).Encode(out)
	}
}
