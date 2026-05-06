// D13 P3 — drift detection substrate (Pattern P-DocsArchitecture).
//
// Consolidated structural invariants for the docs/ tree. The four P1
// guards (README cap, docs/README.md exists, subdir indexes, metadata
// blocks) live in audit_pattern_p_docs_test.go and remain the
// authoritative tests for those invariants — this file does not
// duplicate them. P-DocsArchitecture adds the structural-completeness
// invariant that P1 deliberately deferred:
//
//   - Every authored doc under docs/{agents,subsystems,patterns}/
//     (excluding README.md indexes) carries at least
//     `minH2SectionsPerDoc` H2 sections — the cheapest signal that the
//     doc is structurally complete vs. a bare empty stub.
//
// Auto-rendered files (managed by `make render-rules`) are exempt from
// the H2 cap because their structure is determined by the audit-slice
// content, not by the per-doc author. The exempt list is the same
// list Pattern P18 (TestPattern_P18_RenderCoherence) already byte-checks.
//
// The H2 floor (4) was chosen by surveying the actual fleet content
// after D13 P2 Wave B/C/D; every authored subsystem/agent/pattern doc
// today carries 6+ H2 sections (canonical six-section template), so 4
// gives one section of slack for legitimate variation while still
// rejecting "two paragraphs and a code block" stubs that would slip
// past the metadata-block check.
//
// Stub files (the placeholder navigation slots authored by D13 P3 to
// clear known-broken links) carry exactly the canonical six H2 sections
// and pass cleanly. If a future deliverable adds a stub it follows the
// same template — see e.g. docs/subsystems/security.md.
package audittools

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// minH2SectionsPerDoc is the floor for H2 section count in any non-
// index *.md file under the four canonical subdirectories. Survey of
// actual fleet content (D13 P3 lands with every doc at 6+); 4 is the
// chosen contract floor because it leaves slack for legitimate
// variation while rejecting empty stubs.
const minH2SectionsPerDoc = 4

// h2ExemptFiles is the set of *.md files under docs/{agents,
// subsystems,patterns}/ that the H2-floor invariant intentionally
// skips. The set is currently empty — every authored doc in those
// directories should carry the minimum.
//
// Auto-rendered files are NOT in these subdirectories (they live at
// docs/*.md, not docs/<sub>/*.md), so they don't need an entry here;
// the structural-completeness invariant simply doesn't apply to them.
//
// Entry shape: docs-relpath (e.g. "docs/subsystems/foo.md").
var h2ExemptFiles = map[string]struct{}{}

// autoRenderedDocPaths lists the docs/*.md files that are auto-
// rendered from the FleetRules audit slice via `make render-rules`.
// Their structural shape is determined by the audit slice, not by an
// author. This list is byte-checked by Pattern P18
// (TestPattern_P18_RenderCoherence); P-DocsArchitecture honors the
// same exemption when checking metadata blocks on top-level docs/*.md
// files.
//
// Source of truth: every FleetRules row with
// RenderTo='per-domain-doc:<path>' in
// internal/store/fleet_rules_audit.go. The list below is the post-D13
// snapshot; if a new per-domain-doc row lands, both this list and the
// FleetRules audit slice change in the same commit.
var autoRenderedDocPaths = map[string]struct{}{
	"docs/dashboard-conventions.md": {},
	"docs/pr-flow-invariants.md":    {},
	"docs/self-healing.md":          {},
}

