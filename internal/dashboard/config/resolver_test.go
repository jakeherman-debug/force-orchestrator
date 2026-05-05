package dashconfig

import (
	"testing"

	"force-orchestrator/internal/store"
)

// makeTestConfig returns a small, complete DashboardConfig for use by
// the resolver + seeder tests.
func makeTestConfig(t *testing.T) *DashboardConfig {
	t.Helper()
	yaml := `
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
  - id: logs
    visible: false
    order: 3
    refresh_seconds: 5
display:
  theme: light
  density: comfortable
  default_sort:
    tasks: created_at_desc
    convoys: id_desc
  per_table_pagination: 50
saved_filters: []
`
	cfg, err := ParseConfig([]byte(yaml), "test.yaml")
	if err != nil {
		t.Fatalf("makeTestConfig parse: %v", err)
	}
	return cfg
}

// TestDashConfig_ResolveTabConfig_NoConfig — fail-closed when no
// config is installed.
func TestDashConfig_ResolveTabConfig_NoConfig(t *testing.T) {
	prev := GetGlobalConfig()
	t.Cleanup(func() { SetGlobalConfig(prev) })
	SetGlobalConfig(nil)

	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	_, err := ResolveTabConfig(db, "tasks")
	if err != ErrNoConfig {
		t.Errorf("ResolveTabConfig: want ErrNoConfig, got %v", err)
	}
}

// TestDashConfig_ResolveTabConfig_UnknownTab — fail-closed when the
// tab id isn't registered.
func TestDashConfig_ResolveTabConfig_UnknownTab(t *testing.T) {
	prev := GetGlobalConfig()
	t.Cleanup(func() { SetGlobalConfig(prev) })
	SetGlobalConfig(makeTestConfig(t))

	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	_, err := ResolveTabConfig(db, "no-such-tab")
	if err == nil {
		t.Fatal("ResolveTabConfig: want error, got nil")
	}
	var ute ErrUnknownTab
	if e, ok := err.(ErrUnknownTab); !ok {
		t.Errorf("ResolveTabConfig error type = %T, want ErrUnknownTab", err)
	} else {
		ute = e
		if ute.TabID != "no-such-tab" {
			t.Errorf("ErrUnknownTab.TabID = %q, want 'no-such-tab'", ute.TabID)
		}
	}
}

// TestDashConfig_ResolveTabConfig_YAMLDefault — no SystemConfig
// override → YAML default wins.
func TestDashConfig_ResolveTabConfig_YAMLDefault(t *testing.T) {
	prev := GetGlobalConfig()
	t.Cleanup(func() { SetGlobalConfig(prev) })
	SetGlobalConfig(makeTestConfig(t))

	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	got, err := ResolveTabConfig(db, "tasks")
	if err != nil {
		t.Fatalf("ResolveTabConfig: %v", err)
	}
	want := TabConfig{ID: "tasks", Visible: true, Order: 1, RefreshSeconds: 5}
	if got != want {
		t.Errorf("ResolveTabConfig: got %+v, want %+v", got, want)
	}
}

// TestDashConfig_ResolveTabConfig_VisibleOverride — operator-set
// visibility wins over YAML default. Tests both directions
// (default-true → false; default-false → true).
func TestDashConfig_ResolveTabConfig_VisibleOverride(t *testing.T) {
	prev := GetGlobalConfig()
	t.Cleanup(func() { SetGlobalConfig(prev) })
	SetGlobalConfig(makeTestConfig(t))

	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	// tasks is visible by default; hide it.
	store.SetConfig(db, ConfigKeyTabVisiblePrefix+"tasks", "0")
	got, err := ResolveTabConfig(db, "tasks")
	if err != nil {
		t.Fatalf("ResolveTabConfig: %v", err)
	}
	if got.Visible {
		t.Errorf("after override 0: Visible = true, want false")
	}

	// logs is hidden by default; show it.
	store.SetConfig(db, ConfigKeyTabVisiblePrefix+"logs", "1")
	got, err = ResolveTabConfig(db, "logs")
	if err != nil {
		t.Fatalf("ResolveTabConfig: %v", err)
	}
	if !got.Visible {
		t.Errorf("after override 1: Visible = false, want true")
	}
}

// TestDashConfig_ResolveTabConfig_OrderOverride — operator-set order
// wins.
func TestDashConfig_ResolveTabConfig_OrderOverride(t *testing.T) {
	prev := GetGlobalConfig()
	t.Cleanup(func() { SetGlobalConfig(prev) })
	SetGlobalConfig(makeTestConfig(t))

	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	store.SetConfig(db, ConfigKeyTabOrderPrefix+"tasks", "99")
	got, err := ResolveTabConfig(db, "tasks")
	if err != nil {
		t.Fatalf("ResolveTabConfig: %v", err)
	}
	if got.Order != 99 {
		t.Errorf("Order = %d, want 99", got.Order)
	}
}

