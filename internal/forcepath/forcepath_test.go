package forcepath

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// withCleanEnv isolates the test process's env-var view so tests can set
// FORCE_DIR / FORCE_HOLOCRON_DSN without leaking into sibling tests. The
// dirCached singleton is also reset.
func withCleanEnv(t *testing.T) {
	t.Helper()
	prevDSN := os.Getenv(envHolocronDSN)
	prevDir := os.Getenv(envForceDir)
	t.Cleanup(func() {
		_ = os.Setenv(envHolocronDSN, prevDSN)
		_ = os.Setenv(envForceDir, prevDir)
		ResetDirCacheForTests()
	})
	_ = os.Unsetenv(envHolocronDSN)
	_ = os.Unsetenv(envForceDir)
	ResetDirCacheForTests()
}

func TestForcePath_HolocronDefault(t *testing.T) {
	withCleanEnv(t)
	// Point FORCE_DIR at a temp dir so we don't actually create ~/.force/
	// on the developer machine during `go test`.
	tmp := t.TempDir()
	t.Setenv(envForceDir, tmp)
	ResetDirCacheForTests()

	got := Holocron()
	want := filepath.Join(tmp, "holocron.db") + "?_busy_timeout=5000&_journal_mode=WAL"
	if got != want {
		t.Fatalf("Holocron() = %q; want %q", got, want)
	}
	// Dir() must have run mkdir -p on tmp/.force layout. tmp itself was
	// the override target, so it should exist + be mode 0700.
	fi, err := os.Stat(tmp)
	if err != nil {
		t.Fatalf("Stat(tmp): %v", err)
	}
	if !fi.IsDir() {
		t.Fatalf("expected %s to be a directory", tmp)
	}
}

func TestForcePath_HolocronEnvOverride(t *testing.T) {
	withCleanEnv(t)
	t.Setenv(envHolocronDSN, "file::memory:?cache=shared")
	ResetDirCacheForTests()

	got := Holocron()
	if got != "file::memory:?cache=shared" {
		t.Fatalf("Holocron() = %q; want verbatim env override", got)
	}
	// HolocronFile() must surface "" for in-memory DSNs so callers can
	// short-circuit snapshot / migration paths.
	if HolocronFile() != "" {
		t.Fatalf("HolocronFile() = %q; want \"\" for in-memory DSN", HolocronFile())
	}
}

func TestForcePath_DirEnvOverride(t *testing.T) {
	withCleanEnv(t)
	tmp := t.TempDir()
	override := filepath.Join(tmp, "custom-state")
	t.Setenv(envForceDir, override)
	ResetDirCacheForTests()

	dir := Dir()
	if dir != override {
		t.Fatalf("Dir() = %q; want %q", dir, override)
	}
	holocron := Holocron()
	if !strings.HasPrefix(holocron, override+"/holocron.db") {
		t.Fatalf("Holocron() = %q; want prefix %q", holocron, override+"/holocron.db")
	}
}

func TestForcePath_DirMkdirOnFirstCall(t *testing.T) {
	withCleanEnv(t)
	tmp := t.TempDir()
	override := filepath.Join(tmp, "fresh-dir-that-does-not-exist-yet")
	// Sanity: ensure the dir really doesn't exist.
	if _, err := os.Stat(override); !os.IsNotExist(err) {
		t.Fatalf("expected %s to not exist; stat err=%v", override, err)
	}
	t.Setenv(envForceDir, override)
	ResetDirCacheForTests()

	got := Dir()
	if got != override {
		t.Fatalf("Dir() = %q; want %q", got, override)
	}
	fi, err := os.Stat(override)
	if err != nil {
		t.Fatalf("expected Dir() to mkdir %s; stat err=%v", override, err)
	}
	if !fi.IsDir() {
		t.Fatalf("%s exists but is not a directory", override)
	}
	// Mode must be 0700 (operator-private; trust file lives here).
	if fi.Mode().Perm()&0o077 != 0 {
		t.Fatalf("Dir() mkdir mode = %v; want 0700 (no group/other bits)", fi.Mode().Perm())
	}
}

