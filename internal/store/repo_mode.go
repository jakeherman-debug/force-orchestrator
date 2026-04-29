package store

import (
	"database/sql"
	"errors"
	"fmt"
)

// RepoMode is the tri-state writability flag on Repositories.mode (D2 T1-4).
//
//   - ModeReadOnly  — the default for newly-added repos. Astromechs cannot
//     claim tasks against the repo and every destructive git op refuses with
//     ErrRepoNotWritable. Read-only operations (Librarian indexing, Senator
//     consultation, Captain/Council review) work as normal.
//   - ModeWrite     — the repo is fully active. Tasks can be claimed and
//     destructive git ops are permitted (subject to AssertNotDefaultBranch
//     and the rest of the protected-branch guard layers).
//   - ModeQuarantined — behaves like ModeReadOnly plus the dashboard surfaces
//     a persistent banner and a quarantined-repo dog emits operator mail
//     when claims are blocked. Reserved for repos with broken remotes,
//     auth issues, or other infra problems that need operator attention
//     before the repo can be re-enabled.
type RepoMode string

const (
	ModeReadOnly    RepoMode = "read_only"
	ModeWrite       RepoMode = "write"
	ModeQuarantined RepoMode = "quarantined"
)

// validRepoModes mirrors the CHECK constraint on Repositories.mode in
// createSchema. Kept in sync by hand — the store package can't import
// the schema literal at runtime so this is the second source of truth
// (the migration's CHECK is the first).
var validRepoModes = map[RepoMode]struct{}{
	ModeReadOnly:    {},
	ModeWrite:       {},
	ModeQuarantined: {},
}

// IsValid reports whether m is one of the three legal mode values.
func (m RepoMode) IsValid() bool {
	_, ok := validRepoModes[m]
	return ok
}

// ErrRepoNotWritable is the typed sentinel returned by AssertRepoWritable
// (in internal/git/repo_mode_guard.go) when a destructive op is attempted
// on a repo whose mode is not 'write'. Classification is permanent — the
// caller routes to handleInfraFailure / FailBounty rather than retrying.
//
// Defined here in the store package so callers that don't already import
// internal/git can errors.Is against it without a cyclical dependency.
var ErrRepoNotWritable = errors.New("repository is not in write mode")

// ErrRepoNotFound is returned by GetRepoMode when the repository has no row
// in Repositories. The caller usually treats this as a permanent failure —
// the task targets a repo that was deleted out from under it.
var ErrRepoNotFound = errors.New("repository not found")

// GetRepoMode reads the mode column for the named repo. Returns
// ErrRepoNotFound if no row exists.
//
// Per the no-silent-failures invariant, a real DB error (table missing,
// driver failure) is wrapped and surfaced rather than masked as
// "not found." Tests rely on the distinction.
func GetRepoMode(db *sql.DB, repoName string) (RepoMode, error) {
	if repoName == "" {
		return "", fmt.Errorf("GetRepoMode: empty repoName")
	}
	var raw string
	err := db.QueryRow(`SELECT IFNULL(mode, '') FROM Repositories WHERE name = ?`, repoName).Scan(&raw)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", ErrRepoNotFound
		}
		return "", fmt.Errorf("GetRepoMode(%q): %w", repoName, err)
	}
	// An empty string here means a row exists but the mode column was never
	// populated (impossible under the NOT NULL DEFAULT 'read_only' schema,
	// but defend against schema drift). Treat as read_only.
	if raw == "" {
		return ModeReadOnly, nil
	}
	m := RepoMode(raw)
	if !m.IsValid() {
		return "", fmt.Errorf("GetRepoMode(%q): invalid mode value %q in DB (schema drift)", repoName, raw)
	}
	return m, nil
}

// SetRepoMode atomically updates the mode column AND writes an AuditLog
// entry recording who changed it and when. operatorEmail is recorded
// verbatim in AuditLog.actor — pass the empty string to record as the
// generic "operator" actor.
//
// Returns:
//   - ErrRepoNotFound if no row matched.
//   - A wrapped error if mode is not one of the three valid values.
//   - Any DB error from the UPDATE or AuditLog INSERT.
//
// The two writes happen inside a single transaction so a crash between
// the UPDATE and the INSERT never leaves an unaudited mode change.
func SetRepoMode(db *sql.DB, repoName string, mode RepoMode, operatorEmail string) error {
	if repoName == "" {
		return fmt.Errorf("SetRepoMode: empty repoName")
	}
	if !mode.IsValid() {
		return fmt.Errorf("SetRepoMode: invalid mode %q (must be read_only|write|quarantined)", mode)
	}
	actor := operatorEmail
	if actor == "" {
		actor = "operator"
	}

	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("SetRepoMode: begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	// Capture the prior mode for the audit detail. A SELECT inside the tx
	// guarantees we record exactly the value we displaced.
	var prior string
	if err := tx.QueryRow(`SELECT IFNULL(mode, '') FROM Repositories WHERE name = ?`, repoName).Scan(&prior); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrRepoNotFound
		}
		return fmt.Errorf("SetRepoMode(%q): read prior: %w", repoName, err)
	}

	res, err := tx.Exec(`UPDATE Repositories SET mode = ? WHERE name = ?`, string(mode), repoName)
	if err != nil {
		return fmt.Errorf("SetRepoMode(%q): UPDATE: %w", repoName, err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrRepoNotFound
	}

	detail := fmt.Sprintf("repo=%s prior_mode=%s new_mode=%s", repoName, prior, mode)
	if _, err := tx.Exec(
		`INSERT INTO AuditLog (actor, action, task_id, detail) VALUES (?, ?, ?, ?)`,
		actor, "repo.set_mode", 0, detail,
	); err != nil {
		return fmt.Errorf("SetRepoMode(%q): audit insert: %w", repoName, err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("SetRepoMode(%q): commit: %w", repoName, err)
	}
	return nil
}
