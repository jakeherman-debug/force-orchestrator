package main

// add_repo_smart_defaults_e2e_test.go — behavioral subprocess tests for
// Sweep D's smart-default + batch-import work. These exercise the full
// CLI surface end-to-end against a fresh-built `force` binary, which is
// the same boundary the operator hits.
//
// The in-process unit tests in add_repo_smart_defaults_test.go cover the
// derivation helpers in isolation; this file proves that
// `force add-repo <path>` and `force add-repos <dir>` actually wire those
// helpers into the CLI dispatcher and write the right rows to
// holocron.db.

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"force-orchestrator/internal/store"
)

// buildForceBinary builds the `force` CLI into a fresh temp dir and
// returns the path. Shared across the e2e tests below so each test pays
// the build cost only once per `go test` invocation if `t.TempDir()` is
// reused — but in practice `go test` runs each test with its own dir, so
// we accept the rebuild for hermeticity.
func buildForceBinary(t *testing.T) string {
	t.Helper()
	if testing.Short() {
		t.Skip("subprocess build is too slow for -short")
	}
	binPath := filepath.Join(t.TempDir(), "force")
	build := exec.Command("go", "build", "-tags", "sqlite_fts5", "-o", binPath, ".")
	build.Stderr = os.Stderr
	if out, err := build.Output(); err != nil {
		t.Fatalf("go build force: %v\n%s", err, out)
	}
	return binPath
}

// makeStubGitRepo creates a real git repo at `path` with a README whose
// first paragraph is `desc`. Returns nothing — fatals on error.
func makeStubGitRepo(t *testing.T, path, desc string) {
	t.Helper()
	if err := os.MkdirAll(path, 0755); err != nil {
		t.Fatal(err)
	}
	if err := exec.Command("git", "init", "-q", "-b", "main", path).Run(); err != nil {
		t.Skipf("git unavailable: %v", err)
	}
	exec.Command("git", "-C", path, "config", "user.email", "t@t").Run()
	exec.Command("git", "-C", path, "config", "user.name", "Test").Run()
	exec.Command("git", "-C", path, "config", "commit.gpgsign", "false").Run()
	readme := "# " + filepath.Base(path) + "\n\n" + desc + "\n"
	if err := os.WriteFile(filepath.Join(path, "README.md"), []byte(readme), 0644); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("git", "-C", path, "add", ".").CombinedOutput(); err != nil {
		t.Fatalf("git add: %v — %s", err, out)
	}
	if out, err := exec.Command("git", "-C", path, "commit", "-q", "-m", "init").CombinedOutput(); err != nil {
		t.Fatalf("git commit: %v — %s", err, out)
	}
}

// runForceCmd runs the binary with workdir `cwd` (so holocron.db lands
// there) and returns stdout/stderr/err.
//
// Sweep-E (D12): the subprocess inherits LIVE_HAIKU_DISABLED=1 so the
// add-repo description-derivation path uses the regex scrape (not a
// real Claude call). Pre-Sweep-E the regex was the only path and the
// env var didn't matter; post-Sweep-E without this set, the E2E tests
// would attempt a real Claude call from inside the test binary and
// either hang (no network) or charge an LLM token. Tests that want
// to exercise the LLM path stub the runner in-process.
func runForceCmd(t *testing.T, bin, cwd string, stdin string, args ...string) (string, string, error) {
	t.Helper()
	cmd := exec.Command(bin, args...)
	cmd.Dir = cwd
	cmd.Env = append(os.Environ(), "LIVE_HAIKU_DISABLED=1")
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	return stdout.String(), stderr.String(), err
}

