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
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// updateHistoryRequiredTables is the set of tables that must be
// declared in all three schema locations.
var updateHistoryRequiredTables = []string{"DaemonUpdateHistory", "DaemonStartLog"}

// checkDaemonUpdateHistory asserts the update-history contract holds
// for the source tree rooted at rootDir. Returns nil on success,
// otherwise the first violation as an error.
func checkDaemonUpdateHistory(rootDir string) error {
	target := filepath.Join(rootDir, "cmd", "force", "daemon_cmds.go")
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, target, nil, parser.ParseComments)
	if err != nil {
		return fmt.Errorf("parse %s: %w", target, err)
	}

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
			return fmt.Errorf("%s does not call store.RecordDaemonUpdate — operator-facing history will silently drop this exit path", name)
		}
	}

	schemaGoBytes, err := os.ReadFile(filepath.Join(rootDir, "internal", "store", "schema.go"))
	if err != nil {
		return fmt.Errorf("read schema.go: %w", err)
	}
	schemaSQLBytes, err := os.ReadFile(filepath.Join(rootDir, "schema", "schema.sql"))
	if err != nil {
		return fmt.Errorf("read schema.sql: %w", err)
	}
	schemaGo := string(schemaGoBytes)
	schemaSQL := string(schemaSQLBytes)

	for _, table := range updateHistoryRequiredTables {
		needle := "CREATE TABLE IF NOT EXISTS " + table
		count := strings.Count(schemaGo, needle)
		if count < 2 {
			return fmt.Errorf("%s appears %d time(s) in schema.go, want 2 (createSchema + runMigrations)",
				table, count)
		}
		if !strings.Contains(schemaSQL, needle) {
			return fmt.Errorf("%s missing from schema/schema.sql", table)
		}
	}
	return nil
}

