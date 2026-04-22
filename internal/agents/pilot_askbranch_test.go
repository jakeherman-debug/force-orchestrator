package agents

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"force-orchestrator/internal/store"
)

// ── helpers ──────────────────────────────────────────────────────────────────

// makeOriginAndClone mirrors the git package test helper but is replicated here
// so these agent-level tests don't have to import internal/git/test helpers.
func makeOriginAndClone(t *testing.T) (worktree, origin string) {
	t.Helper()
	originDir := t.TempDir()
	if err := exec.Command("git", "init", "-q", "--bare", "-b", "main", originDir).Run(); err != nil {
		t.Fatal(err)
	}
	wt := t.TempDir()
	if err := exec.Command("git", "clone", "-q", originDir, wt).Run(); err != nil {
		t.Fatal(err)
	}
	exec.Command("git", "-C", wt, "config", "user.email", "t@t").Run()
	exec.Command("git", "-C", wt, "config", "user.name", "Test").Run()
	os.WriteFile(filepath.Join(wt, "README"), []byte("hi"), 0644)
	exec.Command("git", "-C", wt, "add", ".").Run()
	exec.Command("git", "-C", wt, "commit", "-q", "-m", "initial").Run()
	exec.Command("git", "-C", wt, "push", "-u", "origin", "main").Run()
	exec.Command("git", "-C", wt, "remote", "set-head", "origin", "main").Run()
	return wt, originDir
}

// setupRegisteredRepo creates a git repo with origin and registers it.
func setupRegisteredRepo(t *testing.T, db interface{ /* *sql.DB */ }, name string) string {
	t.Helper()
	wt, _ := makeOriginAndClone(t)
	remoteOut, _ := exec.Command("git", "-C", wt, "remote", "get-url", "origin").Output()
	_ = remoteOut
	// Use type assertion since we need sql.DB but want t.Helper-friendly signature.
	return wt
}

// ── AskBranchNameForConvoy ───────────────────────────────────────────────────

func TestAskBranchNameForConvoy(t *testing.T) {
	cases := []struct {
		convoyID   int
		convoyName string
		want       string
	}{
		{5, "[5] Add OAuth", "force/ask-5-add-oauth"},
		{12, "Freestyle name", "force/ask-12-freestyle-name"},
		{1, "", "force/ask-1-ask"},
		{99, "[99]", "force/ask-99-ask"},
	}
	for _, c := range cases {
		got := AskBranchNameForConvoy(c.convoyID, c.convoyName)
		if got != c.want {
			t.Errorf("AskBranchNameForConvoy(%d, %q) = %q, want %q", c.convoyID, c.convoyName, got, c.want)
		}
	}
}

// ── QueueCreateAskBranch ─────────────────────────────────────────────────────

func TestQueueCreateAskBranch_WritesPayload(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	cid, _ := store.CreateConvoy(db, "[7] test")
	id, err := QueueCreateAskBranch(db, cid)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := store.GetBounty(db, id)
	if b.Type != "CreateAskBranch" || b.Status != "Pending" {
		t.Errorf("unexpected bounty: %+v", b)
	}
	if !strings.Contains(b.Payload, `"convoy_id":`) {
		t.Errorf("payload missing convoy_id: %q", b.Payload)
	}
}

// ── runCreateAskBranch — end-to-end against a real origin ───────────────────

func TestRunCreateAskBranch_HappyPathSingleRepo(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	wt, _ := makeOriginAndClone(t)
	store.AddRepo(db, "api", wt, "API repo")
	_ = store.SetRepoRemoteInfo(db, "api", "file://"+wt, "main")

	cid, _ := store.CreateConvoy(db, "[1] add oauth")
	_, _ = store.AddConvoyTask(db, 0, "api", "implement oauth", cid, 0, "Pending")

	taskID, _ := QueueCreateAskBranch(db, cid)
	b, _ := store.GetBounty(db, taskID)
	runCreateAskBranch(db, b, testLogger{})

	got := store.GetConvoyAskBranch(db, cid, "api")
	if got == nil {
		t.Fatal("ConvoyAskBranch row not created")
	}
	if got.AskBranch != "force/ask-1-add-oauth" {
		t.Errorf("unexpected branch name: %q", got.AskBranch)
	}
	if len(got.AskBranchBaseSHA) != 40 {
		t.Errorf("base SHA not recorded: %q", got.AskBranchBaseSHA)
	}

	// Task must be marked Completed.
	updated, _ := store.GetBounty(db, taskID)
	if updated.Status != "Completed" {
		t.Errorf("task status: %q", updated.Status)
	}
}

