// handlers_dashboard_config_write.go — D11 Phase 3 sub-task B.
//
// Operator-facing WRITE endpoints for the dashboard-personalization
// substrate that P3-A introduced. These mirror P3-A's read handler
// (handlers_dashboard_config.go) in shape: method-gated, JSON wire
// contract, all writes route through SystemConfig keys defined in
// internal/dashboard/config/resolver.go.
//
// Endpoints:
//
//	POST /api/dashboard/config/tab/<tab_id>          — set per-tab
//	                                                   visibility / order /
//	                                                   refresh_seconds
//	POST /api/dashboard/config/tab/<tab_id>/clear    — clear per-tab
//	                                                   override keys
//	POST /api/dashboard/config/display               — set theme / density /
//	                                                   default_sort /
//	                                                   per_table_pagination
//	POST /api/dashboard/config/display/clear         — clear display keys
//
// Validation is server-side (the SPA may pre-validate, but the
// authoritative check lives here). Theme ∈ {light,dark,system};
// density ∈ {compact,comfortable}; refresh_seconds ∈ (0, 3600];
// pagination ∈ [10, 500]; tab_id must be a YAML-registered tab.
//
// Every successful mutation logs an AuditLog row via store.LogAudit so
// "who flipped this preference?" is answerable from the holocron.
//
// Sub-task C owns POST /api/dashboard/saved-filter and the
// `saved_filters` field of DashboardConfig — this file deliberately
// does not touch either.
package dashboard

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"

	dashconfig "force-orchestrator/internal/dashboard/config"
	"force-orchestrator/internal/store"
)

// Validation bounds for the write endpoints. Pinned as constants so
// the handler tests can reference the exact thresholds without
// duplicating magic numbers.
const (
	dashCfgMinRefreshSeconds = 1
	dashCfgMaxRefreshSeconds = 3600
	dashCfgMinPagination     = 10
	dashCfgMaxPagination     = 500
)

