// Package audittools: Pattern P17 — CLAUDE.md size invariant.
package audittools

import (
	"os"
	"path/filepath"
	"testing"
)

// claudeMdHardCapBytes mirrors agents.ClaudeMdHardCapBytes. Duplicated
// as a literal here so this audit test does not import internal/agents
// (audittools is meant to read source on disk, not link the runtime).
// If the cap moves, both sites must move in lockstep — that's the
// trade-off for keeping audittools dependency-light.
const claudeMdHardCapBytes = 20 * 1024

// TestPattern_P17_ClaudeMdSize is the file-size invariant for the
// auto-generated CLAUDE.md.
//
// D3 Phase 1 pivots CLAUDE.md from a hand-edited 50 KB rulebook to a
// renderer-managed file with `render_to='claude-md-file'` content
// only. The Phase 1 target is ≤ 10 KB; the absolute upper bound
// enforced by this test (and the pre-commit hook) is 20 KB.
//
// Bumping the cap is a deliberate operator action — it requires
// editing this constant + agents.ClaudeMdHardCapBytes +
// scripts/pre-commit/claude-md-size-check.sh in the same commit. The
// audit_pattern_p17 history then carries the rationale.
func TestPattern_P17_ClaudeMdSize(t *testing.T) {
	root := moduleRoot(t)
	path := filepath.Join(root, "CLAUDE.md")

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat CLAUDE.md: %v", err)
	}
	size := info.Size()
	if size > claudeMdHardCapBytes {
		t.Fatalf("CLAUDE.md is %d bytes; hard cap is %d (Phase 1 target ≤ 10240).\n"+
			"  Either:\n"+
			"    - move content out via the FleetRules render_to enum\n"+
			"      (agent-prompt / fix-log / pattern-test-docstring / per-domain-doc:*),\n"+
			"      then `make render-rules`, OR\n"+
			"    - bump claudeMdHardCapBytes + agents.ClaudeMdHardCapBytes +\n"+
			"      scripts/pre-commit/claude-md-size-check.sh together with a\n"+
			"      commit message that justifies the growth.", size, claudeMdHardCapBytes)
	}
	t.Logf("CLAUDE.md: %d bytes (hard cap %d, Phase 1 target ≤ 10240)", size, claudeMdHardCapBytes)
}
