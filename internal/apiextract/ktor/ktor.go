// Package ktor provides a ProviderExtractor for the Ktor routing DSL in Kotlin
// source files. It uses a line-by-line regex scan that tracks curly-brace
// nesting depth and accumulated path prefixes to reconstruct full route paths.
//
// Known misses:
//   - Deeply nested scopes with paths assembled from runtime variables
//   - Routes registered via programmatic application call objects
//   - WebSockets and other non-HTTP verb blocks
package ktor

import (
	"fmt"
	"regexp"
	"strings"

	"force-orchestrator/internal/store"
)

const (
	apiKind   = "http_route"
	extractor = "ktor-routing"
)

// HTTP verb functions recognised in the Ktor DSL.
var httpVerbs = map[string]bool{
	"get":    true,
	"post":   true,
	"put":    true,
	"delete": true,
	"patch":  true,
	"head":   true,
	"options": true,
}

var (
	// reRoutePath matches route("/path") { or route("/path") { (with optional spaces)
	reRoutePath = regexp.MustCompile(
		`\broute\s*\(\s*"([^"]+)"\s*\)\s*\{`,
	)

	// reVerbWithPath matches get("/path") { — groups: 1=verb, 2=path
	reVerbWithPath = regexp.MustCompile(
		`\b(get|post|put|delete|patch|head|options)\s*\(\s*"([^"]+)"\s*\)\s*\{`,
	)

	// reVerbNoPath matches get { (no path argument — implies root of current prefix)
	reVerbNoPath = regexp.MustCompile(
		`\b(get|post|put|delete|patch|head|options)\s*\{\s*$`,
	)
)

// frame holds a path segment and the brace depth at which it was pushed.
type frame struct {
	segment string
	depth   int // brace depth at the opening {
}

// Extractor implements apiextract.ProviderExtractor for the Ktor routing DSL.
type Extractor struct{}

// New returns a Ktor Extractor.
func New() *Extractor { return &Extractor{} }

func (*Extractor) Kind() string          { return apiKind }
func (*Extractor) ExtractorName() string { return extractor }

// Extract parses a Kotlin source file for Ktor routing DSL declarations and
// returns one CrossRepoAPI row per discovered HTTP verb handler.
func (e *Extractor) Extract(repoName, filePath string, content []byte) ([]store.CrossRepoAPI, error) {
	if !strings.HasSuffix(strings.ToLower(filePath), ".kt") {
		return nil, nil
	}

	lines := splitLines(content)

	// Stack-based path accumulator. Each element is the path segment pushed
	// when we enter a route{} or routing{} block. We track the brace depth
	// separately to detect when we leave each scope.
	var stack []frame
	braceDepth := 0

	// inRoutingScope is true once we've seen a routing { or Application.routing
	// block. We ignore verb calls outside a routing scope.
	inRoutingScope := false

	var out []store.CrossRepoAPI

	for lineIdx, line := range lines {
		lineNum := lineIdx + 1
		trimmed := strings.TrimSpace(line)

		// Count brace changes on this line BEFORE processing patterns so we
		// can pop the stack correctly.
		opens := strings.Count(line, "{")
		closes := strings.Count(line, "}")

		// --- Pattern matching (before adjusting depth) ---

		// Detect entry into a routing { } scope.
		if !inRoutingScope {
			if isRoutingEntry(trimmed) {
				inRoutingScope = true
				// The opening { for routing itself is counted below.
			}
		}

		if inRoutingScope {
			// route("/path") {
			if m := reRoutePath.FindStringSubmatch(trimmed); m != nil {
				seg := m[1]
				// depth after processing this line's opens
				// We push at (braceDepth + opens - closes) but the open for
				// this block is included in `opens`, so we compute after.
				newDepth := braceDepth + opens - closes
				stack = append(stack, frame{segment: seg, depth: newDepth})
			}

			// get("/path") { / post("/path") {
			if m := reVerbWithPath.FindStringSubmatch(trimmed); m != nil {
				verb := strings.ToUpper(m[1])
				seg := m[2]
				prefix := currentPrefix(stack)
				full := store.NormalizeAPIPath(joinPaths(prefix, seg))
				out = append(out, store.CrossRepoAPI{
					RepoName:      repoName,
					APIKind:       apiKind,
					APIIdentifier: fmt.Sprintf("%s %s", verb, full),
					SourceFile:    filePath,
					SourceLine:    lineNum,
					Extractor:     extractor,
				})
			} else if m := reVerbNoPath.FindStringSubmatch(trimmed); m != nil {
				// get { — the path is whatever the current prefix is.
				verb := strings.ToUpper(m[1])
				prefix := currentPrefix(stack)
				if prefix == "" {
					prefix = "/"
				}
				full := store.NormalizeAPIPath(prefix)
				out = append(out, store.CrossRepoAPI{
					RepoName:      repoName,
					APIKind:       apiKind,
					APIIdentifier: fmt.Sprintf("%s %s", verb, full),
					SourceFile:    filePath,
					SourceLine:    lineNum,
					Extractor:     extractor,
				})
			}
		}

		// --- Update brace depth ---
		braceDepth += opens - closes

		// Pop any frames whose depth is now greater than braceDepth (we've
		// closed their scope).
		for len(stack) > 0 && stack[len(stack)-1].depth > braceDepth {
			stack = stack[:len(stack)-1]
		}

		// If we've closed back to depth 0 we've exited the routing scope.
		if braceDepth <= 0 {
			inRoutingScope = false
			braceDepth = 0
			stack = stack[:0]
		}
	}

	return out, nil
}

// isRoutingEntry returns true when the line looks like a Ktor routing block
// entry: `routing {`, `install(Routing) {`, or `fun Application.module`.
func isRoutingEntry(line string) bool {
	return strings.Contains(line, "routing {") ||
		strings.Contains(line, "routing{") ||
		strings.HasPrefix(line, "fun Application.")
}

// currentPrefix returns the accumulated path prefix from the current stack.
func currentPrefix(stack []frame) string {
	var parts []string
	for _, f := range stack {
		parts = append(parts, f.segment)
	}
	return joinPathParts(parts)
}

// joinPaths joins a prefix and a segment, collapsing duplicate slashes.
func joinPaths(prefix, seg string) string {
	if prefix == "" {
		return seg
	}
	prefix = strings.TrimRight(prefix, "/")
	if !strings.HasPrefix(seg, "/") {
		return prefix + "/" + seg
	}
	return prefix + seg
}

// joinPathParts joins multiple path segments into a single path.
func joinPathParts(parts []string) string {
	if len(parts) == 0 {
		return ""
	}
	result := ""
	for _, p := range parts {
		result = joinPaths(result, p)
	}
	return result
}

// splitLines splits content into a slice of lines, stripping newlines.
func splitLines(content []byte) []string {
	raw := string(content)
	raw = strings.ReplaceAll(raw, "\r\n", "\n")
	raw = strings.ReplaceAll(raw, "\r", "\n")
	lines := strings.Split(raw, "\n")
	// Remove trailing empty line if content ended with newline.
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}
