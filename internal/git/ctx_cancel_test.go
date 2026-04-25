// Fix #8e — cancellation-propagation tests for the internal/git helpers.
// Pre-fix the helpers fabricated `context.WithTimeout(context.Background(),
// shortGitOpTimeout)` internally; daemon cancellation could not interrupt a
// `git fetch` on an unreachable remote until the fabricated 5-min deadline
// fired. Post-fix the helpers wrap the CALLER's ctx, so a SIGINT/e-stop
// cancellation propagates within seconds. These tests prove that contract.
//
// Fix #8f — rewritten to actually call the production helpers
// (bestEffortRun, runGitCtx) instead of inline-mirroring their shape. The
// prior shape-mirror form constructed an `exec.CommandContext(c, "sleep",
// "30")` block that mimicked the helper body. A regression in the helper
// body (e.g. reverting to `WithTimeout(context.Background(), T)`) would
// have left the shape-mirror tests passing — they tested the author's
// asserted shape, not the helper's body. The Fix #8e verifier flagged
// this gap as Defect #4 (non-blocker but worth closing). Each rewritten
// test now exercises the production symbol with a slow-op fixture so any
// future divergence in the helper body surfaces here at -race -count=5.
//
// Slow-op fixture: a "slowgit" — a symlink at <tmpBin>/git pointing back
// at the test binary, with FIX8F_SLOWGIT_MODE=true set so the test
// binary's init() (in ctx_cancel_helper_init_test.go) detects the env
// var on subprocess startup and sleeps for 30 seconds instead of running
// tests. tmpBin is prepended to PATH for the test, so when runGitCtx
// resolves the binary "git" via PATH lookup, it finds the symlink and
// execs the test binary as the slowgit.
//
// The crucial property of this fixture: slowgit IS the immediate child
// of runGitCtx's exec.Cmd. When ctx cancels, exec.Cmd SIGKILLs the
// immediate child; the kernel closes its inherited fd 1 / fd 2 (which
// are the runGitCtx CombinedOutput pipe write-ends); the pipe has zero
// remaining write-end references; CombinedOutput's read-goroutine sees
// EOF; runGitCtx returns. Cancel-to-return latency is sub-millisecond,
// well within the 2-second assertion budget.
//
// Why this design beats the prior `git ls-remote <slow-helper>`
// fixtures (including the pre-receive-hook + push variant): real git
// ls-remote forks intermediate processes (git remote-fix8fhang, then
// the actual remote helper) that inherit the runGitCtx CombinedOutput
// pipe write-end via fd 2. exec.Cmd's ctx-cancel only SIGKILLs the
// IMMEDIATE child (top-level git); the intermediate `git remote-X`
// process survives and holds the pipe write-end until its natural
// completion (~30s in our fixture). CombinedOutput's read-goroutine
// then blocks for the full 30s — indistinguishable from the
// fabricated-Background regression case. The slowgit shape eliminates
// intermediate processes entirely: SIGKILL of slowgit is sufficient
// because slowgit IS the only process holding the pipe write-end.
//
// This was empirically determined: the helper-redirect fixtures
// (shell-based and Go-based, including brute-force syscall.Close(0)
// through syscall.Close(65535) in init()) all left runGitCtx blocked
// for the full 30s natural-completion of the helper, because the
// intermediate `git remote-X` process held the pipe regardless of
// what the leaf helper did. lsof confirmed: with `git ls-remote
// fix8f://hang`, the surviving pipe holder is `git remote-fix8f`
// (the intermediate process), not the leaf helper.

package git

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// installSlowGit symlinks the test binary at `<tmpBin>/git` and prepends
// tmpBin to PATH. Sets FIX8F_SLOWGIT_MODE=true so the symlink-invocation
// path goes through the slowgit-mode init in
// ctx_cancel_helper_init_test.go.
func installSlowGit(t *testing.T) {
	t.Helper()

	if runtime.GOOS == "windows" {
		t.Skip("test-binary symlink fixture not portable to Windows CI")
	}

	tmpBin := t.TempDir()
	gitShim := filepath.Join(tmpBin, "git")

	self, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	if err := os.Symlink(self, gitShim); err != nil {
		t.Fatalf("symlink %s -> %s: %v", gitShim, self, err)
	}

	t.Setenv("FIX8F_SLOWGIT_MODE", "true")
	t.Setenv("PATH", tmpBin+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// TestBestEffortRun_CtxCancelKillsSubprocess proves bestEffortRun honours
// parent-ctx cancellation. It calls the production helper with a slowgit
// shim and asserts the call returns within 2 seconds of cancel().
//
// Pre-Fix-#8e the helper fabricated context.Background, so a cancel had
// no effect and the call would block until shortGitOpTimeout (5m) fired
// — or until the slowgit exited on its own (~30s in our fixture).
// Pre-Fix-#8f this test inlined `exec.CommandContext(c, "sleep", "30")`
// and never called bestEffortRun — a regression in the helper body
// would not have been observable. Both gaps are now closed.
func TestBestEffortRun_CtxCancelKillsSubprocess(t *testing.T) {
	installSlowGit(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{})
	go func() {
		// Production helper by name. bestEffortRun internally wraps the
		// passed ctx via WithTimeout(ctx, shortGitOpTimeout); a regression
		// to fabricating Background would surface here because the cancel
		// below would no longer reach the spawned slowgit subprocess.
		// Args are arbitrary — slowgit ignores them and just sleeps.
		bestEffortRun(ctx, "fix8f-cancel-test", "rev-parse", "HEAD")
		close(done)
	}()

	time.Sleep(250 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("bestEffortRun did not honor ctx cancellation within 2s — helper body regressed (likely fabricating context.Background)")
	}
}

// TestRunGitCtx_CtxCancel proves runGitCtx honours parent-ctx cancellation.
// Same shape as TestBestEffortRun_CtxCancelKillsSubprocess but exercises
// the CombinedOutput path. Fix #8f — rewritten from the prior shape-mirror
// form to actually call the production helper. The slowgit fixture is
// critical here: CombinedOutput's pipe goroutines wait for EOF on the
// merged stdout/stderr pipe, and any pipe-inheriting descendant would
// block them past the 2s budget. With slowgit as the immediate child,
// SIGKILL of slowgit closes its fds and unblocks CombinedOutput
// directly.
func TestRunGitCtx_CtxCancel(t *testing.T) {
	installSlowGit(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	type result struct {
		out []byte
		err error
	}
	done := make(chan result, 1)
	go func() {
		// Production helper by name. runGitCtx returns CombinedOutput +
		// error; the test only cares about the latency of the return.
		out, err := runGitCtx(ctx, "rev-parse", "HEAD")
		done <- result{out: out, err: err}
	}()

	time.Sleep(250 * time.Millisecond)
	cancel()

	select {
	case r := <-done:
		if r.err == nil {
			t.Fatalf("expected cancellation error from interrupted slowgit; got nil (output: %s)", r.out)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("runGitCtx did not honor ctx cancellation within 2s — helper body regressed (likely fabricating context.Background)")
	}
}