// handleDashboardConfigTabWrite — POST /api/dashboard/config/tab/<id>
// and POST /api/dashboard/config/tab/<id>/clear.
//
// Body for the set form:
//
//	{
//	  "visible":         true,        // optional — bool
//	  "order":           4,           // optional — positive int
//	  "refresh_seconds": 30,          // optional — int in (0, 3600]
//	  "operator":        "jake"       // optional — actor for audit log
//	}
//
// Each field is independently optional; a request with only `visible`
// updates only the visibility key. Fields are validated even when
// optional — an explicit `refresh_seconds: 0` is rejected.
//
// The /clear suffix wipes all three SystemConfig keys for the tab
// (back to YAML default), regardless of body contents.
func handleDashboardConfigTabWrite(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		jsonCORS(w)
		if r.Method != http.MethodPost {
			http.Error(w, `{"error":"POST required"}`, http.StatusMethodNotAllowed)
			return
		}
		path := strings.TrimPrefix(r.URL.Path, "/api/dashboard/config/tab/")
		path = strings.TrimSuffix(path, "/")
		if path == "" {
			http.Error(w, `{"error":"tab id required in path: /api/dashboard/config/tab/<id>"}`, http.StatusBadRequest)
			return
		}
		isClear := false
		if strings.HasSuffix(path, "/clear") {
			isClear = true
			path = strings.TrimSuffix(path, "/clear")
		}
		tabID := strings.TrimSpace(path)
		if tabID == "" {
			http.Error(w, `{"error":"tab id required"}`, http.StatusBadRequest)
			return
		}
		cfg := dashconfig.GetGlobalConfig()
		if cfg == nil {
			http.Error(w, `{"error":"dashboard config not loaded"}`, http.StatusInternalServerError)
			return
		}
		if _, ok := cfg.TabByID(tabID); !ok {
			http.Error(w, fmt.Sprintf(`{"error":"unknown tab %q"}`, tabID), http.StatusBadRequest)
			return
		}

		// Body decoding is best-effort: the /clear path tolerates an
		// empty body (operator may be implicit), the set path requires
		// at least one mutable field.
		var body struct {
			Visible        *bool `json:"visible"`
			Order          *int  `json:"order"`
			RefreshSeconds *int  `json:"refresh_seconds"`
			Operator       string `json:"operator"`
		}
		// Decode, but a parse error on the set path is a 400; on the
		// clear path we silently fall through (empty body is fine).
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil && !isClear {
			http.Error(w, `{"error":"bad JSON"}`, http.StatusBadRequest)
			return
		}
		actor := strings.TrimSpace(body.Operator)
		if actor == "" {
			actor = "operator"
		}

		if isClear {
			store.SetConfig(db, dashconfig.ConfigKeyTabVisiblePrefix+tabID, "")
			store.SetConfig(db, dashconfig.ConfigKeyTabOrderPrefix+tabID, "")
			store.SetConfig(db, dashconfig.ConfigKeyTabRefreshPrefix+tabID, "")
			store.LogAudit(db, actor, "dashboard_tab_clear", 0, tabID)
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "tab_id": tabID, "cleared": true})
			return
		}

		// Set form — at least one field MUST be present. If all three
		// are nil, the operator gave us an empty op; better to 400
		// than silently no-op.
		if body.Visible == nil && body.Order == nil && body.RefreshSeconds == nil {
			http.Error(w, `{"error":"at least one of visible, order, refresh_seconds must be set"}`, http.StatusBadRequest)
			return
		}
		// Validate refresh_seconds bounds before any write so a partial
		// update doesn't land. Same for order.
		if body.Order != nil && *body.Order <= 0 {
			http.Error(w, fmt.Sprintf(`{"error":"order must be positive (got %d)"}`, *body.Order), http.StatusBadRequest)
			return
		}
		if body.RefreshSeconds != nil {
			if *body.RefreshSeconds < dashCfgMinRefreshSeconds || *body.RefreshSeconds > dashCfgMaxRefreshSeconds {
				http.Error(w, fmt.Sprintf(`{"error":"refresh_seconds must be in [%d, %d] (got %d)"}`,
					dashCfgMinRefreshSeconds, dashCfgMaxRefreshSeconds, *body.RefreshSeconds),
					http.StatusBadRequest)
				return
			}
		}

		// Validation passed — write the keys.
		var written []string
		if body.Visible != nil {
			val := "0"
			if *body.Visible {
				val = "1"
			}
			store.SetConfig(db, dashconfig.ConfigKeyTabVisiblePrefix+tabID, val)
			written = append(written, "visible")
		}
		if body.Order != nil {
			store.SetConfig(db, dashconfig.ConfigKeyTabOrderPrefix+tabID, fmt.Sprintf("%d", *body.Order))
			written = append(written, "order")
		}
		if body.RefreshSeconds != nil {
			store.SetConfig(db, dashconfig.ConfigKeyTabRefreshPrefix+tabID, fmt.Sprintf("%d", *body.RefreshSeconds))
			written = append(written, "refresh_seconds")
		}
		// Audit detail records the tab + which fields landed so a
		// "who silenced the convoys tab?" audit is one query away.
		store.LogAudit(db, actor, "dashboard_tab_set", 0,
			fmt.Sprintf("tab=%s fields=%s", tabID, strings.Join(written, ",")))
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok":     true,
			"tab_id": tabID,
			"set":    written,
		})
	}
}

