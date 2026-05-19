// Package openapi provides a ProviderExtractor for OpenAPI/Swagger YAML and
// JSON specification files. It emits one openapi_op row per HTTP operation.
// Accuracy target: ≥ 95%.
package openapi

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"force-orchestrator/internal/store"

	"gopkg.in/yaml.v3"
)

const (
	apiKind       = "openapi_op"
	extractorName = "openapi-yaml"
)

// Extractor implements apiextract.ProviderExtractor for OpenAPI specs.
type Extractor struct{}

// Kind returns "openapi_op".
func (e *Extractor) Kind() string { return apiKind }

// ExtractorName returns "openapi-yaml".
func (e *Extractor) ExtractorName() string { return extractorName }

// httpMethods lists the HTTP verbs recognised in OpenAPI path items.
var httpMethods = []string{"get", "put", "post", "delete", "options", "head", "patch", "trace"}

// Extract parses an OpenAPI YAML or JSON file and returns CrossRepoAPI rows.
func (e *Extractor) Extract(repoName, filePath string, content []byte) ([]store.CrossRepoAPI, error) {
	trimmed := strings.TrimSpace(string(content))

	// Determine format: JSON starts with '{'.
	if strings.HasPrefix(trimmed, "{") {
		return extractJSON(repoName, filePath, content)
	}
	return extractYAML(repoName, filePath, content)
}

// spec is the minimal structure we care about — just paths.
type spec struct {
	Paths map[string]map[string]interface{} `yaml:"paths" json:"paths"`
}

// extractYAML uses gopkg.in/yaml.v3 to parse the spec, then uses a line-based
// scan to assign accurate source_line numbers to each HTTP method entry.
func extractYAML(repoName, filePath string, content []byte) ([]store.CrossRepoAPI, error) {
	var s spec
	if err := yaml.Unmarshal(content, &s); err != nil {
		return nil, fmt.Errorf("openapi extractor: yaml parse %s: %w", filePath, err)
	}
	if len(s.Paths) == 0 {
		return nil, nil
	}

	// Build a line-number index: for each (path, method) pair determine the
	// line number of the method key inside the YAML file.
	lineIndex := buildYAMLLineIndex(content, s.Paths)

	var out []store.CrossRepoAPI
	// Sort paths for deterministic output.
	paths := make([]string, 0, len(s.Paths))
	for p := range s.Paths {
		paths = append(paths, p)
	}
	sort.Strings(paths)

	for _, rawPath := range paths {
		ops := s.Paths[rawPath]
		normalPath := store.NormalizeAPIPath(rawPath)
		// Sort methods for deterministic output.
		methods := make([]string, 0, len(ops))
		for m := range ops {
			methods = append(methods, m)
		}
		sort.Strings(methods)

		for _, method := range methods {
			if !isHTTPMethod(method) {
				continue
			}
			identifier := fmt.Sprintf("%s %s", strings.ToUpper(method), normalPath)
			lineNo := lineIndex[rawPath+"|"+method]
			out = append(out, store.CrossRepoAPI{
				RepoName:      repoName,
				APIKind:       apiKind,
				APIIdentifier: identifier,
				SourceFile:    filePath,
				SourceLine:    lineNo,
				Extractor:     extractorName,
				LastScannedAt: store.NowSQLite(),
			})
		}
	}
	return out, nil
}

// extractJSON parses an OpenAPI JSON file.
func extractJSON(repoName, filePath string, content []byte) ([]store.CrossRepoAPI, error) {
	var s spec
	if err := json.Unmarshal(content, &s); err != nil {
		return nil, fmt.Errorf("openapi extractor: json parse %s: %w", filePath, err)
	}
	if len(s.Paths) == 0 {
		return nil, nil
	}

	var out []store.CrossRepoAPI
	paths := make([]string, 0, len(s.Paths))
	for p := range s.Paths {
		paths = append(paths, p)
	}
	sort.Strings(paths)

	for _, rawPath := range paths {
		ops := s.Paths[rawPath]
		normalPath := store.NormalizeAPIPath(rawPath)

		methods := make([]string, 0, len(ops))
		for m := range ops {
			methods = append(methods, m)
		}
		sort.Strings(methods)

		for _, method := range methods {
			if !isHTTPMethod(method) {
				continue
			}
			identifier := fmt.Sprintf("%s %s", strings.ToUpper(method), normalPath)
			out = append(out, store.CrossRepoAPI{
				RepoName:      repoName,
				APIKind:       apiKind,
				APIIdentifier: identifier,
				SourceFile:    filePath,
				SourceLine:    0, // line numbers not supported for JSON
				Extractor:     extractorName,
				LastScannedAt: store.NowSQLite(),
			})
		}
	}
	return out, nil
}

// isHTTPMethod returns true if the string is a recognised HTTP method key.
func isHTTPMethod(s string) bool {
	s = strings.ToLower(s)
	for _, m := range httpMethods {
		if s == m {
			return true
		}
	}
	return false
}

// buildYAMLLineIndex scans the raw YAML bytes line-by-line to find the 1-based
// line number of each HTTP method key under a given path key. Returns a map
// keyed by "rawPath|method" (both as they appear in the YAML).
func buildYAMLLineIndex(content []byte, paths map[string]map[string]interface{}) map[string]int {
	index := make(map[string]int)

	scanner := bufio.NewScanner(bytes.NewReader(content))
	lineNum := 0
	var currentPath string

	for scanner.Scan() {
		lineNum++
		line := scanner.Text()
		trimmed := strings.TrimSpace(line)

		// Path keys look like:  "  /users/{id}:"  — starts with "/" after trim.
		if strings.HasPrefix(trimmed, "/") && strings.HasSuffix(trimmed, ":") {
			pathKey := strings.TrimSuffix(trimmed, ":")
			if _, ok := paths[pathKey]; ok {
				currentPath = pathKey
				continue
			}
		}

		if currentPath == "" {
			continue
		}

		// Method keys look like:  "    get:" — a single word ending with ":".
		if strings.HasSuffix(trimmed, ":") {
			candidate := strings.TrimSuffix(trimmed, ":")
			if isHTTPMethod(candidate) {
				key := currentPath + "|" + candidate
				if _, seen := index[key]; !seen {
					index[key] = lineNum
				}
			}
		}
	}
	return index
}
