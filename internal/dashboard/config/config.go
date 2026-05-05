// Package dashconfig is the dashboard-personalization substrate (D11
// Phase 3 — Sub-task A).
//
// The package owns:
//
//  1. The YAML schema (config/dashboard.yaml) — the canonical
//     declaration of every operator-facing dashboard tab + display
//     preference.
//  2. The resolution chain (YAML default → SystemConfig override) for
//     per-tab visibility / order / refresh and global display
//     preferences (theme, density, default sort, pagination).
//  3. The seeding logic that upserts tabs into
//     DashboardCatalogRegistry.
//
// This file (config.go) defines the YAML model and the parser. The
// resolver lives in resolver.go; the seeder lives in seed.go.
//
// Sub-task B (SPA tab visibility / ordering / refresh + theme/density)
// and sub-task C (saved filters per tab) build on this substrate by
// adding write endpoints — the substrate ships only the read path.
package dashconfig

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"

	"gopkg.in/yaml.v3"
)

// Theme is the resolved display theme for the dashboard.
type Theme string

const (
	ThemeLight  Theme = "light"
	ThemeDark   Theme = "dark"
	ThemeSystem Theme = "system"
)

// IsValid reports whether t is one of the three legal Theme values.
func (t Theme) IsValid() bool {
	switch t {
	case ThemeLight, ThemeDark, ThemeSystem:
		return true
	}
	return false
}

// Density is the resolved table density preference.
type Density string

const (
	DensityCompact     Density = "compact"
	DensityComfortable Density = "comfortable"
)

// IsValid reports whether d is one of the two legal Density values.
func (d Density) IsValid() bool {
	switch d {
	case DensityCompact, DensityComfortable:
		return true
	}
	return false
}

// TabConfig is the resolved configuration for a single dashboard tab.
// The values are the composition of the YAML default + any
// SystemConfig override. The resolver returns this shape; the seeder
// reads only the YAML half.
type TabConfig struct {
	ID             string `json:"id" yaml:"id"`
	Visible        bool   `json:"visible" yaml:"visible"`
	Order          int    `json:"order" yaml:"order"`
	RefreshSeconds int    `json:"refresh_seconds" yaml:"refresh_seconds"`
}

// DisplayConfig is the resolved global display preferences.
type DisplayConfig struct {
	Theme              Theme             `json:"theme" yaml:"theme"`
	Density            Density           `json:"density" yaml:"density"`
	DefaultSort        map[string]string `json:"default_sort" yaml:"default_sort"`
	PerTablePagination int               `json:"per_table_pagination" yaml:"per_table_pagination"`
}

// SavedFilter is the placeholder shape for sub-task C. Substrate only
// declares the type so the parser shape is stable; sub-task C wires
// the write endpoint that populates it.
type SavedFilter struct {
	ID    string            `json:"id" yaml:"id"`
	TabID string            `json:"tab_id" yaml:"tab_id"`
	Name  string            `json:"name" yaml:"name"`
	Query map[string]string `json:"query" yaml:"query"`
}

// DashboardConfig is the parsed dashboard.yaml document.
type DashboardConfig struct {
	Version      int           `yaml:"version"`
	Tabs         []TabConfig   `yaml:"tabs"`
	Display      DisplayConfig `yaml:"display"`
	SavedFilters []SavedFilter `yaml:"saved_filters"`
}

// TabIDs returns the registered tab IDs in declaration order. Used by
// tests + the seeder.
func (c *DashboardConfig) TabIDs() []string {
	out := make([]string, 0, len(c.Tabs))
	for _, t := range c.Tabs {
		out = append(out, t.ID)
	}
	return out
}

// TabByID returns the YAML tab spec for the given ID, or (nil, false)
// if absent.
func (c *DashboardConfig) TabByID(id string) (*TabConfig, bool) {
	for i := range c.Tabs {
		if c.Tabs[i].ID == id {
			return &c.Tabs[i], true
		}
	}
	return nil, false
}

// rawTab is the on-disk YAML shape for one tab entry. Mirrors
// TabConfig but lets the parser distinguish between an explicit
// `visible: false` and an omitted field — visibility defaults to true
// when absent, so a YAML author can write a minimal entry.
type rawTab struct {
	ID             string `yaml:"id"`
	Visible        *bool  `yaml:"visible"`
	Order          int    `yaml:"order"`
	RefreshSeconds int    `yaml:"refresh_seconds"`
}

// rawDisplay is the on-disk YAML shape for the display section.
type rawDisplay struct {
	Theme              string            `yaml:"theme"`
	Density            string            `yaml:"density"`
	DefaultSort        map[string]string `yaml:"default_sort"`
	PerTablePagination int               `yaml:"per_table_pagination"`
}

// rawConfig is the on-disk YAML shape for the entire document.
type rawConfig struct {
	Version      int           `yaml:"version"`
	Tabs         []rawTab      `yaml:"tabs"`
	Display      rawDisplay    `yaml:"display"`
	SavedFilters []SavedFilter `yaml:"saved_filters"`
}

// LoadConfig reads + parses dashboard.yaml at the given path. Returns
// a fully validated DashboardConfig or an error describing the first
// defect encountered (missing file, malformed YAML, invalid theme,
// invalid density, negative refresh, duplicate tab ID, etc.).
//
// LoadConfig is fail-closed: if anything is off, no Config is
// returned. The daemon-startup seeder treats a parse failure as a
// hard error (same shape as notify.LoadConfig in P1).
func LoadConfig(path string) (*DashboardConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("dashconfig: read %s: %w", path, err)
	}
	return ParseConfig(data, path)
}

