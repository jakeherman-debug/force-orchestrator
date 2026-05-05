// internal/dashboard/config/resolver.go — D11 Phase 3 substrate.
//
// Resolves the effective dashboard configuration for the current
// operator by composing two layers:
//
//  1. YAML default (this package's parsed DashboardConfig)
//  2. SystemConfig override (per-key overrides — see SystemConfig key
//     namespace below)
//
// Sub-task B / C add WRITE endpoints that mutate the SystemConfig
// keys; the substrate ships only the read path. The resolver is a
// pure function modulo its SystemConfig reads — no mutations, no
// side effects.
//
// SystemConfig key namespace (absent = fall through to YAML default):
//
//	dashboard_tab_visible_<id>      — "0" | "1"
//	dashboard_tab_order_<id>        — integer
//	dashboard_tab_refresh_<id>      — integer (seconds)
//	dashboard_display_theme         — "light" | "dark" | "system"
//	dashboard_display_density       — "compact" | "comfortable"
//	dashboard_display_sort_<tab>    — sort-key string
//	dashboard_display_pagination    — integer
package dashconfig

import (
	"database/sql"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"force-orchestrator/internal/store"
)

// SystemConfig keys owned by the dashboard-config resolver. The full
// namespace lives in the package doc above.
const (
	ConfigKeyTabVisiblePrefix = "dashboard_tab_visible_"
	ConfigKeyTabOrderPrefix   = "dashboard_tab_order_"
	ConfigKeyTabRefreshPrefix = "dashboard_tab_refresh_"
	ConfigKeyDisplayTheme     = "dashboard_display_theme"
	ConfigKeyDisplayDensity   = "dashboard_display_density"
	ConfigKeyDisplaySortPfx   = "dashboard_display_sort_"
	ConfigKeyDisplayPagSize   = "dashboard_display_pagination"
)

// ErrUnknownTab is returned when ResolveTabConfig is asked about a
// tab id that isn't in the YAML.
type ErrUnknownTab struct {
	TabID string
}

func (e ErrUnknownTab) Error() string {
	return fmt.Sprintf("dashconfig: unknown tab %q (not in YAML registry)", e.TabID)
}

// ResolveTabConfig returns the effective TabConfig for the given tab,
// composing the YAML default with any SystemConfig overrides. Returns
// ErrUnknownTab if the tab id isn't registered.
//
// Each override layer is opt-in; an absent SystemConfig key means the
// YAML default wins. Malformed override values (e.g.
// `dashboard_tab_refresh_tasks=banana`) fall through to the YAML
// default — log + ignore is preferable to a hard error here because
// the substrate must keep serving the dashboard even if a single
// SystemConfig row is corrupt. The SPA write endpoints in sub-task B
// validate at write-time, so a malformed value implies external
// tampering or a stale schema.
func ResolveTabConfig(db *sql.DB, tabID string) (TabConfig, error) {
	cfg := GetGlobalConfig()
	if cfg == nil {
		return TabConfig{}, ErrNoConfig
	}
	yamlTab, ok := cfg.TabByID(tabID)
	if !ok {
		return TabConfig{}, ErrUnknownTab{TabID: tabID}
	}
	return applyTabOverrides(db, *yamlTab), nil
}

// ResolveAllTabs returns every YAML-registered tab with overrides
// applied, sorted by the resolved Order ascending. Used by the
// /api/dashboard/config GET endpoint.
func ResolveAllTabs(db *sql.DB) ([]TabConfig, error) {
	cfg := GetGlobalConfig()
	if cfg == nil {
		return nil, ErrNoConfig
	}
	out := make([]TabConfig, 0, len(cfg.Tabs))
	for _, t := range cfg.Tabs {
		out = append(out, applyTabOverrides(db, t))
	}
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].Order < out[j].Order
	})
	return out, nil
}

// applyTabOverrides composes one YAML tab with its per-tab
// SystemConfig overrides. Pure: reads SystemConfig only.
func applyTabOverrides(db *sql.DB, base TabConfig) TabConfig {
	out := base
	if v := store.GetConfig(db, ConfigKeyTabVisiblePrefix+base.ID, ""); v != "" {
		switch strings.TrimSpace(v) {
		case "1", "true", "yes":
			out.Visible = true
		case "0", "false", "no":
			out.Visible = false
		}
	}
	if v := store.GetConfig(db, ConfigKeyTabOrderPrefix+base.ID, ""); v != "" {
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && n > 0 {
			out.Order = n
		}
	}
	if v := store.GetConfig(db, ConfigKeyTabRefreshPrefix+base.ID, ""); v != "" {
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && n > 0 {
			out.RefreshSeconds = n
		}
	}
	return out
}

// ResolveDisplayConfig returns the effective DisplayConfig (YAML
// default + SystemConfig overrides). Used by /api/dashboard/config.
func ResolveDisplayConfig(db *sql.DB) (DisplayConfig, error) {
	cfg := GetGlobalConfig()
	if cfg == nil {
		return DisplayConfig{}, ErrNoConfig
	}
	out := DisplayConfig{
		Theme:              cfg.Display.Theme,
		Density:            cfg.Display.Density,
		DefaultSort:        copyStringMap(cfg.Display.DefaultSort),
		PerTablePagination: cfg.Display.PerTablePagination,
	}
	if v := store.GetConfig(db, ConfigKeyDisplayTheme, ""); v != "" {
		t := Theme(v)
		if t.IsValid() {
			out.Theme = t
		}
	}
	if v := store.GetConfig(db, ConfigKeyDisplayDensity, ""); v != "" {
		d := Density(v)
		if d.IsValid() {
			out.Density = d
		}
	}
	if v := store.GetConfig(db, ConfigKeyDisplayPagSize, ""); v != "" {
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && n > 0 {
			out.PerTablePagination = n
		}
	}
	// Per-tab default-sort overrides — read once per registered tab.
	for _, t := range cfg.Tabs {
		key := ConfigKeyDisplaySortPfx + t.ID
		if v := store.GetConfig(db, key, ""); v != "" {
			out.DefaultSort[t.ID] = v
		}
	}
	return out, nil
}

// copyStringMap returns a shallow copy of m so the resolver doesn't
// mutate the YAML-default map shared across requests.
func copyStringMap(m map[string]string) map[string]string {
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// ResolveSavedFilters returns the saved-filters list from the
// in-memory YAML config. Substrate ships an empty list; sub-task C
// will replace this with a SystemConfig-backed read once the write
// endpoint lands.
func ResolveSavedFilters(_ *sql.DB) ([]SavedFilter, error) {
	cfg := GetGlobalConfig()
	if cfg == nil {
		return nil, ErrNoConfig
	}
	out := make([]SavedFilter, len(cfg.SavedFilters))
	copy(out, cfg.SavedFilters)
	return out, nil
}
