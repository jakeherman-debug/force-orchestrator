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
	"time"

	"force-orchestrator/internal/clients/librarian"
	igit "force-orchestrator/internal/git"
)

// repoDescriptionMaxLen caps derived descriptions. Long enough to be useful
// in dashboard tables and `force repos` output, short enough that the
// operator can still read the row. The trailing "…" indicates truncation.
//
// Sweep-E (D12): the canonical constant lives in the librarian package
// (librarian.ReadmeDescriptionMaxLen). This local alias is kept so the
// existing add_repo_smart_defaults_test.go assertions keep compiling
// without a wholesale rewrite — both names now refer to the same
// truncation cap.
const repoDescriptionMaxLen = librarian.ReadmeDescriptionMaxLen

// generateRepoDescriptionTimeout caps the librarian's LLM-backed
// derivation call from the add-repo flow. Operators waiting on
// `force add-repo` should never spin for longer than this before the
// regex fallback fires.
const generateRepoDescriptionTimeout = 60 * time.Second

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

// deriveRepoDescription is the legacy no-librarian shim retained for
// callers / tests that don't have a `librarian.Client` in scope. It
// delegates to scrapeReadmeFirstParagraph — the deterministic regex
// scrape that lived here pre-Sweep-E. Production callers MUST use
// deriveRepoDescriptionWithLibrarian so the LLM-backed path runs.
func deriveRepoDescription(absPath string) string {
	return scrapeReadmeFirstParagraph(absPath)
}

// deriveRepoDescriptionWithLibrarian is the Sweep-E (D12) entry-point.
// It routes through the librarian's GenerateRepoDescription
// (LLM-backed; LIVE_HAIKU-gated) and falls back to the deterministic
// regex scrape when:
//
//   - lib is nil (test paths that don't wire a client),
//   - the LLM returns an error (logged inside the librarian),
//   - the LLM returns an empty string (no description), OR
//   - the LLM returns the "(no description)" sentinel (which the
//     sanitiser maps to "").
//
// The 60-second timeout matches generateRepoDescriptionTimeout so the
// operator-facing `force add-repo` flow never hangs.
func deriveRepoDescriptionWithLibrarian(absPath string, lib librarian.Client) string {
	if absPath == "" {
		return ""
	}
	if lib == nil {
		return scrapeReadmeFirstParagraph(absPath)
	}
	ctx, cancel := context.WithTimeout(context.Background(), generateRepoDescriptionTimeout)
	defer cancel()
	desc, err := lib.GenerateRepoDescription(ctx, absPath)
	if err != nil || strings.TrimSpace(desc) == "" {
		return scrapeReadmeFirstParagraph(absPath)
	}
	return desc
}

// scrapeReadmeFirstParagraph is the regex-based deterministic
// description scraper preserved as the LIVE_HAIKU-disabled /
// LLM-failure fallback. Thin re-export of
// librarian.ScrapeReadmeFirstParagraph so the cmd/force test suite
// (TestDeriveRepoDescription_*, TestExtractFirstParagraph_*) keeps
// asserting on the same byte-for-byte shape it did pre-Sweep-E.
func scrapeReadmeFirstParagraph(absPath string) string {
	return librarian.ScrapeReadmeFirstParagraph(absPath)
}

// findReadme / extractFirstParagraph / isSkippableReadmeLine /
// isHorizontalRule / collapseWhitespace are re-exports of the shared
// librarian helpers. Kept here under their old names so the existing
// cmd/force unit tests compile unchanged; the canonical
// implementations live in internal/clients/librarian/readme.go.
func findReadme(dir string) string                { return librarian.FindReadme(dir) }
func extractFirstParagraph(content string) string { return librarian.ExtractFirstParagraph(content) }
func isSkippableReadmeLine(line string) bool      { return librarian.IsSkippableReadmeLine(line) }
func isHorizontalRule(line string) bool           { return librarian.IsHorizontalRule(line) }
func collapseWhitespace(s string) string          { return librarian.CollapseWhitespace(s) }
