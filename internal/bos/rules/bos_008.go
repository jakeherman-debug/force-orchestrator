package rules

import (
	"go/ast"
	"go/types"
	"regexp"
	"strings"

	"force-orchestrator/internal/bos"
)

// BOS-008 — NewTableMissingIndex
//
// CLAUDE.md anchor: P4 (hot-path query coverage).
//
// Flags new `CREATE TABLE <name> (...)` statements where the file
// also contains a `CREATE INDEX` referencing other columns of OTHER
// tables but NOT a single `CREATE INDEX ... ON <name>(...)` for the
// new table. This is a heuristic — the rule is intentionally narrow
// (file-local scan) so it surfaces obvious omissions without an
// inter-file analysis pass.
//
// Anti-cheat: severity=advise at launch (matches docs/next-gen-agents.md
// — BOS-008 is the one rule that already starts at advise upstream).
type bos008 struct{}

func (bos008) ID() string             { return "BOS-008" }
func (bos008) CLAUDEMDAnchor() string { return "P4 — hot-path indexes" }
func (bos008) Severity() bos.Severity { return bos.SeverityAdvise }

var (
	createTableRe = regexp.MustCompile(`(?is)CREATE\s+TABLE\s+(?:IF\s+NOT\s+EXISTS\s+)?(\w+)\s*\(`)
	createIndexRe = regexp.MustCompile(`(?i)CREATE(?:\s+UNIQUE)?\s+INDEX\s+(?:IF\s+NOT\s+EXISTS\s+)?\w+\s+ON\s+(\w+)\s*\(`)
)

func (bos008) Check(file *ast.File, path string, _ *types.Info) []bos.Finding {
	var out []bos.Finding

	// Collect every SQL string literal in the file.
	type sqlLit struct {
		text string
		node ast.Node
	}
	var lits []sqlLit
	ast.Inspect(file, func(n ast.Node) bool {
		lit, ok := n.(*ast.BasicLit)
		if !ok || lit.Kind != 9 /* token.STRING */ {
			return true
		}
		val := strings.Trim(lit.Value, "`\"")
		if strings.Contains(strings.ToUpper(val), "CREATE TABLE") ||
			strings.Contains(strings.ToUpper(val), "CREATE INDEX") {
			lits = append(lits, sqlLit{text: val, node: lit})
		}
		return true
	})

	indexedTables := map[string]bool{}
	for _, l := range lits {
		for _, m := range createIndexRe.FindAllStringSubmatch(l.text, -1) {
			indexedTables[strings.ToLower(m[1])] = true
		}
	}

	for _, l := range lits {
		for _, m := range createTableRe.FindAllStringSubmatch(l.text, -1) {
			tableName := m[1]
			if indexedTables[strings.ToLower(tableName)] {
				continue
			}
			out = append(out, bos.Finding{
				RuleID:   "BOS-008",
				Severity: bos.SeverityAdvise,
				Path:     path,
				Line:     positionLine(l.node),
				Message:  "table " + tableName + " has no CREATE INDEX in this file — confirm hot-path columns are indexed per Pattern P4",
			})
		}
	}
	return out
}

func init() { bos.Register(bos008{}) }
