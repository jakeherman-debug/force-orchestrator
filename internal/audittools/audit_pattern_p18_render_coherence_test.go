// Package audittools: Pattern P18 — render coherence invariant.
//
// The auto-generated files (CLAUDE.md, FIX-LOG.md, every per-domain doc
// rendered by `force render-rules`) MUST byte-equal the bytes produced
// by rendering the FleetRules audit slice into a fresh in-memory DB.
// Any drift means an operator edited fleet_rules_audit.go (or its
// embedded fixlog/*.md content) without re-running `make render-rules`
// and re-staging the resulting files.
//
// This is the systemic fix-class third layer:
//   - Layer 1: BootstrapFleetRules is convergent on content_hash
//     mismatch — re-bootstrapping a stale persistent DB refreshes it.
//   - Layer 2: `force render-rules` defaults to fresh in-memory — the
//     CLI's render output never depends on operator-side DB state.
//   - Layer 3: TestPattern_P18_RenderCoherence (this file) catches
//     drift in `make test` / CI, instead of waiting for the operator
//     to manually run `force render-rules --check`.
//
// Mirrors the schema-parity pattern (TestSchemaParity in
// internal/store/schema_parity_test.go): two sources must agree, the
// test fails loudly if they don't, the remedy is one command.
//
// Pattern P18 graduates to a BoS commit-time rule when D4 ships,
// alongside the existing pattern test inventory.
package audittools

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"force-orchestrator/internal/agents"
	"force-orchestrator/internal/store"
)

// TestPattern_P18_RenderCoherence asserts on-disk auto-generated files
// byte-equal what the audit slice renders against a fresh in-memory DB.
func TestPattern_P18_RenderCoherence(t *testing.T) {
	ctx := context.Background()
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	if _, err := store.BootstrapFleetRules(ctx, db, ""); err != nil {
		t.Fatalf("bootstrap audit slice: %v", err)
	}

	repoRoot := moduleRoot(t)

	type target struct {
		path string
		want []byte
	}
	var targets []target

	claudeMd, err := agents.RenderClaudeMdFile(ctx, db)
	if err != nil {
		t.Fatalf("render CLAUDE.md: %v", err)
	}
	targets = append(targets, target{path: "CLAUDE.md", want: claudeMd})

	fixLog, err := agents.RenderFixLog(ctx, db)
	if err != nil {
		t.Fatalf("render FIX-LOG.md: %v", err)
	}
	targets = append(targets, target{path: "FIX-LOG.md", want: fixLog})

	// SENATE.md: render only if at least one senate-md-file rule is
	// active. Until the first PromotionProposal ratifies, the rule
	// set is empty and the renderer returns nil — there should be no
	// SENATE.md on disk to compare against. Pattern P34 enforces that
	// Senators cannot self-promote, so this branch stays inert in the
	// pre-ratification steady state.
	senateMd, err := agents.RenderSenateMdFile(ctx, db)
	if err != nil {
		t.Fatalf("render SENATE.md: %v", err)
	}
	if senateMd != nil {
		targets = append(targets, target{path: "SENATE.md", want: senateMd})
	}

	perDomain, err := agents.RenderPerDomainDocs(ctx, db)
	if err != nil {
		t.Fatalf("render per-domain docs: %v", err)
	}
	perDomainPaths := make([]string, 0, len(perDomain))
	for p := range perDomain {
		perDomainPaths = append(perDomainPaths, p)
	}
	sort.Strings(perDomainPaths) // stable failure ordering
	for _, p := range perDomainPaths {
		targets = append(targets, target{path: p, want: perDomain[p]})
	}

	var failures []string
	for _, tgt := range targets {
		got, rerr := os.ReadFile(filepath.Join(repoRoot, tgt.path))
		if rerr != nil {
			failures = append(failures, fmt.Sprintf("%s: read failed: %v", tgt.path, rerr))
			continue
		}
		if !bytes.Equal(got, tgt.want) {
			failures = append(failures, fmt.Sprintf(
				"%s: on-disk content differs from audit-slice render.\n"+
					"  Fix: `make render-rules` and re-stage the resulting files.\n"+
					"  First differing lines:\n%s",
				tgt.path, firstDiffLines(got, tgt.want, 30)))
		}
	}

	if len(failures) > 0 {
		t.Fatalf("Render coherence violated:\n\n%s", strings.Join(failures, "\n\n"))
	}
}

// TestPattern_P18_DetectsInjectedDrift proves the comparison helper
// actually reports drift when there is drift. Without this, a future
// refactor that silently neutered firstDiffLines (or replaced
// bytes.Equal with a pass-through) would leave P18 toothless.
func TestPattern_P18_DetectsInjectedDrift(t *testing.T) {
	got := []byte("alpha\nbravo\ncharlie\n")
	want := []byte("alpha\nBRAVO\ncharlie\n")
	if bytes.Equal(got, want) {
		t.Fatal("test fixture broken: got and want must differ")
	}
	report := firstDiffLines(got, want, 30)
	if !strings.Contains(report, "bravo") || !strings.Contains(report, "BRAVO") {
		t.Errorf("firstDiffLines did not surface the differing lines.\n  got fixture: %q\n  want fixture: %q\n  report: %q",
			got, want, report)
	}
}

// firstDiffLines returns a short markdown-friendly snippet showing the
// first byte-divergent line between got and want, plus a few lines of
// context. Caps at maxLines so an enormous diff doesn't drown the
// failure log.
func firstDiffLines(got, want []byte, maxLines int) string {
	gotLines := strings.Split(string(got), "\n")
	wantLines := strings.Split(string(want), "\n")
	n := len(gotLines)
	if len(wantLines) < n {
		n = len(wantLines)
	}
	first := -1
	for i := 0; i < n; i++ {
		if gotLines[i] != wantLines[i] {
			first = i
			break
		}
	}
	if first < 0 {
		// Length differs; surface the trailing tail.
		first = n
	}
	start := first - 2
	if start < 0 {
		start = 0
	}
	end := first + maxLines
	if end > len(gotLines) {
		end = len(gotLines)
	}
	wantEnd := first + maxLines
	if wantEnd > len(wantLines) {
		wantEnd = len(wantLines)
	}

	var b strings.Builder
	fmt.Fprintf(&b, "    --- on-disk lines %d..%d ---\n", start+1, end)
	for i := start; i < end; i++ {
		marker := "  "
		if i == first {
			marker = "→ "
		}
		fmt.Fprintf(&b, "    %s%4d: %s\n", marker, i+1, gotLines[i])
	}
	fmt.Fprintf(&b, "    --- audit-slice lines %d..%d ---\n", start+1, wantEnd)
	for i := start; i < wantEnd; i++ {
		marker := "  "
		if i == first {
			marker = "→ "
		}
		fmt.Fprintf(&b, "    %s%4d: %s\n", marker, i+1, wantLines[i])
	}
	return b.String()
}
