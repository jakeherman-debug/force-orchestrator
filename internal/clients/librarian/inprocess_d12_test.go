package librarian

// inprocess_d12_test.go — Sweep-E (D12) tests for the LLM-backed
// GenerateRepoDescription. Covers:
//
//   - Happy path (stubbed Claude returns a known description)
//   - LIVE_HAIKU_DISABLED falls back to the regex scrape
//   - LLM error falls back to the regex scrape
//   - No README → empty string
//   - Sanitiser strips "Description:" / bullet / quote prefixes
//   - Long output is rune-truncated to ReadmeDescriptionMaxLen + "…"
//   - "(no description)" sentinel collapses to empty
//
// The Claude CLI is stubbed via SetCLIRunner / ResetCLIRunner (same
// pattern the transcript tests use). The transcript-DB seam is also
// installed so the LLM call's row gets recorded — we don't assert on
// the row content here (transcript_test.go covers that) but the
// install ensures the call path is exercised end-to-end.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"force-orchestrator/internal/claude"
	"force-orchestrator/internal/store"
)

// writeReadme is a tiny helper — creates dir/README.md with the
// supplied content.
func writeReadme(t *testing.T, dir, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte(content), 0644); err != nil {
		t.Fatalf("writeReadme: %v", err)
	}
}

// setUpClaudeStub wires both the CLI runner stub and the transcript
// DB so GenerateRepoDescription can run end-to-end without hitting a
// real claude process. Cleanup restores both.
func setUpClaudeStub(t *testing.T, response string, callErr error) {
	t.Helper()
	claude.SetCLIRunner(func(_ context.Context, _, _, _, _, _ string, _ int, _ time.Duration) (string, error) {
		return response, callErr
	})
	t.Cleanup(func() { claude.SetCLIRunner(claude.DefaultCLIRunner) })

	db := store.InitHolocronDSN(":memory:")
	t.Cleanup(func() { db.Close() })
	claude.SetTranscriptDB(db)
	t.Cleanup(func() { claude.SetTranscriptDB(nil) })
}

// TestGenerateRepoDescription_LiveHaiku_HappyPath stubs the LLM to
// return a known description; the helper must return it verbatim
// (modulo trim).
func TestGenerateRepoDescription_LiveHaiku_HappyPath(t *testing.T) {
	// LIVE_HAIKU enabled means LIVE_HAIKU_DISABLED is NOT set.
	t.Setenv("LIVE_HAIKU_DISABLED", "")
	setUpClaudeStub(t, "A test repo for unit testing.", nil)

	dir := t.TempDir()
	writeReadme(t, dir, "# Some project\n\nA real README with prose.\n")

	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	c := NewInProcess(db)

	got, err := c.GenerateRepoDescription(context.Background(), dir)
	if err != nil {
		t.Fatalf("GenerateRepoDescription: %v", err)
	}
	want := "A test repo for unit testing."
	if got != want {
		t.Errorf("\nwant: %q\ngot:  %q", want, got)
	}
}

// TestGenerateRepoDescription_LiveHaikuDisabled_FallsBackToRegex
// pins LIVE_HAIKU_DISABLED=1 and confirms the helper returns the
// regex-scrape result (without making any LLM call).
func TestGenerateRepoDescription_LiveHaikuDisabled_FallsBackToRegex(t *testing.T) {
	t.Setenv("LIVE_HAIKU_DISABLED", "1")

	// Stub the runner to FAIL so we can prove the LLM path was never
	// reached — if GenerateRepoDescription called the runner, the
	// error would propagate (or, post-fallback, hide the regex result
	// behind the fallback path); either way the test contract is "no
	// LLM call when LIVE_HAIKU is disabled".
	called := false
	claude.SetCLIRunner(func(_ context.Context, _, _, _, _, _ string, _ int, _ time.Duration) (string, error) {
		called = true
		return "", errors.New("LLM should not have been called")
	})
	t.Cleanup(func() { claude.SetCLIRunner(claude.DefaultCLIRunner) })

	dir := t.TempDir()
	writeReadme(t, dir, "# My Repo\n\nThis is the regex-scraped description.\n")

	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	c := NewInProcess(db)

	got, err := c.GenerateRepoDescription(context.Background(), dir)
	if err != nil {
		t.Fatalf("GenerateRepoDescription: %v", err)
	}
	if called {
		t.Errorf("LLM runner should NOT be called when LIVE_HAIKU_DISABLED=1")
	}
	want := "This is the regex-scraped description."
	if got != want {
		t.Errorf("\nwant: %q\ngot:  %q", want, got)
	}
}

