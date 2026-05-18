package main

// D12 — daemon subcommand --help / unknown-flag safety tests.
//
// These tests build the binary as a subprocess and exercise the actual
// flag-parsing boundary the operator hits. We can't call the cmdDaemon*
// functions in-process because they call os.Exit (via dispatchDaemon).
//
// Each test:
//   1. Builds force into a temp dir.
//   2. Sets HOME to a temp dir so any side effects (plist, trust file)
//      land in a sandbox.
//   3. Asserts the destructive operation did NOT occur.

import (
	"bytes"
	"database/sql"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"force-orchestrator/internal/store"
)

// buildForceForHelpTest builds the force binary into a temp dir and
// returns the path. Skips on build failure (e.g. constrained CI without
// SQLite headers).
func buildForceForHelpTest(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	binPath := filepath.Join(dir, "force")
	build := exec.Command("go", "build", "-tags", "sqlite_fts5", "-o", binPath, "./")
	build.Dir = "."
	if out, err := build.CombinedOutput(); err != nil {
		t.Skipf("can't build binary: %v (%s)", err, out)
	}
	return binPath
}

// launchdPlistPathForHome mirrors launchdPlistPath() with an explicit
// HOME so the test doesn't depend on os.UserHomeDir's env-var lookup
// timing.
func launchdPlistPathForHome(home string) string {
	return filepath.Join(home, "Library", "LaunchAgents", "com.force-orchestrator.daemon.plist")
}

func systemdUnitPathForHome(home string) string {
	return filepath.Join(home, ".config", "systemd", "user", "force-orchestrator.service")
}

// installArtifactPathForHome returns the OS-correct artifact that
// `force daemon install` would write under HOME=home.
func installArtifactPathForHome(home string) string {
	if runtime.GOOS == "linux" {
		return systemdUnitPathForHome(home)
	}
	return launchdPlistPathForHome(home)
}

