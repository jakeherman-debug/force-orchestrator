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
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"
)

// checkDaemonSingleton parses src as Go source, locates cmdDaemon,
// and verifies (a) the singleton import is present and (b) cmdDaemon
// calls singleton.Acquire BEFORE the first agents.Spawn*. Returns nil
// when the contract holds; returns a descriptive error otherwise.
//
// If src == "" the file at srcName is read from disk (production path);
// if src != "" it is parsed verbatim (synthetic-input sentinel path).
//
// Extracted from the production check so the sentinel can drive it
// with synthetic source.
func checkDaemonSingleton(srcName, src string) error {
	fset := token.NewFileSet()
	var parseSrc interface{}
	if src != "" {
		parseSrc = src
	}
	file, err := parser.ParseFile(fset, srcName, parseSrc, parser.ParseComments)
	if err != nil {
		return fmt.Errorf("parse %s: %w", srcName, err)
	}

	wantImport := `"force-orchestrator/internal/daemon/singleton"`
	hasImport := false
	for _, imp := range file.Imports {
		if imp.Path.Value == wantImport {
			hasImport = true
			break
		}
	}
	if !hasImport {
		return fmt.Errorf("%s does not import %s — daemon entry would skip the flock guard", srcName, wantImport)
	}

	var (
		acquirePos     int
		firstSpawnPos  int
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
		return fmt.Errorf("cmdDaemon function not found in %s", srcName)
	}
	if acquirePos == 0 {
		return fmt.Errorf("cmdDaemon does not call singleton.Acquire(...) — second concurrent daemon would silently boot")
	}
	if firstSpawnPos == 0 {
		return fmt.Errorf("cmdDaemon does not call agents.Spawn* — sanity check failed (this audit assumes the daemon spawns agents)")
	}
	if acquirePos >= firstSpawnPos {
		return fmt.Errorf("singleton.Acquire (pos=%d) is AFTER first agents.Spawn* (pos=%d). Acquire must run BEFORE any agent goroutine starts, so a second daemon never gets to claim work.",
			acquirePos, firstSpawnPos)
	}
	return nil
}

func TestPattern_P_DaemonSingleton(t *testing.T) {
	root := moduleRoot(t)
	target := filepath.Join(root, "cmd", "force", "fleet_cmds.go")
	if err := checkDaemonSingleton(target, ""); err != nil {
		t.Fatalf("Pattern P_DaemonSingleton: %v", err)
	}
}

// TestPattern_P_DaemonSingleton_DetectsInjectedDrift proves the
// AST-based singleton check would actually fire if a future refactor
// dropped singleton.Acquire from cmdDaemon. We feed checkDaemonSingleton
// synthetic source that violates each branch of the contract and
// assert it rejects each one.
func TestPattern_P_DaemonSingleton_DetectsInjectedDrift(t *testing.T) {
	cases := []struct {
		name    string
		src     string
		wantSub string
	}{
		{
			name: "missing-singleton-import",
			src: `package force
import "force-orchestrator/internal/agents"
func cmdDaemon() { agents.SpawnAstromech(nil) }
`,
			wantSub: "does not import",
		},
		{
			name: "missing-cmdDaemon",
			src: `package force
import _ "force-orchestrator/internal/daemon/singleton"
func somethingElse() {}
`,
			wantSub: "cmdDaemon function not found",
		},
		{
			name: "no-acquire-call",
			src: `package force
import (
	_ "force-orchestrator/internal/daemon/singleton"
	"force-orchestrator/internal/agents"
)
func cmdDaemon() { agents.SpawnAstromech(nil) }
`,
			wantSub: "does not call singleton.Acquire",
		},
		{
			name: "no-spawn-call",
			src: `package force
import "force-orchestrator/internal/daemon/singleton"
func cmdDaemon() { singleton.Acquire("") }
`,
			wantSub: "does not call agents.Spawn",
		},
		{
			name: "acquire-after-spawn",
			src: `package force
import (
	"force-orchestrator/internal/daemon/singleton"
	"force-orchestrator/internal/agents"
)
func cmdDaemon() {
	agents.SpawnAstromech(nil)
	singleton.Acquire("")
}
`,
			wantSub: "is AFTER first agents.Spawn",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := checkDaemonSingleton("synthetic.go", tc.src)
			if err == nil {
				t.Fatalf("checker accepted a violating source; want failure containing %q", tc.wantSub)
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("error %q does not contain %q", err.Error(), tc.wantSub)
			}
		})
	}

	// Positive control: a minimal compliant source should pass.
	good := `package force
import (
	"force-orchestrator/internal/daemon/singleton"
	"force-orchestrator/internal/agents"
)
func cmdDaemon() {
	singleton.Acquire("")
	agents.SpawnAstromech(nil)
}
`
	if err := checkDaemonSingleton("synthetic.go", good); err != nil {
		t.Fatalf("checker rejected compliant source: %v", err)
	}
}
