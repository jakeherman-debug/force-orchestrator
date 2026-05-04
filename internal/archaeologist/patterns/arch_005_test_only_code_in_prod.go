// Pattern ARCH-005 — leftover test-only code in production paths.
//
// Detects callers of testing-package symbols (and similar test-only
// identifiers) from non-_test.go files. The classic regression: a
// `testing.T` parameter or a panic("UNREACHABLE: test-only path")
// sneaks into production code via an incomplete refactor and the
// `_test.go` boundary stops protecting it.
//
// Language-aware (anti-cheat #2): Go-only — only opens .go files and
// only flags non-_test.go files. A Rust equivalent would land as a
// sibling pattern.

package patterns

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"strings"

	"force-orchestrator/internal/archaeologist"
)

// arch005TestOnlyImports flags any import of these packages from a
// non-_test.go file. The `testing` package is the canonical signal;
// `testing/quick`, `testing/iotest`, and friends inherit.
var arch005TestOnlyImports = map[string]struct{}{
	`"testing"`:                {},
	`"testing/quick"`:          {},
	`"testing/iotest"`:         {},
	`"testing/fstest"`:         {},
	`"testing/synctest"`:       {},
	`"github.com/stretchr/testify/assert"`:  {},
	`"github.com/stretchr/testify/require"`: {},
}

type arch005 struct{}

// NewARCH005 returns the ARCH-005 pattern.
func NewARCH005() archaeologist.Pattern { return arch005{} }

func (arch005) ID() string             { return "ARCH-005" }
func (arch005) MinHitsForFeature() int { return 3 }

func (p arch005) Scan(repo *archaeologist.Repo) []archaeologist.Hit {
	if repo == nil || repo.LocalPath == "" {
		return nil
	}
	var hits []archaeologist.Hit
	_ = walkRepoFiles(repo.LocalPath, []string{".go"}, func(absPath, relPath string) error {
		if strings.HasSuffix(relPath, "_test.go") {
			return nil // _test.go files are allowed to import testing
		}
		// `testdata/` directories carry test fixtures; skip.
		if strings.Contains(relPath, "/testdata/") || strings.HasPrefix(relPath, "testdata/") {
			return nil
		}
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, absPath, nil, parser.ImportsOnly|parser.SkipObjectResolution)
		if err != nil {
			return nil
		}
		for _, imp := range f.Imports {
			if imp.Path == nil {
				continue
			}
			if _, flagged := arch005TestOnlyImports[imp.Path.Value]; !flagged {
				continue
			}
			pos := fset.Position(imp.Pos())
			detail, _ := json.Marshal(map[string]any{
				"import_path": strings.Trim(imp.Path.Value, `"`),
				"language":    "go",
			})
			hits = append(hits, archaeologist.Hit{
				FilePath:   relPath,
				LineNumber: pos.Line,
				DetailJSON: string(detail),
			})
		}
		// Re-parse for full-AST inspection of testing.T parameter
		// usage (catches the case where a non-_test file imports
		// testing as `_` or via a subpackage that ARCH-005 doesn't
		// list — the import-level scan above is the primary signal,
		// the AST parameter scan is the belt.
		fAST, fErr := parser.ParseFile(fset, absPath, nil, parser.SkipObjectResolution)
		if fErr != nil {
			return nil
		}
		ast.Inspect(fAST, func(n ast.Node) bool {
			fn, ok := n.(*ast.FuncDecl)
			if !ok || fn.Type.Params == nil {
				return true
			}
			for _, field := range fn.Type.Params.List {
				if !arch005ParamLooksTestOnly(field.Type) {
					continue
				}
				pos := fset.Position(field.Pos())
				detail, _ := json.Marshal(map[string]any{
					"function_name": fn.Name.Name,
					"param_kind":    "testing.T or testing.B parameter in non-_test.go",
					"language":      "go",
				})
				hits = append(hits, archaeologist.Hit{
					FilePath:   relPath,
					LineNumber: pos.Line,
					DetailJSON: string(detail),
				})
			}
			return true
		})
		return nil
	})
	return hits
}

// arch005ParamLooksTestOnly returns true for a *testing.T / *testing.B /
// *testing.M parameter type.
func arch005ParamLooksTestOnly(e ast.Expr) bool {
	star, ok := e.(*ast.StarExpr)
	if !ok {
		return false
	}
	sel, ok := star.X.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	pkg, ok := sel.X.(*ast.Ident)
	if !ok || pkg.Name != "testing" {
		return false
	}
	switch sel.Sel.Name {
	case "T", "B", "M", "TB", "F":
		return true
	}
	return false
}

func init() { Register(NewARCH005()) }
