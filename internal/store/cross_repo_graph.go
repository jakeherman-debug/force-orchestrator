// Package store: D8 Track 1 helpers for the cross-repo dependency graph.
// Mirrors the schema additions in schema.go (createSchema + runMigrations) and
// schema/schema.sql per CLAUDE.md § "Store / schema conventions".
//
// dogRepoGraphScan owns the writers (Upsert*); Track 2 (Chancellor blast-radius)
// owns the readers (List*). Track 3 (IntegTest) consumes both. Errors are
// returned per CLAUDE.md "no silent failures" — the dog routes them through
// the standard RunDogs error→operator-mail path.
package store

import (
	"database/sql"
	"fmt"
)

// CrossRepoSymbol is one exported-symbol row maintained by dogRepoGraphScan.
type CrossRepoSymbol struct {
	ID             int64
	RepoName       string
	SymbolPath     string // 'pkg.Type.Method' | 'module/api/UserHandler'
	SymbolKind     string // 'function' | 'type' | 'http_handler' | 'cli_command' | 'exported_const'
	FilePath       string // repo-relative
	LineNumber     int
	SignatureHash  string // AST-stable digest; unchanged across pure renames
	LastScannedAt  string // SQLite UTC
	IsPublic       bool
}

// CrossRepoDependency is one consumer→provider edge.
type CrossRepoDependency struct {
	ID               int64
	ConsumerRepoName string
	ConsumerFile     string
	ConsumerLine     int
	ProviderSymbolID int64
	DiscoveredAt     string
	DeletedAt        string // '' = live; non-empty = soft-deleted
}

// UpsertCrossRepoSymbol inserts or updates a symbol row keyed by
// (repo_name, symbol_path). Returns the row id.
//
// Idempotence: re-scanning the same symbol re-stamps last_scanned_at and the
// downstream metadata (file_path, line, signature_hash, is_public, kind) but
// preserves the row id so existing CrossRepoDependencies edges keep pointing
// at the right symbol across renames-of-non-key-fields.
func UpsertCrossRepoSymbol(db *sql.DB, s CrossRepoSymbol) (int64, error) {
	if db == nil {
		return 0, fmt.Errorf("UpsertCrossRepoSymbol: db is nil")
	}
	if s.RepoName == "" || s.SymbolPath == "" || s.SymbolKind == "" {
		return 0, fmt.Errorf("UpsertCrossRepoSymbol: repo_name, symbol_path, symbol_kind required")
	}
	if s.LastScannedAt == "" {
		s.LastScannedAt = NowSQLite()
	}
	isPub := 0
	if s.IsPublic {
		isPub = 1
	}
	_, err := db.Exec(`INSERT INTO CrossRepoSymbols
			(repo_name, symbol_path, symbol_kind, file_path, line_number,
			 signature_hash, last_scanned_at, is_public)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(repo_name, symbol_path) DO UPDATE SET
			symbol_kind     = excluded.symbol_kind,
			file_path       = excluded.file_path,
			line_number     = excluded.line_number,
			signature_hash  = excluded.signature_hash,
			last_scanned_at = excluded.last_scanned_at,
			is_public       = excluded.is_public`,
		s.RepoName, s.SymbolPath, s.SymbolKind, s.FilePath, s.LineNumber,
		s.SignatureHash, s.LastScannedAt, isPub)
	if err != nil {
		return 0, fmt.Errorf("UpsertCrossRepoSymbol(%s, %s): %w", s.RepoName, s.SymbolPath, err)
	}
	var id int64
	if qErr := db.QueryRow(`SELECT id FROM CrossRepoSymbols
			WHERE repo_name = ? AND symbol_path = ?`,
		s.RepoName, s.SymbolPath).Scan(&id); qErr != nil {
		return 0, fmt.Errorf("UpsertCrossRepoSymbol(%s, %s): id lookup: %w", s.RepoName, s.SymbolPath, qErr)
	}
	return id, nil
}