func TestPattern_P_DaemonUpdateHistory(t *testing.T) {
	if err := checkDaemonUpdateHistory(moduleRoot(t)); err != nil {
		t.Errorf("Pattern P_DaemonUpdateHistory: %v", err)
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

// writeUpdateHistoryCompliantTree builds a fully compliant
// daemon-update-history source tree under root.
func writeUpdateHistoryCompliantTree(t *testing.T, root string) {
	t.Helper()
	mk := func(rel, body string) {
		full := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", full, err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatalf("write %s: %v", full, err)
		}
	}
	mk("cmd/force/daemon_cmds.go", "package force\n"+
		"import _ \"force-orchestrator/internal/store\"\n"+
		"func cmdDaemonUpdate() {\n"+
		"\tdefer store.RecordDaemonUpdate(nil)\n"+
		"}\n"+
		"func cmdDaemonRollback() {\n"+
		"\tdefer store.RecordDaemonUpdate(nil)\n"+
		"}\n")
	// schema.go: TWO occurrences each (createSchema + runMigrations).
	mk("internal/store/schema.go", "package store\n"+
		"const createSchemaSQL = \"\\nCREATE TABLE IF NOT EXISTS DaemonUpdateHistory (id INTEGER PRIMARY KEY);\\nCREATE TABLE IF NOT EXISTS DaemonStartLog (id INTEGER PRIMARY KEY);\\n\"\n"+
		"const migrationsSQL = \"\\nCREATE TABLE IF NOT EXISTS DaemonUpdateHistory (id INTEGER PRIMARY KEY);\\nCREATE TABLE IF NOT EXISTS DaemonStartLog (id INTEGER PRIMARY KEY);\\n\"\n")
	mk("schema/schema.sql", "CREATE TABLE IF NOT EXISTS DaemonUpdateHistory (id INTEGER PRIMARY KEY);\n"+
		"CREATE TABLE IF NOT EXISTS DaemonStartLog (id INTEGER PRIMARY KEY);\n")
}

// TestPattern_P_DaemonUpdateHistory_DetectsInjectedDrift proves the
// update-history checker would fire when each contract clause is
// dropped.
func TestPattern_P_DaemonUpdateHistory_DetectsInjectedDrift(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(t *testing.T, root string)
		wantSub string
	}{
		{
			name: "missing-RecordDaemonUpdate-in-update",
			mutate: func(t *testing.T, root string) {
				p := filepath.Join(root, "cmd", "force", "daemon_cmds.go")
				body := "package force\n" +
					"import _ \"force-orchestrator/internal/store\"\n" +
					"func cmdDaemonUpdate() {}\n" +
					"func cmdDaemonRollback() {\n" +
					"\tdefer store.RecordDaemonUpdate(nil)\n" +
					"}\n"
				if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
					t.Fatal(err)
				}
			},
			wantSub: "cmdDaemonUpdate does not call store.RecordDaemonUpdate",
		},
		{
			name: "missing-RecordDaemonUpdate-in-rollback",
			mutate: func(t *testing.T, root string) {
				p := filepath.Join(root, "cmd", "force", "daemon_cmds.go")
				body := "package force\n" +
					"import _ \"force-orchestrator/internal/store\"\n" +
					"func cmdDaemonUpdate() {\n" +
					"\tdefer store.RecordDaemonUpdate(nil)\n" +
					"}\n" +
					"func cmdDaemonRollback() {}\n"
				if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
					t.Fatal(err)
				}
			},
			wantSub: "cmdDaemonRollback does not call store.RecordDaemonUpdate",
		},
		{
			name: "schema-go-only-one-DaemonUpdateHistory",
			mutate: func(t *testing.T, root string) {
				p := filepath.Join(root, "internal", "store", "schema.go")
				body := "package store\n" +
					"const createSchemaSQL = \"\\nCREATE TABLE IF NOT EXISTS DaemonUpdateHistory (id INTEGER PRIMARY KEY);\\nCREATE TABLE IF NOT EXISTS DaemonStartLog (id INTEGER PRIMARY KEY);\\n\"\n" +
					"const migrationsSQL = \"\\nCREATE TABLE IF NOT EXISTS DaemonStartLog (id INTEGER PRIMARY KEY);\\n\"\n"
				if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
					t.Fatal(err)
				}
			},
			wantSub: "DaemonUpdateHistory appears 1 time(s) in schema.go",
		},
		{
			name: "schema-go-only-one-DaemonStartLog",
			mutate: func(t *testing.T, root string) {
				p := filepath.Join(root, "internal", "store", "schema.go")
				body := "package store\n" +
					"const createSchemaSQL = \"\\nCREATE TABLE IF NOT EXISTS DaemonUpdateHistory (id INTEGER PRIMARY KEY);\\nCREATE TABLE IF NOT EXISTS DaemonStartLog (id INTEGER PRIMARY KEY);\\n\"\n" +
					"const migrationsSQL = \"\\nCREATE TABLE IF NOT EXISTS DaemonUpdateHistory (id INTEGER PRIMARY KEY);\\n\"\n"
				if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
					t.Fatal(err)
				}
			},
			wantSub: "DaemonStartLog appears 1 time(s) in schema.go",
		},
		{
			name: "schema-sql-missing-DaemonUpdateHistory",
			mutate: func(t *testing.T, root string) {
				p := filepath.Join(root, "schema", "schema.sql")
				body := "CREATE TABLE IF NOT EXISTS DaemonStartLog (id INTEGER PRIMARY KEY);\n"
				if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
					t.Fatal(err)
				}
			},
			wantSub: "DaemonUpdateHistory missing from schema/schema.sql",
		},
		{
			name: "schema-sql-missing-DaemonStartLog",
			mutate: func(t *testing.T, root string) {
				p := filepath.Join(root, "schema", "schema.sql")
				body := "CREATE TABLE IF NOT EXISTS DaemonUpdateHistory (id INTEGER PRIMARY KEY);\n"
				if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
					t.Fatal(err)
				}
			},
			wantSub: "DaemonStartLog missing from schema/schema.sql",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			writeUpdateHistoryCompliantTree(t, root)
			tc.mutate(t, root)
			err := checkDaemonUpdateHistory(root)
			if err == nil {
				t.Fatalf("checker accepted violating tree (case %q); want failure containing %q", tc.name, tc.wantSub)
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("error %q does not contain %q", err.Error(), tc.wantSub)
			}
		})
	}

	// Positive control.
	root := t.TempDir()
	writeUpdateHistoryCompliantTree(t, root)
	if err := checkDaemonUpdateHistory(root); err != nil {
		t.Fatalf("checker rejected compliant baseline: %v", err)
	}
}
