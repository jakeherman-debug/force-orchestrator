// Package audittools: Pattern P_DaemonCrashBudget — every daemon entry
// point must consult store.RecentStartCount BEFORE spawning agents and
// terminate the process (log.Fatalf / os.Exit) when the threshold is
// breached. The launchd plist + systemd unit must declare the correct
// auto-restart contract (Crashed=true / SuccessfulExit=false / Restart=
// on-failure) so a crashed daemon is restarted but a clean exit is not.
//
// Why an AST guard, not a runtime check?
//
//   - Adding a new daemon entry-point (or refactoring fleet_cmds.go) and
//     forgetting to invoke RecentStartCount silently regresses the
//     crash-loop guard — launchd / systemd will happily restart a
//     broken binary forever.
//   - launchd's KeepAlive contract is "auto-restart on Crashed=true";
//     dropping the SuccessfulExit=false key would cause the daemon to
//     auto-restart even after `force daemon stop` succeeded.
//
// Scope:
//   1. cmd/force/fleet_cmds.go::cmdDaemon contains a call to
//      store.RecentStartCount BEFORE the first agents.Spawn*.
//   2. The same function contains os.Exit(...) (or equivalent) on the
//      threshold-breach path.
//   3. launchdPlistTemplate emits the Crashed + SuccessfulExit keys.
//   4. systemdUnitTemplate emits Restart=on-failure + RestartSec=5.
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

// checkDaemonCrashBudget asserts the crash-budget contract holds for
// the source tree rooted at rootDir. It expects the same two files
// the production check cares about.
//
// Returns nil when every contract clause is satisfied. Returns the
// first violation found otherwise.
func checkDaemonCrashBudget(rootDir string) error {
	target := filepath.Join(rootDir, "cmd", "force", "fleet_cmds.go")

	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, target, nil, parser.ParseComments)
	if err != nil {
		return fmt.Errorf("parse %s: %w", target, err)
	}

	var (
		recentStartPos int
		firstSpawnPos  int
		exitOnBreachOk bool
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
			case pkg.Name == "store" && sel.Sel.Name == "RecentStartCount":
				if recentStartPos == 0 || pos < recentStartPos {
					recentStartPos = pos
				}
			case pkg.Name == "agents" && strings.HasPrefix(sel.Sel.Name, "Spawn"):
				if firstSpawnPos == 0 || pos < firstSpawnPos {
					firstSpawnPos = pos
				}
			case pkg.Name == "os" && sel.Sel.Name == "Exit":
				exitOnBreachOk = true
			case pkg.Name == "log" && (sel.Sel.Name == "Fatalf" || sel.Sel.Name == "Fatal" || sel.Sel.Name == "Fatalln"):
				exitOnBreachOk = true
			}
			return true
		})
		return false
	})

	if !foundCmdDaemon {
		return fmt.Errorf("cmdDaemon function not found in %s", target)
	}
	if recentStartPos == 0 {
		return fmt.Errorf("cmdDaemon does not call store.RecentStartCount(...) — crash-budget guard is missing; a broken binary would chew CPU forever via launchd/systemd auto-restart")
	}
	if firstSpawnPos == 0 {
		return fmt.Errorf("cmdDaemon does not call agents.Spawn* — sanity check failed")
	}
	if recentStartPos >= firstSpawnPos {
		return fmt.Errorf("store.RecentStartCount (pos=%d) is AFTER first agents.Spawn* (pos=%d). The crash-budget check MUST run before any agent goroutine starts.",
			recentStartPos, firstSpawnPos)
	}
	if !exitOnBreachOk {
		return fmt.Errorf("cmdDaemon does not call os.Exit / log.Fatal — the crash-budget breach path must terminate the process, not just log a warning")
	}

	// (3) launchd plist + (4) systemd unit. We grep the template
	//     functions' string output via the templates themselves: read
	//     daemon_cmds.go as text and assert the literal keys are present
	//     inside the launchd / systemd template bodies.
	cmdsPath := filepath.Join(rootDir, "cmd", "force", "daemon_cmds.go")
	body, rerr := os.ReadFile(cmdsPath)
	if rerr != nil {
		return fmt.Errorf("read %s: %w", cmdsPath, rerr)
	}
	src := string(body)

	plistRequired := []string{
		"<key>Crashed</key>",
		"<key>SuccessfulExit</key>",
		"<true/>",
		"<false/>",
	}
	for _, want := range plistRequired {
		if !strings.Contains(src, want) {
			return fmt.Errorf("launchdPlistTemplate must contain %q — auto-restart contract incomplete", want)
		}
	}
	systemdRequired := []string{
		"Restart=on-failure",
		"RestartSec=5",
	}
	for _, want := range systemdRequired {
		if !strings.Contains(src, want) {
			return fmt.Errorf("systemdUnitTemplate must contain %q — auto-restart contract incomplete", want)
		}
	}
	return nil
}

func TestPattern_P_DaemonCrashBudget(t *testing.T) {
	if err := checkDaemonCrashBudget(moduleRoot(t)); err != nil {
		t.Errorf("Pattern P_DaemonCrashBudget: %v", err)
	}
}

