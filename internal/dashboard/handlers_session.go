// D3 P6A.5 — OperatorSessionState API endpoints.
package dashboard

import (
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"force-orchestrator/internal/store"
)

func handleSessionState(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		jsonCORS(w)
		switch r.Method {
		case http.MethodGet:
			op := r.URL.Query().Get("operator")
			if op == "" {
				op = "default@operator"
			}
			s, err := store.GetOperatorSession(r.Context(), db, op)
			if errors.Is(err, sql.ErrNoRows) {
				_ = json.NewEncoder(w).Encode(map[string]any{"session": nil})
				return
			}
			if err != nil {
				http.Error(w, `{"error":"get session failed"}`, http.StatusInternalServerError)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"session": s})

		case http.MethodPut:
			body, err := io.ReadAll(r.Body)
			if err != nil {
				http.Error(w, `{"error":"body read failed"}`, http.StatusRequestEntityTooLarge)
				return
			}
			var s store.OperatorSession
			if err := json.Unmarshal(body, &s); err != nil {
				http.Error(w, `{"error":"bad JSON"}`, http.StatusBadRequest)
				return
			}
			if s.OperatorEmail == "" {
				s.OperatorEmail = "default@operator"
			}
			if err := store.SaveOperatorSession(r.Context(), db, s); err != nil {
				if errors.Is(err, store.ErrSessionPayloadTooLarge) {
					http.Error(w, `{"error":"partial_review_state_json exceeds 32KB cap"}`, http.StatusRequestEntityTooLarge)
					return
				}
				http.Error(w, `{"error":"save failed"}`, http.StatusInternalServerError)
				return
			}
			w.WriteHeader(http.StatusNoContent)

		case http.MethodDelete:
			op := r.URL.Query().Get("operator")
			if op == "" {
				op = "default@operator"
			}
			if err := store.ClearOperatorSession(r.Context(), db, op); err != nil {
				http.Error(w, `{"error":"clear failed"}`, http.StatusInternalServerError)
				return
			}
			w.WriteHeader(http.StatusNoContent)

		default:
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		}
	}
}
