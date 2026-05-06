package precommit_test

// D13 P3 — docs-check pre-commit hook integration tests.
//
// Mirror of the architecture-md-check / onboarding-md-check pattern:
// drive scripts/pre-commit/docs-check.sh against synthetic staged
// blobs in a temp git repo and assert the exit codes.
//
// The hook's slow path (`go test ./internal/audittools/...`) is heavy
// — running it inside a temp repo would require checking out the
// audittools package. So our integration tests focus on the
// fast-path: the hook should exit 0 cleanly when no *.md is staged,
// and skip cleanly when go isn't in PATH or audittools isn't
// resolvable. The full validation contract is exercised directly via
// the underlying Go tests.
//
// Skipped when bash or git is unavailable on the host.

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

func locateDocsCheckHook(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatalf("runtime.Caller failed")
	}
	return filepath.Join(filepath.Dir(thisFile), "docs-check.sh")
}

// TestDocsCheckHook_NoMarkdownStaged_ExitsZero confirms the fast-path:
// a commit with no *.md files staged exits 0 without running the Go
// test suite.
func TestDocsCheckHook_NoMarkdownStaged_ExitsZero(t *testing.T) {
	skipIfMissing(t, "git")
	skipIfMissing(t, "bash")

	dir := newTempGitRepo(t)
	// Stage a non-markdown file so the diff-filter actually has
	// content to consider.
	if err := os.WriteFile(filepath.Join(dir, "code.go"),
		[]byte("package x\n"), 0644); err != nil {
		t.Fatalf("write code.go: %v", err)
	}
	cmd := exec.Command("git", "-C", dir, "add", "code.go")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git add: %s", out)
	}

	hook := locateDocsCheckHook(t)
	code, out := runHook(t, dir, hook)
	if code != 0 {
		t.Errorf("expected exit 0 (no *.md staged); got %d, out=%s", code, out)
	}
}

// TestDocsCheckHook_NotInRepo_ExitsTwo confirms the pre-flight check:
// when the hook runs outside a git repo (e.g. a temp dir with no
// .git/), it returns exit 2.
func TestDocsCheckHook_NotInRepo_ExitsTwo(t *testing.T) {
	skipIfMissing(t, "bash")
	skipIfMissing(t, "git")

	dir := t.TempDir()
	hook := locateDocsCheckHook(t)
	code, out := runHook(t, dir, hook)
	if code != 2 {
		t.Errorf("expected exit 2 (not in a git repo); got %d, out=%s", code, out)
	}
}
