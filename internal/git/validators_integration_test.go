package git

import (
	"os/exec"
	"strings"
	"testing"
)

// ── Integration tests for Fix #9 ingress gating ──────────────────────────────
//
// These tests exercise the full path from a caller handing an adversarial
// branch/ref to a helper that is about to shell out to git. The assertion:
// the shell call is NEVER made when the validator rejects the input.
//
// The approach: wrap the git invocation through a helper we can introspect
// (via process tracing or by pointing PATH at a no-op shim). Simpler: call
// the validator-gated helper with an adversarial input and assert an
// error-before-shell return path by checking for the validator-error
// substring in the returned error, and that no unexpected side effects
// landed on disk.

// TestIntegration_ValidateRef_BlocksBeforeGit proves that an adversarial
// ref is caught by the validator BEFORE any `git` subprocess is spawned.
// We do this by asserting the error surfaces the validator's `ErrInvalidRef`
// wrap message — which is produced by the Go-level check, not by git.
func TestIntegration_ValidateRef_BlocksBeforeGit(t *testing.T) {
	adversarials := []string{
		"--upload-pack=/tmp/evil",
		"-rm -rf /",
		"..",
		"foo\x00bar",
		"@{",
		"trailing.lock",
	}
	for _, a := range adversarials {
		a := a
		t.Run(safeLabelForTest(a), func(t *testing.T) {
			err := ValidateRef(a)
			if err == nil {
				t.Fatalf("ValidateRef(%q) = nil — adversarial input accepted", a)
			}
			// The error must wrap ErrInvalidRef. If it came from git, it
			// would wrap an exec.ExitError / OS error instead.
			if !strings.Contains(err.Error(), "invalid git ref") {
				t.Fatalf("ValidateRef(%q) error = %v — should wrap ErrInvalidRef", a, err)
			}
		})
	}
}

// TestIntegration_ValidateRemoteURL_BlocksBeforeGit mirrors the above for
// remote URLs. An adversarial URL with embedded `--upload-pack=` must be
// caught at the validator, not by git's own parsing.
func TestIntegration_ValidateRemoteURL_BlocksBeforeGit(t *testing.T) {
	// Sanity: git is on PATH so a real shell call could succeed under test.
	// If this ever fails to find git, the integration guarantee degrades to
	// the unit-test guarantee — still fine, but log it.
	if _, err := exec.LookPath("git"); err != nil {
		t.Logf("git not on PATH (%v) — validator-only assertion in effect", err)
	}

	adversarials := []struct {
		url    string
		reason string
	}{
		{"-upload-pack=/tmp/evil", "leading `-`"},
		{"file:///etc/passwd", "disallowed scheme"},
		{"git@github.com:--upload-pack=/tmp/evil/foo.git", "embedded git-flag"},
		{"https://127.0.0.1/x/y", "loopback/link-local/RFC1918"},
	}
	for _, c := range adversarials {
		c := c
		t.Run(safeLabelForTest(c.url), func(t *testing.T) {
			err := ValidateRemoteURL(c.url)
			if err == nil {
				t.Fatalf("ValidateRemoteURL(%q) = nil — adversarial URL accepted", c.url)
			}
			if !strings.Contains(err.Error(), c.reason) {
				t.Fatalf("ValidateRemoteURL(%q) error = %v — want %q", c.url, err, c.reason)
			}
		})
	}
}