func TestRunCreateAskBranch_MultiRepoFansOut(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	wtA, _ := makeOriginAndClone(t)
	wtB, _ := makeOriginAndClone(t)
	store.AddRepo(db, "api", wtA, "")
	store.AddRepo(db, "monolith", wtB, "")
	_ = store.SetRepoRemoteInfo(db, "api", "file://"+wtA, "main")
	_ = store.SetRepoRemoteInfo(db, "monolith", "file://"+wtB, "main")

	cid, _ := store.CreateConvoy(db, "[2] cross-repo")
	_, _ = store.AddConvoyTask(db, 0, "api", "part 1", cid, 0, "Pending")
	_, _ = store.AddConvoyTask(db, 0, "monolith", "part 2", cid, 0, "Pending")

	taskID, _ := QueueCreateAskBranch(db, cid)
	b, _ := store.GetBounty(db, taskID)
	runCreateAskBranch(db, b, testLogger{})

	branches := store.ListConvoyAskBranches(db, cid)
	if len(branches) != 2 {
		t.Fatalf("expected ask-branches for both repos, got %d: %+v", len(branches), branches)
	}
	// Both repos must share the same branch NAME — derived from the convoy ID
	// (not the [N] prefix) plus a slug of the human-readable portion of the name.
	expected := AskBranchNameForConvoy(cid, "[2] cross-repo")
	for _, b := range branches {
		if b.AskBranch != expected {
			t.Errorf("repo %s: expected %q, got %q", b.Repo, expected, b.AskBranch)
		}
	}
}

func TestRunCreateAskBranch_Idempotent(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	wt, _ := makeOriginAndClone(t)
	store.AddRepo(db, "api", wt, "")
	_ = store.SetRepoRemoteInfo(db, "api", "file://"+wt, "main")

	cid, _ := store.CreateConvoy(db, "[3] idempotent")
	_, _ = store.AddConvoyTask(db, 0, "api", "t", cid, 0, "Pending")

	// First run.
	id1, _ := QueueCreateAskBranch(db, cid)
	b1, _ := store.GetBounty(db, id1)
	runCreateAskBranch(db, b1, testLogger{})

	// Second run — handler should skip repo because branch exists.
	id2, _ := QueueCreateAskBranch(db, cid)
	b2, _ := store.GetBounty(db, id2)
	runCreateAskBranch(db, b2, testLogger{})

	branches := store.ListConvoyAskBranches(db, cid)
	if len(branches) != 1 {
		t.Errorf("second run shouldn't duplicate: got %d rows", len(branches))
	}
	// Second task completed too.
	b2updated, _ := store.GetBounty(db, id2)
	if b2updated.Status != "Completed" {
		t.Errorf("second run should complete: got %q", b2updated.Status)
	}
}

func TestRunCreateAskBranch_SkipsPRFlowDisabledRepo(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	wt, _ := makeOriginAndClone(t)
	store.AddRepo(db, "legacy", wt, "")
	_ = store.SetRepoRemoteInfo(db, "legacy", "file://"+wt, "main")
	_ = store.SetRepoPRFlowEnabled(db, "legacy", false)

	cid, _ := store.CreateConvoy(db, "[4] legacy-repo")
	_, _ = store.AddConvoyTask(db, 0, "legacy", "do work", cid, 0, "Pending")

	taskID, _ := QueueCreateAskBranch(db, cid)
	b, _ := store.GetBounty(db, taskID)
	runCreateAskBranch(db, b, testLogger{})

	// No ask-branch should have been created.
	if branches := store.ListConvoyAskBranches(db, cid); len(branches) != 0 {
		t.Errorf("expected no branches for pr-flow-disabled repo, got %+v", branches)
	}
	// Task still completes (the skip is deliberate, not a failure).
	updated, _ := store.GetBounty(db, taskID)
	if updated.Status != "Completed" {
		t.Errorf("task should complete: %q", updated.Status)
	}
}

func TestRunCreateAskBranch_FailsWhenRepoUnregistered(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	cid, _ := store.CreateConvoy(db, "[5] no-repo")
	_, _ = store.AddConvoyTask(db, 0, "ghost", "t", cid, 0, "Pending")

	taskID, _ := QueueCreateAskBranch(db, cid)
	b, _ := store.GetBounty(db, taskID)
	runCreateAskBranch(db, b, testLogger{})

	updated, _ := store.GetBounty(db, taskID)
	if updated.Status != "Failed" {
		t.Errorf("unregistered repo should fail task: got %q", updated.Status)
	}
}