// TestGenerateRepoDescription_LLMError_FallsBackToRegex stubs the LLM
// to return an error; the helper must fall back to the regex scrape
// rather than propagating the error to the operator.
func TestGenerateRepoDescription_LLMError_FallsBackToRegex(t *testing.T) {
	t.Setenv("LIVE_HAIKU_DISABLED", "")
	setUpClaudeStub(t, "", errors.New("simulated LLM failure"))

	dir := t.TempDir()
	writeReadme(t, dir, "# Title\n\nFallback paragraph from the README.\n")

	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	c := NewInProcess(db)

	got, err := c.GenerateRepoDescription(context.Background(), dir)
	if err != nil {
		t.Fatalf("GenerateRepoDescription should not propagate LLM error: %v", err)
	}
	want := "Fallback paragraph from the README."
	if got != want {
		t.Errorf("\nwant: %q\ngot:  %q", want, got)
	}
}

// TestGenerateRepoDescription_NoReadme_ReturnsEmpty creates a temp
// dir with no README; both LLM and stub should produce nothing.
func TestGenerateRepoDescription_NoReadme_ReturnsEmpty(t *testing.T) {
	t.Setenv("LIVE_HAIKU_DISABLED", "")
	// Stub the runner — but the helper short-circuits when no README
	// is found, so the stub should never be hit. We assert that.
	called := false
	claude.SetCLIRunner(func(_ context.Context, _, _, _, _, _ string, _ int, _ time.Duration) (string, error) {
		called = true
		return "should not have been called", nil
	})
	t.Cleanup(func() { claude.SetCLIRunner(claude.DefaultCLIRunner) })

	dir := t.TempDir()
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	c := NewInProcess(db)

	got, err := c.GenerateRepoDescription(context.Background(), dir)
	if err != nil {
		t.Fatalf("GenerateRepoDescription: %v", err)
	}
	if got != "" {
		t.Errorf("no README must return empty, got %q", got)
	}
	if called {
		t.Errorf("LLM runner should not fire when README is absent")
	}
}

// TestGenerateRepoDescription_SanitizesPrefix exercises the
// "Description:" prefix stripper.
func TestGenerateRepoDescription_SanitizesPrefix(t *testing.T) {
	t.Setenv("LIVE_HAIKU_DISABLED", "")
	setUpClaudeStub(t, "Description: A repo.", nil)

	dir := t.TempDir()
	writeReadme(t, dir, "# foo\n\nbody\n")

	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	c := NewInProcess(db)

	got, err := c.GenerateRepoDescription(context.Background(), dir)
	if err != nil {
		t.Fatalf("GenerateRepoDescription: %v", err)
	}
	want := "A repo."
	if got != want {
		t.Errorf("\nwant: %q\ngot:  %q", want, got)
	}
}

// TestGenerateRepoDescription_TruncatesLong asserts a 500-char
// response is rune-truncated to ReadmeDescriptionMaxLen + "…".
func TestGenerateRepoDescription_TruncatesLong(t *testing.T) {
	t.Setenv("LIVE_HAIKU_DISABLED", "")
	long := strings.Repeat("x", 500)
	setUpClaudeStub(t, long, nil)

	dir := t.TempDir()
	writeReadme(t, dir, "# foo\n\nbody\n")

	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	c := NewInProcess(db)

	got, err := c.GenerateRepoDescription(context.Background(), dir)
	if err != nil {
		t.Fatalf("GenerateRepoDescription: %v", err)
	}
	if !strings.HasSuffix(got, "…") {
		t.Errorf("truncated output must end with ellipsis, got tail=%q", got[len(got)-3:])
	}
	gotRunes := []rune(got)
	if len(gotRunes) != ReadmeDescriptionMaxLen+1 {
		t.Errorf("truncated rune length = %d, want %d", len(gotRunes), ReadmeDescriptionMaxLen+1)
	}
}

// TestGenerateRepoDescription_NoDescriptionSentinel stubs the LLM to
// return the "(no description)" sentinel; the helper must collapse it
// to "" before any fallback (regex would also be empty for this test's
// minimal README, which a) confirms the sentinel path and b) doesn't
// trip a fallback-masquerade).
func TestGenerateRepoDescription_NoDescriptionSentinel(t *testing.T) {
	t.Setenv("LIVE_HAIKU_DISABLED", "")
	setUpClaudeStub(t, "(no description)", nil)

	dir := t.TempDir()
	// README that the regex scraper also rejects (only HTML comment).
	writeReadme(t, dir, "<!-- copyright -->\n")

	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	c := NewInProcess(db)

	got, err := c.GenerateRepoDescription(context.Background(), dir)
	if err != nil {
		t.Fatalf("GenerateRepoDescription: %v", err)
	}
	if got != "" {
		t.Errorf("sentinel must collapse to empty, got %q", got)
	}
}

