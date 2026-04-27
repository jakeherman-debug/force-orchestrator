// Package audittools: pattern test for the cross-agent service
// interface invariant introduced in D0 (CLAUDE.md "Cross-agent service
// interfaces"). Pattern P16 enforces, at CI time, that:
//
//  1. Every package under internal/clients/<service>/ exports a type
//     named exactly `Client` and that type is an interface — never a
//     struct. This is the load-bearing piece: agents depend on the
//     interface, and that contract has to actually be an interface.
//
//  2. Production agent code (internal/agents/*.go, non-_test) never
//     constructs a concrete implementation by writing a composite
//     literal against a `*Client`-suffixed type from a clients/<svc>/
//     package. Examples that fail:
//       &librarian.MockClient{}
//       librarian.InProcessClient{db: db}
//       &capabilities.GRPCClient{...}
//     Construction MUST go through the package's exported factory
//     functions (NewInProcess / NewGRPC / NewShared / NewMock).
//
// The check is AST-based, not grep-based, so a comment that mentions
// the literal `&librarian.MockClient{...}` (e.g. in a doc-comment that
// describes what the rule forbids) is fine; only real composite
// literals trip the test.
//
// Pattern P16 graduates to a BoS commit-time rule when D4 ships; until
// then, this test is the only enforcement.
package audittools

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// clientsPkgPrefix is the import-path prefix every Client interface
// lives under. Discovery walks `<root>/internal/clients/` and treats
// every direct subdirectory as a service package whose exported
// `Client` type must be an interface.
const clientsPkgPrefix = "force-orchestrator/internal/clients/"

// p16AgentDir is the production-code subtree the second half of the
// check walks. Other internal/* subtrees may legitimately construct
// concrete clients (cmd/force daemon wiring, dashboard one-shot dog
// runs) — the rule is scoped to agents because they're the ones P16
// is ultimately protecting.
const p16AgentDir = "internal/agents"

