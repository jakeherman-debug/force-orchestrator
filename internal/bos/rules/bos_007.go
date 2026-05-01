package rules

import (
	"go/ast"
	"go/types"
	"regexp"
	"strings"

	"force-orchestrator/internal/bos"
)

// BOS-007 — PayloadLikeConvoyId
//
// CLAUDE.md anchor: "Convoy-scoped queries use convoy_id not LIKE"
// (P3, AUDIT-011).
//
// Flags new SQL string literals containing the
// `payload LIKE '%"convoy_id":...'` substring shape. The CLAUDE.md
// invariant requires `WHERE convoy_id = ?` instead.
//
// Anti-cheat: severity=advise at launch.
type bos007 struct{}

func (bos007) ID() string             { return "BOS-007" }
func (bos007) CLAUDEMDAnchor() string { return "Convoy-scoped queries use convoy_id not LIKE" }
func (bos007) Severity() bos.Severity { return bos.SeverityAdvise }

// payloadLikeConvoyRe matches the canonical violating shape regardless
// of whitespace and quoting. Examples that match:
//   - payload LIKE '%"convoy_id":1,%'
//   - payload  LIKE  "%\"convoy_id\":1,%"
var payloadLikeConvoyRe = regexp.MustCompile(`(?i)payload\s+LIKE\s+.{1,5}.*convoy_id`)

func (bos007) Check(file *ast.File, path string, _ *types.Info) []bos.Finding {
	var out []bos.Finding
	ast.Inspect(file, func(n ast.Node) bool {
		lit, ok := n.(*ast.BasicLit)
		if !ok || lit.Kind != 9 /* token.STRING */ {
			return true
		}
		body := strings.Trim(lit.Value, "`\"")
		if !payloadLikeConvoyRe.MatchString(body) {
			return true
		}
		out = append(out, bos.Finding{
			RuleID:   "BOS-007",
			Severity: bos.SeverityAdvise,
			Path:     path,
			Line:     positionLine(lit),
			Message:  "use WHERE convoy_id = ? not payload LIKE — see CLAUDE.md Convoy-scoped queries invariant",
		})
		return true
	})
	return out
}

func init() { bos.Register(bos007{}) }
