// internal/dashboard/config/seed.go — D11 Phase 3 substrate.
//
// Seeds DashboardCatalogRegistry from a parsed YAML DashboardConfig.
// Called from the daemon-startup path in cmd/force/fleet_cmds.go after
// InitHolocronDSN runs (the table exists by then). Idempotent:
// re-runs upsert tabs that already exist (in case the YAML default
// flipped between daemon restarts) and leave tabs present in DB but
// absent from YAML alone — same preservation rule as
// notify.SeedRegistryFromYAML in P1.
package dashconfig

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
)

// SeedRegistryFromYAML upserts each YAML tab into
// DashboardCatalogRegistry. Returns nil on full success; the first
// error encountered is wrapped + returned after attempting all rows
// so the daemon-startup path gets a comprehensive picture.
//
// On upsert: visible_default + order_default + refresh_default +
// yaml_version are updated (registered_at is preserved on existing
// rows because it's a "first registration" audit field).
func SeedRegistryFromYAML(db *sql.DB, cfg *DashboardConfig) error {
	if cfg == nil {
		return fmt.Errorf("dashconfig: SeedRegistryFromYAML called with nil DashboardConfig")
	}
	var firstErr error
	for _, t := range cfg.Tabs {
		visibleInt := 0
		if t.Visible {
			visibleInt = 1
		}
		_, err := db.Exec(
			`INSERT INTO DashboardCatalogRegistry
			   (tab_id, visible_default, order_default, refresh_default, yaml_version)
			 VALUES (?, ?, ?, ?, ?)
			 ON CONFLICT(tab_id) DO UPDATE SET
			   visible_default = excluded.visible_default,
			   order_default   = excluded.order_default,
			   refresh_default = excluded.refresh_default,
			   yaml_version    = excluded.yaml_version`,
			t.ID, visibleInt, t.Order, t.RefreshSeconds, cfg.Version,
		)
		if err != nil && firstErr == nil {
			firstErr = fmt.Errorf("dashconfig: seed tab %q: %w", t.ID, err)
		}
	}
	return firstErr
}

// RegistryEntry is the read-side projection of one row from
// DashboardCatalogRegistry. Used by tests + the dashboard.
type RegistryEntry struct {
	ID             int
	TabID          string
	VisibleDefault bool
	OrderDefault   int
	RefreshDefault int
	RegisteredAt   string
	YAMLVersion    int
}

