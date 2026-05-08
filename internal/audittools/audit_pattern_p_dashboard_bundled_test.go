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
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// checkDashboardBundled asserts that the dashboard-bundled contract
// holds for the source tree rooted at rootDir. It expects the same
// two files cmdDaemon's audit cares about: cmd/force/fleet_cmds.go
// and cmd/force/daemon_cmds.go.
//
// Returns nil when every contract clause is satisfied. Returns the
// first violation's error otherwise. Extracted from the production
// check so the sentinel can drive it with a synthetic TempDir.
func checkDashboardBundled(rootDir string) error {
	fleetPath := filepath.Join(rootDir, "cmd", "force", "fleet_cmds.go")
	fleetBytes, err := os.ReadFile(fleetPath)
	if err != nil {
		return fmt.Errorf("read %s: %w", fleetPath, err)
	}
	fleet := string(fleetBytes)

	if !strings.Contains(fleet, "dashboard.RunDashboardCtx(") {
		return fmt.Errorf("%s does not call dashboard.RunDashboardCtx(...) — daemon will boot without the bundled dashboard", fleetPath)
	}
	// Defensive check: the *legacy* RunDashboard (no Ctx) MUST NOT be
	// invoked from the daemon body, because it os.Exits on a port
	// collision and would torpedo the daemon. The fact that
	// "RunDashboardCtx" contains "RunDashboard" forces us to be precise.
	if strings.Contains(fleet, "dashboard.RunDashboard(") {
		lines := strings.Split(fleet, "\n")
		for i, ln := range lines {
			trimmed := strings.TrimSpace(ln)
			if strings.HasPrefix(trimmed, "//") {
				continue
			}
			if strings.Contains(ln, "dashboard.RunDashboard(") &&
				!strings.Contains(ln, "dashboard.RunDashboardCtx(") {
				return fmt.Errorf("%s:%d invokes legacy dashboard.RunDashboard — daemon path must use RunDashboardCtx so SIGTERM cleanly drains the HTTP server", fleetPath, i+1)
			}
		}
	}

	// (2) Default port literal 41977 in fleet_cmds.go.
	if !strings.Contains(fleet, "41977") {
		return fmt.Errorf("%s does not contain default port 41977 — operator-mnemonic default missing", fleetPath)
	}

	// (3) daemon_cmds.go has the SystemConfig helpers.
	dcPath := filepath.Join(rootDir, "cmd", "force", "daemon_cmds.go")
	dcBytes, err := os.ReadFile(dcPath)
	if err != nil {
		return fmt.Errorf("read %s: %w", dcPath, err)
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
			return fmt.Errorf("%s missing %q", dcPath, want)
		}
	}
	return nil
}

func TestPattern_P_DashboardBundled(t *testing.T) {
	if err := checkDashboardBundled(moduleRoot(t)); err != nil {
		t.Errorf("Pattern P_DashboardBundled: %v", err)
	}
}

