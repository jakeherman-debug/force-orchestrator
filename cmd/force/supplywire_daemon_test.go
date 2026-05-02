// supplywire_daemon_test.go — daemon-side static guard for D5 fix-loop
// iter 1 slice α.
//
// The strict-verifier NO-GO finding was that fleet_cmds.go never called
// agents.WireSupplyRules — the rules were registered only in tests.
// The agents-package regression tests
// (internal/agents/supplywire_test.go) pin the wire helper's behaviour;
// THIS test pins the cmd/force/fleet_cmds.go call site so a future
// refactor can't silently delete the daemon-side hook.
//
// Why a string-based source scan rather than running cmdDaemon: the
// daemon brings up the full agent fleet, claim loops, signal handlers,
// holocron migrations, and PID files — none of which we want in a
// regression test. The single load-bearing fact this test pins is the
// presence of a `WireSupplyRules(` call inside cmdDaemon's source.

package main

import (
	"os"
	"strings"
	"testing"
)

// TestFleetCmds_CallsWireSupplyRules pins the daemon's WireSupplyRules
// call. Fails if a refactor removes the call from fleet_cmds.go (the
// strict-verifier NO-GO regression).
func TestFleetCmds_CallsWireSupplyRules(t *testing.T) {
	src, err := os.ReadFile("fleet_cmds.go")
	if err != nil {
		t.Fatalf("read fleet_cmds.go: %v", err)
	}
	body := string(src)

	// Match either `agents.WireSupplyRules(` or `WireSupplyRules(` —
	// the latter would only match if cmd/force somehow vendored its
	// own copy, which it doesn't, but keeping the assertion permissive
	// avoids brittleness against future import-alias changes.
	if !strings.Contains(body, "WireSupplyRules(") {
		t.Fatal("fleet_cmds.go: WireSupplyRules call missing — strict-verifier NO-GO regression has re-opened. " +
			"The daemon must call agents.WireSupplyRules(db, caClient, osvClient) once at startup so SUPPLY-* " +
			"rules are registered into the manifest-gated dispatcher AND the supply-token-recheck dog deps " +
			"are populated.")
	}

	// Also check the osv import: the daemon must construct osv.NewInProcess()
	// to hand to WireSupplyRules. Without it, SUPPLY-005 is broken at boot.
	if !strings.Contains(body, "osv.NewInProcess") {
		t.Error("fleet_cmds.go: osv.NewInProcess() call missing — SUPPLY-005 will fail at boot. " +
			"The daemon must construct an osv.Client and pass it to agents.WireSupplyRules.")
	}
}
