package rules

import (
	"go/ast"
	"go/types"

	"force-orchestrator/internal/isb"
)

// ISB-002 — ExecCommandPositionalRefBeforeDoubleDash
//
// AUDIT-018 / Pattern P10 anchor. Flags `exec.Command(name, args...)`
// call sites where any positional arg references an identifier (i.e.
// is not a string literal) and the args list does NOT contain a
// literal "--" separator before the first non-literal arg.
//
// The `--` separator is the universal "end of flag parsing" sentinel
// in POSIX-style CLIs (git, go, find, grep, ...). Without it, an
// attacker-controlled positional that begins with a `-` is
// interpreted as a flag — a classic shell-injection class. See
// CLAUDE.md / FIX-LOG.md "Fix #0 destructive git ops" + "Fix #5
// argument injection."
//
// Anti-cheat: severity=advise at launch.
//
// Deterministic-fallback note: this rule is pure AST. No LLM. The
// shape "CallExpr where Fun resolves to exec.Command" is structurally
// detectable; the `--` heuristic is also structural (literal-string
// comparison).
type isb002 struct{}

func (isb002) ID() string             { return "ISB-002" }
func (isb002) CLAUDEMDAnchor() string { return "AUDIT-018 / Pattern P10 shell injection" }
func (isb002) Severity() isb.Severity { return isb.SeverityAdvise }

func (isb002) Check(file *ast.File, path, _ string, _ *types.Info) []isb.Finding {
	var out []isb.Finding
	ast.Inspect(file, func(n ast.Node) bool {
		ce, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		if !isExecCommand(ce.Fun) {
			return true
		}
		// args[0] is the program name; args[1..] are command args.
		if len(ce.Args) < 2 {
			return true
		}
		// Track: did we see a literal "--" before any non-literal?
		sawDoubleDash := false
		hasNonLiteralPositional := false
		var firstSuspectPos int
		for i := 1; i < len(ce.Args); i++ {
			arg := ce.Args[i]
			if bl, ok := arg.(*ast.BasicLit); ok {
				if bl.Value == `"--"` {
					sawDoubleDash = true
				}
				continue
			}
			// A non-literal expression (Ident, CallExpr, SelectorExpr,
			// etc.) is the suspect.
			if !sawDoubleDash {
				hasNonLiteralPositional = true
				if firstSuspectPos == 0 {
					firstSuspectPos = positionLineAt(arg.Pos())
				}
			}
		}
		if hasNonLiteralPositional {
			out = append(out, isb.Finding{
				RuleID:   "ISB-002",
				Severity: isb.SeverityAdvise,
				Path:     path,
				Line:     firstSuspectPos,
				Message:  "ISB-002: exec.Command has user-input-shaped arg before a literal `--` separator — prepend `--` so a leading dash isn't interpreted as a flag",
			})
		}
		return true
	})
	return out
}

// isExecCommand returns true iff fn looks like `exec.Command` or
// `exec.CommandContext`. Lexical SelectorExpr match.
func isExecCommand(fn ast.Expr) bool {
	se, ok := fn.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	pkg, ok := se.X.(*ast.Ident)
	if !ok {
		return false
	}
	if pkg.Name != "exec" {
		return false
	}
	return se.Sel.Name == "Command" || se.Sel.Name == "CommandContext"
}

func init() { isb.Register(isb002{}) }
