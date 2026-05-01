package rules

import (
	"go/ast"
	"go/types"
	"strings"

	"force-orchestrator/internal/isb"
)

// ISB-010 — JSONUnmarshalLLMResponseWithoutDisallowUnknownFields
//
// Pattern P12 / Fix #8.5 anchor. Flags `json.Unmarshal(data, &v)` calls
// in functions whose name suggests they handle LLM responses
// (function names containing Response/LLMResult/Claude/Haiku, or in
// internal/agents/) when the surrounding scope does NOT use
// `decoder.DisallowUnknownFields()`.
//
// Anti-cheat: severity=advise at launch (matches the spec's posture
// for ISB-010).
//
// Deterministic-fallback note: pure AST. The "function-name heuristic"
// + "DisallowUnknownFields call presence" both resolve structurally.
type isb010 struct{}

func (isb010) ID() string             { return "ISB-010" }
func (isb010) CLAUDEMDAnchor() string { return "Pattern P12 / Fix #8.5 LLM response unmarshal" }
func (isb010) Severity() isb.Severity { return isb.SeverityAdvise }

// llmFunctionPatterns is the case-insensitive substring match against
// a func decl's name. If the function file lives in internal/agents/
// the rule fires regardless.
var llmFunctionPatterns = []string{
	"response", "llmresult", "claude", "haiku", "opus",
	"transcript", "promptresult",
}

func (isb010) Check(file *ast.File, path, _ string, _ *types.Info) []isb.Finding {
	inAgentsPath := strings.Contains(path, "internal/agents")
	var out []isb.Finding
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			continue
		}
		if !inAgentsPath && !looksLikeLLMHandler(fn.Name.Name) {
			continue
		}
		hasDisallow := false
		var unmarshalCalls []*ast.CallExpr
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			ce, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			if isJSONUnmarshal(ce) {
				unmarshalCalls = append(unmarshalCalls, ce)
			}
			if isDisallowUnknownFields(ce) {
				hasDisallow = true
			}
			return true
		})
		if hasDisallow || len(unmarshalCalls) == 0 {
			continue
		}
		for _, ce := range unmarshalCalls {
			out = append(out, isb.Finding{
				RuleID:   "ISB-010",
				Severity: isb.SeverityAdvise,
				Path:     path,
				Line:     positionLineAt(ce.Pos()),
				Message:  "ISB-010: json.Unmarshal in LLM-response context without DisallowUnknownFields — use json.NewDecoder + DisallowUnknownFields to reject prompt-injected fields (Pattern P12)",
			})
		}
	}
	return out
}

func looksLikeLLMHandler(name string) bool {
	lower := strings.ToLower(name)
	for _, p := range llmFunctionPatterns {
		if strings.Contains(lower, p) {
			return true
		}
	}
	return false
}

func isJSONUnmarshal(ce *ast.CallExpr) bool {
	se, ok := ce.Fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	if id, ok := se.X.(*ast.Ident); ok && id.Name == "json" && se.Sel.Name == "Unmarshal" {
		return true
	}
	return false
}

func isDisallowUnknownFields(ce *ast.CallExpr) bool {
	se, ok := ce.Fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	return se.Sel.Name == "DisallowUnknownFields"
}

func init() { isb.Register(isb010{}) }
