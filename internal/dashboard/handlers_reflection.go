// Package dashboard — D3 P6B.12 Reflection learning panel handler.
//
// Exposes GET /api/reflection/learning (latest rendered panel) and POST
// /api/reflection/learning/refresh (re-render now). Both routes are
// same-origin gated by the existing securityMiddleware. The refresh
// route is mutating; CLI parity is provided by `force learning refresh`
// (registered in cmd/force/main.go and visible to Pattern P25).

package dashboard

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"force-orchestrator/internal/agents"
)

// learningPanelResponse is the JSON shape the SPA consumes.
type learningPanelResponse struct {
	ID         int64    `json:"id"`
	RenderedAt string   `json:"rendered_at"`
	Prose      string   `json:"prose"`
	Sources    []string `json:"sources"`
}

// handleReflectionLearning serves both GET (read latest) and POST
// (refresh now). GET is the default; POST triggers a synchronous
// re-render and returns the new row.
func handleReflectionLearning(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		switch r.Method {
		case http.MethodGet:
			id, renderedAt, prose, sources, err := agents.LatestFleetLearningPanel(ctx, db)
			if err != nil {
				http.Error(w, `{"error":"db read failed"}`, http.StatusInternalServerError)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(learningPanelResponse{
				ID: id, RenderedAt: renderedAt, Prose: prose, Sources: sources,
			})
		case http.MethodPost:
			// "Refresh now" — re-render synchronously. Read-only side
			// effect: inserts one row in FleetLearningPanels. The
			// daily cap is the caller's contract; the renderer's
			// cost is 0 in the deterministic-synth shape.
			if !checkRefreshSegment(r) {
				http.Error(w, `{"error":"unknown sub-route"}`, http.StatusNotFound)
				return
			}
			id, err := agents.RenderFleetLearningPanel(ctx, db, time.Now())
			if err != nil {
				http.Error(w, `{"error":"render failed: `+err.Error()+`"}`, http.StatusInternalServerError)
				return
			}
			_, renderedAt, prose, sources, _ := agents.LatestFleetLearningPanel(ctx, db)
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(learningPanelResponse{
				ID: id, RenderedAt: renderedAt, Prose: prose, Sources: sources,
			})
		default:
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		}
	}
}

// checkRefreshSegment accepts both /api/reflection/learning (POST) and
// /api/reflection/learning/refresh (POST) as the refresh trigger.
func checkRefreshSegment(r *http.Request) bool {
	p := r.URL.Path
	return strings.HasSuffix(p, "/learning") || strings.HasSuffix(p, "/learning/refresh") || strings.HasSuffix(p, "/learning/")
}
