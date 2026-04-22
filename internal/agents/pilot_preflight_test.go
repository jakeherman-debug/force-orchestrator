package agents

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"force-orchestrator/internal/gh"
	"force-orchestrator/internal/store"
)

// ── helpers ────────────────────────────────────────────────────────────────

// makeGitRepo runs `git init` and optionally sets an origin URL.
func makeGitRepo(t *testing.T, dir string, remoteURL string) {
	t.Helper()
	if err := exec.Command("git", "init", "-q", "-b", "main", dir).Run(); err != nil {
		t.Fatalf("git init: %v", err)
	}
	// Need a commit so `rev-parse --verify main` succeeds.
	exec.Command("git", "-C", dir, "config", "user.email", "test@test").Run()
	exec.Command("git", "-C", dir, "config", "user.name", "Test").Run()
	os.WriteFile(filepath.Join(dir, "README"), []byte("hi"), 0644)
	exec.Command("git", "-C", dir, "add", ".").Run()
	exec.Command("git", "-C", dir, "commit", "-q", "-m", "initial").Run()
	if remoteURL != "" {
		exec.Command("git", "-C", dir, "remote", "add", "origin", remoteURL).Run()
		// Simulate `origin/HEAD → origin/main` without actually pushing.
		exec.Command("git", "-C", dir, "update-ref", "refs/remotes/origin/main", "HEAD").Run()
		exec.Command("git", "-C", dir, "symbolic-ref", "refs/remotes/origin/HEAD", "refs/remotes/origin/main").Run()
	}
}

// stubGH builds a gh.Client with a canned auth response.
func stubGH(authOK bool) *gh.Client {
	stub := ghStub{authOK: authOK}
	return gh.NewClientWithRunner(stub)
}

type ghStub struct{ authOK bool }

func (s ghStub) Run(cwd string, args []string, stdin []byte) ([]byte, []byte, error) {
	if len(args) >= 2 && args[0] == "auth" && args[1] == "status" {
		if s.authOK {
			return []byte("✓ Logged in to github.com as testuser\n"), nil, nil
		}
		return nil, []byte("not authenticated\n"), fmt.Errorf("exit 1")
	}
	return nil, nil, fmt.Errorf("unexpected gh args: %v", args)
}

// ── PRFlowPreflight ────────────────────────────────────────────────────────

func TestPRFlowPreflight_AuthFatalOnFailure(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	checks := PRFlowPreflight(db, stubGH(false))
	var authCheck *PreflightCheck
	for i, c := range checks {
		if c.Name == "gh-auth" {
			authCheck = &checks[i]
		}
	}
	if authCheck == nil {
		t.Fatal("missing gh-auth check")
	}
	if authCheck.Passed {
		t.Errorf("auth should have failed")
	}
	if !authCheck.Fatal {
		t.Errorf("gh-auth failure MUST be fatal")
	}
}

func TestPRFlowPreflight_PerRepoOriginChecks(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	// Repo A: has origin.
	dirA := t.TempDir()
	makeGitRepo(t, dirA, "git@github.com:acme/a.git")
	store.AddRepo(db, "a", dirA, "")

	// Repo B: git repo but no origin remote configured.
	dirB := t.TempDir()
	makeGitRepo(t, dirB, "")
	store.AddRepo(db, "b", dirB, "")

	// Repo C: no local_path at all (stored empty).
	store.AddRepo(db, "c", "", "")

	checks := PRFlowPreflight(db, stubGH(true))

	var aRes, bRes, cRes *PreflightCheck
	for i, c := range checks {
		if c.Name != "repo-origin" {
			continue
		}
		switch c.RepoKey {
		case "a":
			aRes = &checks[i]
		case "b":
			bRes = &checks[i]
		case "c":
			cRes = &checks[i]
		}
	}
	if aRes == nil || !aRes.Passed {
		t.Errorf("repo a with origin must pass: %+v", aRes)
	}
	if bRes == nil || bRes.Passed {
		t.Errorf("repo b without origin must fail: %+v", bRes)
	}
	if bRes != nil && bRes.Fatal {
		t.Errorf("per-repo failure must not be fatal")
	}
	if cRes == nil || cRes.Passed {
		t.Errorf("repo with empty local_path must fail: %+v", cRes)
	}
}

