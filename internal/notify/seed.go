// internal/notify/seed.go — D11 Phase 1.
//
// Seeds NotificationCategoryRegistry from a parsed YAML Config.
// Called from the daemon-startup path in cmd/force/fleet_cmds.go after
// InitHolocronDSN runs (the table exists by then). Idempotent: re-runs
// upsert categories that already exist (in case the YAML default
// flipped between daemon restarts) and leave categories present in DB
// but absent from YAML alone — operators may have removed-then-re-added
// a category mid-rollout, and we don't want to lose their override
// history by auto-deleting.
package notify

import (
	"database/sql"
	"fmt"
)

// SeedRegistryFromYAML upserts each YAML category into
// NotificationCategoryRegistry. Returns nil on full success; partial
// failures surface as a wrapped error after attempting all rows so the
// daemon-startup path gets a comprehensive picture.
//
// On upsert: tier + yaml_default + description + yaml_version are
// updated (registered_at is preserved on existing rows).
func SeedRegistryFromYAML(db *sql.DB, cfg *Config) error {
	if cfg == nil {
		return fmt.Errorf("notify: SeedRegistryFromYAML called with nil Config")
	}
	var firstErr error
	for _, name := range cfg.CategoryNames() {
		spec := cfg.Categories[name]
		// SQLite UPSERT: insert if missing, update tier/yaml_default/
		// description/yaml_version if present. registered_at is
		// preserved on conflict because it's a "first registration"
		// audit field.
		_, err := db.Exec(
			`INSERT INTO NotificationCategoryRegistry
			   (category, tier, yaml_default, description, yaml_version)
			 VALUES (?, ?, ?, ?, ?)
			 ON CONFLICT(category) DO UPDATE SET
			   tier = excluded.tier,
			   yaml_default = excluded.yaml_default,
			   description = excluded.description,
			   yaml_version = excluded.yaml_version`,
			spec.Name, int(spec.Tier), string(spec.Default), spec.Description, cfg.Version,
		)
		if err != nil && firstErr == nil {
			firstErr = fmt.Errorf("notify: seed category %q: %w", name, err)
		}
	}
	return firstErr
}

// RegistryEntry is the read-side projection of one row from
// NotificationCategoryRegistry. Used by tests + the dashboard.
type RegistryEntry struct {
	ID           int
	Category     string
	Tier         Tier
	YAMLDefault  Setting
	Description  string
	RegisteredAt string
	YAMLVersion  int
}

// ListRegisteredCategories returns every row from the registry in
// alphabetical category order. The dispatcher does NOT use this — it
// reads from the in-memory Config — but the dashboard + tests need the
// row-level projection.
func ListRegisteredCategories(db *sql.DB) ([]RegistryEntry, error) {
	rows, err := db.Query(
		`SELECT id, category, tier, yaml_default, IFNULL(description, ''),
		        IFNULL(registered_at, ''), yaml_version
		 FROM NotificationCategoryRegistry
		 ORDER BY category`,
	)
	if err != nil {
		return nil, fmt.Errorf("notify: list registry: %w", err)
	}
	defer rows.Close()
	var out []RegistryEntry
	for rows.Next() {
		var e RegistryEntry
		var tierInt int
		var def string
		if err := rows.Scan(&e.ID, &e.Category, &tierInt, &def, &e.Description, &e.RegisteredAt, &e.YAMLVersion); err != nil {
			return nil, fmt.Errorf("notify: scan registry row: %w", err)
		}
		e.Tier = Tier(tierInt)
		e.YAMLDefault = Setting(def)
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("notify: registry rows iter: %w", err)
	}
	return out, nil
}
