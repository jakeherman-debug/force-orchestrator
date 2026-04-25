// Fix #8f Track B — slowgit-mode init for the ctx-cancel tests.
//
// When TestRunGitCtx_CtxCancel / TestBestEffortRun_CtxCancelKillsSubprocess
// run, they install a symlink at `<tmpBin>/git` pointing back at the
// test binary, then set FIX8F_SLOWGIT_MODE=true and prepend tmpBin to
// PATH. Subsequent calls to `runGitCtx(ctx, ...)` resolve "git" via
// PATH lookup, find the symlinked test binary, and exec it. The init
// below detects the env var BEFORE Go's testing framework parses
// flags, then just sleeps — holding the inherited fd 1, fd 2
// (= runGitCtx CombinedOutput pipe write-ends) until either:
//
//   (a) the test cancels the ctx, exec.Cmd SIGKILLs the immediate
//       child (this binary), the kernel closes its fds, the
//       runGitCtx pipe write-ends have 0 references, the
//       CombinedOutput read-goroutine sees EOF, runGitCtx returns —
//       cancel-to-return latency is sub-millisecond, well within
//       the 2-second budget.
//   (b) the natural 30-second sleep elapses (pre-fix behaviour: the
//       cancel never reached this binary, so it ran to completion).
//
// Why this design beats the prior `git push` / `git ls-remote
// custom-helper` shapes: those left git as the immediate child of
// runGitCtx, and git internally fork+execs intermediate processes
// (git-receive-pack, git remote-X, git-remote-X) that inherit the
// pipe write-end. exec.Cmd's ctx-cancel only SIGKILLs the immediate
// child; intermediate processes survive and hold the pipe until
// their natural completion. CombinedOutput's read-goroutine then
// blocks for the full natural-completion time (30s in our fixture)
// — indistinguishable from the fabricated-Background regression
// case. The slowgit shape eliminates intermediate processes:
// runGitCtx's immediate child IS slowgit, SIGKILL kills it, EOF.
//
// Why init() and not TestMain: when the test binary is invoked as
// the symlinked "git", git's caller (in this case, runGitCtx itself
// from the test process) passes positional arguments meant for
// real git. Go's test framework's flag.Parse() rejects those.
// init() runs before flag parsing.

package git

import (
	"os"
	"time"
)

func init() {
	if os.Getenv("FIX8F_SLOWGIT_MODE") != "true" {
		return
	}
	// Sleep, holding inherited fds (fd 1, fd 2 = runGitCtx CombinedOutput
	// pipe write-ends, which is exactly what we want — the test relies
	// on SIGKILL of THIS process to close those fds and signal pipe
	// EOF). 30s is much longer than the 2s test-assertion budget so
	// this is effectively unbounded for test purposes; pre-fix
	// runGitCtx returns at 30s natural completion (cancel never
	// reaches us), post-fix runGitCtx returns at 250ms+epsilon.
	time.Sleep(30 * time.Second)
	os.Exit(0)
}
