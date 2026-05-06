// Package audittools: Pattern P_DaemonSingleton — every code path that
// boots the long-running daemon (and therefore spawns agents) MUST
// first acquire the singleton lock at ~/.force/force.pid via
// internal/daemon/singleton.Acquire.
//
// Why an AST guard, not a runtime check?
//
//   - Adding a new daemon entry-point in cmd/force without going
//     through Acquire() would silently allow a second concurrent
//     daemon, and the only symptom (until the next crash) is two
//     agents fighting over the same Pending row.
//   - The flock semantics are correct EVERY time, but only if the
//     code path actually calls Acquire. The AST audit watches for
//     drift.
//
// Scope: cmdDaemon in cmd/force/fleet_cmds.go is the canonical entry
// point. If a new entry point lands (e.g. a `force daemon foreground-
// supervised` that wraps cmdDaemon), it MUST also call Acquire OR
// route through cmdDaemon (which already does). This test asserts:
//
//   1. cmd/force/fleet_cmds.go imports internal/daemon/singleton.
//   2. cmdDaemon (the function body) contains a call to
//      singleton.Acquire(...) BEFORE the first agents.Spawn* call.
package audittools

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"
)

func TestPattern_P_DaemonSingleton(t *testing.T) {
	root := moduleRoot(t)
	target := filepath.Join(root, "cmd", "force", "fleet_cmds.go")

	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, target, nil, parser.ParseComments)
	if err != nil {
		t.Fatalf("parse %s: %v", target, err)
	}

	// (1) singleton import present.
	wantImport := `"force-orchestrator/internal/daemon/singleton"`
	hasImport := false
	for _, imp := range file.Imports {
		if imp.Path.Value == wantImport {
			hasImport = true
			break
		}
	}
	if !hasImport {
		t.Fatalf("Pattern P_DaemonSingleton: cmd/force/fleet_cmds.go does not import %s — daemon entry would skip the flock guard", wantImport)
	}

	// (2) cmdDaemon body contains singleton.Acquire(...) BEFORE any
	// agents.Spawn* call. We walk the function body in source order
	// (positions are monotonic).
	var (
		acquirePos int
		firstSpawnPos int
		foundCmdDaemon bool
	)
	ast.Inspect(file, func(n ast.Node) bool {
		fn, ok := n.(*ast.FuncDecl)
		if !ok || fn.Name == nil || fn.Name.Name != "cmdDaemon" {
			return true
		}
		foundCmdDaemon = true
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
			pos := int(call.Pos())
			switch {
			case pkg.Name == "singleton" && sel.Sel.Name == "Acquire":
				if acquirePos == 0 || pos < acquirePos {
					acquirePos = pos
				}
			case pkg.Name == "agents" && strings.HasPrefix(sel.Sel.Name, "Spawn"):
				if firstSpawnPos == 0 || pos < firstSpawnPos {
					firstSpawnPos = pos
				}
			}
			return true
		})
		return false
	})

	if !foundCmdDaemon {
		t.Fatalf("Pattern P_DaemonSingleton: cmdDaemon function not found in %s", target)
	}
	if acquirePos == 0 {
		t.Fatalf("Pattern P_DaemonSingleton: cmdDaemon does not call singleton.Acquire(...) — second concurrent daemon would silently boot")
	}
	if firstSpawnPos == 0 {
		t.Fatalf("Pattern P_DaemonSingleton: cmdDaemon does not call agents.Spawn* — sanity check failed (this audit assumes the daemon spawns agents)")
	}
	if acquirePos >= firstSpawnPos {
		t.Fatalf("Pattern P_DaemonSingleton: singleton.Acquire (pos=%d) is AFTER first agents.Spawn* (pos=%d). Acquire must run BEFORE any agent goroutine starts, so a second daemon never gets to claim work.",
			acquirePos, firstSpawnPos)
	}
}
