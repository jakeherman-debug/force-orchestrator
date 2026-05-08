package main

// D12 P1 — end-to-end singleton check.
//
// Acceptance bar #6: "two concurrent `force daemon foreground` invocations:
// second one exits 1 with 'daemon already running' before any agent loop
// starts." The original shape of this test called `singleton.Acquire`
// twice in-process, which proved the helper's flock contract held but
// did NOT prove that two REAL `force daemon foreground` processes
// rejected the second start. The verifier flagged this gap as a
// SEV-MEDIUM follow-up.
//
// This file now uses a real subprocess test:
//   1. Build the daemon binary into t.TempDir().
//   2. Spawn process A with --exit-after-acquire-singleton
//      --hold-singleton-for=5s and HOME redirected to a TempDir so the
//      PID file lives at $TMPDIR/.force/force.pid (no collision with the
//      operator's real ~/.force/force.pid).
//   3. Wait until A has actually written its PID file (poll up to 3s).
//   4. Spawn process B with the same HOME and assert it exits non-zero
//      with the "daemon already running" rejection message in its
//      combined output.
//   5. Wait for A to exit cleanly (exit 0).
//
// The hidden --exit-after-acquire-singleton + --hold-singleton-for flags
// (declared in cmd/force/fleet_cmds.go::cmdDaemon) make this hermetic —
// neither process spawns the agent fleet, the dashboard, or touches a
// real Holocron.

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"force-orchestrator/internal/daemon/singleton"
)

