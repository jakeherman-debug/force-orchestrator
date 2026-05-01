package rules

import (
	"go/ast"
	"go/types"
	"strings"

	"force-orchestrator/internal/bos"
)

// BOS-010 — OutboundContentWithoutRedactSecrets
//
// CLAUDE.md anchor: Fix #10 — outbound content redaction.
//
// Flags new outbound emit sites (Slack, GitHub PR/issue body, email,
// generic webhook payloads) where the body argument is NOT wrapped by
// a `RedactSecrets(...)` call.
//
// Recognised emit sites (function-name match):
//   - SendMail, SendSlack, SendSlackMessage
//   - PostComment, AddComment
//   - PostWebhook, EmitWebhook
//
// The check is conservative: any string-typed argument that is NOT a
// string literal (i.e. a name carrying potentially-tainted data) and
// that is not the result of RedactSecrets(...) flags the call.
//
// Anti-cheat: severity=advise at launch.
type bos010 struct{}

func (bos010) ID() string             { return "BOS-010" }
func (bos010) CLAUDEMDAnchor() string { return "Fix #10 — outbound redaction" }
func (bos010) Severity() bos.Severity { return bos.SeverityAdvise }

var outboundEmitNames = map[string]bool{
	"SendMail":         true,
	"SendSlack":        true,
	"SendSlackMessage": true,
	"PostComment":      true,
	"AddComment":       true,
	"PostWebhook":      true,
	"EmitWebhook":      true,
}

func (bos010) Check(file *ast.File, path string, _ *types.Info) []bos.Finding {
	var out []bos.Finding
	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		name := callName(call)
		if !outboundEmitNames[name] {
			return true
		}
		if hasRedactSecretsArg(call) {
			return true
		}
		// Only flag if the call has at least one non-literal,
		// non-redacted argument that could carry tainted content.
		hasTaintedArg := false
		for _, a := range call.Args {
			if isStringLikeNonLiteralIdent(a) {
				hasTaintedArg = true
				break
			}
		}
		if !hasTaintedArg {
			return true
		}
		out = append(out, bos.Finding{
			RuleID:   "BOS-010",
			Severity: bos.SeverityAdvise,
			Path:     path,
			Line:     positionLine(call),
			Message:  "outbound emit " + name + " has a tainted argument not wrapped by RedactSecrets — wrap per Fix #10",
		})
		return true
	})
	return out
}

func hasRedactSecretsArg(call *ast.CallExpr) bool {
	for _, a := range call.Args {
		inner, ok := a.(*ast.CallExpr)
		if !ok {
			continue
		}
		if callName(inner) == "RedactSecrets" {
			return true
		}
	}
	// Tolerate the wrap-then-pass shape: a local var assigned from
	// RedactSecrets is OK, but we cannot reason about that lexically.
	// Operators using that idiom can add a // BOS-BYPASS comment.
	return false
}

// isStringLikeNonLiteralIdent treats bare identifiers, selector
// expressions, and most non-literal expressions as potentially tainted
// string-like arguments. String literals are explicitly NOT flagged.
func isStringLikeNonLiteralIdent(e ast.Expr) bool {
	switch v := e.(type) {
	case *ast.BasicLit:
		return false // literals are constants — safe by definition
	case *ast.Ident:
		// Boolean / numeric idents are not the concern; we only want
		// identifiers that obviously resemble strings/payloads.
		// Conservative heuristic: skip well-known non-string idents.
		switch v.Name {
		case "nil", "true", "false":
			return false
		}
		return true
	case *ast.SelectorExpr, *ast.CallExpr, *ast.BinaryExpr:
		// Calls like `BuildMessage(...)` are tainted; concatenations
		// like `prefix + body` are tainted; selectors like `b.Payload`
		// are tainted.
		// CallExpr only counts if it's NOT RedactSecrets — that
		// branch is filtered upstream by hasRedactSecretsArg.
		if c, ok := e.(*ast.CallExpr); ok && callName(c) == "RedactSecrets" {
			return false
		}
		return true
	default:
		_ = strings.TrimSpace // keep import live
		return false
	}
}

func init() { bos.Register(bos010{}) }
