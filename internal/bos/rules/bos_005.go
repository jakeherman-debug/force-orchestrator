package rules

import (
	"go/ast"
	"go/types"
	"strings"

	"force-orchestrator/internal/bos"
)

// BOS-005 — DestructiveGitWithoutAssertNotDefaultBranch
//
// CLAUDE.md anchor: Fix #0 (default-branch protection).
//
// Flags new destructive git call sites without a preceding
// AssertNotDefaultBranch(...) call in the same function body.
// Destructive ops covered:
//   - push --force (any ref)
//   - reset --hard
//   - branch -D (force delete)
//   - clean -fdx (with -f or -x)
//
// Anti-cheat: severity=advise at launch.
type bos005 struct{}

func (bos005) ID() string             { return "BOS-005" }
func (bos005) CLAUDEMDAnchor() string { return "Fix #0 — destructive git ops" }
func (bos005) Severity() bos.Severity { return bos.SeverityAdvise }

func (bos005) Check(file *ast.File, path string, _ *types.Info) []bos.Finding {
	var out []bos.Finding
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			continue
		}

		hasAssert := false
		var dangerCalls []*ast.CallExpr
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			if callName(call) == "AssertNotDefaultBranch" {
				hasAssert = true
			}
			if isDestructiveGitCall(call) {
				dangerCalls = append(dangerCalls, call)
			}
			return true
		})
		if hasAssert {
			continue
		}
		for _, c := range dangerCalls {
			out = append(out, bos.Finding{
				RuleID:   "BOS-005",
				Severity: bos.SeverityAdvise,
				Path:     path,
				Line:     positionLine(c),
				Message:  "destructive git op without AssertNotDefaultBranch — guard with the helper per Fix #0",
			})
		}
	}
	return out
}

// isDestructiveGitCall returns true when the call's string-arg list
// describes one of the destructive shapes. Inspects ALL string-literal
// arguments of the call regardless of position so it matches both
// igit.LogAndRun(ctx, "push", "--force") and runShortGit("git",
// "push", "--force") shapes.
func isDestructiveGitCall(call *ast.CallExpr) bool {
	args := []string{}
	for _, a := range call.Args {
		if lit, ok := a.(*ast.BasicLit); ok && lit.Kind == 9 /* token.STRING */ {
			args = append(args, strings.Trim(lit.Value, `"`))
		}
	}
	joined := strings.Join(args, " ")
	switch {
	case strings.Contains(joined, "push") && strings.Contains(joined, "--force"):
		return true
	case strings.Contains(joined, "reset") && strings.Contains(joined, "--hard"):
		return true
	case strings.Contains(joined, "branch") && strings.Contains(joined, "-D"):
		return true
	case strings.Contains(joined, "clean") && (strings.Contains(joined, "-fdx") || (strings.Contains(joined, "-f") && strings.Contains(joined, "-x"))):
		return true
	}
	return false
}

func init() { bos.Register(bos005{}) }
