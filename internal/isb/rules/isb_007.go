package rules

import (
	"go/ast"
	"go/types"

	"force-orchestrator/internal/isb"
)

// ISB-007 — DestructiveFileOpWithoutContainmentCheck
//
// AUDIT-019 anchor. Flags `os.Remove` / `os.RemoveAll` calls and
// subprocess shell-outs to `git clean -fdx` where the enclosing
// function does NOT contain a preceding `AssertWithinRepo(...)` /
// `ValidateNoSymlinkEscape(...)` containment check.
//
// Anti-cheat: severity=advise at launch.
//
// Deterministic-fallback note: pure AST. The "preceding containment
// check" predicate is structural — a name-based scan inside the
// enclosing FuncDecl body is sufficient since the containment
// helpers have stable names in the codebase.
type isb007 struct{}

func (isb007) ID() string             { return "ISB-007" }
func (isb007) CLAUDEMDAnchor() string { return "AUDIT-019 destructive file ops" }
func (isb007) Severity() isb.Severity { return isb.SeverityAdvise }

var containmentNames = map[string]bool{
	"AssertWithinRepo":         true,
	"AssertNotDefaultBranch":   true, // catches the destructive-git-op family
	"ValidateNoSymlinkEscape":  true,
	"ValidateRepoRelativePath": true,
}

func (isb007) Check(file *ast.File, path, _ string, _ *types.Info) []isb.Finding {
	var out []isb.Finding
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			continue
		}
		hasContainment := false
		var destructiveCalls []*ast.CallExpr
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			ce, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			name := callName(ce)
			if containmentNames[name] {
				hasContainment = true
			}
			if isDestructiveFileOp(ce) {
				destructiveCalls = append(destructiveCalls, ce)
			}
			return true
		})
		if hasContainment || len(destructiveCalls) == 0 {
			continue
		}
		for _, ce := range destructiveCalls {
			out = append(out, isb.Finding{
				RuleID:   "ISB-007",
				Severity: isb.SeverityAdvise,
				Path:     path,
				Line:     positionLineAt(ce.Pos()),
				Message:  "ISB-007: destructive file op without preceding AssertWithinRepo / ValidateNoSymlinkEscape — guard against symlink escape and out-of-repo deletion (AUDIT-019)",
			})
		}
	}
	return out
}

// isDestructiveFileOp recognizes os.Remove/RemoveAll and a heuristic
// for `exec.Command("git", "clean", "-fdx", ...)` shape.
func isDestructiveFileOp(ce *ast.CallExpr) bool {
	se, ok := ce.Fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	if id, ok := se.X.(*ast.Ident); ok && id.Name == "os" {
		if se.Sel.Name == "Remove" || se.Sel.Name == "RemoveAll" {
			return true
		}
	}
	// `exec.Command("git", "clean", "-fdx", ...)` — three string lits.
	if id, ok := se.X.(*ast.Ident); ok && id.Name == "exec" && (se.Sel.Name == "Command" || se.Sel.Name == "CommandContext") {
		hits := 0
		for _, arg := range ce.Args {
			bl, ok := arg.(*ast.BasicLit)
			if !ok {
				continue
			}
			switch bl.Value {
			case `"git"`, `"clean"`, `"-fdx"`, `"-f"`, `"-d"`, `"-x"`:
				hits++
			}
		}
		if hits >= 3 {
			return true
		}
	}
	return false
}

func init() { isb.Register(isb007{}) }
