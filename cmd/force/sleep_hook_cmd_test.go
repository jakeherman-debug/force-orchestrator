// D3 P6A.13 / exit criterion 13 — tests for `force install-sleep-hook`.
//
// Validates:
//   - happy path on darwin: ~/.sleep + ~/.wakeup written, exit 0
//   - idempotence: re-running the command on a force-owned file is a
//     no-op (no error, file content stable)
//   - operator-authored protection: existing ~/.sleep without the
//     marker is NOT overwritten without --force
//   - --check: reports state without modifying files
//   - --uninstall: removes force-owned files, preserves operator-
//     authored ones
//   - linux / unsupported: prints helpful message and exit 0
//   - missing sleepwatcher: errors unless --force
//
// Tests redirect sleepHookHomeFunc to a per-test temp dir and stub
// sleepHookExecLookPath / sleepHookOS so the test never touches the
// real $HOME or queries the real PATH.
package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// withSleepHookEnv installs the test stubs and restores originals
// on cleanup.
func withSleepHookEnv(t *testing.T, homeDir, goos string, swPresent bool) {
	t.Helper()
	origHome := sleepHookHomeFunc
	origLook := sleepHookExecLookPath
	origOS := sleepHookOS

	sleepHookHomeFunc = func() (string, error) { return homeDir, nil }
	sleepHookOS = func() string { return goos }
	if swPresent {
		sleepHookExecLookPath = func(name string) (string, error) {
			if name == "sleepwatcher" {
				return "/usr/local/bin/sleepwatcher", nil
			}
			return "", exec.ErrNotFound
		}
	} else {
		sleepHookExecLookPath = func(name string) (string, error) {
			return "", exec.ErrNotFound
		}
	}

	t.Cleanup(func() {
		sleepHookHomeFunc = origHome
		sleepHookExecLookPath = origLook
		sleepHookOS = origOS
	})
}

func TestInstallSleepHook_DarwinHappyPath(t *testing.T) {
	tmp := t.TempDir()
	withSleepHookEnv(t, tmp, "darwin", true)

	rc := cmdInstallSleepHook(context.Background(), nil, []string{})
	if rc != 0 {
		t.Fatalf("expected exit 0, got %d", rc)
	}

	for _, name := range []string{".sleep", ".wakeup"} {
		path := filepath.Join(tmp, name)
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		if !strings.Contains(string(body), SleepHookMarker) {
			t.Errorf("%s missing marker line %q\n%s", path, SleepHookMarker, body)
		}
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat %s: %v", path, err)
		}
		// Mode 0o755 — executable for OS to invoke.
		if info.Mode()&0o100 == 0 {
			t.Errorf("%s mode %o does not have user-execute bit", path, info.Mode())
		}
	}

	// Wake script must reference the heartbeat endpoint — that's the
	// integration contract per docs/roadmap.md exit 13.
	wake, _ := os.ReadFile(filepath.Join(tmp, ".wakeup"))
	if !strings.Contains(string(wake), "/api/dashboard/health") {
		t.Errorf("wake script missing heartbeat endpoint reference:\n%s", wake)
	}
}

func TestInstallSleepHook_Idempotence(t *testing.T) {
	tmp := t.TempDir()
	withSleepHookEnv(t, tmp, "darwin", true)

	rc1 := cmdInstallSleepHook(context.Background(), nil, []string{})
	if rc1 != 0 {
		t.Fatalf("first install: expected 0, got %d", rc1)
	}
	first, _ := os.ReadFile(filepath.Join(tmp, ".sleep"))
	firstStat, _ := os.Stat(filepath.Join(tmp, ".sleep"))

	rc2 := cmdInstallSleepHook(context.Background(), nil, []string{})
	if rc2 != 0 {
		t.Fatalf("re-install: expected 0, got %d", rc2)
	}
	second, _ := os.ReadFile(filepath.Join(tmp, ".sleep"))
	if string(first) != string(second) {
		t.Errorf("re-install produced different content:\nfirst:  %s\nsecond: %s", first, second)
	}
	secondStat, _ := os.Stat(filepath.Join(tmp, ".sleep"))
	if firstStat.Mode() != secondStat.Mode() {
		t.Errorf("mode drifted: first=%o second=%o", firstStat.Mode(), secondStat.Mode())
	}
}

