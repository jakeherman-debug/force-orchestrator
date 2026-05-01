// D3 P6A.7 — Pulse narrative panel API.
package dashboard

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"strconv"

	"force-orchestrator/internal/agents"
)

// handlePulseNarrative — GET /api/pulse/narrative
// Returns the most recent NarrativeRenders rows for the Pulse panel.
// SSE streaming arrives in 6B; for 6A we use polling.
func handlePulseNarrative(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		jsonCORS(w)
		limit := 10
		if l := r.URL.Query().Get("limit"); l != "" {
			if n, err := strconv.Atoi(l); err == nil && n > 0 && n <= 100 {
				limit = n
			}
		}
		rows, err := agents.ListLatestNarrativeRenders(r.Context(), db, limit)
		if err != nil {
			http.Error(w, `{"error":"list narratives failed"}`, http.StatusInternalServerError)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"narratives": rows})
	}
}
