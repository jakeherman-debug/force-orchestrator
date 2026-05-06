package store

import (
	"database/sql"
	"fmt"
	"log"
)

// DaemonUpdateHistoryEntry is one row in the DaemonUpdateHistory table.
// Each row records a single `force daemon update` invocation and its
// outcome. Outcome values: "success" | "rolled_back" | "failed".
type DaemonUpdateHistoryEntry struct {
	ID           int64
	TS           string
	OldBinarySHA string
	NewBinarySHA string
	OldGitSHA    string
	NewGitSHA    string
	Operator     string
	Outcome      string
	Notes        string
}

// RecordDaemonUpdate writes one row to DaemonUpdateHistory. Called from
// cmd/force/daemon_cmds.go's update path on every outcome (success,
// rolled-back, failed) — the AST audit (Pattern P_DaemonUpdateHistory)
// confirms every exit-from-update path lands a row here.
//
// Returns an error per the "no silent failures" invariant — callers MUST
// either check the error or explicitly log+continue (the update path
// already logs on failure-to-record because failing to record an outcome
// is observability, not correctness, but we still surface the error so a
// pattern test can see the call site).
func RecordDaemonUpdate(db *sql.DB, oldSHA, newSHA, oldGit, newGit, operator, outcome, notes string) error {
	if outcome == "" {
		return fmt.Errorf("RecordDaemonUpdate: outcome must be one of success|rolled_back|failed (got empty)")
	}
	_, err := db.Exec(`INSERT INTO DaemonUpdateHistory
		(ts, old_binary_sha, new_binary_sha, old_git_sha, new_git_sha, operator, outcome, notes)
		VALUES (datetime('now'), ?, ?, ?, ?, ?, ?, ?)`,
		oldSHA, newSHA, oldGit, newGit, operator, outcome, notes)
	if err != nil {
		return fmt.Errorf("RecordDaemonUpdate: insert: %w", err)
	}
	return nil
}

// ListDaemonUpdateHistory returns the most recent N entries (newest
// first). limit <= 0 maps to 50. Used by `force daemon history`.
func ListDaemonUpdateHistory(db *sql.DB, limit int) ([]DaemonUpdateHistoryEntry, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := db.Query(`SELECT
			id, ts, old_binary_sha, new_binary_sha,
			IFNULL(old_git_sha, ''), IFNULL(new_git_sha, ''),
			IFNULL(operator, ''), outcome, IFNULL(notes, '')
		FROM DaemonUpdateHistory ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("ListDaemonUpdateHistory: query: %w", err)
	}
	defer rows.Close()
	var out []DaemonUpdateHistoryEntry
	for rows.Next() {
		var e DaemonUpdateHistoryEntry
		if scanErr := rows.Scan(
			&e.ID, &e.TS, &e.OldBinarySHA, &e.NewBinarySHA,
			&e.OldGitSHA, &e.NewGitSHA,
			&e.Operator, &e.Outcome, &e.Notes,
		); scanErr != nil {
			log.Printf("ListDaemonUpdateHistory: scan: %v", scanErr)
			continue
		}
		out = append(out, e)
	}
	if rerr := rows.Err(); rerr != nil {
		return out, fmt.Errorf("ListDaemonUpdateHistory: rows: %w", rerr)
	}
	return out, nil
}
