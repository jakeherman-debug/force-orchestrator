// Package rubyclient extracts HTTP client call sites from Ruby source files.
// It detects HTTParty, Faraday, Net::HTTP, and RestClient invocations and
// emits CrossRepoAPIDependency rows with ProviderAPIID = 0 for P6 to resolve.
package rubyclient

import (
	"regexp"
	"strings"

	"force-orchestrator/internal/store"
)

// Extractor implements apiextract.ConsumerExtractor for Ruby files.
type Extractor struct{}

// New returns an initialised Extractor.
func New() *Extractor { return &Extractor{} }

// SupportedCallKinds returns the call_kind values produced by this extractor.
func (e *Extractor) SupportedCallKinds() []string {
	return []string{"httparty", "faraday", "net-http", "rest-client"}
}

// Compiled regexes.
var (
	// HTTParty.get('url', ...)
	reHTTParty = regexp.MustCompile(`HTTParty\.(get|post|put|delete|patch)\s*\(\s*['"]([^'"]+)['"]`)

	// Faraday.get('/path')
	reFaraday = regexp.MustCompile(`Faraday\.(get|post|put|delete)\s*\(\s*['"]([^'"]+)['"]`)

	// conn.get('/path') — common Faraday connection variable names
	reConn = regexp.MustCompile(`(?:conn|connection|client|faraday)\.(get|post|put|delete)\s*\(\s*['"]([^'"]+)['"]`)

	// Net::HTTP.get(URI('url')) or Net::HTTP.post(URI('url'))
	reNetHTTP = regexp.MustCompile(`Net::HTTP\.(get|post)\s*\(\s*URI\s*\(\s*['"]([^'"]+)['"]`)

	// RestClient.get('url')
	reRestClient = regexp.MustCompile(`RestClient\.(get|post|put|delete)\s*\(\s*['"]([^'"]+)['"]`)
)

// Extract parses content line-by-line and returns dependency rows.
func (e *Extractor) Extract(repoName, filePath string, content []byte) ([]store.CrossRepoAPIDependency, error) {
	var deps []store.CrossRepoAPIDependency
	lines := strings.Split(string(content), "\n")

	for i, line := range lines {
		lineNo := i + 1

		if m := reHTTParty.FindStringSubmatch(line); m != nil {
			path := stripSchemeHost(m[2])
			if isHTTPPath(path) {
				identifier := strings.ToUpper(m[1]) + " " + store.NormalizeAPIPath(path)
				deps = append(deps, makeDep(repoName, filePath, lineNo, "httparty", identifier, 1.0))
			}
			continue
		}

		if m := reFaraday.FindStringSubmatch(line); m != nil {
			path := stripSchemeHost(m[2])
			if isHTTPPath(path) {
				identifier := strings.ToUpper(m[1]) + " " + store.NormalizeAPIPath(path)
				deps = append(deps, makeDep(repoName, filePath, lineNo, "faraday", identifier, 1.0))
			}
			continue
		}

		if m := reConn.FindStringSubmatch(line); m != nil {
			path := stripSchemeHost(m[2])
			if isHTTPPath(path) {
				identifier := strings.ToUpper(m[1]) + " " + store.NormalizeAPIPath(path)
				deps = append(deps, makeDep(repoName, filePath, lineNo, "faraday", identifier, 1.0))
			}
			continue
		}

		if m := reNetHTTP.FindStringSubmatch(line); m != nil {
			path := stripSchemeHost(m[2])
			if isHTTPPath(path) {
				identifier := strings.ToUpper(m[1]) + " " + store.NormalizeAPIPath(path)
				deps = append(deps, makeDep(repoName, filePath, lineNo, "net-http", identifier, 1.0))
			}
			continue
		}

		if m := reRestClient.FindStringSubmatch(line); m != nil {
			path := stripSchemeHost(m[2])
			if isHTTPPath(path) {
				identifier := strings.ToUpper(m[1]) + " " + store.NormalizeAPIPath(path)
				deps = append(deps, makeDep(repoName, filePath, lineNo, "rest-client", identifier, 1.0))
			}
			continue
		}
	}

	return deps, nil
}

// stripSchemeHost removes https?://host from a URL, keeping only the path.
// If the string is already a path (starts with /), it is returned unchanged.
func stripSchemeHost(url string) string {
	for _, prefix := range []string{"https://", "http://"} {
		if strings.HasPrefix(url, prefix) {
			rest := url[len(prefix):]
			slash := strings.Index(rest, "/")
			if slash < 0 {
				// URL is just the host with no path — treat as root.
				return "/"
			}
			return rest[slash:]
		}
	}
	return url
}

// isHTTPPath returns true when s starts with / (absolute path) or http(s)://.
func isHTTPPath(s string) bool {
	return strings.HasPrefix(s, "/") ||
		strings.HasPrefix(s, "http://") ||
		strings.HasPrefix(s, "https://")
}

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
