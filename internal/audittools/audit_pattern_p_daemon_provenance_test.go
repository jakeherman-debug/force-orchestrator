// Package audittools: Pattern P_DaemonProvenance — the build-time
// provenance vars MUST be wired correctly so the operator (and the
// trust file) can audit which binary is running.
//
// Three checks:
//
//  1. cmd/force/main.go declares package-level string vars GitSHA,
//     BuildTime, GitBranch, each defaulting to "unknown".
//
//  2. The Makefile's `build` target includes -ldflags that pass
//     -X main.GitSHA, -X main.BuildTime, -X main.GitBranch.
//
//  3. cmd/force/main.go calls provenance.Set(...) from init() so
//     non-main code (dashboard, daemon status, etc.) can read the
//     values without importing main.
//
// We don't shell out to `make build` from the test (that would
// require make + go in the test env and would dirty the worktree).
// Instead we treat the Makefile + main.go as the contract source.
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

func TestPattern_P_DaemonProvenance(t *testing.T) {
	root := moduleRoot(t)

	// ── Check 1: main.go vars ─────────────────────────────────────────
	mainPath := filepath.Join(root, "cmd", "force", "main.go")
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, mainPath, nil, parser.ParseComments)
	if err != nil {
		t.Fatalf("parse %s: %v", mainPath, err)
	}
	wantVars := map[string]bool{
		"GitSHA":    false,
		"BuildTime": false,
		"GitBranch": false,
	}
	for _, decl := range file.Decls {
		gd, ok := decl.(*ast.GenDecl)
		if !ok || gd.Tok != token.VAR {
			continue
		}
		for _, spec := range gd.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			for _, name := range vs.Names {
				if _, watched := wantVars[name.Name]; watched {
					wantVars[name.Name] = true
				}
			}
		}
	}
	for name, found := range wantVars {
		if !found {
			t.Errorf("Pattern P_DaemonProvenance: cmd/force/main.go missing package-level var %s — `force version` won't surface build provenance", name)
		}
	}

	// ── Check 2: Makefile -ldflags ────────────────────────────────────
	mkPath := filepath.Join(root, "Makefile")
	mkBytes, err := os.ReadFile(mkPath)
	if err != nil {
		t.Fatalf("read Makefile: %v", err)
	}
	mk := string(mkBytes)
	for _, want := range []string{
		"-X main.GitSHA",
		"-X main.BuildTime",
		"-X main.GitBranch",
	} {
		if !strings.Contains(mk, want) {
			t.Errorf("Pattern P_DaemonProvenance: Makefile missing %q in -ldflags — `make build` will produce a binary with default 'unknown' provenance", want)
		}
	}
	// The build target must reference $(LDFLAGS).
	if !strings.Contains(mk, "$(LDFLAGS)") {
		t.Errorf("Pattern P_DaemonProvenance: Makefile defines LDFLAGS but the build target doesn't reference $(LDFLAGS)")
	}

	// ── Check 3: provenance.Set wired via init() ──────────────────────
	hasInitWithProvenanceSet := false
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Name == nil || fn.Name.Name != "init" || fn.Recv != nil {
			continue
		}
		ast.Inspect(fn, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
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
			if pkg.Name == "provenance" && sel.Sel.Name == "Set" {
				hasInitWithProvenanceSet = true
			}
			return true
		})
	}
	if !hasInitWithProvenanceSet {
		t.Errorf("Pattern P_DaemonProvenance: cmd/force/main.go does not call provenance.Set(...) from init() — non-main packages can't read GitSHA/BuildTime/GitBranch")
	}
}
