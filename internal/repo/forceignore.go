// Package repo holds target-repo-aware utilities — the bits of Force-Go
// that need to reason about a registered target repository's filesystem
// without going through the SQLite store. Today the package hosts the
// .forceignore loader (D1 T0-2) and is intended to host any future
// repo-local-policy reader (.forcedirectives, .forcecodeowners, etc.).
package repo

import (
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	gitignore "github.com/sabhiram/go-gitignore"
)

// pkgLog is the package-level logger; aliased so the logf indirection
// can be retargeted in tests without rewriting the standard log calls.
var pkgLog = log.Default()

// ForceIgnore wraps a parsed .forceignore file. The patterns are
// gitignore-style (delegated to github.com/sabhiram/go-gitignore for
// matching semantics — no roll-your-own pattern matcher per T0-2
// directives), but the meaning is different: a matching path is one
// that astromechs SHOULD NOT read into a Claude prompt or task payload.
//
// A nil *ForceIgnore is the "no policy" case — IsIgnored always returns
// false. This is the normal state for repos that have not adopted the
// convention; LoadForceIgnore returns (nil, nil) when the file is
// absent so the caller's IsIgnored gate is a one-line no-op.
type ForceIgnore struct {
	repoPath  string
	matcher   *gitignore.GitIgnore
	rawLines  []string
}

// LoadForceIgnore reads <repoPath>/.forceignore and compiles its
// patterns. Returns:
//
//   - (compiled, nil) when the file exists and parses cleanly.
//   - (nil, nil)      when the file is absent — the conventional "no
//     policy" state. Callers treat this as "permit everything" and
//     proceed without an error.
//   - (nil, err)      on parse / read I/O errors. Callers must surface
//     these — silent fallback to "permit everything" would re-open
//     T0-2's threat model (a stat() race or NFS hiccup masking a real
//     policy file).
//
// The repoPath argument must be absolute or relative to the daemon's
// working directory; the loader does not auto-resolve.
func LoadForceIgnore(repoPath string) (*ForceIgnore, error) {
	if repoPath == "" {
		return nil, errors.New("LoadForceIgnore: repoPath required")
	}
	path := filepath.Join(repoPath, ".forceignore")
	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			// Absent file is the "no policy" sentinel — not an error.
			return nil, nil
		}
		return nil, fmt.Errorf("LoadForceIgnore: read %s: %w", path, err)
	}

	// Normalise: strip leading/trailing whitespace, drop empty + comment
	// lines. sabhiram/go-gitignore tolerates comments and blanks, but
	// the rawLines slice we keep for diagnostic logging should be the
	// effective ruleset only.
	lines := strings.Split(string(raw), "\n")
	var effective []string
	for _, ln := range lines {
		t := strings.TrimSpace(ln)
		if t == "" || strings.HasPrefix(t, "#") {
			continue
		}
		effective = append(effective, t)
	}

	matcher := gitignore.CompileIgnoreLines(effective...)
	if matcher == nil {
		return nil, fmt.Errorf("LoadForceIgnore: compile %s: matcher returned nil", path)
	}
	return &ForceIgnore{
		repoPath: repoPath,
		matcher:  matcher,
		rawLines: effective,
	}, nil
}

// IsIgnored returns true when relPath is matched by any .forceignore
// rule. The path is relative to the .forceignore's repoPath.
//
// Symlinks are resolved before matching: a symlink "link.txt" pointing
// at ".env" is treated as ".env" for the purpose of policy match. This
// closes the T0-2 anti-cheat directive against symlink bypass — without
// resolution, an attacker (or a careless astromech) could place
// "link.txt → .env" in a repo whose .forceignore says "exclude .env"
// and the policy would silently miss the symlinked alias.
//
// On a nil receiver (the "no policy" case), IsIgnored returns false.
func (fi *ForceIgnore) IsIgnored(relPath string) bool {
	if fi == nil {
		return false
	}
	if relPath == "" {
		return false
	}
	// Symlink resolution: resolve via EvalSymlinks if the path exists
	// on disk, then re-derive the relative path. We resolve BOTH sides
	// (repoPath and the joined target) before taking Rel — on macOS
	// /var is a symlink to /private/var, so a one-sided resolve would
	// produce nonsense Rel results that prevent in-repo symlinks from
	// matching their resolved targets. If the file does not exist (or
	// the resolution fails), match the original path — we do not want
	// a missing-file error to be a "permit by default" outcome.
	abs := filepath.Join(fi.repoPath, relPath)
	if resolvedTarget, err := filepath.EvalSymlinks(abs); err == nil {
		resolvedRoot := fi.repoPath
		if rr, err2 := filepath.EvalSymlinks(fi.repoPath); err2 == nil {
			resolvedRoot = rr
		}
		if rel, err3 := filepath.Rel(resolvedRoot, resolvedTarget); err3 == nil {
			// If the symlink target is OUTSIDE the repo, fall back to
			// the original path — the matcher works in repo-relative
			// space and a "../..." target is not meaningfully matchable.
			if !strings.HasPrefix(rel, "..") {
				relPath = rel
			}
		}
	}
	// gitignore matchers are convention-bound to forward slashes; on
	// Windows or on edge inputs that contain backslashes, normalise.
	relPath = filepath.ToSlash(relPath)
	return fi.matcher.MatchesPath(relPath)
}

