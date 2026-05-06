package singleton

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestAcquire_HappyPath: clean acquire on a fresh path, release works.
func TestAcquire_HappyPath(t *testing.T) {
	dir := t.TempDir()
	pidPath := filepath.Join(dir, "force.pid")

	release, stale, err := Acquire(pidPath)
	if err != nil {
		t.Fatalf("Acquire failed: %v", err)
	}
	if stale.Stale {
		t.Errorf("expected non-stale on fresh path, got Stale=true PriorPID=%d", stale.PriorPID)
	}
	defer release()

	// PID file should contain our PID
	b, err := os.ReadFile(pidPath)
	if err != nil {
		t.Fatalf("read pid file: %v", err)
	}
	got := strings.TrimSpace(string(b))
	want := strings.TrimSpace(string([]byte(itoa(os.Getpid()))))
	if got != want {
		t.Errorf("pid file content = %q, want %q", got, want)
	}

	// IsLocked should return true while we hold it
	locked, holder, err := IsLocked(pidPath)
	if err != nil {
		t.Fatalf("IsLocked: %v", err)
	}
	if !locked {
		t.Errorf("expected locked, got unlocked")
	}
	if holder != os.Getpid() {
		t.Errorf("holder PID = %d, want %d", holder, os.Getpid())
	}
}

// TestAcquire_DoubleAcquire_SameProcess: same process can't take the
// same lock twice (fail-fast safety net for double-init in tests).
func TestAcquire_DoubleAcquire_SameProcess(t *testing.T) {
	dir := t.TempDir()
	pidPath := filepath.Join(dir, "force.pid")

	release, _, err := Acquire(pidPath)
	if err != nil {
		t.Fatalf("first Acquire failed: %v", err)
	}
	defer release()

	// flock semantics on Linux/macOS: a SECOND flock from the same
	// process on the same file (different FD) gets EWOULDBLOCK.
	_, _, err2 := Acquire(pidPath)
	if err2 == nil {
		t.Fatalf("second Acquire should have failed but got nil")
	}
	var alreadyErr *ErrAlreadyRunning
	if !errors.As(err2, &alreadyErr) {
		t.Errorf("expected ErrAlreadyRunning, got %T: %v", err2, err2)
	}
}

// TestAcquire_AfterRelease: a release allows re-acquisition.
func TestAcquire_AfterRelease(t *testing.T) {
	dir := t.TempDir()
	pidPath := filepath.Join(dir, "force.pid")

	r1, _, err := Acquire(pidPath)
	if err != nil {
		t.Fatalf("first Acquire failed: %v", err)
	}
	r1()

	r2, _, err := Acquire(pidPath)
	if err != nil {
		t.Fatalf("second Acquire after release failed: %v", err)
	}
	defer r2()
}

// TestAcquire_StaleTakeover: pre-existing PID file with no live holder.
// We simulate by writing a PID and not flocking — Acquire should
// succeed and report stale=true.
func TestAcquire_StaleTakeover(t *testing.T) {
	dir := t.TempDir()
	pidPath := filepath.Join(dir, "force.pid")

	// Write a stale PID file (no flock held). PID 99999 is well above
	// the typical pid_max of 32768 / 99999 — likely-dead even on Linux.
	if err := os.WriteFile(pidPath, []byte("99999\n"), 0o644); err != nil {
		t.Fatalf("seed stale pid: %v", err)
	}

	release, stale, err := Acquire(pidPath)
	if err != nil {
		t.Fatalf("Acquire over stale failed: %v", err)
	}
	defer release()
	if !stale.Stale || stale.PriorPID != 99999 {
		t.Errorf("expected Stale=true PriorPID=99999, got %+v", stale)
	}
}

// TestAcquire_OtherProcessHolds: spawn a child that holds the lock,
// confirm the parent's Acquire fails with ErrAlreadyRunning.
func TestAcquire_OtherProcessHolds(t *testing.T) {
	if testing.Short() {
		t.Skip("skip in -short mode (spawns subprocess)")
	}
	dir := t.TempDir()
	pidPath := filepath.Join(dir, "force.pid")

	// Use a tiny Go program: same package, but invoked via a helper
	// flag. We instead use a subtest pattern: rerun ourselves with
	// FORCE_HOLD_LOCK=1 in env.
	if os.Getenv("FORCE_HOLD_LOCK") == "1" {
		holdPath := os.Getenv("FORCE_HOLD_LOCK_PATH")
		release, _, err := Acquire(holdPath)
		if err != nil {
			os.Exit(2)
		}
		// Hold for 5s
		time.Sleep(5 * time.Second)
		release()
		os.Exit(0)
	}

	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	cmd := exec.Command(exe, "-test.run", "TestAcquire_OtherProcessHolds", "-test.timeout=30s")
	cmd.Env = append(os.Environ(),
		"FORCE_HOLD_LOCK=1",
		"FORCE_HOLD_LOCK_PATH="+pidPath,
	)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start child: %v", err)
	}
	defer func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	}()

	// Wait up to 2s for the child to take the lock.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		locked, _, _ := IsLocked(pidPath)
		if locked {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	// Now our Acquire MUST fail.
	_, _, err = Acquire(pidPath)
	if err == nil {
		t.Fatalf("expected Acquire to fail while child holds lock")
	}
	var alreadyErr *ErrAlreadyRunning
	if !errors.As(err, &alreadyErr) {
		t.Fatalf("expected ErrAlreadyRunning, got %T: %v", err, err)
	}
	if alreadyErr.Pid != cmd.Process.Pid {
		t.Errorf("err.Pid = %d, want child PID %d", alreadyErr.Pid, cmd.Process.Pid)
	}
}

// TestIsLocked_NotPresent: missing path is not locked.
func TestIsLocked_NotPresent(t *testing.T) {
	dir := t.TempDir()
	pidPath := filepath.Join(dir, "no-such-file.pid")
	locked, pid, err := IsLocked(pidPath)
	if err != nil {
		t.Fatalf("IsLocked: %v", err)
	}
	if locked || pid != 0 {
		t.Errorf("expected unlocked/0, got %v/%d", locked, pid)
	}
}

// TestDefaultPIDPath: returns a non-empty path.
func TestDefaultPIDPath(t *testing.T) {
	p := DefaultPIDPath()
	if p == "" {
		t.Errorf("DefaultPIDPath returned empty")
	}
	if !filepath.IsAbs(p) {
		t.Errorf("DefaultPIDPath %q is not absolute", p)
	}
}

// itoa avoids strconv to keep the import surface small.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
