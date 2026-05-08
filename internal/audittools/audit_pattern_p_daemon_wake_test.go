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
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// wakeRequiredFiles maps each required platform file under
// internal/daemon/wake/ to the build-tag substring that file must
// declare.
var wakeRequiredFiles = map[string]string{
	"wake_darwin.go":       "darwin",
	"wake_darwin_nocgo.go": "darwin",
	"wake_linux.go":        "linux",
	"wake_other.go":        "!darwin && !linux",
}

// checkDaemonWakeReconcile asserts the wake-reconcile contract holds
// for the source tree rooted at rootDir. Returns nil on success,
// otherwise the first violation as an error.
//
// Extracted from the production check so the sentinel can drive it
// with a synthetic TempDir.
func checkDaemonWakeReconcile(rootDir string) error {
	// (1) fleet_cmds.go: import + wake.Subscribe call inside cmdDaemon.
	fleetPath := filepath.Join(rootDir, "cmd", "force", "fleet_cmds.go")
	fset := token.NewFileSet()
	fleetFile, err := parser.ParseFile(fset, fleetPath, nil, parser.ParseComments)
	if err != nil {
		return fmt.Errorf("parse %s: %w", fleetPath, err)
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
		return fmt.Errorf("%s does not import %s — daemon would skip sleep/wake hooks", fleetPath, wantImport)
	}

	subscribeFound := false
	cmdDaemonFound := false
	ast.Inspect(fleetFile, func(n ast.Node) bool {
		fn, ok := n.(*ast.FuncDecl)
		if !ok || fn.Name == nil || fn.Name.Name != "cmdDaemon" {
			return true
		}
		cmdDaemonFound = true
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
	if !cmdDaemonFound {
		return fmt.Errorf("%s missing cmdDaemon function — sanity check failed", fleetPath)
	}
	if !subscribeFound {
		return fmt.Errorf("cmdDaemon does not call wake.Subscribe — daemon won't receive sleep/wake events")
	}

	// (2) daemon_wake.go must reference both event constants AND
	// reconcilePostWake.
	daemonWakePath := filepath.Join(rootDir, "cmd", "force", "daemon_wake.go")
	srcBytes, err := os.ReadFile(daemonWakePath)
	if err != nil {
		return fmt.Errorf("read %s: %w", daemonWakePath, err)
	}
	src := string(srcBytes)
	for _, want := range []string{"wake.GoingToSleep", "wake.Woke", "reconcilePostWake"} {
		if !strings.Contains(src, want) {
			return fmt.Errorf("%s does not reference %s — wake event routing or reconcile wiring is missing", daemonWakePath, want)
		}
	}

	// (3) internal/daemon/wake/ has all four required build-tagged files.
	wakeDir := filepath.Join(rootDir, "internal", "daemon", "wake")
	for fname, wantTag := range wakeRequiredFiles {
		path := filepath.Join(wakeDir, fname)
		body, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("missing platform file %s — multi-platform coverage is incomplete: %w", path, err)
		}
		head := string(body)
		if i := strings.Index(head, "\n\n"); i > 0 {
			head = head[:i]
		}
		if !strings.Contains(head, "//go:build") {
			return fmt.Errorf("%s does not contain a //go:build directive", path)
		}
		if !strings.Contains(head, wantTag) {
			return fmt.Errorf("%s build tag does not contain %q (head: %q)", path, wantTag, head)
		}
	}
	return nil
}

func TestPattern_P_DaemonWakeReconcile(t *testing.T) {
	if err := checkDaemonWakeReconcile(moduleRoot(t)); err != nil {
		t.Errorf("Pattern P_DaemonWakeReconcile: %v", err)
	}
}

// writeWakeCompliantTree builds a fully compliant wake/reconcile
// source tree under root. The sentinel uses this as the baseline and
// then mutates one file at a time.
func writeWakeCompliantTree(t *testing.T, root string) {
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
	mk("cmd/force/fleet_cmds.go", "package force\n"+
		"import \"force-orchestrator/internal/daemon/wake\"\n"+
		"func cmdDaemon() {\n"+
		"\twake.Subscribe(ctx)\n"+
		"}\n")
	mk("cmd/force/daemon_wake.go", "package force\n"+
		"// references wake.GoingToSleep and wake.Woke; calls reconcilePostWake\n"+
		"func handleEvents() {\n"+
		"\tswitch ev {\n"+
		"\tcase wake.GoingToSleep:\n"+
		"\t\t_ = ev\n"+
		"\tcase wake.Woke:\n"+
		"\t\treconcilePostWake()\n"+
		"\t}\n"+
		"}\n")
	mk("internal/daemon/wake/wake_darwin.go", "//go:build darwin && cgo\n\npackage wake\n")
	mk("internal/daemon/wake/wake_darwin_nocgo.go", "//go:build darwin && !cgo\n\npackage wake\n")
	mk("internal/daemon/wake/wake_linux.go", "//go:build linux\n\npackage wake\n")
	mk("internal/daemon/wake/wake_other.go", "//go:build !darwin && !linux\n\npackage wake\n")
}

// TestPattern_P_DaemonWakeReconcile_DetectsInjectedDrift proves the
// wake-reconcile checker would fire when each contract clause is
// dropped. We build a compliant TempDir baseline and mutate one
// clause at a time.
func TestPattern_P_DaemonWakeReconcile_DetectsInjectedDrift(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(t *testing.T, root string)
		wantSub string
	}{
		{
			name: "missing-wake-import",
			mutate: func(t *testing.T, root string) {
				p := filepath.Join(root, "cmd", "force", "fleet_cmds.go")
				body := "package force\nfunc cmdDaemon() {}\n"
				if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
					t.Fatal(err)
				}
			},
			wantSub: "does not import",
		},
		{
			name: "missing-wake-Subscribe-call",
			mutate: func(t *testing.T, root string) {
				p := filepath.Join(root, "cmd", "force", "fleet_cmds.go")
				body := "package force\nimport _ \"force-orchestrator/internal/daemon/wake\"\nfunc cmdDaemon() {}\n"
				if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
					t.Fatal(err)
				}
			},
			wantSub: "does not call wake.Subscribe",
		},
		{
			name: "missing-cmdDaemon",
			mutate: func(t *testing.T, root string) {
				p := filepath.Join(root, "cmd", "force", "fleet_cmds.go")
				body := "package force\nimport _ \"force-orchestrator/internal/daemon/wake\"\nfunc somethingElse() {}\n"
				if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
					t.Fatal(err)
				}
			},
			wantSub: "missing cmdDaemon function",
		},
		{
			name: "missing-GoingToSleep",
			mutate: func(t *testing.T, root string) {
				p := filepath.Join(root, "cmd", "force", "daemon_wake.go")
				body := "package force\n// references wake.Woke; calls reconcilePostWake\nfunc handleEvents() {\n\tif ev == wake.Woke {\n\t\treconcilePostWake()\n\t}\n}\n"
				if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
					t.Fatal(err)
				}
			},
			wantSub: "wake.GoingToSleep",
		},
		{
			name: "missing-reconcilePostWake",
			mutate: func(t *testing.T, root string) {
				p := filepath.Join(root, "cmd", "force", "daemon_wake.go")
				body := "package force\n// references wake.GoingToSleep, wake.Woke\nfunc handleEvents() {\n\tswitch ev {\n\tcase wake.GoingToSleep:\n\tcase wake.Woke:\n\t}\n}\n"
				if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
					t.Fatal(err)
				}
			},
			wantSub: "reconcilePostWake",
		},
		{
			name: "missing-platform-file-linux",
			mutate: func(t *testing.T, root string) {
				if err := os.Remove(filepath.Join(root, "internal", "daemon", "wake", "wake_linux.go")); err != nil {
					t.Fatal(err)
				}
			},
			wantSub: "missing platform file",
		},
		{
			name: "build-tag-missing-on-other",
			mutate: func(t *testing.T, root string) {
				p := filepath.Join(root, "internal", "daemon", "wake", "wake_other.go")
				body := "package wake\n" // no //go:build line
				if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
					t.Fatal(err)
				}
			},
			wantSub: "//go:build directive",
		},
		{
			name: "wrong-build-tag-on-darwin",
			mutate: func(t *testing.T, root string) {
				p := filepath.Join(root, "internal", "daemon", "wake", "wake_darwin.go")
				body := "//go:build linux\n\npackage wake\n"
				if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
					t.Fatal(err)
				}
			},
			wantSub: "build tag does not contain",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			writeWakeCompliantTree(t, root)
			tc.mutate(t, root)
			err := checkDaemonWakeReconcile(root)
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
	writeWakeCompliantTree(t, root)
	if err := checkDaemonWakeReconcile(root); err != nil {
		t.Fatalf("checker rejected compliant baseline: %v", err)
	}
}