// SeedSavedFiltersFromYAML synchronises DashboardSavedFilters with
// the saved_filters: block in config/dashboard.yaml. Two-way sync,
// scoped to source='yaml' rows ONLY:
//
//  1. Each YAML-declared filter is upserted with source='yaml'. Existing
//     yaml-source rows have name/tab/description/filter_json/sort_*
//     refreshed on every seed (YAML is canonical).
//  2. yaml-source rows in DB whose (name, tab) tuple is not present in
//     the YAML are DELETED. The operator must remove them via a YAML
//     edit, not via the dashboard.
//
// source='dashboard' rows are NEVER touched — they are the operator's
// runtime-created filters, exported via the export endpoint into a YAML
// diff for review.
//
// The (name, tab) UNIQUE constraint forbids the (admittedly unlikely)
// case of a yaml-source filter colliding with a pre-existing dashboard-
// source filter of the same name. SeedSavedFiltersFromYAML refuses to
// upsert in that case and returns an error so the operator notices the
// collision at daemon start instead of at runtime.
func SeedSavedFiltersFromYAML(db *sql.DB, cfg *DashboardConfig) error {
	if cfg == nil {
		return fmt.Errorf("dashconfig: SeedSavedFiltersFromYAML called with nil DashboardConfig")
	}

	// Build the desired (name, tab) set from YAML.
	desired := make(map[string]struct{}, len(cfg.SavedFilters))
	for _, f := range cfg.SavedFilters {
		desired[f.Tab+"\x00"+f.Name] = struct{}{}
	}

	// Step 1 — refuse to upsert if a dashboard-source filter already
	// owns one of the (name, tab) tuples we're about to claim. This is
	// a hard error: the YAML author needs to either rename the filter
	// or the operator needs to delete the dashboard-source row first.
	for _, f := range cfg.SavedFilters {
		var src string
		err := db.QueryRow(
			`SELECT source FROM DashboardSavedFilters WHERE name = ? AND tab = ?`,
			f.Name, f.Tab,
		).Scan(&src)
		if err == sql.ErrNoRows {
			continue
		}
		if err != nil {
			return fmt.Errorf("dashconfig: probe saved_filter %q on tab %q: %w", f.Name, f.Tab, err)
		}
		if src != "" && src != "yaml" {
			return fmt.Errorf("dashconfig: yaml saved_filter %q on tab %q collides with existing source=%q row (rename the YAML filter or delete the dashboard row)", f.Name, f.Tab, src)
		}
	}

	// Step 2 — upsert each YAML row as source='yaml'.
	var firstErr error
	for _, f := range cfg.SavedFilters {
		filterJSON, err := json.Marshal(f.Filter)
		if err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("dashconfig: marshal saved_filter %q filter: %w", f.Name, err)
			}
			continue
		}
		_, err = db.Exec(
			`INSERT INTO DashboardSavedFilters
			   (name, tab, description, filter_json, sort_by, sort_dir, source, created_by)
			 VALUES (?, ?, ?, ?, ?, ?, 'yaml', 'yaml')
			 ON CONFLICT(name, tab) DO UPDATE SET
			   description = excluded.description,
			   filter_json = excluded.filter_json,
			   sort_by     = excluded.sort_by,
			   sort_dir    = excluded.sort_dir,
			   source      = 'yaml'`,
			f.Name, f.Tab, f.Description, string(filterJSON), f.SortBy, f.SortDir,
		)
		if err != nil && firstErr == nil {
			firstErr = fmt.Errorf("dashconfig: upsert saved_filter %q on tab %q: %w", f.Name, f.Tab, err)
		}
	}

	// Step 3 — sweep yaml-source rows that the YAML no longer declares.
	rows, err := db.Query(`SELECT id, name, tab FROM DashboardSavedFilters WHERE source = 'yaml'`)
	if err != nil {
		if firstErr == nil {
			firstErr = fmt.Errorf("dashconfig: list yaml saved_filters for sweep: %w", err)
		}
		return firstErr
	}
	type rowKey struct {
		id   int
		name string
		tab  string
	}
	var existing []rowKey
	for rows.Next() {
		var rk rowKey
		if err := rows.Scan(&rk.id, &rk.name, &rk.tab); err != nil {
			rows.Close()
			if firstErr == nil {
				firstErr = fmt.Errorf("dashconfig: scan yaml saved_filter sweep row: %w", err)
			}
			return firstErr
		}
		existing = append(existing, rk)
	}
	if rErr := rows.Err(); rErr != nil && firstErr == nil {
		firstErr = fmt.Errorf("dashconfig: yaml saved_filter sweep rows iter: %w", rErr)
	}
	rows.Close()
	for _, rk := range existing {
		if _, ok := desired[rk.tab+"\x00"+rk.name]; ok {
			continue
		}
		if _, derr := db.Exec(`DELETE FROM DashboardSavedFilters WHERE id = ? AND source = 'yaml'`, rk.id); derr != nil && firstErr == nil {
			firstErr = fmt.Errorf("dashconfig: sweep stale yaml saved_filter %q on tab %q: %w", rk.name, rk.tab, derr)
		}
	}

	return firstErr
}

// SavedFilterRow is the read-side projection of one DashboardSavedFilters
// row. The Filter map is decoded from filter_json on the way out.
type SavedFilterRow struct {
	ID          int                 `json:"id"`
	Name        string              `json:"name"`
	Tab         string              `json:"tab"`
	Description string              `json:"description"`
	Filter      map[string][]string `json:"filter"`
	SortBy      string              `json:"sort_by,omitempty"`
	SortDir     string              `json:"sort_dir,omitempty"`
	Source      string              `json:"source"`
	CreatedAt   string              `json:"created_at"`
	CreatedBy   string              `json:"created_by"`
}

