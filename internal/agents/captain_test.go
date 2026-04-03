package agents

import (
	"fmt"
	"io"
	"log"
	"os/exec"
	"strings"
	"testing"

	"force-orchestrator/internal/store"
)

// ── isKnownRepo ───────────────────────────────────────────────────────────────

func TestIsKnownRepo_Known(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	store.AddRepo(db, "my-api", "/tmp/api", "the API")
	if !isKnownRepo(db, "my-api") {
		t.Error("expected 'my-api' to be known")
	}
}

func TestIsKnownRepo_Unknown(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	if isKnownRepo(db, "nonexistent-repo") {
		t.Error("expected 'nonexistent-repo' to not be known")
	}
}

func TestIsKnownRepo_Registered(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	store.AddRepo(db, "myrepo", "/tmp/myrepo", "my repo")

	if !isKnownRepo(db, "myrepo") {
		t.Error("expected isKnownRepo to return true for registered repo")
	}
	if isKnownRepo(db, "unknown") {
		t.Error("expected isKnownRepo to return false for unregistered repo")
	}
}

// ── buildConvoyContext ────────────────────────────────────────────────────────

func TestBuildConvoyContext_NoConvoy(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	b := &store.Bounty{ID: 1, ConvoyID: 0}
	result := buildConvoyContext(db, b)
	if result != "" {
		t.Errorf("expected empty context for task with no convoy, got %q", result)
	}
}

func TestBuildConvoyContext_WithConvoy(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	convoyID, _ := store.CreateConvoy(db, "feature-convoy")

	// Completed task
	db.Exec(`INSERT INTO BountyBoard (parent_id, target_repo, type, status, payload, convoy_id)
		VALUES (0, 'api', 'CodeEdit', 'Completed', 'add endpoint', ?)`, convoyID)
	// Pending task
	pendingID := store.AddBounty(db, 0, "CodeEdit", "add tests")
	db.Exec(`UPDATE BountyBoard SET convoy_id = ? WHERE id = ?`, convoyID, pendingID)

	// Current task under review
	currentID := store.AddBounty(db, 0, "CodeEdit", "add docs")
	db.Exec(`UPDATE BountyBoard SET convoy_id = ? WHERE id = ?`, convoyID, currentID)
	b := &store.Bounty{ID: currentID, ConvoyID: convoyID, TargetRepo: "api", Payload: "add docs task"}

	result := buildConvoyContext(db, b)
	if result == "" {
		t.Error("expected non-empty convoy context")
	}
	if !strings.Contains(result, "CONVOY CONTEXT") {
		t.Error("expected CONVOY CONTEXT header")
	}
	if !strings.Contains(result, "feature-convoy") {
		t.Error("expected convoy name in context")
	}
}

func TestBuildConvoyContext_WithBlockedTask(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	convoyID, _ := store.CreateConvoy(db, "blocked-convoy")

	// Current task under review (Locked)
	var currentID int
	db.QueryRow(`INSERT INTO BountyBoard (parent_id, target_repo, type, status, payload, convoy_id)
		VALUES (0, 'repo', 'CodeEdit', 'Locked', 'current task', ?) RETURNING id`, convoyID).Scan(&currentID)

	// Pending task that depends on the current (non-completed) task → activeDep > 0
	var blockedID int
	db.QueryRow(`INSERT INTO BountyBoard (parent_id, target_repo, type, status, payload, convoy_id)
		VALUES (0, 'repo', 'CodeEdit', 'Pending', 'blocked task', ?) RETURNING id`, convoyID).Scan(&blockedID)
	db.Exec(`INSERT INTO TaskDependencies (task_id, depends_on) VALUES (?, ?)`, blockedID, currentID)

	b := &store.Bounty{ID: currentID, ConvoyID: convoyID, TargetRepo: "repo", Payload: "current task"}
	ctx := buildConvoyContext(db, b)
	if !strings.Contains(ctx, "blocked by") {
		t.Errorf("expected 'blocked by' in convoy context for blocked task, got: %s", ctx)
	}
}