func TestInstallSleepHook_OperatorAuthoredProtected(t *testing.T) {
	tmp := t.TempDir()
	withSleepHookEnv(t, tmp, "darwin", true)

	// Plant an operator-authored ~/.sleep WITHOUT the marker.
	operatorBody := "#!/bin/sh\n# operator's own sleep hook\nlogger 'sleeping'\n"
	if err := os.WriteFile(filepath.Join(tmp, ".sleep"), []byte(operatorBody), 0o755); err != nil {
		t.Fatalf("plant operator sleep: %v", err)
	}

	rc := cmdInstallSleepHook(context.Background(), nil, []string{})
	if rc == 0 {
		t.Fatalf("expected non-zero exit when operator-authored ~/.sleep present, got 0")
	}

	// File preserved.
	body, err := os.ReadFile(filepath.Join(tmp, ".sleep"))
	if err != nil {
		t.Fatalf("read sleep after install: %v", err)
	}
	if string(body) != operatorBody {
		t.Errorf("operator-authored file was overwritten:\nwant: %s\ngot:  %s", operatorBody, body)
	}
}

func TestInstallSleepHook_ForceOverwritesOperatorAuthored(t *testing.T) {
	tmp := t.TempDir()
	withSleepHookEnv(t, tmp, "darwin", true)

	operatorBody := "#!/bin/sh\n# operator's own sleep hook\n"
	if err := os.WriteFile(filepath.Join(tmp, ".sleep"), []byte(operatorBody), 0o755); err != nil {
		t.Fatalf("plant: %v", err)
	}
	if err := os.WriteFile(filepath.Join(tmp, ".wakeup"), []byte(operatorBody), 0o755); err != nil {
		t.Fatalf("plant: %v", err)
	}

	rc := cmdInstallSleepHook(context.Background(), nil, []string{"--force"})
	if rc != 0 {
		t.Fatalf("expected exit 0 with --force, got %d", rc)
	}
	body, _ := os.ReadFile(filepath.Join(tmp, ".sleep"))
	if !strings.Contains(string(body), SleepHookMarker) {
		t.Errorf("--force should overwrite operator-authored file with marker'd content")
	}
}

func TestInstallSleepHook_Check(t *testing.T) {
	tmp := t.TempDir()
	withSleepHookEnv(t, tmp, "darwin", true)

	// Empty home — both files missing.
	rc := cmdInstallSleepHook(context.Background(), nil, []string{"--check"})
	if rc != 0 {
		t.Errorf("--check with no files: expected 0, got %d", rc)
	}
	// No file should have been written.
	if _, err := os.Stat(filepath.Join(tmp, ".sleep")); !os.IsNotExist(err) {
		t.Errorf("--check should NOT create files, but ~/.sleep exists")
	}

	// Now install for real, then re-check.
	if rc := cmdInstallSleepHook(context.Background(), nil, []string{}); rc != 0 {
		t.Fatalf("install: %d", rc)
	}
	if rc := cmdInstallSleepHook(context.Background(), nil, []string{"--check"}); rc != 0 {
		t.Errorf("--check after install: expected 0, got %d", rc)
	}
}

func TestInstallSleepHook_Uninstall(t *testing.T) {
	tmp := t.TempDir()
	withSleepHookEnv(t, tmp, "darwin", true)

	if rc := cmdInstallSleepHook(context.Background(), nil, []string{}); rc != 0 {
		t.Fatalf("install: %d", rc)
	}
	if rc := cmdInstallSleepHook(context.Background(), nil, []string{"--uninstall"}); rc != 0 {
		t.Fatalf("uninstall: %d", rc)
	}
	for _, name := range []string{".sleep", ".wakeup"} {
		path := filepath.Join(tmp, name)
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("expected %s removed after uninstall, got err=%v", path, err)
		}
	}
}

