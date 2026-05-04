// Package store — D10 PRHandoffSyntheses + Repositories.handoff_synthesis_enabled
// helpers.
//
// One row per Diplomat-emitted reviewer narrative comment posted on a draft
// PR. The row is the audit trail: the LLM call landed, the gh post landed,
// and the (convoy, PR url) pair is recorded for the operator + the
// validating paired-run experiment harness.
package store

import (
	"database/sql"
	"fmt"
)

// PRHandoffSynthesis is one row of PRHandoffSyntheses.
type PRHandoffSynthesis struct {
	ID            int
	ConvoyID      int
	PRURL         string
	PostedAt      string // SQLite UTC ('YYYY-MM-DD HH:MM:SS')
	ExperimentArm string // e.g. 'control_off' | 'treatment_on'
	CommentID     int64  // GitHub REST comment ID (0 when unknown)
}

// SetHandoffSynthesisEnabled flips Repositories.handoff_synthesis_enabled
// for the named repo. Default at creation is 0 (OFF) per D10 anti-cheat
// directive #1 — repos opt IN, not out. Returns an error if no row was
// updated (i.e. unknown repo).
func SetHandoffSynthesisEnabled(db *sql.DB, repoName string, enabled bool) error {
	if repoName == "" {
		return fmt.Errorf("SetHandoffSynthesisEnabled: repoName required")
	}
	v := 0
	if enabled {
		v = 1
	}
	res, err := db.Exec(
		`UPDATE Repositories SET handoff_synthesis_enabled = ? WHERE name = ?`,
		v, repoName,
	)
	if err != nil {
		return fmt.Errorf("SetHandoffSynthesisEnabled: %w", err)
	}
	rows, _ := res.RowsAffected()
	if rows == 0 {
		return fmt.Errorf("SetHandoffSynthesisEnabled: repo %q not registered", repoName)
	}
	return nil
}

// HandoffSynthesisEnabled reads the flag for a single repo. Unknown repo
// returns false (the default-OFF semantic) — callers that need to
// distinguish unknown-repo from disabled should use GetRepo and check
// `r != nil`.
func HandoffSynthesisEnabled(db *sql.DB, repoName string) bool {
	if repoName == "" {
		return false
	}
	var v int
	err := db.QueryRow(
		`SELECT IFNULL(handoff_synthesis_enabled, 0) FROM Repositories WHERE name = ?`,
		repoName,
	).Scan(&v)
	if err != nil {
		return false
	}
	return v == 1
}

// InsertPRHandoffSynthesis records one Diplomat-emitted reviewer
// narrative. PostedAt is stamped with NowSQLite() if the caller passes
// "" (the common path).
func InsertPRHandoffSynthesis(db *sql.DB, row PRHandoffSynthesis) (int, error) {
	if row.ConvoyID <= 0 {
		return 0, fmt.Errorf("InsertPRHandoffSynthesis: convoy_id required")
	}
	if row.PRURL == "" {
		return 0, fmt.Errorf("InsertPRHandoffSynthesis: pr_url required")
	}
	postedAt := row.PostedAt
	if postedAt == "" {
		postedAt = NowSQLite()
	}
	res, err := db.Exec(
		`INSERT INTO PRHandoffSyntheses
			(convoy_id, pr_url, posted_at, experiment_arm, comment_id)
		 VALUES (?, ?, ?, ?, ?)`,
		row.ConvoyID, row.PRURL, postedAt, row.ExperimentArm, row.CommentID,
	)
	if err != nil {
		return 0, fmt.Errorf("InsertPRHandoffSynthesis: %w", err)
	}
	id, _ := res.LastInsertId()
	return int(id), nil
}

// ListPRHandoffSynthesesForConvoy returns every row recorded for the
// given convoy, newest-first. Empty slice if none.
func ListPRHandoffSynthesesForConvoy(db *sql.DB, convoyID int) ([]PRHandoffSynthesis, error) {
	rows, err := db.Query(
		`SELECT id, convoy_id, pr_url, posted_at,
		        IFNULL(experiment_arm, ''), IFNULL(comment_id, 0)
		   FROM PRHandoffSyntheses
		  WHERE convoy_id = ?
		  ORDER BY id DESC`,
		convoyID,
	)
	if err != nil {
		return nil, fmt.Errorf("ListPRHandoffSynthesesForConvoy: %w", err)
	}
	defer rows.Close()
	var out []PRHandoffSynthesis
	for rows.Next() {
		var r PRHandoffSynthesis
		if scanErr := rows.Scan(&r.ID, &r.ConvoyID, &r.PRURL, &r.PostedAt,
			&r.ExperimentArm, &r.CommentID); scanErr != nil {
			return nil, fmt.Errorf("ListPRHandoffSynthesesForConvoy: scan: %w", scanErr)
		}
		out = append(out, r)
	}
	if rerr := rows.Err(); rerr != nil {
		return nil, fmt.Errorf("ListPRHandoffSynthesesForConvoy: rows iter: %w", rerr)
	}
	return out, nil
}
