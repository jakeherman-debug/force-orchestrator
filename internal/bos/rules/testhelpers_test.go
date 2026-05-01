package rules

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"

	"force-orchestrator/internal/bos"
)

// parse is a one-line helper that turns a Go source string into an
// *ast.File. The caller's path argument flows into Finding.Path so red
// tests can assert on it.
func parse(t *testing.T, path, src string) *ast.File {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, src, parser.ParseComments)
	if err != nil {
		t.Fatalf("parse %s: %v\nsrc:\n%s", path, err, src)
	}
	// The rules under test position-aware. ast.NodePosition needs the
	// fileset to resolve to a line; we stash the fileset on a global
	// fallback so the rules can resolve positions without each test
	// passing it in. The loadFset/setFset hooks in fileset.go are the
	// indirection.
	setFset(fset)
	return f
}

// runRule executes a single rule against a parsed file and returns the
// findings.
func runRule(t *testing.T, r bos.Rule, path, src string) []bos.Finding {
	t.Helper()
	f := parse(t, path, src)
	return r.Check(f, path, nil)
}

// assertHasFinding asserts at least one finding matched the rule and
// (optionally) contains the expected substring in its message.
func assertHasFinding(t *testing.T, findings []bos.Finding, ruleID, msgContains string) {
	t.Helper()
	for _, f := range findings {
		if f.RuleID == ruleID {
			if msgContains == "" || contains(f.Message, msgContains) {
				return
			}
		}
	}
	t.Fatalf("expected finding for %q (msg contains %q); got %v", ruleID, msgContains, findings)
}

// assertNoFindings asserts the rule produced zero findings.
func assertNoFindings(t *testing.T, findings []bos.Finding) {
	t.Helper()
	if len(findings) != 0 {
		t.Fatalf("expected zero findings; got %d: %v", len(findings), findings)
	}
}

func contains(haystack, needle string) bool {
	return haystack != "" && needle != "" && (haystack == needle || hasSubstring(haystack, needle))
}

func hasSubstring(s, substr string) bool {
	if len(substr) > len(s) {
		return false
	}
	for i := 0; i+len(substr) <= len(s); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
