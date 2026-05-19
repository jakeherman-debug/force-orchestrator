// Package nestjs implements a ProviderExtractor that parses NestJS HTTP
// route decorators from TypeScript source files.
//
// Matched decorators:
//
//	Class level:  @Controller('base/path')  or  @Controller()  (empty → "")
//	Method level: @Get('path') | @Post | @Put | @Patch | @Delete
//
// The full route is: NormalizeAPIPath(base + "/" + methodPath)
//
// Known misses (accuracy target ≥ 85%):
//   - Dynamic module factories that compute paths at runtime.
//   - @All() (not in NestJS core; use explicit decorators).
package nestjs

import (
	"bufio"
	"bytes"
	"fmt"
	"regexp"
	"strings"

	"force-orchestrator/internal/store"
)

// reController matches:
//
//	@Controller('path')   @Controller("path")   @Controller(`path`)
//	@Controller()
//
// Capture group [1]: the path string (may be empty when no argument given).
var reController = regexp.MustCompile(
	`@Controller\s*\(\s*(?:['` + "`" + `"]([^'` + "`" + `"]*)['` + "`" + `"])?\s*\)`,
)

// reMethodDecorator matches NestJS HTTP method decorators:
//
//	@Get('path')  @Post("path")  @Put(`path`)  @Patch(…)  @Delete(…)
//	@Get()  (no path → "")
//
// Capture groups:
//
//	[1] HTTP method (Get|Post|Put|Patch|Delete)
//	[2] path string (may be empty)
var reMethodDecorator = regexp.MustCompile(
	`@(Get|Post|Put|Patch|Delete)\s*\(\s*(?:['` + "`" + `"]([^'` + "`" + `"]*)['` + "`" + `"])?\s*\)`,
)

// Extractor implements apiextract.ProviderExtractor for NestJS.
type Extractor struct{}

// Kind returns the api_kind value stored in CrossRepoAPIs.
func (Extractor) Kind() string { return "http_route" }

// ExtractorName returns the extractor column value stored in CrossRepoAPIs.
func (Extractor) ExtractorName() string { return "nestjs-decorator" }

// Extract scans content for NestJS @Controller + method-level decorators and
// combines them into full route identifiers.
func (e Extractor) Extract(repoName, filePath string, content []byte) ([]store.CrossRepoAPI, error) {
	var out []store.CrossRepoAPI

	// We do a two-pass scan:
	//   Pass 1: collect all @Controller() positions so we know which base path
	//           applies to which line number.
	//   Pass 2: for each method decorator line, find the enclosing controller
	//           base path (the last @Controller seen before that line in the file).
	//
	// This handles a single file containing multiple controllers, which is rare
	// but valid TypeScript.

	type controllerEntry struct {
		line     int
		basePath string
	}

	var controllers []controllerEntry
	lines := splitLines(content)

	for i, line := range lines {
		m := reController.FindStringSubmatch(line)
		if m != nil {
			base := m[1] // empty string when @Controller() has no argument
			controllers = append(controllers, controllerEntry{
				line:     i + 1,
				basePath: base,
			})
		}
	}

	// baseBefore returns the base path for the controller whose @Controller
	// decorator appears at or before lineNum.
	baseBefore := func(lineNum int) string {
		best := ""
		for _, c := range controllers {
			if c.line <= lineNum {
				best = c.basePath
			}
		}
		return best
	}

	for i, line := range lines {
		lineNum := i + 1
		m := reMethodDecorator.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		nestMethod := m[1]                    // e.g. "Get"
		httpMethod := strings.ToUpper(nestMethod) // → "GET"
		methodPath := m[2]                    // may be empty

		base := baseBefore(lineNum)
		fullPath := joinPaths(base, methodPath)
		fullPath = store.NormalizeAPIPath(fullPath)

		out = append(out, store.CrossRepoAPI{
			RepoName:      repoName,
			APIKind:       e.Kind(),
			APIIdentifier: fmt.Sprintf("%s %s", httpMethod, fullPath),
			SourceFile:    filePath,
			SourceLine:    lineNum,
			Extractor:     e.ExtractorName(),
		})
	}

	return out, nil
}

// splitLines splits a byte slice into a slice of trimmed line strings,
// preserving empty lines so that line numbers remain accurate.
func splitLines(content []byte) []string {
	var lines []string
	scanner := bufio.NewScanner(bytes.NewReader(content))
	for scanner.Scan() {
		lines = append(lines, scanner.Text())
	}
	return lines
}

// joinPaths concatenates a base controller path and a method-level path,
// ensuring exactly one "/" separator. Both may be empty.
func joinPaths(base, method string) string {
	base = strings.TrimRight(base, "/")
	method = strings.TrimLeft(method, "/")

	if base == "" && method == "" {
		return "/"
	}
	if base == "" {
		return "/" + method
	}
	if method == "" {
		return "/" + base
	}
	return "/" + base + "/" + method
}