func TestBuildConvoyContext_MultilinePayload(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	convoyID, _ := store.CreateConvoy(db, "multiline-convoy")

	// Completed task with multiline payload — buildConvoyContext takes only the first line
	db.Exec(`INSERT INTO BountyBoard (parent_id, target_repo, type, status, payload, convoy_id)
		VALUES (0, 'repo', 'CodeEdit', 'Completed', ?, ?)`, "first line\nsecond line\nthird line", convoyID)

	b := &store.Bounty{ID: 999, ConvoyID: convoyID, TargetRepo: "repo", Payload: "title\ndetails"}
	ctx := buildConvoyContext(db, b)
	if ctx == "" {
		t.Error("expected non-empty convoy context")
	}
	if strings.Contains(ctx, "second line") {
		t.Error("expected only first line of multiline payload, got second line in context")
	}
}

// ── runCaptainTask ────────────────────────────────────────────────────────────

func TestRunCaptainTask_UnknownRepo(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	id := store.AddBounty(db, 0, "CodeEdit", "fix bug")
	db.Exec(`UPDATE BountyBoard SET status = 'AwaitingCaptainReview', target_repo = 'ghost' WHERE id = ?`, id)
	b, _ := store.GetBounty(db, id)

	withStubCLIRunner(t, "", nil)
	logger := log.New(io.Discard, "", 0)
	runCaptainTask(db, "Captain-Rex", b, logger)

	b, _ = store.GetBounty(db, id)
	if b.Status != "Failed" {
		t.Errorf("expected Failed for unknown repo, got %q", b.Status)
	}
}

func TestRunCaptainTask_CLIError(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not found")
	}
	repoDir := initTestRepo(t)
	branchName := setupBranchWithCommit(t, repoDir, "agent/R2-D2/task-200")

	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	store.AddRepo(db, "myrepo", repoDir, "test")
	convoyID, _ := store.CreateConvoy(db, "test convoy")

	id := store.AddBounty(db, 0, "CodeEdit", "fix bug")
	db.Exec(`UPDATE BountyBoard SET status = 'AwaitingCaptainReview', target_repo = 'myrepo', branch_name = ?, convoy_id = ? WHERE id = ?`,
		branchName, convoyID, id)
	b, _ := store.GetBounty(db, id)

	withStubCLIRunner(t, "", fmt.Errorf("claude CLI failed: timeout"))
	logger := log.New(io.Discard, "", 0)
	runCaptainTask(db, "Captain-Rex", b, logger)

	b, _ = store.GetBounty(db, id)
	if b.Status != "AwaitingCaptainReview" {
		t.Errorf("expected AwaitingCaptainReview after CLI error, got %q", b.Status)
	}
}

func TestRunCaptainTask_Approve(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not found")
	}
	repoDir := initTestRepo(t)
	branchName := setupBranchWithCommit(t, repoDir, "agent/R2-D2/task-201")

	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	store.AddRepo(db, "myrepo", repoDir, "test")
	convoyID, _ := store.CreateConvoy(db, "test convoy 2")

	id := store.AddBounty(db, 0, "CodeEdit", "fix bug")
	db.Exec(`UPDATE BountyBoard SET status = 'AwaitingCaptainReview', target_repo = 'myrepo', branch_name = ?, convoy_id = ? WHERE id = ?`,
		branchName, convoyID, id)
	b, _ := store.GetBounty(db, id)

	ruling := `{"decision":"approve","feedback":"","task_updates":[],"new_tasks":[]}`
	withStubCLIRunner(t, ruling, nil)
	logger := log.New(io.Discard, "", 0)
	runCaptainTask(db, "Captain-Rex", b, logger)

	b, _ = store.GetBounty(db, id)
	if b.Status != "AwaitingCouncilReview" {
		t.Errorf("expected AwaitingCouncilReview after captain approve, got %q", b.Status)
	}
}

func TestRunCaptainTask_Reject_RetryRemaining(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not found")
	}
	repoDir := initTestRepo(t)
	branchName := setupBranchWithCommit(t, repoDir, "agent/R2-D2/task-202")

	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	store.AddRepo(db, "myrepo", repoDir, "test")
	convoyID, _ := store.CreateConvoy(db, "test convoy 3")

	id := store.AddBounty(db, 0, "CodeEdit", "fix bug")
	db.Exec(`UPDATE BountyBoard SET status = 'AwaitingCaptainReview', target_repo = 'myrepo', branch_name = ?, convoy_id = ? WHERE id = ?`,
		branchName, convoyID, id)
	b, _ := store.GetBounty(db, id)

	ruling := `{"decision":"reject","feedback":"plan divergence","task_updates":[],"new_tasks":[]}`
	withStubCLIRunner(t, ruling, nil)
	logger := log.New(io.Discard, "", 0)
	runCaptainTask(db, "Captain-Rex", b, logger)

	b, _ = store.GetBounty(db, id)
	if b.Status != "Pending" {
		t.Errorf("expected Pending after captain reject with retries remaining, got %q", b.Status)
	}
}

