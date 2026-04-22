package git

import (
	"os"
	"testing"
)

// TestMain forces BranchPrefix to return "" for all tests in the internal/git
// package so test assertions on branch names remain deterministic regardless
// of the developer's gh/git configuration. Individual tests that want to
// exercise prefix behavior use SetBranchPrefixOverride.
func TestMain(m *testing.M) {
	restore := SetBranchPrefixOverride("")
	code := m.Run()
	restore()
	os.Exit(code)
}
