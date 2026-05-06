// Package audittools: Pattern P_DaemonWakeReconcile — D12 P2 sleep/wake
// survival.
//
// The daemon must subscribe to the platform-specific power-state
// notifier (IOKit on macOS, logind on Linux) at startup and route
// Woke events through reconcilePostWake. This audit catches drift
// where a refactor of fleet_cmds.go accidentally drops the wake
// hookup OR the reconcilePostWake wiring.
//
// Three checks:
//
//  1. cmd/force/fleet_cmds.go imports the wake package and calls
//     wake.Subscribe(ctx) somewhere inside cmdDaemon.
//  2. cmd/force/daemon_wake.go references wake.GoingToSleep AND
//     wake.Woke (so both events are routed) AND calls
//     reconcilePostWake (so the Woke branch actually reconciles).
//  3. internal/daemon/wake/ has all four required build-tagged files:
//     wake_darwin.go, wake_darwin_nocgo.go, wake_linux.go,
//     wake_other.go. Compile-time multi-platform coverage.
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

func TestPattern_P_DaemonWakeReconcile(t *testing.T) {
	root := moduleRoot(t)

	// (1) fleet_cmds.go: import + wake.Subscribe call inside cmdDaemon.
	fleetPath := filepath.Join(root, "cmd", "force", "fleet_cmds.go")
	fset := token.NewFileSet()
	fleetFile, err := parser.ParseFile(fset, fleetPath, nil, parser.ParseComments)
	if err != nil {
		t.Fatalf("parse %s: %v", fleetPath, err)
	}
	wantImport := `"force-orchestrator/internal/daemon/wake"`
	hasImport := false
	for _, imp := range fleetFile.Imports {
		if imp.Path.Value == wantImport {
			hasImport = true
			break
		}
	}
	if !hasImport {
		t.Fatalf("Pattern P_DaemonWakeReconcile: %s does not import %s — daemon would skip sleep/wake hooks", fleetPath, wantImport)
	}

	subscribeFound := false
	ast.Inspect(fleetFile, func(n ast.Node) bool {
		fn, ok := n.(*ast.FuncDecl)
		if !ok || fn.Name == nil || fn.Name.Name != "cmdDaemon" {
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
			if pkg.Name == "wake" && sel.Sel.Name == "Subscribe" {
				subscribeFound = true
			}
			return true
		})
		return false
	})
	if !subscribeFound {
		t.Fatalf("Pattern P_DaemonWakeReconcile: cmdDaemon does not call wake.Subscribe — daemon won't receive sleep/wake events")
	}

	// (2) daemon_wake.go must reference both event constants AND
	// reconcilePostWake. Source-level grep is sufficient — the file
	// is a thin glue layer and we don't need full AST walks.
	daemonWakePath := filepath.Join(root, "cmd", "force", "daemon_wake.go")
	srcBytes, err := os.ReadFile(daemonWakePath)
	if err != nil {
		t.Fatalf("read %s: %v", daemonWakePath, err)
	}
	src := string(srcBytes)
	for _, want := range []string{"wake.GoingToSleep", "wake.Woke", "reconcilePostWake"} {
		if !strings.Contains(src, want) {
			t.Errorf("Pattern P_DaemonWakeReconcile: %s does not reference %s — wake event routing or reconcile wiring is missing", daemonWakePath, want)
		}
	}

	// (3) internal/daemon/wake/ has all four required build-tagged
	// files. Each must exist AND contain a //go:build line that
	// matches the expected platform constraint.
	wakeDir := filepath.Join(root, "internal", "daemon", "wake")
	requiredFiles := map[string]string{
		"wake_darwin.go":       "darwin",
		"wake_darwin_nocgo.go": "darwin",
		"wake_linux.go":        "linux",
		"wake_other.go":        "!darwin && !linux",
	}
	for fname, wantTag := range requiredFiles {
		path := filepath.Join(wakeDir, fname)
		body, err := os.ReadFile(path)
		if err != nil {
			t.Errorf("Pattern P_DaemonWakeReconcile: missing platform file %s — multi-platform coverage is incomplete: %v", path, err)
			continue
		}
		// First non-blank line should be a //go:build directive
		// containing the expected tag.
		head := string(body)
		if i := strings.Index(head, "\n\n"); i > 0 {
			head = head[:i]
		}
		if !strings.Contains(head, "//go:build") {
			t.Errorf("Pattern P_DaemonWakeReconcile: %s does not contain a //go:build directive", path)
			continue
		}
		if !strings.Contains(head, wantTag) {
			t.Errorf("Pattern P_DaemonWakeReconcile: %s build tag does not contain %q (head: %q)", path, wantTag, head)
		}
	}
}
