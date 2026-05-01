package rules

import (
	"go/ast"
	"go/types"
	"strings"

	"force-orchestrator/internal/bos"
)

// BOS-004 — SpawnWithoutContextEstop
//
// CLAUDE.md anchor: "Daemon context threading" + Fix #1 (e-stop /
// spend-cap gating).
//
// Flags new `func Spawn<X>` declarations whose body does NOT contain:
//   - a use of `ctx` (the first parameter, by name), AND
//   - a call to IsEstopped(...), AND
//   - a call to SpendCapExceeded(...).
//
// Anti-cheat: severity=advise at launch.
type bos004 struct{}

func (bos004) ID() string             { return "BOS-004" }
func (bos004) CLAUDEMDAnchor() string { return "Daemon context threading" }
func (bos004) Severity() bos.Severity { return bos.SeverityAdvise }

func (bos004) Check(file *ast.File, path string, _ *types.Info) []bos.Finding {
	var out []bos.Finding
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			continue
		}
		if !strings.HasPrefix(fn.Name.Name, "Spawn") {
			continue
		}
		usesCtx, hasIsEstopped, hasSpendCap := false, false, false
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			switch v := n.(type) {
			case *ast.Ident:
				if v.Name == "ctx" {
					usesCtx = true
				}
			case *ast.CallExpr:
				name := callName(v)
				if name == "IsEstopped" {
					hasIsEstopped = true
				}
				if name == "SpendCapExceeded" {
					hasSpendCap = true
				}
			}
			return true
		})
		if !(usesCtx && hasIsEstopped && hasSpendCap) {
			missing := []string{}
			if !usesCtx {
				missing = append(missing, "ctx use")
			}
			if !hasIsEstopped {
				missing = append(missing, "IsEstopped")
			}
			if !hasSpendCap {
				missing = append(missing, "SpendCapExceeded")
			}
			out = append(out, bos.Finding{
				RuleID:   "BOS-004",
				Severity: bos.SeverityAdvise,
				Path:     path,
				Line:     positionLine(fn),
				Message:  "Spawn function " + fn.Name.Name + " missing required guards: " + strings.Join(missing, ", "),
			})
		}
	}
	return out
}

// callName extracts the function name from a CallExpr — handles both
// `Foo(...)` (Ident) and `pkg.Foo(...)` (SelectorExpr).
func callName(c *ast.CallExpr) string {
	switch fn := c.Fun.(type) {
	case *ast.Ident:
		return fn.Name
	case *ast.SelectorExpr:
		return fn.Sel.Name
	}
	return ""
}

func init() { bos.Register(bos004{}) }
