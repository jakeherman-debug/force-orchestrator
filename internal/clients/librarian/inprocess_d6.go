// Package librarian — D6 BuildRepoDigest implementation.
//
// BuildRepoDigest is the shared knowledge-synthesis primitive consumed
// by BOTH the SenatorOnboarding task type AND the `force onboard`
// CLI. Centralising the assembly here is the anti-cheat seam called
// out in roadmap §D6: a single edit moves both call sites in lockstep.
//
// The function is pure-data — no LLM call. Renderers shape the digest
// for their respective use cases (Markdown for the CLI, prompt
// fragment for the Senator-bootstrap LLM call).
package librarian

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	igit "force-orchestrator/internal/git"
	"force-orchestrator/internal/store"
)

// d6ConventionsFiles are the conventions-document filenames consulted
// by BuildRepoDigest. The list is ordered: tests assert on the
// iteration order so a renderer change cannot silently reshuffle.
var d6ConventionsFiles = []string{
	"CLAUDE.md",
	"CONTRIBUTING.md",
	"SENATE.md",
}

// d6APISurfaceMaxFiles caps how many .go files BuildRepoDigest scans
// for public API symbols. The walk is breadth-first by directory; we
// stop emitting symbols once this cap is reached so very large
// monorepos don't blow the digest's renderer budget.
const d6APISurfaceMaxFiles = 200

// d6APISurfaceMaxSymbols caps the number of detected APISymbol
// entries returned in RepoDigest. 60 is enough for a useful onboarding
// page without overwhelming the operator's read.
const d6APISurfaceMaxSymbols = 60

// d6FragilityMemoryCap is the cap on FleetMemory rows pulled into
// RepoDigest.FragilityMemories.
const d6FragilityMemoryCap = 20

// d6CommitsWindow is the recent-activity window applied by
// BuildRepoDigest. The roadmap calls for 90 days.
const d6CommitsWindow = 90 * 24 * time.Hour

// d6ConventionFileMaxBytes caps each conventions-file value at 4 KB.
const d6ConventionFileMaxBytes = 4 * 1024

// BuildRepoDigest assembles the shared knowledge-synthesis digest
// consumed by both the SenatorOnboarding task and the
// `force onboard` CLI.
//
// repoSpec resolution:
//
//   - if a registered Repository row matches by name AND has a
//     readable local_path, the digest is built against that path.
//   - else if repoSpec is a directory on disk, the digest is built
//     against that path (RepoName left empty so renderers can fall
//     back to filepath.Base).
//   - else: returns an error.
func (c *inProcessClient) BuildRepoDigest(ctx context.Context, repoSpec string) (RepoDigest, error) {
	if err := ctx.Err(); err != nil {
		return RepoDigest{}, err
	}
	if strings.TrimSpace(repoSpec) == "" {
		return RepoDigest{}, fmt.Errorf("librarian: BuildRepoDigest requires a repo name or path")
	}

	digest := RepoDigest{
		Conventions: make(map[string]string, len(d6ConventionsFiles)),
		GeneratedAt: store.NowSQLite(),
	}

	// Resolve repoSpec → (RepoName, LocalPath, Description).
	if r := lookupRegisteredRepo(c, repoSpec); r != nil {
		digest.RepoName = r.Name
		digest.LocalPath = r.LocalPath
		digest.Description = r.Description
	} else if isExistingDir(repoSpec) {
		abs, err := filepath.Abs(repoSpec)
		if err != nil {
			return RepoDigest{}, fmt.Errorf("librarian: BuildRepoDigest absolute path: %w", err)
		}
		digest.LocalPath = abs
	} else {
		return RepoDigest{}, fmt.Errorf("librarian: BuildRepoDigest: %q is neither a registered repo nor a directory on disk", repoSpec)
	}

	// README sample. Reuse the existing helper when we have a registered
	// repo (so the code path matches BootstrapSenatorRules); for
	// disk-only repos we read the file ourselves.
	if digest.RepoName != "" {
		digest.READMESample = readRepoREADMESample(c.db, digest.RepoName)
	} else {
		digest.READMESample = readREADMESampleFromPath(digest.LocalPath)
	}

	// Top-level directories.
	digest.TopLevelDirs = walkTopLevelDirs(digest.LocalPath)

	// Public API surface — best-effort, never fatal.
	digest.PublicAPISymbols = scanPublicAPISurface(digest.LocalPath)

	// Recent commits — only meaningful for registered repos (we route
	// through RecentCommitsDigest which keys off the registered name
	// + GitOperationLog). For disk-only repos we run the same shape
	// but bypass the registry.
	if digest.RepoName != "" {
		commits, err := c.RecentCommitsDigest(ctx, digest.RepoName, d6CommitsWindow)
		if err == nil {
			digest.RecentCommits = commits
		} else {
			// Non-fatal: a missing local-path or a not-yet-cloned repo
			// shouldn't break onboarding. Stamp the empty digest.
			digest.RecentCommits = CommitsDigest{Repo: digest.RepoName, Window: d6CommitsWindow}
		}
	} else {
		digest.RecentCommits = unregisteredRecentCommits(ctx, digest.LocalPath, d6CommitsWindow)
	}

	// Conventions files (CLAUDE.md / CONTRIBUTING.md / SENATE.md).
	for _, name := range d6ConventionsFiles {
		body, err := os.ReadFile(filepath.Join(digest.LocalPath, name))
		if err != nil {
			digest.Conventions[name] = ""
			continue
		}
		if len(body) > d6ConventionFileMaxBytes {
			body = body[:d6ConventionFileMaxBytes]
		}
		digest.Conventions[name] = string(body)
	}

	// Fragility memories. Only meaningful for registered repos
	// (FleetMemory.repo is the canonical name). We pull failure-
	// outcome rows so the "Known fragility areas" section can
	// surface aggregated rejection reasons.
	if digest.RepoName != "" && c.db != nil {
		mems, err := c.GetMemoriesByScope(ctx, Scope{
			Repo:    digest.RepoName,
			Outcome: "failure",
			Limit:   d6FragilityMemoryCap,
		})
		if err == nil {
			digest.FragilityMemories = mems
		}
	}

	return digest, nil
}

