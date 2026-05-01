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
func TestMain(m *testing.M) {
	restore := igit.SetBranchPrefixOverride("")
	if err := os.Setenv("LIVE_HAIKU_DISABLED", "1"); err != nil {
		panic("failed to pin LIVE_HAIKU_DISABLED for tests: " + err.Error())
	}
	code := m.Run()
	restore()
	os.Exit(code)
}
