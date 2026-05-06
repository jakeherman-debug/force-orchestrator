// saved_filters_test.go — D11 Phase 3 sub-task C.
//
// Coverage:
//   - YAML parser: valid saved_filters block + every documented reject case.
//   - Seeder: yaml-source vs dashboard-source distinction (yaml rows replaced
//     across daemon restart; dashboard rows preserved).
//   - List helpers: per-tab + dashboard-source-only.
package dashconfig

import (
	"strings"
	"testing"

	"force-orchestrator/internal/store"
)

const baseYAMLForFilters = `
version: 1
tabs:
  - id: tasks
    visible: true
    order: 1
    refresh_seconds: 5
  - id: convoys
    visible: true
    order: 2
    refresh_seconds: 10
display:
  theme: light
  density: comfortable
  per_table_pagination: 50
`

// TestSavedFilter_Parser_HappyPath — a fully-populated saved_filters
// block parses + survives validation. Empty SavedFilters list is fine.
func TestSavedFilter_Parser_HappyPath(t *testing.T) {
	yaml := baseYAMLForFilters + `
saved_filters:
  - name: My active convoys
    tab: convoys
    description: Active or DraftPROpen
    filter:
      status:
        - Active
        - DraftPROpen
    sort_by: id
    sort_dir: desc
  - name: All tasks
    tab: tasks
    filter:
      status:
        - Pending
`
	cfg, err := ParseConfig([]byte(yaml), "test.yaml")
	if err != nil {
		t.Fatalf("ParseConfig: unexpected error: %v", err)
	}
	if len(cfg.SavedFilters) != 2 {
		t.Fatalf("SavedFilters len = %d, want 2", len(cfg.SavedFilters))
	}
	if cfg.SavedFilters[0].Name != "My active convoys" {
		t.Errorf("name[0] = %q", cfg.SavedFilters[0].Name)
	}
	if cfg.SavedFilters[0].SortDir != "desc" {
		t.Errorf("sort_dir[0] = %q", cfg.SavedFilters[0].SortDir)
	}
	if got := cfg.SavedFilters[0].Filter["status"]; len(got) != 2 || got[0] != "Active" {
		t.Errorf("filter[0].status = %v", got)
	}
}

func TestSavedFilter_Parser_RejectsEmptyName(t *testing.T) {
	yaml := baseYAMLForFilters + `
saved_filters:
  - name: ""
    tab: convoys
    filter:
      status:
        - Active
`
	if _, err := ParseConfig([]byte(yaml), "test.yaml"); err == nil || !strings.Contains(err.Error(), "empty name") {
		t.Errorf("expected empty-name error, got %v", err)
	}
}

func TestSavedFilter_Parser_RejectsUnknownTab(t *testing.T) {
	yaml := baseYAMLForFilters + `
saved_filters:
  - name: foo
    tab: nope
    filter:
      x:
        - y
`
	if _, err := ParseConfig([]byte(yaml), "test.yaml"); err == nil || !strings.Contains(err.Error(), "unknown tab") {
		t.Errorf("expected unknown-tab error, got %v", err)
	}
}

func TestSavedFilter_Parser_RejectsEmptyFilter(t *testing.T) {
	yaml := baseYAMLForFilters + `
saved_filters:
  - name: foo
    tab: convoys
    filter: {}
`
	if _, err := ParseConfig([]byte(yaml), "test.yaml"); err == nil || !strings.Contains(err.Error(), "empty filter") {
		t.Errorf("expected empty-filter error, got %v", err)
	}
}

func TestSavedFilter_Parser_RejectsDuplicateNameTabPair(t *testing.T) {
	yaml := baseYAMLForFilters + `
saved_filters:
  - name: dup
    tab: convoys
    filter:
      status:
        - Active
  - name: dup
    tab: convoys
    filter:
      status:
        - Closed
`
	if _, err := ParseConfig([]byte(yaml), "test.yaml"); err == nil || !strings.Contains(err.Error(), "duplicate saved_filter") {
		t.Errorf("expected duplicate error, got %v", err)
	}
}