func TestRunCreateAskBranch_FailsOnInvalidPayload(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	res, _ := db.Exec(`INSERT INTO BountyBoard
		(parent_id, target_repo, type, status, payload, created_at)
		VALUES (0, '', 'CreateAskBranch', 'Pending', 'not-json', datetime('now'))`)
	id, _ := res.LastInsertId()
	b, _ := store.GetBounty(db, int(id))
	runCreateAskBranch(db, b, testLogger{})
	updated, _ := store.GetBounty(db, int(id))
	if updated.Status != "Failed" {
		t.Errorf("expected Failed, got %q", updated.Status)
	}
}

func TestRunCreateAskBranch_NoCodeEditTasksCompletes(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	cid, _ := store.CreateConvoy(db, "[6] empty")
	// Convoy has no CodeEdit tasks yet.

	taskID, _ := QueueCreateAskBranch(db, cid)
	b, _ := store.GetBounty(db, taskID)
	runCreateAskBranch(db, b, testLogger{})

	updated, _ := store.GetBounty(db, taskID)
	if updated.Status != "Completed" {
		t.Errorf("empty convoy should complete: got %q", updated.Status)
	}
}

// ── runCleanupAskBranch ─────────────────────────────────────────────────────

func TestRunCleanupAskBranch_DeletesBranchesAndRows(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	wt, _ := makeOriginAndClone(t)
	store.AddRepo(db, "api", wt, "")
	_ = store.SetRepoRemoteInfo(db, "api", "file://"+wt, "main")

	cid, _ := store.CreateConvoy(db, "[10] cleanup-me")
	_, _ = store.AddConvoyTask(db, 0, "api", "t", cid, 0, "Pending")

	// First create the branch.
	createID, _ := QueueCreateAskBranch(db, cid)
	b, _ := store.GetBounty(db, createID)
	runCreateAskBranch(db, b, testLogger{})
	if len(store.ListConvoyAskBranches(db, cid)) != 1 {
		t.Fatal("setup failed — expected 1 ask-branch row")
	}

	// Now clean it up.
	cleanupID, _ := QueueCleanupAskBranch(db, cid)
	cb, _ := store.GetBounty(db, cleanupID)
	runCleanupAskBranch(db, cb, testLogger{})

	// Rows gone.
	if len(store.ListConvoyAskBranches(db, cid)) != 0 {
		t.Errorf("rows should be gone after cleanup")
	}
	// Task completed.
	updated, _ := store.GetBounty(db, cleanupID)
	if updated.Status != "Completed" {
		t.Errorf("cleanup task should complete: %q", updated.Status)
	}
}

// ── backfillMissingAskBranches (Layer C) ────────────────────────────────────

func TestBackfillMissingAskBranches_QueuesForEligibleConvoys(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	// Eligible: Active, has CodeEdit tasks, no ask-branch.
	eligibleCID, _ := store.CreateConvoy(db, "[1] eligible")
	_, _ = store.AddConvoyTask(db, 0, "api", "t", eligibleCID, 0, "Pending")

	// Not eligible: Completed status.
	completeCID, _ := store.CreateConvoy(db, "[2] done")
	_, _ = store.AddConvoyTask(db, 0, "api", "t", completeCID, 0, "Completed")
	_ = store.SetConvoyStatus(db, completeCID, "Completed")

	// Not eligible: already has an ask-branch.
	hasBranchCID, _ := store.CreateConvoy(db, "[3] has-branch")
	_, _ = store.AddConvoyTask(db, 0, "api", "t", hasBranchCID, 0, "Pending")
	_ = store.UpsertConvoyAskBranch(db, hasBranchCID, "api", "force/ask-3-has-branch", "sha")

	backfillMissingAskBranches(db, testLogger{})

	// Only eligible convoy should have a CreateAskBranch queued.
	var queued int
	db.QueryRow(`SELECT COUNT(*) FROM BountyBoard WHERE type = 'CreateAskBranch' AND status = 'Pending'`).Scan(&queued)
	if queued != 1 {
		t.Errorf("expected 1 CreateAskBranch queued, got %d", queued)
	}
	// Must reference the eligible convoy.
	var payload string
	db.QueryRow(`SELECT payload FROM BountyBoard WHERE type = 'CreateAskBranch' AND status = 'Pending' LIMIT 1`).Scan(&payload)
	if !strings.Contains(payload, `"convoy_id":`+itoa(eligibleCID)) {
		t.Errorf("queued task targets wrong convoy: %q (wanted id=%d)", payload, eligibleCID)
	}
}

