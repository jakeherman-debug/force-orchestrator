package rules

import (
	"go/ast"
	"go/types"
	"strings"

	"force-orchestrator/internal/isb"
)

// ISB-005 — MutatingHTTPHandlerWithoutSecurityMiddleware
//
// AUDIT-001 / Pattern P8 anchor. Flags new HTTP handler registrations
// (http.HandleFunc, mux.HandleFunc, mux.Handle) for paths that LOOK
// like mutating endpoints (path keywords: create, update, delete,
// reset, mutate, save, post, etc.) when the handler argument is NOT
// wrapped by a `securityMiddleware(...)` (or `withAuth(...)` /
// `requireAuth(...)`) call.
//
// Anti-cheat: severity=advise at launch.
//
// Deterministic-fallback note: The spec hints this rule MAY need an
// LLM ("is this new HTTP handler authenticated?"). We attempted the
// deterministic check first: structural pattern match on the
// HandleFunc/Handle call site, looking for a securityMiddleware-style
// wrapper as the handler argument. The deterministic check is
// sufficient because the operator's middleware convention is well-
// established (see internal/dashboard for the canonical shape) and
// the false-positive cost of a one-line wrapper rename is low. If a
// future shape proves resistant to AST detection, the LLM-judge
// upgrade follows the gating in package isb's docs:
// LIVE_HAIKU_DISABLED + SpendCapExceeded.
type isb005 struct{}

func (isb005) ID() string             { return "ISB-005" }
func (isb005) CLAUDEMDAnchor() string { return "AUDIT-001 / Pattern P8 dashboard auth" }
func (isb005) Severity() isb.Severity { return isb.SeverityAdvise }

// mutatingPathKeywords are case-insensitive substrings; if the
// HandleFunc path arg contains any, the handler is considered
// mutating.
var mutatingPathKeywords = []string{
	"create", "update", "delete", "remove", "reset", "save",
	"mutate", "promote", "approve", "reject", "set", "patch",
	"add", "submit", "trigger",
}

// allowedMiddleware is the set of wrapper-call names that satisfy
// the rule. Lexical match.
var allowedMiddleware = map[string]bool{
	"securityMiddleware": true,
	"SecurityMiddleware": true,
	"withAuth":           true,
	"WithAuth":           true,
	"requireAuth":        true,
	"RequireAuth":        true,
	"authMiddleware":     true,
	"AuthMiddleware":     true,
}

func (isb005) Check(file *ast.File, path, _ string, _ *types.Info) []isb.Finding {
	var out []isb.Finding
	ast.Inspect(file, func(n ast.Node) bool {
		ce, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		if !isHandleFunc(ce) {
			return true
		}
		// args[0] is the path; args[1] is the handler.
		if len(ce.Args) < 2 {
			return true
		}
		pathLit, isLit := ce.Args[0].(*ast.BasicLit)
		if !isLit || pathLit.Kind.String() != "STRING" {
			return true
		}
		if !looksMutating(pathLit.Value) {
			return true
		}
		// The handler must be a CallExpr whose Fun is one of
		// allowedMiddleware (wrapping shape).
		if isWrappedBySecurityMiddleware(ce.Args[1]) {
			return true
		}
		out = append(out, isb.Finding{
			RuleID:   "ISB-005",
			Severity: isb.SeverityAdvise,
			Path:     path,
			Line:     positionLineAt(ce.Pos()),
			Message:  "ISB-005: mutating HTTP handler at " + pathLit.Value + " not wrapped by securityMiddleware/withAuth — protect mutating endpoints (Pattern P8)",
		})
		return true
	})
	return out
}

func isHandleFunc(ce *ast.CallExpr) bool {
	se, ok := ce.Fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	if se.Sel.Name != "HandleFunc" && se.Sel.Name != "Handle" {
		return false
	}
	// Accept both `http.HandleFunc(...)` and `mux.HandleFunc(...)`.
	return true
}

func looksMutating(quotedPath string) bool {
	lower := strings.ToLower(quotedPath)
	for _, kw := range mutatingPathKeywords {
		if strings.Contains(lower, kw) {
			return true
		}
	}
	return false
}

func isWrappedBySecurityMiddleware(handler ast.Expr) bool {
	ce, ok := handler.(*ast.CallExpr)
	if !ok {
		return false
	}
	switch fn := ce.Fun.(type) {
	case *ast.Ident:
		return allowedMiddleware[fn.Name]
	case *ast.SelectorExpr:
		return allowedMiddleware[fn.Sel.Name]
	}
	return false
}

func init() { isb.Register(isb005{}) }