func TestSavedFilter_Parser_AcceptsSameNameDifferentTabs(t *testing.T) {
	yaml := baseYAMLForFilters + `
saved_filters:
  - name: My pinned
    tab: convoys
    filter:
      status:
        - Active
  - name: My pinned
    tab: tasks
    filter:
      status:
        - Pending
`
	cfg, err := ParseConfig([]byte(yaml), "test.yaml")
	if err != nil {
		t.Fatalf("ParseConfig: unexpected error: %v", err)
	}
	if len(cfg.SavedFilters) != 2 {
		t.Errorf("len = %d, want 2", len(cfg.SavedFilters))
	}
}

func TestSavedFilter_Parser_RejectsInvalidSortDir(t *testing.T) {
	yaml := baseYAMLForFilters + `
saved_filters:
  - name: foo
    tab: convoys
    filter:
      status:
        - Active
    sort_dir: sideways
`
	if _, err := ParseConfig([]byte(yaml), "test.yaml"); err == nil || !strings.Contains(err.Error(), "invalid sort_dir") {
		t.Errorf("expected invalid-sort_dir error, got %v", err)
	}
}

func TestSavedFilter_Parser_AcceptsEmptySection(t *testing.T) {
	cfg, err := ParseConfig([]byte(baseYAMLForFilters+"saved_filters: []\n"), "test.yaml")
	if err != nil {
		t.Fatalf("ParseConfig: unexpected error: %v", err)
	}
	if len(cfg.SavedFilters) != 0 {
		t.Errorf("expected zero filters, got %d", len(cfg.SavedFilters))
	}
}

// ── Seeder tests ─────────────────────────────────────────────────────────────

func TestSeedSavedFiltersFromYAML_InsertsRows(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	cfg, err := ParseConfig([]byte(baseYAMLForFilters+`
saved_filters:
  - name: My active
    tab: convoys
    description: hot stuff
    filter:
      status:
        - Active
    sort_by: id
    sort_dir: desc
`), "test.yaml")
	if err != nil {
		t.Fatalf("ParseConfig: %v", err)
	}
	if err := SeedSavedFiltersFromYAML(db, cfg); err != nil {
		t.Fatalf("SeedSavedFiltersFromYAML: %v", err)
	}
	rows, err := ListSavedFilters(db, "convoys")
	if err != nil {
		t.Fatalf("ListSavedFilters: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(rows))
	}
	if rows[0].Source != "yaml" {
		t.Errorf("source = %q, want yaml", rows[0].Source)
	}
	if rows[0].Description != "hot stuff" {
		t.Errorf("description = %q", rows[0].Description)
	}
}

// TestSeedSavedFiltersFromYAML_PreservesDashboardSourceRows — the
// seeder MUST NOT touch source='dashboard' rows. This is the core
// anti-cheat: operator-saved filters survive a daemon restart even
// when they are absent from the YAML.
func TestSeedSavedFiltersFromYAML_PreservesDashboardSourceRows(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	cfg, _ := ParseConfig([]byte(baseYAMLForFilters+"saved_filters: []\n"), "test.yaml")

	// Insert a dashboard-source row first.
	_, err := db.Exec(
		`INSERT INTO DashboardSavedFilters
		   (name, tab, description, filter_json, sort_by, sort_dir, source, created_by)
		 VALUES (?, ?, ?, ?, '', '', 'dashboard', 'jake')`,
		"my custom", "convoys", "operator-only", `{"status":["Active"]}`,
	)
	if err != nil {
		t.Fatalf("insert dashboard row: %v", err)
	}
	if err := SeedSavedFiltersFromYAML(db, cfg); err != nil {
		t.Fatalf("seed: %v", err)
	}
	rows, _ := ListSavedFilters(db, "convoys")
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(rows))
	}
	if rows[0].Source != "dashboard" {
		t.Errorf("source = %q, want dashboard (must be preserved)", rows[0].Source)
	}
}

