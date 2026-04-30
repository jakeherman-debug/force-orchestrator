package agents

import (
	"os"
	"os/exec"
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
		// Argv-walking parser, not the old single-position match.
		`for arg in "$@"`,
		// -c terminator triggers saw_c.
		`-c)`,
		// Combined flag {l,i,c} pattern check.
		`*[!lic]*`,
		// Guard call shape preserved.
		`"$GUARD" "$CMD"`,
		"exec /bin/bash",
	}
	for _, want := range mustContain {
		if !strings.Contains(src, want) {
			t.Errorf("shim source missing %q; full source:\n%s", want, src)
		}
	}
}

// writeStubGuardLogger writes a stub force-bash-guard binary that
// appends its argv (one arg per line) to logFile and exits 0. Returns
// the binary path. Used by the parser-shape tests below to verify the
// shim is feeding the correct command portion to the guard.
func writeStubGuardLogger(t *testing.T, dir, logFile string) string {
	t.Helper()
	bin := filepath.Join(dir, "stub-bash-guard")
	src := "#!/bin/sh\nfor a in \"$@\"; do printf '%s\\n' \"$a\" >> '" + logFile + "'; done\nexit 0\n"
	if err := os.WriteFile(bin, []byte(src), 0o755); err != nil {
		t.Fatalf("write stub guard: %v", err)
	}
	return bin
}

// runShim spawns the per-worktree bash shim with the given argv and
// returns the exit code (or -1 on spawn failure) and stderr output.
func runShim(t *testing.T, shim string, argv ...string) (int, string) {
	t.Helper()
	cmd := exec.Command(shim, argv...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return ee.ExitCode(), string(out)
		}
		return -1, string(out) + " err=" + err.Error()
	}
	return 0, string(out)
}

// TestBashShim_RecognizesArgvShapes drives the per-worktree shim with
// every flag combination the prompt called out + the legacy ones, and
// asserts the stub guard binary saw the *command* portion (not a flag)
// as its argv[1]. A regression where the parser falls through on
// `-c -l <cmd>` (Claude's snapshot bootstrap) trips this test.
func TestBashShim_RecognizesArgvShapes(t *testing.T) {
	worktree := t.TempDir()
	logFile := filepath.Join(worktree, "guard-argv.log")
	stubGuard := writeStubGuardLogger(t, worktree, logFile)

	// Generate the shim with the stub guard wired in.
	shimDir := filepath.Join(worktree, bashGuardShimDirName)
	if err := os.MkdirAll(shimDir, 0o755); err != nil {
		t.Fatalf("mkdir shim dir: %v", err)
	}
	shim := filepath.Join(shimDir, "bash")
	if err := os.WriteFile(shim, []byte(bashShimSource(stubGuard)), 0o755); err != nil {
		t.Fatalf("write shim: %v", err)
	}

	cases := []struct {
		name    string
		argv    []string
		wantCmd string // expected command string fed to the guard
	}{
		{"plain -c", []string{"-c", "echo hi"}, "echo hi"},
		{"combined -lc", []string{"-lc", "echo hi"}, "echo hi"},
		{"-c then -l (Claude snapshot bootstrap)", []string{"-c", "-l", "echo hi"}, "echo hi"},
		{"-l then -c", []string{"-l", "-c", "echo hi"}, "echo hi"},
		{"-i then -c", []string{"-i", "-c", "echo hi"}, "echo hi"},
		{"-li then -c", []string{"-li", "-c", "echo hi"}, "echo hi"},
		{"combined -ilc", []string{"-ilc", "echo hi"}, "echo hi"},
		{"-c then -li (flags after -c)", []string{"-c", "-li", "echo hi"}, "echo hi"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := os.WriteFile(logFile, nil, 0o644); err != nil {
				t.Fatalf("reset log: %v", err)
			}
			// We don't care if the trailing exec /bin/bash succeeds —
			// the guard log is the assertion target.
			runShim(t, shim, tc.argv...)
			got, err := os.ReadFile(logFile)
			if err != nil {
				t.Fatalf("read guard log: %v", err)
			}
			lines := strings.Split(strings.TrimRight(string(got), "\n"), "\n")
			if len(lines) == 0 || lines[0] == "" {
				t.Fatalf("guard never invoked; argv=%v", tc.argv)
			}
			// The stub guard records each argv as a line. Argv[1] of the
			// guard is the command (since stub iterates "$@").
			if lines[0] != tc.wantCmd {
				t.Errorf("argv=%v: guard saw %q, want %q\nfull log: %q",
					tc.argv, lines[0], tc.wantCmd, string(got))
			}
		})
	}
}

// TestBashShim_FallsThroughOnNonCommandShapes ensures we don't validate
// (and therefore don't reject) shapes that don't carry a -c <cmd>. The
// guard log must be empty after invoking these.
func TestBashShim_FallsThroughOnNonCommandShapes(t *testing.T) {
	worktree := t.TempDir()
	logFile := filepath.Join(worktree, "guard-argv.log")
	stubGuard := writeStubGuardLogger(t, worktree, logFile)

	shimDir := filepath.Join(worktree, bashGuardShimDirName)
	if err := os.MkdirAll(shimDir, 0o755); err != nil {
		t.Fatalf("mkdir shim dir: %v", err)
	}
	shim := filepath.Join(shimDir, "bash")
	if err := os.WriteFile(shim, []byte(bashShimSource(stubGuard)), 0o755); err != nil {
		t.Fatalf("write shim: %v", err)
	}

	cases := []struct {
		name string
		argv []string
	}{
		// The trailing /dev/null script keeps bash from hanging on stdin
		// during these "fall-through" cases.
		{"bash <script.sh>", []string{"/dev/null"}},
		{"bash -l <script.sh>", []string{"-l", "/dev/null"}},
		{"bash -i <script.sh>", []string{"-i", "/dev/null"}},
		{"bash --norc -c <cmd> (unrecognized flag — conservative fall-through)",
			[]string{"--norc", "-c", "echo hi"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := os.WriteFile(logFile, nil, 0o644); err != nil {
				t.Fatalf("reset log: %v", err)
			}
			runShim(t, shim, tc.argv...)
			got, _ := os.ReadFile(logFile)
			if len(strings.TrimSpace(string(got))) != 0 {
				t.Errorf("guard was invoked unexpectedly for argv=%v; log=%q",
					tc.argv, string(got))
			}
		})
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