// ParseConfig parses an in-memory YAML byte slice. Used by tests that
// want to drive parser variations without touching the filesystem.
//
// `sourceLabel` is a free-text label (typically the file path) that
// appears in error messages.
func ParseConfig(data []byte, sourceLabel string) (*DashboardConfig, error) {
	var raw rawConfig
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("dashconfig: parse %s: %w", sourceLabel, err)
	}
	if raw.Version != 1 {
		return nil, fmt.Errorf("dashconfig: %s: unsupported version %d (want 1)", sourceLabel, raw.Version)
	}
	if len(raw.Tabs) == 0 {
		return nil, fmt.Errorf("dashconfig: %s: no tabs declared", sourceLabel)
	}

	cfg := &DashboardConfig{
		Version:      raw.Version,
		Tabs:         make([]TabConfig, 0, len(raw.Tabs)),
		SavedFilters: append([]SavedFilter(nil), raw.SavedFilters...),
	}

	seenTab := make(map[string]struct{}, len(raw.Tabs))
	seenOrder := make(map[int]string, len(raw.Tabs))
	for _, rt := range raw.Tabs {
		id := strings.TrimSpace(rt.ID)
		if id == "" {
			return nil, fmt.Errorf("dashconfig: %s: tab with empty id", sourceLabel)
		}
		if _, dup := seenTab[id]; dup {
			return nil, fmt.Errorf("dashconfig: %s: duplicate tab id %q", sourceLabel, id)
		}
		seenTab[id] = struct{}{}
		if rt.Order <= 0 {
			return nil, fmt.Errorf("dashconfig: %s: tab %q has non-positive order %d", sourceLabel, id, rt.Order)
		}
		if other, dup := seenOrder[rt.Order]; dup {
			return nil, fmt.Errorf("dashconfig: %s: tabs %q and %q share order %d (must be unique)", sourceLabel, other, id, rt.Order)
		}
		seenOrder[rt.Order] = id
		if rt.RefreshSeconds <= 0 {
			return nil, fmt.Errorf("dashconfig: %s: tab %q has non-positive refresh_seconds %d", sourceLabel, id, rt.RefreshSeconds)
		}
		visible := true
		if rt.Visible != nil {
			visible = *rt.Visible
		}
		cfg.Tabs = append(cfg.Tabs, TabConfig{
			ID:             id,
			Visible:        visible,
			Order:          rt.Order,
			RefreshSeconds: rt.RefreshSeconds,
		})
	}

	// Sort tabs by Order so downstream consumers (resolver / seeder /
	// API GET) iterate deterministically without re-sorting.
	sort.SliceStable(cfg.Tabs, func(i, j int) bool {
		return cfg.Tabs[i].Order < cfg.Tabs[j].Order
	})

	// Display section — validate with sensible fallbacks for omitted
	// fields. Theme + density default to "light" + "comfortable" if
	// the YAML omits them entirely; an EXPLICIT invalid value (e.g.
	// `theme: cerulean`) is a hard error.
	theme := Theme(raw.Display.Theme)
	if raw.Display.Theme == "" {
		theme = ThemeLight
	} else if !theme.IsValid() {
		return nil, fmt.Errorf("dashconfig: %s: invalid theme %q (want light, dark, or system)", sourceLabel, raw.Display.Theme)
	}
	density := Density(raw.Display.Density)
	if raw.Display.Density == "" {
		density = DensityComfortable
	} else if !density.IsValid() {
		return nil, fmt.Errorf("dashconfig: %s: invalid density %q (want compact or comfortable)", sourceLabel, raw.Display.Density)
	}
	pagination := raw.Display.PerTablePagination
	if pagination == 0 {
		pagination = 50
	}
	if pagination < 0 {
		return nil, fmt.Errorf("dashconfig: %s: per_table_pagination %d is negative", sourceLabel, pagination)
	}
	defaultSort := raw.Display.DefaultSort
	if defaultSort == nil {
		defaultSort = map[string]string{}
	}
	cfg.Display = DisplayConfig{
		Theme:              theme,
		Density:            density,
		DefaultSort:        defaultSort,
		PerTablePagination: pagination,
	}

	return cfg, nil
}

// ── Global config holder ──────────────────────────────────────────────────────

// configHolder owns the in-process YAML config. Mirrors the pattern in
// internal/notify (configHolder + SetGlobalConfig + GetGlobalConfig).
// The daemon-startup seeder calls SetGlobalConfig once; the resolver
// reads it on every call.
type configHolder struct {
	mu  sync.RWMutex
	cfg *DashboardConfig
}

var globalConfig configHolder

// SetGlobalConfig stores the parsed config in the package-level
// holder. Called once from the daemon-startup seeder; tests call it
// per-test to install a synthetic Config.
func SetGlobalConfig(cfg *DashboardConfig) {
	globalConfig.mu.Lock()
	defer globalConfig.mu.Unlock()
	globalConfig.cfg = cfg
}

// GetGlobalConfig returns the currently installed Config, or nil if
// SetGlobalConfig has never been called. Resolvers tolerate a nil
// config by returning ErrNoConfig.
func GetGlobalConfig() *DashboardConfig {
	globalConfig.mu.RLock()
	defer globalConfig.mu.RUnlock()
	return globalConfig.cfg
}

// ErrNoConfig is returned when no Config has been installed via
// SetGlobalConfig. The daemon-startup seeder is responsible for
// installing a config before any resolver runs.
var ErrNoConfig = fmt.Errorf("dashconfig: no Config installed (call SetGlobalConfig at daemon startup)")