func TestInstallSleepHook_UninstallPreservesOperatorAuthored(t *testing.T) {
	tmp := t.TempDir()
	withSleepHookEnv(t, tmp, "darwin", true)

	operatorBody := "#!/bin/sh\n# operator's own\n"
	sleepPath := filepath.Join(tmp, ".sleep")
	if err := os.WriteFile(sleepPath, []byte(operatorBody), 0o755); err != nil {
		t.Fatalf("plant: %v", err)
	}

	if rc := cmdInstallSleepHook(context.Background(), nil, []string{"--uninstall"}); rc != 0 {
		t.Fatalf("uninstall: %d", rc)
	}
	body, err := os.ReadFile(sleepPath)
	if err != nil {
		t.Fatalf("operator file should be preserved: %v", err)
	}
	if string(body) != operatorBody {
		t.Errorf("operator file content changed:\nwant: %s\ngot:  %s", operatorBody, body)
	}
}

func TestInstallSleepHook_NoSleepwatcher(t *testing.T) {
	tmp := t.TempDir()
	withSleepHookEnv(t, tmp, "darwin", false) // sleepwatcher NOT in PATH

	rc := cmdInstallSleepHook(context.Background(), nil, []string{})
	if rc == 0 {
		t.Errorf("expected non-zero exit when sleepwatcher missing, got 0")
	}
	if _, err := os.Stat(filepath.Join(tmp, ".sleep")); !os.IsNotExist(err) {
		t.Errorf("install should NOT have written ~/.sleep when sleepwatcher missing")
	}

	// --force should bypass the sleepwatcher check.
	rc = cmdInstallSleepHook(context.Background(), nil, []string{"--force"})
	if rc != 0 {
		t.Errorf("--force without sleepwatcher: expected 0, got %d", rc)
	}
	if _, err := os.Stat(filepath.Join(tmp, ".sleep")); err != nil {
		t.Errorf("--force should write ~/.sleep even without sleepwatcher: %v", err)
	}
}

func TestInstallSleepHook_LinuxBranch(t *testing.T) {
	tmp := t.TempDir()
	withSleepHookEnv(t, tmp, "linux", true)

	rc := cmdInstallSleepHook(context.Background(), nil, []string{})
	if rc != 0 {
		t.Errorf("linux branch: expected exit 0 (informational), got %d", rc)
	}
	// No files should be written on linux.
	if _, err := os.Stat(filepath.Join(tmp, ".sleep")); !os.IsNotExist(err) {
		t.Errorf("linux branch must NOT write files (got err=%v)", err)
	}
}

func TestInstallSleepHook_UnsupportedOS(t *testing.T) {
	tmp := t.TempDir()
	withSleepHookEnv(t, tmp, "freebsd", true)

	rc := cmdInstallSleepHook(context.Background(), nil, []string{})
	if rc != 0 {
		t.Errorf("unsupported OS branch: expected 0 (informational), got %d", rc)
	}
}

func TestInstallSleepHook_HelpFlag(t *testing.T) {
	rc := cmdInstallSleepHook(context.Background(), nil, []string{"-h"})
	if rc != 0 {
		t.Errorf("-h: expected 0, got %d", rc)
	}
	rc = cmdInstallSleepHook(context.Background(), nil, []string{"--help"})
	if rc != 0 {
		t.Errorf("--help: expected 0, got %d", rc)
	}
}

func TestInstallSleepHook_UnknownFlag(t *testing.T) {
	rc := cmdInstallSleepHook(context.Background(), nil, []string{"--bogus"})
	if rc != 2 {
		t.Errorf("--bogus: expected exit 2, got %d", rc)
	}
}
