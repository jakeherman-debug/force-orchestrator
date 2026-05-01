// D3 P6A.8 — Pulse fleet panel snapshot endpoint.
package dashboard

import (
	"database/sql"
	"encoding/json"
	"net/http"

	"force-orchestrator/internal/store"
)

func handlePulseSnapshot(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		jsonCORS(w)
		op := r.URL.Query().Get("operator")
		if op == "" {
			op = "default@operator"
		}
		// Auto-bootstrap trust dials so the panel never renders missing.
		_ = store.BootstrapTrustDials(r.Context(), db, op)
		snap, err := store.PulseSnapshotFor(r.Context(), db, op)
		if err != nil {
			http.Error(w, `{"error":"snapshot failed"}`, http.StatusInternalServerError)
			return
		}
		_ = json.NewEncoder(w).Encode(snap)
	}
}
