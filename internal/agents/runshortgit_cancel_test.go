// Fix #8f Track B — TestRunShortGit_CtxCancel proves runShortGit (the
// astromech-package short-git helper) honours parent-ctx cancellation
// by exercising the production symbol against a slow git op and
// asserting the call returns within 2 seconds of cancel().
//
// Companion to internal/git/ctx_cancel_test.go's bestEffortRun /
// runGitCtx tests, and to Track A's TestAstromech_EstopCancelsInFlightGitOp.
// All three exercise the same property — caller-ctx cancellation reaches
// the subprocess — but at different layers:
//
//   - TestBestEffortRun_CtxCancelKillsSubprocess (internal/git): the
//     bestEffortRun helper used by internal/git callers
//   - TestRunGitCtx_CtxCancel (internal/git): runGitCtx CombinedOutput sibling
//   - TestRunShortGit_CtxCancel (this file, internal/agents): astromech's
//     runShortGit helper used by SpawnAstromech-spawned tasks
//   - TestAstromech_EstopCancelsInFlightGitOp (internal/agents): the
//     end-to-end e-stop → ctx-cancel → subprocess-kill integration
//
// This test focuses on the runShortGit helper specifically — a simpler
// shape than the integration test (no SetEstop required; just ctx
// cancellation propagation). Spec-named per FIX-8E-PROMPT § Track B.

package agents

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// setupRunShortGitSlowRemote creates a bare git remote whose pre-receive
// hook sleeps 30s, plus a working tree pre-staged with one commit
// pointing at it. Distinct name from setupSlowGitRemote (used in
// astromech_estop_cancel_test.go) to keep the two integration test
// files independent.
func setupRunShortGitSlowRemote(t *testing.T) string {
	t.Helper()

	if runtime.GOOS == "windows" {
		t.Skip("pre-receive hook requires POSIX sh; not portable to Windows CI")
	}

	root := t.TempDir()
	remoteDir := filepath.Join(root, "remote.git")
	workDir := filepath.Join(root, "work")

	if err := exec.Command("git", "init", "--bare", remoteDir).Run(); err != nil {
		t.Fatalf("git init --bare: %v", err)
	}

	hookPath := filepath.Join(remoteDir, "hooks", "pre-receive")
	hookBody := "#!/bin/sh\nsleep 30\nexit 0\n"
	if err := os.WriteFile(hookPath, []byte(hookBody), 0o755); err != nil {
		t.Fatalf("write pre-receive hook: %v", err)
	}

	if err := exec.Command("git", "init", "-b", "main", workDir).Run(); err != nil {
		if err := exec.Command("git", "init", workDir).Run(); err != nil {
			t.Fatalf("git init work: %v", err)
		}
	}
	gitC := func(args ...string) {
		full := append([]string{"-C", workDir}, args...)
		if out, err := exec.Command("git", full...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", full, err, out)
		}
	}
	gitC("config", "user.email", "fix8f@example.com")
	gitC("config", "user.name", "fix8f")
	gitC("config", "commit.gpgsign", "false")
	seed := filepath.Join(workDir, "README")
	if err := os.WriteFile(seed, []byte("fix8f-runshortgit\n"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	gitC("add", "README")
	gitC("commit", "-m", "fix8f seed")
	gitC("branch", "-M", "main")
	gitC("remote", "add", "origin", "file://"+remoteDir)

	return workDir
}

// TestRunShortGit_CtxCancel proves runShortGit (internal/agents/astromech.go:38)
// honours parent-ctx cancellation. Pre-Fix-#8e the helper fabricated
// context.Background; daemon ctx cancellation could not reach the
// subprocess and a hung `git push` would block until shortGitTimeout
// (60s). Post-fix the helper wraps the passed ctx — cancel() reaches
// the subprocess within milliseconds.
func TestRunShortGit_CtxCancel(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not in PATH")
	}
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not in PATH; pre-receive hook requires POSIX sh")
	}
	if _, err := exec.LookPath("sleep"); err != nil {
		t.Skip("sleep not in PATH; pre-receive hook requires sleep")
	}

	workDir := setupRunShortGitSlowRemote(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		// Production helper by name. runShortGit lives at
		// internal/agents/astromech.go:38; its body wraps the passed ctx
		// via WithTimeout(ctx, shortGitTimeout). A regression to
		// fabricating context.Background would surface here because the
		// cancel below would no longer reach the spawned subprocess.
		done <- runShortGit(ctx, "-C", workDir, "push", "origin", "main")
	}()

	time.Sleep(250 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected cancellation error from interrupted git push; got nil — runShortGit isn't honouring ctx")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("runShortGit did not honor ctx cancellation within 2s — helper body regressed (likely fabricating context.Background)")
	}
}
