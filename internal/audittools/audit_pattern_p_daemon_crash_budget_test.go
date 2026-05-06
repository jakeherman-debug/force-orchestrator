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
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPattern_P_DaemonCrashBudget(t *testing.T) {
	root := moduleRoot(t)
	target := filepath.Join(root, "cmd", "force", "fleet_cmds.go")

	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, target, nil, parser.ParseComments)
	if err != nil {
		t.Fatalf("parse %s: %v", target, err)
	}

	// (1) cmdDaemon must call store.RecentStartCount BEFORE the first
	//     agents.Spawn*.
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
		t.Fatalf("Pattern P_DaemonCrashBudget: cmdDaemon function not found in %s", target)
	}
	if recentStartPos == 0 {
		t.Fatalf("Pattern P_DaemonCrashBudget: cmdDaemon does not call store.RecentStartCount(...) — crash-budget guard is missing; a broken binary would chew CPU forever via launchd/systemd auto-restart")
	}
	if firstSpawnPos == 0 {
		t.Fatalf("Pattern P_DaemonCrashBudget: cmdDaemon does not call agents.Spawn* — sanity check failed")
	}
	if recentStartPos >= firstSpawnPos {
		t.Fatalf("Pattern P_DaemonCrashBudget: store.RecentStartCount (pos=%d) is AFTER first agents.Spawn* (pos=%d). The crash-budget check MUST run before any agent goroutine starts.",
			recentStartPos, firstSpawnPos)
	}
	if !exitOnBreachOk {
		t.Errorf("Pattern P_DaemonCrashBudget: cmdDaemon does not call os.Exit / log.Fatal — the crash-budget breach path must terminate the process, not just log a warning")
	}

	// (3) launchd plist + (4) systemd unit. We grep the template
	//     functions' string output via the templates themselves: read
	//     daemon_cmds.go as text and assert the literal keys are present
	//     inside the launchd / systemd template bodies.
	cmdsPath := filepath.Join(root, "cmd", "force", "daemon_cmds.go")
	body, rerr := os.ReadFile(cmdsPath)
	if rerr != nil {
		t.Fatalf("read %s: %v", cmdsPath, rerr)
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
			t.Errorf("Pattern P_DaemonCrashBudget: launchdPlistTemplate must contain %q — auto-restart contract incomplete", want)
		}
	}
	systemdRequired := []string{
		"Restart=on-failure",
		"RestartSec=5",
	}
	for _, want := range systemdRequired {
		if !strings.Contains(src, want) {
			t.Errorf("Pattern P_DaemonCrashBudget: systemdUnitTemplate must contain %q — auto-restart contract incomplete", want)
		}
	}
}
