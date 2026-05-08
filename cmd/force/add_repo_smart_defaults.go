package main

// add_repo_smart_defaults.go — deterministic helpers that derive a repo's
// `name` and `description` from on-disk data the system already has, so the
// operator doesn't have to type them.
//
// Sweep D's framing: `force add-repo <name> <path> <desc>` requiring three
// positional args is friction-by-default. Both `name` and `desc` can be
// derived without an LLM:
//
//   - name        → trailing component of `git remote get-url origin`
//                   (stripping `.git`); falls back to basename(path)
//   - description → first non-blank paragraph from README.md / README,
//                   skipping frontmatter, HTML comments, badge lines,
//                   blank lines, and Markdown headings; truncated to 200
//                   chars with a trailing ellipsis
//
// Both helpers are deliberately defensive: they NEVER panic and NEVER return
// uninitialized output. An empty string means "couldn't derive" — the caller
// is expected to either error out or accept that field as blank.

import (
	"context"
	"os"
	"path/filepath"
	"strings"

	igit "force-orchestrator/internal/git"
)

// repoDescriptionMaxLen caps derived descriptions. Long enough to be useful
// in dashboard tables and `force repos` output, short enough that the
// operator can still read the row. The trailing "…" indicates truncation.
const repoDescriptionMaxLen = 200

// addRepoBoolFlags lists the boolean flags accepted by the add-repo /
// add-repos handlers — needed so reorderFlagsFirst doesn't grab the
// next positional as their value.
var addRepoBoolFlags = map[string]bool{
	"--assume-yes": true,
	"-y":           true,
}

// reorderFlagsFirst moves every flag-shaped arg (`--foo`, `--foo=bar`,
// `--foo bar`, `-x`) to the front of the slice while preserving the
// relative order of positionals. Go's stdlib flag package stops at the
// first non-flag positional, which makes
// `force add-repo /path --name foo` mis-parse without this reorder.
//
// `boolFlags` is the set of flags that don't take a value (so we don't
// hoist the next token as their argument).
func reorderFlagsFirst(args []string, boolFlags map[string]bool) []string {
	var flags, positionals []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			// Everything after `--` is positional verbatim.
			positionals = append(positionals, args[i+1:]...)
			break
		}
		if strings.HasPrefix(a, "-") && a != "-" {
			flags = append(flags, a)
			// `--foo=bar` is self-contained; `--foo bar` consumes the
			// next token unless --foo is a known boolean.
			if !strings.Contains(a, "=") && !boolFlags[a] && i+1 < len(args) {
				next := args[i+1]
				if !strings.HasPrefix(next, "-") || next == "-" {
					flags = append(flags, next)
					i++
				}
			}
			continue
		}
		positionals = append(positionals, a)
	}
	return append(flags, positionals...)
}

// deriveRepoName picks the registered name for a repo at the given absPath.
// Priority:
//  1. git remote get-url origin → trailing path component, stripping ".git"
//  2. basename(absPath)
//
// Returns empty string only if both fail (caller should error then).
func deriveRepoName(absPath string) string {
	// Defensive — never panic on garbage input.
	if absPath == "" {
		return ""
	}

	// Strip trailing slashes so basename gives the right answer for
	// "/path/to/foo/" → "foo" (filepath.Base would otherwise return "foo"
	// only when there's no trailing separator; the strip below is
	// belt-and-suspenders for paths that survived the validator with a
	// trailing slash).
	clean := strings.TrimRight(absPath, string(os.PathSeparator))
	if clean == "" {
		clean = absPath
	}

	// 1) Try `git remote get-url origin`. We deliberately use the same
	// igit.LogAndRun shape cmdAddRepo uses for consistency (same operator
	// breadcrumbs, same audit trail). A non-zero exit, missing remote, or
	// any other failure simply falls through — never an error.
	ctx := context.Background()
	if out, err := igit.LogAndRun(ctx,
		igit.OpContext{Repo: clean},
		"derive-repo-name-remote",
		"git", "-C", clean, "remote", "get-url", "origin",
	); err == nil {
		remote := strings.TrimSpace(string(out))
		if name := repoNameFromRemoteURL(remote); name != "" {
			return name
		}
	}

	// 2) basename(absPath). filepath.Base handles trailing-slash and
	// drive-letter quirks; we already trimmed trailing slashes above.
	base := filepath.Base(clean)
	if base == "." || base == "/" || base == string(os.PathSeparator) {
		return ""
	}
	return base
}

