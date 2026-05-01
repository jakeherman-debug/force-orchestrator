// D3 P6A.6 — Trust dials API.
package dashboard

import (
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"force-orchestrator/internal/store"
)

func handleTrustDials(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		jsonCORS(w)
		if r.Method != http.MethodGet {
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
			return
		}
		op := r.URL.Query().Get("operator")
		if op == "" {
			op = "default@operator"
		}
		// Auto-bootstrap on first GET so the panel never renders "missing".
		_ = store.BootstrapTrustDials(r.Context(), db, op)
		dials, err := store.ListCurrentTrustDials(r.Context(), db, op)
		if err != nil {
			http.Error(w, `{"error":"list trust dials failed"}`, http.StatusInternalServerError)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"dials": dials})
	}
}

func handleTrustDialUpsert(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		jsonCORS(w)
		if r.Method != http.MethodPut {
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
			return
		}
		// /api/trust-dials/<agent>
		agent := strings.TrimPrefix(r.URL.Path, "/api/trust-dials/")
		if agent == "" || strings.Contains(agent, "/") {
			http.Error(w, `{"error":"path: /api/trust-dials/<agent>"}`, http.StatusBadRequest)
			return
		}

		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, `{"error":"body read failed"}`, http.StatusRequestEntityTooLarge)
			return
		}
		var in struct {
			OperatorEmail string `json:"operator_email"`
			DialValue     int    `json:"dial_value"`
			Rationale     string `json:"rationale"`
		}
		if err := json.Unmarshal(body, &in); err != nil {
			http.Error(w, `{"error":"bad JSON"}`, http.StatusBadRequest)
			return
		}
		if in.OperatorEmail == "" {
			in.OperatorEmail = "default@operator"
		}
		if err := store.SetTrustDial(r.Context(), db, store.TrustDial{
			OperatorEmail: in.OperatorEmail,
			Agent:         agent,
			DialValue:     in.DialValue,
			SetBy:         string(store.TrustDialOperator),
			Rationale:     in.Rationale,
		}); err != nil {
			http.Error(w, `{"error":"set failed: `+err.Error()+`"}`, http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}