// TestE2E_AddRepo_SmartDefaults verifies `force add-repo <path>` (no
// flags, no other args) registers the repo with derived name +
// description.
func TestE2E_AddRepo_SmartDefaults(t *testing.T) {
	bin := buildForceBinary(t)

	workdir := t.TempDir()
	repoDir := filepath.Join(t.TempDir(), "smart-default-repo")
	makeStubGitRepo(t, repoDir, "Smart-default test repo description.")

	stdout, stderr, err := runForceCmd(t, bin, workdir, "", "add-repo", repoDir)
	if err != nil {
		t.Fatalf("force add-repo failed: %v\nstdout: %s\nstderr: %s", err, stdout, stderr)
	}
	// Open the holocron.db that was created in workdir to verify the row.
	db := store.InitHolocronDSN(filepath.Join(workdir, "holocron.db"))
	defer db.Close()
	repo := store.GetRepo(db, "smart-default-repo")
	if repo == nil {
		t.Fatalf("repo not registered (stdout: %s)", stdout)
	}
	if repo.LocalPath != repoDir {
		t.Errorf("local_path = %q, want %q", repo.LocalPath, repoDir)
	}
	if !strings.Contains(repo.Description, "Smart-default test repo description.") {
		t.Errorf("description not auto-derived from README: %q", repo.Description)
	}
}

// TestE2E_AddRepo_NameOverride verifies --name overrides the derived name.
func TestE2E_AddRepo_NameOverride(t *testing.T) {
	bin := buildForceBinary(t)
	workdir := t.TempDir()
	repoDir := filepath.Join(t.TempDir(), "ignored-basename")
	makeStubGitRepo(t, repoDir, "desc")

	stdout, stderr, err := runForceCmd(t, bin, workdir, "",
		"add-repo", repoDir, "--name", "explicit-name")
	if err != nil {
		t.Fatalf("force add-repo failed: %v\nstdout: %s\nstderr: %s", err, stdout, stderr)
	}
	db := store.InitHolocronDSN(filepath.Join(workdir, "holocron.db"))
	defer db.Close()
	if r := store.GetRepo(db, "explicit-name"); r == nil {
		t.Errorf("--name override didn't take — no row at 'explicit-name' (stdout: %s)", stdout)
	}
	if r := store.GetRepo(db, "ignored-basename"); r != nil {
		t.Errorf("basename row also present — override didn't suppress derivation")
	}
}

// TestE2E_AddRepo_DescriptionOverride verifies --description overrides
// the derived description.
func TestE2E_AddRepo_DescriptionOverride(t *testing.T) {
	bin := buildForceBinary(t)
	workdir := t.TempDir()
	repoDir := filepath.Join(t.TempDir(), "desc-override")
	makeStubGitRepo(t, repoDir, "Auto-derived description from README.")

	stdout, stderr, err := runForceCmd(t, bin, workdir, "",
		"add-repo", repoDir, "--description", "Custom operator-supplied description.")
	if err != nil {
		t.Fatalf("force add-repo failed: %v\nstdout: %s\nstderr: %s", err, stdout, stderr)
	}
	db := store.InitHolocronDSN(filepath.Join(workdir, "holocron.db"))
	defer db.Close()
	repo := store.GetRepo(db, "desc-override")
	if repo == nil {
		t.Fatalf("repo not registered")
	}
	if repo.Description != "Custom operator-supplied description." {
		t.Errorf("description = %q, want explicit override", repo.Description)
	}
}

// TestE2E_AddRepo_LegacyPositional verifies the 3-positional form still
// works (operator muscle memory + scripted use).
func TestE2E_AddRepo_LegacyPositional(t *testing.T) {
	bin := buildForceBinary(t)
	workdir := t.TempDir()
	repoDir := filepath.Join(t.TempDir(), "legacy-form-repo")
	makeStubGitRepo(t, repoDir, "ignored — legacy mode supplies its own desc")

	stdout, stderr, err := runForceCmd(t, bin, workdir, "",
		"add-repo", "myname", repoDir, "my-description")
	if err != nil {
		t.Fatalf("legacy add-repo failed: %v\nstdout: %s\nstderr: %s", err, stdout, stderr)
	}
	db := store.InitHolocronDSN(filepath.Join(workdir, "holocron.db"))
	defer db.Close()
	repo := store.GetRepo(db, "myname")
	if repo == nil {
		t.Fatalf("legacy 3-positional didn't register row 'myname' (stdout: %s)", stdout)
	}
	if repo.Description != "my-description" {
		t.Errorf("description = %q, want %q (legacy form must NOT auto-derive)", repo.Description, "my-description")
	}
}

