// D13 P3 — drift detection substrate (Pattern P-DocsBrokenLinks).
//
// Walks every Markdown file in the repo and verifies that every
// relative-path link `[text](path/to/file.md#anchor)` resolves:
//
//   - The target file exists.
//   - If `#anchor` is present, an H1/H2/H3 heading slugifying to the
//     anchor exists in the target file.
//
// External `http(s)://` links are SKIPPED — the operator's commit-time
// gate is not the right place to ping the network. Mailto + protocol-
// relative links are also skipped. Reference-style + bare-anchor (no
// path before `#`) links resolve against the source file itself.
//
// Drift class this catches:
//   - Wave B/C/D forward-references to docs that haven't shipped yet
//     (e.g. `subsystems/security.md`, `subsystems/dogs.md`).
//   - File renames that miss a callsite.
//   - Heading rename that orphans the existing `#anchor` link.
//
// Pair: TestPatternP_DocsOrphan (sibling file) — same shape, different
// invariant (every doc is reachable from its index).
package audittools

import (
	"bufio"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// linkRe matches `[text](url)` Markdown inline links. It deliberately
// rejects images (`![text](url)`) by requiring a non-`!` byte (or start
// of line) before the opening `[`. URL capture is non-greedy and stops
// at the first unescaped `)`.
//
// Caveats: this regex is intentionally simple. Markdown is messy; we
// accept the cost of a few false-positives on, say, code-fence content
// and handle them by allowlisting if-and-when they appear. The drift
// checker is a contract gate, not a parser.
var linkRe = regexp.MustCompile(`(^|[^!\\])\[([^\]\n]*)\]\(([^)\n]+)\)`)

// headingRe captures Markdown ATX headings (`# H1`, `## H2`, `### H3`).
// Setext-style headings (underline) are not used in this repo and are
// not accepted by the slugifier.
var headingRe = regexp.MustCompile(`^(#{1,6})\s+(.+?)\s*$`)

// directoriesToSkip are walked-past during the repo scan. .git, vendor,
// build outputs, parallel worktrees — none of these contain authored
// Markdown that needs link integrity.
var directoriesToSkip = map[string]struct{}{
	".git":              {},
	".fix-worktrees":    {},
	".force-worktrees":  {},
	".build-worktrees":  {},
	".d7-worktrees":     {},
	".claude":           {},
	"vendor":            {},
	"node_modules":      {},
	"bin":               {},
	"operator-archives": {}, // historical operator notes preserved for audit; not actively maintained
}

// fileRelOk is the allowlist of link targets that are NOT *.md but ARE
// expected to exist. Empty by default; populate if a load-bearing link
// to (e.g.) a YAML config needs to be checked.
var fileRelOk = map[string]struct{}{}

// brokenLinkAllowlist documents links the test deliberately tolerates.
// Each entry MUST come with a one-line rationale (the comment to its
// right) so future grep-readers know whether the deferral is still
// valid. The end-state goal is len(brokenLinkAllowlist) == 0.
//
// Entry shape: "<source-relpath>::<link-target>" — keyed on the source
// file so a copy-pasted link in two places is caught individually.
var brokenLinkAllowlist = map[string]string{}

// TestPatternP_DocsBrokenLinks asserts every relative-path Markdown
// link in the repo resolves to an extant file (and, if `#anchor`, an
// extant heading).
func TestPatternP_DocsBrokenLinks(t *testing.T) {
	root := moduleRoot(t)

	mdFiles, err := walkMarkdown(root)
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}

	// Pre-index headings per file so we don't re-read each target on
	// every link. Slug map: file → set-of-slugs.
	headingIndex := make(map[string]map[string]struct{})

	type breakage struct {
		source string
		line   int
		raw    string
		reason string
	}
	var failures []breakage

	for _, src := range mdFiles {
		body, rerr := os.ReadFile(src)
		if rerr != nil {
			t.Fatalf("read %s: %v", src, rerr)
		}
		lines := strings.Split(string(body), "\n")
		inFence := false
		for i, line := range lines {
			// Skip fenced code blocks — links inside a ``` block are
			// not Markdown links, they're documentation samples.
			if strings.HasPrefix(strings.TrimLeft(line, " \t"), "```") {
				inFence = !inFence
				continue
			}
			if inFence {
				continue
			}
			matches := linkRe.FindAllStringSubmatch(line, -1)
			for _, m := range matches {
				target := m[3]
				if shouldSkipLink(target) {
					continue
				}
				// Strip surrounding whitespace + any title attribute
				// (`url "title"`).
				target = stripTitle(strings.TrimSpace(target))
				path, anchor := splitAnchor(target)

				// Bare-anchor link (`#section`) resolves against the
				// source file itself.
				resolveSource := src
				if path != "" {
					resolveSource = resolvePath(src, path)
				}

				// Allowlist short-circuit, keyed on source-relpath +
				// raw target.
				key := relTo(root, src) + "::" + target
				if _, ok := brokenLinkAllowlist[key]; ok {
					continue
				}

				if path != "" {
					if _, err := os.Stat(resolveSource); err != nil {
						failures = append(failures, breakage{
							source: relTo(root, src),
							line:   i + 1,
							raw:    target,
							reason: fmt.Sprintf("file not found: %s", relTo(root, resolveSource)),
						})
						continue
					}
				}

				if anchor == "" {
					continue
				}

				// Headings only meaningful for *.md targets. For
				// anything else (e.g. *.go), accept the file-exists
				// check and move on.
				if !strings.HasSuffix(strings.ToLower(resolveSource), ".md") {
					continue
				}

				slugs, ok := headingIndex[resolveSource]
				if !ok {
					slugs, err = readHeadingSlugs(resolveSource)
					if err != nil {
						failures = append(failures, breakage{
							source: relTo(root, src),
							line:   i + 1,
							raw:    target,
							reason: fmt.Sprintf("could not read headings of %s: %v", relTo(root, resolveSource), err),
						})
						continue
					}
					headingIndex[resolveSource] = slugs
				}
				if _, ok := slugs[anchor]; !ok {
					failures = append(failures, breakage{
						source: relTo(root, src),
						line:   i + 1,
						raw:    target,
						reason: fmt.Sprintf("anchor #%s not found in %s", anchor, relTo(root, resolveSource)),
					})
				}
			}
		}
	}

	if len(failures) == 0 {
		return
	}

	sort.Slice(failures, func(i, j int) bool {
		if failures[i].source != failures[j].source {
			return failures[i].source < failures[j].source
		}
		return failures[i].line < failures[j].line
	})

	var b strings.Builder
	fmt.Fprintf(&b, "TestPatternP_DocsBrokenLinks FAILED — %d broken Markdown link(s):\n\n", len(failures))
	for _, f := range failures {
		fmt.Fprintf(&b, "  %s:%d  →  [%s]  (%s)\n", f.source, f.line, f.raw, f.reason)
	}
	b.WriteString("\nFix options:\n")
	b.WriteString("  (a) create the missing file (with metadata block + at least `## Status: Stub`),\n")
	b.WriteString("  (b) update the link to the correct path / anchor, or\n")
	b.WriteString("  (c) remove the link.\n")
	b.WriteString("\nDo NOT add to brokenLinkAllowlist without a one-line rationale — the goal is zero allowlist entries.\n")
	t.Error(b.String())
}

