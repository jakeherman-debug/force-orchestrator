// Package dashboard — D3 P6B.7 Replay endpoints.
//
//   - POST /api/drill/replay/<kind>/<id>   trigger a replay
//   - GET  /api/drill/replay/<replay_id>   load a stored ReplayResult
//
// Pure-read on the side of original-state — only writes are
// ReplayResults + the replay's own LLMCallTranscripts row (per
// agents.ReplayDecision contract).

package dashboard

import (
	"database/sql"
	"net/http"
	"strconv"
	"strings"

	"force-orchestrator/internal/agents"
)

func handleDrillReplay(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Path: /api/drill/replay/<kind>/<id>  (POST)
		// or   /api/drill/replay/<replay_id>   (GET)
		rest := strings.TrimPrefix(r.URL.Path, "/api/drill/replay/")
		parts := strings.Split(strings.TrimSuffix(rest, "/"), "/")

		switch r.Method {
		case http.MethodPost:
			if len(parts) < 2 {
				http.Error(w, `{"error":"path: /api/drill/replay/<kind>/<id>"}`, http.StatusBadRequest)
				return
			}
			kind := parts[0]
			id, err := strconv.ParseInt(parts[1], 10, 64)
			if err != nil || id <= 0 {
				http.Error(w, `{"error":"invalid id"}`, http.StatusBadRequest)
				return
			}
			currentVersion := r.URL.Query().Get("prompt_version")
			if currentVersion == "" {
				currentVersion = "current"
			}
			operator := r.URL.Query().Get("operator")
			if operator == "" {
				operator = "default@operator"
			}
			res, err := agents.ReplayDecision(r.Context(), db, kind, id, currentVersion, operator)
			if err != nil {
				http.Error(w, `{"error":"`+err.Error()+`"}`, http.StatusBadRequest)
				return
			}
			writeJSON(w, res)
		case http.MethodGet:
			if len(parts) < 1 || parts[0] == "" {
				http.Error(w, `{"error":"missing replay id"}`, http.StatusBadRequest)
				return
			}
			id, err := strconv.ParseInt(parts[0], 10, 64)
			if err != nil || id <= 0 {
				http.Error(w, `{"error":"invalid id"}`, http.StatusBadRequest)
				return
			}
			res, err := agents.LoadReplayResult(r.Context(), db, id)
			if err != nil {
				http.Error(w, `{"error":"`+err.Error()+`"}`, http.StatusNotFound)
				return
			}
			writeJSON(w, res)
		default:
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		}
	}
}