// TestDashConfig_ResolveTabConfig_RefreshOverride — operator-set
// refresh wins.
func TestDashConfig_ResolveTabConfig_RefreshOverride(t *testing.T) {
	prev := GetGlobalConfig()
	t.Cleanup(func() { SetGlobalConfig(prev) })
	SetGlobalConfig(makeTestConfig(t))

	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	store.SetConfig(db, ConfigKeyTabRefreshPrefix+"tasks", "42")
	got, err := ResolveTabConfig(db, "tasks")
	if err != nil {
		t.Fatalf("ResolveTabConfig: %v", err)
	}
	if got.RefreshSeconds != 42 {
		t.Errorf("RefreshSeconds = %d, want 42", got.RefreshSeconds)
	}
}

// TestDashConfig_ResolveTabConfig_MalformedOverride — corrupt
// SystemConfig values fall through to YAML default rather than
// surfacing an error or breaking the dashboard.
func TestDashConfig_ResolveTabConfig_MalformedOverride(t *testing.T) {
	prev := GetGlobalConfig()
	t.Cleanup(func() { SetGlobalConfig(prev) })
	SetGlobalConfig(makeTestConfig(t))

	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	store.SetConfig(db, ConfigKeyTabOrderPrefix+"tasks", "banana")
	store.SetConfig(db, ConfigKeyTabRefreshPrefix+"tasks", "-7")
	got, err := ResolveTabConfig(db, "tasks")
	if err != nil {
		t.Fatalf("ResolveTabConfig: %v", err)
	}
	if got.Order != 1 {
		t.Errorf("Order = %d, want 1 (YAML default)", got.Order)
	}
	if got.RefreshSeconds != 5 {
		t.Errorf("RefreshSeconds = %d, want 5 (YAML default)", got.RefreshSeconds)
	}
}

// TestDashConfig_ResolveAllTabs — iterates every YAML tab + applies
// overrides + sorts by resolved order.
func TestDashConfig_ResolveAllTabs(t *testing.T) {
	prev := GetGlobalConfig()
	t.Cleanup(func() { SetGlobalConfig(prev) })
	SetGlobalConfig(makeTestConfig(t))

	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	// Push tasks to the end with an override.
	store.SetConfig(db, ConfigKeyTabOrderPrefix+"tasks", "999")
	tabs, err := ResolveAllTabs(db)
	if err != nil {
		t.Fatalf("ResolveAllTabs: %v", err)
	}
	if len(tabs) != 3 {
		t.Fatalf("ResolveAllTabs: got %d tabs, want 3", len(tabs))
	}
	// Resolved-order ascending: convoys(2), logs(3), tasks(999).
	wantOrder := []string{"convoys", "logs", "tasks"}
	for i, id := range wantOrder {
		if tabs[i].ID != id {
			t.Errorf("tabs[%d].ID = %q, want %q (full=%v)", i, tabs[i].ID, id, tabs)
		}
	}
}

// TestDashConfig_ResolveDisplayConfig_YAMLDefault — no overrides, YAML
// defaults win.
func TestDashConfig_ResolveDisplayConfig_YAMLDefault(t *testing.T) {
	prev := GetGlobalConfig()
	t.Cleanup(func() { SetGlobalConfig(prev) })
	SetGlobalConfig(makeTestConfig(t))

	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	got, err := ResolveDisplayConfig(db)
	if err != nil {
		t.Fatalf("ResolveDisplayConfig: %v", err)
	}
	if got.Theme != ThemeLight {
		t.Errorf("Theme = %q, want light", got.Theme)
	}
	if got.Density != DensityComfortable {
		t.Errorf("Density = %q, want comfortable", got.Density)
	}
	if got.PerTablePagination != 50 {
		t.Errorf("Pagination = %d, want 50", got.PerTablePagination)
	}
	if got.DefaultSort["tasks"] != "created_at_desc" {
		t.Errorf("DefaultSort[tasks] = %q, want created_at_desc", got.DefaultSort["tasks"])
	}
}

