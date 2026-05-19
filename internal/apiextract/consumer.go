package apiextract

import "force-orchestrator/internal/store"

// ConsumerExtractor extracts API call sites from source files.
type ConsumerExtractor interface {
	// SupportedCallKinds returns the call_kind values this extractor produces.
	SupportedCallKinds() []string
	// Extract parses the file and returns dependency rows.
	// ProviderAPIID is left as 0 — the path-matcher in P6 resolves it.
	Extract(repoName, filePath string, content []byte) ([]store.CrossRepoAPIDependency, error)
}
