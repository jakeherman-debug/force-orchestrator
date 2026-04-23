package git

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// ── Ingress validators (Fix #9) ──────────────────────────────────────────────
//
// Every branch / ref / path / URL / repo-spec that flows from the DB, an LLM
// payload, or external input into a `git` / `gh` shell call MUST route
// through one of the helpers in this file. Otherwise the CVE-2017-1000117
// class (ref names that start with `--upload-pack=/tmp/evil`, `-rm`,
// `--delete`, etc.) is reachable end-to-end.
//
// The rules are intentionally strict. When in doubt, REJECT — a spurious
// rejection surfaces as a loud error at task-queue time; a false accept
// reaches `exec.Command("git", …)` as flag injection.
//
// Three layers of defence are expected at every ingress:
//
//   1. A validator here that returns error on malformed input.
//   2. An `--` separator in the shell call so even a slipped-through string
//      cannot be re-interpreted as a flag.
//   3. For worktree paths, an `os.Lstat` + `filepath.EvalSymlinks` +
//      containment check, because a symlink under `.force-worktrees/…`
//      causes `git clean -fdx` to wipe wherever it points.

// ── Error sentinels ──────────────────────────────────────────────────────────

// ErrInvalidRef is returned by ValidateRef for any input that fails
// git-check-ref-format rules. Callers may unwrap to distinguish from
// transient errors, but the usual path is to surface the message directly.
var ErrInvalidRef = errors.New("invalid git ref name")

// ErrInvalidRepoPath is returned by ValidateRepoPath for a path that is
// missing, not absolute, contains traversal, or (if containment is required)
// escapes the expected base.
var ErrInvalidRepoPath = errors.New("invalid repo path")

// ErrInvalidRemoteURL is returned by ValidateRemoteURL for URLs that are
// empty, have a disallowed scheme, resolve to a link-local/loopback/RFC1918
// host, or carry shell-injection hazards (leading `-`, embedded flags).
var ErrInvalidRemoteURL = errors.New("invalid remote URL")

// ErrInvalidGHRepoSpec is returned by ValidateGHRepoSpec for any spec that
// doesn't match the strict `owner/repo` form required by `gh --repo`.
var ErrInvalidGHRepoSpec = errors.New("invalid gh repo spec")

// ── ValidateRef ──────────────────────────────────────────────────────────────

// refNameMaxLen caps accepted ref names. Git itself allows much longer names,
// but nothing in this codebase needs more than ~160 chars (the longest
// ask-branch we've observed is ~90). The cap prevents pathological inputs
// from turning a cheap string scan into a hot-path cost.
const refNameMaxLen = 256

