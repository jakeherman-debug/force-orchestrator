// Package singleton provides a PID-file + flock based singleton lock for
// the force daemon. D12 P1 — single-instance enforcement.
//
// Why two states?
//   - LOCK_EX | LOCK_NB exclusive lock: the FD is held open for the
//     daemon's lifetime. A second process attempting to flock the same
//     file gets EWOULDBLOCK and exits 1.
//   - PID file content: the live PID is written so operators (and
//     `force daemon status`) can identify the running daemon without
//     having to scrape `ps`.
//
// On a clean shutdown, Acquire's release closure removes the PID file
// AND drops the flock (closing the FD releases it). On a crash the FD
// is closed by the kernel, which also drops the flock — the next
// daemon start sees a "stale" PID file but flock succeeds, and the
// caller logs the takeover.
package singleton

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

// ErrAlreadyRunning is returned by Acquire when another live daemon
// holds the singleton lock. The PID of the holder is exposed via the
// embedded Pid field.
type ErrAlreadyRunning struct {
	Pid  int
	Path string
}

func (e *ErrAlreadyRunning) Error() string {
	if e.Pid > 0 {
		return fmt.Sprintf("daemon already running (PID %d) — see `force daemon stop`", e.Pid)
	}
	return fmt.Sprintf("daemon already running (PID file %s held)", e.Path)
}

// Acquire opens pidPath, takes a non-blocking exclusive flock, writes
// the current PID, and returns a release closure.
//
// The release closure:
//   - drops the flock (Close on the FD)
//   - removes the PID file (best-effort — a missing file is fine)
//
// Errors:
//   - ErrAlreadyRunning: another live daemon holds the lock.
//   - underlying os/syscall error: file system / permission failure.
//
// Stale handling: if pidPath exists but no process holds the flock,
// flock succeeds; the caller observes the prior content (via
// IsLocked or by reading the file) and logs the takeover. The current
// PID then overwrites the file.
func Acquire(pidPath string) (release func(), staleHint StaleInfo, err error) {
	if pidPath == "" {
		return nil, StaleInfo{}, errors.New("singleton.Acquire: pidPath is empty")
	}

	if dir := filepath.Dir(pidPath); dir != "" && dir != "." {
		if mkErr := os.MkdirAll(dir, 0o755); mkErr != nil {
			return nil, StaleInfo{}, fmt.Errorf("create pid dir %s: %w", dir, mkErr)
		}
	}

	// Capture any prior PID file content BEFORE we open + truncate the
	// file. The "stale takeover" log line in cmdDaemon depends on this.
	prior, _ := readPID(pidPath)

	f, err := os.OpenFile(pidPath, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return nil, StaleInfo{}, fmt.Errorf("open pid file %s: %w", pidPath, err)
	}

	if flockErr := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); flockErr != nil {
		// Lock is held by another live daemon. Read the PID for the
		// operator-friendly error message.
		holder, _ := readPID(pidPath)
		_ = f.Close()
		return nil, StaleInfo{}, &ErrAlreadyRunning{Pid: holder, Path: pidPath}
	}

	// We hold the lock. Truncate + write our PID.
	if _, seekErr := f.Seek(0, 0); seekErr != nil {
		_ = f.Close()
		return nil, StaleInfo{}, fmt.Errorf("seek pid file: %w", seekErr)
	}
	if truncErr := f.Truncate(0); truncErr != nil {
		_ = f.Close()
		return nil, StaleInfo{}, fmt.Errorf("truncate pid file: %w", truncErr)
	}
	if _, wErr := fmt.Fprintf(f, "%d\n", os.Getpid()); wErr != nil {
		_ = f.Close()
		return nil, StaleInfo{}, fmt.Errorf("write pid file: %w", wErr)
	}
	// Flush to disk so a `cat pid` from another tool sees the value.
	_ = f.Sync()

	stale := StaleInfo{}
	if prior > 0 && prior != os.Getpid() {
		stale = StaleInfo{Stale: true, PriorPID: prior}
	}

	release = func() {
		// Order matters: remove the file FIRST so a new daemon starting
		// before our Close() doesn't see our PID and conclude "still
		// running". The flock keeps the start-race safe regardless of
		// ordering, but removing first avoids a misleading PID read.
		_ = os.Remove(pidPath)
		_ = f.Close()
	}
	return release, stale, nil
}

// StaleInfo describes a pre-existing PID file that Acquire took over
// after the prior owner died (flock would have failed otherwise).
// Zero value means no prior file.
type StaleInfo struct {
	Stale    bool
	PriorPID int
}

// IsLocked reports whether another process currently holds the
// singleton lock on pidPath. If yes, returns the holder PID (best
// effort — the file may be racy mid-write).
//
// Implementation: open the file with O_RDONLY, attempt a non-blocking
// shared flock. If that succeeds, the file is unlocked (we drop it
// immediately). If it fails with EWOULDBLOCK, the lock is held.
func IsLocked(pidPath string) (locked bool, pid int, err error) {
	if pidPath == "" {
		return false, 0, errors.New("singleton.IsLocked: pidPath is empty")
	}
	f, openErr := os.Open(pidPath)
	if openErr != nil {
		if errors.Is(openErr, os.ErrNotExist) {
			return false, 0, nil
		}
		return false, 0, openErr
	}
	defer f.Close()

	// Try a non-blocking SHARED lock. If a daemon holds LOCK_EX, this
	// fails with EWOULDBLOCK — that's our "locked" signal.
	flockErr := unix.Flock(int(f.Fd()), unix.LOCK_SH|unix.LOCK_NB)
	if flockErr == nil {
		// We acquired a shared lock — nobody holds exclusive. Drop it.
		_ = unix.Flock(int(f.Fd()), unix.LOCK_UN)
		return false, 0, nil
	}
	if errors.Is(flockErr, syscall.EWOULDBLOCK) || flockErr == unix.EAGAIN {
		holder, _ := readPID(pidPath)
		return true, holder, nil
	}
	return false, 0, flockErr
}

// readPID parses the integer PID from the pid file. Empty / malformed
// content returns 0.
func readPID(path string) (int, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		return 0, err
	}
	return pid, nil
}

// DefaultPIDPath returns the canonical pid-file path: ~/.force/force.pid.
// Resolves $HOME portably; falls back to /tmp/force.pid if no HOME is
// set (CI / minimal-user environments).
func DefaultPIDPath() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return filepath.Join(os.TempDir(), "force.pid")
	}
	return filepath.Join(home, ".force", "force.pid")
}
