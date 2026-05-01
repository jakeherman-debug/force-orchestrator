package rules

import (
	"go/ast"
	"go/types"

	"force-orchestrator/internal/bos"
)

// BOS-009 — TimeSleepInEstopLoop
//
// CLAUDE.md anchor: Fix #1 (e-stop responsiveness).
//
// Flags raw `time.Sleep(...)` calls inside a `for` loop body that
// also contains a call to `IsEstopped(...)`. The fleet pattern is:
// when a goroutine is gated on IsEstopped, the sleep MUST be
// context-aware (e.g. EstopAwareSleep / select on ctx.Done()) so the
// loop exits promptly when the operator hits e-stop.
//
// Anti-cheat: severity=advise at launch.
type bos009 struct{}

func (bos009) ID() string             { return "BOS-009" }
func (bos009) CLAUDEMDAnchor() string { return "Fix #1 — e-stop responsiveness" }
func (bos009) Severity() bos.Severity { return bos.SeverityAdvise }

func (bos009) Check(file *ast.File, path string, _ *types.Info) []bos.Finding {
	var out []bos.Finding
	ast.Inspect(file, func(n ast.Node) bool {
		fl, ok := n.(*ast.ForStmt)
		if !ok {
			// Also handle range-style loops.
			if _, isRange := n.(*ast.RangeStmt); !isRange {
				return true
			}
		}
		var body *ast.BlockStmt
		switch v := n.(type) {
		case *ast.ForStmt:
			body = v.Body
			_ = fl
		case *ast.RangeStmt:
			body = v.Body
		}
		if body == nil {
			return true
		}

		hasEstop := false
		var sleeps []ast.Node
		ast.Inspect(body, func(inner ast.Node) bool {
			call, ok := inner.(*ast.CallExpr)
			if !ok {
				return true
			}
			if callName(call) == "IsEstopped" {
				hasEstop = true
			}
			if sel, ok := call.Fun.(*ast.SelectorExpr); ok {
				pkg, isIdent := sel.X.(*ast.Ident)
				if isIdent && pkg.Name == "time" && sel.Sel.Name == "Sleep" {
					sleeps = append(sleeps, call)
				}
			}
			return true
		})
		if hasEstop {
			for _, s := range sleeps {
				out = append(out, bos.Finding{
					RuleID:   "BOS-009",
					Severity: bos.SeverityAdvise,
					Path:     path,
					Line:     positionLine(s),
					Message:  "raw time.Sleep inside e-stop loop — use a ctx-aware sleep so e-stop preempts promptly per Fix #1",
				})
			}
		}
		return true
	})
	return out
}

func init() { bos.Register(bos009{}) }