// TestGenerateRepoDescription_BulletPrefixStripped is the sanitiser
// edge case for "- A repo." → "A repo." (some models emit a leading
// bullet despite the instructions).
func TestGenerateRepoDescription_BulletPrefixStripped(t *testing.T) {
	t.Setenv("LIVE_HAIKU_DISABLED", "")
	setUpClaudeStub(t, "- A bulleted repo description.", nil)

	dir := t.TempDir()
	writeReadme(t, dir, "# foo\n\nbody\n")

	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	c := NewInProcess(db)

	got, err := c.GenerateRepoDescription(context.Background(), dir)
	if err != nil {
		t.Fatalf("GenerateRepoDescription: %v", err)
	}
	want := "A bulleted repo description."
	if got != want {
		t.Errorf("\nwant: %q\ngot:  %q", want, got)
	}
}

// TestGenerateRepoDescription_QuoteStripped covers the "matched
// quotes" sanitisation edge case.
func TestGenerateRepoDescription_QuoteStripped(t *testing.T) {
	t.Setenv("LIVE_HAIKU_DISABLED", "")
	setUpClaudeStub(t, "\"A quoted repo.\"", nil)

	dir := t.TempDir()
	writeReadme(t, dir, "# foo\n\nbody\n")

	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	c := NewInProcess(db)

	got, err := c.GenerateRepoDescription(context.Background(), dir)
	if err != nil {
		t.Fatalf("GenerateRepoDescription: %v", err)
	}
	want := "A quoted repo."
	if got != want {
		t.Errorf("\nwant: %q\ngot:  %q", want, got)
	}
}

// TestGenerateRepoDescription_EmptyPath returns an error.
func TestGenerateRepoDescription_EmptyPath(t *testing.T) {
	t.Setenv("LIVE_HAIKU_DISABLED", "1")
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	c := NewInProcess(db)

	_, err := c.GenerateRepoDescription(context.Background(), "")
	if err == nil {
		t.Errorf("expected error for empty path, got nil")
	}
}

// TestSanitizeGeneratedDescription_TableDriven exercises the pure
// sanitiser without an LLM call.
func TestSanitizeGeneratedDescription_TableDriven(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"plain", "Hello world.", "Hello world."},
		{"trim", "  Hello world.  ", "Hello world."},
		{"description prefix", "Description: Hello.", "Hello."},
		{"summary prefix", "Summary: Hello.", "Hello."},
		{"bullet dash", "- Hello.", "Hello."},
		{"bullet star", "* Hello.", "Hello."},
		{"numbered list 1.", "1. Hello.", "Hello."},
		{"numbered list 1)", "1) Hello.", "Hello."},
		{"double-quoted", `"Hello."`, "Hello."},
		{"single-quoted", "'Hello.'", "Hello."},
		{"backticks", "`Hello.`", "Hello."},
		{"curly double", "“Hello.”", "Hello."},
		{"sentinel exact", "(no description)", ""},
		{"sentinel mixed case", "(No Description)", ""},
		{"empty", "", ""},
		{"two lines", "Line one.\nLine two.", "Line one. Line two."},
		{"strip usage annotation", "Hello.\n[claude_usage: 100 input 50 output]", "Hello."},
		{"compound — bullet + prefix + quote", `- "Description: Hello."`, "Hello."},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := sanitizeGeneratedDescription(c.in)
			if got != c.want {
				t.Errorf("sanitize(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

// TestSanitizeGeneratedDescription_TruncatesLong covers the rune-
// truncate path directly.
func TestSanitizeGeneratedDescription_TruncatesLong(t *testing.T) {
	long := strings.Repeat("x", 500)
	got := sanitizeGeneratedDescription(long)
	if !strings.HasSuffix(got, "…") {
		t.Errorf("expected trailing ellipsis, got tail=%q", got[len(got)-3:])
	}
	gotRunes := []rune(got)
	if len(gotRunes) != ReadmeDescriptionMaxLen+1 {
		t.Errorf("rune length = %d, want %d", len(gotRunes), ReadmeDescriptionMaxLen+1)
	}
}

// TestGenerateRepoDescription_MockClient sanity-checks the
// MockClient hook so unit tests in other packages can rely on it.
func TestGenerateRepoDescription_MockClient(t *testing.T) {
	m := NewMock()
	m.GenerateRepoDescriptionFn = func(_ context.Context, repoPath string) (string, error) {
		return fmt.Sprintf("mocked for %s", repoPath), nil
	}
	got, err := m.GenerateRepoDescription(context.Background(), "/some/path")
	if err != nil {
		t.Fatalf("mock: %v", err)
	}
	if got != "mocked for /some/path" {
		t.Errorf("got %q", got)
	}
	if len(m.GenerateRepoDescriptionCalls) != 1 || m.GenerateRepoDescriptionCalls[0] != "/some/path" {
		t.Errorf("call recording broken: %+v", m.GenerateRepoDescriptionCalls)
	}
	// Reset clears the recording.
	m.Reset()
	if len(m.GenerateRepoDescriptionCalls) != 0 {
		t.Errorf("Reset did not clear call history")
	}
}