// RepoPath returns the repo root used to load this .forceignore.
// Useful for log messages so the operator can disambiguate which repo's
// policy fired.
func (fi *ForceIgnore) RepoPath() string {
	if fi == nil {
		return ""
	}
	return fi.repoPath
}

// Patterns returns the raw effective lines (post-strip-comments) used
// to build the matcher. Intended for diagnostic logging and the
// pre-commit hook's reporting.
func (fi *ForceIgnore) Patterns() []string {
	if fi == nil {
		return nil
	}
	out := make([]string, len(fi.rawLines))
	copy(out, fi.rawLines)
	return out
}

// ReadRepoFileGated loads the .forceignore at repoPath, checks absPath
// against it, and returns the file content unless the policy matches.
// agentName is echoed into the [FORCEIGNORE SKIP] log line so the
// operator can correlate the skip with the agent that triggered it.
//
// Returns:
//   - (content, false, nil) on a successful read of a non-ignored file.
//   - ("", true, nil)       when the file is matched by an ignore rule.
//   - ("", false, err)      on any I/O or LoadForceIgnore error.
//
// Use this helper at every Force-Go-side ingress that pulls target-repo
// file content into a Claude prompt or task payload. It is the .forceignore
// equivalent of the inbound-redact wrapper at the Claude CLI boundary —
// belt-and-suspenders together prevent the secret from ever reaching
// claude -p.
func ReadRepoFileGated(repoPath, absPath, agentName string) (content string, ignored bool, err error) {
	fi, ferr := LoadForceIgnore(repoPath)
	if ferr != nil {
		return "", false, ferr
	}
	rel, rerr := filepath.Rel(repoPath, absPath)
	if rerr != nil {
		return "", false, fmt.Errorf("ReadRepoFileGated: rel: %w", rerr)
	}
	if fi.IsIgnored(rel) {
		forceIgnoreSkipLog(agentName, filepath.Base(repoPath), rel)
		return "", true, nil
	}
	data, derr := os.ReadFile(absPath)
	if derr != nil {
		return "", false, derr
	}
	return string(data), false, nil
}

// forceIgnoreSkipLog emits the canonical [FORCEIGNORE SKIP] log line.
// Pulled into a helper so the integration test can reliably grep for
// the prefix.
//
// The function is replaceable in tests via SetForceIgnoreSkipObserver
// so the integration test can capture skip events without scraping log
// output through stderr.
var forceIgnoreSkipLog = func(agentName, repoBaseName, relPath string) {
	defaultForceIgnoreSkipLog(agentName, repoBaseName, relPath)
}

func defaultForceIgnoreSkipLog(agentName, repoBaseName, relPath string) {
	// Logging deliberately avoids any portion of the file CONTENT — only
	// the agent, repo base name, and relative path. The path itself is
	// not secret (it's a name, not a value).
	logf("[FORCEIGNORE SKIP] agent=%s repo=%s path=%s", agentName, repoBaseName, relPath)
}

// SetForceIgnoreSkipObserver installs a test observer that fires for
// every [FORCEIGNORE SKIP] event. Pass nil to restore the default
// log-only behaviour. Callers in production must NOT use this.
func SetForceIgnoreSkipObserver(fn func(agentName, repoBaseName, relPath string)) {
	if fn == nil {
		forceIgnoreSkipLog = func(a, r, p string) { defaultForceIgnoreSkipLog(a, r, p) }
		return
	}
	forceIgnoreSkipLog = func(a, r, p string) {
		defaultForceIgnoreSkipLog(a, r, p)
		fn(a, r, p)
	}
}

// logf is the package-level log entry — kept as a tiny indirection so
// the tests can swap it out without touching the standard library log
// package (which has no thread-safe override hook).
var logf = func(format string, args ...any) {
	pkgLog.Printf(format, args...)
}
