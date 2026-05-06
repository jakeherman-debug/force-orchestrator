// Package audittools: Pattern P_DaemonUpdateHistory — every code path
// that exits cmd/force/daemon_cmds.go::cmdDaemonUpdate (and ::cmdDaemonRollback)
// MUST land a row in DaemonUpdateHistory via store.RecordDaemonUpdate.
//
// Why an AST guard, not a runtime check?
//
//   - DaemonUpdateHistory is the operator-facing audit trail for every
//     binary swap. A future refactor that drops the recorder on, say,
//     the "rolled-back due to copy failure" path produces a silent gap
//     (the operator sees a successful boot via launchd but no history
//     row to explain why the binary has the wrong SHA).
//   - The defer-based recorder pattern in the current implementation
//     guarantees every return path lands a row. This audit confirms
//     the deferred call is present in both functions AND the schema is
//     declared in all three locations (createSchema, runMigrations,
//     schema/schema.sql).
//
// Scope:
//   1. cmdDaemonUpdate and cmdDaemonRollback contain a deferred call to
//      store.RecordDaemonUpdate.
//   2. The DaemonUpdateHistory and DaemonStartLog tables are defined in
//      all three schema locations.
package audittools

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPattern_P_DaemonUpdateHistory(t *testing.T) {
	root := moduleRoot(t)
	target := filepath.Join(root, "cmd", "force", "daemon_cmds.go")

	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, target, nil, parser.ParseComments)
	if err != nil {
		t.Fatalf("parse %s: %v", target, err)
	}

	// (1) Both cmdDaemonUpdate and cmdDaemonRollback must invoke
	//     store.RecordDaemonUpdate. We accept either a direct call or a
	//     deferred call (`defer func() { ... store.RecordDaemonUpdate(...) ... }()`).
	wantFuncs := map[string]bool{
		"cmdDaemonUpdate":   false,
		"cmdDaemonRollback": false,
	}
	ast.Inspect(file, func(n ast.Node) bool {
		fn, ok := n.(*ast.FuncDecl)
		if !ok || fn.Name == nil {
			return true
		}
		if _, want := wantFuncs[fn.Name.Name]; !want {
			return true
		}
		ast.Inspect(fn, func(inner ast.Node) bool {
			call, ok := inner.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			pkg, ok := sel.X.(*ast.Ident)
			if !ok {
				return true
			}
			if pkg.Name == "store" && sel.Sel.Name == "RecordDaemonUpdate" {
				wantFuncs[fn.Name.Name] = true
			}
			return true
		})
		return false
	})
	for name, found := range wantFuncs {
		if !found {
			t.Errorf("Pattern P_DaemonUpdateHistory: %s does not call store.RecordDaemonUpdate — operator-facing history will silently drop this exit path", name)
		}
	}

	// (2) Schema parity: DaemonUpdateHistory and DaemonStartLog must be
	//     declared in all three locations (createSchema, runMigrations,
	//     schema/schema.sql).
	schemaGo := mustReadAudit(t, filepath.Join(root, "internal", "store", "schema.go"))
	schemaSQL := mustReadAudit(t, filepath.Join(root, "schema", "schema.sql"))

	tables := []string{"DaemonUpdateHistory", "DaemonStartLog"}
	for _, table := range tables {
		// schema.go: the table must appear in BOTH createSchema and
		// runMigrations (heuristic: count `CREATE TABLE IF NOT EXISTS <table>`
		// occurrences — should be 2).
		needle := "CREATE TABLE IF NOT EXISTS " + table
		count := strings.Count(schemaGo, needle)
		if count < 2 {
			t.Errorf("Pattern P_DaemonUpdateHistory: %s appears %d time(s) in schema.go, want 2 (createSchema + runMigrations)",
				table, count)
		}
		// schema.sql: must appear at least once.
		if !strings.Contains(schemaSQL, needle) {
			t.Errorf("Pattern P_DaemonUpdateHistory: %s missing from schema/schema.sql", table)
		}
	}
}

func mustReadAudit(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}
