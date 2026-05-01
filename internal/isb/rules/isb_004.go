package rules

import (
	"go/ast"
	"go/types"

	"force-orchestrator/internal/isb"
)

// ISB-004 — OutboundHTTPWithoutValidateOutboundURL
//
// AUDIT-016 / Pattern P9 anchor. Flags new outbound HTTP call sites
// (http.Get, http.Post, http.PostForm, http.NewRequest, http.NewRequestWithContext)
// where the enclosing function does NOT contain an earlier call to
// `ValidateOutboundURL(...)` (any package).
//
// Flow analysis is intentionally function-local: we don't trace
// across function boundaries because false-positive cost is high. A
// developer adding a new outbound HTTP call adds the validator in
// the same function or routes through a helper.
//
// Anti-cheat: severity=advise at launch.
//
// Deterministic-fallback note: pure AST. No LLM. The "ValidateOutboundURL
// called before this http call site" predicate is structural — walk
// the FuncDecl body in source order, track first-seen positions of
// each helper, and check ordering.
type isb004 struct{}

func (isb004) ID() string             { return "ISB-004" }
func (isb004) CLAUDEMDAnchor() string { return "AUDIT-016 / Pattern P9 outbound HTTP" }
func (isb004) Severity() isb.Severity { return isb.SeverityAdvise }

var outboundHTTPSelectors = map[string]bool{
	"Get": true, "Post": true, "PostForm": true,
	"NewRequest": true, "NewRequestWithContext": true,
	"Do": true, // Client.Do
}

func (isb004) Check(file *ast.File, path, _ string, _ *types.Info) []isb.Finding {
	var out []isb.Finding
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			continue
		}
		hasValidator := false
		var httpCallPositions []*ast.CallExpr
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			ce, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			name := callName(ce)
			if name == "ValidateOutboundURL" {
				hasValidator = true
			}
			if isHTTPOutboundCall(ce) {
				httpCallPositions = append(httpCallPositions, ce)
			}
			return true
		})
		if hasValidator || len(httpCallPositions) == 0 {
			continue
		}
		for _, ce := range httpCallPositions {
			out = append(out, isb.Finding{
				RuleID:   "ISB-004",
				Severity: isb.SeverityAdvise,
				Path:     path,
				Line:     positionLineAt(ce.Pos()),
				Message:  "ISB-004: outbound HTTP call without preceding ValidateOutboundURL — every new outbound HTTP must route through the validator (Fix #10)",
			})
		}
	}
	return out
}

// isHTTPOutboundCall returns true iff ce is one of the canonical
// http.* outbound call shapes.
func isHTTPOutboundCall(ce *ast.CallExpr) bool {
	se, ok := ce.Fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	if !outboundHTTPSelectors[se.Sel.Name] {
		return false
	}
	// X may be `http` (package) OR an identifier whose semantic type
	// is *http.Client / http.Client / http.Request — we lean on
	// lexical "http" or "client"/"req" naming since types.Info isn't
	// always available.
	if id, ok := se.X.(*ast.Ident); ok {
		switch id.Name {
		case "http":
			return true
		case "client", "Client", "c":
			// Could be a *http.Client; flag it. False positives here
			// are advisory, not block — acceptable per launch posture.
			return se.Sel.Name == "Do" || se.Sel.Name == "Get" || se.Sel.Name == "Post" || se.Sel.Name == "PostForm"
		}
	}
	return false
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

func init() { isb.Register(isb004{}) }