// TestDashConfig_ResolveDisplayConfig_AllOverrides — operator
// overrides win for theme + density + pagination + per-tab sort.
func TestDashConfig_ResolveDisplayConfig_AllOverrides(t *testing.T) {
	prev := GetGlobalConfig()
	t.Cleanup(func() { SetGlobalConfig(prev) })
	SetGlobalConfig(makeTestConfig(t))

	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	store.SetConfig(db, ConfigKeyDisplayTheme, "dark")
	store.SetConfig(db, ConfigKeyDisplayDensity, "compact")
	store.SetConfig(db, ConfigKeyDisplayPagSize, "200")
	store.SetConfig(db, ConfigKeyDisplaySortPfx+"tasks", "id_asc")

	got, err := ResolveDisplayConfig(db)
	if err != nil {
		t.Fatalf("ResolveDisplayConfig: %v", err)
	}
	if got.Theme != ThemeDark {
		t.Errorf("Theme = %q, want dark", got.Theme)
	}
	if got.Density != DensityCompact {
		t.Errorf("Density = %q, want compact", got.Density)
	}
	if got.PerTablePagination != 200 {
		t.Errorf("Pagination = %d, want 200", got.PerTablePagination)
	}
	if got.DefaultSort["tasks"] != "id_asc" {
		t.Errorf("DefaultSort[tasks] = %q, want id_asc", got.DefaultSort["tasks"])
	}
	// convoys default sort untouched.
	if got.DefaultSort["convoys"] != "id_desc" {
		t.Errorf("DefaultSort[convoys] = %q, want id_desc (untouched)", got.DefaultSort["convoys"])
	}
}

// TestDashConfig_ResolveDisplayConfig_MalformedOverride — invalid
// theme/density values fall through to YAML default.
func TestDashConfig_ResolveDisplayConfig_MalformedOverride(t *testing.T) {
	prev := GetGlobalConfig()
	t.Cleanup(func() { SetGlobalConfig(prev) })
	SetGlobalConfig(makeTestConfig(t))

	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	store.SetConfig(db, ConfigKeyDisplayTheme, "cerulean")
	store.SetConfig(db, ConfigKeyDisplayDensity, "spacious")
	store.SetConfig(db, ConfigKeyDisplayPagSize, "banana")

	got, err := ResolveDisplayConfig(db)
	if err != nil {
		t.Fatalf("ResolveDisplayConfig: %v", err)
	}
	if got.Theme != ThemeLight {
		t.Errorf("Theme = %q, want light (YAML default)", got.Theme)
	}
	if got.Density != DensityComfortable {
		t.Errorf("Density = %q, want comfortable (YAML default)", got.Density)
	}
	if got.PerTablePagination != 50 {
		t.Errorf("Pagination = %d, want 50 (YAML default)", got.PerTablePagination)
	}
}

// TestDashConfig_ResolveDisplayConfig_NoConfig — fail-closed when no
// config is installed.
func TestDashConfig_ResolveDisplayConfig_NoConfig(t *testing.T) {
	prev := GetGlobalConfig()
	t.Cleanup(func() { SetGlobalConfig(prev) })
	SetGlobalConfig(nil)

	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	_, err := ResolveDisplayConfig(db)
	if err != ErrNoConfig {
		t.Errorf("ResolveDisplayConfig: want ErrNoConfig, got %v", err)
	}
}

// TestDashConfig_ResolveSavedFilters_Empty — substrate ships an empty
// list; ResolveSavedFilters returns an empty slice, never nil.
func TestDashConfig_ResolveSavedFilters_Empty(t *testing.T) {
	prev := GetGlobalConfig()
	t.Cleanup(func() { SetGlobalConfig(prev) })
	SetGlobalConfig(makeTestConfig(t))

	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	out, err := ResolveSavedFilters(db)
	if err != nil {
		t.Fatalf("ResolveSavedFilters: %v", err)
	}
	if out == nil {
		t.Error("ResolveSavedFilters: want empty non-nil slice, got nil")
	}
	if len(out) != 0 {
		t.Errorf("ResolveSavedFilters: got %d entries, want 0", len(out))
	}
}

// TestDashConfig_ResolveDisplayConfig_DoesNotMutateYAML — the
// resolver returns a copy of the YAML default-sort map; mutating the
// returned map must not affect subsequent resolves.
func TestDashConfig_ResolveDisplayConfig_DoesNotMutateYAML(t *testing.T) {
	prev := GetGlobalConfig()
	t.Cleanup(func() { SetGlobalConfig(prev) })
	SetGlobalConfig(makeTestConfig(t))

	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	first, err := ResolveDisplayConfig(db)
	if err != nil {
		t.Fatalf("first resolve: %v", err)
	}
	first.DefaultSort["tasks"] = "GARBAGE"

	second, err := ResolveDisplayConfig(db)
	if err != nil {
		t.Fatalf("second resolve: %v", err)
	}
	if second.DefaultSort["tasks"] == "GARBAGE" {
		t.Error("YAML default-sort map leaked between resolves; second sees mutated value")
	}
}