// TestPattern_P_DaemonCrashBudget_DetectsInjectedDrift proves the
// crash-budget checker would fire when each contract clause is
// dropped. We build a synthetic two-file tree under t.TempDir() and
// mutate one clause at a time.
func TestPattern_P_DaemonCrashBudget_DetectsInjectedDrift(t *testing.T) {
	// Compliant baseline — RecentStartCount before Spawn*, os.Exit
	// present in the breach branch.
	fleetGood := "package force\n" +
		"import (\n" +
		"\t\"os\"\n" +
		"\t_ \"force-orchestrator/internal/store\"\n" +
		"\t_ \"force-orchestrator/internal/agents\"\n" +
		")\n" +
		"func cmdDaemon() {\n" +
		"\tif store.RecentStartCount() > 5 {\n" +
		"\t\tos.Exit(1)\n" +
		"\t}\n" +
		"\tagents.SpawnAstromech(nil)\n" +
		"}\n"
	dcGood := "package force\n" +
		"const launchdPlistTemplate = \"\\n<key>Crashed</key>\\n<true/>\\n<key>SuccessfulExit</key>\\n<false/>\\n\"\n" +
		"const systemdUnitTemplate = \"\\nRestart=on-failure\\nRestartSec=5\\n\"\n"

	cases := []struct {
		name    string
		fleet   string
		dc      string
		wantSub string
	}{
		{
			name:    "no-cmdDaemon",
			fleet:   "package force\nfunc somethingElse() {}\n",
			dc:      dcGood,
			wantSub: "cmdDaemon function not found",
		},
		{
			name:    "missing-RecentStartCount",
			fleet:   strings.Replace(fleetGood, "store.RecentStartCount() > 5", "false", 1),
			dc:      dcGood,
			wantSub: "does not call store.RecentStartCount",
		},
		{
			name: "no-spawn-call",
			fleet: "package force\n" +
				"import (\n" +
				"\t\"os\"\n" +
				"\t_ \"force-orchestrator/internal/store\"\n" +
				")\n" +
				"func cmdDaemon() {\n" +
				"\tif store.RecentStartCount() > 5 {\n" +
				"\t\tos.Exit(1)\n" +
				"\t}\n" +
				"}\n",
			dc:      dcGood,
			wantSub: "does not call agents.Spawn",
		},
		{
			name: "RecentStartCount-after-spawn",
			fleet: "package force\n" +
				"import (\n" +
				"\t\"os\"\n" +
				"\t_ \"force-orchestrator/internal/store\"\n" +
				"\t_ \"force-orchestrator/internal/agents\"\n" +
				")\n" +
				"func cmdDaemon() {\n" +
				"\tagents.SpawnAstromech(nil)\n" +
				"\tif store.RecentStartCount() > 5 {\n" +
				"\t\tos.Exit(1)\n" +
				"\t}\n" +
				"}\n",
			dc:      dcGood,
			wantSub: "is AFTER first agents.Spawn",
		},
		{
			name: "no-exit-on-breach",
			fleet: "package force\n" +
				"import (\n" +
				"\t_ \"force-orchestrator/internal/store\"\n" +
				"\t_ \"force-orchestrator/internal/agents\"\n" +
				")\n" +
				"func cmdDaemon() {\n" +
				"\t_ = store.RecentStartCount()\n" +
				"\tagents.SpawnAstromech(nil)\n" +
				"}\n",
			dc:      dcGood,
			wantSub: "does not call os.Exit / log.Fatal",
		},
		{
			name:    "plist-missing-Crashed",
			fleet:   fleetGood,
			dc:      strings.Replace(dcGood, "<key>Crashed</key>", "<key>Other</key>", 1),
			wantSub: `<key>Crashed</key>`,
		},
		{
			name:    "plist-missing-SuccessfulExit",
			fleet:   fleetGood,
			dc:      strings.Replace(dcGood, "<key>SuccessfulExit</key>", "<key>Other2</key>", 1),
			wantSub: `<key>SuccessfulExit</key>`,
		},
		{
			name:    "plist-missing-true",
			fleet:   fleetGood,
			dc:      strings.Replace(dcGood, "<true/>", "<not-bool/>", 1),
			wantSub: `<true/>`,
		},
		{
			name:    "systemd-missing-restart-onfailure",
			fleet:   fleetGood,
			dc:      strings.Replace(dcGood, "Restart=on-failure", "Restart=always", 1),
			wantSub: "Restart=on-failure",
		},
		{
			name:    "systemd-missing-RestartSec",
			fleet:   fleetGood,
			dc:      strings.Replace(dcGood, "RestartSec=5", "RestartSec=99", 1),
			wantSub: "RestartSec=5",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			if err := os.MkdirAll(filepath.Join(root, "cmd", "force"), 0o755); err != nil {
				t.Fatalf("mkdir: %v", err)
			}
			if err := os.WriteFile(filepath.Join(root, "cmd", "force", "fleet_cmds.go"), []byte(tc.fleet), 0o644); err != nil {
				t.Fatalf("write fleet: %v", err)
			}
			if err := os.WriteFile(filepath.Join(root, "cmd", "force", "daemon_cmds.go"), []byte(tc.dc), 0o644); err != nil {
				t.Fatalf("write dc: %v", err)
			}
			err := checkDaemonCrashBudget(root)
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
	if err := os.MkdirAll(filepath.Join(root, "cmd", "force"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "cmd", "force", "fleet_cmds.go"), []byte(fleetGood), 0o644); err != nil {
		t.Fatalf("write fleet: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "cmd", "force", "daemon_cmds.go"), []byte(dcGood), 0o644); err != nil {
		t.Fatalf("write dc: %v", err)
	}
	if err := checkDaemonCrashBudget(root); err != nil {
		t.Fatalf("checker rejected compliant baseline: %v", err)
	}
}