// TestE2E_AddRepos_BatchImport_AssumeYes verifies the batch command
// enumerates + adds every git repo under <dir>, skipping non-git dirs,
// and is idempotent on a re-run.
func TestE2E_AddRepos_BatchImport_AssumeYes(t *testing.T) {
	bin := buildForceBinary(t)
	workdir := t.TempDir()

	// Parent dir holding 3 git repos + 1 non-git dir. The non-git dir
	// must be skipped silently; the git ones should all be picked up.
	parent := t.TempDir()
	repos := []string{"alpha", "beta", "gamma"}
	for _, name := range repos {
		makeStubGitRepo(t, filepath.Join(parent, name), "Description for "+name+".")
	}
	if err := os.MkdirAll(filepath.Join(parent, "not-a-repo"), 0755); err != nil {
		t.Fatal(err)
	}

	stdout, stderr, err := runForceCmd(t, bin, workdir, "",
		"add-repos", parent, "--assume-yes")
	if err != nil {
		t.Fatalf("force add-repos failed: %v\nstdout: %s\nstderr: %s", err, stdout, stderr)
	}
	db := store.InitHolocronDSN(filepath.Join(workdir, "holocron.db"))
	defer db.Close()
	for _, name := range repos {
		r := store.GetRepo(db, name)
		if r == nil {
			t.Errorf("repo %q not registered after batch (stdout: %s)", name, stdout)
			continue
		}
		if !strings.Contains(r.Description, name) {
			t.Errorf("repo %q description %q does not look auto-derived", name, r.Description)
		}
	}
	if r := store.GetRepo(db, "not-a-repo"); r != nil {
		t.Errorf("non-git dir was registered: %+v", r)
	}
	db.Close()

	// Re-run — must be a no-op (skipped: already registered).
	stdout2, stderr2, err := runForceCmd(t, bin, workdir, "",
		"add-repos", parent, "--assume-yes")
	if err != nil {
		t.Fatalf("re-run failed: %v\nstdout: %s\nstderr: %s", err, stdout2, stderr2)
	}
	if !strings.Contains(stdout2, "already registered") {
		t.Errorf("re-run must mark all repos 'already registered', stdout: %s", stdout2)
	}
}

// TestE2E_AddRepos_BatchImport_NoConfirmAborts verifies that omitting
// --assume-yes and answering "no" (or anything not "yes") aborts without
// writing.
func TestE2E_AddRepos_BatchImport_NoConfirmAborts(t *testing.T) {
	bin := buildForceBinary(t)
	workdir := t.TempDir()
	parent := t.TempDir()
	makeStubGitRepo(t, filepath.Join(parent, "abort-repo"), "should not land")

	// Pipe "no\n" to stdin to decline the prompt.
	stdout, stderr, err := runForceCmd(t, bin, workdir, "no\n",
		"add-repos", parent)
	if err != nil {
		t.Fatalf("force add-repos failed: %v\nstdout: %s\nstderr: %s", err, stdout, stderr)
	}
	if !strings.Contains(stdout, "Aborted") {
		t.Errorf("decline path must print 'Aborted', got: %s", stdout)
	}
	db := store.InitHolocronDSN(filepath.Join(workdir, "holocron.db"))
	defer db.Close()
	if r := store.GetRepo(db, "abort-repo"); r != nil {
		t.Errorf("abort path must not register row, got: %+v", r)
	}
}
