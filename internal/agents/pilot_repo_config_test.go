package agents

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"force-orchestrator/internal/store"
)

// ── RevalidateRepoConfig handler ────────────────────────────────────────────

func TestRunRevalidateRepoConfig_AllGood_NoChange(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	wt, _ := makeOriginAndClone(t)
	templatePath := filepath.Join(wt, ".github", "pull_request_template.md")
	os.MkdirAll(filepath.Dir(templatePath), 0755)
	os.WriteFile(templatePath, []byte("## Summary\n"), 0644)

	store.AddRepo(db, "api", wt, "")
	remote, _ := repoRemoteURL(wt)
	_ = store.SetRepoRemoteInfo(db, "api", remote, "main")
	_ = store.SetRepoPRTemplatePath(db, "api", templatePath)

	id, _ := QueueRevalidateRepoConfig(db, "api")
	b, _ := store.GetBounty(db, id)
	runRevalidateRepoConfig(db, b, testLogger{})

	updated, _ := store.GetBounty(db, id)
	if updated.Status != "Completed" {
		t.Errorf("healthy repo should Complete, got %q", updated.Status)
	}
	r := store.GetRepo(db, "api")
	if !r.PRFlowEnabled {
		t.Errorf("healthy repo should stay pr_flow_enabled=true")
	}
	if r.QuarantinedAt != "" {
		t.Errorf("healthy repo must not be quarantined: %q", r.QuarantinedAt)
	}
	if r.PRTemplatePath != templatePath {
		t.Errorf("template path shouldn't change: got %q", r.PRTemplatePath)
	}
}

func TestRunRevalidateRepoConfig_RemoteUnreachable_Quarantines(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	// Repo path exists but has no git remote configured.
	wt := t.TempDir()
	os.MkdirAll(filepath.Join(wt, ".git"), 0755) // not a real repo, triggers remote error
	store.AddRepo(db, "broken", wt, "")
	_ = store.SetRepoRemoteInfo(db, "broken", "https://expected.com/x.git", "main")

	id, _ := QueueRevalidateRepoConfig(db, "broken")
	b, _ := store.GetBounty(db, id)
	runRevalidateRepoConfig(db, b, testLogger{})

	r := store.GetRepo(db, "broken")
	if r.PRFlowEnabled {
		t.Errorf("unreachable-remote repo must be quarantined (pr_flow_enabled=false)")
	}
	if r.QuarantinedAt == "" {
		t.Errorf("quarantined_at should be set")
	}
	if !strings.Contains(r.QuarantineReason, "unreachable") {
		t.Errorf("quarantine reason missing hint: %q", r.QuarantineReason)
	}
}

func TestRunRevalidateRepoConfig_TemplateMoved_RequeuesFindPRTemplate(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	wt, _ := makeOriginAndClone(t)
	store.AddRepo(db, "api", wt, "")
	remote, _ := repoRemoteURL(wt)
	_ = store.SetRepoRemoteInfo(db, "api", remote, "main")
	// Point to a template path that doesn't exist.
	missingPath := filepath.Join(wt, ".github", "gone.md")
	_ = store.SetRepoPRTemplatePath(db, "api", missingPath)

	id, _ := QueueRevalidateRepoConfig(db, "api")
	b, _ := store.GetBounty(db, id)
	runRevalidateRepoConfig(db, b, testLogger{})

	// The handler should have cleared the stale path and queued FindPRTemplate.
	r := store.GetRepo(db, "api")
	if r.PRTemplatePath != "" {
		t.Errorf("stale template path should be cleared, got %q", r.PRTemplatePath)
	}
	var queued int
	db.QueryRow(`SELECT COUNT(*) FROM BountyBoard WHERE type = 'FindPRTemplate' AND target_repo = 'api' AND status = 'Pending'`).Scan(&queued)
	if queued != 1 {
		t.Errorf("missing template should requeue FindPRTemplate, got %d", queued)
	}
}

func TestRunRevalidateRepoConfig_RemoteURLChanged_Updates(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	wt, _ := makeOriginAndClone(t)
	actualRemote, _ := repoRemoteURL(wt)
	// Store a WRONG remote — revalidation should correct it.
	store.AddRepo(db, "api", wt, "")
	_ = store.SetRepoRemoteInfo(db, "api", "https://wrong.example.com/x.git", "main")

	id, _ := QueueRevalidateRepoConfig(db, "api")
	b, _ := store.GetBounty(db, id)
	runRevalidateRepoConfig(db, b, testLogger{})

	r := store.GetRepo(db, "api")
	if r.RemoteURL != actualRemote {
		t.Errorf("stored remote should be corrected to %q, got %q", actualRemote, r.RemoteURL)
	}
	if !r.PRFlowEnabled {
		t.Errorf("URL correction shouldn't quarantine: pr_flow_enabled=%v", r.PRFlowEnabled)
	}
}

func TestRunRevalidateRepoConfig_NoLocalPath_Quarantines(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	store.AddRepo(db, "nopath", "", "")
	_ = store.SetRepoRemoteInfo(db, "nopath", "x", "main")

	id, _ := QueueRevalidateRepoConfig(db, "nopath")
	b, _ := store.GetBounty(db, id)
	runRevalidateRepoConfig(db, b, testLogger{})

	r := store.GetRepo(db, "nopath")
	if r.PRFlowEnabled {
		t.Errorf("no-local-path repo should be quarantined")
	}
}

func TestRunRevalidateRepoConfig_RepoRemovedCompletes(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	store.AddRepo(db, "temp", "/tmp/x", "")
	_ = store.SetRepoRemoteInfo(db, "temp", "x", "main")
	id, _ := QueueRevalidateRepoConfig(db, "temp")
	// Delete the repo between queue and run.
	store.RemoveRepo(db, "temp")

	b, _ := store.GetBounty(db, id)
	runRevalidateRepoConfig(db, b, testLogger{})
	updated, _ := store.GetBounty(db, id)
	if updated.Status != "Completed" {
		t.Errorf("removed-repo should complete as no-op, got %q", updated.Status)
	}
}

// ── repo-config-check dog ───────────────────────────────────────────────────

func TestDogRepoConfigCheck_QueuesOncePerRepo(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	store.AddRepo(db, "a", "/a", "")
	store.AddRepo(db, "b", "/b", "")
	store.AddRepo(db, "c", "/c", "")

	if err := dogRepoConfigCheck(db, testLogger{}); err != nil {
		t.Fatal(err)
	}
	var queued int
	db.QueryRow(`SELECT COUNT(*) FROM BountyBoard WHERE type = 'RevalidateRepoConfig' AND status = 'Pending'`).Scan(&queued)
	if queued != 3 {
		t.Errorf("expected 3 revalidate tasks, got %d", queued)
	}
}

func TestDogRepoConfigCheck_SkipsExistingPending(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	store.AddRepo(db, "a", "/a", "")

	// Pre-queue one manually.
	_, _ = QueueRevalidateRepoConfig(db, "a")

	_ = dogRepoConfigCheck(db, testLogger{})
	var queued int
	db.QueryRow(`SELECT COUNT(*) FROM BountyBoard WHERE type = 'RevalidateRepoConfig' AND status = 'Pending'`).Scan(&queued)
	if queued != 1 {
		t.Errorf("existing pending task should dedupe dog, got %d", queued)
	}
}
