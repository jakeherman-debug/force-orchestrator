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

// TestPattern_P15_BashGuardEnvWiring asserts that setupBashGuardShim
// returns BOTH a PATH= entry and a SHELL= entry. PATH-only wiring was
// the gap the 2026-04-29 empirical investigation surfaced — Claude
// CLI's Bash tool resolves the shell via $SHELL as an absolute path,
// so a PATH-only env override never reaches the shim. This static
// guard catches a future refactor that drops one of the two entries
// without depending on the runtime-effectiveness test under
// internal/agents being run.
//
// We pure-string-match the constructed env entries inside
// bash_guard_setup.go — string-level enforcement is equivalent to the
// other Pattern-style guards in this package (P12, P13, P15
// wiring-presence) and stays agnostic to import-graph constraints.
func TestPattern_P15_BashGuardEnvWiring(t *testing.T) {
	root := moduleRoot(t)
	path := filepath.Join(root, "internal", "agents", "bash_guard_setup.go")
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("Pattern P15 env-wiring: cannot read %s: %v", path, err)
	}
	text := string(body)

	required := []string{
		// The PATH entry is the original wiring; keep both layers.
		`pathEntry := fmt.Sprintf("PATH=%s`,
		// The SHELL entry is the Fix's load-bearing addition. Without
		// this Claude's Bash tool bypasses the shim entirely.
		`shellEntry := fmt.Sprintf("SHELL=%s"`,
		// And both must be returned together.
		`return []string{pathEntry, shellEntry}, nil`,
	}
	for _, want := range required {
		if !strings.Contains(text, want) {
			t.Errorf("Pattern P15 env-wiring: %s missing required snippet %q\n"+
				"  -> the bash-guard shim is unreachable in production without both PATH and SHELL entries",
				path, want)
		}
	}
}
