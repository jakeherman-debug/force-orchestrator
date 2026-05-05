package dashconfig

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestDashConfig_LoadConfig_RealFile parses the canonical
// config/dashboard.yaml shipped with the repo. Every assertion below
// is structural — if it fails after a YAML edit, the edit removed a
// tab the substrate guarantees exists.
func TestDashConfig_LoadConfig_RealFile(t *testing.T) {
	root := repoRoot(t)
	cfg, err := LoadConfig(filepath.Join(root, "config", "dashboard.yaml"))
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.Version != 1 {
		t.Errorf("version: got %d, want 1", cfg.Version)
	}
	// Every tab in the SPA must be in the YAML.
	wantTabs := []string{
		"tasks", "escalations", "convoys", "agents", "mail",
		"knowledge", "experiments", "ec", "security",
		"arch-health", "senate", "notifications", "logs",
	}
	for _, tab := range wantTabs {
		if _, ok := cfg.TabByID(tab); !ok {
			t.Errorf("tab %q missing from real YAML", tab)
		}
	}
	// Display defaults are sane.
	if cfg.Display.Theme != ThemeLight {
		t.Errorf("theme: got %q, want light", cfg.Display.Theme)
	}
	if cfg.Display.Density != DensityComfortable {
		t.Errorf("density: got %q, want comfortable", cfg.Display.Density)
	}
	if cfg.Display.PerTablePagination != 50 {
		t.Errorf("pagination: got %d, want 50", cfg.Display.PerTablePagination)
	}
	if cfg.Display.DefaultSort["tasks"] != "created_at_desc" {
		t.Errorf("default_sort.tasks: got %q, want created_at_desc", cfg.Display.DefaultSort["tasks"])
	}
	// Saved filters must be empty (substrate ships empty).
	if len(cfg.SavedFilters) != 0 {
		t.Errorf("saved_filters: got %d entries, want 0", len(cfg.SavedFilters))
	}
}

// TestDashConfig_TabIDsMatchSPA cross-checks the YAML tabs against the
// SPA tab bar (data-tab="..."). Drift in either direction (tab in
// SPA but not YAML; tab in YAML but not SPA) fails the test so the
// resolver doesn't silently surface stale state.
func TestDashConfig_TabIDsMatchSPA(t *testing.T) {
	root := repoRoot(t)
	cfg, err := LoadConfig(filepath.Join(root, "config", "dashboard.yaml"))
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	indexPath := filepath.Join(root, "internal", "dashboard", "static", "index.html")
	indexBytes, err := os.ReadFile(indexPath)
	if err != nil {
		t.Fatalf("read index.html: %v", err)
	}
	indexStr := string(indexBytes)
	yamlTabs := map[string]struct{}{}
	for _, t := range cfg.Tabs {
		yamlTabs[t.ID] = struct{}{}
	}
	// Pull `data-tab="<id>"` occurrences out of the SPA tab bar. The
	// reflection sub-tabs use `data-reflection-tab=` so they don't
	// match this pattern.
	spaTabs := extractDataTabIDs(indexStr)
	for _, tab := range spaTabs {
		if _, ok := yamlTabs[tab]; !ok {
			t.Errorf("SPA tab %q is missing from config/dashboard.yaml", tab)
		}
	}
	for tab := range yamlTabs {
		if !contains(spaTabs, tab) {
			t.Errorf("YAML tab %q is not in the SPA tab bar (data-tab attribute)", tab)
		}
	}
}

// extractDataTabIDs returns every `data-tab="..."` value occurring in
// the SPA HTML. Filters out the `data-tab-` prefixed attributes
// (data-tab-id, etc.) and reflection sub-tabs.
func extractDataTabIDs(html string) []string {
	var out []string
	for {
		idx := strings.Index(html, `data-tab="`)
		if idx < 0 {
			break
		}
		// Reject prefix matches like `data-tab-id="..."` — the literal
		// we want has the closing quote before any -.
		html = html[idx+len(`data-tab="`):]
		end := strings.Index(html, `"`)
		if end < 0 {
			break
		}
		val := html[:end]
		if val != "" {
			out = append(out, val)
		}
		html = html[end:]
	}
	return out
}

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

