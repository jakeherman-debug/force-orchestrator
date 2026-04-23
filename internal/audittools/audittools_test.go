// Package audittools hosts test-time guards that enforce audit-related
// invariants across the tree. The package has no production code.
package audittools

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// remainingAuditSkips is the allowlist of AUDIT IDs whose `t.Skip("AUDIT-NNN:`
// markers are known to remain after the fix plan's 10 initial PRs merged.
// Each entry is documented in FIX-LOG.md / AUDIT-TEST-MANIFEST.md with the
// follow-up fix that will close it.
//
// Adding an ID here is NOT a license to sweep it under the rug — it's a
// signed acknowledgement that the finding is tracked elsewhere. Removing
// a finding from the allowlist means: the fix PR that closes it must also
// drop the `t.Skip` line from the corresponding test.
//
// The operator's end-state goal is this list shrinking to zero.
var remainingAuditSkips = map[string]string{
	// AUDIT-011, AUDIT-025, AUDIT-085, AUDIT-149: closed by Campaign 2
	// (scope deferrals). See FIX-LOG.md § "Campaign 2 — Scope deferrals".
	//
	// AUDIT-030, -108, -109, -110, -114, -115, -116, -139: closed by
	// Campaign 1 / Fix #8.5. Skip markers removed from
	// audit_pattern_p12_test.go; the pattern test now asserts the post-fix
	// contract (boundary markers, DisallowUnknownFields, Approved *bool,
	// Captain fail-closed, Chancellor fail-closed, signal-token sanitizer).


	// Fix #8 Phase A closed the three self-heal terminator signatures and
	// two one-liners. Phase B/C migrate the remaining ~108 call sites. Each
	// AUDIT-NNN below is tracked by a TODO(Fix #8b) marker in the code.
	"AUDIT-015": "Fix #8b (onSubPRMerged mid-tx log)",
	"AUDIT-026": "Fix #8 (ResetTask resurrect)",
	"AUDIT-027": "Fix #8 (concurrent cancel-vs-approve)",
	"AUDIT-040": "Fix #8b (escalate CI-triage double UPDATE)",
	"AUDIT-042": "Fix #8b (UpdateAskBranchPRChecks discarded)",
	"AUDIT-043": "Fix #8b (PRCloseUnconditionalMarkClosed)",
	"AUDIT-044": "Fix #8b (librarian silent fallback)",
	"AUDIT-045": "Fix #4/#8 (concurrency batch)",
	"AUDIT-046": "Fix #8b (concurrency batch)",
	"AUDIT-047": "Fix #8b (concurrency batch)",
	"AUDIT-066": "Fix #8b companion (PruneFleet unparameterised interval)",
	"AUDIT-068": "Fix #8b (ClaimBounty conflates ErrNoRows)",
	"AUDIT-069": "Fix #8b (ResolveFeatureBlockers no transaction)",
	"AUDIT-072": "Fix #8b pattern-covered (P7)",
	"AUDIT-125": "Fix #8b (lifecycle batch)",
	"AUDIT-126": "Fix #8b (lifecycle batch)",
	"AUDIT-127": "Fix #8b (lifecycle batch)",
	"AUDIT-129": "Fix #8b (lifecycle batch)",
	"AUDIT-130": "Fix #8b (schema+time batch)",
	"AUDIT-131": "Fix #8b (schema+time batch)",
	"AUDIT-132": "Fix #8b (schema+time batch)",
	"AUDIT-137": "Fix #8b (test-quality)",
	"AUDIT-151": "Fix #8b spotcheck-c",
	"AUDIT-155": "Fix #8b spotcheck-d (union merge no repo lock)",
	"AUDIT-156": "Fix #8b pattern-covered",
	"AUDIT-158": "Fix #8b lifecycle batch",
	"AUDIT-159": "Fix #8b pattern-covered",
	"AUDIT-164": "Fix #8b lifecycle batch (low, pattern-covered)",
	"AUDIT-165": "Fix #8b lifecycle batch (low, pattern-covered)",

	// Schema+time batch (AUDIT-077, -078, -080, -082, -143, -146, -147, -148):
	// each requires a DDL or time-handling migration separate from the
	// Fix #4 hot-table index pass.
	"AUDIT-077": "Fix #8c schema+time batch",
	"AUDIT-078": "Fix #8c schema+time batch",
	"AUDIT-080": "Fix #8c schema+time batch (stall_retrigger_count column defaulted)",
	"AUDIT-082": "Fix #8c schema+time batch",
	"AUDIT-143": "Fix #8c schema+time batch",
	"AUDIT-146": "Fix #8c schema+time batch",
	"AUDIT-147": "Fix #8c schema+time batch",
	"AUDIT-148": "Fix #8c schema+time batch",

	// Concurrency batch: the remaining races covered by Pattern P1/P7
	// need Fix #8's UpdateBountyStatusFrom variant.
	"AUDIT-090": "Fix #8 pattern-covered (P1)",
	"AUDIT-091": "Fix #8 pattern-covered (P1)",
	"AUDIT-092": "Fix #4/#8 concurrency batch",
	"AUDIT-093": "Fix #4/#8 concurrency batch",
	"AUDIT-094": "Fix #8 pattern-covered (P1)",
	"AUDIT-095": "Fix #8 pattern-covered (P1)",
	"AUDIT-096": "Fix #4/#8 concurrency batch",
	"AUDIT-097": "Fix #4/#8 concurrency batch",
	"AUDIT-099": "Fix #10 pattern-covered / Fix #8 silent-failure",
	"AUDIT-100": "Fix #8 pattern-covered (P1)",
}

