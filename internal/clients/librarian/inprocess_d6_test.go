package librarian

// inprocess_d6_test.go — D6 BuildRepoDigest tests.
//
// Coverage:
//
//   - happy path against a registered repo (RecentCommits + README +
//     TopLevelDirs populated; conventions + fragility paths exercised)
//   - happy path against an on-disk-only path (RepoName empty; the
//     unregistered-repo git path runs through igit.LogAndRun)
//   - failure mode: empty repo spec
//   - failure mode: unregistered name AND non-existent path
//   - idempotence: two consecutive calls return digest with the same
//     non-time fields

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"force-orchestrator/internal/store"
)

func TestBuildRepoDigest_RegisteredRepo(t *testing.T) {
	t.Setenv("LIVE_HAIKU_DISABLED", "1")

	repoDir := t.TempDir()
	mustRunGit(t, repoDir, "init", "-q")
	mustRunGit(t, repoDir, "config", "user.email", "test@example.com")
	mustRunGit(t, repoDir, "config", "user.name", "Test")
	mustRunGit(t, repoDir, "config", "commit.gpgsign", "false")

	if err := os.WriteFile(filepath.Join(repoDir, "README.md"),
		[]byte("# Sample Repo\n\nThis is a one-line description.\n"), 0644); err != nil {
		t.Fatalf("write README: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(repoDir, "internal"), 0755); err != nil {
		t.Fatalf("mkdir internal: %v", err)
	}
	if err := os.WriteFile(filepath.Join(repoDir, "internal", "x.go"),
		[]byte("package internal\n\ntype Greeter interface {\n\tGreet() string\n}\n"), 0644); err != nil {
		t.Fatalf("write internal/x.go: %v", err)
	}
	if err := os.WriteFile(filepath.Join(repoDir, "CLAUDE.md"),
		[]byte("# CLAUDE\nbe careful\n"), 0644); err != nil {
		t.Fatalf("write CLAUDE.md: %v", err)
	}
	mustRunGit(t, repoDir, "add", ".")
	mustRunGit(t, repoDir, "commit", "-q", "-m", "seed commit")

	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	store.AddRepo(db, "d6-fixture", repoDir, "fixture for D6 BuildRepoDigest")

	c := NewInProcess(db).(*inProcessClient)
	digest, err := c.BuildRepoDigest(context.Background(), "d6-fixture")
	if err != nil {
		t.Fatalf("BuildRepoDigest: %v", err)
	}
	if digest.RepoName != "d6-fixture" {
		t.Errorf("expected RepoName=d6-fixture, got %q", digest.RepoName)
	}
	if digest.LocalPath != repoDir {
		t.Errorf("expected LocalPath=%s, got %s", repoDir, digest.LocalPath)
	}
	if digest.Description == "" {
		t.Errorf("expected Description populated; AddRepo passed a description")
	}
	if digest.READMESample == "" || !contains(digest.READMESample, "Sample Repo") {
		t.Errorf("expected README sample to mention 'Sample Repo'; got %q", digest.READMESample)
	}
	if !containsString(digest.TopLevelDirs, "internal") {
		t.Errorf("expected TopLevelDirs to include 'internal'; got %v", digest.TopLevelDirs)
	}
	// Public API surface: the Greeter interface should be detected.
	foundGreeter := false
	for _, sym := range digest.PublicAPISymbols {
		if sym.Kind == "interface" && sym.Name == "Greeter" {
			foundGreeter = true
			break
		}
	}
	if !foundGreeter {
		t.Errorf("expected Greeter interface to be detected; symbols=%v", digest.PublicAPISymbols)
	}
	if len(digest.RecentCommits.Commits) == 0 {
		t.Errorf("expected RecentCommits to be populated for a fresh repo")
	}
	if got, ok := digest.Conventions["CLAUDE.md"]; !ok || got == "" {
		t.Errorf("expected CLAUDE.md convention populated; got %q (ok=%v)", got, ok)
	}
	if got, ok := digest.Conventions["CONTRIBUTING.md"]; !ok || got != "" {
		t.Errorf("expected CONTRIBUTING.md key present-but-empty; got %q (ok=%v)", got, ok)
	}
	if digest.GeneratedAt == "" {
		t.Errorf("expected GeneratedAt timestamp populated")
	}
}

func TestBuildRepoDigest_DiskOnlyPath(t *testing.T) {
	t.Setenv("LIVE_HAIKU_DISABLED", "1")

	repoDir := t.TempDir()
	mustRunGit(t, repoDir, "init", "-q")
	mustRunGit(t, repoDir, "config", "user.email", "test@example.com")
	mustRunGit(t, repoDir, "config", "user.name", "Test")
	mustRunGit(t, repoDir, "config", "commit.gpgsign", "false")
	if err := os.WriteFile(filepath.Join(repoDir, "README"),
		[]byte("disk only path repo\n"), 0644); err != nil {
		t.Fatalf("write README: %v", err)
	}
	mustRunGit(t, repoDir, "add", "README")
	mustRunGit(t, repoDir, "commit", "-q", "-m", "seed")

	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	c := NewInProcess(db).(*inProcessClient)
	digest, err := c.BuildRepoDigest(context.Background(), repoDir)
	if err != nil {
		t.Fatalf("BuildRepoDigest(disk-path): %v", err)
	}
	if digest.RepoName != "" {
		t.Errorf("expected RepoName empty for unregistered disk path; got %q", digest.RepoName)
	}
	if digest.LocalPath == "" {
		t.Errorf("expected LocalPath populated")
	}
	if digest.READMESample == "" {
		t.Errorf("expected README sample populated for disk-path repo")
	}
}

func TestBuildRepoDigest_EmptySpec(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	c := NewInProcess(db).(*inProcessClient)
	_, err := c.BuildRepoDigest(context.Background(), "")
	if err == nil {
		t.Fatalf("expected error for empty spec, got nil")
	}
}

func TestBuildRepoDigest_UnregisteredAndMissing(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	c := NewInProcess(db).(*inProcessClient)
	_, err := c.BuildRepoDigest(context.Background(), "/totally/missing/path-d6-test-12345")
	if err == nil {
		t.Fatalf("expected error for unregistered + missing path, got nil")
	}
	if !contains(err.Error(), "neither a registered repo nor a directory") {
		t.Errorf("expected resolution-failure message, got %v", err)
	}
}

func TestBuildRepoDigest_Idempotent(t *testing.T) {
	t.Setenv("LIVE_HAIKU_DISABLED", "1")

	repoDir := t.TempDir()
	mustRunGit(t, repoDir, "init", "-q")
	mustRunGit(t, repoDir, "config", "user.email", "t@t")
	mustRunGit(t, repoDir, "config", "user.name", "t")
	mustRunGit(t, repoDir, "config", "commit.gpgsign", "false")
	if err := os.WriteFile(filepath.Join(repoDir, "README.md"), []byte("x\n"), 0644); err != nil {
		t.Fatalf("write README: %v", err)
	}
	mustRunGit(t, repoDir, "add", "README.md")
	mustRunGit(t, repoDir, "commit", "-q", "-m", "seed")

	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	store.AddRepo(db, "idem", repoDir, "idempotence test")
	c := NewInProcess(db).(*inProcessClient)

	a, err := c.BuildRepoDigest(context.Background(), "idem")
	if err != nil {
		t.Fatalf("first BuildRepoDigest: %v", err)
	}
	b, err := c.BuildRepoDigest(context.Background(), "idem")
	if err != nil {
		t.Fatalf("second BuildRepoDigest: %v", err)
	}
	if a.RepoName != b.RepoName ||
		a.LocalPath != b.LocalPath ||
		a.Description != b.Description ||
		a.READMESample != b.READMESample ||
		len(a.TopLevelDirs) != len(b.TopLevelDirs) ||
		len(a.PublicAPISymbols) != len(b.PublicAPISymbols) ||
		len(a.Conventions) != len(b.Conventions) {
		t.Errorf("BuildRepoDigest is not idempotent on its non-time fields\nfirst:  %+v\nsecond: %+v", a, b)
	}
}

// ── small helpers ─────────────────────────────────────────────────────

func contains(haystack, needle string) bool {
	return strings.Contains(haystack, needle)
}

func containsString(slice []string, s string) bool {
	for _, v := range slice {
		if v == s {
			return true
		}
	}
	return false
}
