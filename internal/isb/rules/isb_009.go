package rules

import (
	"go/ast"
	"go/types"

	"force-orchestrator/internal/isb"
)

// ISB-009 — UnboundedReadAllOnExternalInput
//
// AUDIT-057 anchor. Flags `io.ReadAll(r)` and `bytes.Buffer.ReadFrom(r)`
// where the reader is plausibly external (HTTP body, network reader)
// AND the enclosing function does NOT contain a preceding
// `io.LimitReader(...)` (or `http.MaxBytesReader(...)`) wrap.
//
// Heuristic: if the receiver name is one of `r`, `body`, `resp`,
// `response`, `req`, `request`, `reader` AND no LimitReader-style
// call appears in the function body, the rule fires. Functions that
// don't take an external-shaped reader argument are skipped to keep
// false-positive rate low.
//
// Anti-cheat: severity=advise at launch (matches the spec's posture
// for ISB-009).
//
// Deterministic-fallback note: pure AST.
type isb009 struct{}

func (isb009) ID() string             { return "ISB-009" }
func (isb009) CLAUDEMDAnchor() string { return "AUDIT-057 unbounded read" }
func (isb009) Severity() isb.Severity { return isb.SeverityAdvise }

var externalReaderNames = map[string]bool{
	"r": true, "body": true, "Body": true,
	"resp": true, "response": true, "Response": true,
	"req": true, "request": true, "Request": true,
	"reader": true,
}

var limiterNames = map[string]bool{
	"LimitReader":     true,
	"MaxBytesReader":  true,
	"LimitedReader":   true,
}

func (isb009) Check(file *ast.File, path, _ string, _ *types.Info) []isb.Finding {
	var out []isb.Finding
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			continue
		}
		hasLimiter := false
		var unsafeReads []*ast.CallExpr
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			ce, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			name := callName(ce)
			if limiterNames[name] {
				hasLimiter = true
			}
			if isUnboundedReadOnExternal(ce) {
				unsafeReads = append(unsafeReads, ce)
			}
			return true
		})
		if hasLimiter || len(unsafeReads) == 0 {
			continue
		}
		for _, ce := range unsafeReads {
			out = append(out, isb.Finding{
				RuleID:   "ISB-009",
				Severity: isb.SeverityAdvise,
				Path:     path,
				Line:     positionLineAt(ce.Pos()),
				Message:  "ISB-009: io.ReadAll on external-shaped reader without LimitReader/MaxBytesReader wrap — bound the read (AUDIT-057)",
			})
		}
	}
	return out
}

// isUnboundedReadOnExternal: io.ReadAll(arg) or buf.ReadFrom(arg)
// where arg is one of externalReaderNames (or a SelectorExpr whose
// Sel.Name is, e.g., r.Body).
func isUnboundedReadOnExternal(ce *ast.CallExpr) bool {
	se, ok := ce.Fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	switch se.Sel.Name {
	case "ReadAll":
		// io.ReadAll(arg) or ioutil.ReadAll(arg)
		if id, ok := se.X.(*ast.Ident); ok && (id.Name == "io" || id.Name == "ioutil") {
			if len(ce.Args) >= 1 && argLooksExternal(ce.Args[0]) {
				return true
			}
		}
	case "ReadFrom":
		// buf.ReadFrom(arg)
		if len(ce.Args) >= 1 && argLooksExternal(ce.Args[0]) {
			return true
		}
	}
	return false
}

func argLooksExternal(e ast.Expr) bool {
	switch v := e.(type) {
	case *ast.Ident:
		return externalReaderNames[v.Name]
	case *ast.SelectorExpr:
		// e.g., r.Body, resp.Body
		return externalReaderNames[v.Sel.Name]
	}
	return false
}

func init() { isb.Register(isb009{}) }
