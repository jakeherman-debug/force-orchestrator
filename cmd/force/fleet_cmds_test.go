// fleet_cmds_test.go — daemon-side static guard for D5.5 P3 ζ.
//
// The Wave-3-ζ wiring extends cmdDaemon to construct a stagegate.Registry,
// register the baseline + all P3 advanced gates against it, and call
// agents.RegisterStageGateRegistry to install it for the convoy-stage-watch
// dog. THIS test pins those call sites so a future refactor can't silently
// delete the daemon-side hook.
//
// Why a string-based source scan rather than running cmdDaemon: the
// daemon brings up the full agent fleet, claim loops, signal handlers,
// holocron migrations, and PID files — none of which we want in a
// regression test. The single load-bearing fact this test pins is the
// presence of the wiring calls inside cmdDaemon's source. Mirrors the
// shape of supplywire_daemon_test.go (D5 fix-loop iter 1 slice α).

package main

import (
	"os"
	"strings"
	"testing"
)

// TestFleetCmds_StageGateRegistryWired pins the daemon's stage-gate
// registry construction + registration. Fails if a refactor removes any
// of the four load-bearing calls from fleet_cmds.go.
func TestFleetCmds_StageGateRegistryWired(t *testing.T) {
	src, err := os.ReadFile("fleet_cmds.go")
	if err != nil {
		t.Fatalf("read fleet_cmds.go: %v", err)
	}
	body := string(src)

	// 1. Datadog client construction must be present so the gate can
	//    actually evaluate metrics.
	if !strings.Contains(body, "datadog.NewInProcess(") {
		t.Error("fleet_cmds.go: datadog.NewInProcess() call missing — datadog_metric_threshold gate will be unavailable. " +
			"The daemon must construct the in-process client at startup.")
	}

	// 2. Databricks client construction (same shape as datadog).
	if !strings.Contains(body, "databricks.NewInProcess(") {
		t.Error("fleet_cmds.go: databricks.NewInProcess() call missing — databricks_query_threshold gate will be unavailable.")
	}

	// 3. Stage-gate registry must be constructed.
	if !strings.Contains(body, "stagegate.NewRegistry(") {
		t.Error("fleet_cmds.go: stagegate.NewRegistry() call missing — the convoy-stage-watch dog will run with no registry.")
	}

	// 4. Baseline gates must be registered.
	if !strings.Contains(body, "stagegate.RegisterBaselineGates(") {
		t.Error("fleet_cmds.go: stagegate.RegisterBaselineGates() call missing — soak_minutes / operator_confirm / null / all_of / any_of will be unavailable.")
	}

	// 5. P3 advanced gates must be registered.
	if !strings.Contains(body, "stagegate.RegisterAllP3Gates(") {
		t.Error("fleet_cmds.go: stagegate.RegisterAllP3Gates() call missing — probe_endpoint / release_label_present / datadog_metric_threshold / databricks_query_threshold will be unavailable.")
	}

	// 6. Registry must be installed via the agents-package seam so the
	//    dog can resolve it at tick time.
	if !strings.Contains(body, "agents.RegisterStageGateRegistry(") {
		t.Error("fleet_cmds.go: agents.RegisterStageGateRegistry() call missing — the dog's package-var seam will stay nil and the dog will skip every tick.")
	}
}

// TestFleetCmds_StageGateClientFailures_NonFatal pins the "log + pass nil"
// shape for datadog/databricks construction failures. The daemon must
// continue startup when these clients fail to construct (operator may not
// have configured the integration yet); we assert the fallback paths exist.
func TestFleetCmds_StageGateClientFailures_NonFatal(t *testing.T) {
	src, err := os.ReadFile("fleet_cmds.go")
	if err != nil {
		t.Fatalf("read fleet_cmds.go: %v", err)
	}
	body := string(src)

	// Both clients must have a "set to nil on error" code path. We look
	// for the construction-error-handler pattern that pairs the construct
	// call with a nil-assignment to the same variable.
	if !strings.Contains(body, "ddClient = nil") {
		t.Error("fleet_cmds.go: missing 'ddClient = nil' fallback — a Datadog construction failure must be non-fatal so the daemon can boot without Datadog credentials configured.")
	}
	if !strings.Contains(body, "dbxClient = nil") {
		t.Error("fleet_cmds.go: missing 'dbxClient = nil' fallback — a Databricks construction failure must be non-fatal so the daemon can boot without Databricks credentials configured.")
	}
}