// lookupRegisteredRepo returns the registered Repository row for the
// supplied name, or nil if not registered. We hit the store directly
// (the inProcessClient is the only D6-built-in implementation of the
// interface that has a *sql.DB) so the CLI does not need to import
// internal/store itself.
func lookupRegisteredRepo(c *inProcessClient, name string) *store.Repository {
	if c == nil || c.db == nil {
		return nil
	}
	r := store.GetRepo(c.db, name)
	if r == nil {
		return nil
	}
	if strings.TrimSpace(r.LocalPath) == "" {
		return nil
	}
	if _, err := os.Stat(r.LocalPath); err != nil {
		return nil
	}
	return r
}

// isExistingDir is true when path resolves to an existing directory.
func isExistingDir(path string) bool {
	info, err := os.Stat(path)
	if err != nil {
		return false
	}
	return info.IsDir()
}

// readREADMESampleFromPath is the disk-only sibling of
// readRepoREADMESample (which keys off the registered repo name).
func readREADMESampleFromPath(localPath string) string {
	for _, name := range []string{"README.md", "README.rst", "README.txt", "README"} {
		body, err := os.ReadFile(filepath.Join(localPath, name))
		if err != nil {
			continue
		}
		const cap = 4 * 1024
		if len(body) > cap {
			body = body[:cap]
		}
		return string(body)
	}
	return ""
}