// repoNameFromRemoteURL extracts the trailing path component from a git
// remote URL, stripping any `.git` suffix. Handles both SSH-style
// (git@github.com:org/repo.git) and HTTPS-style
// (https://github.com/org/repo.git) URLs. Returns "" if the URL has no
// usable trailing component.
func repoNameFromRemoteURL(url string) string {
	if url == "" {
		return ""
	}
	// SSH form `git@host:org/repo.git` — split on `:` first, then `/` so
	// the host part is dropped before path parsing.
	if i := strings.LastIndex(url, ":"); i != -1 {
		// Only treat as SSH if there's no `://` before it (i.e. the `:` is
		// not part of a scheme like https://).
		if !strings.Contains(url[:i], "://") {
			url = url[i+1:]
		}
	}
	// Strip everything before the last `/` — handles HTTPS, ssh://, and
	// the post-colon SSH path uniformly.
	if i := strings.LastIndex(url, "/"); i != -1 {
		url = url[i+1:]
	}
	url = strings.TrimSuffix(url, ".git")
	url = strings.TrimSpace(url)
	return url
}

// deriveRepoDescription extracts a one-paragraph description from the
// repo's README. Searches for README.md / README / readme.md (case-
// insensitive) at the repo root. Skips frontmatter (`---\n...\n---`),
// HTML comments, badge lines (lines starting with `[![` or `![`),
// blank lines, and Markdown headings. Returns the first remaining
// non-blank paragraph, truncated to 200 chars (with "…" if truncated).
// Returns empty string if no README found or no usable paragraph.
func deriveRepoDescription(absPath string) string {
	if absPath == "" {
		return ""
	}
	readmePath := findReadme(absPath)
	if readmePath == "" {
		return ""
	}
	data, err := os.ReadFile(readmePath)
	if err != nil {
		return ""
	}
	return extractFirstParagraph(string(data))
}

// findReadme walks the immediate children of dir and returns the first one
// whose name matches a README variant (case-insensitive). README.md is
// preferred over README, but if both exist we take whichever ReadDir lists
// first — operationally good enough; the helper doesn't claim a strict
// priority since the README content is what matters.
func findReadme(dir string) string {
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

// extractFirstParagraph parses README markdown text and returns the first
// non-blank paragraph after skipping:
//   - YAML frontmatter (leading `---\n...\n---`)
//   - HTML comments (`<!-- ... -->`, possibly multi-line)
//   - badge / image lines (`[![...]` or `![...`)
//   - blank lines
//   - Markdown headings (`#`, `##`, etc.)
//   - Horizontal rules (`---`, `***`, `___`)
//
// The returned paragraph has its internal newlines folded to single spaces
// and is truncated to repoDescriptionMaxLen (with a trailing `…` if
// truncation happened). Returns "" when no usable paragraph exists.
func extractFirstParagraph(content string) string {
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
	// is a run of consecutive non-skipped non-blank lines; the run ends at
	// a blank line or end of file.
	var (
		para  []string
		seen  bool
	)
	for _, raw := range lines {
		line := strings.TrimSpace(raw)
		if line == "" {
			if seen {
				break
			}
			continue
		}
		if isSkippableReadmeLine(line) {
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
	joined = collapseWhitespace(joined)
	joined = strings.TrimSpace(joined)
	if joined == "" {
		return ""
	}

	// Truncate. Use rune-aware slicing so multi-byte chars don't get cut
	// in the middle.
	runes := []rune(joined)
	if len(runes) > repoDescriptionMaxLen {
		runes = runes[:repoDescriptionMaxLen]
		// Trim trailing whitespace before adding the ellipsis so we don't
		// produce "foo …".
		for len(runes) > 0 && (runes[len(runes)-1] == ' ' || runes[len(runes)-1] == '\t') {
			runes = runes[:len(runes)-1]
		}
		return string(runes) + "…"
	}
	return string(runes)
}

// isSkippableReadmeLine returns true for headings, horizontal rules, badge
// lines, and lone image lines. The caller has already stripped the line's
// outer whitespace.
func isSkippableReadmeLine(line string) bool {
	if line == "" {
		return true
	}
	// Headings: `#`, `##`, … (Setext-style "===" / "---" underlines are
	// rare in modern READMEs and would be eaten by the HR check below
	// anyway).
	if strings.HasPrefix(line, "#") {
		return true
	}
	// Horizontal rules: `---`, `***`, `___` (3+ of the same char, possibly
	// with spaces between). Cheap check: if every non-space char is the
	// same and length >= 3.
	if isHorizontalRule(line) {
		return true
	}
	// Badge lines: `[![...]...]` or `![...]...` — both forms are images
	// or linked images, conventionally CI/coverage badges at the top of a
	// README. We skip the WHOLE line even if there's trailing text after
	// the badge (the typical "[![Build][x]][y] [![Cov][a]][b]" pattern).
	if strings.HasPrefix(line, "[![") || strings.HasPrefix(line, "![") {
		return true
	}
	return false
}

// isHorizontalRule reports whether the trimmed line is a Markdown
// horizontal rule (`---`, `***`, `___`, with at least 3 identical chars
// and only spaces between them).
func isHorizontalRule(line string) bool {
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

// collapseWhitespace replaces every run of whitespace (space, tab,
// newline) with a single ASCII space. Used after joining paragraph lines
// so the result reads like a single-line description.
func collapseWhitespace(s string) string {
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
