// Package scanner provides the Scanner struct that walks a repository
// filesystem and dispatches files to registered extractors. It imports
// only the apiextract registry and store packages — never any concrete
// extractor implementations (rails, spring, express, etc.). The single
// wiring point for concrete extractors is cmd/force/fleet_cmds.go.
package scanner

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"force-orchestrator/internal/apiextract"
	"force-orchestrator/internal/store"
)

// Scanner walks a repository tree and dispatches files to the registered
// ProviderExtractors and ConsumerExtractors in the given registry.
type Scanner struct {
	registry *apiextract.ExtractorRegistry
	db       *sql.DB
}

// New returns a Scanner bound to the given DB and registry.
func New(db *sql.DB, registry *apiextract.ExtractorRegistry) *Scanner {
	return &Scanner{registry: registry, db: db}
}

// ScanProviders walks all files under repoPath, invokes all matching
// ProviderExtractors for each file by extension, and upserts the resulting
// CrossRepoAPIs rows. Returns the total count of rows upserted.
func (s *Scanner) ScanProviders(ctx context.Context, repoName, repoPath string) (int, error) {
	providers := s.registry.AllProviders()
	if len(providers) == 0 {
		return 0, nil
	}

	total := 0
	err := filepath.WalkDir(repoPath, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // skip unreadable entries
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if d.IsDir() {
			if shouldSkipDir(d.Name()) {
				return filepath.SkipDir
			}
			return nil
		}

		matched := matchProviders(path, providers)
		if len(matched) == 0 {
			return nil
		}

		content, rErr := os.ReadFile(path)
		if rErr != nil {
			// Skip unreadable files — not a scan failure.
			return nil
		}
		relPath := relOrAbs(repoPath, path)
		hash := sha256sum(content)
		now := store.NowSQLite()

		for _, ex := range matched {
			apis, xErr := ex.Extract(repoName, relPath, content)
			if xErr != nil {
				// Per CLAUDE.md "no silent failures": log-grade skip, not a
				// top-level error — a single bad file doesn't abort the scan.
				continue
			}
			for _, api := range apis {
				api.RepoName = repoName
				api.SourceFile = relPath
				api.Extractor = ex.ExtractorName()
				api.SignatureHash = hash
				api.LastScannedAt = now
				api.APIIdentifier = store.NormalizeAPIPath(api.APIIdentifier)
				if _, uErr := store.UpsertCrossRepoAPI(s.db, api); uErr != nil {
					return fmt.Errorf("scanner: upsert CrossRepoAPI: %w", uErr)
				}
				total++
			}
		}
		return nil
	})
	if err != nil {
		return total, fmt.Errorf("ScanProviders(%s): walk: %w", repoName, err)
	}
	return total, nil
}

// ScanConsumers walks all files under repoPath, invokes all matching
// ConsumerExtractors for each file by extension. For each extracted dependency,
// it attempts to resolve ProviderAPIID via CrossRepoAPIs lookup before upserting.
// Only resolved rows (ProviderAPIID > 0) are written to CrossRepoAPIDependencies.
// Returns the count of dependency rows upserted (only resolved rows count).
//
// The foreign-key constraint on CrossRepoAPIDependencies.provider_api_id prevents
// inserting rows with ProviderAPIID = 0. Resolution therefore happens inline
// before each upsert. Unresolvable call-sites are collected in-memory and returned
// via the companion ResolveConsumerDependenciesWithDeps function once a separate
// ScanProviders pass has run (e.g. on a second fleet-wide scan cycle).
func (s *Scanner) ScanConsumers(ctx context.Context, repoName, repoPath string) (int, error) {
	consumers := s.registry.AllConsumers()
	if len(consumers) == 0 {
		return 0, nil
	}

	total := 0
	err := filepath.WalkDir(repoPath, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if d.IsDir() {
			if shouldSkipDir(d.Name()) {
				return filepath.SkipDir
			}
			return nil
		}

		matched := matchConsumers(path, consumers)
		if len(matched) == 0 {
			return nil
		}

		content, rErr := os.ReadFile(path)
		if rErr != nil {
			return nil
		}
		relPath := relOrAbs(repoPath, path)
		now := store.NowSQLite()

		for _, ex := range matched {
			deps, xErr := ex.Extract(repoName, relPath, content)
			if xErr != nil {
				continue
			}
			for _, dep := range deps {
				dep.ConsumerRepo = repoName
				dep.ConsumerFile = relPath
				dep.DiscoveredAt = now

				// Inline resolution: look up CrossRepoAPIs by normalised identifier.
				// The FK constraint on CrossRepoAPIDependencies.provider_api_id
				// requires a valid CrossRepoAPIs.id — provider_api_id = 0 is not
				// insertable while foreign_keys PRAGMA is ON.
				if dep.APIIdentifier != "" {
					norm := store.NormalizeAPIPath(dep.APIIdentifier)
					var apiID int
					lookupErr := s.db.QueryRowContext(ctx,
						`SELECT id FROM CrossRepoAPIs WHERE api_identifier = ? LIMIT 1`, norm,
					).Scan(&apiID)
					if lookupErr == nil {
						dep.ProviderAPIID = apiID
					}
					// If no match, dep.ProviderAPIID stays 0 — not persisted (FK violation).
				}

				if dep.ProviderAPIID == 0 {
					// Unresolvable at this scan cycle. Skip without error — the
					// path-matcher will retry on subsequent scans once providers land.
					continue
				}

				if uErr := store.UpsertCrossRepoAPIDependency(s.db, dep); uErr != nil {
					return fmt.Errorf("scanner: upsert CrossRepoAPIDependency: %w", uErr)
				}
				total++
			}
		}
		return nil
	})
	if err != nil {
		return total, fmt.Errorf("ScanConsumers(%s): walk: %w", repoName, err)
	}
	return total, nil
}