// TestPatternP_DocsArchitecture is the consolidated structural
// invariant for the docs/ tree. The four P1 guards (README cap, index
// files, metadata blocks) remain in audit_pattern_p_docs_test.go;
// P-DocsArchitecture adds H2-section-floor + auto-rendered exemption
// honoring.
func TestPatternP_DocsArchitecture(t *testing.T) {
	root := moduleRoot(t)

	// Sub-test 1: H2-section floor on authored subsystem/agent/pattern docs.
	t.Run("H2SectionFloor", func(t *testing.T) {
		var failures []string
		for _, sub := range docsSubdirsRequiringIndex {
			// references/ has no per-file mini-index pattern today; skip.
			if sub == "references" {
				continue
			}
			dir := filepath.Join(root, "docs", sub)
			entries, err := os.ReadDir(dir)
			if err != nil {
				continue // covered by sibling tests
			}
			for _, e := range entries {
				if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
					continue
				}
				if e.Name() == "README.md" {
					continue
				}
				abs := filepath.Join(dir, e.Name())
				rel := relTo(root, abs)
				if _, ok := h2ExemptFiles[rel]; ok {
					continue
				}
				count, err := countH2Sections(abs)
				if err != nil {
					failures = append(failures, fmt.Sprintf("%s: read failed: %v", rel, err))
					continue
				}
				if count < minH2SectionsPerDoc {
					failures = append(failures, fmt.Sprintf(
						"%s: only %d H2 section(s); minimum is %d",
						rel, count, minH2SectionsPerDoc))
				}
			}
		}
		if len(failures) == 0 {
			return
		}
		sort.Strings(failures)
		t.Errorf("H2 section floor violated:\n  %s\n\n"+
			"Every doc under docs/{agents,subsystems,patterns}/ (except README.md indexes)\n"+
			"must carry at least %d `## H2` headings — the cheapest signal that the doc\n"+
			"is structurally complete. Stubs use the canonical six-section template:\n"+
			"  ## Status: Stub  ## What this will cover  ## Until then  ## See also  ## When this page lands  (+ one of your choosing)",
			strings.Join(failures, "\n  "), minH2SectionsPerDoc)
	})

	// Sub-test 2: auto-rendered exempt list is honored.
	//
	// The contract is: every file in autoRenderedDocPaths exists on
	// disk (i.e. the rendered output is present), and the list does
	// not include any file that should be hand-authored. We don't
	// re-do P18's byte check here; P18 is already the authoritative
	// gate. We just verify the exempt list matches reality.
	t.Run("AutoRenderedExemptionHonored", func(t *testing.T) {
		var missing []string
		for p := range autoRenderedDocPaths {
			if _, err := os.Stat(filepath.Join(root, p)); err != nil {
				missing = append(missing, p)
			}
		}
		if len(missing) > 0 {
			sort.Strings(missing)
			t.Errorf("autoRenderedDocPaths references files that do not exist:\n  %s\n\n"+
				"Either remove the entry (the FleetRules audit slice no longer renders it)\n"+
				"or run `make render-rules` to regenerate.",
				strings.Join(missing, "\n  "))
		}
	})

	// Sub-test 3: docs/README.md carries the canonical sections.
	//
	// "Canonical" = the minimum operator-vs-agent split + at least
	// one of the per-category index pointers. We assert the H2 set
	// includes a navigation entry for each of the four required
	// subdirectories' README.md indexes — i.e. there is a path from
	// docs/README.md to every category mini-index in 1 hop. The
	// orphan checker covers the per-file reachability; this sub-test
	// covers the per-category reachability.
	t.Run("DocsIndexHasCategoryPointers", func(t *testing.T) {
		body, err := os.ReadFile(filepath.Join(root, "docs", "README.md"))
		if err != nil {
			t.Fatalf("read docs/README.md: %v", err)
		}
		text := string(body)
		// We look for a Markdown link to each category README. The
		// link can be a bare filename (`patterns/README.md`) or with
		// a trailing slash that GitHub treats as the directory
		// listing — accept either.
		for _, sub := range docsSubdirsRequiringIndex {
			needle1 := "(" + sub + "/README.md"
			needle2 := "(" + sub + "/)"
			if !strings.Contains(text, needle1) && !strings.Contains(text, needle2) {
				t.Errorf("docs/README.md does not link to docs/%s/README.md\n"+
					"  Add a link of the form `[%s](%s/README.md)` so the per-category\n"+
					"  mini-index is reachable in 1 hop from the canonical entry point.",
					sub, sub, sub)
			}
		}
	})
}

// countH2Sections returns the number of `## H2` ATX headings in the
// file at path. Skips fenced code blocks so a `## inside ```...```` is
// not double-counted.
func countH2Sections(path string) (int, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1<<16), 1<<20)
	inFence := false
	count := 0
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(strings.TrimLeft(line, " \t"), "```") {
			inFence = !inFence
			continue
		}
		if inFence {
			continue
		}
		// Strict H2: exactly two `#` followed by a space.
		t := strings.TrimLeft(line, " \t")
		if strings.HasPrefix(t, "## ") && !strings.HasPrefix(t, "### ") {
			count++
		}
	}
	return count, sc.Err()
}
