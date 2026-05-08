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

// checkDocsH2Floor walks every non-index *.md file under
// docs/{agents,subsystems,patterns}/ rooted at rootDir and returns
// the (sorted) set of files that fall below minH2SectionsPerDoc, or
// that fail to read. Extracted from the production check so the
// sentinel can drive it against a synthetic TempDir.
func checkDocsH2Floor(rootDir string) []string {
	var failures []string
	for _, sub := range docsSubdirsRequiringIndex {
		// references/ has no per-file mini-index pattern today; skip.
		if sub == "references" {
			continue
		}
		dir := filepath.Join(rootDir, "docs", sub)
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
			rel := relTo(rootDir, abs)
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
	sort.Strings(failures)
	return failures
}

// checkDocsAutoRenderedExist returns the (sorted) paths in
// autoRenderedDocPaths that do NOT exist under rootDir.
func checkDocsAutoRenderedExist(rootDir string) []string {
	var missing []string
	for p := range autoRenderedDocPaths {
		if _, err := os.Stat(filepath.Join(rootDir, p)); err != nil {
			missing = append(missing, p)
		}
	}
	sort.Strings(missing)
	return missing
}

// checkDocsIndexCategoryPointers reads docs/README.md under rootDir
// and returns each docsSubdirsRequiringIndex entry that is NOT linked
// from the index. Returns a non-nil readErr if the index file itself
// can't be read.
func checkDocsIndexCategoryPointers(rootDir string) (missing []string, readErr error) {
	body, err := os.ReadFile(filepath.Join(rootDir, "docs", "README.md"))
	if err != nil {
		return nil, fmt.Errorf("read docs/README.md: %w", err)
	}
	text := string(body)
	for _, sub := range docsSubdirsRequiringIndex {
		// Accept either a bare-filename link or trailing-slash listing.
		needle1 := "(" + sub + "/README.md"
		needle2 := "(" + sub + "/)"
		if !strings.Contains(text, needle1) && !strings.Contains(text, needle2) {
			missing = append(missing, sub)
		}
	}
	sort.Strings(missing)
	return missing, nil
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
		failures := checkDocsH2Floor(root)
		if len(failures) == 0 {
			return
		}
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
		missing := checkDocsAutoRenderedExist(root)
		if len(missing) > 0 {
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
		missing, err := checkDocsIndexCategoryPointers(root)
		if err != nil {
			t.Fatalf("%v", err)
		}
		for _, sub := range missing {
			t.Errorf("docs/README.md does not link to docs/%s/README.md\n"+
				"  Add a link of the form `[%s](%s/README.md)` so the per-category\n"+
				"  mini-index is reachable in 1 hop from the canonical entry point.",
				sub, sub, sub)
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

// writeDocsArchitectureCompliantTree builds a fully compliant docs/
// tree under root. The sentinel uses this baseline and mutates one
// invariant at a time. Compliant = every authored doc under
// docs/{agents,subsystems,patterns}/ has >= minH2SectionsPerDoc H2
// sections; every entry in autoRenderedDocPaths exists on disk;
// docs/README.md links to each category mini-index.
func writeDocsArchitectureCompliantTree(t *testing.T, root string) {
	t.Helper()
	mk := func(rel, body string) {
		full := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", full, err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatalf("write %s: %v", full, err)
		}
	}
	// docs/README.md links to all four category mini-indexes.
	mk("docs/README.md", "# Docs Index\n\n"+
		"- [Agents](agents/README.md)\n"+
		"- [Subsystems](subsystems/README.md)\n"+
		"- [Patterns](patterns/README.md)\n"+
		"- [References](references/README.md)\n")
	// Each category subdirectory has an index.
	for _, sub := range docsSubdirsRequiringIndex {
		mk("docs/"+sub+"/README.md", "# "+sub+" index\n")
	}
	// One compliant authored doc per content category (skip
	// references which the production helper skips). Canonical
	// six-section template.
	canonical := "# Sample\n\n" +
		"## Status\n\nstable\n\n" +
		"## What this covers\n\nthings\n\n" +
		"## Until then\n\nn/a\n\n" +
		"## See also\n\nother docs\n\n" +
		"## When this page lands\n\nnow\n\n" +
		"## Final\n"
	mk("docs/agents/sample.md", canonical)
	mk("docs/subsystems/sample.md", canonical)
	mk("docs/patterns/sample.md", canonical)
	// Auto-rendered files exist (their content shape is checked
	// elsewhere; we just need them to be present on disk).
	for p := range autoRenderedDocPaths {
		mk(p, "# auto-rendered\n")
	}
}

// TestPattern_P_DocsArchitecture_DetectsInjectedDrift proves the
// three docs-architecture sub-checks would actually fire when each
// invariant is dropped. We build a compliant TempDir and mutate one
// invariant at a time, asserting the relevant helper surfaces the
// violation.
func TestPattern_P_DocsArchitecture_DetectsInjectedDrift(t *testing.T) {
	t.Run("H2-floor-violation-detected", func(t *testing.T) {
		root := t.TempDir()
		writeDocsArchitectureCompliantTree(t, root)
		// Replace one authored doc with a stub that has only 2 H2s.
		stubBody := "# stub\n\n## one\n\n## two\n"
		if err := os.WriteFile(filepath.Join(root, "docs", "subsystems", "sample.md"), []byte(stubBody), 0o644); err != nil {
			t.Fatal(err)
		}
		failures := checkDocsH2Floor(root)
		if len(failures) == 0 {
			t.Fatalf("checkDocsH2Floor accepted a 2-H2 doc; want failure containing 'docs/subsystems/sample.md'")
		}
		joined := strings.Join(failures, "\n")
		if !strings.Contains(joined, "docs/subsystems/sample.md") {
			t.Fatalf("failures %q does not mention docs/subsystems/sample.md", joined)
		}
		if !strings.Contains(joined, "only 2 H2") {
			t.Fatalf("failures %q does not report the 2-H2 count", joined)
		}
	})

	t.Run("H2-floor-passes-on-compliant-tree", func(t *testing.T) {
		root := t.TempDir()
		writeDocsArchitectureCompliantTree(t, root)
		failures := checkDocsH2Floor(root)
		if len(failures) != 0 {
			t.Fatalf("checkDocsH2Floor reported failures on compliant tree: %v", failures)
		}
	})

	t.Run("auto-rendered-missing-detected", func(t *testing.T) {
		root := t.TempDir()
		writeDocsArchitectureCompliantTree(t, root)
		// Pick any one auto-rendered path and remove it.
		var pick string
		for p := range autoRenderedDocPaths {
			pick = p
			break
		}
		if pick == "" {
			t.Skip("autoRenderedDocPaths empty; nothing to mutate")
		}
		if err := os.Remove(filepath.Join(root, pick)); err != nil {
			t.Fatal(err)
		}
		missing := checkDocsAutoRenderedExist(root)
		if len(missing) == 0 {
			t.Fatalf("checkDocsAutoRenderedExist did not report %s", pick)
		}
		found := false
		for _, m := range missing {
			if m == pick {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("missing %v does not contain %s", missing, pick)
		}
	})

	t.Run("auto-rendered-passes-on-compliant-tree", func(t *testing.T) {
		root := t.TempDir()
		writeDocsArchitectureCompliantTree(t, root)
		missing := checkDocsAutoRenderedExist(root)
		if len(missing) != 0 {
			t.Fatalf("checkDocsAutoRenderedExist reported missing on compliant tree: %v", missing)
		}
	})

	t.Run("docs-index-missing-category-pointer-detected", func(t *testing.T) {
		root := t.TempDir()
		writeDocsArchitectureCompliantTree(t, root)
		// Strip the patterns link from docs/README.md.
		body := "# Docs Index\n\n" +
			"- [Agents](agents/README.md)\n" +
			"- [Subsystems](subsystems/README.md)\n" +
			"- [References](references/README.md)\n"
		if err := os.WriteFile(filepath.Join(root, "docs", "README.md"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		missing, err := checkDocsIndexCategoryPointers(root)
		if err != nil {
			t.Fatal(err)
		}
		if len(missing) == 0 {
			t.Fatalf("checkDocsIndexCategoryPointers did not detect missing patterns link")
		}
		found := false
		for _, m := range missing {
			if m == "patterns" {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("missing %v does not include 'patterns'", missing)
		}
	})

	t.Run("docs-index-passes-on-compliant-tree", func(t *testing.T) {
		root := t.TempDir()
		writeDocsArchitectureCompliantTree(t, root)
		missing, err := checkDocsIndexCategoryPointers(root)
		if err != nil {
			t.Fatal(err)
		}
		if len(missing) != 0 {
			t.Fatalf("checkDocsIndexCategoryPointers reported %v on compliant tree", missing)
		}
	})

	t.Run("docs-index-missing-file-returns-error", func(t *testing.T) {
		root := t.TempDir()
		// No docs/README.md at all — checker must return readErr.
		_, err := checkDocsIndexCategoryPointers(root)
		if err == nil {
			t.Fatalf("checkDocsIndexCategoryPointers accepted missing docs/README.md")
		}
		if !strings.Contains(err.Error(), "docs/README.md") {
			t.Fatalf("error %q does not mention docs/README.md", err.Error())
		}
	})
}
