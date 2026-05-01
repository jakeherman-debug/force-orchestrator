package agents

import (
	"os"
	"testing"

	igit "force-orchestrator/internal/git"
)

// TestMain forces the git package's BranchPrefix to return "" so branch-name
// assertions in agents tests are deterministic. Tests that specifically want
// to verify prefix behavior use igit.SetBranchPrefixOverride themselves.
//
// D3 polish-pass iteration 2: also pin LIVE_HAIKU_DISABLED=1 so unit tests
// never spend an LLM call. The renderers' env-flag guard at
// liveHaikuDisabled() returns the deterministic synthesise* path. Tests
// that specifically want to exercise the live path unset / re-set the
// flag via t.Setenv themselves.
//
// Pre-D4 race-baseline closure: pin the adversarial-pairing rate to 0
// across the unit-test binary. Production default is 0.1 (10% ramp);
// in tests, 10% sampling means the pair goroutine fires
// non-deterministically, and after the join-handle wait was added to
// the production callers (Council/Medic/ConvoyReview), the goroutine's
// critic call lands in the stub prompts buffer ~10% of the time —
// which breaks tests that read stub.LastPrompt() (e.g.
// TestCouncilBoundaryIntegrity_InvokedEndToEnd). Hot-path tests that
// specifically exercise rate-resolution clear the override on a
// per-test basis via clearAdversarialPairingRateOverrideForTest.
func TestMain(m *testing.M) {
	restore := igit.SetBranchPrefixOverride("")
	if err := os.Setenv("LIVE_HAIKU_DISABLED", "1"); err != nil {
		panic("failed to pin LIVE_HAIKU_DISABLED for tests: " + err.Error())
	}
	restoreRate := setAdversarialPairingRateOverrideForTest(0.0)
	code := m.Run()
	restoreRate()
	restore()
	os.Exit(code)
}
