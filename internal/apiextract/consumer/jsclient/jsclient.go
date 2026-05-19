// Package jsclient extracts HTTP client call sites from JavaScript and
// TypeScript source files. It detects fetch() and axios.* invocations and
// emits CrossRepoAPIDependency rows with ProviderAPIID = 0 for P6 to resolve.
package jsclient

import (
	"regexp"
	"strings"

	"force-orchestrator/internal/store"
)

// Extractor implements apiextract.ConsumerExtractor for JS/TS files.
type Extractor struct{}

// New returns an initialised Extractor.
func New() *Extractor { return &Extractor{} }

// SupportedCallKinds returns the call_kind values produced by this extractor.
func (e *Extractor) SupportedCallKinds() []string {
	return []string{"fetch", "axios"}
}

// Compiled regexes — package-level for performance.
var (
	// fetch('/path') or fetch("/path")
	reFetchLiteral = regexp.MustCompile(`fetch\s*\(\s*['"]([^'"]+)['"]`)

	// fetch(`/path/${id}`) — template literal; extract prefix up to ${
	reFetchTemplate = regexp.MustCompile("fetch\\s*\\(\\s*`([^`]*?)(?:\\$\\{|`)")

	// axios.get('/path')
	reAxiosMethod = regexp.MustCompile(`axios\.(get|post|put|delete|patch)\s*\(\s*['"]([^'"]+)['"]`)

	// axios.get(`/path/${id}`)
	reAxiosMethodTemplate = regexp.MustCompile("axios\\.(get|post|put|delete|patch)\\s*\\(\\s*`([^`]*?)(?:\\$\\{|`)")

	// axios.request({...})
	reAxiosRequest = regexp.MustCompile(`axios\.request\s*\(`)

	// url: '/path'  or  url: "/path"
	reObjURL = regexp.MustCompile(`url\s*:\s*['"]([^'"]+)['"]`)

	// url: `/path/${id}`
	reObjURLTemplate = regexp.MustCompile("url\\s*:\\s*`([^`]*?)(?:\\$\\{|`)")

	// method: 'GET'
	reObjMethod = regexp.MustCompile(`(?i)method\s*:\s*['"]([A-Za-z]+)['"]`)
)

// Extract parses content line-by-line and returns dependency rows.
func (e *Extractor) Extract(repoName, filePath string, content []byte) ([]store.CrossRepoAPIDependency, error) {
	var deps []store.CrossRepoAPIDependency
	lines := strings.Split(string(content), "\n")

	for i, line := range lines {
		lineNo := i + 1

		// ---- fetch literal ----
		if m := reFetchLiteral.FindStringSubmatch(line); m != nil {
			path := m[1]
			if isHTTPPath(path) {
				deps = append(deps, makeDep(repoName, filePath, lineNo, "fetch", store.NormalizeAPIPath(path), 1.0))
			}
			continue
		}

		// ---- fetch template literal ----
		if m := reFetchTemplate.FindStringSubmatch(line); m != nil {
			prefix := strings.TrimRight(m[1], "/")
			if prefix != "" && isHTTPPath(prefix) {
				deps = append(deps, makeDep(repoName, filePath, lineNo, "fetch", store.NormalizeAPIPath(prefix), 0.7))
			}
			continue
		}

		// ---- axios.method literal ----
		if m := reAxiosMethod.FindStringSubmatch(line); m != nil {
			method := strings.ToUpper(m[1])
			path := m[2]
			if isHTTPPath(path) {
				identifier := method + " " + store.NormalizeAPIPath(path)
				deps = append(deps, makeDep(repoName, filePath, lineNo, "axios", identifier, 1.0))
			}
			continue
		}

		// ---- axios.method template literal ----
		if m := reAxiosMethodTemplate.FindStringSubmatch(line); m != nil {
			method := strings.ToUpper(m[1])
			prefix := strings.TrimRight(m[2], "/")
			if prefix != "" && isHTTPPath(prefix) {
				identifier := method + " " + store.NormalizeAPIPath(prefix)
				deps = append(deps, makeDep(repoName, filePath, lineNo, "axios", identifier, 0.7))
			}
			continue
		}

		// ---- axios.request({...}) — may span multiple lines ----
		if reAxiosRequest.MatchString(line) {
			end := i + 10
			if end > len(lines) {
				end = len(lines)
			}
			block := strings.Join(lines[i:end], "\n")

			var path string
			conf := 1.0

			if m := reObjURL.FindStringSubmatch(block); m != nil {
				path = m[1]
			} else if m := reObjURLTemplate.FindStringSubmatch(block); m != nil {
				path = strings.TrimRight(m[1], "/")
				conf = 0.7
			}

			if path == "" || !isHTTPPath(path) {
				continue
			}

			identifier := store.NormalizeAPIPath(path)
			if m := reObjMethod.FindStringSubmatch(block); m != nil {
				identifier = strings.ToUpper(m[1]) + " " + identifier
			}
			deps = append(deps, makeDep(repoName, filePath, lineNo, "axios", identifier, conf))
		}
	}

	return deps, nil
}

// isHTTPPath returns true when s looks like a URL path or full HTTP URL.
func isHTTPPath(s string) bool {
	return strings.HasPrefix(s, "/") ||
		strings.HasPrefix(s, "http://") ||
		strings.HasPrefix(s, "https://")
}

// makeDep constructs a dependency row with ProviderAPIID = 0.
func makeDep(repoName, filePath string, lineNo int, callKind, apiIdentifier string, conf float64) store.CrossRepoAPIDependency {
	return store.CrossRepoAPIDependency{
		ConsumerRepo:  repoName,
		ConsumerFile:  filePath,
		ConsumerLine:  lineNo,
		ProviderAPIID: 0,
		CallKind:      callKind,
		APIIdentifier: apiIdentifier,
		MatchConf:     conf,
		DiscoveredAt:  store.NowSQLite(),
	}
}
