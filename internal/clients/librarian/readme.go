// Package librarian — shared README scraping helpers.
//
// These helpers are the deterministic fallback path used by
// GenerateRepoDescription (this package) AND
// cmd/force/add_repo_smart_defaults.go (the smart-default derivation in
// `force add-repo`). The sweep-E refactor moved them out of
// cmd/force/add_repo_smart_defaults.go so the librarian's LLM-backed
// path AND its deterministic fallback share a single implementation —
// when LIVE_HAIKU is disabled (tests / offline operator) the librarian
// returns the same string the cmd/force CLI would have produced
// pre-sweep-E.
//
// Anti-cheat: the helpers are pure functions of their input bytes; no
// network, no DB, no LLM. The cmd/force tests that previously exercised
// the regex scraper now exercise these functions through a thin
// re-export so the regression surface is preserved.
package librarian

import (
	"os"
	"path/filepath"
	"strings"
)

// ReadmeDescriptionMaxLen caps derived descriptions. Long enough to be
// useful in dashboard tables and `force repos` output, short enough
// that the operator can still read the row. The trailing "…"
// indicates truncation.
const ReadmeDescriptionMaxLen = 200

// ScrapeReadmeFirstParagraph is the deterministic README → one-line
// description scraper. Searches for README.md / README / readme.md
// (case-insensitive) at dir, reads the file, and runs the markdown
// extractor. Returns "" when no README is present or the README has
// no usable paragraph (HTML-comment-only, badge-only, etc.).
//
// This is the function the librarian's GenerateRepoDescription falls
// back to when LIVE_HAIKU is disabled or the LLM call fails. The
// cmd/force CLI calls it through deriveRepoDescriptionWithLibrarian
// when no librarian client is wired (test path).
func ScrapeReadmeFirstParagraph(absPath string) string {
	if absPath == "" {
		return ""
	}
	readmePath := FindReadme(absPath)
	if readmePath == "" {
		return ""
	}
	data, err := os.ReadFile(readmePath)
	if err != nil {
		return ""
	}
	return ExtractFirstParagraph(string(data))
}

// FindReadme walks the immediate children of dir and returns the
// first one whose name matches a README variant (case-insensitive).
// README.md is preferred over README, but if both exist we take
// whichever ReadDir lists first — operationally good enough; the
// helper doesn't claim a strict priority since the README content is
// what matters.
func FindReadme(dir string) string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	// Two-pass so "README.md" wins over "README" / "readme" if both exist.
	var fallback string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		lower := strings.ToLower(name)
		if lower == "readme.md" || lower == "readme.markdown" {
			return filepath.Join(dir, name)
		}
		if lower == "readme" || lower == "readme.txt" || lower == "readme.rst" {
			if fallback == "" {
				fallback = filepath.Join(dir, name)
			}
		}
	}
	return fallback
}

// ReadReadmeBytes reads up to maxBytes of the README at dir. Returns
// (data, path, ok). When the README is absent or unreadable returns
// ("", "", false). The librarian's LLM path uses this directly so the
// prompt size is bounded without re-implementing findReadme.
func ReadReadmeBytes(dir string, maxBytes int) (string, string, bool) {
	path := FindReadme(dir)
	if path == "" {
		return "", "", false
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", "", false
	}
	if maxBytes > 0 && len(data) > maxBytes {
		data = data[:maxBytes]
	}
	return string(data), path, true
}

