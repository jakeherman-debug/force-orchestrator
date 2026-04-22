package agents

import (
	"database/sql"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"force-orchestrator/internal/store"
)

// ── sanityCheckPRBody ────────────────────────────────────────────────────────

func TestSanityCheckPRBody_EmptyFails(t *testing.T) {
	if err := sanityCheckPRBody(""); err == nil {
		t.Errorf("empty body should fail sanity")
	}
	if err := sanityCheckPRBody("   \n  "); err == nil {
		t.Errorf("whitespace-only body should fail sanity")
	}
}

func TestSanityCheckPRBody_TooLongFails(t *testing.T) {
	huge := strings.Repeat("x", 60001)
	if err := sanityCheckPRBody(huge); err == nil {
		t.Errorf("body over 60k should fail")
	}
}

func TestSanityCheckPRBody_SecretsDetected(t *testing.T) {
	cases := []string{
		"Some text with sk-" + strings.Repeat("a", 32) + " embedded",
		"AWS access key: AKIA" + strings.Repeat("A", 16),
		"Got a GitHub token ghp_" + strings.Repeat("a", 36),
		"Slack webhook xoxb-1234567890-abcdef",
		"-----BEGIN PRIVATE KEY-----\nAAAA\n-----END PRIVATE KEY-----",
	}
	for _, c := range cases {
		if err := sanityCheckPRBody(c); err == nil {
			t.Errorf("should reject secret pattern: %.40s...", c)
		}
	}
}

func TestSanityCheckPRBody_UnresolvedPlaceholder(t *testing.T) {
	if err := sanityCheckPRBody("## Summary\n{{describe here}}\n"); err == nil {
		t.Errorf("unresolved {{placeholder}} should fail")
	}
}

func TestSanityCheckPRBody_UnfilledTemplateComment(t *testing.T) {
	body := `## Summary

<!-- describe the change here -->
`
	if err := sanityCheckPRBody(body); err == nil {
		t.Errorf("unfilled HTML comment should fail")
	}
}

func TestSanityCheckPRBody_ValidBodyPasses(t *testing.T) {
	body := `## Summary

Add OAuth flow to the API.

## Testing

Unit tests cover the new middleware.

## Risks

New dependency: golang.org/x/oauth2.
`
	if err := sanityCheckPRBody(body); err != nil {
		t.Errorf("valid body should pass: %v", err)
	}
}

// ── stripMarkdownFences ─────────────────────────────────────────────────────

