// Package store — D10 PRHandoffSyntheses + Repositories.handoff_synthesis_enabled
// helpers, plus D17 P2B T+30 verdict helpers for ExperimentRuns.
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

// ── D17 P2B — T+30 verdict helpers ────────────────────────────────────────────

// T30VerdictRun is a minimal view of an ExperimentRun row used by the T+30
// verdict dog. Only the fields needed for verdict-mail composition are included.
type T30VerdictRun struct {
	ID           int
	ExperimentID int
	CompletedAt  string // SQLite UTC shape
	AgentName    string
	ScoreSource  string
	Score        float64
}

// ListT30VerdictPending returns ExperimentRuns rows whose completed_at is
// between 30 and 31 days ago (the 24-hour window the daily dog checks) AND
// whose t30_verdict_sent_at is empty (verdict not yet sent). Mode is
// constrained to 'holdout' or 'paired_real' because shadow runs have no
// meaningful keep-or-deprecate verdict. Only rows with a non-empty
// completed_at are returned (runs still in flight are skipped).
func ListT30VerdictPending(db *sql.DB) ([]T30VerdictRun, error) {
	rows, err := db.Query(`
		SELECT id, experiment_id, completed_at,
		       IFNULL(agent_name, ''), IFNULL(score_source, ''), IFNULL(score, 0.0)
		  FROM ExperimentRuns
		 WHERE IFNULL(completed_at, '') != ''
		   AND t30_verdict_sent_at = ''
		   AND mode IN ('holdout', 'paired_real')
		   AND completed_at <= datetime('now', '-30 days')
		   AND completed_at >  datetime('now', '-31 days')
		 ORDER BY id ASC`)
	if err != nil {
		return nil, fmt.Errorf("ListT30VerdictPending: %w", err)
	}
	defer rows.Close()
	var out []T30VerdictRun
	for rows.Next() {
		var r T30VerdictRun
		if scanErr := rows.Scan(&r.ID, &r.ExperimentID, &r.CompletedAt,
			&r.AgentName, &r.ScoreSource, &r.Score); scanErr != nil {
			return nil, fmt.Errorf("ListT30VerdictPending: scan: %w", scanErr)
		}
		out = append(out, r)
	}
	if rerr := rows.Err(); rerr != nil {
		return nil, fmt.Errorf("ListT30VerdictPending: rows iter: %w", rerr)
	}
	return out, nil
}

// MarkT30VerdictSent stamps t30_verdict_sent_at = datetime('now') for the
// given ExperimentRun row, preventing duplicate verdict mails on subsequent
// dog ticks. Returns an error if the row was not found or the update fails.
func MarkT30VerdictSent(db *sql.DB, runID int) error {
	res, err := db.Exec(
		`UPDATE ExperimentRuns SET t30_verdict_sent_at = datetime('now') WHERE id = ?`,
		runID,
	)
	if err != nil {
		return fmt.Errorf("MarkT30VerdictSent(%d): %w", runID, err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("MarkT30VerdictSent(%d): row not found", runID)
	}
	return nil
}

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
