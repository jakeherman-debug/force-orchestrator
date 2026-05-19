package store

import (
	"database/sql"
)

// CrossRepoAPI mirrors a CrossRepoAPIs table row.
type CrossRepoAPI struct {
	ID            int
	RepoName      string
	APIKind       string
	APIIdentifier string
	SourceFile    string
	SourceLine    int
	Extractor     string
	SignatureHash string
	LastScannedAt string
}

// CrossRepoAPIDependency mirrors a CrossRepoAPIDependencies table row.
type CrossRepoAPIDependency struct {
	ID            int
	ConsumerRepo  string
	ConsumerFile  string
	ConsumerLine  int
	ProviderAPIID int
	CallKind      string
	MatchConf     float64
	DiscoveredAt  string
	DeletedAt     string
}

// UpsertCrossRepoAPI inserts or updates a CrossRepoAPIs row keyed on
// (repo_name, api_kind, api_identifier). On conflict the mutable fields
// (source_file, source_line, extractor, signature_hash, last_scanned_at)
// are updated. Returns the row's id.
func UpsertCrossRepoAPI(db *sql.DB, api CrossRepoAPI) (int, error) {
	_, err := db.Exec(`
		INSERT INTO CrossRepoAPIs
			(repo_name, api_kind, api_identifier, source_file, source_line,
			 extractor, signature_hash, last_scanned_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(repo_name, api_kind, api_identifier) DO UPDATE SET
			source_file     = excluded.source_file,
			source_line     = excluded.source_line,
			extractor       = excluded.extractor,
			signature_hash  = excluded.signature_hash,
			last_scanned_at = excluded.last_scanned_at`,
		api.RepoName, api.APIKind, api.APIIdentifier,
		api.SourceFile, api.SourceLine,
		api.Extractor, api.SignatureHash, api.LastScannedAt,
	)
	if err != nil {
		return 0, err
	}

	var id int
	err = db.QueryRow(`
		SELECT id FROM CrossRepoAPIs
		WHERE repo_name = ? AND api_kind = ? AND api_identifier = ?`,
		api.RepoName, api.APIKind, api.APIIdentifier,
	).Scan(&id)
	if err != nil {
		return 0, err
	}
	return id, nil
}

// UpsertCrossRepoAPIDependency inserts a CrossRepoAPIDependencies row.
// On conflict (same consumer_repo + consumer_file + consumer_line +
// provider_api_id) the row is ignored — use SoftDeleteCrossRepoAPIDependency
// to expire stale edges, then re-insert.
func UpsertCrossRepoAPIDependency(db *sql.DB, dep CrossRepoAPIDependency) error {
	_, err := db.Exec(`
		INSERT OR IGNORE INTO CrossRepoAPIDependencies
			(consumer_repo, consumer_file, consumer_line, provider_api_id,
			 call_kind, match_confidence, discovered_at, deleted_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		dep.ConsumerRepo, dep.ConsumerFile, dep.ConsumerLine, dep.ProviderAPIID,
		dep.CallKind, dep.MatchConf, dep.DiscoveredAt, dep.DeletedAt,
	)
	return err
}

// GetAPIBlastRadius returns all active (non-soft-deleted) CrossRepoAPIDependency
// rows for the given provider_api_id. Rows where deleted_at != '' are excluded.
func GetAPIBlastRadius(db *sql.DB, providerAPIID int) ([]CrossRepoAPIDependency, error) {
	rows, err := db.Query(`
		SELECT id, consumer_repo, consumer_file, consumer_line,
		       provider_api_id, call_kind, match_confidence,
		       discovered_at, IFNULL(deleted_at, '')
		FROM CrossRepoAPIDependencies
		WHERE provider_api_id = ? AND deleted_at = ''`,
		providerAPIID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []CrossRepoAPIDependency
	for rows.Next() {
		var d CrossRepoAPIDependency
		if err := rows.Scan(
			&d.ID, &d.ConsumerRepo, &d.ConsumerFile, &d.ConsumerLine,
			&d.ProviderAPIID, &d.CallKind, &d.MatchConf,
			&d.DiscoveredAt, &d.DeletedAt,
		); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// ListCrossRepoAPIs returns all CrossRepoAPIs rows for the given repo.
func ListCrossRepoAPIs(db *sql.DB, repoName string) ([]CrossRepoAPI, error) {
	rows, err := db.Query(`
		SELECT id, repo_name, api_kind, api_identifier, source_file, source_line,
		       extractor, signature_hash, last_scanned_at
		FROM CrossRepoAPIs
		WHERE repo_name = ?
		ORDER BY api_kind, api_identifier`,
		repoName,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []CrossRepoAPI
	for rows.Next() {
		var a CrossRepoAPI
		if err := rows.Scan(
			&a.ID, &a.RepoName, &a.APIKind, &a.APIIdentifier,
			&a.SourceFile, &a.SourceLine,
			&a.Extractor, &a.SignatureHash, &a.LastScannedAt,
		); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// SoftDeleteCrossRepoAPIDependency marks a CrossRepoAPIDependencies row as
// deleted by setting deleted_at to the current UTC time. Does not remove the
// row so the audit trail is preserved.
func SoftDeleteCrossRepoAPIDependency(db *sql.DB, id int) error {
	_, err := db.Exec(`
		UPDATE CrossRepoAPIDependencies
		SET deleted_at = ?
		WHERE id = ?`,
		NowSQLite(), id,
	)
	return err
}
