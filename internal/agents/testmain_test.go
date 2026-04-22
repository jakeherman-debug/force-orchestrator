package agents

import (
	"os"
	"testing"

	igit "force-orchestrator/internal/git"
)

// TestMain forces the git package's BranchPrefix to return "" so branch-name
// assertions in agents tests are deterministic. Tests that specifically want
// to verify prefix behavior use igit.SetBranchPrefixOverride themselves.
func TestMain(m *testing.M) {
	restore := igit.SetBranchPrefixOverride("")
	code := m.Run()
	restore()
	os.Exit(code)
}