// shouldSkipLink returns true for link targets the checker intentionally
// ignores.
func shouldSkipLink(target string) bool {
	t := strings.TrimSpace(target)
	if t == "" {
		return true
	}
	low := strings.ToLower(t)
	if strings.HasPrefix(low, "http://") || strings.HasPrefix(low, "https://") ||
		strings.HasPrefix(low, "mailto:") || strings.HasPrefix(low, "tel:") {
		return true
	}
	// Protocol-relative.
	if strings.HasPrefix(t, "//") {
		return true
	}
	return false
}

// stripTitle removes a trailing `"title"` attribute Markdown allows in
// inline links: `[text](url "title")`.
func stripTitle(target string) string {
	// Two title delimiters: " and '.
	for _, q := range []string{" \"", " '"} {
		if i := strings.LastIndex(target, q); i > 0 {
			return strings.TrimSpace(target[:i])
		}
	}
	return target
}

// splitAnchor splits `path#anchor` into (path, anchor). If there is no
// `#`, returns (target, ""). Bare-anchor `#anchor` returns ("", anchor).
func splitAnchor(target string) (string, string) {
	if idx := strings.Index(target, "#"); idx >= 0 {
		return target[:idx], target[idx+1:]
	}
	return target, ""
}

// resolvePath joins a relative link target against the *directory* of
// the source file and returns the absolute path. Absolute paths are
// returned as-is (after rooting against `/`).
func resolvePath(source, target string) string {
	if filepath.IsAbs(target) {
		return target
	}
	return filepath.Join(filepath.Dir(source), target)
}