// ---- file-routing helpers ----

// matchProviders returns the subset of providers that match the given file path.
// Routing rules mirror the spec:
//   - .rb           → rails extractor (ExtractorName "rails-routes")
//   - .proto        → proto extractor
//   - openapi*.yaml/.yml/.json or swagger*.yaml/.yml/.json → openapi extractor
//   - .java         → spring extractor
//   - .kt           → spring + ktor extractors
//   - .js / .ts     → express + nestjs extractors
//
// Matching is by ExtractorName prefix for routing clarity.
func matchProviders(path string, providers []apiextract.ProviderExtractor) []apiextract.ProviderExtractor {
	base := strings.ToLower(filepath.Base(path))
	ext := strings.ToLower(filepath.Ext(path))
	var out []apiextract.ProviderExtractor
	for _, p := range providers {
		if wantsFile(p.ExtractorName(), base, ext) {
			out = append(out, p)
		}
	}
	return out
}

// matchConsumers returns the subset of consumers that match the given file path.
func matchConsumers(path string, consumers []apiextract.ConsumerExtractor) []apiextract.ConsumerExtractor {
	base := strings.ToLower(filepath.Base(path))
	ext := strings.ToLower(filepath.Ext(path))
	var out []apiextract.ConsumerExtractor
	for _, c := range consumers {
		if consumerWantsFile(c, base, ext) {
			out = append(out, c)
		}
	}
	return out
}

// wantsFile maps extractor name → file extension/name pattern.
func wantsFile(extractorName, base, ext string) bool {
	switch {
	case strings.HasPrefix(extractorName, "rails"):
		return ext == ".rb"
	case strings.HasPrefix(extractorName, "proto"):
		return ext == ".proto"
	case strings.HasPrefix(extractorName, "openapi") || strings.HasPrefix(extractorName, "swagger"):
		// openapi*.yaml, openapi*.yml, openapi*.json, swagger*.yaml, etc.
		if ext != ".yaml" && ext != ".yml" && ext != ".json" {
			return false
		}
		return strings.HasPrefix(base, "openapi") || strings.HasPrefix(base, "swagger")
	case strings.HasPrefix(extractorName, "spring"):
		return ext == ".java" || ext == ".kt"
	case strings.HasPrefix(extractorName, "ktor"):
		return ext == ".kt"
	case strings.HasPrefix(extractorName, "express"):
		return ext == ".js" || ext == ".ts"
	case strings.HasPrefix(extractorName, "nestjs"):
		return ext == ".js" || ext == ".ts"
	default:
		return false
	}
}

// consumerWantsFile maps consumer extractor call-kind → file extension.
func consumerWantsFile(c apiextract.ConsumerExtractor, base, ext string) bool {
	_ = base
	kinds := c.SupportedCallKinds()
	if len(kinds) == 0 {
		return false
	}
	// Route by first supported call kind (each consumer extractor has a
	// well-known primary kind that implies its language).
	primary := kinds[0]
	switch {
	case primary == "fetch" || primary == "axios":
		// JS/TS HTTP client
		return ext == ".js" || ext == ".ts"
	case primary == "ruby-http":
		// Ruby HTTP client
		return ext == ".rb"
	case primary == "java-http":
		// Java HTTP client
		return ext == ".java" || ext == ".kt"
	case primary == "grpc":
		// Go gRPC client
		return ext == ".go"
	default:
		return false
	}
}

// shouldSkipDir returns true for directories that should not be scanned
// (vendor, node_modules, hidden dirs, build outputs).
func shouldSkipDir(name string) bool {
	switch name {
	case "vendor", "node_modules", ".git", ".svn", "target", "build",
		"dist", "__pycache__", ".idea", ".vscode":
		return true
	}
	return strings.HasPrefix(name, ".")
}

// relOrAbs returns path relative to base, falling back to path if it
// can't be made relative (shouldn't happen in practice).
func relOrAbs(base, path string) string {
	rel, err := filepath.Rel(base, path)
	if err != nil {
		return path
	}
	return rel
}

// sha256sum returns the hex-encoded SHA-256 of content, used as
// SignatureHash to detect unchanged files on re-scan.
func sha256sum(content []byte) string {
	h := sha256.Sum256(content)
	return hex.EncodeToString(h[:])
}