// TestSeedSavedFiltersFromYAML_DeletesStaleYAMLRows — yaml-source rows
// whose name+tab is no longer in the YAML get DELETED on reseed.
func TestSeedSavedFiltersFromYAML_DeletesStaleYAMLRows(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	// Initial seed has two YAML filters.
	cfg1, _ := ParseConfig([]byte(baseYAMLForFilters+`
saved_filters:
  - name: keep me
    tab: convoys
    filter:
      status:
        - Active
  - name: stale
    tab: convoys
    filter:
      status:
        - Closed
`), "test.yaml")
	if err := SeedSavedFiltersFromYAML(db, cfg1); err != nil {
		t.Fatalf("first seed: %v", err)
	}
	if rows, _ := ListSavedFilters(db, "convoys"); len(rows) != 2 {
		t.Fatalf("after first seed rows = %d, want 2", len(rows))
	}

	// YAML evolves — drop "stale".
	cfg2, _ := ParseConfig([]byte(baseYAMLForFilters+`
saved_filters:
  - name: keep me
    tab: convoys
    filter:
      status:
        - Active
`), "test.yaml")
	if err := SeedSavedFiltersFromYAML(db, cfg2); err != nil {
		t.Fatalf("second seed: %v", err)
	}
	rows, _ := ListSavedFilters(db, "convoys")
	if len(rows) != 1 {
		t.Fatalf("after second seed rows = %d, want 1", len(rows))
	}
	if rows[0].Name != "keep me" {
		t.Errorf("survivor = %q, want keep me", rows[0].Name)
	}
}

// TestSeedSavedFiltersFromYAML_RefusesYAMLDashboardCollision — a yaml
// filter that wants a (name, tab) tuple already owned by a dashboard
// row aborts the seed (operator must rename or delete first).
func TestSeedSavedFiltersFromYAML_RefusesYAMLDashboardCollision(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	_, err := db.Exec(
		`INSERT INTO DashboardSavedFilters
		   (name, tab, filter_json, source, created_by)
		 VALUES (?, ?, ?, 'dashboard', 'jake')`,
		"colliding", "convoys", `{"x":["y"]}`,
	)
	if err != nil {
		t.Fatalf("insert dashboard row: %v", err)
	}
	cfg, _ := ParseConfig([]byte(baseYAMLForFilters+`
saved_filters:
  - name: colliding
    tab: convoys
    filter:
      status:
        - Active
`), "test.yaml")
	if err := SeedSavedFiltersFromYAML(db, cfg); err == nil || !strings.Contains(err.Error(), "collides") {
		t.Errorf("expected collision error, got %v", err)
	}
}

// TestSeedSavedFiltersFromYAML_Idempotent — re-seeding identical
// YAML twice produces the same DB state.
func TestSeedSavedFiltersFromYAML_Idempotent(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	cfg, _ := ParseConfig([]byte(baseYAMLForFilters+`
saved_filters:
  - name: stable
    tab: convoys
    filter:
      status:
        - Active
`), "test.yaml")
	for i := 0; i < 2; i++ {
		if err := SeedSavedFiltersFromYAML(db, cfg); err != nil {
			t.Fatalf("seed iter %d: %v", i, err)
		}
	}
	rows, _ := ListSavedFilters(db, "convoys")
	if len(rows) != 1 {
		t.Errorf("rows = %d, want 1 (idempotent)", len(rows))
	}
}

func TestListDashboardSourceFilters_OnlyReturnsDashboardSource(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	_, _ = db.Exec(
		`INSERT INTO DashboardSavedFilters (name, tab, filter_json, source, created_by)
		 VALUES ('y1', 'convoys', '{"x":["y"]}', 'yaml', 'yaml'),
		        ('d1', 'convoys', '{"x":["y"]}', 'dashboard', 'jake'),
		        ('d2', 'tasks',   '{"x":["y"]}', 'dashboard', 'jake')`,
	)
	rows, err := ListDashboardSourceFilters(db)
	if err != nil {
		t.Fatalf("ListDashboardSourceFilters: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2", len(rows))
	}
	for _, r := range rows {
		if r.Source != "dashboard" {
			t.Errorf("non-dashboard source leaked into results: %+v", r)
		}
	}
}