func TestForcePath_AstromechLog_PerAgent(t *testing.T) {
	withCleanEnv(t)
	tmp := t.TempDir()
	t.Setenv(envForceDir, tmp)
	ResetDirCacheForTests()

	a := AstromechLog("R2-D2")
	b := AstromechLog("Council-Yoda")
	if a == b {
		t.Fatalf("AstromechLog should differ per agent; both returned %q", a)
	}
	if !strings.Contains(a, "R2-D2") {
		t.Fatalf("AstromechLog(R2-D2) = %q; missing agent name", a)
	}
	if !strings.Contains(b, "Council-Yoda") {
		t.Fatalf("AstromechLog(Council-Yoda) = %q; missing agent name", b)
	}
	// Both must live under <Dir>/logs/.
	logsDir := filepath.Join(tmp, "logs")
	if filepath.Dir(a) != logsDir || filepath.Dir(b) != logsDir {
		t.Fatalf("expected both logs under %q; got %q + %q", logsDir, a, b)
	}
	// Subdir must exist after the call.
	if _, err := os.Stat(logsDir); err != nil {
		t.Fatalf("expected logs subdir created; stat err=%v", err)
	}
}

func TestForcePath_AstromechLog_SanitizesPathSeparators(t *testing.T) {
	withCleanEnv(t)
	tmp := t.TempDir()
	t.Setenv(envForceDir, tmp)
	ResetDirCacheForTests()

	// A hostile agent name with a path separator must not escape the
	// logs/ subdirectory. sanitizeAgent replaces / with _.
	got := AstromechLog("../evil")
	if !strings.HasPrefix(got, filepath.Join(tmp, "logs")) {
		t.Fatalf("AstromechLog escaped logs dir: %q", got)
	}
	if strings.Contains(filepath.Base(got), "/") {
		t.Fatalf("AstromechLog leaked path separator: %q", got)
	}
}

func TestForcePath_ScratchTaskFile_PerTask(t *testing.T) {
	withCleanEnv(t)
	tmp := t.TempDir()
	t.Setenv(envForceDir, tmp)
	ResetDirCacheForTests()

	a := ScratchTaskFile(7)
	b := ScratchTaskFile(42)
	if a == b {
		t.Fatalf("ScratchTaskFile should differ per task; both = %q", a)
	}
	scratchDir := filepath.Join(tmp, "scratch")
	if filepath.Dir(a) != scratchDir {
		t.Fatalf("ScratchTaskFile not under scratch/: %q", a)
	}
	if !strings.Contains(a, "fleet-task-7.log") {
		t.Fatalf("ScratchTaskFile(7) = %q; want fleet-task-7.log", a)
	}
	if _, err := os.Stat(scratchDir); err != nil {
		t.Fatalf("expected scratch subdir created; stat err=%v", err)
	}
}

func TestForcePath_PIDFleetLogHolonet_StableUnderDir(t *testing.T) {
	withCleanEnv(t)
	tmp := t.TempDir()
	t.Setenv(envForceDir, tmp)
	ResetDirCacheForTests()

	cases := []struct {
		name string
		got  string
		want string
	}{
		{"PIDFile", PIDFile(), filepath.Join(tmp, "force.pid")},
		{"FleetLog", FleetLog(), filepath.Join(tmp, "fleet.log")},
		{"HolonetEventStream", HolonetEventStream(), filepath.Join(tmp, "holonet.jsonl")},
		{"HolocronFile", HolocronFile(), filepath.Join(tmp, "holocron.db")},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("%s = %q; want %q", c.name, c.got, c.want)
		}
	}
}

func TestForcePath_HolocronFile_StripsQueryFromCustomDSN(t *testing.T) {
	withCleanEnv(t)
	t.Setenv(envHolocronDSN, "/custom/path/foo.db?_busy_timeout=1234")
	ResetDirCacheForTests()

	got := HolocronFile()
	want := "/custom/path/foo.db"
	if got != want {
		t.Fatalf("HolocronFile() = %q; want %q (query stripped)", got, want)
	}
}

// ── migration tests ─────────────────────────────────────────────────────────

