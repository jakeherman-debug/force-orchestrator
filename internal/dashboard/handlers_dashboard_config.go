// handlers_dashboard_config.go — D11 Phase 3 substrate.
//
// GET /api/dashboard/config returns the resolved dashboard
// personalization config (YAML defaults + SystemConfig overrides
// composed). Sub-tasks B and C add WRITE endpoints
// (POST /api/dashboard/config/tab/<id>, /display, /saved-filter); the
// substrate ships only the read path.
//
// Response shape (JSON):
//
//	{
//	  "tabs":          [{"id": "...", "visible": true, "order": 1, "refresh_seconds": 5}, ...],
//	  "display":       {"theme": "dark", "density": "comfortable",
//	                    "default_sort": {...}, "per_table_pagination": 50},
//	  "saved_filters": []
//	}
package dashboard

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"

	dashconfig "force-orchestrator/internal/dashboard/config"
)

// dashboardConfigResponse is the wire shape for GET /api/dashboard/config.
// Mirrors dashconfig types but pinned at the boundary so the SPA wire
// contract is independent of in-process struct shape.
type dashboardConfigResponse struct {
	Tabs         []dashconfig.TabConfig    `json:"tabs"`
	Display      dashconfig.DisplayConfig  `json:"display"`
	SavedFilters []dashconfig.SavedFilter  `json:"saved_filters"`
}

// handleDashboardConfig — GET /api/dashboard/config.
//
// Returns the resolved per-operator dashboard personalization. Read
// path only; the SPA mutates state via the (yet-to-land) sub-task B/C
// write endpoints.
//
// Method-gated: anything other than GET returns 405 with a JSON error
// body so the SPA's fetch() error handler can render a coherent
// message rather than the default text/plain stack.
func handleDashboardConfig(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		jsonCORS(w)
		if r.Method != http.MethodGet {
			http.Error(w, `{"error":"GET required"}`, http.StatusMethodNotAllowed)
			return
		}
		tabs, err := dashconfig.ResolveAllTabs(db)
		if err != nil {
			http.Error(w, fmt.Sprintf(`{"error":%q}`, err.Error()), http.StatusInternalServerError)
			return
		}
		display, err := dashconfig.ResolveDisplayConfig(db)
		if err != nil {
			http.Error(w, fmt.Sprintf(`{"error":%q}`, err.Error()), http.StatusInternalServerError)
			return
		}
		filters, err := dashconfig.ResolveSavedFilters(db)
		if err != nil {
			http.Error(w, fmt.Sprintf(`{"error":%q}`, err.Error()), http.StatusInternalServerError)
			return
		}
		resp := dashboardConfigResponse{
			Tabs:         tabs,
			Display:      display,
			SavedFilters: filters,
		}
		// Saved filters always serialises as an array (never null);
		// substrate ships an empty list and the SPA expects an array
		// shape regardless of length.
		if resp.SavedFilters == nil {
			resp.SavedFilters = []dashconfig.SavedFilter{}
		}
		_ = json.NewEncoder(w).Encode(resp)
	}
}
