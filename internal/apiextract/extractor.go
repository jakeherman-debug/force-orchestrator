// Package apiextract defines the ProviderExtractor interface that each
// sub-package implements to parse source files and emit CrossRepoAPI rows.
package apiextract

import "force-orchestrator/internal/store"

// ProviderExtractor extracts API definitions from source files.
type ProviderExtractor interface {
	// Kind returns the api_kind this extractor produces (e.g. "http_route", "proto_event").
	Kind() string
	// ExtractorName returns the extractor label stored in CrossRepoAPIs.extractor.
	ExtractorName() string
	// Extract parses the file at path and returns API rows.
	Extract(repoName, filePath string, content []byte) ([]store.CrossRepoAPI, error)
}
