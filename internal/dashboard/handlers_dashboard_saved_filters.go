// handlers_dashboard_saved_filters.go — D11 Phase 3 sub-task C.
//
// WRITE endpoints for the per-tab saved-filters surface. Sibling to
// handlers_dashboard_config.go (the GET-only substrate from sub-task A);
// kept in its own file so sub-task B (tab visibility / display
// preferences) and sub-task C don't fight over the same file at
// integration time.
//
// Endpoints:
//
//	GET    /api/dashboard/saved-filter?tab=<tab>       — list per-tab
//	POST   /api/dashboard/saved-filter                 — create dashboard-source filter
//	DELETE /api/dashboard/saved-filter/<id>            — delete (dashboard-source ONLY)
//	POST   /api/dashboard/saved-filter/export          — produce a YAML diff of all
//	                                                     dashboard-source rows so the
//	                                                     operator can paste into
//	                                                     config/dashboard.yaml + PR
//
// All state mutations write through DashboardSavedFilters; YAML-source
// rows are produced by the daemon-startup seeder (SeedSavedFiltersFromYAML)
// — operators delete them via a YAML edit + restart, NEVER via the
// dashboard. Anti-cheat: DELETE on a yaml-source row returns 400.
//
// Audit-trail: every state-mutating endpoint records a row via
// store.LogAudit (action ∈ {dashboard_saved_filter_create,
// dashboard_saved_filter_delete, dashboard_saved_filter_export}).
package dashboard

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	dashconfig "force-orchestrator/internal/dashboard/config"
	"force-orchestrator/internal/store"
)

// handleDashboardSavedFilter dispatches GET (list) and POST (create) on
// /api/dashboard/saved-filter. The path is a fixed leaf — DELETE goes
// through the /<id> sub-route handler below.
func handleDashboardSavedFilter(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		jsonCORS(w)
		switch r.Method {
		case http.MethodGet:
			handleSavedFilterList(db, w, r)
		case http.MethodPost:
			handleSavedFilterCreate(db, w, r)
		default:
			http.Error(w, `{"error":"GET or POST required"}`, http.StatusMethodNotAllowed)
		}
	}
}

// handleSavedFilterList — GET /api/dashboard/saved-filter?tab=<tab>.
func handleSavedFilterList(db *sql.DB, w http.ResponseWriter, r *http.Request) {
	tab := strings.TrimSpace(r.URL.Query().Get("tab"))
	if tab == "" {
		http.Error(w, `{"error":"tab query param required"}`, http.StatusBadRequest)
		return
	}
	rows, err := dashconfig.ListSavedFilters(db, tab)
	if err != nil {
		http.Error(w, fmt.Sprintf(`{"error":%q}`, err.Error()), http.StatusInternalServerError)
		return
	}
	if rows == nil {
		rows = []dashconfig.SavedFilterRow{}
	}
	_ = json.NewEncoder(w).Encode(map[string]any{
		"tab":     tab,
		"filters": rows,
	})
}

