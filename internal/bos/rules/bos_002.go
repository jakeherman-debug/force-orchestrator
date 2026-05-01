package rules

import (
	"go/ast"
	"go/types"
	"strings"

	"force-orchestrator/internal/bos"
)

// BOS-002 — UnmarkedStoreErrorDiscard
//
// CLAUDE.md anchor: "No silent failures" (Fix #8a).
//
// Flags new `_ = store.Foo(...)` patterns that drop a store-call's
// error return without an immediately-preceding `// TODO(Fix #8b):`
// marker comment. Fix #8b is the historical "this discard is
// intentional, error already routed elsewhere" annotation; new
// discards without the marker are the silent-failure pattern.
//
// Anti-cheat: severity=advise at launch.
type bos002 struct{}

func (bos002) ID() string             { return "BOS-002" }
func (bos002) CLAUDEMDAnchor() string { return "No silent failures" }
func (bos002) Severity() bos.Severity { return bos.SeverityAdvise }

func (bos002) Check(file *ast.File, path string, _ *types.Info) []bos.Finding {
	var out []bos.Finding

	// Build a map of line → comment text so we can check the
	// "immediately preceding" requirement without an O(n*m) scan.
	commentByLine := map[int]string{}
	for _, cg := range file.Comments {
		for _, c := range cg.List {
			line := positionLine(c)
			commentByLine[line] = strings.TrimSpace(c.Text)
		}
	}

	ast.Inspect(file, func(n ast.Node) bool {
		assign, ok := n.(*ast.AssignStmt)
		if !ok {
			return true
		}
		// Shape: `_ = store.X(...)` — exactly one LHS that's the
		// blank identifier, exactly one RHS that's a call.
		if len(assign.Lhs) != 1 || len(assign.Rhs) != 1 {
			return true
		}
		lhs, ok := assign.Lhs[0].(*ast.Ident)
		if !ok || lhs.Name != "_" {
			return true
		}
		call, ok := assign.Rhs[0].(*ast.CallExpr)
		if !ok {
			return true
		}
		if !isStoreCall(call) {
			return true
		}
		assignLine := positionLine(assign)
		// Marker MUST be on the line immediately above (assignLine-1)
		// or any line in [assignLine-3, assignLine-1] — cluster of
		// comment lines preceding the discard counts as preceding.
		hasMarker := false
		for delta := 1; delta <= 3; delta++ {
			if c, ok := commentByLine[assignLine-delta]; ok {
				if strings.Contains(c, "TODO(Fix #8b)") {
					hasMarker = true
					break
				}
			}
		}
		if hasMarker {
			return true
		}
		out = append(out, bos.Finding{
			RuleID:   "BOS-002",
			Severity: bos.SeverityAdvise,
			Path:     path,
			Line:     assignLine,
			Message:  "discarded store error without `// TODO(Fix #8b):` marker — annotate or route the error per CLAUDE.md No silent failures",
		})
		return true
	})
	return out
}

// isStoreCall returns true when the call's receiver evaluates to the
// `store` package qualifier (or `s.store.Foo()` pattern in tests).
// Matches `store.Foo(...)` lexically.
func isStoreCall(call *ast.CallExpr) bool {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	pkg, ok := sel.X.(*ast.Ident)
	if !ok {
		return false
	}
	return pkg.Name == "store"
}

func init() { bos.Register(bos002{}) }