// TestDaemonInstall_HelpFlag_DoesNotInstall: smoking-gun regression for
// the D12 P4 bug. `force daemon install --help` MUST print help and exit
// 0 WITHOUT writing the launchd plist / systemd unit.
func TestDaemonInstall_HelpFlag_DoesNotInstall(t *testing.T) {
	bin := buildForceForHelpTest(t)
	home := t.TempDir()
	artifact := installArtifactPathForHome(home)

	cmd := exec.Command(bin, "daemon", "install", "--help")
	cmd.Env = append(os.Environ(), "HOME="+home, "FORCE_DIR="+home)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err != nil {
		t.Fatalf("daemon install --help should exit 0, got %v\nstdout: %s\nstderr: %s",
			err, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "Usage: force daemon install") {
		t.Errorf("expected help output, got stdout=%q", stdout.String())
	}
	if _, statErr := os.Stat(artifact); statErr == nil {
		t.Errorf("CRITICAL: daemon install --help wrote artifact %s — the bug is NOT fixed", artifact)
	}
}

// TestDaemonInstall_UnknownFlag_ExitsNonZero: an unrecognized flag must
// reject with non-zero exit + usage on stderr, BEFORE any side-effect.
func TestDaemonInstall_UnknownFlag_ExitsNonZero(t *testing.T) {
	bin := buildForceForHelpTest(t)
	home := t.TempDir()
	artifact := installArtifactPathForHome(home)

	cmd := exec.Command(bin, "daemon", "install", "--bogus-flag")
	cmd.Env = append(os.Environ(), "HOME="+home, "FORCE_DIR="+home)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err == nil {
		t.Fatalf("daemon install --bogus-flag must exit non-zero")
	}
	combined := stderr.String()
	if !strings.Contains(combined, "not defined") && !strings.Contains(combined, "unknown") && !strings.Contains(combined, "unrecognized") {
		t.Errorf("stderr should mention the flag is unrecognized, got: %q", combined)
	}
	if _, statErr := os.Stat(artifact); statErr == nil {
		t.Errorf("daemon install --bogus-flag wrote artifact %s — must reject before any side-effect", artifact)
	}
}

// TestDaemonInstall_DryRunFlag_StillWorks: regression guard — the
// pre-fix dry-run path must continue to render the would-be artifact
// without writing it.
func TestDaemonInstall_DryRunFlag_StillWorks(t *testing.T) {
	bin := buildForceForHelpTest(t)
	home := t.TempDir()
	artifact := installArtifactPathForHome(home)

	cmd := exec.Command(bin, "daemon", "install", "--dry-run")
	cmd.Env = append(os.Environ(), "HOME="+home, "FORCE_DIR="+home)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err != nil {
		t.Fatalf("daemon install --dry-run should exit 0, got %v\nstderr: %s", err, stderr.String())
	}
	if !strings.Contains(stdout.String(), "[dry-run] would write") {
		t.Errorf("expected dry-run preview, got stdout=%q", stdout.String())
	}
	if _, statErr := os.Stat(artifact); statErr == nil {
		t.Errorf("dry-run must NOT write %s", artifact)
	}
}

// TestDaemonUninstall_HelpFlag_DoesNotUninstall: pre-create a fake
// plist, then run `daemon uninstall --help`. The plist must still exist
// after the call.
func TestDaemonUninstall_HelpFlag_DoesNotUninstall(t *testing.T) {
	bin := buildForceForHelpTest(t)
	home := t.TempDir()
	artifact := installArtifactPathForHome(home)

	// Pre-create the artifact under HOME so we can detect deletion.
	if err := os.MkdirAll(filepath.Dir(artifact), 0o755); err != nil {
		t.Fatalf("setup: mkdir %s: %v", filepath.Dir(artifact), err)
	}
	if err := os.WriteFile(artifact, []byte("<plist/>"), 0o644); err != nil {
		t.Fatalf("setup: write %s: %v", artifact, err)
	}

	cmd := exec.Command(bin, "daemon", "uninstall", "--help")
	cmd.Env = append(os.Environ(), "HOME="+home, "FORCE_DIR="+home)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("daemon uninstall --help should exit 0, got %v\nstderr: %s", err, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Usage: force daemon uninstall") {
		t.Errorf("expected uninstall help, got stdout=%q", stdout.String())
	}
	if _, statErr := os.Stat(artifact); statErr != nil {
		t.Errorf("daemon uninstall --help removed %s — must not run side-effect", artifact)
	}
}

// TestDaemonUpdate_HelpFlag_DoesNotMutate: the destructive update path
// (binary swap + DaemonUpdateHistory write + trust-file append) must
// not run when --help is the first arg. We assert no trust file is
// created and no DaemonUpdateHistory row is written.
func TestDaemonUpdate_HelpFlag_DoesNotMutate(t *testing.T) {
	bin := buildForceForHelpTest(t)
	home := t.TempDir()
	dbPath := filepath.Join(home, "holocron.db")

	// Initialize a holocron under HOME so we can query DaemonUpdateHistory
	// after the call.
	db := store.InitHolocronDSN(dbPath)
	preCount := countDaemonUpdateHistoryRows(t, db)
	db.Close()

	cmd := exec.Command(bin, "daemon", "update", "--help")
	cmd.Env = append(os.Environ(), "HOME="+home, "FORCE_DIR="+home)
	cmd.Dir = home
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("daemon update --help should exit 0, got %v\nstderr: %s", err, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Usage: force daemon update") {
		t.Errorf("expected update help, got stdout=%q", stdout.String())
	}

	// Trust file: HOME/.force/trusted-binary-hashes — must NOT exist.
	tp := filepath.Join(home, ".force", "trusted-binary-hashes")
	if _, statErr := os.Stat(tp); statErr == nil {
		t.Errorf("daemon update --help mutated trust file %s — must not run side-effect", tp)
	}

	// DaemonUpdateHistory: row count must be unchanged.
	db = store.InitHolocronDSN(dbPath)
	defer db.Close()
	postCount := countDaemonUpdateHistoryRows(t, db)
	if postCount != preCount {
		t.Errorf("daemon update --help wrote %d DaemonUpdateHistory row(s) — must not record side-effect", postCount-preCount)
	}
}

// TestDaemonClearCrashBudget_HelpFlag_DoesNotClear: pre-populate
// DaemonStartLog with a row, run `daemon clear-crash-budget --help`,
// assert the row survives.
func TestDaemonClearCrashBudget_HelpFlag_DoesNotClear(t *testing.T) {
	bin := buildForceForHelpTest(t)
	home := t.TempDir()
	dbPath := filepath.Join(home, "holocron.db")

	db := store.InitHolocronDSN(dbPath)
	// Insert a synthetic DaemonStartLog row so we can detect a truncate.
	if _, err := db.Exec(`INSERT INTO DaemonStartLog (ts, binary_sha, pid, outcome)
		VALUES (datetime('now'), 'test-sha', 1234, 'started')`); err != nil {
		t.Fatalf("seed DaemonStartLog: %v", err)
	}
	preCount := countDaemonStartLogRows(t, db)
	if preCount == 0 {
		t.Fatalf("seed failed: DaemonStartLog still empty")
	}
	db.Close()

	cmd := exec.Command(bin, "daemon", "clear-crash-budget", "--help")
	cmd.Env = append(os.Environ(), "HOME="+home, "FORCE_DIR="+home)
	cmd.Dir = home
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("daemon clear-crash-budget --help should exit 0, got %v\nstderr: %s", err, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Usage: force daemon clear-crash-budget") {
		t.Errorf("expected clear-crash-budget help, got stdout=%q", stdout.String())
	}

	db = store.InitHolocronDSN(dbPath)
	defer db.Close()
	postCount := countDaemonStartLogRows(t, db)
	if postCount != preCount {
		t.Errorf("daemon clear-crash-budget --help truncated DaemonStartLog (%d → %d) — must not run side-effect",
			preCount, postCount)
	}
}

// countDaemonUpdateHistoryRows returns the row count for the audit
// table. Used to assert no row was written across a --help invocation.
func countDaemonUpdateHistoryRows(t *testing.T, db *sql.DB) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM DaemonUpdateHistory`).Scan(&n); err != nil {
		t.Fatalf("count DaemonUpdateHistory: %v", err)
	}
	return n
}

// countDaemonStartLogRows returns the row count for the crash-budget
// table. Used to assert clear-crash-budget --help leaves rows intact.
func countDaemonStartLogRows(t *testing.T, db *sql.DB) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM DaemonStartLog`).Scan(&n); err != nil {
		t.Fatalf("count DaemonStartLog: %v", err)
	}
	return n
}
