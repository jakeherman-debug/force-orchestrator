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
	"fmt"
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
