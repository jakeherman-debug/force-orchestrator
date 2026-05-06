// D13 P1 — documentation structure substrate.
//
// These tests are the contract that P2 (parallel content migration), P3
// (drift detection), and P4 (verifier + closure) all build on. The four
// guards below enforce:
//
//  1. The top-level README.md stays under the hard cap (200 lines) so
//     content is forced into the sharded docs/ tree.
//  2. docs/README.md exists as the canonical navigation entry point.
//  3. Each new docs subdirectory (agents/ subsystems/ patterns/ references/)
//     carries its own README.md mini-index.
//  4. Every doc placed into one of the four new subdirectories above
//     carries the metadata block (audience / scope / owner / last_reviewed)
//     at the top so audience expectations + ownership stay legible as the
//     tree grows.
//
// P3 will broaden the test surface (broken-link checker, orphan-doc
// checker, render-coherence-on-docs); P1's scope is the four guards
// above.
package audittools

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// readmeHardCapLines is the hard cap on the top-level README.md. Anything
// beyond this belongs in a sub-doc. Bumping this number without a written
// rationale in FIX-LOG.md is what the test is here to prevent — Force's
// observed-pre-D13 README was 1460 lines; the operator's premise is that
// every line over ~200 is content that should be reachable through the
// sharded docs/ tree, not the front door.
const readmeHardCapLines = 200

// TestReadmeSizeUnder200Lines asserts the top-level README.md is at most
// readmeHardCapLines long. Counting matches `wc -l` semantics: trailing
// newline does not introduce a phantom blank line.
func TestReadmeSizeUnder200Lines(t *testing.T) {
	root := moduleRoot(t)
	path := filepath.Join(root, "README.md")
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	// Match `wc -l`: count of '\n' bytes. A trailing newline is one '\n',
	// not a phantom blank line.
	count := strings.Count(string(body), "\n")
	if count > readmeHardCapLines {
		t.Errorf("README.md is %d lines; hard cap is %d.\n"+
			"Move long-form content into docs/subsystems/ or docs/agents/ or docs/references/.\n"+
			"The top-level README is the front door, not the manual.",
			count, readmeHardCapLines)
	}
}

// TestDocsIndexExists asserts docs/README.md is present — it is the
// canonical navigation entry into the docs/ tree.
func TestDocsIndexExists(t *testing.T) {
	root := moduleRoot(t)
	path := filepath.Join(root, "docs", "README.md")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("docs/README.md is the canonical docs index — it must exist: %v", err)
	}
	if info.Size() == 0 {
		t.Errorf("docs/README.md is empty; populate it with the canonical index")
	}
}

// docsSubdirsRequiringIndex are the per-category subdirectories under
// docs/ that must each carry their own README.md mini-index. P1 creates
// the directories + stub READMEs; P2 fills the contents.
var docsSubdirsRequiringIndex = []string{
	"agents",
	"subsystems",
	"patterns",
	"references",
}

// TestDocsSubdirsHaveIndex asserts every directory in
// docsSubdirsRequiringIndex has a README.md.
func TestDocsSubdirsHaveIndex(t *testing.T) {
	root := moduleRoot(t)
	for _, sub := range docsSubdirsRequiringIndex {
		path := filepath.Join(root, "docs", sub, "README.md")
		info, err := os.Stat(path)
		if err != nil {
			t.Errorf("docs/%s/README.md missing — every category subdir needs a mini-index: %v",
				sub, err)
			continue
		}
		if info.Size() == 0 {
			t.Errorf("docs/%s/README.md is empty; populate it with the category mini-index", sub)
		}
	}
}

// metadataKeys are the four keys that must appear inside the front-matter
// block of every doc placed under the new subdirectories. The block shape
// is YAML-style:
//
//	---
//	audience: operator | agent | both
//	scope: <one-sentence description>
//	owner: <deliverable id or subsystem>
//	last_reviewed: 2026-MM-DD
//	---
var metadataKeys = []string{"audience", "scope", "owner", "last_reviewed"}

// TestMetadataBlockOnAllNewDocs walks the four new subdirectories and
// asserts every *.md file (including the README.md mini-indexes) carries
// the four metadata keys inside the leading YAML front-matter block.
//
// The test is deliberately lenient about formatting: it scans the first
// 30 lines, looks for an opening `---`, then asserts the four keys appear
// as `<key>:` somewhere before the closing `---` (or before line 30 if
// no closing marker is present, to keep the check forward-compatible
// with future metadata extensions).
func TestMetadataBlockOnAllNewDocs(t *testing.T) {
	root := moduleRoot(t)
	for _, sub := range docsSubdirsRequiringIndex {
		dir := filepath.Join(root, "docs", sub)
		entries, err := os.ReadDir(dir)
		if err != nil {
			// Subdir-level missing is caught by TestDocsSubdirsHaveIndex;
			// don't double-fail here.
			continue
		}
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
				continue
			}
			path := filepath.Join(dir, e.Name())
			if err := checkMetadataBlock(path); err != nil {
				t.Errorf("docs/%s/%s: %v", sub, e.Name(), err)
			}
		}
	}
}

// checkMetadataBlock reads the first ~30 lines of a doc and verifies the
// four metadataKeys appear inside the leading front-matter block. Returns
// nil on success.
func checkMetadataBlock(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	const maxScanLines = 30
	lines := make([]string, 0, maxScanLines)
	for i := 0; i < maxScanLines && sc.Scan(); i++ {
		lines = append(lines, sc.Text())
	}
	if err := sc.Err(); err != nil {
		return err
	}

	// Find the leading `---` marker. Allow blank lines before it (some
	// editors strip leading blank but render-rules-style auto-generated
	// banners might prepend an HTML comment line we want to skip past).
	openIdx := -1
	for i, ln := range lines {
		t := strings.TrimSpace(ln)
		if t == "---" {
			openIdx = i
			break
		}
		// Tolerate an HTML comment block or blank lines before the front matter.
		if t == "" || strings.HasPrefix(t, "<!--") || strings.HasSuffix(t, "-->") {
			continue
		}
		// First non-blank, non-comment, non-`---` line means there is
		// no front-matter block.
		break
	}
	if openIdx < 0 {
		return missingFrontMatterError()
	}
	closeIdx := len(lines)
	for i := openIdx + 1; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) == "---" {
			closeIdx = i
			break
		}
	}
	block := strings.Join(lines[openIdx+1:closeIdx], "\n")

	for _, key := range metadataKeys {
		if !blockContainsKey(block, key) {
			return missingKeyError(key)
		}
	}
	return nil
}

func blockContainsKey(block, key string) bool {
	for _, ln := range strings.Split(block, "\n") {
		// Skip leading whitespace so indented metadata still counts.
		t := strings.TrimLeft(ln, " \t")
		if strings.HasPrefix(t, key+":") {
			return true
		}
	}
	return false
}

// distinct error helpers so the test failure message names which key /
// shape was missing without rebuilding a string in every call site.

type docMetadataError struct{ msg string }

func (e *docMetadataError) Error() string { return e.msg }

func missingFrontMatterError() error {
	return &docMetadataError{
		msg: "missing leading `---` YAML front-matter block.\n" +
			"Every doc under docs/agents/, docs/subsystems/, docs/patterns/, docs/references/ must start with:\n" +
			"---\naudience: operator | agent | both\nscope: <one-sentence description>\nowner: <deliverable id or subsystem>\nlast_reviewed: YYYY-MM-DD\n---",
	}
}

func missingKeyError(key string) error {
	return &docMetadataError{
		msg: "front-matter block missing required key `" + key + ":`",
	}
}