// handleSavedFilterCreate — POST /api/dashboard/saved-filter.
//
// Body shape:
//
//	{
//	  "name":        "My active convoys",
//	  "tab":         "convoys",
//	  "description": "Active or DraftPROpen, sorted newest first",
//	  "filter":      {"status": ["Active", "DraftPROpen"]},
//	  "sort_by":     "id",
//	  "sort_dir":    "desc",
//	  "operator":    "jake"
//	}
//
// Validates: name + tab + filter non-empty; tab is registered; sort_dir
// in {asc,desc} when set; UNIQUE(name, tab) — a yaml-source row of the
// same (name, tab) pair causes a 409 with an explicit message.
func handleSavedFilterCreate(db *sql.DB, w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name        string              `json:"name"`
		Tab         string              `json:"tab"`
		Description string              `json:"description"`
		Filter      map[string][]string `json:"filter"`
		SortBy      string              `json:"sort_by"`
		SortDir     string              `json:"sort_dir"`
		Operator    string              `json:"operator"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, `{"error":"bad JSON"}`, http.StatusBadRequest)
		return
	}
	body.Name = strings.TrimSpace(body.Name)
	body.Tab = strings.TrimSpace(body.Tab)
	body.SortBy = strings.TrimSpace(body.SortBy)
	body.SortDir = strings.TrimSpace(body.SortDir)
	if body.Name == "" {
		http.Error(w, `{"error":"name required"}`, http.StatusBadRequest)
		return
	}
	if body.Tab == "" {
		http.Error(w, `{"error":"tab required"}`, http.StatusBadRequest)
		return
	}
	if len(body.Filter) == 0 {
		http.Error(w, `{"error":"filter must be non-empty"}`, http.StatusBadRequest)
		return
	}
	for col := range body.Filter {
		if strings.TrimSpace(col) == "" {
			http.Error(w, `{"error":"filter column key cannot be empty"}`, http.StatusBadRequest)
			return
		}
	}
	if body.SortDir != "" && body.SortDir != "asc" && body.SortDir != "desc" {
		http.Error(w, `{"error":"sort_dir must be empty, asc, or desc"}`, http.StatusBadRequest)
		return
	}

	// Reject unknown tab. The substrate (sub-task A) ships a registry of
	// every operator-facing tab; rejecting unknown tabs at write time
	// keeps the dashboard from accumulating filters bound to tabs that
	// no longer exist.
	cfg := dashconfig.GetGlobalConfig()
	if cfg == nil {
		http.Error(w, `{"error":"dashboard config not loaded"}`, http.StatusInternalServerError)
		return
	}
	if _, ok := cfg.TabByID(body.Tab); !ok {
		http.Error(w, fmt.Sprintf(`{"error":"unknown tab %q"}`, body.Tab), http.StatusBadRequest)
		return
	}

	// Probe for existing (name, tab) row. UNIQUE(name, tab) would
	// otherwise turn this into a generic SQL error; we want a coherent
	// 409 with a hint about source.
	var existingSrc string
	err := db.QueryRow(
		`SELECT IFNULL(source, 'dashboard') FROM DashboardSavedFilters WHERE name = ? AND tab = ?`,
		body.Name, body.Tab,
	).Scan(&existingSrc)
	if err == nil {
		switch existingSrc {
		case "yaml":
			http.Error(w, fmt.Sprintf(`{"error":"saved filter %q on tab %q exists as yaml-source (commit a YAML edit to change it)"}`, body.Name, body.Tab), http.StatusConflict)
		default:
			http.Error(w, fmt.Sprintf(`{"error":"saved filter %q on tab %q already exists"}`, body.Name, body.Tab), http.StatusConflict)
		}
		return
	} else if err != sql.ErrNoRows {
		http.Error(w, fmt.Sprintf(`{"error":%q}`, err.Error()), http.StatusInternalServerError)
		return
	}

	filterJSON, jerr := json.Marshal(body.Filter)
	if jerr != nil {
		http.Error(w, fmt.Sprintf(`{"error":%q}`, jerr.Error()), http.StatusInternalServerError)
		return
	}
	actor := strings.TrimSpace(body.Operator)
	if actor == "" {
		actor = "operator"
	}
	res, ierr := db.Exec(
		`INSERT INTO DashboardSavedFilters
		   (name, tab, description, filter_json, sort_by, sort_dir, source, created_by)
		 VALUES (?, ?, ?, ?, ?, ?, 'dashboard', ?)`,
		body.Name, body.Tab, body.Description, string(filterJSON), body.SortBy, body.SortDir, actor,
	)
	if ierr != nil {
		http.Error(w, fmt.Sprintf(`{"error":%q}`, ierr.Error()), http.StatusInternalServerError)
		return
	}
	id, _ := res.LastInsertId()
	store.LogAudit(db, actor, "dashboard_saved_filter_create", 0,
		fmt.Sprintf("name=%s tab=%s id=%d", body.Name, body.Tab, id))
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"ok":     true,
		"id":     id,
		"name":   body.Name,
		"tab":    body.Tab,
		"source": "dashboard",
	})
}

// handleDashboardSavedFilterByID dispatches DELETE on
// /api/dashboard/saved-filter/<id>. POST/PUT/PATCH not supported —
// updates go through delete + create.
func handleDashboardSavedFilterByID(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		jsonCORS(w)
		// /api/dashboard/saved-filter/export — POST routed here because
		// the prefix is shared. Carve out before the integer-ID parse.
		if strings.HasSuffix(r.URL.Path, "/export") {
			handleSavedFilterExport(db, w, r)
			return
		}
		if r.Method != http.MethodDelete {
			http.Error(w, `{"error":"DELETE required"}`, http.StatusMethodNotAllowed)
			return
		}
		idStr := strings.TrimPrefix(r.URL.Path, "/api/dashboard/saved-filter/")
		idStr = strings.TrimSuffix(idStr, "/")
		if idStr == "" {
			http.Error(w, `{"error":"id required in path"}`, http.StatusBadRequest)
			return
		}
		id, perr := strconv.Atoi(idStr)
		if perr != nil || id <= 0 {
			http.Error(w, `{"error":"id must be a positive integer"}`, http.StatusBadRequest)
			return
		}
		var body struct {
			Operator string `json:"operator"`
			Reason   string `json:"reason"`
		}
		// Body is optional but we still accept JSON if present.
		_ = json.NewDecoder(r.Body).Decode(&body)
		actor := strings.TrimSpace(body.Operator)
		if actor == "" {
			actor = "operator"
		}

		// Look up the row to decide if it's deletable.
		var name, tab, source string
		err := db.QueryRow(
			`SELECT name, tab, IFNULL(source, 'dashboard')
			 FROM DashboardSavedFilters WHERE id = ?`, id,
		).Scan(&name, &tab, &source)
		if err == sql.ErrNoRows {
			http.Error(w, fmt.Sprintf(`{"error":"saved filter id=%d not found"}`, id), http.StatusNotFound)
			return
		}
		if err != nil {
			http.Error(w, fmt.Sprintf(`{"error":%q}`, err.Error()), http.StatusInternalServerError)
			return
		}
		if source == "yaml" {
			// Anti-cheat: yaml-source filters cannot be deleted at runtime.
			// Operator must commit a YAML edit. Tested.
			http.Error(w, fmt.Sprintf(`{"error":"saved filter %q on tab %q is yaml-source; remove it from config/dashboard.yaml and restart the daemon"}`, name, tab), http.StatusBadRequest)
			return
		}
		if _, dErr := db.Exec(`DELETE FROM DashboardSavedFilters WHERE id = ? AND source = 'dashboard'`, id); dErr != nil {
			http.Error(w, fmt.Sprintf(`{"error":%q}`, dErr.Error()), http.StatusInternalServerError)
			return
		}
		store.LogAudit(db, actor, "dashboard_saved_filter_delete", 0,
			fmt.Sprintf("id=%d name=%s tab=%s reason=%s", id, name, tab, body.Reason))
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok":   true,
			"id":   id,
			"name": name,
			"tab":  tab,
		})
	}
}

// handleSavedFilterExport — POST /api/dashboard/saved-filter/export.
//
// Renders every source='dashboard' row across all tabs as a YAML
// `saved_filters:` block formatted for paste-back into
// config/dashboard.yaml. Writes to os.TempDir(); never auto-mutates the
// canonical YAML on disk — the operator is expected to copy + commit
// via the standard PR flow. Same shape as the P2-A preset-save export.
func handleSavedFilterExport(db *sql.DB, w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, `{"error":"POST required"}`, http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		Operator string `json:"operator"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	actor := strings.TrimSpace(body.Operator)
	if actor == "" {
		actor = "operator"
	}

	rows, err := dashconfig.ListDashboardSourceFilters(db)
	if err != nil {
		http.Error(w, fmt.Sprintf(`{"error":%q}`, err.Error()), http.StatusInternalServerError)
		return
	}

	yamlBody := renderSavedFiltersYAML(rows)
	ts := time.Now().UTC().Format("20060102T150405Z")
	fname := fmt.Sprintf("dashboard-saved-filters-%s.yaml.diff", ts)
	fpath := filepath.Join(os.TempDir(), fname)
	if werr := os.WriteFile(fpath, []byte(yamlBody), 0o644); werr != nil {
		http.Error(w, fmt.Sprintf(`{"error":%q}`, werr.Error()), http.StatusInternalServerError)
		return
	}
	store.LogAudit(db, actor, "dashboard_saved_filter_export", 0,
		fmt.Sprintf("count=%d path=%s", len(rows), fpath))
	_ = json.NewEncoder(w).Encode(map[string]any{
		"ok":      true,
		"path":    fpath,
		"content": yamlBody,
		"count":   len(rows),
	})
}