// ValidateRef enforces the git-check-ref-format(1) grammar for a branch or
// ref name that is about to be passed as a positional argument to `git` or
// `gh`. Returns ErrInvalidRef on any failure.
//
// Rejects (non-exhaustive, see tests for the full adversarial corpus):
//
//   - empty string
//   - leading `-` (CVE-2017-1000117 family: `--upload-pack=…`, `-rm`, …)
//   - any ASCII control character (< 0x20) or 0x7F (DEL)
//   - NUL bytes, newlines, tabs
//   - whitespace other than the single-byte ASCII space, which is itself
//     forbidden by git-check-ref-format
//   - the grammar-forbidden characters: `~^:?*[\\`
//   - `..` path component (traversal) or a literal `..` substring
//   - `@{` sequence (reflog grammar conflict)
//   - consecutive slashes `//`
//   - leading or trailing `/`
//   - a trailing `.lock` (git treats as lock file)
//   - a ref that IS `@` (git reserved)
//   - a ref that begins with `.` (hidden file in refs/ tree)
//
// Accepts everything that our own `BranchPrefix()…force/ask-<N>-<slug>`
// and `agent/<name>/task-<N>` branches produce, plus short operator
// branch names like `feature/add-X`.
func ValidateRef(name string) error {
	if name == "" {
		return fmt.Errorf("%w: empty", ErrInvalidRef)
	}
	if len(name) > refNameMaxLen {
		return fmt.Errorf("%w: exceeds %d chars", ErrInvalidRef, refNameMaxLen)
	}
	if name[0] == '-' {
		return fmt.Errorf("%w: leading `-` (flag-injection hazard)", ErrInvalidRef)
	}
	if name[0] == '/' {
		return fmt.Errorf("%w: leading `/`", ErrInvalidRef)
	}
	// Check `..` before the leading-`.` rule so ".." reports the more
	// specific traversal error rather than a generic "leading `.`".
	if strings.Contains(name, "..") {
		return fmt.Errorf("%w: contains `..`", ErrInvalidRef)
	}
	if name[0] == '.' {
		return fmt.Errorf("%w: leading `.`", ErrInvalidRef)
	}
	if name[len(name)-1] == '/' {
		return fmt.Errorf("%w: trailing `/`", ErrInvalidRef)
	}
	if name[len(name)-1] == '.' {
		return fmt.Errorf("%w: trailing `.`", ErrInvalidRef)
	}
	if strings.HasSuffix(name, ".lock") {
		return fmt.Errorf("%w: trailing `.lock`", ErrInvalidRef)
	}
	if strings.Contains(name, "//") {
		return fmt.Errorf("%w: contains `//`", ErrInvalidRef)
	}
	if strings.Contains(name, "@{") {
		return fmt.Errorf("%w: contains `@{`", ErrInvalidRef)
	}
	if name == "@" {
		return fmt.Errorf("%w: reserved `@`", ErrInvalidRef)
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		// Reject any ASCII control char, DEL, and the git-forbidden
		// punctuation set (~, ^, :, ?, *, [, \, and space).
		if c < 0x20 || c == 0x7F {
			return fmt.Errorf("%w: control byte 0x%02x at offset %d", ErrInvalidRef, c, i)
		}
		switch c {
		case ' ', '~', '^', ':', '?', '*', '[', '\\':
			return fmt.Errorf("%w: forbidden character %q at offset %d", ErrInvalidRef, c, i)
		}
	}
	return nil
}

// IsValidRef reports whether name is accepted by ValidateRef. Convenience
// wrapper for call sites that only need a bool.
func IsValidRef(name string) bool { return ValidateRef(name) == nil }

// ── ValidateRepoPath ─────────────────────────────────────────────────────────

// RepoPathOptions configures ValidateRepoPath. Zero value is valid: it
// requires an absolute path with no traversal but no containment check.
type RepoPathOptions struct {
	// Base, if non-empty, is the absolute directory the path must live
	// under after resolution. Typically the parent of .force-worktrees/.
	// The path is rejected if filepath.EvalSymlinks(path) escapes Base.
	Base string

	// RejectSymlinks, when true, refuses any path whose Lstat reports a
	// symlink. Use for worktree-cleanup ingress (git clean -fdx at a
	// symlink target wipes the symlink's pointee).
	RejectSymlinks bool
}