// TestDashConfig_ParseConfig_Synthetic exercises the parser with
// crafted inputs covering valid + each invalid case.
func TestDashConfig_ParseConfig_Synthetic(t *testing.T) {
	cases := []struct {
		name      string
		yaml      string
		wantErr   bool
		errSubstr string
	}{
		{
			name: "minimal-valid",
			yaml: `
version: 1
tabs:
  - id: tasks
    order: 1
    refresh_seconds: 5
display:
  theme: light
  density: comfortable
  per_table_pagination: 50
saved_filters: []
`,
		},
		{
			name: "explicit-visible-false",
			yaml: `
version: 1
tabs:
  - id: tasks
    visible: false
    order: 1
    refresh_seconds: 5
display: {}
saved_filters: []
`,
		},
		{
			name: "missing-version",
			yaml: `
tabs:
  - id: tasks
    order: 1
    refresh_seconds: 5
display: {}
`,
			wantErr:   true,
			errSubstr: "unsupported version 0",
		},
		{
			name: "wrong-version",
			yaml: `
version: 2
tabs:
  - id: tasks
    order: 1
    refresh_seconds: 5
display: {}
`,
			wantErr:   true,
			errSubstr: "unsupported version 2",
		},
		{
			name: "no-tabs",
			yaml: `
version: 1
tabs: []
display: {}
`,
			wantErr:   true,
			errSubstr: "no tabs declared",
		},
		{
			name: "empty-tab-id",
			yaml: `
version: 1
tabs:
  - id: ""
    order: 1
    refresh_seconds: 5
display: {}
`,
			wantErr:   true,
			errSubstr: "tab with empty id",
		},
		{
			name: "duplicate-tab-id",
			yaml: `
version: 1
tabs:
  - id: tasks
    order: 1
    refresh_seconds: 5
  - id: tasks
    order: 2
    refresh_seconds: 5
display: {}
`,
			wantErr:   true,
			errSubstr: "duplicate tab id",
		},
		{
			name: "negative-order",
			yaml: `
version: 1
tabs:
  - id: tasks
    order: -1
    refresh_seconds: 5
display: {}
`,
			wantErr:   true,
			errSubstr: "non-positive order",
		},
		{
			name: "zero-order",
			yaml: `
version: 1
tabs:
  - id: tasks
    order: 0
    refresh_seconds: 5
display: {}
`,
			wantErr:   true,
			errSubstr: "non-positive order",
		},
		{
			name: "duplicate-order",
			yaml: `
version: 1
tabs:
  - id: tasks
    order: 1
    refresh_seconds: 5
  - id: convoys
    order: 1
    refresh_seconds: 5
display: {}
`,
			wantErr:   true,
			errSubstr: "share order 1",
		},
		{
			name: "negative-refresh",
			yaml: `
version: 1
tabs:
  - id: tasks
    order: 1
    refresh_seconds: -1
display: {}
`,
			wantErr:   true,
			errSubstr: "non-positive refresh_seconds",
		},
		{
			name: "zero-refresh",
			yaml: `
version: 1
tabs:
  - id: tasks
    order: 1
    refresh_seconds: 0
display: {}
`,
			wantErr:   true,
			errSubstr: "non-positive refresh_seconds",
		},
		{
			name: "invalid-theme",
			yaml: `
version: 1
tabs:
  - id: tasks
    order: 1
    refresh_seconds: 5
display:
  theme: cerulean
`,
			wantErr:   true,
			errSubstr: "invalid theme",
		},
		{
			name: "invalid-density",
			yaml: `
version: 1
tabs:
  - id: tasks
    order: 1
    refresh_seconds: 5
display:
  density: spacious
`,
			wantErr:   true,
			errSubstr: "invalid density",
		},
		{
			name: "negative-pagination",
			yaml: `
version: 1
tabs:
  - id: tasks
    order: 1
    refresh_seconds: 5
display:
  per_table_pagination: -10
`,
			wantErr:   true,
			errSubstr: "negative",
		},
		{
			name: "malformed-yaml",
			yaml: `
version: 1
tabs:
  - id: tasks
    order: [bogus
`,
			wantErr:   true,
			errSubstr: "parse",
		},
		{
			name: "valid-dark-compact",
			yaml: `
version: 1
tabs:
  - id: tasks
    order: 1
    refresh_seconds: 5
display:
  theme: dark
  density: compact
  per_table_pagination: 100
  default_sort:
    tasks: id_asc
`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := ParseConfig([]byte(tc.yaml), "synthetic.yaml")
			if tc.wantErr {
				if err == nil {
					t.Fatalf("ParseConfig: want error, got nil (cfg=%+v)", cfg)
				}
				if tc.errSubstr != "" && !strings.Contains(err.Error(), tc.errSubstr) {
					t.Errorf("ParseConfig error = %q, want substring %q", err.Error(), tc.errSubstr)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseConfig: %v", err)
			}
			if cfg == nil {
				t.Fatal("ParseConfig: nil cfg + nil err")
			}
		})
	}
}

