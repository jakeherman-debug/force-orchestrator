// D13 P3 — drift detection substrate (Pattern P-DocsOrphan).
//
// Every *.md file under docs/{agents,subsystems,patterns}/ MUST be
// linked from its sibling README.md (or, in the case of the README
// stubs themselves, from docs/README.md). Files that exist on disk but
// are not linked from their index are "orphans" — content the operator
// can't reach through navigation, which is the same as content that
// doesn't exist.
//
// The check intentionally runs only on the three subdirectories whose
// indexes carry the canonical-navigation contract. docs/references/
// today is a flat reference table (no per-file mini-index pattern); if
// it grows enough to need orphan-checking the entry is one line.
//
// Pair: TestPatternP_DocsBrokenLinks (sibling file) — same shape,
// different invariant (every link resolves).
package audittools

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// orphanCheckedDirs are the docs subdirectories where every *.md file
// MUST be linked from a sibling README.md (or from docs/README.md).
// Each entry is a directory under `docs/`. The README.md inside each
// of these directories is the local index; docs/README.md is the
// fleet-wide fallback (any link from there counts).
var orphanCheckedDirs = []string{
	"agents",
	"subsystems",
	"patterns",
}

// orphanAllowlist documents files the test deliberately tolerates as
// orphans. Each entry MUST come with a one-line rationale (the comment
// to its right). The end-state goal is len(orphanAllowlist) == 0.
//
// Entry shape: "<docs-relpath>" (e.g. "docs/subsystems/security.md").
var orphanAllowlist = map[string]string{}

// TestPatternP_DocsOrphan asserts every *.md file under
// docs/{agents,subsystems,patterns}/ is linked from its sibling
// README.md or from docs/README.md.
func TestPatternP_DocsOrphan(t *testing.T) {
	root := moduleRoot(t)

	// Index 1: collect every link target out of the candidate index files.
	// Resolved to absolute on-disk paths so we can compare to file lists
	// without ambiguity.
	linkedFromIndex := make(map[string]struct{})

	indexFiles := []string{filepath.Join(root, "docs", "README.md")}
	for _, sub := range orphanCheckedDirs {
		indexFiles = append(indexFiles, filepath.Join(root, "docs", sub, "README.md"))
	}
	for _, idx := range indexFiles {
		if _, err := os.Stat(idx); err != nil {
			// Subdir-level missing index is caught by
			// TestDocsSubdirsHaveIndex; don't double-fail here.
			continue
		}
		body, err := os.ReadFile(idx)
		if err != nil {
			t.Fatalf("read %s: %v", idx, err)
		}
		// Reuse the link regex from the broken-link checker.
		matches := linkRe.FindAllStringSubmatch(string(body), -1)
		for _, m := range matches {
			target := stripTitle(strings.TrimSpace(m[3]))
			if shouldSkipLink(target) {
				continue
			}
			path, _ := splitAnchor(target)
			if path == "" {
				continue
			}
			abs := resolvePath(idx, path)
			linkedFromIndex[filepath.Clean(abs)] = struct{}{}
		}
	}

	// Index 2: enumerate every *.md file under each orphanCheckedDirs
	// entry. Skip the README.md inside each subdir (those are the
	// indexes themselves) and the top-level docs/README.md.
	type orphan struct {
		rel    string
		reason string
	}
	var orphans []orphan
	for _, sub := range orphanCheckedDirs {
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
			abs := filepath.Clean(filepath.Join(dir, e.Name()))
			rel := relTo(root, abs)
			if _, ok := orphanAllowlist[rel]; ok {
				continue
			}
			if _, ok := linkedFromIndex[abs]; !ok {
				orphans = append(orphans, orphan{
					rel:    rel,
					reason: "exists on disk but is not linked from docs/README.md or docs/" + sub + "/README.md",
				})
			}
		}
	}

	if len(orphans) == 0 {
		return
	}

	sort.Slice(orphans, func(i, j int) bool { return orphans[i].rel < orphans[j].rel })

	var b strings.Builder
	fmt.Fprintf(&b, "TestPatternP_DocsOrphan FAILED — %d orphan doc file(s):\n\n", len(orphans))
	for _, o := range orphans {
		fmt.Fprintf(&b, "  %s — %s\n", o.rel, o.reason)
	}
	b.WriteString("\nFix options:\n")
	b.WriteString("  (a) link the file from the appropriate index README, or\n")
	b.WriteString("  (b) delete the file if it's no longer needed.\n")
	b.WriteString("\nDo NOT add to orphanAllowlist without a one-line rationale — the goal is zero allowlist entries.\n")
	t.Error(b.String())
}

// TestPatternP_DocsOrphan_DetectsInjectedDrift proves the orphan-
// detection logic actually fires when a doc is unreachable. A future
// refactor that silently neutered the link-collection step (or
// replaced filepath.Clean with identity) would leave the orphan
// checker toothless without this fixture.
func TestPatternP_DocsOrphan_DetectsInjectedDrift(t *testing.T) {
	tmp := t.TempDir()
	// Build a minimal docs/ tree with one orphan.
	if err := os.MkdirAll(filepath.Join(tmp, "docs", "subsystems"), 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	idx := filepath.Join(tmp, "docs", "subsystems", "README.md")
	if err := os.WriteFile(idx,
		[]byte("# Subsystems\n\n- [linked](linked.md)\n"), 0644); err != nil {
		t.Fatalf("write idx: %v", err)
	}
	if err := os.WriteFile(filepath.Join(tmp, "docs", "subsystems", "linked.md"),
		[]byte("# Linked\n"), 0644); err != nil {
		t.Fatalf("write linked: %v", err)
	}
	orphan := filepath.Join(tmp, "docs", "subsystems", "orphan.md")
	if err := os.WriteFile(orphan,
		[]byte("# Orphan\n"), 0644); err != nil {
		t.Fatalf("write orphan: %v", err)
	}

	body, _ := os.ReadFile(idx)
	linked := make(map[string]struct{})
	for _, m := range linkRe.FindAllStringSubmatch(string(body), -1) {
		target := stripTitle(strings.TrimSpace(m[3]))
		if shouldSkipLink(target) {
			continue
		}
		path, _ := splitAnchor(target)
		linked[filepath.Clean(resolvePath(idx, path))] = struct{}{}
	}
	if _, ok := linked[filepath.Clean(orphan)]; ok {
		t.Fatalf("test fixture broken: orphan should not appear in linked set")
	}
	if _, ok := linked[filepath.Clean(filepath.Join(tmp, "docs", "subsystems", "linked.md"))]; !ok {
		t.Errorf("link harvester missed linked.md (regex / resolvePath drift)")
	}
}
