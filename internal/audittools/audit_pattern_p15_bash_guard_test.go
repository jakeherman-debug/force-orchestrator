// Package audittools: Pattern P15 — astromech Bash boundary integrity.
package audittools

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestPattern_P15_BashGuardIntegrity is the grep-based regression guard
// for D2 T1-3 (astromech Bash allowlist wrapper).
//
// The astromech's claude session MUST route Bash invocations through
// `force-bash-guard`. Two enforcement points are checked here:
//
//  1. internal/agents/astromech.go must reference `setupBashGuardShim`
//     in the daemon-side runAstromechTask call path so a per-worktree
//     PATH override is installed before the claude subprocess starts.
//  2. internal/agents/bash_guard_setup.go must exist and reference the
//     `force-bash-guard` binary name; this is the helper that materialises
//     the shim and the env entry passed to RunCLIStreamingContext.
//
// A regression where the shim setup is removed or the helper function is
// quietly bypassed (e.g., a future refactor that drops the `extraEnv`
// argument) trips this test. The test does NOT and cannot verify Claude
// CLI's runtime exec semantics — that responsibility lives in
// force-bash-guard's own test suite + the integration test in
// bash_guard_setup_test.go.
func TestPattern_P15_BashGuardIntegrity(t *testing.T) {
	root := moduleRoot(t)

	type expect struct {
		path     string
		mustHave []string
	}
	expectations := []expect{
		{
			path: filepath.Join(root, "internal", "agents", "astromech.go"),
			mustHave: []string{
				"setupBashGuardShim",
				"force-bash-guard",
				// Confirm we actually pass the bash-guard env into the
				// claude call — not just compute it and drop it.
				"bashGuardEnv",
			},
		},
		{
			path: filepath.Join(root, "internal", "agents", "bash_guard_setup.go"),
			mustHave: []string{
				"force-bash-guard",
				"BashGuardBinaryName",
				"bashGuardShimDirName",
				"setupBashGuardShim",
				"bashShimSource",
				"FORCE_BASH_GUARD_BIN",
			},
		},
		{
			path: filepath.Join(root, "cmd", "force-bash-guard", "main.go"),
			mustHave: []string{
				"allowedPrograms",
				"deniedPrograms",
				"evaluateCompound",
			},
		},
	}

	for _, e := range expectations {
		body, err := os.ReadFile(e.path)
		if err != nil {
			t.Errorf("Pattern P15: cannot read %s: %v — wiring incomplete", e.path, err)
			continue
		}
		text := string(body)
		for _, want := range e.mustHave {
			if !strings.Contains(text, want) {
				t.Errorf("Pattern P15 violation: %s does not contain %q (bash-guard wiring missing)",
					e.path, want)
			}
		}
	}
}