// ValidateRepoPath enforces that `path` is safe to pass as a positional arg
// to git (as `-C <path>`, worktree root, etc.). Returns ErrInvalidRepoPath
// on any failure.
//
// Rules:
//   - non-empty
//   - no leading `-` (flag injection)
//   - no NUL byte
//   - no newline
//   - absolute (filepath.IsAbs)
//   - cleaned form has no `..` component
//   - if opts.RejectSymlinks: os.Lstat returns a non-symlink mode
//   - if opts.Base != "": filepath.EvalSymlinks(path) resolves under Base
//     (filepath.Rel-based containment — escape rejected)
//
// The stat/EvalSymlinks calls are only made when the path exists. A
// non-existent path is allowed at the string-level checks so repo
// registration can validate before git creates the directory.
func ValidateRepoPath(path string, opts RepoPathOptions) error {
	if path == "" {
		return fmt.Errorf("%w: empty", ErrInvalidRepoPath)
	}
	if strings.ContainsRune(path, 0) {
		return fmt.Errorf("%w: contains NUL", ErrInvalidRepoPath)
	}
	if strings.ContainsAny(path, "\n\r") {
		return fmt.Errorf("%w: contains newline", ErrInvalidRepoPath)
	}
	if path[0] == '-' {
		return fmt.Errorf("%w: leading `-` (flag-injection hazard)", ErrInvalidRepoPath)
	}
	if !filepath.IsAbs(path) {
		return fmt.Errorf("%w: not absolute: %q", ErrInvalidRepoPath, path)
	}
	// Reject literal `..` segments even after Clean would remove them —
	// an attacker may have encoded the traversal via redundant slashes
	// that Clean collapses, but pre-Clean we can still see the intent.
	for _, seg := range strings.Split(path, string(filepath.Separator)) {
		if seg == ".." {
			return fmt.Errorf("%w: contains `..` segment: %q", ErrInvalidRepoPath, path)
		}
	}
	cleaned := filepath.Clean(path)
	if cleaned != path && strings.Contains(cleaned, "..") {
		return fmt.Errorf("%w: cleaned form contains `..`: %q", ErrInvalidRepoPath, cleaned)
	}

	// On-disk checks — skip cleanly when the path doesn't exist yet
	// (e.g. operator registering a new repo).
	info, lerr := os.Lstat(path)
	if lerr != nil {
		// Non-existence is not a validation failure — the on-disk
		// containment check is a best-effort defence-in-depth.
		return nil
	}
	if opts.RejectSymlinks && info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%w: path is a symlink: %q", ErrInvalidRepoPath, path)
	}
	if opts.Base != "" {
		resolved, evErr := filepath.EvalSymlinks(path)
		if evErr != nil {
			// EvalSymlinks can fail on dangling symlinks or permission
			// errors — fall back to the Clean form.
			resolved = cleaned
		}
		baseResolved, bErr := filepath.EvalSymlinks(opts.Base)
		if bErr != nil {
			baseResolved = filepath.Clean(opts.Base)
		}
		rel, relErr := filepath.Rel(baseResolved, resolved)
		if relErr != nil || strings.HasPrefix(rel, "..") || rel == ".." {
			return fmt.Errorf("%w: path %q escapes base %q", ErrInvalidRepoPath, path, opts.Base)
		}
	}
	return nil
}

// IsValidRepoPath reports whether path is accepted under the given options.
func IsValidRepoPath(path string, opts RepoPathOptions) bool {
	return ValidateRepoPath(path, opts) == nil
}

// ── ValidateRemoteURL ────────────────────────────────────────────────────────

// allowedRemoteSchemes enumerates the URL schemes the fleet will accept
// for a git remote. `file://` is explicitly EXCLUDED — accepting it lets
// an attacker register `/etc` as a repo and then have WorktreeReset
// `git clean -fdx` arbitrary filesystem locations.
var allowedRemoteSchemes = map[string]struct{}{
	"https": {},
	"http":  {},
	"ssh":   {},
	"git":   {},
}

// sshRemoteRe matches the SCP-like SSH form git remotes use:
// `[user@]host:path`. Host must start with a letter/digit and contain
// only letters, digits, `.`, `-`. Path must not start with `/` (that
// would be absolute-on-remote and is ambiguous), must not start with `-`
// (flag injection), and must not contain `..`.
var sshRemoteRe = regexp.MustCompile(
	`^(?:[A-Za-z0-9_][A-Za-z0-9_.-]*@)?` + // optional user@
		`[A-Za-z0-9][A-Za-z0-9.-]*:` + // host:
		`[A-Za-z0-9_][A-Za-z0-9_./-]*$`) // path

