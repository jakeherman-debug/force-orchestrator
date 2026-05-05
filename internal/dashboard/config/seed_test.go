package dashconfig

import (
	"testing"

	"force-orchestrator/internal/store"
)

// TestDashConfig_SeedRegistryFromYAML_Roundtrip — fresh DB, seed
// once, every YAML tab present in the registry with the right
// values.
func TestDashConfig_SeedRegistryFromYAML_Roundtrip(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	cfg := makeTestConfig(t)
	if err := SeedRegistryFromYAML(db, cfg); err != nil {
		t.Fatalf("seed: %v", err)
	}
	rows, err := ListRegisteredTabs(db)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("seeded %d rows, want 3 (tasks/convoys/logs)", len(rows))
	}
	// rows are ordered by order_default ASC.
	wantIDs := []string{"tasks", "convoys", "logs"}
	for i, want := range wantIDs {
		if rows[i].TabID != want {
			t.Errorf("rows[%d].TabID = %q, want %q", i, rows[i].TabID, want)
		}
	}
	// tasks: visible=true, order=1, refresh=5
	tasks := rows[0]
	if !tasks.VisibleDefault || tasks.OrderDefault != 1 || tasks.RefreshDefault != 5 {
		t.Errorf("tasks row: %+v", tasks)
	}
	// logs: visible=false
	logs := rows[2]
	if logs.VisibleDefault {
		t.Errorf("logs.VisibleDefault = true, want false")
	}
	if rows[0].YAMLVersion != 1 {
		t.Errorf("yaml_version = %d, want 1", rows[0].YAMLVersion)
	}
}

// TestDashConfig_SeedRegistryFromYAML_Idempotent — re-seeding the
// same config yields the same row count + same registered_at
// (preserved on conflict).
func TestDashConfig_SeedRegistryFromYAML_Idempotent(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	cfg := makeTestConfig(t)
	if err := SeedRegistryFromYAML(db, cfg); err != nil {
		t.Fatalf("first seed: %v", err)
	}
	first, _ := ListRegisteredTabs(db)
	if err := SeedRegistryFromYAML(db, cfg); err != nil {
		t.Fatalf("second seed: %v", err)
	}
	second, _ := ListRegisteredTabs(db)
	if len(first) != len(second) {
		t.Fatalf("row count drift: first=%d second=%d", len(first), len(second))
	}
	for i := range first {
		if first[i].TabID != second[i].TabID {
			t.Errorf("row %d TabID drift: %q -> %q", i, first[i].TabID, second[i].TabID)
		}
		if first[i].RegisteredAt != second[i].RegisteredAt {
			t.Errorf("row %d RegisteredAt changed across re-seed: %q -> %q",
				i, first[i].RegisteredAt, second[i].RegisteredAt)
		}
	}
}

// TestDashConfig_SeedRegistryFromYAML_UpdatesValues — when the YAML
// default flips between daemon restarts, the registry row updates.
func TestDashConfig_SeedRegistryFromYAML_UpdatesValues(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	cfg := makeTestConfig(t)
	if err := SeedRegistryFromYAML(db, cfg); err != nil {
		t.Fatalf("first seed: %v", err)
	}
	// Mutate cfg's tasks tab (simulating an operator editing the
	// YAML between restarts) and re-seed.
	for i := range cfg.Tabs {
		if cfg.Tabs[i].ID == "tasks" {
			cfg.Tabs[i].RefreshSeconds = 99
			cfg.Tabs[i].Visible = false
		}
	}
	if err := SeedRegistryFromYAML(db, cfg); err != nil {
		t.Fatalf("second seed: %v", err)
	}
	rows, _ := ListRegisteredTabs(db)
	for _, r := range rows {
		if r.TabID == "tasks" {
			if r.RefreshDefault != 99 {
				t.Errorf("tasks.RefreshDefault = %d, want 99 (post-update)", r.RefreshDefault)
			}
			if r.VisibleDefault {
				t.Errorf("tasks.VisibleDefault = true, want false (post-update)")
			}
		}
	}
}

// TestDashConfig_SeedRegistryFromYAML_PreservesUnknownTabs — tabs in
// DB but absent from YAML survive a re-seed (operators may have
// removed-then-re-added a tab mid-rollout). Mirrors
// notify.SeedRegistryFromYAML's preservation guarantee.
func TestDashConfig_SeedRegistryFromYAML_PreservesUnknownTabs(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	cfg := makeTestConfig(t)
	// Manually insert a row not in the YAML.
	if _, err := db.Exec(
		`INSERT INTO DashboardCatalogRegistry
		   (tab_id, visible_default, order_default, refresh_default, yaml_version)
		 VALUES ('legacy_tab', 1, 99, 30, 1)`,
	); err != nil {
		t.Fatalf("insert legacy: %v", err)
	}

	if err := SeedRegistryFromYAML(db, cfg); err != nil {
		t.Fatalf("seed: %v", err)
	}
	rows, _ := ListRegisteredTabs(db)
	var sawLegacy bool
	for _, r := range rows {
		if r.TabID == "legacy_tab" {
			sawLegacy = true
		}
	}
	if !sawLegacy {
		t.Error("legacy_tab removed by seeder; should be preserved")
	}
}

// TestDashConfig_SeedRegistryFromYAML_NilConfig — seeder returns a
// readable error rather than panicking on nil.
func TestDashConfig_SeedRegistryFromYAML_NilConfig(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	err := SeedRegistryFromYAML(db, nil)
	if err == nil {
		t.Fatal("seed nil: want error, got nil")
	}
}

// TestDashConfig_ListRegisteredTabs_EmptyTable — empty registry
// returns empty slice + nil error rather than nil slice.
func TestDashConfig_ListRegisteredTabs_EmptyTable(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	rows, err := ListRegisteredTabs(db)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("got %d rows on fresh DB, want 0", len(rows))
	}
}