// renderSavedFiltersYAML produces a deterministic YAML rendering of the
// saved-filters list. Format mirrors the canonical config/dashboard.yaml
// `saved_filters:` block so the operator can paste straight in.
//
// Determinism rules:
//   - filters sorted by tab, then name (matches ListDashboardSourceFilters)
//   - filter columns sorted alphabetically
//   - filter values preserved in insertion order (operator's intent)
func renderSavedFiltersYAML(rows []dashconfig.SavedFilterRow) string {
	var sb strings.Builder
	sb.WriteString("# Generated by /api/dashboard/saved-filter/export — paste under\n")
	sb.WriteString("# `saved_filters:` in config/dashboard.yaml and commit via the normal\n")
	sb.WriteString("# review path. This export contains only dashboard-source filters\n")
	sb.WriteString("# (the operator's runtime-saved entries); yaml-source filters are\n")
	sb.WriteString("# already in the YAML.\n")
	sb.WriteString("saved_filters:\n")
	if len(rows) == 0 {
		sb.WriteString("  []\n")
		return sb.String()
	}
	for _, r := range rows {
		fmt.Fprintf(&sb, "  - name: %s\n", yamlScalar(r.Name))
		fmt.Fprintf(&sb, "    tab: %s\n", yamlScalar(r.Tab))
		if r.Description != "" {
			fmt.Fprintf(&sb, "    description: %s\n", yamlScalar(r.Description))
		}
		sb.WriteString("    filter:\n")
		cols := make([]string, 0, len(r.Filter))
		for k := range r.Filter {
			cols = append(cols, k)
		}
		sort.Strings(cols)
		for _, col := range cols {
			fmt.Fprintf(&sb, "      %s:\n", yamlScalar(col))
			for _, v := range r.Filter[col] {
				fmt.Fprintf(&sb, "        - %s\n", yamlScalar(v))
			}
		}
		if r.SortBy != "" {
			fmt.Fprintf(&sb, "    sort_by: %s\n", yamlScalar(r.SortBy))
		}
		if r.SortDir != "" {
			fmt.Fprintf(&sb, "    sort_dir: %s\n", yamlScalar(r.SortDir))
		}
	}
	return sb.String()
}

// yamlScalar quotes a scalar value for safe paste-back. Quotes are
// always applied for free-text values (description / filter values) to
// dodge the "scalar starts with -, :, or #" parser trap; identifier-
// shaped strings (alphanum + underscore + hyphen) are emitted unquoted
// for visual cleanliness.
func yamlScalar(s string) string {
	if s == "" {
		return `""`
	}
	clean := true
	for _, c := range s {
		switch {
		case c >= 'a' && c <= 'z',
			c >= 'A' && c <= 'Z',
			c >= '0' && c <= '9',
			c == '_', c == '-':
		default:
			clean = false
		}
	}
	if clean {
		return s
	}
	escaped := strings.ReplaceAll(s, `\`, `\\`)
	escaped = strings.ReplaceAll(escaped, `"`, `\"`)
	return `"` + escaped + `"`
}
