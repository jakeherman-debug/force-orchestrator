package agents

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// stubBashGuardBinary writes a fake force-bash-guard binary into a
// temp dir and overrides FORCE_BASH_GUARD_BIN to point at it. Returns
// the path; the test cleanup restores the prior env.
func stubBashGuardBinary(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, BashGuardBinaryName)
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("stub bin: %v", err)
	}
	old := os.Getenv("FORCE_BASH_GUARD_BIN")
	os.Setenv("FORCE_BASH_GUARD_BIN", bin)
	t.Cleanup(func() { os.Setenv("FORCE_BASH_GUARD_BIN", old) })
	return bin
}

func TestSetupBashGuardShim_WritesExecutableShim(t *testing.T) {
	stubBashGuardBinary(t)
	worktree := t.TempDir()

	envEntries, err := setupBashGuardShim(worktree)
	if err != nil {
		t.Fatalf("setupBashGuardShim: %v", err)
	}
	if len(envEntries) != 2 {
		t.Fatalf("expected 2 env entries (PATH + SHELL), got %d: %v", len(envEntries), envEntries)
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
	if pathEntry == "" {
		t.Errorf("missing PATH= entry in %v", envEntries)
	}
	if shellEntry == "" {
		t.Errorf("missing SHELL= entry in %v", envEntries)
	}
	if !strings.Contains(pathEntry, bashGuardShimDirName) {
		t.Errorf("PATH entry %q does not contain shim dir name", pathEntry)
	}
	if !strings.Contains(shellEntry, bashGuardShimDirName) {
		t.Errorf("SHELL entry %q does not contain shim dir name", shellEntry)
	}
	if !strings.HasSuffix(shellEntry, "/bash") {
		t.Errorf("SHELL entry %q does not point at the shim's bash file", shellEntry)
	}

	shim := filepath.Join(worktree, bashGuardShimDirName, "bash")
	st, err := os.Stat(shim)
	if err != nil {
		t.Fatalf("shim stat: %v", err)
	}
	if st.Mode()&0o111 == 0 {
		t.Errorf("shim mode = %v, expected executable bits", st.Mode())
	}
	contents, err := os.ReadFile(shim)
	if err != nil {
		t.Fatalf("read shim: %v", err)
	}
	if !strings.Contains(string(contents), "force-bash-guard") {
		t.Errorf("shim does not reference the gatekeeper binary; got: %q", contents)
	}
	if !strings.Contains(string(contents), "exec /bin/bash") {
		t.Errorf("shim missing exec fallthrough to /bin/bash; got: %q", contents)
	}
}

func TestSetupBashGuardShim_IsIdempotent(t *testing.T) {
	stubBashGuardBinary(t)
	worktree := t.TempDir()

	first, err := setupBashGuardShim(worktree)
	if err != nil {
		t.Fatalf("first setup: %v", err)
	}
	second, err := setupBashGuardShim(worktree)
	if err != nil {
		t.Fatalf("second setup: %v", err)
	}
	if len(first) != len(second) {
		t.Fatalf("env entry count changed across calls: %d vs %d", len(first), len(second))
	}
	for i := range first {
		if first[i] != second[i] {
			t.Errorf("env entry %d changed across calls: %q vs %q", i, first[i], second[i])
		}
	}
}

func TestSetupBashGuardShim_FailsCleanlyWithoutBinary(t *testing.T) {
	// Override env to point at a missing file AND empty PATH.
	old := os.Getenv("FORCE_BASH_GUARD_BIN")
	os.Setenv("FORCE_BASH_GUARD_BIN", "")
	t.Cleanup(func() { os.Setenv("FORCE_BASH_GUARD_BIN", old) })

	// Stub findOnPath / lookPath so the binary is also "not found".
	origLook := lookPath
	lookPath = func(name string) (string, error) {
		return "", os.ErrNotExist
	}
	t.Cleanup(func() { lookPath = origLook })

	// Also need to ensure the cwd/bin/force-bash-guard doesn't exist.
	// We can't change cwd reliably; instead, we accept that some test
	// hosts may have ./bin/force-bash-guard present (the
	// build-bash-guard target produced it). If it does, this test is
	// a no-op — the binary IS findable. We assert only that an empty
	// override + missing PATH lookup yields a non-nil error when no
	// fallback hits.
	if _, err := os.Stat(filepath.Join(".", "bin", BashGuardBinaryName)); err == nil {
		t.Skip("local ./bin/force-bash-guard present; cannot exercise the not-found path")
	}

	if _, err := setupBashGuardShim(t.TempDir()); err == nil {
		t.Errorf("expected an error when binary cannot be located, got nil")
	}
}

func TestBashShimSource_ContainsValidationPath(t *testing.T) {
	src := bashShimSource("/opt/force/bin/force-bash-guard")
	mustContain := []string{
		`'/opt/force/bin/force-bash-guard'`,
		"if [ \"$1\" = \"-c\" ]",
		"exec /bin/bash",
	}
	for _, want := range mustContain {
		if !strings.Contains(src, want) {
			t.Errorf("shim source missing %q; full source:\n%s", want, src)
		}
	}
}

// TestBashGuardWiringInAstromech is the per-package integration test
// asserting that astromech.go references both the helper
// (`setupBashGuardShim`) AND the bash-guard binary by name. This is
// belt-and-suspenders alongside Pattern P15 in audittools — if the
// audittools test ever stops being run, the regression still catches
// here.
func TestBashGuardWiringInAstromech(t *testing.T) {
	src, err := os.ReadFile("astromech.go")
	if err != nil {
		t.Fatalf("read astromech.go: %v", err)
	}
	body := string(src)
	for _, want := range []string{
		"setupBashGuardShim",
		"force-bash-guard",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("astromech.go missing reference to %q (bash-guard wiring missing)", want)
		}
	}
}