// ValidateRemoteURL accepts a git remote URL for use with `git remote`,
// `git fetch`, `git push`, and derivations into `gh --repo`. Returns
// ErrInvalidRemoteURL on any failure.
//
// Accepts:
//   - `https://host/owner/repo[.git]`
//   - `http://host/owner/repo[.git]`  (only because internal CI git mirrors
//     sometimes run over plain HTTP — kept but flagged in audit)
//   - `ssh://[user@]host[:port]/owner/repo.git`
//   - `git://host/owner/repo.git`
//   - `[user@]host:owner/repo.git` (SCP-like; most common GitHub SSH form)
//
// Rejects:
//   - empty
//   - leading `-` (flag injection)
//   - any control byte, NUL, newline
//   - `file://` (CVE class: lets attacker register arbitrary directories)
//   - unknown schemes (e.g. `ext::`, `gopher://`)
//   - URLs with embedded `--upload-pack=` or `--receive-pack=`
//   - URLs whose host resolves to a loopback, link-local, or RFC1918
//     address when a scheme is present (prevents SSRF-style abuse when
//     webhook or dashboard code echoes the URL back)
//   - SSH URLs whose path starts with `-` (flag injection after the `:`)
func ValidateRemoteURL(raw string) error {
	if raw == "" {
		return fmt.Errorf("%w: empty", ErrInvalidRemoteURL)
	}
	if raw[0] == '-' {
		return fmt.Errorf("%w: leading `-` (flag-injection hazard)", ErrInvalidRemoteURL)
	}
	for i := 0; i < len(raw); i++ {
		c := raw[i]
		if c < 0x20 || c == 0x7F {
			return fmt.Errorf("%w: control byte 0x%02x at offset %d", ErrInvalidRemoteURL, c, i)
		}
	}
	// Defence in depth against URL-embedded git-protocol flag injection.
	// `git clone` / `git fetch` re-parse the URL to extract the remote
	// command, and some older git versions still accept these here.
	low := strings.ToLower(raw)
	if strings.Contains(low, "--upload-pack=") ||
		strings.Contains(low, "--receive-pack=") ||
		strings.Contains(low, "--config=") ||
		strings.Contains(low, "--exec=") {
		return fmt.Errorf("%w: embedded git-flag hazard", ErrInvalidRemoteURL)
	}

	// Try SCP-style SSH first — that form has no URL scheme but is the most
	// common GitHub remote. sshRemoteRe enforces it. Bare absolute
	// filesystem paths are ALSO accepted here — `git clone /local/path`
	// leaves `git remote get-url origin` returning the naked path, and
	// that's the actual production shape for cross-repo local mirrors
	// and test fixtures. They contain no `:` (SCP host delimiter), so
	// the SCP regex won't match them; we route to the path-style check
	// below.
	if !strings.Contains(raw, "://") {
		if !strings.Contains(raw, ":") && filepath.IsAbs(raw) {
			// Bare absolute path — accept. The path string was already
			// checked for leading `-`, control bytes, and git-flag
			// embeds at the top of this function.
			if strings.Contains(raw, "..") {
				return fmt.Errorf("%w: path contains `..`: %q", ErrInvalidRemoteURL, raw)
			}
			return nil
		}
		if !sshRemoteRe.MatchString(raw) {
			return fmt.Errorf("%w: not a recognisable remote: %q", ErrInvalidRemoteURL, raw)
		}
		// Extract the path after `:` and guard against leading `-` (path
		// starting with `-foo` becomes a flag if git ever positions it).
		colon := strings.Index(raw, ":")
		if colon >= 0 && colon+1 < len(raw) {
			p := raw[colon+1:]
			if p != "" && p[0] == '-' {
				return fmt.Errorf("%w: SSH path starts with `-`: %q", ErrInvalidRemoteURL, raw)
			}
			if strings.Contains(p, "..") {
				return fmt.Errorf("%w: SSH path contains `..`: %q", ErrInvalidRemoteURL, raw)
			}
		}
		return nil
	}

	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("%w: parse: %v", ErrInvalidRemoteURL, err)
	}
	scheme := strings.ToLower(u.Scheme)
	if _, ok := allowedRemoteSchemes[scheme]; !ok {
		return fmt.Errorf("%w: disallowed scheme %q", ErrInvalidRemoteURL, u.Scheme)
	}
	if scheme == "file" {
		// Belt-and-suspenders: allowedRemoteSchemes doesn't include file,
		// but if someone adds it by mistake, the host reachability check
		// below never fires for file URLs.
		return fmt.Errorf("%w: `file://` scheme not permitted", ErrInvalidRemoteURL)
	}
	host := u.Hostname()
	if host == "" {
		return fmt.Errorf("%w: empty host", ErrInvalidRemoteURL)
	}
	if host[0] == '-' {
		return fmt.Errorf("%w: host begins with `-`", ErrInvalidRemoteURL)
	}
	// Reject private-network and loopback hosts when the host parses as an
	// IP literal. DNS names are allowed — resolving them here would require
	// a network call on a hot path.
	if ip := net.ParseIP(host); ip != nil {
		if ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
			ip.IsPrivate() || ip.IsUnspecified() || ip.IsMulticast() {
			return fmt.Errorf("%w: host %q is loopback/link-local/RFC1918", ErrInvalidRemoteURL, host)
		}
	}
	// Reject paths starting with `-` within the URL path too — git can
	// sometimes feed path segments as positional args.
	if u.Path != "" && strings.HasPrefix(strings.TrimPrefix(u.Path, "/"), "-") {
		return fmt.Errorf("%w: URL path starts with `-`", ErrInvalidRemoteURL)
	}
	return nil
}

