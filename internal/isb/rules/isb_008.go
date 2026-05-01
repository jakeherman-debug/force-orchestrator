package rules

import (
	"go/ast"
	"go/types"
	"strings"

	"force-orchestrator/internal/isb"
)

// ISB-008 — LLMPromptConcatExternalContent
//
// Pattern P12 / Fix #8.5 anchor. Flags new prompts (string-built
// arguments to claude.CallWithTranscript or similar) that interpolate
// external content (HTTP body, file contents, user input) without
// the `<user_content>` / `</user_content>` tag wrapping.
//
// Heuristic: the rule fires when a CallExpr targets `claude.Call*`
// AND any of its string-typed arguments is a BinaryExpr (`+` concat)
// involving non-literal expressions AND no string literal in the
// expression chain contains the `<user_content>` sentinel.
//
// Anti-cheat: severity=advise at launch.
//
// Deterministic-fallback note: The spec hints this rule MAY need an
// LLM ("could this prompt leak / get hijacked?"). We attempted the
// deterministic check first: AST walk of the prompt construction
// expression, checking for the `<user_content>` literal. The
// deterministic check is sufficient for the 80% case (a developer
// builds a prompt with `+` and a variable). If a future shape
// (template-based prompts with hidden interpolation) defeats the AST
// walk, the LLM-judge upgrade follows the same gating as ISB-005.
type isb008 struct{}

func (isb008) ID() string             { return "ISB-008" }
func (isb008) CLAUDEMDAnchor() string { return "Pattern P12 / Fix #8.5 prompt injection" }
func (isb008) Severity() isb.Severity { return isb.SeverityAdvise }

const userContentSentinel = "<user_content>"

func (isb008) Check(file *ast.File, path, _ string, _ *types.Info) []isb.Finding {
	var out []isb.Finding
	ast.Inspect(file, func(n ast.Node) bool {
		ce, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		if !isClaudeCallSite(ce) {
			return true
		}
		// Walk every arg looking for string-concat expressions.
		for _, arg := range ce.Args {
			if !isStringConcatWithExternal(arg) {
				continue
			}
			if expressionContainsSentinel(arg, userContentSentinel) {
				continue
			}
			out = append(out, isb.Finding{
				RuleID:   "ISB-008",
				Severity: isb.SeverityAdvise,
				Path:     path,
				Line:     positionLineAt(arg.Pos()),
				Message:  "ISB-008: LLM prompt interpolates external content without `<user_content>` tag wrapping — wrap untrusted input in <user_content>...</user_content> sentinels (Fix #8.5 / Pattern P12)",
			})
		}
		return true
	})
	return out
}

// isClaudeCallSite returns true iff the CallExpr targets one of the
// claude.Call* helpers.
func isClaudeCallSite(ce *ast.CallExpr) bool {
	se, ok := ce.Fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	id, ok := se.X.(*ast.Ident)
	if !ok || id.Name != "claude" {
		return false
	}
	return strings.HasPrefix(se.Sel.Name, "Call")
}

// isStringConcatWithExternal returns true iff the expression is a
// BinaryExpr (+) involving at least one non-literal operand and at
// least one string literal operand. Recursive to walk chains.
func isStringConcatWithExternal(e ast.Expr) bool {
	be, ok := e.(*ast.BinaryExpr)
	if !ok || be.Op.String() != "+" {
		return false
	}
	hasLit, hasNonLit := walkConcatChain(be)
	return hasLit && hasNonLit
}

func walkConcatChain(e ast.Expr) (hasLit, hasNonLit bool) {
	switch v := e.(type) {
	case *ast.BinaryExpr:
		if v.Op.String() != "+" {
			// non-concat binary — treat the whole thing as non-literal
			return false, true
		}
		l1, n1 := walkConcatChain(v.X)
		l2, n2 := walkConcatChain(v.Y)
		return l1 || l2, n1 || n2
	case *ast.BasicLit:
		if v.Kind.String() == "STRING" {
			return true, false
		}
		return false, true
	default:
		return false, true
	}
}

// expressionContainsSentinel returns true iff any string literal in
// the concat chain contains the sentinel substring.
func expressionContainsSentinel(e ast.Expr, sentinel string) bool {
	found := false
	ast.Inspect(e, func(n ast.Node) bool {
		bl, ok := n.(*ast.BasicLit)
		if !ok || bl.Kind.String() != "STRING" {
			return true
		}
		if strings.Contains(bl.Value, sentinel) {
			found = true
			return false
		}
		return true
	})
	return found
}

func init() { isb.Register(isb008{}) }
