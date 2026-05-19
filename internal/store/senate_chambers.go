// Package store: SenateChambers — one row per Senator. D4 Phase 3.
//
// A Senator is a repo-scoped reviewer consulted by the Chancellor between
// the ProposedConvoys write and the AwaitingChancellorReview transition
// (docs/next-gen-agents.md § "Senate"). This file is the operator-facing
// helper layer for the SenateChambers table.
//
// Schema lives in schema.go (createSchema + runMigrations) and
// schema/schema.sql per CLAUDE.md § "Store / schema conventions". Every
// mutator returns error per CLAUDE.md § "No silent failures".
package store

import (
	"database/sql"
	"errors"
	"fmt"
)

// SenateChamber is the in-memory shape of one SenateChambers row.
type SenateChamber struct {
	SenatorName     string // 'force-orchestrator' | ...
	Scope           string // 'repo:<name>' | 'team:<name>'
	SenateMDPath    string
	Status          string // 'onboarding' | 'active' | 'suspended' | 'retired'
	OnboardedAt     string
	LastRefreshedAt string
	RetiredAt       string
	CreatedAt       string
}

// UpsertSenateChamber inserts a new chamber row or updates the existing
// row's scope / senate_md_path / status. Used by the SenatorOnboarding
// task to seed an 'onboarding' row, and by ratification to flip
// 'onboarding' → 'active' once the first FleetRules row lands.
func UpsertSenateChamber(db *sql.DB, c SenateChamber) error {
	if c.SenatorName == "" {
		return errors.New("UpsertSenateChamber: SenatorName required")
	}
	if c.Scope == "" {
		return errors.New("UpsertSenateChamber: Scope required")
	}
	if c.Status == "" {
		c.Status = "onboarding"
	}
	_, err := db.Exec(`
		INSERT INTO SenateChambers (senator_name, scope, senate_md_path, status, onboarded_at)
		VALUES (?, ?, ?, ?, COALESCE(NULLIF(?,''), datetime('now')))
		ON CONFLICT(senator_name) DO UPDATE SET
		  scope          = excluded.scope,
		  senate_md_path = excluded.senate_md_path,
		  status         = excluded.status`,
		c.SenatorName, c.Scope, c.SenateMDPath, c.Status, c.OnboardedAt)
	if err != nil {
		return fmt.Errorf("UpsertSenateChamber(%s): %w", c.SenatorName, err)
	}
	return nil
}

// GetSenateChamber loads one chamber by senator_name. Returns (nil, nil)
// when no row exists; the caller decides whether absence is an error.
func GetSenateChamber(db *sql.DB, senatorName string) (*SenateChamber, error) {
	var c SenateChamber
	err := db.QueryRow(`
		SELECT senator_name, scope, IFNULL(senate_md_path,''), status,
		       IFNULL(onboarded_at,''), IFNULL(last_refreshed_at,''),
		       IFNULL(retired_at,''), IFNULL(created_at,'')
		  FROM SenateChambers
		 WHERE senator_name = ?`, senatorName).
		Scan(&c.SenatorName, &c.Scope, &c.SenateMDPath, &c.Status,
			&c.OnboardedAt, &c.LastRefreshedAt, &c.RetiredAt, &c.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("GetSenateChamber(%s): %w", senatorName, err)
	}
	return &c, nil
}