// LookupCrossRepoSymbolID returns the id for the given (repo, symbol_path) or
// 0 + sql.ErrNoRows when the symbol hasn't been scanned yet. Used by
// dogRepoGraphScan to resolve consumer call-sites after the symbol pass.
func LookupCrossRepoSymbolID(db *sql.DB, repoName, symbolPath string) (int64, error) {
	if db == nil {
		return 0, fmt.Errorf("LookupCrossRepoSymbolID: db is nil")
	}
	var id int64
	err := db.QueryRow(`SELECT id FROM CrossRepoSymbols
		WHERE repo_name = ? AND symbol_path = ?`,
		repoName, symbolPath).Scan(&id)
	if err != nil {
		return 0, err
	}
	return id, nil
}

// UpsertCrossRepoDependency inserts or revives an edge keyed by
// (consumer_repo, file, line, provider_symbol_id). On revive (the row exists
// with a non-empty deleted_at) it clears deleted_at and re-stamps
// discovered_at — a re-emerged consumer site should appear "fresh" rather than
// carrying its prior soft-delete tombstone. Returns the row id.
func UpsertCrossRepoDependency(db *sql.DB, d CrossRepoDependency) (int64, error) {
	if db == nil {
		return 0, fmt.Errorf("UpsertCrossRepoDependency: db is nil")
	}
	if d.ConsumerRepoName == "" || d.ConsumerFile == "" || d.ProviderSymbolID == 0 {
		return 0, fmt.Errorf("UpsertCrossRepoDependency: consumer_repo, consumer_file, provider_symbol_id required")
	}
	if d.DiscoveredAt == "" {
		d.DiscoveredAt = NowSQLite()
	}
	_, err := db.Exec(`INSERT INTO CrossRepoDependencies
			(consumer_repo_name, consumer_file, consumer_line,
			 provider_symbol_id, discovered_at, deleted_at)
			VALUES (?, ?, ?, ?, ?, '')
		ON CONFLICT(consumer_repo_name, consumer_file, consumer_line, provider_symbol_id)
		DO UPDATE SET
			discovered_at = excluded.discovered_at,
			deleted_at    = ''`,
		d.ConsumerRepoName, d.ConsumerFile, d.ConsumerLine,
		d.ProviderSymbolID, d.DiscoveredAt)
	if err != nil {
		return 0, fmt.Errorf("UpsertCrossRepoDependency(%s, %s:%d → %d): %w",
			d.ConsumerRepoName, d.ConsumerFile, d.ConsumerLine, d.ProviderSymbolID, err)
	}
	var id int64
	if qErr := db.QueryRow(`SELECT id FROM CrossRepoDependencies
			WHERE consumer_repo_name = ? AND consumer_file = ?
			  AND consumer_line = ? AND provider_symbol_id = ?`,
		d.ConsumerRepoName, d.ConsumerFile, d.ConsumerLine, d.ProviderSymbolID).Scan(&id); qErr != nil {
		return 0, fmt.Errorf("UpsertCrossRepoDependency: id lookup: %w", qErr)
	}
	return id, nil
}

// SoftDeleteCrossRepoDependency stamps deleted_at on a single edge row. The
// row stays in the table so (a) historical queries still work, and (b) a
// re-emerged consumer site can be revived via UpsertCrossRepoDependency
// without losing its row id (which downstream alerts may reference).
func SoftDeleteCrossRepoDependency(db *sql.DB, id int64) error {
	if db == nil {
		return fmt.Errorf("SoftDeleteCrossRepoDependency: db is nil")
	}
	_, err := db.Exec(`UPDATE CrossRepoDependencies
		SET deleted_at = ?
		WHERE id = ? AND deleted_at = ''`,
		NowSQLite(), id)
	if err != nil {
		return fmt.Errorf("SoftDeleteCrossRepoDependency(%d): %w", id, err)
	}
	return nil
}

