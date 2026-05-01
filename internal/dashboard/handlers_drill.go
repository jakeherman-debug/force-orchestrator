// Package dashboard — D3 P6B.3-6B.5 Drill diagnostic surface.
//
// Three handlers form the substrate:
//   - GET /api/drill/convoy/:id          — unified event stream (timeline)
//   - GET /api/drill/convoy/:id/spend    — per-task/agent cost rollup
//   - GET /api/drill/task/:id            — single-task event stream
//   - GET /api/drill/event/:kind/:id     — full body for one event
//
// The handlers are read-only; no mutation, no LLM calls. Same-origin
// gating happens via securityMiddleware (Pattern P8 invariant). The
// 4-KB stdout/stderr truncation persisted by 6B.2 means even very
// large convoys serialise to a bounded JSON body.

package dashboard

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"force-orchestrator/internal/store"
)

// handleDrillConvoy serves /api/drill/convoy/<id> and
// /api/drill/convoy/<id>/spend.
func handleDrillConvoy(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
			return
		}
		// Path: /api/drill/convoy/<id>[/spend]
		rest := strings.TrimPrefix(r.URL.Path, "/api/drill/convoy/")
		parts := strings.Split(rest, "/")
		if len(parts) == 0 || parts[0] == "" {
			http.Error(w, `{"error":"missing convoy id"}`, http.StatusBadRequest)
			return
		}
		convoyID, err := strconv.Atoi(parts[0])
		if err != nil || convoyID <= 0 {
			http.Error(w, `{"error":"invalid convoy id"}`, http.StatusBadRequest)
			return
		}

		// Sub-route?
		if len(parts) >= 2 && parts[1] == "spend" {
			rows, sErr := store.LoadConvoyDrillSpend(r.Context(), db, convoyID)
			if sErr != nil {
				http.Error(w, fmt.Sprintf(`{"error":"%s"}`, sErr.Error()), http.StatusInternalServerError)
				return
			}
			writeJSON(w, map[string]any{"convoy_id": convoyID, "spend": rows})
			return
		}

		// Default: unified event stream + summary.
		limit := atoiDefault(r.URL.Query().Get("limit"), 200)
		offset := atoiDefault(r.URL.Query().Get("offset"), 0)
		events, err := store.LoadConvoyDrillEvents(r.Context(), db, convoyID, limit, offset)
		if err != nil {
			http.Error(w, fmt.Sprintf(`{"error":"%s"}`, err.Error()), http.StatusInternalServerError)
			return
		}
		writeJSON(w, map[string]any{
			"convoy_id": convoyID,
			"events":    events,
			"limit":     limit,
			"offset":    offset,
		})
	}
}

// handleDrillTask serves /api/drill/task/<id>.
func handleDrillTask(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
			return
		}
		rest := strings.TrimPrefix(r.URL.Path, "/api/drill/task/")
		parts := strings.Split(rest, "/")
		if len(parts) == 0 || parts[0] == "" {
			http.Error(w, `{"error":"missing task id"}`, http.StatusBadRequest)
			return
		}
		taskID, err := strconv.Atoi(parts[0])
		if err != nil || taskID <= 0 {
			http.Error(w, `{"error":"invalid task id"}`, http.StatusBadRequest)
			return
		}
		limit := atoiDefault(r.URL.Query().Get("limit"), 200)
		offset := atoiDefault(r.URL.Query().Get("offset"), 0)
		events, err := store.LoadTaskDrillEvents(r.Context(), db, taskID, limit, offset)
		if err != nil {
			http.Error(w, fmt.Sprintf(`{"error":"%s"}`, err.Error()), http.StatusInternalServerError)
			return
		}
		writeJSON(w, map[string]any{
			"task_id": taskID,
			"events":  events,
			"limit":   limit,
			"offset":  offset,
		})
	}
}

// handleDrillEvent serves /api/drill/event/<kind>/<id>.
func handleDrillEvent(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
			return
		}
		rest := strings.TrimPrefix(r.URL.Path, "/api/drill/event/")
		parts := strings.SplitN(rest, "/", 2)
		if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
			http.Error(w, `{"error":"missing kind or id"}`, http.StatusBadRequest)
			return
		}
		kind := parts[0]
		refIDStr := strings.TrimSuffix(parts[1], "/")
		refID, err := strconv.ParseInt(refIDStr, 10, 64)
		if err != nil || refID <= 0 {
			http.Error(w, `{"error":"invalid id"}`, http.StatusBadRequest)
			return
		}
		body, err := store.LoadEventDetails(r.Context(), db, kind, refID)
		if err != nil {
			http.Error(w, fmt.Sprintf(`{"error":"%s"}`, err.Error()), http.StatusNotFound)
			return
		}
		writeJSON(w, body)
	}
}

func atoiDefault(s string, def int) int {
	if s == "" {
		return def
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return def
	}
	return n
}

func writeJSON(w http.ResponseWriter, body any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(body)
}
