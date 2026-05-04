// Package store: ArchaeologistFindings — persistence for the
// Archaeologist agent's debt-pattern hits (D9).
//
// Schema lives in schema.go (createSchema + runMigrations) and
// schema/schema.sql per CLAUDE.md § "Store / schema conventions". This
// file is the helper layer used by internal/agents/archaeologist.go.
// Every mutator returns error per CLAUDE.md § "No silent failures".
package store

import (
	"database/sql"
	"errors"
	"fmt"
	"log"
	"strings"
)

// ArchaeologistFinding is the in-memory shape of one row.
type ArchaeologistFinding struct {
	ID         int
	PatternID  string
	RepoID     int
	FilePath   string
	LineNumber int
	DetailJSON string
	DetectedAt string
	Status     string
}

// InsertArchaeologistFinding records one Archaeologist hit. The
// underlying UNIQUE(pattern_id, repo_id, file_path, line_number)
// constraint makes re-inserts idempotent — INSERT OR IGNORE turns the
// repeat into a no-op while still flowing the row's id back via a
// follow-up SELECT. Returns (0, nil) on a deduped row (so the caller
// can distinguish "wrote a new finding" from "already had it").
func InsertArchaeologistFinding(db *sql.DB, f ArchaeologistFinding) (int, error) {
	if strings.TrimSpace(f.PatternID) == "" {
		return 0, errors.New("InsertArchaeologistFinding: PatternID required")
	}
	if f.RepoID <= 0 {
		return 0, errors.New("InsertArchaeologistFinding: RepoID required")
	}
	if strings.TrimSpace(f.FilePath) == "" {
		return 0, errors.New("InsertArchaeologistFinding: FilePath required")
	}
	if strings.TrimSpace(f.DetailJSON) == "" {
		f.DetailJSON = "{}"
	}
	if strings.TrimSpace(f.Status) == "" {
		f.Status = "open"
	}
	if strings.TrimSpace(f.DetectedAt) == "" {
		f.DetectedAt = NowSQLite()
	}
	res, err := db.Exec(`
		INSERT OR IGNORE INTO ArchaeologistFindings
			(pattern_id, repo_id, file_path, line_number, detail_json, detected_at, status)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		f.PatternID, f.RepoID, f.FilePath, f.LineNumber, f.DetailJSON, f.DetectedAt, f.Status)
	if err != nil {
		return 0, fmt.Errorf("InsertArchaeologistFinding(%s): %w", f.PatternID, err)
	}
	rows, _ := res.RowsAffected()
	if rows == 0 {
		// Row already existed (UNIQUE conflict). Not an error.
		return 0, nil
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("InsertArchaeologistFinding(%s): LastInsertId: %w", f.PatternID, err)
	}
	return int(id), nil
}

// CountOpenFindingsForPattern returns the number of open-status
// ArchaeologistFindings rows scoped to (pattern, repo). The
// migration-proposal handler compares this against
// Pattern.MinHitsForFeature() to decide whether to fire the
// ArchaeologistProposeMigration task type.
func CountOpenFindingsForPattern(db *sql.DB, patternID string, repoID int) (int, error) {
	var n int
	err := db.QueryRow(`
		SELECT COUNT(*) FROM ArchaeologistFindings
		WHERE pattern_id = ? AND repo_id = ? AND status = 'open'`,
		patternID, repoID).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("CountOpenFindingsForPattern(%s,%d): %w", patternID, repoID, err)
	}
	return n, nil
}

// ListOpenArchaeologistFindings returns every open-status row for the
// given (pattern, repo). Newest-first ordering keeps the migration-
// proposal preview deterministic.
func ListOpenArchaeologistFindings(db *sql.DB, patternID string, repoID int) ([]ArchaeologistFinding, error) {
	rows, err := db.Query(`
		SELECT id, pattern_id, repo_id, file_path, line_number,
		       IFNULL(detail_json,'{}'), IFNULL(detected_at,''), IFNULL(status,'open')
		FROM ArchaeologistFindings
		WHERE pattern_id = ? AND repo_id = ? AND status = 'open'
		ORDER BY id DESC`,
		patternID, repoID)
	if err != nil {
		return nil, fmt.Errorf("ListOpenArchaeologistFindings(%s,%d): %w", patternID, repoID, err)
	}
	defer rows.Close()
	var out []ArchaeologistFinding
	for rows.Next() {
		var f ArchaeologistFinding
		if scanErr := rows.Scan(&f.ID, &f.PatternID, &f.RepoID, &f.FilePath, &f.LineNumber,
			&f.DetailJSON, &f.DetectedAt, &f.Status); scanErr != nil {
			return nil, fmt.Errorf("ListOpenArchaeologistFindings(%s,%d): scan: %w", patternID, repoID, scanErr)
		}
		out = append(out, f)
	}
	if rErr := rows.Err(); rErr != nil {
		return nil, fmt.Errorf("ListOpenArchaeologistFindings(%s,%d): rows.Err: %w", patternID, repoID, rErr)
	}
	return out, nil
}

// SetArchaeologistFindingsStatus bulk-flips the status column for
// every open finding scoped to (pattern, repo). Used by the
// migration-proposal handler to mark the cluster as 'proposed' once
// the Librarian.EmitCandidate call returns successfully — preventing
// double-fires of the same migration in subsequent sweeps.
func SetArchaeologistFindingsStatus(db *sql.DB, patternID string, repoID int, fromStatus, toStatus string) (int, error) {
	if strings.TrimSpace(toStatus) == "" {
		return 0, errors.New("SetArchaeologistFindingsStatus: toStatus required")
	}
	res, err := db.Exec(`
		UPDATE ArchaeologistFindings
		SET status = ?
		WHERE pattern_id = ? AND repo_id = ? AND status = ?`,
		toStatus, patternID, repoID, fromStatus)
	if err != nil {
		return 0, fmt.Errorf("SetArchaeologistFindingsStatus(%s,%d): %w", patternID, repoID, err)
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// ─── Repository helpers ──────────────────────────────────────────────────────

// ArchaeologistRepoTarget is the per-repo shape consumed by the
// Archaeologist's sweep-dog: the rowid (used as
// ArchaeologistFindings.repo_id), the canonical name (for log lines),
// and the local filesystem path (the scan target).
type ArchaeologistRepoTarget struct {
	ID        int
	Name      string
	LocalPath string
}

// ListArchaeologistSweepTargets returns the list of registered repos
// whose archaeologist_sweep_disabled column is 0 (the default). Used
// by the sweep dog to enqueue ArchaeologistSweep tasks per repo.
//
// repos with empty local_path are filtered out — the Archaeologist
// scans the working tree on disk, so a repo registered without a
// clone is unscannable.
func ListArchaeologistSweepTargets(db *sql.DB) ([]ArchaeologistRepoTarget, error) {
	rows, err := db.Query(`
		SELECT rowid, name, IFNULL(local_path, '')
		FROM Repositories
		WHERE IFNULL(archaeologist_sweep_disabled, 0) = 0
		  AND IFNULL(local_path, '') != ''
		ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("ListArchaeologistSweepTargets: %w", err)
	}
	defer rows.Close()
	var out []ArchaeologistRepoTarget
	for rows.Next() {
		var t ArchaeologistRepoTarget
		if scanErr := rows.Scan(&t.ID, &t.Name, &t.LocalPath); scanErr != nil {
			return nil, fmt.Errorf("ListArchaeologistSweepTargets: scan: %w", scanErr)
		}
		out = append(out, t)
	}
	if rErr := rows.Err(); rErr != nil {
		return nil, fmt.Errorf("ListArchaeologistSweepTargets: rows.Err: %w", rErr)
	}
	return out, nil
}

