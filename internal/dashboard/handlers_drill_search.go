// Package dashboard — D3 P6B.6 Drill free-text search handler.
//
// GET /api/drill/search?q=<query>&kind=convoy&id=<convoy_id> (scoped)
// GET /api/drill/search?q=<query>&scope=global                  (default)
//
// Calls store.SearchDrill which runs the query against the fts5
// virtual tables built at holocron init time.

package dashboard

import (
	"database/sql"
	"net/http"
	"strconv"
	"strings"

	"force-orchestrator/internal/store"
)

func handleDrillSearch(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
			return
		}
		q := strings.TrimSpace(r.URL.Query().Get("q"))
		if q == "" {
			http.Error(w, `{"error":"q is required"}`, http.StatusBadRequest)
			return
		}
		// Cap user input length so a 10-MB search query can't pin
		// the daemon. fts5 happily accepts arbitrary input.
		if len(q) > 1024 {
			q = q[:1024]
		}
		scope := r.URL.Query().Get("scope")
		if scope == "" {
			scope = "global"
		}
		convoyID := 0
		if idStr := r.URL.Query().Get("id"); idStr != "" {
			if n, err := strconv.Atoi(idStr); err == nil && n > 0 {
				convoyID = n
				if r.URL.Query().Get("kind") == "convoy" {
					scope = "convoy"
				}
			}
		}
		limit := atoiDefault(r.URL.Query().Get("limit"), 50)
		if limit > 200 {
			limit = 200
		}

		results, err := store.SearchDrill(r.Context(), db, q, scope, convoyID, limit)
		if err != nil {
			http.Error(w, `{"error":"search failed"}`, http.StatusInternalServerError)
			return
		}
		writeJSON(w, map[string]any{
			"query":   q,
			"scope":   scope,
			"id":      convoyID,
			"results": results,
		})
	}
}
