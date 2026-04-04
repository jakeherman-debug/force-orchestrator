package agents

import (
	"bytes"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"force-orchestrator/internal/claude"
)

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
func withStubCLIRunner(t *testing.T, output string, err error) {
	t.Helper()
	claude.SetCLIRunner(func(prompt, tools, dir string, maxTurns int, timeout time.Duration) (string, error) {
		return output, err
	})
	t.Cleanup(claude.ResetCLIRunner)
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