// IsValidRemoteURL reports whether raw passes ValidateRemoteURL.
func IsValidRemoteURL(raw string) bool { return ValidateRemoteURL(raw) == nil }

// ── ValidateGHRepoSpec ───────────────────────────────────────────────────────

// ghRepoSpecRe matches the `owner/repo` form required by `gh --repo`.
// Both parts start with an alphanumeric and contain only alphanumerics,
// `_`, `.`, `-`. Exactly one `/` separator. No whitespace, no flags, no
// hyphens at the start (CVE-2017-1000117 for gh).
var ghRepoSpecRe = regexp.MustCompile(
	`^[A-Za-z0-9][A-Za-z0-9_.-]*/[A-Za-z0-9][A-Za-z0-9_.-]*$`)

// ValidateGHRepoSpec checks `owner/repo` strings before they are passed to
// `gh --repo`. Returns ErrInvalidGHRepoSpec on failure.
//
// Accepts: `acme/api`, `jake.herman/force-orchestrator`, `gh_foo/bar.lib`.
// Rejects: empty, leading `-`, missing or extra `/`, whitespace, flag
// sequences like `--upload-pack=…`, `..`, paths with scheme/host prefix.
func ValidateGHRepoSpec(spec string) error {
	if spec == "" {
		return fmt.Errorf("%w: empty", ErrInvalidGHRepoSpec)
	}
	if len(spec) > refNameMaxLen {
		return fmt.Errorf("%w: exceeds %d chars", ErrInvalidGHRepoSpec, refNameMaxLen)
	}
	if strings.Contains(spec, "..") {
		return fmt.Errorf("%w: contains `..`", ErrInvalidGHRepoSpec)
	}
	if !ghRepoSpecRe.MatchString(spec) {
		return fmt.Errorf("%w: %q does not match owner/repo", ErrInvalidGHRepoSpec, spec)
	}
	return nil
}

// IsValidGHRepoSpec reports whether spec is accepted by ValidateGHRepoSpec.
func IsValidGHRepoSpec(spec string) bool { return ValidateGHRepoSpec(spec) == nil }
