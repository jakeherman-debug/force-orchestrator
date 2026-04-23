package gh

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// TestRedactGHError_StrippsPATFromStderr is the AUDIT-055 acceptance
// test: simulate a `gh` call whose stderr contains a GitHub PAT, wrap
// it through redactGHError, assert the returned error string has the
// PAT replaced with [REDACTED] while the prefix + wrapped exit status
// stay intact.
func TestRedactGHError_StrippsPATFromStderr(t *testing.T) {
	stderr := []byte("error: bad credentials for token ghp_SecretTokenBody1234567 (401)")
	wrapped := redactGHError("gh pr view", fmt.Errorf("exit status 1"), stderr)
	msg := wrapped.Error()
	if strings.Contains(msg, "ghp_SecretTokenBody") {
		t.Errorf("PAT survived redactGHError:\n  msg=%q", msg)
	}
	if !strings.Contains(msg, "[REDACTED]") {
		t.Errorf("[REDACTED] placeholder missing in %q", msg)
	}
	if !strings.Contains(msg, "gh pr view") {
		t.Errorf("prefix dropped from wrapped error: %q", msg)
	}
	if !strings.Contains(msg, "exit status 1") {
		t.Errorf("wrapped exit status lost: %q", msg)
	}
}

// TestAuthFailureErrorLogRedacted is a second acceptance scenario —
// the specific shape of gh's auth-failure stderr which includes both a
// Bearer token and a URL with embedded basic auth. Both must be gone.
func TestAuthFailureErrorLogRedacted(t *testing.T) {
	stderr := []byte(`gh auth: remote rejected Authorization: Bearer eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9abcdef` +
		` when pushing to https://ci-bot:hunter2@github.enterprise.example/owner/repo.git`)
	wrapped := redactGHError("gh push", fmt.Errorf("exit 128"), stderr)
	msg := wrapped.Error()

	if strings.Contains(msg, "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9abcdef") {
		t.Errorf("Bearer token survived: %q", msg)
	}
	if strings.Contains(msg, "hunter2") {
		t.Errorf("URL basic-auth password survived: %q", msg)
	}
	if !strings.Contains(msg, "Bearer [REDACTED]") {
		t.Errorf("expected Bearer [REDACTED] in %q", msg)
	}
	if !strings.Contains(msg, "https://[REDACTED]@github.enterprise.example") {
		t.Errorf("expected https://[REDACTED]@host in %q", msg)
	}
}

// TestClassifyError_OverflowMapsToPermanent wires the ErrOverflow path:
// once capWriter rejects a write, ExecRunner surfaces the error; when
// it bubbles up to Pilot via ClassifyError(err.Error()) the fleet must
// route it to ErrClassPermanent (no retry — the next attempt will OOM
// identically).
func TestClassifyError_OverflowMapsToPermanent(t *testing.T) {
	msg := ErrOverflow.Error()
	if got := ClassifyError(msg); got != ErrClassPermanent {
		t.Errorf("ClassifyError(%q) = %v, want ErrClassPermanent", msg, got)
	}
	// Wrapped form — Pilot typically sees fmt.Errorf("...: %w", ErrOverflow).
	wrapped := fmt.Errorf("pr_flow: stdout capture failed: %w", ErrOverflow)
	if got := ClassifyError(wrapped.Error()); got != ErrClassPermanent {
		t.Errorf("ClassifyError(wrapped) = %v, want ErrClassPermanent", got)
	}
}

// TestCapWriter_EnforcesLimit exercises the cap directly — a slice
// bigger than the cap returns ErrOverflow on first write, and the
// buffer content up to the cap is preserved so partial-response
// diagnostics remain useful.
func TestCapWriter_EnforcesLimit(t *testing.T) {
	testCap := 16
	var buf bytes.Buffer
	cw := &capWriter{buf: &buf, cap: testCap}

	// First write fits.
	if n, err := cw.Write([]byte("hello ")); err != nil || n != 6 {
		t.Fatalf("first write: n=%d err=%v", n, err)
	}
	// Second write straddles the cap: writes 10 bytes (filling to 16),
	// returns ErrOverflow on the remaining.
	n, err := cw.Write([]byte("world, everything after is dropped"))
	if !errors.Is(err, ErrOverflow) {
		t.Fatalf("expected ErrOverflow, got err=%v (n=%d)", err, n)
	}
	if n != 10 {
		t.Errorf("expected 10 bytes written before cap, got %d", n)
	}
	if got := buf.String(); got != "hello world, eve" {
		t.Errorf("buffer contents wrong: %q", got)
	}
	// Subsequent writes are all refused.
	if n, err := cw.Write([]byte("more")); !errors.Is(err, ErrOverflow) || n != 0 {
		t.Errorf("post-cap write: n=%d err=%v; want n=0 err=ErrOverflow", n, err)
	}
}