// ListActiveSenateChambers returns every chamber with status='active'.
// Used by the SenateReview claim path to fan out reviews to the active
// roster, and by senate-refresh to enumerate the dog's per-Senator work.
func ListActiveSenateChambers(db *sql.DB) ([]SenateChamber, error) {
	rows, err := db.Query(`
		SELECT senator_name, scope, IFNULL(senate_md_path,''), status,
		       IFNULL(onboarded_at,''), IFNULL(last_refreshed_at,''),
		       IFNULL(retired_at,''), IFNULL(created_at,'')
		  FROM SenateChambers
		 WHERE status = 'active'
		 ORDER BY senator_name ASC`)
	if err != nil {
		return nil, fmt.Errorf("ListActiveSenateChambers: %w", err)
	}
	defer rows.Close()
	var out []SenateChamber
	for rows.Next() {
		var c SenateChamber
		if scanErr := rows.Scan(&c.SenatorName, &c.Scope, &c.SenateMDPath, &c.Status,
			&c.OnboardedAt, &c.LastRefreshedAt, &c.RetiredAt, &c.CreatedAt); scanErr != nil {
			return nil, fmt.Errorf("ListActiveSenateChambers: scan: %w", scanErr)
		}
		out = append(out, c)
	}
	if rErr := rows.Err(); rErr != nil {
		return nil, fmt.Errorf("ListActiveSenateChambers: rows.Err: %w", rErr)
	}
	return out, nil
}

// SetSenateChamberStatus flips one chamber's status; used by the
// onboarding-completion path (operator ratifies → 'active') and
// retirement (operator removes a Senator → 'retired').
func SetSenateChamberStatus(db *sql.DB, senatorName, status string) error {
	if senatorName == "" {
		return errors.New("SetSenateChamberStatus: senatorName required")
	}
	switch status {
	case "onboarding", "active", "suspended", "retired":
	default:
		return fmt.Errorf("SetSenateChamberStatus: invalid status %q", status)
	}
	res, err := db.Exec(`
		UPDATE SenateChambers
		   SET status      = ?,
		       retired_at  = CASE WHEN ? = 'retired' THEN datetime('now') ELSE retired_at END
		 WHERE senator_name = ?`, status, status, senatorName)
	if err != nil {
		return fmt.Errorf("SetSenateChamberStatus(%s): %w", senatorName, err)
	}
	n, _ := res.RowsAffected()
	if n != 1 {
		return fmt.Errorf("SetSenateChamberStatus(%s): no row updated", senatorName)
	}
	return nil
}

// ListAllSenateChambers returns all chambers regardless of status, ordered by
// senator_name. Used by `force senate` to display the full roster.
func ListAllSenateChambers(db *sql.DB) ([]SenateChamber, error) {
	rows, err := db.Query(`
		SELECT senator_name, scope, IFNULL(senate_md_path,''), status,
		       IFNULL(onboarded_at,''), IFNULL(last_refreshed_at,''),
		       IFNULL(retired_at,''), IFNULL(created_at,'')
		  FROM SenateChambers
		 ORDER BY senator_name ASC`)
	if err != nil {
		return nil, fmt.Errorf("ListAllSenateChambers: %w", err)
	}
	defer rows.Close()
	var out []SenateChamber
	for rows.Next() {
		var c SenateChamber
		if scanErr := rows.Scan(&c.SenatorName, &c.Scope, &c.SenateMDPath, &c.Status,
			&c.OnboardedAt, &c.LastRefreshedAt, &c.RetiredAt, &c.CreatedAt); scanErr != nil {
			return nil, fmt.Errorf("ListAllSenateChambers: scan: %w", scanErr)
		}
		out = append(out, c)
	}
	if rErr := rows.Err(); rErr != nil {
		return nil, fmt.Errorf("ListAllSenateChambers: rows.Err: %w", rErr)
	}
	return out, nil
}

// MarkSenateChamberRefreshed bumps last_refreshed_at to now. Called by
// the senate-refresh dog after each successful per-Senator memory
// digest pass.
func MarkSenateChamberRefreshed(db *sql.DB, senatorName string) error {
	res, err := db.Exec(`
		UPDATE SenateChambers
		   SET last_refreshed_at = datetime('now')
		 WHERE senator_name = ?`, senatorName)
	if err != nil {
		return fmt.Errorf("MarkSenateChamberRefreshed(%s): %w", senatorName, err)
	}
	n, _ := res.RowsAffected()
	if n != 1 {
		return fmt.Errorf("MarkSenateChamberRefreshed(%s): no row updated", senatorName)
	}
	return nil
}
