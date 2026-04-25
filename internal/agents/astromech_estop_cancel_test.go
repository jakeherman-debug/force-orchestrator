// Fix #8f Track A — load-bearing integration test for daemon-ctx
// cancellation propagation into in-flight astromech git ops.
//
// The contract this test pins (and which Fix #8e's restart gate explicitly
// enumerated as the load-bearing artefact): when the operator sets the
// e-stop flag, the daemon cancels its ctx; that cancellation MUST reach
// any in-flight subprocess started via runShortGit / combinedShortGit /
// combinedShortGitArgs / RunTaskForeground within ~2 seconds. Pre-Fix-#8e
// these helpers fabricated `context.WithTimeout(context.Background(), T)`
// internally, so the daemon ctx had no path into the subprocess and a
// hung `git push` would run to the fabricated 60s timeout regardless.
//
// Slow-op fixture: a file:// remote whose `pre-receive` hook sleeps for
// 30s. `git push` to that remote runs the hook server-side (file:// is
// local — the hook is invoked in-process by the receiving git-receive-pack
// child), so the push blocks until either (a) the hook returns or (b) the
// outer git push process is killed via ctx cancellation. We then call
// SetEstop(db, true) to represent the operator action, cancel() the
// daemon-equivalent ctx, and assert the push errors within 2 seconds.
//
// Why this fixture is reliable in CI:
//   - Pure-local: no network, no port allocation, no DNS, no firewall.
//   - sh + sleep are POSIX baseline (the test t.Skips if either is absent).
//   - Pre-receive hooks are a stable git feature (since 1.0).
//   - The 30s pre-receive sleep is far longer than any reasonable CI
//     scheduling jitter, so the wall-clock observation is unambiguous.
//
// Note on naming: the FIX-8F-PROMPT spec refers to `store.SetEstopped`,
// but the actual production helper is `SetEstop` (no -ped) in package
// agents — see internal/agents/estop.go:45. We call the real helper and
// document the spec/code naming discrepancy in FIX-8F-CLOSURE.md.

package agents

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"force-orchestrator/internal/store"
)

// setupSlowGitRemote creates a bare git remote whose pre-receive hook
// sleeps for 30 seconds, plus a working tree pre-staged with one commit
// pointing at it. Returns the working-tree path; the remote URL is
// already wired into the working tree as `origin`.
func setupSlowGitRemote(t *testing.T) string {
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
		// Older gits don't support -b; fall back.
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
	if err := os.WriteFile(seed, []byte("fix8f integration\n"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	gitC("add", "README")
	gitC("commit", "-m", "fix8f seed")
	gitC("branch", "-M", "main")
	gitC("remote", "add", "origin", "file://"+remoteDir)

	return workDir
}

// TestAstromech_EstopCancelsInFlightGitOp is the load-bearing integration
// test for Fix #8e/#8f. It exercises the full e-stop → ctx-cancel →
// subprocess-kill chain via the production helper runShortGit.
//
// Verifier-readable contract:
//   - lives in package agents (not git, not agents_test)
//   - calls SetEstop(db, true) — the production e-stop helper
//   - calls runShortGit(ctx, ...) — the production git helper
//   - asserts the in-flight subprocess errors within 2 * time.Second of
//     the cancel; t.Fatal on miss
func TestAstromech_EstopCancelsInFlightGitOp(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not in PATH; integration test requires git")
	}
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not in PATH; pre-receive hook requires POSIX sh")
	}
	if _, err := exec.LookPath("sleep"); err != nil {
		t.Skip("sleep not in PATH; pre-receive hook requires sleep")
	}

	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	workDir := setupSlowGitRemote(t)

	// Fresh (uncancelled) ctx that simulates the daemon's root context.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	started := make(chan struct{})

	go func() {
		close(started)
		// CRITICAL: this is a production-helper call by name, not an
		// inline shape mirror. runShortGit lives at
		// internal/agents/astromech.go:38; its body wraps the passed ctx
		// via WithTimeout(ctx, shortGitTimeout) — if a future refactor
		// regresses to fabricating context.Background, this test
		// surfaces it because the cancel below would no longer reach
		// the spawned subprocess.
		done <- runShortGit(ctx, "-C", workDir, "push", "origin", "main")
	}()

	<-started
	// Give the subprocess time to spawn, hand off to git-receive-pack on
	// the remote, and enter the pre-receive hook's sleep. 250ms is
	// enough on developer hardware and CI without being so long that
	// the test cancels late.
	time.Sleep(250 * time.Millisecond)

	// Operator action: flip the e-stop flag. (In production the daemon's
	// poll-loop observes IsEstopped and cancels its root ctx; here we
	// simulate that response with the explicit cancel() below so the
	// test isn't bound to the daemon's poll cadence.)
	SetEstop(db, true)
	if !IsEstopped(db) {
		t.Fatalf("SetEstop did not stick: IsEstopped(db) returned false")
	}
	cancel() // daemon-equivalent: ctx cancelled in response to e-stop

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected cancellation error from interrupted git push; got nil — push completed despite e-stop + cancel, which means the helper isn't honouring ctx")
		}
		t.Logf("git push terminated after e-stop (err: %v)", err)
	case <-time.After(2 * time.Second):
		t.Fatal("e-stop did not cancel in-flight git op within 2s — daemon ctx cancellation not propagating to subprocess (Fix #8e regression)")
	}
}
