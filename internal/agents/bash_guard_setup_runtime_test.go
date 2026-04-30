package agents

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestSetupBashGuardShim_RuntimeEffectiveness is the runtime-level
// regression that the Pattern P15 wiring-presence test cannot catch.
// It builds a stub force-bash-guard, calls setupBashGuardShim, then
// spawns `bash -c "echo runtime-effectiveness-marker"` with ONLY the
// returned env entries applied (no inherited PATH / SHELL). If the
// shim is reachable end-to-end, the stub will record the command in
// its log file. A regression where setupBashGuardShim drops the SHELL
// entry, or astromech.go stops threading the entries, will trip this.
//
// Hermetic: uses a stubbed force-bash-guard binary (logger that always
// approves) and per-test temp directories. No dependency on
// bin/force-bash-guard having been built.
func TestSetupBashGuardShim_RuntimeEffectiveness(t *testing.T) {
	worktree := t.TempDir()

	// Stub force-bash-guard: logs argv to a known file, always exits 0.
	logFile := filepath.Join(worktree, "stub-guard-invocations.log")
	stubDir := t.TempDir()
	stubBin := filepath.Join(stubDir, BashGuardBinaryName)
	stubSrc := "#!/bin/sh\nfor a in \"$@\"; do printf '%s\\n' \"$a\" >> '" + logFile + "'; done\nexit 0\n"
	if err := os.WriteFile(stubBin, []byte(stubSrc), 0o755); err != nil {
		t.Fatalf("write stub guard: %v", err)
	}

	// Capture pre-test env so we can confirm we don't mutate the parent.
	prevSHELL := os.Getenv("SHELL")
	prevPATH := os.Getenv("PATH")

	// Override the production binary lookup to point at the stub.
	prevEnv := os.Getenv("FORCE_BASH_GUARD_BIN")
	os.Setenv("FORCE_BASH_GUARD_BIN", stubBin)
	t.Cleanup(func() { os.Setenv("FORCE_BASH_GUARD_BIN", prevEnv) })

	envEntries, err := setupBashGuardShim(worktree)
	if err != nil {
		t.Fatalf("setupBashGuardShim: %v", err)
	}
	if len(envEntries) != 2 {
		t.Fatalf("expected 2 env entries (PATH + SHELL), got %d: %v",
			len(envEntries), envEntries)
	}
	var pathEntry, shellEntry string
	for _, e := range envEntries {
		switch {
		case strings.HasPrefix(e, "PATH="):
			pathEntry = e
		case strings.HasPrefix(e, "SHELL="):
			shellEntry = e
		}
	}
	if pathEntry == "" || shellEntry == "" {
		t.Fatalf("missing PATH or SHELL entry; got %v", envEntries)
	}

	// Spawn `bash -c "echo runtime-effectiveness-marker"` via the SHELL
	// path (the actual lever Claude uses). The shim will validate the
	// command against the stub guard — which logs and approves — then
	// exec /bin/bash to actually run echo.
	shellPath := strings.TrimPrefix(shellEntry, "SHELL=")
	if _, err := os.Stat(shellPath); err != nil {
		t.Fatalf("shim at SHELL=%s not present: %v", shellPath, err)
	}

	cmd := exec.Command(shellPath, "-c", "echo runtime-effectiveness-marker")
	// Apply ONLY the env entries the shim returned. We deliberately do
	// NOT inherit the parent process's PATH/SHELL — those would mask a
	// regression where the shim becomes unreachable for the very env
	// entries we're testing.
	cmd.Env = envEntries
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("shim invocation failed: %v\noutput: %s", err, string(out))
	}
	if !strings.Contains(string(out), "runtime-effectiveness-marker") {
		t.Errorf("expected echo output to contain marker; got %q", string(out))
	}

	// Assert the stub guard was actually invoked with the user command.
	guardLog, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatalf("read guard log: %v", err)
	}
	if !strings.Contains(string(guardLog), "runtime-effectiveness-marker") {
		t.Errorf("guard log does not contain the user command — shim was not reached.\nlog: %q",
			string(guardLog))
	}

	// Defense in depth: parent process env must not have been mutated.
	if got := os.Getenv("SHELL"); got != prevSHELL {
		t.Errorf("parent SHELL was mutated: was %q, now %q", prevSHELL, got)
	}
	if got := os.Getenv("PATH"); got != prevPATH {
		t.Errorf("parent PATH was mutated: was %q, now %q", prevPATH, got)
	}
}