// SoftDeleteCrossRepoDependenciesNotIn soft-deletes every live edge for
// (consumer_repo, consumer_file) whose id is NOT in the keep set. Used by
// dogRepoGraphScan after a per-file rescan: edges discovered this pass stay
// live; everything else for the same file becomes a tombstone. Returns the
// number of rows tombstoned.
//
// keep may be empty — that's the legitimate "file no longer references any
// scanned provider" case. We pass keep through a NOT IN (...) clause built
// inline rather than via a temp-table because the typical fan-out is < 50.
func SoftDeleteCrossRepoDependenciesNotIn(db *sql.DB, consumerRepo, consumerFile string, keep []int64) (int64, error) {
	if db == nil {
		return 0, fmt.Errorf("SoftDeleteCrossRepoDependenciesNotIn: db is nil")
	}
	if consumerRepo == "" || consumerFile == "" {
		return 0, fmt.Errorf("SoftDeleteCrossRepoDependenciesNotIn: consumer_repo and consumer_file required")
	}
	now := NowSQLite()
	// Build the NOT IN clause inline. We pass nothing in keep as the
	// degenerate case → "soft-delete everything live for this file."
	if len(keep) == 0 {
		res, err := db.Exec(`UPDATE CrossRepoDependencies
			SET deleted_at = ?
			WHERE consumer_repo_name = ? AND consumer_file = ? AND deleted_at = ''`,
			now, consumerRepo, consumerFile)
		if err != nil {
			return 0, fmt.Errorf("SoftDeleteCrossRepoDependenciesNotIn(%s, %s): %w", consumerRepo, consumerFile, err)
		}
		n, _ := res.RowsAffected()
		return n, nil
	}
	// Build "?, ?, ?" placeholders for the keep set.
	placeholders := ""
	args := []any{now, consumerRepo, consumerFile}
	for i, id := range keep {
		if i > 0 {
			placeholders += ","
		}
		placeholders += "?"
		args = append(args, id)
	}
	q := `UPDATE CrossRepoDependencies
		SET deleted_at = ?
		WHERE consumer_repo_name = ? AND consumer_file = ?
		  AND deleted_at = ''
		  AND id NOT IN (` + placeholders + `)`
	res, err := db.Exec(q, args...)
	if err != nil {
		return 0, fmt.Errorf("SoftDeleteCrossRepoDependenciesNotIn(%s, %s): %w", consumerRepo, consumerFile, err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// ListProvidersOfSymbol returns every CrossRepoSymbols row for `repoName`
// whose symbol_path matches `symbolPath` exactly. Used by Chancellor (Track 2)
// to walk "every place that exports this symbol" — usually a single row, but
// the API accepts the multi-repo case (e.g. a fork) without the caller having
// to know.
func ListProvidersOfSymbol(db *sql.DB, repoName, symbolPath string) ([]CrossRepoSymbol, error) {
	if db == nil {
		return nil, fmt.Errorf("ListProvidersOfSymbol: db is nil")
	}
	rows, err := db.Query(`SELECT id, repo_name, symbol_path, symbol_kind, file_path,
			line_number, signature_hash, last_scanned_at, is_public
		FROM CrossRepoSymbols
		WHERE repo_name = ? AND symbol_path = ?`,
		repoName, symbolPath)
	if err != nil {
		return nil, fmt.Errorf("ListProvidersOfSymbol(%s, %s): %w", repoName, symbolPath, err)
	}
	defer rows.Close()
	var out []CrossRepoSymbol
	for rows.Next() {
		var s CrossRepoSymbol
		var pub int
		if sErr := rows.Scan(&s.ID, &s.RepoName, &s.SymbolPath, &s.SymbolKind,
			&s.FilePath, &s.LineNumber, &s.SignatureHash, &s.LastScannedAt, &pub); sErr != nil {
			return nil, fmt.Errorf("ListProvidersOfSymbol(%s, %s): scan: %w", repoName, symbolPath, sErr)
		}
		s.IsPublic = pub == 1
		out = append(out, s)
	}
	if rErr := rows.Err(); rErr != nil {
		return nil, fmt.Errorf("ListProvidersOfSymbol(%s, %s): iter: %w", repoName, symbolPath, rErr)
	}
	return out, nil
}

// ListConsumersOfSymbol returns every live (deleted_at='') edge pointing at
// the given provider symbol id. Used by Chancellor's blast-radius pass.
// Soft-deleted edges are excluded — Track 2's blast-radius runs against the
// "currently observed" graph, not the historical one.
func ListConsumersOfSymbol(db *sql.DB, providerSymbolID int64) ([]CrossRepoDependency, error) {
	if db == nil {
		return nil, fmt.Errorf("ListConsumersOfSymbol: db is nil")
	}
	rows, err := db.Query(`SELECT id, consumer_repo_name, consumer_file, consumer_line,
			provider_symbol_id, discovered_at, deleted_at
		FROM CrossRepoDependencies
		WHERE provider_symbol_id = ? AND deleted_at = ''
		ORDER BY consumer_repo_name, consumer_file, consumer_line`,
		providerSymbolID)
	if err != nil {
		return nil, fmt.Errorf("ListConsumersOfSymbol(%d): %w", providerSymbolID, err)
	}
	defer rows.Close()
	var out []CrossRepoDependency
	for rows.Next() {
		var d CrossRepoDependency
		if sErr := rows.Scan(&d.ID, &d.ConsumerRepoName, &d.ConsumerFile, &d.ConsumerLine,
			&d.ProviderSymbolID, &d.DiscoveredAt, &d.DeletedAt); sErr != nil {
			return nil, fmt.Errorf("ListConsumersOfSymbol(%d): scan: %w", providerSymbolID, sErr)
		}
		out = append(out, d)
	}
	if rErr := rows.Err(); rErr != nil {
		return nil, fmt.Errorf("ListConsumersOfSymbol(%d): iter: %w", providerSymbolID, rErr)
	}
	return out, nil
}

// ListLiveDependenciesForConsumerFile returns every live edge for
// (consumer_repo, consumer_file). Used by dogRepoGraphScan during a per-file
// rescan to compute the "edges to soft-delete" set.
func ListLiveDependenciesForConsumerFile(db *sql.DB, consumerRepo, consumerFile string) ([]CrossRepoDependency, error) {
	if db == nil {
		return nil, fmt.Errorf("ListLiveDependenciesForConsumerFile: db is nil")
	}
	rows, err := db.Query(`SELECT id, consumer_repo_name, consumer_file, consumer_line,
			provider_symbol_id, discovered_at, deleted_at
		FROM CrossRepoDependencies
		WHERE consumer_repo_name = ? AND consumer_file = ? AND deleted_at = ''`,
		consumerRepo, consumerFile)
	if err != nil {
		return nil, fmt.Errorf("ListLiveDependenciesForConsumerFile(%s, %s): %w", consumerRepo, consumerFile, err)
	}
	defer rows.Close()
	var out []CrossRepoDependency
	for rows.Next() {
		var d CrossRepoDependency
		if sErr := rows.Scan(&d.ID, &d.ConsumerRepoName, &d.ConsumerFile, &d.ConsumerLine,
			&d.ProviderSymbolID, &d.DiscoveredAt, &d.DeletedAt); sErr != nil {
			return nil, fmt.Errorf("ListLiveDependenciesForConsumerFile(%s, %s): scan: %w", consumerRepo, consumerFile, sErr)
		}
		out = append(out, d)
	}
	if rErr := rows.Err(); rErr != nil {
		return nil, fmt.Errorf("ListLiveDependenciesForConsumerFile(%s, %s): iter: %w", consumerRepo, consumerFile, rErr)
	}
	return out, nil
}

// ListLiveDependenciesForConsumerRepo returns every live edge for the given
// consumer repo. Used by dogRepoGraphScan when a previously-scanned consumer
// file no longer exists — its edges need to be soft-deleted in bulk.
func ListLiveDependenciesForConsumerRepo(db *sql.DB, consumerRepo string) ([]CrossRepoDependency, error) {
	if db == nil {
		return nil, fmt.Errorf("ListLiveDependenciesForConsumerRepo: db is nil")
	}
	rows, err := db.Query(`SELECT id, consumer_repo_name, consumer_file, consumer_line,
			provider_symbol_id, discovered_at, deleted_at
		FROM CrossRepoDependencies
		WHERE consumer_repo_name = ? AND deleted_at = ''`,
		consumerRepo)
	if err != nil {
		return nil, fmt.Errorf("ListLiveDependenciesForConsumerRepo(%s): %w", consumerRepo, err)
	}
	defer rows.Close()
	var out []CrossRepoDependency
	for rows.Next() {
		var d CrossRepoDependency
		if sErr := rows.Scan(&d.ID, &d.ConsumerRepoName, &d.ConsumerFile, &d.ConsumerLine,
			&d.ProviderSymbolID, &d.DiscoveredAt, &d.DeletedAt); sErr != nil {
			return nil, fmt.Errorf("ListLiveDependenciesForConsumerRepo(%s): scan: %w", consumerRepo, sErr)
		}
		out = append(out, d)
	}
	if rErr := rows.Err(); rErr != nil {
		return nil, fmt.Errorf("ListLiveDependenciesForConsumerRepo(%s): iter: %w", consumerRepo, rErr)
	}
	return out, nil
}

// ListCrossRepoSymbolsByRepo returns every CrossRepoSymbols row for the
// given repo. Used by Chancellor's blast-radius post-process (D8 Track 2)
// to enumerate the set of provider symbols the repo exports — the
// post-process scans each Feature task's payload against this set to
// identify which symbols a planned modification would touch.
//
// Returns rows ordered by symbol_path ASC for stable iteration.
func ListCrossRepoSymbolsByRepo(db *sql.DB, repoName string) ([]CrossRepoSymbol, error) {
	if db == nil {
		return nil, fmt.Errorf("ListCrossRepoSymbolsByRepo: db is nil")
	}
	if repoName == "" {
		return nil, fmt.Errorf("ListCrossRepoSymbolsByRepo: repoName required")
	}
	rows, err := db.Query(`SELECT id, repo_name, symbol_path, symbol_kind, file_path,
			line_number, signature_hash, last_scanned_at, is_public
		FROM CrossRepoSymbols
		WHERE repo_name = ?
		ORDER BY symbol_path ASC`,
		repoName)
	if err != nil {
		return nil, fmt.Errorf("ListCrossRepoSymbolsByRepo(%s): %w", repoName, err)
	}
	defer rows.Close()
	var out []CrossRepoSymbol
	for rows.Next() {
		var s CrossRepoSymbol
		var pub int
		if sErr := rows.Scan(&s.ID, &s.RepoName, &s.SymbolPath, &s.SymbolKind,
			&s.FilePath, &s.LineNumber, &s.SignatureHash, &s.LastScannedAt, &pub); sErr != nil {
			return nil, fmt.Errorf("ListCrossRepoSymbolsByRepo(%s): scan: %w", repoName, sErr)
		}
		s.IsPublic = pub == 1
		out = append(out, s)
	}
	if rErr := rows.Err(); rErr != nil {
		return nil, fmt.Errorf("ListCrossRepoSymbolsByRepo(%s): iter: %w", repoName, rErr)
	}
	return out, nil
}

// CountCrossRepoSymbols / CountCrossRepoDependencies are convenience helpers
// the dog logs at end-of-run for operator-visibility (mirrors how
// supply-allowlist-refresh logs per-ecosystem package counts).
func CountCrossRepoSymbols(db *sql.DB) (int, error) {
	if db == nil {
		return 0, fmt.Errorf("CountCrossRepoSymbols: db is nil")
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM CrossRepoSymbols`).Scan(&n); err != nil {
		return 0, fmt.Errorf("CountCrossRepoSymbols: %w", err)
	}
	return n, nil
}

func CountCrossRepoDependencies(db *sql.DB, includeSoftDeleted bool) (int, error) {
	if db == nil {
		return 0, fmt.Errorf("CountCrossRepoDependencies: db is nil")
	}
	q := `SELECT COUNT(*) FROM CrossRepoDependencies WHERE deleted_at = ''`
	if includeSoftDeleted {
		q = `SELECT COUNT(*) FROM CrossRepoDependencies`
	}
	var n int
	if err := db.QueryRow(q).Scan(&n); err != nil {
		return 0, fmt.Errorf("CountCrossRepoDependencies: %w", err)
	}
	return n, nil
}
