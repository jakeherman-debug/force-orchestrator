// Package store — D9 Phase 1: ArchHealthAggregates mutators + readers.
//
// ArchHealthAggregates persists the (report_month, rule_id, repo_id,
// author_type) totals produced each month by dogArchitectureHealthReport.
// The UNIQUE clause on (report_month, rule_id, repo_id, author_type)
// makes UpsertArchHealthAggregate idempotent — re-running the same month
// is a no-op for already-recorded rows; the helper uses INSERT...ON
// CONFLICT...DO UPDATE to refresh violation_count when the dog re-scans
// (D9 spec: "2 runs same month → no duplicates").
//
// CLAUDE.md "No silent failures": every mutator returns error.
package store

import (
	"database/sql"
	"fmt"
)

// ArchHealthAggregate is the row shape for ArchHealthAggregates.
type ArchHealthAggregate struct {
	ID             int64
	ReportMonth    string // 'YYYY-MM'
	RuleID         string // e.g. 'BOS-001'
	RepoID         int    // synthetic id (rowid order from ListRepos at scan time)
	AuthorType     string // 'human' | 'astromech' | 'archaeologist-migration'
	ViolationCount int
	CreatedAt      string
}

// UpsertArchHealthAggregate inserts a new ArchHealthAggregates row, or
// updates the violation_count + created_at when (report_month, rule_id,
// repo_id, author_type) already exists. Idempotent on a re-run within
// the same month per the D9 spec.
func UpsertArchHealthAggregate(db *sql.DB, a ArchHealthAggregate) error {
	if a.ReportMonth == "" {
		return fmt.Errorf("UpsertArchHealthAggregate: empty report_month")
	}
	if a.RuleID == "" {
		return fmt.Errorf("UpsertArchHealthAggregate: empty rule_id")
	}
	if a.AuthorType == "" {
		return fmt.Errorf("UpsertArchHealthAggregate: empty author_type")
	}
	_, err := db.Exec(`
		INSERT INTO ArchHealthAggregates
			(report_month, rule_id, repo_id, author_type, violation_count, created_at)
		VALUES (?, ?, ?, ?, ?, datetime('now'))
		ON CONFLICT(report_month, rule_id, repo_id, author_type)
		DO UPDATE SET
			violation_count = excluded.violation_count,
			created_at      = excluded.created_at`,
		a.ReportMonth, a.RuleID, a.RepoID, a.AuthorType, a.ViolationCount)
	if err != nil {
		return fmt.Errorf("UpsertArchHealthAggregate: %w", err)
	}
	return nil
}

// ListArchHealthAggregatesForMonth returns all ArchHealthAggregates rows
// for the given report_month, ordered by rule_id, repo_id, author_type
// for deterministic rendering.
func ListArchHealthAggregatesForMonth(db *sql.DB, month string) ([]ArchHealthAggregate, error) {
	rows, err := db.Query(`
		SELECT id, report_month, rule_id, repo_id, author_type,
		       violation_count, IFNULL(created_at, '')
		FROM ArchHealthAggregates
		WHERE report_month = ?
		ORDER BY rule_id, repo_id, author_type`, month)
	if err != nil {
		return nil, fmt.Errorf("ListArchHealthAggregatesForMonth(%s): %w", month, err)
	}
	defer rows.Close()
	var out []ArchHealthAggregate
	for rows.Next() {
		var a ArchHealthAggregate
		if sErr := rows.Scan(&a.ID, &a.ReportMonth, &a.RuleID, &a.RepoID,
			&a.AuthorType, &a.ViolationCount, &a.CreatedAt); sErr != nil {
			return nil, fmt.Errorf("ListArchHealthAggregatesForMonth scan: %w", sErr)
		}
		out = append(out, a)
	}
	if rErr := rows.Err(); rErr != nil {
		return nil, fmt.Errorf("ListArchHealthAggregatesForMonth iter: %w", rErr)
	}
	return out, nil
}

// ListArchHealthMonths returns the distinct report_month values present
// in ArchHealthAggregates, ordered ascending. Used by the renderer's
// 6-month-trend graph and by the dashboard's month-picker.
func ListArchHealthMonths(db *sql.DB) ([]string, error) {
	rows, err := db.Query(`SELECT DISTINCT report_month FROM ArchHealthAggregates ORDER BY report_month ASC`)
	if err != nil {
		return nil, fmt.Errorf("ListArchHealthMonths: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var m string
		if sErr := rows.Scan(&m); sErr != nil {
			return nil, fmt.Errorf("ListArchHealthMonths scan: %w", sErr)
		}
		out = append(out, m)
	}
	if rErr := rows.Err(); rErr != nil {
		return nil, fmt.Errorf("ListArchHealthMonths iter: %w", rErr)
	}
	return out, nil
}