// walkTopLevelDirs returns the alphabetically-sorted list of
// directories at the repo root, skipping hidden / vendor /
// node_modules conventions. The returned list is plain names (not
// paths) — the renderer reconstructs paths if needed.
func walkTopLevelDirs(localPath string) []string {
	if localPath == "" {
		return nil
	}
	entries, err := os.ReadDir(localPath)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		if strings.HasPrefix(name, ".") {
			continue
		}
		switch name {
		case "vendor", "node_modules", "target", "dist", "build":
			continue
		}
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// d6InterfaceRe matches `type FooBar interface {` declarations at the
// start of a line; the captured group is the interface's name.
var d6InterfaceRe = regexp.MustCompile(`^type\s+([A-Z]\w*)\s+interface\s*\{`)

// d6CLICommandRe matches the cobra/flag-style `case "foo":` strings
// that act as subcommand dispatchers in cmd/force/main.go. We accept
// both 1-tab and the indented 2-tab form (case "x":  inside
// nested switch). The captured group is the literal subcommand name.
var d6CLICommandRe = regexp.MustCompile(`^\s*case\s+"([a-z][a-z0-9-]*)"\s*[:,]`)

// d6HTTPRouteRe matches `mux.HandleFunc("/some/path", …)` style HTTP
// route registrations. The captured group is the path literal.
var d6HTTPRouteRe = regexp.MustCompile(`HandleFunc\(\s*"([^"]+)"`)

// scanPublicAPISurface walks the repo's .go files and emits a list of
// detected APISymbol entries. The scan is regex-based (deliberate —
// we are NOT type-checking, just surfacing names for human review).
//
// Ordering: deterministic. Files are walked in lexicographic order;
// within a file, symbols are emitted in the order they appear. We
// stop at d6APISurfaceMaxSymbols.
//
// The scan tolerates malformed Go source — a file that fails to open
// is silently skipped.
func scanPublicAPISurface(localPath string) []APISymbol {
	if localPath == "" {
		return nil
	}
	var (
		out         []APISymbol
		filesSeen   int
		stopWalking bool
	)
	_ = filepath.WalkDir(localPath, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if stopWalking {
			return filepath.SkipAll
		}
		if d.IsDir() {
			name := d.Name()
			if path == localPath {
				return nil
			}
			if strings.HasPrefix(name, ".") {
				return filepath.SkipDir
			}
			switch name {
			case "vendor", "node_modules", "testdata", "target", "dist", "build":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		if strings.HasSuffix(path, "_test.go") {
			return nil
		}
		filesSeen++
		if filesSeen > d6APISurfaceMaxFiles {
			return nil
		}
		f, oerr := os.Open(path)
		if oerr != nil {
			return nil
		}
		defer f.Close()
		rel, rerr := filepath.Rel(localPath, path)
		if rerr != nil {
			rel = path
		}
		scanner := bufio.NewScanner(f)
		scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		lineNo := 0
		for scanner.Scan() {
			lineNo++
			line := scanner.Text()
			if m := d6InterfaceRe.FindStringSubmatch(line); m != nil {
				out = append(out, APISymbol{
					Kind:        "interface",
					Name:        m[1],
					Location:    fmt.Sprintf("%s:%d", rel, lineNo),
					Description: fmt.Sprintf("exported Go interface %s (declared at %s:%d)", m[1], rel, lineNo),
				})
			}
			if m := d6HTTPRouteRe.FindStringSubmatch(line); m != nil {
				out = append(out, APISymbol{
					Kind:        "http",
					Name:        m[1],
					Location:    fmt.Sprintf("%s:%d", rel, lineNo),
					Description: fmt.Sprintf("HTTP route %s (registered at %s:%d)", m[1], rel, lineNo),
				})
			}
			// CLI subcommand detection — restrict to cmd/ subtrees so
			// random `case "foo":` matches in production code don't
			// pollute the surface.
			if strings.HasPrefix(rel, "cmd"+string(filepath.Separator)) {
				if m := d6CLICommandRe.FindStringSubmatch(line); m != nil {
					out = append(out, APISymbol{
						Kind:        "cli",
						Name:        m[1],
						Location:    fmt.Sprintf("%s:%d", rel, lineNo),
						Description: fmt.Sprintf("CLI subcommand %q (dispatched at %s:%d)", m[1], rel, lineNo),
					})
				}
			}
			if len(out) >= d6APISurfaceMaxSymbols {
				stopWalking = true
				return nil
			}
		}
		return nil
	})
	// Deterministic ordering — sort by Kind then Name then Location.
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Kind != out[j].Kind {
			return out[i].Kind < out[j].Kind
		}
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		return out[i].Location < out[j].Location
	})
	// Defence against the cap: SliceStable might shuffle past-cap
	// entries to the front. Re-cap.
	if len(out) > d6APISurfaceMaxSymbols {
		out = out[:d6APISurfaceMaxSymbols]
	}
	return out
}

// unregisteredRecentCommits is the disk-path sibling of
// RecentCommitsDigest — used when the repo is NOT in the registry
// (so store.GetRepoPath returns ""). We route through igit.LogAndRun
// with an empty OpContext.Repo (per Pattern P32 — empty Repo is
// allowed and the call is still captured in GitOperationLog when a
// DB is attached). Tolerates non-git directories by returning an
// empty digest.
func unregisteredRecentCommits(ctx context.Context, localPath string, window time.Duration) CommitsDigest {
	digest := CommitsDigest{Window: window}
	if localPath == "" {
		return digest
	}
	if _, err := os.Stat(filepath.Join(localPath, ".git")); err != nil {
		return digest
	}
	const fieldSep = "\x1f"
	pretty := fmt.Sprintf("--pretty=format:%%H%s%%s%s%%an%s%%aI", fieldSep, fieldSep, fieldSep)
	since := fmt.Sprintf("--since=%d.seconds.ago", int64(window.Seconds()))
	out, err := igit.LogAndRun(ctx, igit.OpContext{},
		"librarian-onboard-recent-commits-digest", "git", "-C", localPath,
		"log", since, "--shortstat", pretty,
		fmt.Sprintf("-n%d", recentCommitsDigestMaxCommits+1))
	if err != nil {
		return digest
	}
	commits := parseCommitsDigestOutput(string(out))
	if len(commits) > recentCommitsDigestMaxCommits {
		commits = commits[:recentCommitsDigestMaxCommits]
		digest.Truncated = true
	}
	digest.Commits = commits
	return digest
}
