// Package javaclient extracts HTTP client call sites from Java and Kotlin
// source files. It detects RestTemplate, OkHttp, and Retrofit invocations and
// emits CrossRepoAPIDependency rows with ProviderAPIID = 0 for P6 to resolve.
package javaclient

import (
	"regexp"
	"strings"

	"force-orchestrator/internal/store"
)

// Extractor implements apiextract.ConsumerExtractor for Java/Kotlin files.
type Extractor struct{}

// New returns an initialised Extractor.
func New() *Extractor { return &Extractor{} }

// SupportedCallKinds returns the call_kind values produced by this extractor.
func (e *Extractor) SupportedCallKinds() []string {
	return []string{"rest-template", "okhttp", "retrofit"}
}

// Compiled regexes.
var (
	// restTemplate.getForObject("url", ...)
	// restTemplate.postForObject("url", ...) etc.
	reRestTemplate = regexp.MustCompile(`restTemplate\.(getForObject|postForObject|exchange|getForEntity)\s*\(\s*"([^"]+)"`)

	// new Request.Builder().url("url")
	reOkHTTP = regexp.MustCompile(`\.url\s*\(\s*"([^"]+)"\s*\)`)

	// @GET("/path") @POST("/path") @PUT("/path") @DELETE("/path") annotations
	reRetrofit = regexp.MustCompile(`@(GET|POST|PUT|DELETE|PATCH)\s*\(\s*"([^"]+)"\s*\)`)
)

// Extract parses content line-by-line and returns dependency rows.
func (e *Extractor) Extract(repoName, filePath string, content []byte) ([]store.CrossRepoAPIDependency, error) {
	var deps []store.CrossRepoAPIDependency
	lines := strings.Split(string(content), "\n")

	for i, line := range lines {
		lineNo := i + 1

		if m := reRestTemplate.FindStringSubmatch(line); m != nil {
			javaMethod := m[1]
			rawURL := m[2]
			path := stripSchemeHost(rawURL)
			if isHTTPPath(path) {
				httpMethod := javaMethodToHTTPMethod(javaMethod)
				identifier := httpMethod + " " + store.NormalizeAPIPath(path)
				deps = append(deps, makeDep(repoName, filePath, lineNo, "rest-template", identifier, 1.0))
			}
			continue
		}

		if m := reOkHTTP.FindStringSubmatch(line); m != nil {
			rawURL := m[1]
			path := stripSchemeHost(rawURL)
			if isHTTPPath(path) {
				identifier := store.NormalizeAPIPath(path)
				deps = append(deps, makeDep(repoName, filePath, lineNo, "okhttp", identifier, 1.0))
			}
			continue
		}

		if m := reRetrofit.FindStringSubmatch(line); m != nil {
			method := m[1]
			path := m[2]
			if isHTTPPath(path) {
				identifier := method + " " + store.NormalizeAPIPath(path)
				deps = append(deps, makeDep(repoName, filePath, lineNo, "retrofit", identifier, 1.0))
			}
			continue
		}
	}

	return deps, nil
}

// javaMethodToHTTPMethod maps RestTemplate method names to HTTP verbs.
func javaMethodToHTTPMethod(method string) string {
	switch method {
	case "getForObject", "getForEntity":
		return "GET"
	case "postForObject":
		return "POST"
	case "exchange":
		return "GET" // conservative default; exchange takes an HttpMethod arg
	default:
		return "GET"
	}
}

// stripSchemeHost removes https?://host from a URL, keeping only the path.
func stripSchemeHost(url string) string {
	for _, prefix := range []string{"https://", "http://"} {
		if strings.HasPrefix(url, prefix) {
			rest := url[len(prefix):]
			slash := strings.Index(rest, "/")
			if slash < 0 {
				return "/"
			}
			return rest[slash:]
		}
	}
	return url
}

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