// ListSavedFilters returns saved filters for one tab, ordered by source
// (yaml first — they're canonical) then name ASC. Used by the GET
// endpoint and by the export endpoint (which scopes to dashboard-source
// only).
func ListSavedFilters(db *sql.DB, tab string) ([]SavedFilterRow, error) {
	rows, err := db.Query(
		`SELECT id, name, tab, description, filter_json,
		        IFNULL(sort_by, ''), IFNULL(sort_dir, ''),
		        IFNULL(source, 'dashboard'), IFNULL(created_at, ''),
		        IFNULL(created_by, '')
		 FROM DashboardSavedFilters
		 WHERE tab = ?
		 ORDER BY (CASE WHEN source = 'yaml' THEN 0 ELSE 1 END), name ASC`,
		tab,
	)
	if err != nil {
		return nil, fmt.Errorf("dashconfig: list saved filters for tab %q: %w", tab, err)
	}
	defer rows.Close()
	var out []SavedFilterRow
	for rows.Next() {
		var r SavedFilterRow
		var filterJSON string
		if err := rows.Scan(&r.ID, &r.Name, &r.Tab, &r.Description, &filterJSON, &r.SortBy, &r.SortDir, &r.Source, &r.CreatedAt, &r.CreatedBy); err != nil {
			return nil, fmt.Errorf("dashconfig: scan saved filter row: %w", err)
		}
		if filterJSON != "" {
			if jerr := json.Unmarshal([]byte(filterJSON), &r.Filter); jerr != nil {
				return nil, fmt.Errorf("dashconfig: decode filter_json for filter %q: %w", r.Name, jerr)
			}
		} else {
			r.Filter = map[string][]string{}
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("dashconfig: saved-filters rows iter: %w", err)
	}
	return out, nil
}

// ListDashboardSourceFilters returns every source='dashboard' row across
// all tabs, sorted by tab then name. Used by the export endpoint.
func ListDashboardSourceFilters(db *sql.DB) ([]SavedFilterRow, error) {
	rows, err := db.Query(
		`SELECT id, name, tab, description, filter_json,
		        IFNULL(sort_by, ''), IFNULL(sort_dir, ''),
		        IFNULL(source, 'dashboard'), IFNULL(created_at, ''),
		        IFNULL(created_by, '')
		 FROM DashboardSavedFilters
		 WHERE source = 'dashboard'
		 ORDER BY tab ASC, name ASC`,
	)
	if err != nil {
		return nil, fmt.Errorf("dashconfig: list dashboard saved filters: %w", err)
	}
	defer rows.Close()
	var out []SavedFilterRow
	for rows.Next() {
		var r SavedFilterRow
		var filterJSON string
		if err := rows.Scan(&r.ID, &r.Name, &r.Tab, &r.Description, &filterJSON, &r.SortBy, &r.SortDir, &r.Source, &r.CreatedAt, &r.CreatedBy); err != nil {
			return nil, fmt.Errorf("dashconfig: scan dashboard saved filter row: %w", err)
		}
		if filterJSON != "" {
			if jerr := json.Unmarshal([]byte(filterJSON), &r.Filter); jerr != nil {
				return nil, fmt.Errorf("dashconfig: decode filter_json for filter %q: %w", r.Name, jerr)
			}
		} else {
			r.Filter = map[string][]string{}
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("dashconfig: dashboard saved-filters rows iter: %w", err)
	}
	// Make sure column-key iteration in callers is deterministic.
	for i := range out {
		_ = sortStringKeys(out[i].Filter)
	}
	return out, nil
}

// sortStringKeys returns a sorted slice of the map's keys; helper used
// for deterministic YAML rendering. Returned slice is unused at present
// (callers re-sort at render time) but kept to make the determinism
// intent explicit and discoverable.
func sortStringKeys(m map[string][]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// ListRegisteredTabs returns every row from the registry in
// order_default ascending order (then tab_id alpha as the
// tiebreaker). Used by tests + the dashboard.
func ListRegisteredTabs(db *sql.DB) ([]RegistryEntry, error) {
	rows, err := db.Query(
		`SELECT id, tab_id, visible_default, order_default, refresh_default,
		        IFNULL(registered_at, ''), yaml_version
		 FROM DashboardCatalogRegistry
		 ORDER BY order_default ASC, tab_id ASC`,
	)
	if err != nil {
		return nil, fmt.Errorf("dashconfig: list registry: %w", err)
	}
	defer rows.Close()
	var out []RegistryEntry
	for rows.Next() {
		var e RegistryEntry
		var visibleInt int
		if err := rows.Scan(&e.ID, &e.TabID, &visibleInt, &e.OrderDefault, &e.RefreshDefault, &e.RegisteredAt, &e.YAMLVersion); err != nil {
			return nil, fmt.Errorf("dashconfig: scan registry row: %w", err)
		}
		e.VisibleDefault = visibleInt != 0
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("dashconfig: registry rows iter: %w", err)
	}
	return out, nil
}