func TestRunCaptainTask_Reject_MaxRetries(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not found")
	}
	repoDir := initTestRepo(t)
	branchName := setupBranchWithCommit(t, repoDir, "agent/R2-D2/task-203")

	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	store.AddRepo(db, "myrepo", repoDir, "test")
	convoyID, _ := store.CreateConvoy(db, "test convoy 4")

	id := store.AddBounty(db, 0, "CodeEdit", "fix bug")
	for i := 0; i < MaxRetries-1; i++ {
		store.IncrementRetryCount(db, id)
	}
	db.Exec(`UPDATE BountyBoard SET status = 'AwaitingCaptainReview', target_repo = 'myrepo', branch_name = ?, convoy_id = ? WHERE id = ?`,
		branchName, convoyID, id)
	b, _ := store.GetBounty(db, id)

	ruling := `{"decision":"reject","feedback":"still wrong","task_updates":[],"new_tasks":[]}`
	withStubCLIRunner(t, ruling, nil)
	logger := log.New(io.Discard, "", 0)
	runCaptainTask(db, "Captain-Rex", b, logger)

	b, _ = store.GetBounty(db, id)
	if b.Status != "Failed" {
		t.Errorf("expected Failed at max retries for captain, got %q", b.Status)
	}
}

func TestRunCaptainTask_Escalate(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not found")
	}
	repoDir := initTestRepo(t)
	branchName := setupBranchWithCommit(t, repoDir, "agent/R2-D2/task-204")

	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	store.AddRepo(db, "myrepo", repoDir, "test")
	convoyID, _ := store.CreateConvoy(db, "test convoy 5")

	id := store.AddBounty(db, 0, "CodeEdit", "fix bug")
	db.Exec(`UPDATE BountyBoard SET status = 'AwaitingCaptainReview', target_repo = 'myrepo', branch_name = ?, convoy_id = ? WHERE id = ?`,
		branchName, convoyID, id)
	b, _ := store.GetBounty(db, id)

	ruling := `{"decision":"escalate","feedback":"convoy plan is fundamentally broken","task_updates":[],"new_tasks":[]}`
	withStubCLIRunner(t, ruling, nil)
	logger := log.New(io.Discard, "", 0)
	runCaptainTask(db, "Captain-Rex", b, logger)

	var count int
	db.QueryRow(`SELECT COUNT(*) FROM Escalations WHERE task_id = ?`, id).Scan(&count)
	if count != 1 {
		t.Errorf("expected escalation created, got %d", count)
	}
}

func TestRunCaptainTask_UnknownDecision(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not found")
	}
	repoDir := initTestRepo(t)
	branchName := setupBranchWithCommit(t, repoDir, "agent/R2-D2/task-205")

	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	store.AddRepo(db, "myrepo", repoDir, "test")
	convoyID, _ := store.CreateConvoy(db, "test convoy 6")

	id := store.AddBounty(db, 0, "CodeEdit", "fix bug")
	db.Exec(`UPDATE BountyBoard SET status = 'AwaitingCaptainReview', target_repo = 'myrepo', branch_name = ?, convoy_id = ? WHERE id = ?`,
		branchName, convoyID, id)
	b, _ := store.GetBounty(db, id)

	ruling := `{"decision":"bogus","feedback":"","task_updates":[],"new_tasks":[]}`
	withStubCLIRunner(t, ruling, nil)
	logger := log.New(io.Discard, "", 0)
	runCaptainTask(db, "Captain-Rex", b, logger)

	b, _ = store.GetBounty(db, id)
	// Unknown decision defaults to approve
	if b.Status != "AwaitingCouncilReview" {
		t.Errorf("expected AwaitingCouncilReview for unknown decision, got %q", b.Status)
	}
}
