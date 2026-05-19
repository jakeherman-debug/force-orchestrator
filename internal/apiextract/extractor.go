// Package apiextract defines the ProviderExtractor interface implemented by
// each language/framework-specific extractor. Extractors parse source files
// and return zero or more CrossRepoAPI rows representing the HTTP (or other
// API-surface) endpoints declared in that file.
package apiextract

import "force-orchestrator/internal/store"

// ProviderExtractor is implemented by each language/framework extractor.
// A single extractor handles one framework (e.g. Spring annotations, Ktor DSL)
// and is responsible for understanding its own file extensions.
type ProviderExtractor interface {
	// Kind returns the api_kind value written to CrossRepoAPIs rows
	// (e.g. "http_route").
	Kind() string

	// ExtractorName returns the extractor label written to CrossRepoAPIs rows
	// (e.g. "spring-annotation", "ktor-routing").
	ExtractorName() string

	// Extract parses content (a single source file) and returns all API
	// surface rows found. repoName is stored verbatim; filePath is stored as
	// SourceFile. Returns nil, nil when the file contains no recognisable
	// API declarations. Errors are reserved for I/O or parse failures — an
	// empty file is not an error.
	Extract(repoName, filePath string, content []byte) ([]store.CrossRepoAPI, error)
}