// TestBackfillMissingAskBranches_QueuesForPartialMultiRepoConvoy verifies the
// multi-repo backfill path: a convoy with repos [api, monolith] where api has
// a ConvoyAskBranch but monolith doesn't must be queued for backfill.
// Regression test for the original NOT EXISTS gap where any ask-branch on the
// convoy suppressed the queue.
func TestBackfillMissingAskBranches_QueuesForPartialMultiRepoConvoy(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	cid, _ := store.CreateConvoy(db, "[1] partial")
	_, _ = store.AddConvoyTask(db, 0, "api", "t1", cid, 0, "Pending")
	_, _ = store.AddConvoyTask(db, 0, "monolith", "t2", cid, 0, "Pending")
	// Only one repo has an ask-branch; the other is missing.
	_ = store.UpsertConvoyAskBranch(db, cid, "api", "force/ask-1-partial", "sha")

	backfillMissingAskBranches(db, testLogger{})

	var queued int
	db.QueryRow(`SELECT COUNT(*) FROM BountyBoard WHERE type = 'CreateAskBranch' AND status = 'Pending'`).Scan(&queued)
	if queued != 1 {
		t.Errorf("partial-branch multi-repo convoy must be queued for backfill, got %d", queued)
	}
}

func TestBackfillMissingAskBranches_DoesNotDuplicate(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	cid, _ := store.CreateConvoy(db, "[1] eligible")
	_, _ = store.AddConvoyTask(db, 0, "api", "t", cid, 0, "Pending")

	backfillMissingAskBranches(db, testLogger{})
	backfillMissingAskBranches(db, testLogger{})

	var queued int
	db.QueryRow(`SELECT COUNT(*) FROM BountyBoard WHERE type = 'CreateAskBranch' AND status = 'Pending'`).Scan(&queued)
	if queued != 1 {
		t.Errorf("second run must not duplicate, got %d", queued)
	}
}

func itoa(i int) string {
	// tiny helper to avoid pulling strconv
	if i == 0 {
		return "0"
	}
	neg := i < 0
	if neg {
		i = -i
	}
	var digits [20]byte
	n := 0
	for i > 0 {
		digits[n] = byte('0' + i%10)
		n++
		i /= 10
	}
	var b strings.Builder
	if neg {
		b.WriteByte('-')
	}
	for j := n - 1; j >= 0; j-- {
		b.WriteByte(digits[j])
	}
	return b.String()
}

// TestQueueCreateAskBranch_BlocksCodeEditTasksUntilComplete verifies that
// QueueCreateAskBranch wires TaskDependencies so CodeEdit tasks in the same
// convoy cannot be claimed by an astromech before Pilot finishes. This is the
// regression test for the race condition where task-206 merged to main because
// the Council ran before the ask-branch existed.
func TestQueueCreateAskBranch_BlocksCodeEditTasksUntilComplete(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	cid, _ := store.CreateConvoy(db, "[99] test race")
	ceID, _ := store.AddConvoyTask(db, 0, "api", "do work", cid, 0, "Pending")

	pilotID, err := QueueCreateAskBranch(db, cid)
	if err != nil {
		t.Fatal(err)
	}

	// CodeEdit task must NOT be claimable while the Pilot task is Pending.
	if b, ok := store.ClaimBounty(db, "CodeEdit", "R2-D2"); ok {
		t.Errorf("CodeEdit task %d was claimable before CreateAskBranch completed", b.ID)
	}

	// Complete the Pilot task — dependency edge should clear.
	store.UpdateBountyStatus(db, pilotID, "Completed")
	store.UnblockDependentsOf(db, pilotID)

	// Now the CodeEdit task must be claimable.
	b, ok := store.ClaimBounty(db, "CodeEdit", "R2-D2")
	if !ok {
		t.Fatal("CodeEdit task should be claimable after CreateAskBranch completed")
	}
	if b.ID != ceID {
		t.Errorf("claimed wrong task: got %d, want %d", b.ID, ceID)
	}
}
