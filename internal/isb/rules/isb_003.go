package rules

import (
	"go/ast"
	"go/types"
	"strings"

	"force-orchestrator/internal/isb"
)

// ISB-003 — ConcatenatedSQL
//
// Pattern P3 anchor. Flags string concatenation that builds a SQL
// query (heuristic: a string literal containing SELECT/INSERT/
// UPDATE/DELETE concatenated via `+` to an identifier or expression).
// Allows the `?`-style and `:name`-style parameterizations.
//
// Anti-cheat: severity=advise at launch.
//
// Deterministic-fallback note: pure AST. The shape "BinaryExpr Op=+
// where one operand is a SQL-keyword-bearing string literal and the
// other is non-literal" is structurally detectable. The
// scanners.SQLConcatPatterns regex covers cross-line `+\n` shapes
// AST might miss; we only run AST here to keep the scope tight.
type isb003 struct{}

func (isb003) ID() string             { return "ISB-003" }
func (isb003) CLAUDEMDAnchor() string { return "Pattern P3 SQL injection" }
func (isb003) Severity() isb.Severity { return isb.SeverityAdvise }

var sqlKeywords = []string{"SELECT ", "INSERT ", "UPDATE ", "DELETE ", "DROP ", "ALTER "}

func (isb003) Check(file *ast.File, path, _ string, _ *types.Info) []isb.Finding {
	var out []isb.Finding
	ast.Inspect(file, func(n ast.Node) bool {
		be, ok := n.(*ast.BinaryExpr)
		if !ok || be.Op.String() != "+" {
			return true
		}
		// One side must be a string literal containing a SQL keyword;
		// the other side must NOT be a literal (i.e., it's an
		// expression that interpolates user data).
		litSide, otherSide := classifyBinarySides(be)
		if litSide == nil || otherSide == nil {
			return true
		}
		if !looksLikeSQLLiteral(litSide.Value) {
			return true
		}
		// Allow `?`-bearing literals (parameterized).
		if strings.Contains(litSide.Value, "?") {
			return true
		}
		out = append(out, isb.Finding{
			RuleID:   "ISB-003",
			Severity: isb.SeverityAdvise,
			Path:     path,
			Line:     positionLineAt(be.Pos()),
			Message:  "ISB-003: SQL string concatenation — use parameterized query (?, $1, :name) instead of building the string with `+`",
		})
		return true
	})
	return out
}

// classifyBinarySides returns (literal-side, other-side) iff exactly
// one operand is a string BasicLit; otherwise (nil, nil).
func classifyBinarySides(be *ast.BinaryExpr) (lit *ast.BasicLit, other ast.Expr) {
	bl, isLit := be.X.(*ast.BasicLit)
	bl2, isLit2 := be.Y.(*ast.BasicLit)
	switch {
	case isLit && bl.Kind.String() == "STRING" && (!isLit2 || bl2.Kind.String() != "STRING"):
		return bl, be.Y
	case isLit2 && bl2.Kind.String() == "STRING" && (!isLit || bl.Kind.String() != "STRING"):
		return bl2, be.X
	}
	return nil, nil
}

func looksLikeSQLLiteral(v string) bool {
	upper := strings.ToUpper(v)
	for _, kw := range sqlKeywords {
		if strings.Contains(upper, kw) {
			return true
		}
	}
	return false
}

func init() { isb.Register(isb003{}) }