// TestDashConfig_LoadConfig_MissingFile — file-not-found path returns
// a wrapped error rather than a panic.
func TestDashConfig_LoadConfig_MissingFile(t *testing.T) {
	cfg, err := LoadConfig("/tmp/dashconfig-this-does-not-exist.yaml")
	if err == nil {
		t.Fatal("LoadConfig: want error for missing file, got nil")
	}
	if cfg != nil {
		t.Errorf("LoadConfig: want nil cfg on error, got %+v", cfg)
	}
}

// TestDashConfig_TabsSortedByOrder — parser sorts tabs by Order so
// downstream consumers don't have to.
func TestDashConfig_TabsSortedByOrder(t *testing.T) {
	yaml := `
version: 1
tabs:
  - id: third
    order: 3
    refresh_seconds: 5
  - id: first
    order: 1
    refresh_seconds: 5
  - id: second
    order: 2
    refresh_seconds: 5
display: {}
`
	cfg, err := ParseConfig([]byte(yaml), "ordered.yaml")
	if err != nil {
		t.Fatalf("ParseConfig: %v", err)
	}
	got := cfg.TabIDs()
	want := []string{"first", "second", "third"}
	for i, id := range want {
		if got[i] != id {
			t.Errorf("TabIDs[%d] = %q, want %q (full=%v)", i, got[i], id, got)
		}
	}
}

// TestDashConfig_GlobalConfigHolder — Set/Get round-trips, default
// nil. Mirrors the pattern in internal/notify.
func TestDashConfig_GlobalConfigHolder(t *testing.T) {
	// Reset to nil after the test so we don't leak state into
	// subsequent runs (parallel test packages). The cfg holder is
	// global; a t.Cleanup restoration is the safe shape.
	prev := GetGlobalConfig()
	t.Cleanup(func() { SetGlobalConfig(prev) })

	SetGlobalConfig(nil)
	if GetGlobalConfig() != nil {
		t.Fatal("after SetGlobalConfig(nil): want nil, got non-nil")
	}
	cfg := &DashboardConfig{Version: 1}
	SetGlobalConfig(cfg)
	if got := GetGlobalConfig(); got != cfg {
		t.Fatalf("GetGlobalConfig: got %+v, want %+v", got, cfg)
	}
}

// repoRoot finds the repo root by walking up from the current test's
// working directory until a go.mod is found.
func repoRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	dir := wd
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("no go.mod above %s", wd)
		}
		dir = parent
	}
}
