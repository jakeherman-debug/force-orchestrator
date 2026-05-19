package scanner

import (
	"context"
	"database/sql"
	"fmt"

	"force-orchestrator/internal/store"
)

// ResolveConsumerDependencies matches CrossRepoAPIDependency rows that have
// provider_api_id = 0 against CrossRepoAPIs rows by normalised api_identifier.
// Updates provider_api_id for matched rows. Unresolved rows stay at 0.
// Returns the count of rows updated.
//
// NOTE: due to the SQLite foreign-key constraint on provider_api_id, ScanConsumers
// never inserts rows with provider_api_id = 0. ResolveConsumerDependencies is
// therefore a "second-pass" path for rows that were inserted in an earlier scan
// cycle before the provider APIs were registered, or for any future schema
// relaxation. It is also the canonical test-facing entry point for path-matching
// logic tests (TestPattern_APIConsumerProviderResolverComplete seeds a row with
// provider_api_id = 0 by bypassing the FK check via a raw INSERT).
//
// In the normal scanner flow, ResolveConsumerDependenciesWithDeps is called
// with the in-memory dep slice produced by ScanConsumers so that the transient
// APIIdentifier field is still available without a DB round-trip.
func ResolveConsumerDependencies(ctx context.Context, db *sql.DB) (int, error) {
	if db == nil {
		return 0, fmt.Errorf("ResolveConsumerDependencies: db is nil")
	}

	// Fetch all unresolved dependency rows.
	rows, err := db.QueryContext(ctx, `
		SELECT id, consumer_repo, consumer_file, consumer_line, call_kind
		FROM CrossRepoAPIDependencies
		WHERE provider_api_id = 0 AND IFNULL(deleted_at,'') = ''`)
	if err != nil {
		return 0, fmt.Errorf("ResolveConsumerDependencies: query unresolved: %w", err)
	}

	type depRow struct {
		id                         int
		consumerRepo, consumerFile string
		line                       int
		callKind                   string
	}
	var deps []depRow
	for rows.Next() {
		var d depRow
		if sErr := rows.Scan(&d.id, &d.consumerRepo, &d.consumerFile, &d.line, &d.callKind); sErr != nil {
			rows.Close()
			return 0, fmt.Errorf("ResolveConsumerDependencies: scan: %w", sErr)
		}
		deps = append(deps, d)
	}
	rows.Close()
	if rErr := rows.Err(); rErr != nil {
		return 0, fmt.Errorf("ResolveConsumerDependencies: rows: %w", rErr)
	}

	if len(deps) == 0 {
		return 0, nil
	}

	// Build a normalised identifier → api ID map for O(1) lookup.
	apiRows, err := db.QueryContext(ctx, `SELECT id, api_identifier FROM CrossRepoAPIs`)
	if err != nil {
		return 0, fmt.Errorf("ResolveConsumerDependencies: query providers: %w", err)
	}
	normalized := make(map[string]int)
	for apiRows.Next() {
		var id int
		var identifier string
		if sErr := apiRows.Scan(&id, &identifier); sErr != nil {
			apiRows.Close()
			return 0, fmt.Errorf("ResolveConsumerDependencies: scan api: %w", sErr)
		}
		// api_identifier rows are already normalised by ScanProviders.
		normalized[identifier] = id
	}
	apiRows.Close()
	if rErr := apiRows.Err(); rErr != nil {
		return 0, fmt.Errorf("ResolveConsumerDependencies: api rows: %w", rErr)
	}

	updated := 0
	for _, dep := range deps {
		// consumer_file is used as a proxy for the API identifier in this
		// DB-only resolution path. When it looks like a URL path (starts with '/')
		// we try to match it directly against normalised provider API identifiers.
		candidate := store.NormalizeAPIPath(dep.consumerFile)
		if apiID, ok := normalized[candidate]; ok {
			_, uErr := db.ExecContext(ctx,
				`UPDATE CrossRepoAPIDependencies SET provider_api_id = ? WHERE id = ?`,
				apiID, dep.id)
			if uErr != nil {
				return updated, fmt.Errorf("ResolveConsumerDependencies: update dep %d: %w", dep.id, uErr)
			}
			updated++
		}
	}
	return updated, nil
}

// ResolveConsumerDependenciesWithDeps resolves the in-memory dep slice produced
// by ScanConsumers. For each dep with ProviderAPIID = 0 and a non-empty
// APIIdentifier, queries CrossRepoAPIs for a matching api_identifier and
// updates the DB row. Returns the count resolved.
//
// This is the primary resolution path — called from the dog body with the live
// dep structs so the transient APIIdentifier field is still available.
func ResolveConsumerDependenciesWithDeps(ctx context.Context, db *sql.DB, deps []store.CrossRepoAPIDependency) (int, error) {
	if db == nil {
		return 0, fmt.Errorf("ResolveConsumerDependenciesWithDeps: db is nil")
	}
	resolved := 0
	for _, dep := range deps {
		if dep.ProviderAPIID != 0 || dep.APIIdentifier == "" {
			continue
		}
		norm := store.NormalizeAPIPath(dep.APIIdentifier)
		var apiID int
		err := db.QueryRowContext(ctx,
			`SELECT id FROM CrossRepoAPIs WHERE api_identifier = ? LIMIT 1`, norm,
		).Scan(&apiID)
		if err == sql.ErrNoRows {
			continue // unresolvable — leave ProviderAPIID = 0
		}
		if err != nil {
			return resolved, fmt.Errorf("ResolveConsumerDependenciesWithDeps: lookup %q: %w", norm, err)
		}
		_, uErr := db.ExecContext(ctx,
			`UPDATE CrossRepoAPIDependencies SET provider_api_id = ?
			 WHERE consumer_repo = ? AND consumer_file = ? AND consumer_line = ?
			   AND provider_api_id = 0`,
			apiID, dep.ConsumerRepo, dep.ConsumerFile, dep.ConsumerLine)
		if uErr != nil {
			return resolved, fmt.Errorf("ResolveConsumerDependenciesWithDeps: update dep: %w", uErr)
		}
		resolved++
	}
	return resolved, nil
}
