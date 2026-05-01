// Package dashboard — D4 Phase 0 — ConflictTickets endpoint.
//
// Two operations:
//
//   - GET  /api/conflicts/tickets   → JSON list of open tickets
//   - POST /api/conflicts/tickets/{id}/resolve  (body: {"note":"…"})
//
// The list ordering matches store.ListOpenConflictTickets (newest
// first). Resolve transitions status from 'open' → 'resolved' and
// stamps resolved_at + resolution_note via store.ResolveConflictTicket.
package dashboard

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"force-orchestrator/internal/store"
)

// handleConflictsTickets is the dispatcher for /api/conflicts/tickets
// and /api/conflicts/tickets/{id}/resolve. Routing is path-suffix
// matched here rather than relying on a chi-style router so we
// stay consistent with the rest of internal/dashboard's handler
// pattern (path-suffix Sscanf parsing).
func handleConflictsTickets(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		jsonCORS(w)
		path := strings.TrimSuffix(r.URL.Path, "/")

		// Resolve subroute: /api/conflicts/tickets/{id}/resolve
		if strings.HasSuffix(path, "/resolve") {
			handleConflictsResolve(db, w, r, path)
			return
		}

		// List route: /api/conflicts/tickets
		if r.Method != http.MethodGet {
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
			return
		}
		limit := 100
		if lStr := r.URL.Query().Get("limit"); lStr != "" {
			fmt.Sscanf(lStr, "%d", &limit)
		}
		tickets, err := store.ListOpenConflictTickets(context.Background(), db, limit)
		if err != nil {
			http.Error(w, fmt.Sprintf(`{"error":%q}`, err.Error()), http.StatusInternalServerError)
			return
		}
		// Marshal explicit shape so the JSON keys are stable for the
		// dashboard front-end. Empty slice (no tickets) renders as
		// [] not null.
		out := make([]map[string]any, 0, len(tickets))
		for _, t := range tickets {
			out = append(out, map[string]any{
				"id":             t.ID,
				"memory_a_id":    t.MemoryAID,
				"memory_b_id":    t.MemoryBID,
				"reason":         t.Reason,
				"status":         t.Status,
				"created_at":     t.CreatedAt,
				"resolution":     t.ResolutionNote,
				"resolved_at":    t.ResolvedAt,
			})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"tickets": out})
	}
}

// handleConflictsResolve handles POST /api/conflicts/tickets/{id}/resolve.
func handleConflictsResolve(db *sql.DB, w http.ResponseWriter, r *http.Request, path string) {
	if r.Method != http.MethodPost {
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}
	// Path: /api/conflicts/tickets/{id}/resolve
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) < 4 {
		http.Error(w, `{"error":"missing ticket id"}`, http.StatusBadRequest)
		return
	}
	id, err := strconv.Atoi(parts[len(parts)-2])
	if err != nil || id <= 0 {
		http.Error(w, `{"error":"invalid ticket id"}`, http.StatusBadRequest)
		return
	}
	var body struct {
		Note string `json:"note"`
	}
	if r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&body)
	}
	if err := store.ResolveConflictTicket(context.Background(), db, id, body.Note); err != nil {
		http.Error(w, fmt.Sprintf(`{"error":%q}`, err.Error()), http.StatusBadRequest)
		return
	}
	store.LogAudit(db, "dashboard", "conflict-ticket-resolve", id, body.Note)
	fmt.Fprintf(w, `{"ok":true,"id":%d}`, id)
}
