// D3 P6A.13 — Cooldown API.
package dashboard

import (
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"

	"force-orchestrator/internal/agents"
)

func handleCooldownList(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		jsonCORS(w)
		pending, err := agents.ListPendingCooldowns(r.Context(), db)
		if err != nil {
			http.Error(w, `{"error":"list failed"}`, http.StatusInternalServerError)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"cooldowns": pending})
	}
}

func handleCooldownAction(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		jsonCORS(w)
		// /api/cooldown/<id>/<action>
		path := strings.TrimPrefix(r.URL.Path, "/api/cooldown/")
		parts := strings.Split(path, "/")
		if len(parts) != 2 {
			http.Error(w, `{"error":"path: /api/cooldown/<id>/<action>"}`, http.StatusBadRequest)
			return
		}
		id, err := strconv.ParseInt(parts[0], 10, 64)
		if err != nil {
			http.Error(w, `{"error":"id must be integer"}`, http.StatusBadRequest)
			return
		}
		action := parts[1]

		switch action {
		case "pause":
			if r.Method != http.MethodPost {
				http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
				return
			}
			email := r.URL.Query().Get("operator")
			if email == "" {
				email = "default@operator"
			}
			if err := agents.PauseCooldown(r.Context(), db, id, email); err != nil {
				http.Error(w, `{"error":"pause failed"}`, http.StatusInternalServerError)
				return
			}
			w.WriteHeader(http.StatusNoContent)

		case "resume":
			if r.Method != http.MethodPost {
				http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
				return
			}
			body, _ := io.ReadAll(r.Body)
			var in struct{ Rationale string `json:"rationale"` }
			_ = json.Unmarshal(body, &in)
			if err := agents.ResumeCooldown(r.Context(), db, id, in.Rationale); err != nil {
				http.Error(w, `{"error":"`+err.Error()+`"}`, http.StatusBadRequest)
				return
			}
			w.WriteHeader(http.StatusNoContent)

		case "cancel":
			if r.Method != http.MethodPost {
				http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
				return
			}
			if err := agents.CancelCooldown(r.Context(), db, id); err != nil {
				http.Error(w, `{"error":"cancel failed"}`, http.StatusInternalServerError)
				return
			}
			w.WriteHeader(http.StatusNoContent)

		default:
			http.Error(w, `{"error":"unknown action"}`, http.StatusBadRequest)
		}
	}
}