func TestMigrateLegacyHolocronDB_CanonicalMissing_LegacyExists_MovesIt(t *testing.T) {
	withCleanEnv(t)
	tmp := t.TempDir()
	cwd := filepath.Join(tmp, "old-cwd")
	stateDir := filepath.Join(tmp, "force-state")
	if err := os.MkdirAll(cwd, 0o755); err != nil {
		t.Fatalf("mkdir cwd: %v", err)
	}
	t.Setenv(envForceDir, stateDir)
	ResetDirCacheForTests()

	// Create a non-empty legacy holocron.db in the candidate dir.
	legacy := filepath.Join(cwd, "holocron.db")
	if err := os.WriteFile(legacy, []byte("legacy-data"), 0o600); err != nil {
		t.Fatalf("write legacy: %v", err)
	}
	// Plus an -shm sidecar so we exercise the sidecar move path.
	if err := os.WriteFile(legacy+"-shm", []byte("shm"), 0o600); err != nil {
		t.Fatalf("write shm: %v", err)
	}

	if err := MigrateLegacyHolocronDB(context.Background(), nil, cwd); err != nil {
		t.Fatalf("MigrateLegacyHolocronDB: %v", err)
	}
	// Canonical must now exist with the legacy contents.
	canonical := filepath.Join(stateDir, "holocron.db")
	data, err := os.ReadFile(canonical)
	if err != nil {
		t.Fatalf("read canonical: %v", err)
	}
	if string(data) != "legacy-data" {
		t.Fatalf("canonical contents = %q; want %q", string(data), "legacy-data")
	}
	// Sidecar must have moved too.
	if _, err := os.Stat(canonical + "-shm"); err != nil {
		t.Fatalf("canonical -shm missing post-migration: %v", err)
	}
	// Legacy must be gone.
	if _, err := os.Stat(legacy); !os.IsNotExist(err) {
		t.Fatalf("legacy still present post-migration: err=%v", err)
	}
	if _, err := os.Stat(legacy + "-shm"); !os.IsNotExist(err) {
		t.Fatalf("legacy -shm still present post-migration: err=%v", err)
	}
}

func TestMigrateLegacyHolocronDB_BothExist_DataInBoth_Errors(t *testing.T) {
	withCleanEnv(t)
	tmp := t.TempDir()
	cwd := filepath.Join(tmp, "old-cwd")
	stateDir := filepath.Join(tmp, "force-state")
	for _, d := range []string{cwd, stateDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}
	t.Setenv(envForceDir, stateDir)
	ResetDirCacheForTests()

	// Both files have data.
	legacy := filepath.Join(cwd, "holocron.db")
	canonical := filepath.Join(stateDir, "holocron.db")
	if err := os.WriteFile(legacy, []byte("legacy-data-1234"), 0o600); err != nil {
		t.Fatalf("write legacy: %v", err)
	}
	if err := os.WriteFile(canonical, []byte("canonical-data-5678"), 0o600); err != nil {
		t.Fatalf("write canonical: %v", err)
	}

	err := MigrateLegacyHolocronDB(context.Background(), nil, cwd)
	if err == nil {
		t.Fatal("expected AMBIGUOUS error; got nil")
	}
	if !strings.Contains(err.Error(), "AMBIGUOUS") {
		t.Fatalf("expected AMBIGUOUS in error; got %v", err)
	}
	// BOTH files must be untouched on the ambiguous error path.
	if data, _ := os.ReadFile(legacy); string(data) != "legacy-data-1234" {
		t.Fatalf("legacy was modified: %q", string(data))
	}
	if data, _ := os.ReadFile(canonical); string(data) != "canonical-data-5678" {
		t.Fatalf("canonical was modified: %q", string(data))
	}
}

func TestMigrateLegacyHolocronDB_CanonicalExists_NoOp(t *testing.T) {
	withCleanEnv(t)
	tmp := t.TempDir()
	cwd := filepath.Join(tmp, "old-cwd")
	stateDir := filepath.Join(tmp, "force-state")
	for _, d := range []string{cwd, stateDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}
	t.Setenv(envForceDir, stateDir)
	ResetDirCacheForTests()

	canonical := filepath.Join(stateDir, "holocron.db")
	if err := os.WriteFile(canonical, []byte("canonical"), 0o600); err != nil {
		t.Fatalf("write canonical: %v", err)
	}
	// No legacy file.

	if err := MigrateLegacyHolocronDB(context.Background(), nil, cwd); err != nil {
		t.Fatalf("MigrateLegacyHolocronDB: %v", err)
	}
	if data, _ := os.ReadFile(canonical); string(data) != "canonical" {
		t.Fatalf("canonical was modified: %q", string(data))
	}
}

