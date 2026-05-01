package rules

import (
	"go/ast"
	"go/types"
	"strconv"
	"strings"

	"force-orchestrator/internal/isb"
)

// ISB-006 — FilePermissionTooOpen
//
// AUDIT-100 anchor. Flags `os.Create`, `os.MkdirAll`, `os.OpenFile`,
// `os.WriteFile` calls with a numeric mode literal > 0700 when the
// path argument is a literal pointing into a sensitive prefix
// (`/etc/`, `/var/`, `secrets/`, `credentials/`, etc.) — OR when the
// path argument is non-literal (i.e., dynamic) AND the mode is > 0700
// (the dynamic-path case errs on the side of caution).
//
// Anti-cheat: severity=advise at launch.
//
// Deterministic-fallback note: pure AST + literal numeric parsing.
type isb006 struct{}

func (isb006) ID() string             { return "ISB-006" }
func (isb006) CLAUDEMDAnchor() string { return "AUDIT-100 file permissions" }
func (isb006) Severity() isb.Severity { return isb.SeverityAdvise }

var sensitivePathPrefixes = []string{
	"/etc/", "/var/", "/root/", "/home/",
	"secrets/", "credentials/", ".ssh/", "private/",
}

func (isb006) Check(file *ast.File, path, _ string, _ *types.Info) []isb.Finding {
	var out []isb.Finding
	ast.Inspect(file, func(n ast.Node) bool {
		ce, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		fnName, fnPkg := selectorParts(ce.Fun)
		if fnPkg != "os" {
			return true
		}
		modeIdx, pathIdx, hasPerm := osCallShape(fnName)
		if !hasPerm || len(ce.Args) <= modeIdx {
			return true
		}
		mode, ok := extractFileMode(ce.Args[modeIdx])
		if !ok {
			return true
		}
		// Tighter than 0700 always passes.
		if mode <= 0o700 {
			return true
		}
		// Mode > 0700 + sensitive path → finding.
		sensitive := false
		if pathIdx < len(ce.Args) {
			if bl, isLit := ce.Args[pathIdx].(*ast.BasicLit); isLit && bl.Kind.String() == "STRING" {
				lower := strings.ToLower(bl.Value)
				for _, p := range sensitivePathPrefixes {
					if strings.Contains(lower, p) {
						sensitive = true
						break
					}
				}
			} else {
				// Non-literal path + open mode → also flag.
				sensitive = true
			}
		}
		if !sensitive {
			return true
		}
		out = append(out, isb.Finding{
			RuleID:   "ISB-006",
			Severity: isb.SeverityAdvise,
			Path:     path,
			Line:     positionLineAt(ce.Pos()),
			Message:  "ISB-006: os." + fnName + " with permission mode > 0700 in sensitive path — restrict permissions (AUDIT-100)",
		})
		return true
	})
	return out
}

// osCallShape returns (modeArgIndex, pathArgIndex, hasMode) for the
// supported os.* funcs. Hard-coded shapes per stdlib signatures.
func osCallShape(name string) (modeIdx, pathIdx int, ok bool) {
	switch name {
	case "Create":
		// os.Create(name) — no mode arg. Skip.
		return 0, 0, false
	case "MkdirAll":
		return 1, 0, true
	case "Mkdir":
		return 1, 0, true
	case "OpenFile":
		return 2, 0, true
	case "WriteFile":
		return 2, 0, true
	}
	return 0, 0, false
}

// extractFileMode parses an integer literal from an ast.Expr. Handles
// hex (0x...) and octal (0...) and decimal forms.
func extractFileMode(e ast.Expr) (int, bool) {
	bl, ok := e.(*ast.BasicLit)
	if !ok || bl.Kind.String() != "INT" {
		return 0, false
	}
	v, err := strconv.ParseInt(bl.Value, 0, 32)
	if err != nil {
		return 0, false
	}
	return int(v), true
}

// selectorParts returns (selectorName, packageOrReceiverName) for a
// SelectorExpr-shape call.
func selectorParts(fn ast.Expr) (sel, pkg string) {
	se, ok := fn.(*ast.SelectorExpr)
	if !ok {
		return "", ""
	}
	id, ok := se.X.(*ast.Ident)
	if !ok {
		return se.Sel.Name, ""
	}
	return se.Sel.Name, id.Name
}

func init() { isb.Register(isb006{}) }
