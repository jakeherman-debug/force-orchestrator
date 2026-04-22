package git

import (
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"time"
)

// ── GitHub username discovery for branch-name prefixing ─────────────────────
//
// Branches created by the fleet are prefixed with the operator's GitHub
// username (`<user>/force/ask-<id>-<slug>` for ask-branches, `<user>/agent/
// <name>/task-<id>` for astromech branches). Enterprise repos often enforce
// this convention via branch-protection rules and cleanup dogs.
//
// The fallback chain (first non-empty wins):
//
//   1. `gh api user --jq '.login'`              — canonical, matches gh auth
//   2. `gh config get user -h github.com`       — local gh configuration
//   3. `git config user.name`                   — last resort (email-based
//                                                  usernames get sanitized)
//
// Result is sanitized to a git-branch-safe slug (alphanumerics, `-`, `_`).
// The lookup runs once per process and is cached — the result is stable
// during a daemon run, and cheap enough (~50ms of shell-outs) that running
// it once at startup isn't a concern.

var (
	usernameOnce  sync.Once
	cachedUser    string
	usernameMu    sync.Mutex
)

// BranchPrefix returns the branch-name prefix including trailing slash, e.g.
// "alice-smith/". Returns "" when no GitHub username can be discovered — in
// that case branches use their bare historical names (`force/ask-...`,
// `agent/...`).
//
// Test hook: use SetBranchPrefixOverride to force a known value; the override
// persists until cleared with SetBranchPrefixOverride("").
func BranchPrefix() string {
	usernameMu.Lock()
	override := branchPrefixOverride
	usernameMu.Unlock()
	if override != nil {
		return *override
	}
	usernameOnce.Do(func() {
		cachedUser = discoverGitHubUsername()
	})
	if cachedUser == "" {
		return ""
	}
	return cachedUser + "/"
}

// branchPrefixOverride is set by tests via SetBranchPrefixOverride. When
// non-nil, BranchPrefix returns *branchPrefixOverride verbatim (including
// the trailing slash — or lack thereof, per the caller's choice).
var branchPrefixOverride *string

// SetBranchPrefixOverride installs a fixed prefix for the duration of a test.
// Pass the full prefix WITH the trailing slash (e.g. "testuser/"), or "" to
// simulate "no GitHub username discoverable". Pass nil (via the restore fn)
// to clear the override and fall back to real discovery. Returns a restore
// function for use with t.Cleanup.
func SetBranchPrefixOverride(prefix string) (restore func()) {
	usernameMu.Lock()
	defer usernameMu.Unlock()
	prev := branchPrefixOverride
	branchPrefixOverride = &prefix
	return func() {
		usernameMu.Lock()
		defer usernameMu.Unlock()
		branchPrefixOverride = prev
	}
}

// ResetBranchPrefixCache clears the memoised username so the next call to
// BranchPrefix re-runs discovery. Only useful in tests that want to verify
// discovery behaviour itself.
func ResetBranchPrefixCache() {
	usernameMu.Lock()
	defer usernameMu.Unlock()
	usernameOnce = sync.Once{}
	cachedUser = ""
}

// discoverGitHubUsername runs the fallback chain. Returns a sanitized
// username or empty string.
func discoverGitHubUsername() string {
	for _, lookup := range []func() string{
		ghAPIUserLogin,
		ghConfigUser,
		gitConfigUserName,
	} {
		if u := lookup(); u != "" {
			if clean := sanitizeUsername(u); clean != "" {
				return clean
			}
		}
	}
	return ""
}

// ghAPIUserLogin calls `gh api user --jq '.login'`. Short timeout — if gh is
// slow or unavailable, we fall through to the next lookup fast.
func ghAPIUserLogin() string {
	return runWithTimeout(3*time.Second, "gh", "api", "user", "--jq", ".login")
}

// ghConfigUser calls `gh config get user -h github.com`. Only needs file I/O,
// so faster than the API call.
func ghConfigUser() string {
	return runWithTimeout(2*time.Second, "gh", "config", "get", "user", "-h", "github.com")
}

// gitConfigUserName calls `git config user.name`. Last-resort fallback —
// usually a human name (not a username), so heavily sanitized downstream.
func gitConfigUserName() string {
	return runWithTimeout(1*time.Second, "git", "config", "user.name")
}

// runWithTimeout executes a command with a bounded deadline and returns its
// trimmed stdout, or "" on any error.
func runWithTimeout(timeout time.Duration, name string, args ...string) string {
	cmd := exec.Command(name, args...)
	done := make(chan struct{})
	var out []byte
	var runErr error
	go func() {
		out, runErr = cmd.Output()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(timeout):
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		<-done
		return ""
	}
	if runErr != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// sanitizeUsername collapses a possibly-free-form name into a git-branch-safe
// slug. Rules:
//   - lowercase
//   - a-z, 0-9, '-', '_' preserved
//   - everything else (spaces, dots, '@', etc.) becomes '-'
//   - collapse runs of '-'
//   - trim leading/trailing '-'
//   - cap at 40 chars
//
// "Alice O'Brien" → "alice-o-brien"; "alice@example.com" → "alice-example-com".
// An email-style `git config user.name` won't be mistaken for a branch name
// because we stop at the '@'.
var usernameBadChar = regexp.MustCompile(`[^a-z0-9_-]+`)

func sanitizeUsername(u string) string {
	lower := strings.ToLower(u)
	cleaned := usernameBadChar.ReplaceAllString(lower, "-")
	// Collapse runs of '-' and trim.
	for strings.Contains(cleaned, "--") {
		cleaned = strings.ReplaceAll(cleaned, "--", "-")
	}
	cleaned = strings.Trim(cleaned, "-")
	if len(cleaned) > 40 {
		cleaned = strings.TrimRight(cleaned[:40], "-")
	}
	return cleaned
}
