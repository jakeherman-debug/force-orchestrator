package agents

import (
	"context"
	"bytes"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"force-orchestrator/internal/claude"
)

// stubCLIRunner is the state shared by a single test's Claude stub. It
// records every call the stub receives so a test can make bounded-cost
// assertions (Fix #7 / AUDIT-111, -113, -135, -161, -162).
//
// The shape is intentionally simple: a monotonic call counter + a full
// prompt history. Tests that care only about bounds read CallCount();
// tests that care about prompt structure read Prompts(). A canned output
// pair drives the (stdout, err) the stub returns on every call.
type stubCLIRunner struct {
	callCount atomic.Int64
	mu        sync.Mutex
	prompts   []string
	tools     []string
	dirs      []string
	output    string
	err       error
	// fn, if set, overrides the static (output, err). Tests use this to
	// return different canned responses per call (e.g. adversarial parse
	// failures alternating with valid JSON).
	fn func(_ context.Context, prompt, tools, dir string, maxTurns int, timeout time.Duration) (string, error)
}

// CallCount returns the current number of Claude invocations observed.
func (s *stubCLIRunner) CallCount() int64 { return s.callCount.Load() }

// LastPrompt returns the most recent user prompt, or "" if none yet.
func (s *stubCLIRunner) LastPrompt() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.prompts) == 0 {
		return ""
	}
	return s.prompts[len(s.prompts)-1]
}

// Prompts returns a copy of every prompt captured so far (in order).
func (s *stubCLIRunner) Prompts() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, len(s.prompts))
	copy(out, s.prompts)
	return out
}

// captureOutput captures everything written to os.Stdout during f().
func captureOutput(f func()) string {
	r, w, err := os.Pipe()
	if err != nil {
		return ""
	}
	old := os.Stdout
	os.Stdout = w
	f()
	w.Close()
	os.Stdout = old
	var buf bytes.Buffer
	io.Copy(&buf, r)
	return buf.String()
}

// initTestRepo creates a fresh git repo in a temp dir, commits an initial file,
// and returns the repo path. Tests that need git are skipped if git is not found.
func initTestRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not found in PATH — skipping git integration test")
	}

	dir := t.TempDir()

	gitRun := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=Test",
			"GIT_AUTHOR_EMAIL=test@test.com",
			"GIT_COMMITTER_NAME=Test",
			"GIT_COMMITTER_EMAIL=test@test.com",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %s", args, out)
		}
	}

	gitRun("init", "-b", "main")
	gitRun("config", "user.email", "test@test.com")
	gitRun("config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("hello\n"), 0644); err != nil {
		t.Fatal(err)
	}
	gitRun("add", ".")
	gitRun("commit", "-m", "initial commit")

	return dir
}

// withStubCLIRunner replaces the claude package's cliRunner for the duration of the test.
//
// Returns a *stubCLIRunner the caller can read to assert CallCount and
// Prompts. Existing callers that ignore the return value keep working
// (the stub still returns (output, err) on every call).
//
// Fix #7 (AUDIT-111): this replaces a stateless closure that made it
// impossible to assert how many Claude calls a test path produced. Every
// new cost-loop test MUST check CallCount against an expected bound.
func withStubCLIRunner(t *testing.T, output string, err error) *stubCLIRunner {
	t.Helper()
	s := &stubCLIRunner{output: output, err: err}
	claude.SetCLIRunner(func(_ context.Context, prompt, tools, dir string, maxTurns int, timeout time.Duration) (string, error) {
		s.callCount.Add(1)
		s.mu.Lock()
		s.prompts = append(s.prompts, prompt)
		s.tools = append(s.tools, tools)
		s.dirs = append(s.dirs, dir)
		fn := s.fn
		s.mu.Unlock()
		if fn != nil {
			return fn(context.Background(), prompt, tools, dir, maxTurns, timeout)
		}
		return s.output, s.err
	})
	t.Cleanup(claude.ResetCLIRunner)
	return s
}

// withStubCLIRunnerFn is like withStubCLIRunner but lets the test provide
// a per-call dispatcher. Used for adversarial LLM stubs that return
// different responses on different calls (e.g. pass 1 = malformed JSON,
// pass 2 = needs_work, pass 3 = clean).
func withStubCLIRunnerFn(t *testing.T, fn func(_ context.Context, prompt, tools, dir string, maxTurns int, timeout time.Duration) (string, error)) *stubCLIRunner {
	t.Helper()
	s := &stubCLIRunner{fn: fn}
	claude.SetCLIRunner(func(_ context.Context, prompt, tools, dir string, maxTurns int, timeout time.Duration) (string, error) {
		s.callCount.Add(1)
		s.mu.Lock()
		s.prompts = append(s.prompts, prompt)
		s.tools = append(s.tools, tools)
		s.dirs = append(s.dirs, dir)
		s.mu.Unlock()
		return fn(context.Background(), prompt, tools, dir, maxTurns, timeout)
	})
	t.Cleanup(claude.ResetCLIRunner)
	return s
}

// setupBranchWithCommit creates a branch with a file change on it, leaving the
// repo on main when done. Returns the branch name.
func setupBranchWithCommit(t *testing.T, repoDir, branchName string) string {
	t.Helper()
	gitEnv := append(os.Environ(),
		"GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=t@t.com",
		"GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=t@t.com",
	)
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", repoDir}, args...)...)
		cmd.Env = gitEnv
		if out, err2 := cmd.CombinedOutput(); err2 != nil {
			t.Fatalf("git %v: %s", args, string(out))
		}
	}
	run("checkout", "-b", branchName)
	if err := os.WriteFile(filepath.Join(repoDir, "change.txt"), []byte("changed\n"), 0644); err != nil {
		t.Fatal(err)
	}
	run("add", ".")
	run("commit", "-m", "branch change")
	run("checkout", "main")
	return branchName
}

// withClaudeStub installs testdata/claude-stub as "claude" in PATH for the
// duration of the test, and sets any supplied environment variables.
func withClaudeStub(t *testing.T, env map[string]string) {
	t.Helper()

	// Locate the stub script relative to cmd/force/testdata/
	stubSrc, err := filepath.Abs(filepath.Join("..", "..", "cmd", "force", "testdata", "claude-stub"))
	if err != nil || func() bool { _, e := os.Stat(stubSrc); return e != nil }() {
		t.Skip("testdata/claude-stub not found — skipping E2E stub test")
	}

	// Copy stub into a temp dir as "claude" so exec.LookPath resolves it
	stubDir := t.TempDir()
	stubDst := filepath.Join(stubDir, "claude")
	data, err := os.ReadFile(stubSrc)
	if err != nil {
		t.Fatalf("read stub: %v", err)
	}
	if err := os.WriteFile(stubDst, data, 0755); err != nil {
		t.Fatalf("write stub: %v", err)
	}

	// Prepend stub dir to PATH
	origPath := os.Getenv("PATH")
	os.Setenv("PATH", stubDir+":"+origPath)

	// Set caller-supplied env vars
	origVals := map[string]string{}
	for k, v := range env {
		origVals[k] = os.Getenv(k)
		os.Setenv(k, v)
	}

	t.Cleanup(func() {
		os.Setenv("PATH", origPath)
		for k, v := range origVals {
			if v == "" {
				os.Unsetenv(k)
			} else {
				os.Setenv(k, v)
			}
		}
	})
}