// handleDashboardConfigDisplayWrite — POST /api/dashboard/config/display
// and POST /api/dashboard/config/display/clear.
//
// Body for the set form:
//
//	{
//	  "theme":                "dark",                   // light|dark|system
//	  "density":              "compact",                // compact|comfortable
//	  "default_sort":         {"tasks":"created_desc"}, // map[tab_id]sort_key
//	  "per_table_pagination": 100,                      // [10, 500]
//	  "operator":             "jake"
//	}
//
// All fields optional; at least one must be present (no-op rejection
// matches the per-tab handler shape).
//
// /clear wipes every dashboard_display_* SystemConfig key, including
// the per-tab default-sort overrides for every YAML-registered tab.
func handleDashboardConfigDisplayWrite(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		jsonCORS(w)
		if r.Method != http.MethodPost {
			http.Error(w, `{"error":"POST required"}`, http.StatusMethodNotAllowed)
			return
		}
		path := strings.TrimPrefix(r.URL.Path, "/api/dashboard/config/display")
		path = strings.TrimPrefix(path, "/")
		path = strings.TrimSuffix(path, "/")
		isClear := path == "clear"
		if path != "" && !isClear {
			http.Error(w, fmt.Sprintf(`{"error":"unknown subpath %q"}`, path), http.StatusBadRequest)
			return
		}
		cfg := dashconfig.GetGlobalConfig()
		if cfg == nil {
			http.Error(w, `{"error":"dashboard config not loaded"}`, http.StatusInternalServerError)
			return
		}

		var body struct {
			Theme              *string           `json:"theme"`
			Density            *string           `json:"density"`
			DefaultSort        map[string]string `json:"default_sort"`
			PerTablePagination *int              `json:"per_table_pagination"`
			Operator           string            `json:"operator"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil && !isClear {
			http.Error(w, `{"error":"bad JSON"}`, http.StatusBadRequest)
			return
		}
		actor := strings.TrimSpace(body.Operator)
		if actor == "" {
			actor = "operator"
		}

		if isClear {
			store.SetConfig(db, dashconfig.ConfigKeyDisplayTheme, "")
			store.SetConfig(db, dashconfig.ConfigKeyDisplayDensity, "")
			store.SetConfig(db, dashconfig.ConfigKeyDisplayPagSize, "")
			// Clear every per-tab default-sort key.
			for _, t := range cfg.Tabs {
				store.SetConfig(db, dashconfig.ConfigKeyDisplaySortPfx+t.ID, "")
			}
			store.LogAudit(db, actor, "dashboard_display_clear", 0, "all")
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "cleared": true})
			return
		}

		// Set form — at least one field must be present.
		if body.Theme == nil && body.Density == nil && body.PerTablePagination == nil && len(body.DefaultSort) == 0 {
			http.Error(w, `{"error":"at least one of theme, density, default_sort, per_table_pagination must be set"}`, http.StatusBadRequest)
			return
		}
		// Validate before any write — partial application is worse
		// than failing fast.
		if body.Theme != nil {
			if !dashconfig.Theme(*body.Theme).IsValid() {
				http.Error(w, fmt.Sprintf(`{"error":"theme must be one of light|dark|system (got %q)"}`, *body.Theme), http.StatusBadRequest)
				return
			}
		}
		if body.Density != nil {
			if !dashconfig.Density(*body.Density).IsValid() {
				http.Error(w, fmt.Sprintf(`{"error":"density must be one of compact|comfortable (got %q)"}`, *body.Density), http.StatusBadRequest)
				return
			}
		}
		if body.PerTablePagination != nil {
			if *body.PerTablePagination < dashCfgMinPagination || *body.PerTablePagination > dashCfgMaxPagination {
				http.Error(w, fmt.Sprintf(`{"error":"per_table_pagination must be in [%d, %d] (got %d)"}`,
					dashCfgMinPagination, dashCfgMaxPagination, *body.PerTablePagination),
					http.StatusBadRequest)
				return
			}
		}
		// Per-tab default-sort entries — every key must be a known tab.
		// (An unknown tab id silently ignored at resolve time would
		// confuse operators; reject at write time so they get a clear
		// error.)
		if len(body.DefaultSort) > 0 {
			sortKeys := make([]string, 0, len(body.DefaultSort))
			for k := range body.DefaultSort {
				sortKeys = append(sortKeys, k)
			}
			sort.Strings(sortKeys)
			for _, k := range sortKeys {
				if _, ok := cfg.TabByID(k); !ok {
					http.Error(w, fmt.Sprintf(`{"error":"default_sort references unknown tab %q"}`, k), http.StatusBadRequest)
					return
				}
				if strings.TrimSpace(body.DefaultSort[k]) == "" {
					http.Error(w, fmt.Sprintf(`{"error":"default_sort[%q] is empty"}`, k), http.StatusBadRequest)
					return
				}
			}
		}

		// Validation passed — write the keys.
		var written []string
		if body.Theme != nil {
			store.SetConfig(db, dashconfig.ConfigKeyDisplayTheme, *body.Theme)
			written = append(written, "theme")
		}
		if body.Density != nil {
			store.SetConfig(db, dashconfig.ConfigKeyDisplayDensity, *body.Density)
			written = append(written, "density")
		}
		if body.PerTablePagination != nil {
			store.SetConfig(db, dashconfig.ConfigKeyDisplayPagSize, fmt.Sprintf("%d", *body.PerTablePagination))
			written = append(written, "per_table_pagination")
		}
		if len(body.DefaultSort) > 0 {
			for k, v := range body.DefaultSort {
				store.SetConfig(db, dashconfig.ConfigKeyDisplaySortPfx+k, v)
			}
			written = append(written, "default_sort")
		}
		store.LogAudit(db, actor, "dashboard_display_set", 0,
			fmt.Sprintf("fields=%s", strings.Join(written, ",")))
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok":  true,
			"set": written,
		})
	}
}