// TestE2E_TwoConcurrentDaemons_SingletonRejectsSecond spawns two real
// `force daemon foreground` subprocesses against the same PID file and
// asserts the singleton gate rejects the second one.
func TestE2E_TwoConcurrentDaemons_SingletonRejectsSecond(t *testing.T) {
	if testing.Short() {
		t.Skip("subprocess build is too slow for -short")
	}

	binDir := t.TempDir()
	binPath := filepath.Join(binDir, "force")

	// Build a fresh binary into the temp dir. We build the same package
	// the operator runs (cmd/force) so the smoke flag we declared lands
	// in the produced executable. `go build` runs from this test's cwd
	// (cmd/force/), so the package path is the current directory `.`.
	build := exec.Command("go", "build", "-tags", "sqlite_fts5", "-o", binPath, ".")
	build.Stderr = os.Stderr
	if buildOut, err := build.Output(); err != nil {
		t.Fatalf("go build daemon binary: %v\n%s", err, buildOut)
	}

	// HOME redirected so singleton.DefaultPIDPath() resolves to
	// $HOME/.force/force.pid inside the test sandbox.
	homeDir := t.TempDir()
	pidPath := filepath.Join(homeDir, ".force", "force.pid")

	// Process A: holds the singleton for 5s, then exits 0.
	holdDur := 5 * time.Second
	startedAt := time.Now()
	cmdA := exec.Command(binPath,
		"daemon", "foreground",
		"--exit-after-acquire-singleton",
		"--hold-singleton-for="+holdDur.String(),
	)
	cmdA.Env = append(os.Environ(), "HOME="+homeDir)
	var aOut, aErr bytes.Buffer
	cmdA.Stdout = &aOut
	cmdA.Stderr = &aErr
	if err := cmdA.Start(); err != nil {
		t.Fatalf("start process A: %v", err)
	}
	t.Cleanup(func() {
		// Best-effort kill in case the test panics before Wait.
		if cmdA.Process != nil {
			_ = cmdA.Process.Kill()
		}
	})

	// Wait until process A has acquired the lock — poll the PID file
	// existence + IsLocked. Bound the wait at 3s so a failing build /
	// startup error surfaces fast. The subprocess prints
	// "smoke singleton acquired" on stdout once it's holding the lock,
	// but reading the buffer concurrently is racy; IsLocked is the
	// authoritative signal.
	deadline := time.Now().Add(3 * time.Second)
	var locked bool
	for time.Now().Before(deadline) {
		l, _, ierr := singleton.IsLocked(pidPath)
		if ierr == nil && l {
			locked = true
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !locked {
		// A may have already exited (build sane, but flag broken). Capture
		// its output to diagnose.
		_ = cmdA.Process.Kill()
		_ = cmdA.Wait()
		t.Fatalf("process A never acquired singleton at %s within 3s\nstdout:\n%s\nstderr:\n%s",
			pidPath, aOut.String(), aErr.String())
	}

	// Process B: should be rejected by the singleton gate. We give it a
	// short hold-for so even if (somehow) it acquired the lock, it would
	// exit promptly without bootstrapping.
	cmdB := exec.Command(binPath,
		"daemon", "foreground",
		"--exit-after-acquire-singleton",
		"--hold-singleton-for=100ms",
	)
	cmdB.Env = append(os.Environ(), "HOME="+homeDir)
	var bOut, bErr bytes.Buffer
	cmdB.Stdout = &bOut
	cmdB.Stderr = &bErr
	bRunErr := cmdB.Run()

	// Process B must exit non-zero.
	if bRunErr == nil {
		// Defensive: kill A, then fail.
		_ = cmdA.Process.Kill()
		_ = cmdA.Wait()
		t.Fatalf("process B exited 0 but should have been rejected by singleton\nB stdout:\n%s\nB stderr:\n%s",
			bOut.String(), bErr.String())
	}
	var bExitErr *exec.ExitError
	if !errors.As(bRunErr, &bExitErr) {
		t.Fatalf("process B failed to start (not an exit error): %v", bRunErr)
	}
	if bExitErr.ExitCode() == 0 {
		t.Fatalf("process B exit code = 0; want non-zero")
	}
	// The actual cmdDaemon path uses fmt.Println for the rejection
	// message, which goes to stdout. Match against the combined output
	// so the assertion is robust to either stream choice.
	bCombined := bOut.String() + bErr.String()
	if !strings.Contains(bCombined, "daemon already running") {
		t.Fatalf("process B output missing rejection message\nstdout:\n%s\nstderr:\n%s",
			bOut.String(), bErr.String())
	}

	// Process A must exit 0 within its hold window (+ generous slack).
	aDone := make(chan error, 1)
	go func() { aDone <- cmdA.Wait() }()
	select {
	case aErrWait := <-aDone:
		if aErrWait != nil {
			t.Fatalf("process A Wait: %v\nstdout:\n%s\nstderr:\n%s",
				aErrWait, aOut.String(), aErr.String())
		}
	case <-time.After(holdDur + 10*time.Second):
		_ = cmdA.Process.Kill()
		t.Fatalf("process A did not exit within hold+10s\nstdout:\n%s\nstderr:\n%s",
			aOut.String(), aErr.String())
	}
	if cmdA.ProcessState == nil || cmdA.ProcessState.ExitCode() != 0 {
		t.Fatalf("process A exit code = %v; want 0\nstdout:\n%s\nstderr:\n%s",
			cmdA.ProcessState, aOut.String(), aErr.String())
	}

	// Sanity: A should have held the lock for at least most of holdDur.
	// (Not a strict floor — kernel scheduling slack is fine.)
	elapsed := time.Since(startedAt)
	if elapsed < holdDur/2 {
		t.Errorf("process A returned too quickly: elapsed=%s want >= %s",
			elapsed, holdDur/2)
	}

	// Sanity: A's stdout should mention the smoke acquisition log.
	if !strings.Contains(aOut.String(), "smoke singleton acquired") {
		t.Errorf("process A stdout missing smoke log line\nstdout:\n%s", aOut.String())
	}
}

// TestE2E_StaleDaemonPID_TakeoverLogs: a leftover PID file from a
// crashed daemon should NOT block the next start. This is still
// in-process because it exercises only singleton.Acquire's stale-file
// handling — no concurrency race involved.
func TestE2E_StaleDaemonPID_TakeoverLogs(t *testing.T) {
	dir := t.TempDir()
	pidPath := filepath.Join(dir, "force.pid")
	// Seed a stale PID. PID 99999 is well above the typical max.
	if err := os.WriteFile(pidPath, []byte("99999\n"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}

	release, stale, err := singleton.Acquire(pidPath)
	if err != nil {
		t.Fatalf("Acquire on stale should succeed, got %v", err)
	}
	defer release()
	if !stale.Stale {
		t.Errorf("expected Stale=true on takeover")
	}
	if stale.PriorPID != 99999 {
		t.Errorf("PriorPID = %d, want 99999", stale.PriorPID)
	}
}