var auditSkipRe = regexp.MustCompile(`t\.Skip\("(AUDIT-\d+)`)

// TestNoAuditSkipMarkersRemain walks the entire module and fails if any
// `t.Skip("AUDIT-NNN:` marker is present for an AUDIT ID that is NOT on
// the allowlist. This is the `make test-audit` gate.
//
// The walker intentionally ignores:
//   - `.fix-worktrees/` — operator-managed parallel checkouts; not shipped.
//   - `vendor/` — not in this repo today; future-proof.
//   - non-*.go files.
func TestNoAuditSkipMarkersRemain(t *testing.T) {
	root := moduleRoot(t)

	offenders := make(map[string][]string) // "<path>:<line>" → AUDIT-NNN
	unexpected := make(map[string]struct{})

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			name := d.Name()
			if name == ".fix-worktrees" || name == ".force-worktrees" ||
				name == "vendor" || name == ".git" || name == "node_modules" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		body, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		lines := strings.Split(string(body), "\n")
		for i, line := range lines {
			m := auditSkipRe.FindStringSubmatch(line)
			if m == nil {
				continue
			}
			auditID := m[1]
			if _, ok := remainingAuditSkips[auditID]; !ok {
				loc := fmt.Sprintf("%s:%d", rel(root, path), i+1)
				offenders[loc] = append(offenders[loc], auditID)
				unexpected[auditID] = struct{}{}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}

	if len(offenders) == 0 {
		t.Logf("make test-audit clean: %d AUDIT-NNN skip markers remain (all on the allowlist)", countAllowlistHits(root))
		return
	}

	// Sort offenders for stable output.
	paths := make([]string, 0, len(offenders))
	for p := range offenders {
		paths = append(paths, p)
	}
	sort.Strings(paths)

	ids := make([]string, 0, len(unexpected))
	for id := range unexpected {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	t.Errorf("make test-audit FAILED — %d unlisted AUDIT-NNN skip marker(s) found:\n", len(offenders))
	for _, p := range paths {
		t.Errorf("  %s — %s", p, strings.Join(offenders[p], ", "))
	}
	t.Errorf("\nAffected AUDIT IDs: %s\n\nTo clear this test:\n"+
		"  (a) drop the matching t.Skip(\"AUDIT-NNN: ...\") line (the fix landed);\n"+
		"  (b) OR add the ID to `remainingAuditSkips` in internal/audittools/audittools_test.go with the follow-up fix name.\n"+
		"Option (a) is preferred. Option (b) is a sanctioned defer, not a silencer.",
		strings.Join(ids, ", "))
}

// countAllowlistHits counts the total AUDIT skip markers found, for the
// clean-run log line. No-op if it fails.
func countAllowlistHits(root string) int {
	count := 0
	filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			if d != nil && d.IsDir() {
				name := d.Name()
				if name == ".fix-worktrees" || name == ".force-worktrees" ||
					name == "vendor" || name == ".git" || name == "node_modules" {
					return filepath.SkipDir
				}
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		body, rerr := os.ReadFile(path)
		if rerr != nil {
			return nil
		}
		count += len(auditSkipRe.FindAllString(string(body), -1))
		return nil
	})
	return count
}

func moduleRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	// Walk up until we find go.mod.
	dir := wd
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("could not find go.mod above %s", wd)
		}
		dir = parent
	}
}

func rel(root, path string) string {
	r, err := filepath.Rel(root, path)
	if err != nil {
		return path
	}
	return r
}
