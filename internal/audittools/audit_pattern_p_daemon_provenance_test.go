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
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// checkProvenanceMainGo parses src as Go source representing
// cmd/force/main.go and returns nil iff (a) all three package-level
// vars (GitSHA, BuildTime, GitBranch) are declared AND (b) init()
// calls provenance.Set(...). If src == "" the file at srcName is
// read from disk.
func checkProvenanceMainGo(srcName, src string) error {
	fset := token.NewFileSet()
	var parseSrc interface{}
	if src != "" {
		parseSrc = src
	}
	file, err := parser.ParseFile(fset, srcName, parseSrc, parser.ParseComments)
	if err != nil {
		return fmt.Errorf("parse %s: %w", srcName, err)
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
	var missingVars []string
	for name, found := range wantVars {
		if !found {
			missingVars = append(missingVars, name)
		}
	}
	if len(missingVars) > 0 {
		return fmt.Errorf("%s missing package-level var(s) %v — `force version` won't surface build provenance", srcName, missingVars)
	}

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
		return fmt.Errorf("%s does not call provenance.Set(...) from init() — non-main packages can't read GitSHA/BuildTime/GitBranch", srcName)
	}
	return nil
}

// checkProvenanceMakefile asserts the Makefile body contains the
// three -X main.* ldflag entries and references $(LDFLAGS) somewhere.
// Returns nil on success.
func checkProvenanceMakefile(mk string) error {
	for _, want := range []string{
		"-X main.GitSHA",
		"-X main.BuildTime",
		"-X main.GitBranch",
	} {
		if !strings.Contains(mk, want) {
			return fmt.Errorf("Makefile missing %q in -ldflags — `make build` will produce a binary with default 'unknown' provenance", want)
		}
	}
	if !strings.Contains(mk, "$(LDFLAGS)") {
		return fmt.Errorf("Makefile defines LDFLAGS but the build target doesn't reference $(LDFLAGS)")
	}
	return nil
}

func TestPattern_P_DaemonProvenance(t *testing.T) {
	root := moduleRoot(t)

	mainPath := filepath.Join(root, "cmd", "force", "main.go")
	if err := checkProvenanceMainGo(mainPath, ""); err != nil {
		t.Errorf("Pattern P_DaemonProvenance: %v", err)
	}

	mkPath := filepath.Join(root, "Makefile")
	mkBytes, err := os.ReadFile(mkPath)
	if err != nil {
		t.Fatalf("read Makefile: %v", err)
	}
	if err := checkProvenanceMakefile(string(mkBytes)); err != nil {
		t.Errorf("Pattern P_DaemonProvenance: %v", err)
	}
}

// TestPattern_P_DaemonProvenance_DetectsInjectedDrift proves both
// the main.go AST check and the Makefile literal check would actually
// fire if a future refactor regressed either side. We feed each
// sub-checker synthetic input that violates a single contract clause
// and assert it returns the expected error.
func TestPattern_P_DaemonProvenance_DetectsInjectedDrift(t *testing.T) {
	t.Run("main-missing-var", func(t *testing.T) {
		// Drop GitBranch.
		src := `package main
import _ "force-orchestrator/internal/provenance"
var (
	GitSHA    = "unknown"
	BuildTime = "unknown"
)
func init() { provenance.Set(GitSHA, BuildTime, "") }
func main() {}
`
		err := checkProvenanceMainGo("synthetic-main.go", src)
		if err == nil || !strings.Contains(err.Error(), "GitBranch") {
			t.Fatalf("checker accepted main.go missing GitBranch; err=%v", err)
		}
	})

	t.Run("main-missing-provenance-set", func(t *testing.T) {
		src := `package main
var (
	GitSHA    = "unknown"
	BuildTime = "unknown"
	GitBranch = "unknown"
)
func main() {}
`
		err := checkProvenanceMainGo("synthetic-main.go", src)
		if err == nil || !strings.Contains(err.Error(), "provenance.Set") {
			t.Fatalf("checker accepted main.go without init() provenance.Set; err=%v", err)
		}
	})

	t.Run("makefile-missing-ldflag", func(t *testing.T) {
		// Missing -X main.GitBranch.
		mk := `LDFLAGS = -X main.GitSHA=$(SHA) -X main.BuildTime=$(NOW)
build:
	go build $(LDFLAGS) -o force .
`
		err := checkProvenanceMakefile(mk)
		if err == nil || !strings.Contains(err.Error(), "main.GitBranch") {
			t.Fatalf("checker accepted Makefile missing -X main.GitBranch; err=%v", err)
		}
	})

	t.Run("makefile-missing-ldflags-ref", func(t *testing.T) {
		mk := `LDFLAGS = -X main.GitSHA=x -X main.BuildTime=y -X main.GitBranch=z
build:
	go build -o force .
`
		err := checkProvenanceMakefile(mk)
		if err == nil || !strings.Contains(err.Error(), "$(LDFLAGS)") {
			t.Fatalf("checker accepted Makefile not referencing $(LDFLAGS); err=%v", err)
		}
	})

	t.Run("positive-control", func(t *testing.T) {
		goodMain := `package main
var (
	GitSHA    = "unknown"
	BuildTime = "unknown"
	GitBranch = "unknown"
)
func init() { provenance.Set(GitSHA, BuildTime, GitBranch) }
func main() {}
`
		if err := checkProvenanceMainGo("synthetic-main.go", goodMain); err != nil {
			t.Fatalf("checker rejected compliant main.go: %v", err)
		}
		goodMk := `LDFLAGS = -X main.GitSHA=x -X main.BuildTime=y -X main.GitBranch=z
build:
	go build $(LDFLAGS) -o force .
`
		if err := checkProvenanceMakefile(goodMk); err != nil {
			t.Fatalf("checker rejected compliant Makefile: %v", err)
		}
	})
}
