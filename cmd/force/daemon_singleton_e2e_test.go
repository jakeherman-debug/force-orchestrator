package main

// D12 P1 — end-to-end singleton check: two concurrent acquirers of
// the same PID path must produce one success + one ErrAlreadyRunning,
// matching acceptance bar #6 ("two concurrent `force daemon foreground`
// invocations: second one exits 1 with 'daemon already running' before
// any agent loop starts").
//
// We can't actually spawn two `force daemon foreground` processes in a
// unit test without dragging in the entire Holocron + agent fleet, so
// this test exercises the singleton package directly via
// `singleton.Acquire` — the same call cmdDaemon makes — proving the
// gate fires before any agent loop runs.

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"force-orchestrator/internal/daemon/singleton"
)

func TestE2E_TwoConcurrentDaemons_SingletonRejectsSecond(t *testing.T) {
	dir := t.TempDir()
	pidPath := filepath.Join(dir, "force.pid")

	r1, _, err := singleton.Acquire(pidPath)
	if err != nil {
		t.Fatalf("first Acquire: %v", err)
	}
	defer r1()

	// Second concurrent Acquire (same process, different FD — flock
	// rejects). In a real two-process scenario, a child process'
	// flock fails the same way, and cmdDaemon prints
	// ErrAlreadyRunning + os.Exits 1.
	_, _, err = singleton.Acquire(pidPath)
	if err == nil {
		t.Fatalf("second Acquire should have failed but got nil")
	}
	var alreadyErr *singleton.ErrAlreadyRunning
	if !errors.As(err, &alreadyErr) {
		t.Errorf("second Acquire returned %T; want *ErrAlreadyRunning", err)
	}
	// The error message must match what cmdDaemon prints to the
	// operator (acceptance #6).
	msg := err.Error()
	if !strE2EContains(msg, "daemon already running") {
		t.Errorf("error message = %q; want it to contain 'daemon already running'", msg)
	}
}

// TestE2E_StaleDaemonPID_TakeoverLogs: a leftover PID file from a
// crashed daemon should NOT block the next start.
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

func strE2EContains(haystack, needle string) bool {
	if len(needle) > len(haystack) {
		return false
	}
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}
