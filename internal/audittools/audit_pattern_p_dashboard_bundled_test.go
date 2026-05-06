// Package audittools: Pattern P_DashboardBundled — the daemon main
// loop MUST spawn the dashboard goroutine when SystemConfig key
// `dashboard_enabled` is true (or unset, which defaults to true), and
// the default port MUST be 41977.
//
// Why an audit guard? The bundled dashboard is the operator's primary
// view into a running fleet. If a refactor accidentally drops the
// goroutine spawn, the daemon would silently start without the UI and
// no other test would catch it (the SPA tests work against a free-
// standing dashboard process).
//
// Three checks:
//
//  1. cmd/force/fleet_cmds.go (cmdDaemon body) contains a call to
//     dashboard.RunDashboardCtx(...) — the cancellable form, NOT
//     dashboard.RunDashboard (which os.Exits on a server error and
//     therefore can't coexist with the daemon's drain loop).
//
//  2. The SystemConfig default port is the literal 41977 — both in
//     fleet_cmds.go (the spawn site) and in cmd/force/daemon_cmds.go
//     (where dashboardPortFromConfig defaults).
//
//  3. cmd/force/daemon_cmds.go contains the helper functions
//     dashboardPortFromConfig and dashboardEnabledFromConfig with the
//     correct defaults (41977 / true).
package audittools

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPattern_P_DashboardBundled(t *testing.T) {
	root := moduleRoot(t)

	// (1) cmdDaemon spawns dashboard.RunDashboardCtx.
	fleetPath := filepath.Join(root, "cmd", "force", "fleet_cmds.go")
	fleetBytes, err := os.ReadFile(fleetPath)
	if err != nil {
		t.Fatalf("read %s: %v", fleetPath, err)
	}
	fleet := string(fleetBytes)

	if !strings.Contains(fleet, "dashboard.RunDashboardCtx(") {
		t.Errorf("Pattern P_DashboardBundled: %s does not call dashboard.RunDashboardCtx(...) — daemon will boot without the bundled dashboard", fleetPath)
	}
	// Defensive check: the *legacy* RunDashboard (no Ctx) MUST NOT be
	// invoked from the daemon body, because it os.Exits on a port
	// collision and would torpedo the daemon. The fact that
	// "RunDashboardCtx" contains "RunDashboard" forces us to be precise.
	if strings.Contains(fleet, "dashboard.RunDashboard(") {
		// Could be a comment that mentions it. Strip the function
		// invocation pattern — `dashboard.RunDashboard(<args>)` without
		// the `Ctx` suffix.
		// Use a simple split-and-scan.
		lines := strings.Split(fleet, "\n")
		for i, ln := range lines {
			trimmed := strings.TrimSpace(ln)
			if strings.HasPrefix(trimmed, "//") {
				continue
			}
			if strings.Contains(ln, "dashboard.RunDashboard(") &&
				!strings.Contains(ln, "dashboard.RunDashboardCtx(") {
				t.Errorf("Pattern P_DashboardBundled: %s:%d invokes legacy dashboard.RunDashboard — daemon path must use RunDashboardCtx so SIGTERM cleanly drains the HTTP server", fleetPath, i+1)
			}
		}
	}

	// (2) Default port literal 41977 in fleet_cmds.go.
	if !strings.Contains(fleet, "41977") {
		t.Errorf("Pattern P_DashboardBundled: %s does not contain default port 41977 — operator-mnemonic default missing", fleetPath)
	}

	// (3) daemon_cmds.go has the SystemConfig helpers.
	dcPath := filepath.Join(root, "cmd", "force", "daemon_cmds.go")
	dcBytes, err := os.ReadFile(dcPath)
	if err != nil {
		t.Fatalf("read %s: %v", dcPath, err)
	}
	dc := string(dcBytes)

	for _, want := range []string{
		"func dashboardPortFromConfig",
		"func dashboardEnabledFromConfig",
		"41977",
		"dashboard_port",
		"dashboard_enabled",
	} {
		if !strings.Contains(dc, want) {
			t.Errorf("Pattern P_DashboardBundled: %s missing %q", dcPath, want)
		}
	}
}