func TestStripMarkdownFences(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"plain text", "plain text"},
		{"```\nfenced\n```", "fenced"},
		{"```markdown\nfenced\n```", "fenced"},
		{"```\n# title\n\nbody\n```", "# title\n\nbody"},
	}
	for _, c := range cases {
		got := stripMarkdownFences(c.in)
		if got != c.want {
			t.Errorf("stripMarkdownFences(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// ── buildDraftPRTitle ───────────────────────────────────────────────────────

func TestBuildDraftPRTitle(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"[5] Add OAuth support", "Add OAuth support"},
		{"Unprefixed name", "Unprefixed name"},
		{"[1]", ""},
	}
	for _, c := range cases {
		got := buildDraftPRTitle(&store.Convoy{Name: c.in})
		if got != c.want {
			t.Errorf("title(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// ── buildFallbackPRBody ─────────────────────────────────────────────────────

func TestBuildFallbackPRBody_ValidAndPassesSanity(t *testing.T) {
	convoy := &store.Convoy{Name: "[1] test"}
	ab := store.ConvoyAskBranch{Repo: "api", AskBranch: "force/ask-1-test"}
	body := buildFallbackPRBody(convoy, ab, "context string")
	if !strings.Contains(body, "## Summary") {
		t.Errorf("fallback body missing Summary section: %s", body)
	}
	if err := sanityCheckPRBody(body); err != nil {
		t.Errorf("fallback body should pass sanity: %v", err)
	}
}

// ── convoyReadyToShip ───────────────────────────────────────────────────────

func TestConvoyReadyToShip_AllCompletedAndMerged(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	cid, _ := store.CreateConvoy(db, "[1] t")
	tid, _ := store.AddConvoyTask(db, 0, "api", "t", cid, 0, "Pending")
	db.Exec(`UPDATE BountyBoard SET status = 'Completed' WHERE id = ?`, tid)
	prID, _ := store.CreateAskBranchPR(db, tid, cid, "api", "u", 1)
	_ = store.MarkAskBranchPRMerged(db, prID)

	if !convoyReadyToShip(db, cid) {
		t.Errorf("all-completed-and-merged should be ready to ship")
	}
}

func TestConvoyReadyToShip_PendingTaskBlocks(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	cid, _ := store.CreateConvoy(db, "[1] t")
	_, _ = store.AddConvoyTask(db, 0, "api", "t1", cid, 0, "Completed")
	db.Exec(`UPDATE BountyBoard SET status = 'Completed' WHERE convoy_id = ? AND status = 'Pending'`, cid)
	_, _ = store.AddConvoyTask(db, 0, "api", "t2", cid, 0, "Pending") // still Pending
	if convoyReadyToShip(db, cid) {
		t.Errorf("pending task must block ship")
	}
}

func TestConvoyReadyToShip_OpenSubPRBlocks(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	cid, _ := store.CreateConvoy(db, "[1] t")
	tid, _ := store.AddConvoyTask(db, 0, "api", "t", cid, 0, "Pending")
	db.Exec(`UPDATE BountyBoard SET status = 'Completed' WHERE id = ?`, tid)
	_, _ = store.CreateAskBranchPR(db, tid, cid, "api", "u", 1)
	// PR is Open, not Merged — blocks ship.
	if convoyReadyToShip(db, cid) {
		t.Errorf("open sub-PR must block ship")
	}
}

// ── Chancellor's convoy-complete → enqueues Diplomat ─────────────────────────

func TestCheckConvoyCompletions_PRFlowQueuesDiplomat(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	cid, _ := store.CreateConvoy(db, "[1] pr-flow")
	tid, _ := store.AddConvoyTask(db, 0, "api", "t", cid, 0, "Pending")
	db.Exec(`UPDATE BountyBoard SET status = 'Completed' WHERE id = ?`, tid)
	// Convoy has an ask-branch row → PR-flow convoy.
	store.AddRepo(db, "api", "/tmp/api", "")
	_ = store.UpsertConvoyAskBranch(db, cid, "api", "force/ask-1-pr-flow", "sha")

	CheckConvoyCompletions(db, testLogger{})

	conv := store.GetConvoy(db, cid)
	if conv.Status != "AwaitingDraftPR" {
		t.Errorf("PR-flow convoy should transition to AwaitingDraftPR, got %q", conv.Status)
	}
	var queued int
	db.QueryRow(`SELECT COUNT(*) FROM BountyBoard WHERE type = 'ShipConvoy' AND status = 'Pending'`).Scan(&queued)
	if queued != 1 {
		t.Errorf("expected 1 ShipConvoy queued, got %d", queued)
	}
}

func TestCheckConvoyCompletions_LegacyConvoyStillCompletes(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	cid, _ := store.CreateConvoy(db, "[1] legacy")
	tid, _ := store.AddConvoyTask(db, 0, "api", "t", cid, 0, "Pending")
	db.Exec(`UPDATE BountyBoard SET status = 'Completed' WHERE id = ?`, tid)
	// No ConvoyAskBranch row → legacy convoy. Chancellor should mark Completed.

	CheckConvoyCompletions(db, testLogger{})

	conv := store.GetConvoy(db, cid)
	if conv.Status != "Completed" {
		t.Errorf("legacy convoy should go straight to Completed, got %q", conv.Status)
	}
	var queued int
	db.QueryRow(`SELECT COUNT(*) FROM BountyBoard WHERE type = 'ShipConvoy'`).Scan(&queued)
	if queued != 0 {
		t.Errorf("legacy convoy should not queue ShipConvoy, got %d", queued)
	}
}

// ── runShipConvoy — end-to-end with real git origin and stubbed gh + Claude ──

func setupShipScenario(t *testing.T, db *sql.DB) (convoyID int, repoDir string) {
	t.Helper()
	origin := t.TempDir()
	exec.Command("git", "init", "-q", "--bare", "-b", "main", origin).Run()
	repoDir = t.TempDir()
	exec.Command("git", "clone", "-q", origin, repoDir).Run()
	run := func(args ...string) {
		cmd := exec.Command("git", append([]string{"-C", repoDir}, args...)...)
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=T", "GIT_AUTHOR_EMAIL=t@t",
			"GIT_COMMITTER_NAME=T", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %s", args, string(out))
		}
	}
	run("config", "user.email", "t@t")
	run("config", "user.name", "Test")
	os.WriteFile(filepath.Join(repoDir, "README"), []byte("hi"), 0644)
	run("add", ".")
	run("commit", "-m", "initial")
	run("push", "-u", "origin", "main")
	run("remote", "set-head", "origin", "main")
	shaOut, _ := exec.Command("git", "-C", repoDir, "rev-parse", "HEAD").Output()
	baseSHA := strings.TrimSpace(string(shaOut))

	// Ask-branch exists on origin with a meaningful diff.
	run("checkout", "-b", "force/ask-1-ship")
	os.WriteFile(filepath.Join(repoDir, "feature.txt"), []byte("feat"), 0644)
	run("add", ".")
	run("commit", "-m", "add feature")
	run("push", "-u", "origin", "force/ask-1-ship")
	run("checkout", "main")

	store.AddRepo(db, "api", repoDir, "")
	_ = store.SetRepoRemoteInfo(db, "api", "https://github.com/acme/api.git", "main")
	convoyID, _ = store.CreateConvoy(db, "[1] ship-test")
	tid, _ := store.AddConvoyTask(db, 0, "api", "add feature", convoyID, 0, "Pending")
	db.Exec(`UPDATE BountyBoard SET status = 'Completed' WHERE id = ?`, tid)
	prID, _ := store.CreateAskBranchPR(db, tid, convoyID, "api", "https://gh/pull/1", 1)
	_ = store.MarkAskBranchPRMerged(db, prID)
	_ = store.UpsertConvoyAskBranch(db, convoyID, "api", "force/ask-1-ship", baseSHA)
	return convoyID, repoDir
}

func TestRunShipConvoy_NotReady_Requeues(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	cid, _ := store.CreateConvoy(db, "[1] not-ready")
	tid, _ := store.AddConvoyTask(db, 0, "api", "t", cid, 0, "Pending") // still Pending
	_ = tid
	store.AddRepo(db, "api", "/tmp/api", "")
	_ = store.UpsertConvoyAskBranch(db, cid, "api", "force/ask-1-not-ready", "sha")

	shipID, _ := QueueShipConvoy(db, cid)
	b, _ := store.GetBounty(db, shipID)
	runShipConvoy(db, "Diplomat", b, testLogger{})

	updated, _ := store.GetBounty(db, shipID)
	if updated.Status != "Pending" {
		t.Errorf("not-ready convoy should re-queue to Pending, got %q", updated.Status)
	}
}

func TestRunShipConvoy_NoAskBranchesCompletes(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	cid, _ := store.CreateConvoy(db, "[1] legacy")
	shipID, _ := QueueShipConvoy(db, cid)
	b, _ := store.GetBounty(db, shipID)
	runShipConvoy(db, "Diplomat", b, testLogger{})

	updated, _ := store.GetBounty(db, shipID)
	if updated.Status != "Completed" {
		t.Errorf("no-ask-branches should complete as no-op, got %q", updated.Status)
	}
}

func TestRunShipConvoy_HappyPath_OpensDraftPR(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	cid, _ := setupShipScenario(t, db)

	// Stub gh pr create.
	stub := installGHStub(t, map[string]ghStubResp{
		"pr create": {stdout: "https://github.com/acme/api/pull/42\n"},
	})

	// Stub Claude so Diplomat gets a deterministic body.
	// No repo template → Diplomat uses buildFallbackPRBody (no Claude call).
	// To exercise the LLM path, we'll install a template file.
	repo := store.GetRepo(db, "api")
	templatePath := filepath.Join(repo.LocalPath, ".github", "pull_request_template.md")
	os.MkdirAll(filepath.Dir(templatePath), 0755)
	os.WriteFile(templatePath, []byte("## Summary\n\n{{summary}}\n\n## Testing\n\n{{testing}}\n"), 0644)
	_ = store.SetRepoPRTemplatePath(db, "api", templatePath)

	withStubCLIRunner(t, "## Summary\n\nAdded a feature file.\n\n## Testing\n\nVerified manually.\n", nil)

	shipID, _ := QueueShipConvoy(db, cid)
	b, _ := store.GetBounty(db, shipID)
	runShipConvoy(db, "Diplomat", b, testLogger{})

	updated, _ := store.GetBounty(db, shipID)
	if updated.Status != "Completed" {
		t.Errorf("happy ship should complete, got %q (err=%s)", updated.Status, updated.Owner)
	}

	// Draft PR recorded.
	ab := store.GetConvoyAskBranch(db, cid, "api")
	if ab.DraftPRNumber != 42 {
		t.Errorf("draft PR number: got %d want 42", ab.DraftPRNumber)
	}
	if ab.DraftPRState != "Open" {
		t.Errorf("draft PR state: got %q want Open", ab.DraftPRState)
	}

	// Convoy status → DraftPROpen.
	conv := store.GetConvoy(db, cid)
	if conv.Status != "DraftPROpen" {
		t.Errorf("convoy status: got %q want DraftPROpen", conv.Status)
	}

	// Verify gh was called with --draft.
	var sawCreate bool
	for _, c := range stub.calls {
		if len(c.args) >= 2 && c.args[0] == "pr" && c.args[1] == "create" {
			sawCreate = true
			joined := strings.Join(c.args, " ")
			if !strings.Contains(joined, "--draft") {
				t.Errorf("PR create missing --draft: %q", joined)
			}
			if !strings.Contains(joined, "--base main") {
				t.Errorf("draft PR should target main: %q", joined)
			}
			if !strings.Contains(joined, "--head force/ask-1-ship") {
				t.Errorf("draft PR head wrong: %q", joined)
			}
		}
	}
	if !sawCreate {
		t.Errorf("gh pr create was never invoked")
	}
}

// TestRunShipConvoy_PRCreateFailurePropagates exercises the error path where
// gh pr create fails — ShipConvoy must FailBounty (NOT silently complete).
func TestRunShipConvoy_PRCreateFailurePropagates(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	cid, _ := setupShipScenario(t, db)

	// Stub gh pr create to fail.
	installGHStub(t, map[string]ghStubResp{
		"pr create": {stderr: "Error: 401 Unauthorized", err: fmt.Errorf("exit 1")},
	})
	withStubCLIRunner(t, "## Summary\n\ndone.\n", nil)

	shipID, _ := QueueShipConvoy(db, cid)
	b, _ := store.GetBounty(db, shipID)
	runShipConvoy(db, "Diplomat", b, testLogger{})

	updated, _ := store.GetBounty(db, shipID)
	if updated.Status != "Failed" {
		t.Errorf("ShipConvoy must fail when gh pr create errors, got %q", updated.Status)
	}

	// No draft PR recorded.
	ab := store.GetConvoyAskBranch(db, cid, "api")
	if ab.DraftPRNumber != 0 {
		t.Errorf("failed ship must not record a draft PR: %d", ab.DraftPRNumber)
	}

	// Convoy must NOT transition to DraftPROpen on failure.
	conv := store.GetConvoy(db, cid)
	if conv.Status == "DraftPROpen" {
		t.Error("failed ship must not transition convoy to DraftPROpen")
	}
}

// TestRunShipConvoy_SanityFailureFallsBackToLLMThenEscalates proves the
// critic-retry loop runs once, then escalates if sanity fails twice.
func TestRunShipConvoy_SanityFailureFallsBackToLLMThenEscalates(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	cid, _ := setupShipScenario(t, db)
	repo := store.GetRepo(db, "api")
	tplPath := filepath.Join(repo.LocalPath, ".github", "pull_request_template.md")
	os.MkdirAll(filepath.Dir(tplPath), 0755)
	os.WriteFile(tplPath, []byte("## Summary\n{{summary}}\n"), 0644)
	_ = store.SetRepoPRTemplatePath(db, "api", tplPath)

	// Both Claude calls return a body that contains an unresolved placeholder
	// (fails sanity). Expect: first attempt fails sanity, critic-retry called,
	// same failure, Diplomat fails the ship task.
	withStubCLIRunner(t, "## Summary\n{{unresolved}}\n", nil)
	installGHStub(t, map[string]ghStubResp{
		"pr create": {stdout: "https://github.com/acme/api/pull/1\n"},
	})

	shipID, _ := QueueShipConvoy(db, cid)
	b, _ := store.GetBounty(db, shipID)
	runShipConvoy(db, "Diplomat", b, testLogger{})

	updated, _ := store.GetBounty(db, shipID)
	if updated.Status != "Failed" {
		t.Errorf("sanity double-failure must fail the ship task, got %q: %s", updated.Status, updated.Payload)
	}
}

func TestRunShipConvoy_SkipAlreadyDraftedBranches(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	cid, _ := setupShipScenario(t, db)
	// Pre-set one branch as already having a draft PR.
	_ = store.SetConvoyAskBranchDraftPR(db, cid, "api", "https://github.com/acme/api/pull/99", 99, "Open")

	stub := installGHStub(t, map[string]ghStubResp{
		"pr create": {stdout: "https://github.com/acme/api/pull/100\n"},
	})
	withStubCLIRunner(t, "## Summary\n\ndone.\n", nil)

	shipID, _ := QueueShipConvoy(db, cid)
	b, _ := store.GetBounty(db, shipID)
	runShipConvoy(db, "Diplomat", b, testLogger{})

	// gh pr create should NOT have been called.
	for _, c := range stub.calls {
		if len(c.args) >= 2 && c.args[0] == "pr" && c.args[1] == "create" {
			t.Errorf("should not have called gh pr create for an already-drafted branch")
		}
	}
	ab := store.GetConvoyAskBranch(db, cid, "api")
	if ab.DraftPRNumber != 99 {
		t.Errorf("existing draft PR should be preserved, got %d", ab.DraftPRNumber)
	}
}