func TestMigrateLegacyHolocronDB_NeitherExists_NoOp(t *testing.T) {
	withCleanEnv(t)
	tmp := t.TempDir()
	cwd := filepath.Join(tmp, "old-cwd")
	stateDir := filepath.Join(tmp, "force-state")
	for _, d := range []string{cwd, stateDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}
	t.Setenv(envForceDir, stateDir)
	ResetDirCacheForTests()

	if err := MigrateLegacyHolocronDB(context.Background(), nil, cwd); err != nil {
		t.Fatalf("MigrateLegacyHolocronDB: %v", err)
	}
	// Canonical must NOT have been created (no work to do).
	if _, err := os.Stat(filepath.Join(stateDir, "holocron.db")); !os.IsNotExist(err) {
		t.Fatalf("canonical unexpectedly created: err=%v", err)
	}
}

func TestMigrateLegacyHolocronDB_InMemoryDSN_NoOp(t *testing.T) {
	withCleanEnv(t)
	t.Setenv(envHolocronDSN, ":memory:")
	ResetDirCacheForTests()

	// Even with a legacy file in CWD, an in-memory DSN must skip cleanly
	// (there's no filesystem to migrate to).
	tmp := t.TempDir()
	if err := os.WriteFile(filepath.Join(tmp, "holocron.db"), []byte("data"), 0o600); err != nil {
		t.Fatalf("write legacy: %v", err)
	}
	if err := MigrateLegacyHolocronDB(context.Background(), nil, tmp); err != nil {
		t.Fatalf("MigrateLegacyHolocronDB with in-memory DSN: %v", err)
	}
	// Legacy must still be there — we didn't touch it.
	if _, err := os.Stat(filepath.Join(tmp, "holocron.db")); err != nil {
		t.Fatalf("legacy unexpectedly removed: %v", err)
	}
}

func TestMigrateLegacyHolocronDB_EmptyCanonical_RemovedThenMigrated(t *testing.T) {
	withCleanEnv(t)
	tmp := t.TempDir()
	cwd := filepath.Join(tmp, "old-cwd")
	stateDir := filepath.Join(tmp, "force-state")
	for _, d := range []string{cwd, stateDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}
	t.Setenv(envForceDir, stateDir)
	ResetDirCacheForTests()

	canonical := filepath.Join(stateDir, "holocron.db")
	if err := os.WriteFile(canonical, []byte{}, 0o600); err != nil {
		t.Fatalf("write empty canonical: %v", err)
	}
	legacy := filepath.Join(cwd, "holocron.db")
	if err := os.WriteFile(legacy, []byte("legacy-data"), 0o600); err != nil {
		t.Fatalf("write legacy: %v", err)
	}

	if err := MigrateLegacyHolocronDB(context.Background(), nil, cwd); err != nil {
		t.Fatalf("MigrateLegacyHolocronDB: %v", err)
	}
	if data, _ := os.ReadFile(canonical); string(data) != "legacy-data" {
		t.Fatalf("canonical contents = %q; want legacy-data", string(data))
	}
}

func TestMigrateLegacyHolocronDB_SkipsCanonicalDirAsCandidate(t *testing.T) {
	// When the operator's CWD IS the canonical dir (an early-adopter or
	// a test that runs with FORCE_DIR pointing at the working tree), we
	// must NOT try to "migrate" the canonical onto itself.
	withCleanEnv(t)
	tmp := t.TempDir()
	t.Setenv(envForceDir, tmp)
	ResetDirCacheForTests()

	canonical := filepath.Join(tmp, "holocron.db")
	if err := os.WriteFile(canonical, []byte("here"), 0o600); err != nil {
		t.Fatalf("write canonical: %v", err)
	}
	// CWD = same as state dir.
	if err := MigrateLegacyHolocronDB(context.Background(), nil, tmp); err != nil {
		t.Fatalf("MigrateLegacyHolocronDB: %v", err)
	}
	// Canonical unchanged.
	if data, _ := os.ReadFile(canonical); string(data) != "here" {
		t.Fatalf("canonical was modified: %q", string(data))
	}
}
