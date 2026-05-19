package store

import (
	"regexp"
	"strings"
)

// rePathParam matches common path-parameter forms and collapses them to :word.
//
//	{id}   → :id
//	${id}  → :id
//	<id>   → :id
//	:id    → :id  (already canonical — not matched, left unchanged)
var (
	reCurly  = regexp.MustCompile(`\{(\w+)\}`)
	reDollar = regexp.MustCompile(`\$\{(\w+)\}`)
	reAngle  = regexp.MustCompile(`<(\w+)>`)
)

// NormalizeAPIPath collapses common path-parameter forms to :param canonical
// form and trims any trailing slash. The HTTP method prefix (e.g. "GET ") is
// preserved as-is so callers can store "GET /api/v1/users/:id" directly.
//
// Transformations applied (in order):
//
//	${word} → :word   (template-literal / Spring-path style)
//	{word}  → :word   (Rails/OpenAPI/FastAPI style)
//	<word>  → :word   (angle-bracket style used by some Python frameworks)
//	trailing slash stripped
//
// For non-HTTP identifiers (e.g. gRPC "service.UserService/GetUser" or proto
// event "events.UserCreated") the function is a no-op — none of the patterns
// match those forms, and no trailing slash is present.
func NormalizeAPIPath(path string) string {
	// Apply substitutions. Dollar-brace must come before curly to avoid
	// double-replacing "${id}" as "{id}" first.
	path = reDollar.ReplaceAllString(path, ":$1")
	path = reCurly.ReplaceAllString(path, ":$1")
	path = reAngle.ReplaceAllString(path, ":$1")

	// Trim trailing slash (keep leading slash and method prefix intact).
	path = strings.TrimRight(path, "/")

	return path
}
