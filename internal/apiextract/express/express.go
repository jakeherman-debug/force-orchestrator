// Package express implements a ProviderExtractor that parses Express.js route
// registrations from JavaScript and TypeScript source files using regular
// expressions.
//
// Matched form:
//
//	(app|router).(get|post|put|patch|delete|all)\s*(\s*['"`]<path>['"`]
//
// Known misses (accuracy target ≥ 70%):
//   - Routes registered via middleware factories or runtime variables.
//   - Template-literal paths containing runtime-evaluated ${expr} (skipped).
package express

import (
	"bufio"
	"bytes"
	"fmt"
	"regexp"
	"strings"

	"force-orchestrator/internal/store"
)

// reRoute matches Express route registrations in the form:
//
//	(app|router).(get|post|put|patch|delete|all)(\s*'path'
//	(app|router).(get|post|put|patch|delete|all)(\s*"path"
//	(app|router).(get|post|put|patch|delete|all)(\s*`path`
//
// Capture groups:
//
//	[1] method (get|post|put|patch|delete|all)
//	[2] path string
var reRoute = regexp.MustCompile(
	`(?:app|router)\.(get|post|put|patch|delete|all)\s*\(\s*['` + "`" + `"]([^'` + "`" + `"]+)['` + "`" + `"]`,
)

// reDynamicTemplate matches a template-literal interpolation like ${expr}.
// If a path contains this, we cannot resolve it statically.
var reDynamicTemplate = regexp.MustCompile(`\$\{[^}]+\}`)

// allMethods is the set of HTTP methods emitted for an app.all() registration.
var allMethods = []string{"GET", "POST", "PUT", "PATCH", "DELETE"}

// Extractor implements apiextract.ProviderExtractor for Express.js.
type Extractor struct{}

// Kind returns the api_kind value stored in CrossRepoAPIs.
func (Extractor) Kind() string { return "http_route" }

// ExtractorName returns the extractor column value stored in CrossRepoAPIs.
func (Extractor) ExtractorName() string { return "express-app" }

// Extract scans content line-by-line for Express route registrations and
// returns a CrossRepoAPI row for every discovered route. Template-literal
// paths with runtime interpolations are silently skipped (known miss).
func (e Extractor) Extract(repoName, filePath string, content []byte) ([]store.CrossRepoAPI, error) {
	var out []store.CrossRepoAPI

	scanner := bufio.NewScanner(bytes.NewReader(content))
	lineNum := 0
	for scanner.Scan() {
		lineNum++
		line := scanner.Text()

		matches := reRoute.FindAllStringSubmatch(line, -1)
		for _, m := range matches {
			method := strings.ToUpper(m[1]) // e.g. "GET"
			rawPath := m[2]

			// Skip paths that contain unresolvable runtime expressions.
			if reDynamicTemplate.MatchString(rawPath) {
				continue
			}

			normalPath := store.NormalizeAPIPath(rawPath)

			if method == "ALL" {
				// Expand app.all() into one row per HTTP method.
				for _, meth := range allMethods {
					out = append(out, store.CrossRepoAPI{
						RepoName:      repoName,
						APIKind:       e.Kind(),
						APIIdentifier: fmt.Sprintf("%s %s", meth, normalPath),
						SourceFile:    filePath,
						SourceLine:    lineNum,
						Extractor:     e.ExtractorName(),
					})
				}
			} else {
				out = append(out, store.CrossRepoAPI{
					RepoName:      repoName,
					APIKind:       e.Kind(),
					APIIdentifier: fmt.Sprintf("%s %s", method, normalPath),
					SourceFile:    filePath,
					SourceLine:    lineNum,
					Extractor:     e.ExtractorName(),
				})
			}
		}
	}
	return out, scanner.Err()
}
