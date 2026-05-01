package rules

import (
	"go/ast"
	"go/types"
	"regexp"
	"strings"

	"force-orchestrator/internal/bos"
)

// BOS-006 — RefColumnWriteWithoutValidator
//
// CLAUDE.md anchor: Fix #9 (validate refs/paths/URLs).
//
// Flags new INSERT/UPDATE statements writing to a ref-bearing column
// (column ending in `_id` or named `parent_id`/`convoy_id`) without a
// matching `Validate*Ref`/`Validate*Id` call earlier in the function.
//
// Anti-cheat: severity=advise at launch.
type bos006 struct{}

func (bos006) ID() string             { return "BOS-006" }
func (bos006) CLAUDEMDAnchor() string { return "Fix #9 — validate refs/paths/URLs" }
func (bos006) Severity() bos.Severity { return bos.SeverityAdvise }

var refColumnRe = regexp.MustCompile(`(?i)\b(\w+_id|parent_id|convoy_id)\b`)

func (bos006) Check(file *ast.File, path string, _ *types.Info) []bos.Finding {
	var out []bos.Finding

	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			continue
		}
		hasValidator := false
		var refWrites []*ast.BasicLit
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			// Validator call detection.
			if call, ok := n.(*ast.CallExpr); ok {
				name := callName(call)
				if strings.HasPrefix(name, "Validate") &&
					(strings.HasSuffix(name, "Ref") ||
						strings.HasSuffix(name, "Id") ||
						strings.HasSuffix(name, "ID") ||
						strings.HasSuffix(name, "Refs")) {
					hasValidator = true
				}
			}
			// Look for SQL-string literals doing INSERT/UPDATE on
			// ref-bearing columns.
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != 9 /* token.STRING */ {
				return true
			}
			body := strings.ToUpper(strings.Trim(lit.Value, "`\""))
			if !(strings.Contains(body, "INSERT") || strings.Contains(body, "UPDATE")) {
				return true
			}
			if refColumnRe.MatchString(strings.ToLower(body)) {
				refWrites = append(refWrites, lit)
			}
			return true
		})
		if hasValidator {
			continue
		}
		for _, w := range refWrites {
			out = append(out, bos.Finding{
				RuleID:   "BOS-006",
				Severity: bos.SeverityAdvise,
				Path:     path,
				Line:     positionLine(w),
				Message:  "INSERT/UPDATE writes a ref-bearing column without a Validate*Ref call in scope per Fix #9",
			})
		}
	}
	return out
}

func init() { bos.Register(bos006{}) }
