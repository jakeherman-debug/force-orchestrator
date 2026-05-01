package rules

import (
	"go/ast"
	"go/types"
	"strings"

	"force-orchestrator/internal/bos"
)

// BOS-003 — MultiWriteWithoutTx
//
// CLAUDE.md anchor: AUDIT-069 (multi-write atomicity). Flags new
// functions that perform >= 2 mutating store calls
// (Insert/Update/Delete/Set/Mark/Add/Remove/...) without a
// db.Begin() / tx.Commit() / tx.Rollback() triple in the function body.
//
// Anti-cheat: severity=advise at launch.
type bos003 struct{}

func (bos003) ID() string             { return "BOS-003" }
func (bos003) CLAUDEMDAnchor() string { return "AUDIT-069 — multi-write atomicity" }
func (bos003) Severity() bos.Severity { return bos.SeverityAdvise }

func (bos003) Check(file *ast.File, path string, _ *types.Info) []bos.Finding {
	var out []bos.Finding

	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			continue
		}
		mutCount := 0
		hasBegin, hasCommitOrRollback := false, false
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			name := sel.Sel.Name
			// Mutating store helpers — heuristic on function name verb.
			if isMutatorVerb(name) && isStoreCallExpr(sel) {
				mutCount++
			}
			// Tx markers — db.Begin / Tx.Commit / Tx.Rollback /
			// db.BeginTx / sql.Tx.Commit. The lexical match is
			// sufficient because the BoS reviewer is structural, not
			// types-aware.
			if name == "Begin" || name == "BeginTx" {
				hasBegin = true
			}
			if name == "Commit" || name == "Rollback" {
				hasCommitOrRollback = true
			}
			return true
		})
		if mutCount >= 2 && !(hasBegin && hasCommitOrRollback) {
			out = append(out, bos.Finding{
				RuleID:   "BOS-003",
				Severity: bos.SeverityAdvise,
				Path:     path,
				Line:     positionLine(fn),
				Message:  "function " + fn.Name.Name + " makes >=2 mutating store calls without db.Begin/Commit — wrap in a transaction per AUDIT-069",
			})
		}
	}
	return out
}

func isMutatorVerb(name string) bool {
	for _, v := range mutatorVerbs {
		if strings.HasPrefix(name, v) {
			return true
		}
	}
	return false
}

// isStoreCallExpr inspects the receiver of a SelectorExpr and
// returns true when it's the `store` package or a *sql.DB-style
// object whose method name implies SQL mutation. We treat both as
// candidates because the rule is about multi-write atomicity, not
// about the specific package.
func isStoreCallExpr(sel *ast.SelectorExpr) bool {
	switch x := sel.X.(type) {
	case *ast.Ident:
		// Direct store.Foo(...) or db.Exec(...).
		if x.Name == "store" {
			return true
		}
	case *ast.SelectorExpr:
		// s.store.Foo(...)
		if inner, ok := x.X.(*ast.Ident); ok && inner.Name != "" {
			_ = inner
		}
		if x.Sel.Name == "store" {
			return true
		}
	}
	return false
}

func init() { bos.Register(bos003{}) }