// readHeadingSlugs reads target as a Markdown file and returns the set
// of GitHub-style anchor slugs for every H1/H2/H3 heading.
func readHeadingSlugs(path string) (map[string]struct{}, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	out := make(map[string]struct{})
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1<<16), 1<<20)
	inFence := false
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(strings.TrimLeft(line, " \t"), "```") {
			inFence = !inFence
			continue
		}
		if inFence {
			continue
		}
		m := headingRe.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		out[slugify(m[2])] = struct{}{}
	}
	return out, sc.Err()
}

// slugify turns "Hello, World!" into "hello-world" — GitHub's heading-
// anchor algorithm (close enough for the gate). Lowercase; strip
// punctuation except `-` and `_`; collapse whitespace to `-`.
func slugify(heading string) string {
	heading = strings.ToLower(heading)
	// Strip leading anchor link `[#](...)` if present (cosmetic on
	// some headings) — defensive, our docs don't use this.
	var b strings.Builder
	prevDash := false
	for _, r := range heading {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			prevDash = false
		case r == '-', r == '_':
			b.WriteRune(r)
			prevDash = false
		case r == ' ', r == '\t':
			if !prevDash {
				b.WriteRune('-')
				prevDash = true
			}
		default:
			// drop punctuation
		}
	}
	return strings.Trim(b.String(), "-")
}

// TestPatternP_DocsBrokenLinks_DetectsInjectedDrift proves the link
// regex + heading slugifier actually reject what they should — a
// future refactor that silently neutered linkRe (or replaced
// resolvePath with the identity function) would otherwise leave
// TestPatternP_DocsBrokenLinks toothless.
func TestPatternP_DocsBrokenLinks_DetectsInjectedDrift(t *testing.T) {
	tmp := t.TempDir()
	src := filepath.Join(tmp, "src.md")
	bad := "# Source\n\nLink to [missing](does-not-exist.md).\n"
	if err := os.WriteFile(src, []byte(bad), 0644); err != nil {
		t.Fatalf("write src: %v", err)
	}
	body, _ := os.ReadFile(src)
	matches := linkRe.FindAllStringSubmatch(string(body), -1)
	if len(matches) != 1 {
		t.Fatalf("regex broken: expected 1 link match, got %d", len(matches))
	}
	target := stripTitle(strings.TrimSpace(matches[0][3]))
	path, _ := splitAnchor(target)
	abs := resolvePath(src, path)
	if _, err := os.Stat(abs); err == nil {
		t.Fatalf("test fixture broken: target %s should not exist", abs)
	}
	// Confirm slugify behaves on a representative heading.
	if got := slugify("Hello, World! 1-2"); got != "hello-world-1-2" {
		t.Errorf("slugify: got %q want hello-world-1-2", got)
	}
	// Confirm the source-file fenced-code-block tracker drops `## ` inside ```.
	insideFence := "```\n## not a heading\n```\n## real heading\n"
	tmpMd := filepath.Join(tmp, "fence.md")
	if err := os.WriteFile(tmpMd, []byte(insideFence), 0644); err != nil {
		t.Fatalf("write fence.md: %v", err)
	}
	slugs, err := readHeadingSlugs(tmpMd)
	if err != nil {
		t.Fatalf("readHeadingSlugs: %v", err)
	}
	if _, ok := slugs["not-a-heading"]; ok {
		t.Errorf("readHeadingSlugs counted a heading inside a fenced code block")
	}
	if _, ok := slugs["real-heading"]; !ok {
		t.Errorf("readHeadingSlugs missed the real heading outside the fence")
	}
}

// walkMarkdown returns the absolute paths of every *.md file under
// root, skipping directoriesToSkip.
func walkMarkdown(root string) ([]string, error) {
	var out []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, werr error) error {
		if werr != nil {
			return werr
		}
		if d.IsDir() {
			if _, skip := directoriesToSkip[d.Name()]; skip {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(strings.ToLower(d.Name()), ".md") {
			return nil
		}
		out = append(out, path)
		return nil
	})
	return out, err
}

// relTo wraps rel(root,path) for stable failure-message paths.
func relTo(root, path string) string { return rel(root, path) }