// TestPattern_P16_ClientsInterfaces is the D0 / Pattern P16 regression.
// Fails if any clients/<svc>/ exports `Client` as a struct, or if any
// agent file constructs a `*Client` type from a clients/<svc>/ via a
// composite literal.
func TestPattern_P16_ClientsInterfaces(t *testing.T) {
	root := moduleRoot(t)

	// ── Phase 1: every clients/<svc>/ exports `Client` as an interface.
	clientsRoot := filepath.Join(root, "internal", "clients")
	services, err := listClientServices(clientsRoot)
	if err != nil {
		t.Fatalf("list clients/<svc>/: %v", err)
	}
	if len(services) == 0 {
		t.Fatalf("no clients/<svc>/ packages found at %s — D0-A may be missing", clientsRoot)
	}

	clientsPkgImportPaths := make(map[string]struct{}, len(services))
	for _, svc := range services {
		clientsPkgImportPaths[clientsPkgPrefix+svc] = struct{}{}

		typ, file, line, found := findExportedClientType(t, filepath.Join(clientsRoot, svc))
		if !found {
			t.Errorf("clients/%s: no exported `Client` type found — every clients/<svc>/ MUST declare `type Client interface{...}`", svc)
			continue
		}
		if _, ok := typ.(*ast.InterfaceType); !ok {
			t.Errorf("clients/%s: exported `Client` is %T at %s:%d — Pattern P16 requires it to be an interface", svc, typ, rel(root, file), line)
		}
	}

	// ── Phase 2: no agent file constructs a *Client type from a
	// clients/<svc>/ package via a composite literal.
	agentRoot := filepath.Join(root, p16AgentDir)
	type offence struct {
		File     string
		Line     int
		PkgAlias string
		TypeName string
	}
	var offences []offence

	walkErr := filepath.WalkDir(agentRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		fset := token.NewFileSet()
		f, parseErr := parser.ParseFile(fset, path, nil, parser.ImportsOnly|parser.ParseComments)
		if parseErr != nil {
			return nil // skip unparseable files; other tests will surface the parse error
		}

		// Map import name → import path. Track only clients/<svc>/ imports.
		clientsImports := map[string]string{} // local alias → service name
		for _, imp := range f.Imports {
			path := strings.Trim(imp.Path.Value, `"`)
			if !strings.HasPrefix(path, clientsPkgPrefix) {
				continue
			}
			svc := strings.TrimPrefix(path, clientsPkgPrefix)
			alias := svc
			if imp.Name != nil {
				alias = imp.Name.Name
			} else {
				// Default alias is the package name; for our purposes
				// the directory basename matches the package name.
				alias = svc
			}
			clientsImports[alias] = svc
		}
		if len(clientsImports) == 0 {
			return nil
		}

		// Re-parse with full bodies so we can walk composite literals.
		full, parseErr := parser.ParseFile(fset, path, nil, 0)
		if parseErr != nil {
			return nil
		}

		ast.Inspect(full, func(n ast.Node) bool {
			cl, ok := n.(*ast.CompositeLit)
			if !ok {
				return true
			}
			sel, ok := cl.Type.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			pkgIdent, ok := sel.X.(*ast.Ident)
			if !ok {
				return true
			}
			svc, isClientsPkg := clientsImports[pkgIdent.Name]
			if !isClientsPkg {
				return true
			}
			// Forbid TypeName ending in "Client" (covers
			// InProcessClient, MockClient, GRPCClient, SharedClient,
			// and any future *Client concrete types).
			if !strings.HasSuffix(sel.Sel.Name, "Client") {
				return true
			}
			pos := fset.Position(cl.Pos())
			offences = append(offences, offence{
				File:     rel(root, path),
				Line:     pos.Line,
				PkgAlias: pkgIdent.Name,
				TypeName: sel.Sel.Name,
			})
			_ = svc
			return true
		})
		return nil
	})
	if walkErr != nil {
		t.Fatalf("walk %s: %v", agentRoot, walkErr)
	}

	if len(offences) == 0 {
		return
	}
	sort.Slice(offences, func(i, j int) bool {
		if offences[i].File != offences[j].File {
			return offences[i].File < offences[j].File
		}
		return offences[i].Line < offences[j].Line
	})
	t.Errorf("Pattern P16 (D0): %d agent file(s) construct a concrete client struct from internal/clients/<svc>/. Use the package's NewInProcess / NewGRPC / NewMock factory function instead — agents depend on the interface, never on the implementation type:", len(offences))
	for _, o := range offences {
		t.Errorf("  %s:%d — &%s.%s{...}  (or %s.%s{...})", o.File, o.Line, o.PkgAlias, o.TypeName, o.PkgAlias, o.TypeName)
	}
}

// listClientServices returns every direct subdirectory under
// internal/clients/ that contains at least one non-_test .go file.
// Used as the authoritative list of service packages Pattern P16
// inspects. Empty placeholder directories (no .go files) are skipped
// so a future "scaffolded but not yet populated" service doesn't
// trip the test.
func listClientServices(clientsRoot string) ([]string, error) {
	entries, err := os.ReadDir(clientsRoot)
	if err != nil {
		return nil, err
	}
	var services []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		matches, _ := filepath.Glob(filepath.Join(clientsRoot, e.Name(), "*.go"))
		hasGo := false
		for _, m := range matches {
			if !strings.HasSuffix(m, "_test.go") {
				hasGo = true
				break
			}
		}
		if !hasGo {
			continue
		}
		services = append(services, e.Name())
	}
	sort.Strings(services)
	return services, nil
}


// findExportedClientType parses every .go file in the package directory
// (skipping _test.go) and returns the TypeSpec for the exported type
// named exactly "Client". Returns (nil, "", 0, false) if no such type
// is declared.
func findExportedClientType(t *testing.T, pkgDir string) (ast.Expr, string, int, bool) {
	t.Helper()
	fset := token.NewFileSet()
	matches, err := filepath.Glob(filepath.Join(pkgDir, "*.go"))
	if err != nil {
		return nil, "", 0, false
	}
	for _, path := range matches {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			continue
		}
		for _, decl := range f.Decls {
			gd, ok := decl.(*ast.GenDecl)
			if !ok || gd.Tok != token.TYPE {
				continue
			}
			for _, spec := range gd.Specs {
				ts, ok := spec.(*ast.TypeSpec)
				if !ok {
					continue
				}
				if ts.Name.Name != "Client" {
					continue
				}
				pos := fset.Position(ts.Pos())
				return ts.Type, path, pos.Line, true
			}
		}
	}
	return nil, "", 0, false
}