// GetArchaeologistRepoByID looks up the (name, local_path) for a given
// SQLite rowid. The Archaeologist's sweep handler consumes this when
// it dequeues an ArchaeologistSweep task by repo_id.
func GetArchaeologistRepoByID(db *sql.DB, repoID int) (ArchaeologistRepoTarget, error) {
	var t ArchaeologistRepoTarget
	err := db.QueryRow(`
		SELECT rowid, name, IFNULL(local_path, '')
		FROM Repositories
		WHERE rowid = ?`, repoID).
		Scan(&t.ID, &t.Name, &t.LocalPath)
	if err != nil {
		return ArchaeologistRepoTarget{}, fmt.Errorf("GetArchaeologistRepoByID(%d): %w", repoID, err)
	}
	return t, nil
}

// SetArchaeologistSweepDisabled flips the per-repo opt-out flag.
// Operator-driven only (no agent ever sets this). Returns ErrNotFound
// when the repo is unknown.
func SetArchaeologistSweepDisabled(db *sql.DB, repoName string, disabled bool) error {
	v := 0
	if disabled {
		v = 1
	}
	res, err := db.Exec(`UPDATE Repositories SET archaeologist_sweep_disabled = ? WHERE name = ?`, v, repoName)
	if err != nil {
		return fmt.Errorf("SetArchaeologistSweepDisabled(%s): %w", repoName, err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("SetArchaeologistSweepDisabled(%s): repo not found", repoName)
	}
	return nil
}

// _ silences the unused-import lint when log isn't referenced; the
// pattern in this package logs from helpers in sibling files.
var _ = log.Printf

// ─── Task queue helpers ──────────────────────────────────────────────────────

// QueueArchaeologistSweep enqueues a per-repo sweep task. Used both by
// the operator CLI (force archaeologist sweep <repo>) and by the
// archaeologist-sweep dog (one task per active repo per week). The
// payload is the repo's SQLite rowid; the handler re-resolves
// (name, local_path) from Repositories at claim time.
func QueueArchaeologistSweep(db *sql.DB, repoID int, repoName string) (int, error) {
	if repoID <= 0 {
		return 0, fmt.Errorf("QueueArchaeologistSweep: repoID required")
	}
	payload := fmt.Sprintf(`{"repo_id":%d}`, repoID)
	res, err := db.Exec(
		`INSERT INTO BountyBoard (parent_id, target_repo, type, status, payload, priority, created_at)
		 VALUES (0, ?, 'ArchaeologistSweep', 'Pending', ?, 2, datetime('now'))`,
		repoName, payload)
	if err != nil {
		return 0, fmt.Errorf("QueueArchaeologistSweep(%s): %w", repoName, err)
	}
	id, _ := res.LastInsertId()
	return int(id), nil
}

// QueueArchaeologistProposeMigration enqueues the proposal handoff
// for one (pattern, repo) cluster. Called from the sweep handler when
// a pattern's open-hit count crosses Pattern.MinHitsForFeature().
// Anti-cheat #1: this task type is the ONLY way an Archaeologist
// hands off a migration; the handler itself routes through
// librarian.Client.EmitCandidate (operator-ratifiable).
func QueueArchaeologistProposeMigration(db *sql.DB, patternID string, repoID int, repoName string) (int, error) {
	if strings.TrimSpace(patternID) == "" {
		return 0, fmt.Errorf("QueueArchaeologistProposeMigration: patternID required")
	}
	if repoID <= 0 {
		return 0, fmt.Errorf("QueueArchaeologistProposeMigration: repoID required")
	}
	// Hand-rolled JSON to keep the dependency on encoding/json out of
	// this file (the rest of the helpers don't need it). Patterns use
	// stable IDs (ARCH-NNN) so embedding them in JSON is safe.
	payload := fmt.Sprintf(`{"pattern_id":%q,"repo_id":%d}`, patternID, repoID)
	res, err := db.Exec(
		`INSERT INTO BountyBoard (parent_id, target_repo, type, status, payload, priority, created_at)
		 VALUES (0, ?, 'ArchaeologistProposeMigration', 'Pending', ?, 3, datetime('now'))`,
		repoName, payload)
	if err != nil {
		return 0, fmt.Errorf("QueueArchaeologistProposeMigration(%s,%d): %w", patternID, repoID, err)
	}
	id, _ := res.LastInsertId()
	return int(id), nil
}