// ── BackfillRepoRemoteInfo ──────────────────────────────────────────────────

func TestBackfillRepoRemoteInfo_PopulatesFields(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	dir := t.TempDir()
	makeGitRepo(t, dir, "git@github.com:acme/api.git")
	store.AddRepo(db, "api", dir, "")

	summary := BackfillRepoRemoteInfo(db)
	if !strings.Contains(summary, "backfilled 1") {
		t.Errorf("summary: %q", summary)
	}
	r := store.GetRepo(db, "api")
	if r.RemoteURL != "git@github.com:acme/api.git" {
		t.Errorf("remote URL: %q", r.RemoteURL)
	}
	if r.DefaultBranch != "main" {
		t.Errorf("default branch: %q", r.DefaultBranch)
	}
	if !r.PRFlowEnabled {
		t.Errorf("successful backfill must leave pr_flow_enabled=true")
	}
}

func TestBackfillRepoRemoteInfo_DisablesPRFlowWhenOriginMissing(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	dir := t.TempDir()
	makeGitRepo(t, dir, "") // no origin
	store.AddRepo(db, "broken", dir, "")

	summary := BackfillRepoRemoteInfo(db)
	if !strings.Contains(summary, "disabled pr_flow for 1") {
		t.Errorf("summary should mention disabled: %q", summary)
	}
	r := store.GetRepo(db, "broken")
	if r.PRFlowEnabled {
		t.Errorf("broken repo should have pr_flow_enabled=false after backfill")
	}
	if r.RemoteURL != "" {
		t.Errorf("empty remote should not be stored: %q", r.RemoteURL)
	}
}

func TestBackfillRepoRemoteInfo_SkipsAlreadyPopulated(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	store.AddRepo(db, "prepop", "/ignored", "")
	_ = store.SetRepoRemoteInfo(db, "prepop", "git@existing.com:x/y.git", "main")

	summary := BackfillRepoRemoteInfo(db)
	if !strings.Contains(summary, "backfilled 0") {
		t.Errorf("already-populated repo should be skipped: %q", summary)
	}
	r := store.GetRepo(db, "prepop")
	if r.RemoteURL != "git@existing.com:x/y.git" {
		t.Errorf("existing remote clobbered: %q", r.RemoteURL)
	}
}

// ── EnqueueMissingFindPRTemplate ───────────────────────────────────────────

func TestEnqueueMissingFindPRTemplate_QueuesOnlyWhenNeeded(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	// Repo needing template: has local_path, pr_flow_enabled, empty template.
	store.AddRepo(db, "needy", "/tmp/needy", "")

	// Repo already populated — skip.
	store.AddRepo(db, "done", "/tmp/done", "")
	_ = store.SetRepoPRTemplatePath(db, "done", "/tmp/done/.github/pull_request_template.md")

	// Disabled — skip.
	store.AddRepo(db, "disabled", "/tmp/disabled", "")
	_ = store.SetRepoPRFlowEnabled(db, "disabled", false)

	// No local_path — skip.
	store.AddRepo(db, "nopath", "", "")

	queued, skipped := EnqueueMissingFindPRTemplate(db)
	if queued != 1 {
		t.Errorf("expected 1 task queued (needy), got %d", queued)
	}
	if skipped != 3 {
		t.Errorf("expected 3 skipped (done, disabled, nopath), got %d", skipped)
	}
}

func TestEnqueueMissingFindPRTemplate_Idempotent(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	store.AddRepo(db, "needy", "/tmp/needy", "")
	q1, _ := EnqueueMissingFindPRTemplate(db)
	q2, _ := EnqueueMissingFindPRTemplate(db)
	if q1 != 1 || q2 != 0 {
		t.Errorf("second run should skip (Pending task already exists): q1=%d q2=%d", q1, q2)
	}
}
