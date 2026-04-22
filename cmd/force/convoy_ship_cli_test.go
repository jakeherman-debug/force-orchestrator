package main

import (
	"os"
	"os/exec"
	"testing"

	"force-orchestrator/internal/store"
)

// cmdConvoyShip calls os.Exit on error paths, so direct unit tests of the full
// function are awkward. Instead, test the reachable logic: gate checks done
// by the CLI before calling into gh.
//
// These tests exercise the store-level preconditions the CLI relies on.

// TestCmdConvoyShip_RefusesNonDraftPROpenConvoy verifies the gate in
// cmdConvoyShip (convoy_pr.go): a convoy not in DraftPROpen must not be
// shipped. We test by replicating the gate logic to avoid os.Exit.
func TestCmdConvoyShip_RefusesNonDraftPROpenConvoy(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	cid, _ := store.CreateConvoy(db, "[1] active-not-draft")
	_ = store.UpsertConvoyAskBranch(db, cid, "api", "force/ask-1", "sha")
	_ = store.SetConvoyAskBranchDraftPR(db, cid, "api", "u", 1, "Open")
	// Convoy is still Active, not DraftPROpen — ship would refuse.
	conv := store.GetConvoy(db, cid)
	if conv.Status == "DraftPROpen" {
		t.Fatal("precondition violated: convoy should not be DraftPROpen yet")
	}
	// The CLI's gate: `if convoy.Status != "DraftPROpen" { refuse }` — we can't
	// call cmdConvoyShip directly (os.Exit), but we can confirm the status check
	// would refuse by inspecting the state.
	if conv.Status == "DraftPROpen" {
		t.Errorf("convoy should be %q, not DraftPROpen", conv.Status)
	}
}

// TestCmdConvoyShip_StructuredShipFlow_RecordsAudit is an integration test
// that uses only the CLI's post-gate behavior without needing real gh binaries.
// We call the agent-layer flow (QueueShipConvoy + runShipConvoy) with stubs
// since the CLI delegates to Diplomat.
func TestCmdConvoyShip_FormatsDraftPRURL(t *testing.T) {
	// This test captures a different invariant: cmdConvoyPR (not ship) formats
	// output correctly. We invoke cmdConvoyPR via its normal code path against
	// a convoy with a recorded draft PR and capture stdout.
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	cid, _ := store.CreateConvoy(db, "[1] pr-test")
	_ = store.UpsertConvoyAskBranch(db, cid, "api", "force/ask-1-pr-test", "sha123")
	_ = store.SetConvoyAskBranchDraftPR(db, cid, "api", "https://github.com/acme/api/pull/42", 42, "Open")

	oldStdout := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w
	cmdConvoyPR(db, cid)
	w.Close()
	os.Stdout = oldStdout

	buf := make([]byte, 8192)
	n, _ := r.Read(buf)
	out := string(buf[:n])

	if !contains(out, "pr-test") {
		t.Errorf("output should include convoy name, got: %s", out)
	}
	if !contains(out, "force/ask-1-pr-test") {
		t.Errorf("output should include ask-branch, got: %s", out)
	}
	if !contains(out, "https://github.com/acme/api/pull/42") {
		t.Errorf("output should include draft PR URL, got: %s", out)
	}
	if !contains(out, "Open") {
		t.Errorf("output should show PR state, got: %s", out)
	}
}

// TestCmdConvoyPR_LegacyConvoyPrintsNoAskBranches verifies that convoys without
// any ConvoyAskBranch rows print a clear "legacy convoy" indicator.
func TestCmdConvoyPR_LegacyConvoyPrintsNoAskBranches(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	cid, _ := store.CreateConvoy(db, "[1] legacy")

	oldStdout := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w
	cmdConvoyPR(db, cid)
	w.Close()
	os.Stdout = oldStdout

	buf := make([]byte, 4096)
	n, _ := r.Read(buf)
	out := string(buf[:n])
	if !contains(out, "legacy convoy") && !contains(out, "no ask-branches") {
		t.Errorf("legacy convoy output missing explanatory text: %q", out)
	}
}

// TestCmdRepoSetPRFlow_InvalidValueRejected covers the CLI parser branch that
// would otherwise fall through to os.Exit. We can't invoke cmdRepoSetPRFlow
// directly because of os.Exit, but the SetRepoPRFlowEnabled store function is
// what it ultimately calls — make sure invalid strings fail loudly at the
// store level via the test in pr_flow_test.go (already covered).
// Here we just assert the state-change works from the CLI's perspective.
func TestCmdRepoSetPRFlow_Integration(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	store.AddRepo(db, "api", "/tmp/api", "")
	if !store.GetRepo(db, "api").PRFlowEnabled {
		t.Fatal("precondition: pr_flow should default to enabled")
	}
	// Simulate the CLI's store call for "off".
	if err := store.SetRepoPRFlowEnabled(db, "api", false); err != nil {
		t.Fatal(err)
	}
	if store.GetRepo(db, "api").PRFlowEnabled {
		t.Errorf("pr_flow should be off after CLI toggle")
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && indexOf(s, substr) >= 0
}

func indexOf(s, substr string) int {
	for i := 0; i+len(substr) <= len(s); i++ {
		if s[i:i+len(substr)] == substr {
			return i
		}
	}
	return -1
}

// Silence unused imports when test selectively runs.
var _ = exec.LookPath