// ExtractFirstParagraph parses README markdown text and returns the
// first non-blank paragraph after skipping:
//   - YAML frontmatter (leading `---\n...\n---`)
//   - HTML comments (`<!-- ... -->`, possibly multi-line)
//   - badge / image lines (`[![...]` or `![...`)
//   - blank lines
//   - Markdown headings (`#`, `##`, etc.)
//   - Horizontal rules (`---`, `***`, `___`)
//
// The returned paragraph has its internal newlines folded to single
// spaces and is truncated to ReadmeDescriptionMaxLen (with a trailing
// `…` if truncation happened). Returns "" when no usable paragraph
// exists.
func ExtractFirstParagraph(content string) string {
	content = strings.ReplaceAll(content, "\r\n", "\n")

	// Strip YAML frontmatter.
	if strings.HasPrefix(content, "---\n") {
		// Find the closing `---` on its own line.
		rest := content[4:]
		if i := strings.Index(rest, "\n---\n"); i != -1 {
			content = rest[i+5:]
		} else if strings.HasSuffix(rest, "\n---") {
			content = ""
		}
	}

	// Strip HTML comments (greedy across lines).
	for {
		start := strings.Index(content, "<!--")
		if start == -1 {
			break
		}
		end := strings.Index(content[start:], "-->")
		if end == -1 {
			content = content[:start]
			break
		}
		content = content[:start] + content[start+end+3:]
	}

	lines := strings.Split(content, "\n")

	// Walk lines, accumulating the first usable paragraph. A "paragraph"
	// is a run of consecutive non-skipped non-blank lines; the run ends
	// at a blank line or end of file.
	var (
		para []string
		seen bool
	)
	for _, raw := range lines {
		line := strings.TrimSpace(raw)
		if line == "" {
			if seen {
				break
			}
			continue
		}
		if IsSkippableReadmeLine(line) {
			// A skippable line still ends the paragraph if we'd already
			// collected text — treat it like a paragraph separator.
			if seen {
				break
			}
			continue
		}
		seen = true
		para = append(para, line)
	}
	if len(para) == 0 {
		return ""
	}
	joined := strings.Join(para, " ")
	// Collapse interior whitespace runs to a single space.
	joined = CollapseWhitespace(joined)
	joined = strings.TrimSpace(joined)
	if joined == "" {
		return ""
	}

	// Truncate. Use rune-aware slicing so multi-byte chars don't get
	// cut in the middle.
	runes := []rune(joined)
	if len(runes) > ReadmeDescriptionMaxLen {
		runes = runes[:ReadmeDescriptionMaxLen]
		// Trim trailing whitespace before adding the ellipsis so we
		// don't produce "foo …".
		for len(runes) > 0 && (runes[len(runes)-1] == ' ' || runes[len(runes)-1] == '\t') {
			runes = runes[:len(runes)-1]
		}
		return string(runes) + "…"
	}
	return string(runes)
}

// IsSkippableReadmeLine returns true for headings, horizontal rules,
// badge lines, and lone image lines. The caller has already stripped
// the line's outer whitespace.
func IsSkippableReadmeLine(line string) bool {
	if line == "" {
		return true
	}
	// Headings: `#`, `##`, … (Setext-style "===" / "---" underlines
	// are rare in modern READMEs and would be eaten by the HR check
	// below anyway).
	if strings.HasPrefix(line, "#") {
		return true
	}
	// Horizontal rules: `---`, `***`, `___` (3+ of the same char,
	// possibly with spaces between). Cheap check: if every non-space
	// char is the same and length >= 3.
	if IsHorizontalRule(line) {
		return true
	}
	// Badge lines: `[![...]...]` or `![...]...` — both forms are
	// images or linked images, conventionally CI/coverage badges at
	// the top of a README. We skip the WHOLE line even if there's
	// trailing text after the badge (the typical
	// "[![Build][x]][y] [![Cov][a]][b]" pattern).
	if strings.HasPrefix(line, "[![") || strings.HasPrefix(line, "![") {
		return true
	}
	return false
}

// IsHorizontalRule reports whether the trimmed line is a Markdown
// horizontal rule (`---`, `***`, `___`, with at least 3 identical
// chars and only spaces between them).
func IsHorizontalRule(line string) bool {
	if len(line) < 3 {
		return false
	}
	var pivot byte
	count := 0
	for i := 0; i < len(line); i++ {
		c := line[i]
		if c == ' ' || c == '\t' {
			continue
		}
		if c != '-' && c != '*' && c != '_' {
			return false
		}
		if pivot == 0 {
			pivot = c
		} else if c != pivot {
			return false
		}
		count++
	}
	return count >= 3
}

// CollapseWhitespace replaces every run of whitespace (space, tab,
// newline) with a single ASCII space. Used after joining paragraph
// lines so the result reads like a single-line description.
func CollapseWhitespace(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	prevSpace := false
	for _, r := range s {
		if r == ' ' || r == '\t' || r == '\n' || r == '\r' {
			if !prevSpace {
				b.WriteByte(' ')
				prevSpace = true
			}
			continue
		}
		b.WriteRune(r)
		prevSpace = false
	}
	return b.String()
}
