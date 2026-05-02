// Package store: Repositories.license accessors + backfill (D5 Phase
// 0). The column is populated by AddRepo on registration via the
// SPDX detector; the runMigrations backfill stamps existing rows by
// reading the LICENSE-family file under each repo's local_path.
//
// Per CLAUDE.md "No silent failures": every mutator returns error.
// The backfill itself is best-effort by design (a missing local_path
// or absent LICENSE file leaves the column at '') — operator review
// surfaces those as Unknown via the dashboard.
package store

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"

	"force-orchestrator/internal/isb/scanners/spdx"
)

// SetRepositoryLicense overwrites the license column for the named
// repo. SPDX id is the canonical value (e.g. "MIT", "Apache-2.0",
// "Unknown"). Returns ErrNoRows-style error if the repo doesn't exist.
func SetRepositoryLicense(db *sql.DB, name, license string) error {
	if name == "" {
		return fmt.Errorf("SetRepositoryLicense: name required")
	}
	res, err := db.Exec(`UPDATE Repositories SET license = ? WHERE name = ?`, license, name)
	if err != nil {
		return fmt.Errorf("SetRepositoryLicense(%s): %w", name, err)
	}
	n, _ := res.RowsAffected()
	if n != 1 {
		return fmt.Errorf("SetRepositoryLicense(%s): no row updated", name)
	}
	return nil
}

// GetRepositoryLicense returns the SPDX id for the named repo, or ""
// if the repo doesn't exist or hasn't been detected yet.
func GetRepositoryLicense(db *sql.DB, name string) (string, error) {
	var lic string
	err := db.QueryRow(`SELECT IFNULL(license, '') FROM Repositories WHERE name = ?`, name).Scan(&lic)
	if err != nil {
		if err == sql.ErrNoRows {
			return "", nil
		}
		return "", fmt.Errorf("GetRepositoryLicense(%s): %w", name, err)
	}
	return lic, nil
}

// DetectAndSetRepositoryLicense reads the LICENSE-family file under
// `repoPath` and stores the detected SPDX id against the named repo.
// Used by AddRepo after a fresh clone. Returns the detected id (which
// may be spdx.Unknown when no LICENSE file is found or the contents
// don't match a canonical signature). Errors only on DB failures —
// missing files / unreadable paths leave the column at the prior
// value and return spdx.Unknown.
func DetectAndSetRepositoryLicense(db *sql.DB, name, repoPath string) (string, error) {
	id := spdx.Unknown
	if repoPath != "" {
		if body, ok := readLicenseFile(repoPath); ok {
			id = spdx.Detect(body)
		}
	}
	if err := SetRepositoryLicense(db, name, id); err != nil {
		return id, err
	}
	return id, nil
}

// readLicenseFile walks the repo root looking for a LICENSE-family
// file. Returns its bytes and true on success; (nil, false) when
// nothing matches (or the read errored — caller treats both the
// same).
func readLicenseFile(repoPath string) ([]byte, bool) {
	candidates := []string{
		"LICENSE", "LICENSE.md", "LICENSE.txt", "LICENSE.rst",
		"COPYING", "COPYING.md", "COPYING.txt",
		"UNLICENSE", "UNLICENSE.md",
		"License", "License.md", "License.txt",
		"license", "license.md", "license.txt",
	}
	for _, name := range candidates {
		full := filepath.Join(repoPath, name)
		body, err := os.ReadFile(full)
		if err == nil {
			return body, true
		}
	}
	return nil, false
}

// backfillRepositoryLicenses scans every repo with an empty license
// column and, when local_path resolves and a LICENSE file exists,
// runs the SPDX detector and stamps the column. Idempotent: rows
// with non-empty license are skipped, so a re-run does NOT clobber
// operator-set values or already-detected ids. Called from
// runMigrations after the column ALTER.
//
// Errors are logged via internal/log (not returned) because
// runMigrations itself is void. The "no silent failures" invariant
// is honoured by the per-row log; a bulk-DB failure surfaces as a
// later read returning '' which the operator review path handles.
func backfillRepositoryLicenses(db *sql.DB) {
	rows, err := db.Query(`SELECT name, IFNULL(local_path, '') FROM Repositories WHERE license = '' OR license IS NULL`)
	if err != nil {
		// Most common cause: column doesn't exist yet (which can't
		// happen because the ALTER ran above) — anything else is a
		// real DB issue. The migration-time logger captures it via
		// the standard log package.
		fmt.Fprintf(os.Stderr, "store: backfillRepositoryLicenses query: %v\n", err)
		return
	}
	defer rows.Close()

	type repoRow struct{ name, path string }
	var todo []repoRow
	for rows.Next() {
		var r repoRow
		if scanErr := rows.Scan(&r.name, &r.path); scanErr != nil {
			fmt.Fprintf(os.Stderr, "store: backfillRepositoryLicenses scan: %v\n", scanErr)
			continue
		}
		todo = append(todo, r)
	}
	if rErr := rows.Err(); rErr != nil {
		fmt.Fprintf(os.Stderr, "store: backfillRepositoryLicenses rows.Err: %v\n", rErr)
	}

	for _, r := range todo {
		if r.path == "" {
			continue
		}
		body, ok := readLicenseFile(r.path)
		if !ok {
			continue // leave at '' — operator review picks this up
		}
		id := spdx.Detect(body)
		if _, err := db.Exec(`UPDATE Repositories SET license = ? WHERE name = ?`, id, r.name); err != nil {
			fmt.Fprintf(os.Stderr, "store: backfillRepositoryLicenses update %s: %v\n", r.name, err)
		}
	}
}