// TestPattern_P_DashboardBundled_DetectsInjectedDrift proves the
// bundled-dashboard checker would fire when each contract clause is
// dropped from the daemon source tree. We build a synthetic two-file
// tree under t.TempDir(), mutate one clause at a time, and assert
// the matching error.
func TestPattern_P_DashboardBundled_DetectsInjectedDrift(t *testing.T) {
	// Compliant baseline. fleet_cmds.go satisfies clauses 1 and 2;
	// daemon_cmds.go satisfies clause 3 (all five required tokens).
	fleetGood := "package force\n" +
		"import _ \"force-orchestrator/internal/dashboard\"\n" +
		"func cmdDaemon() {\n" +
		"\tgo dashboard.RunDashboardCtx(ctx, 41977)\n" +
		"}\n"
	dcGood := "package force\n" +
		"const defaultDashboardPort = 41977\n" +
		"const dashboardPortKey = \"dashboard_port\"\n" +
		"const dashboardEnabledKey = \"dashboard_enabled\"\n" +
		"func dashboardPortFromConfig() int { return defaultDashboardPort }\n" +
		"func dashboardEnabledFromConfig() bool { return true }\n"

	cases := []struct {
		name      string
		fleet     string
		dc        string
		wantSub   string
		dropFleet bool
		dropDc    bool
	}{
		{
			name:    "missing-RunDashboardCtx-call",
			fleet:   strings.Replace(fleetGood, "dashboard.RunDashboardCtx(", "dashboard.SomethingElse(", 1),
			dc:      dcGood,
			wantSub: "does not call dashboard.RunDashboardCtx",
		},
		{
			name: "uses-legacy-RunDashboard",
			// Two calls — one Ctx (passes clause 1), and one legacy
			// (trips the defensive scan).
			fleet: "package force\n" +
				"import _ \"force-orchestrator/internal/dashboard\"\n" +
				"func cmdDaemon() {\n" +
				"\tgo dashboard.RunDashboardCtx(ctx, 41977)\n" +
				"\tgo dashboard.RunDashboard(41977)\n" +
				"}\n",
			dc:      dcGood,
			wantSub: "invokes legacy dashboard.RunDashboard",
		},
		{
			name:    "missing-default-port",
			fleet:   strings.Replace(fleetGood, "41977", "9090", 1),
			dc:      dcGood,
			wantSub: "does not contain default port 41977",
		},
		{
			name:    "dc-missing-portFromConfig",
			fleet:   fleetGood,
			dc:      strings.Replace(dcGood, "func dashboardPortFromConfig", "func somethingElse_PortFromConfig", 1),
			wantSub: `missing "func dashboardPortFromConfig"`,
		},
		{
			name:    "dc-missing-enabledFromConfig",
			fleet:   fleetGood,
			dc:      strings.Replace(dcGood, "func dashboardEnabledFromConfig", "func somethingElse_EnabledFromConfig", 1),
			wantSub: `missing "func dashboardEnabledFromConfig"`,
		},
		{
			name:    "dc-missing-port-literal",
			fleet:   fleetGood,
			dc:      strings.ReplaceAll(dcGood, "41977", "9090"),
			wantSub: `missing "41977"`,
		},
		{
			name:    "dc-missing-dashboard_port-key",
			fleet:   fleetGood,
			dc:      strings.Replace(dcGood, "dashboard_port", "dashpath_xxxx", 1),
			wantSub: `missing "dashboard_port"`,
		},
		{
			name:    "dc-missing-dashboard_enabled-key",
			fleet:   fleetGood,
			dc:      strings.Replace(dcGood, "dashboard_enabled", "dash_xxxxxxxx", 1),
			wantSub: `missing "dashboard_enabled"`,
		},
		{
			name:      "missing-fleet-file",
			dropFleet: true,
			dc:        dcGood,
			wantSub:   "fleet_cmds.go",
		},
		{
			name:    "missing-dc-file",
			fleet:   fleetGood,
			dropDc:  true,
			wantSub: "daemon_cmds.go",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tempRoot := t.TempDir()
			if err := os.MkdirAll(filepath.Join(tempRoot, "cmd", "force"), 0o755); err != nil {
				t.Fatalf("mkdir: %v", err)
			}
			if !tc.dropFleet {
				if err := os.WriteFile(filepath.Join(tempRoot, "cmd", "force", "fleet_cmds.go"), []byte(tc.fleet), 0o644); err != nil {
					t.Fatalf("write fleet: %v", err)
				}
			}
			if !tc.dropDc {
				if err := os.WriteFile(filepath.Join(tempRoot, "cmd", "force", "daemon_cmds.go"), []byte(tc.dc), 0o644); err != nil {
					t.Fatalf("write dc: %v", err)
				}
			}
			err := checkDashboardBundled(tempRoot)
			if err == nil {
				t.Fatalf("checker accepted violating tree (case %q); want failure containing %q", tc.name, tc.wantSub)
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("error %q does not contain %q", err.Error(), tc.wantSub)
			}
		})
	}

	// Positive control: a compliant tree must pass.
	tempRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(tempRoot, "cmd", "force"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(tempRoot, "cmd", "force", "fleet_cmds.go"), []byte(fleetGood), 0o644); err != nil {
		t.Fatalf("write fleet: %v", err)
	}
	if err := os.WriteFile(filepath.Join(tempRoot, "cmd", "force", "daemon_cmds.go"), []byte(dcGood), 0o644); err != nil {
		t.Fatalf("write dc: %v", err)
	}
	if err := checkDashboardBundled(tempRoot); err != nil {
		t.Fatalf("checker rejected compliant tree: %v", err)
	}
}
