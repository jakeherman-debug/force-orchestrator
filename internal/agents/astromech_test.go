package agents

import (
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"strings"
	"testing"

	"force-orchestrator/internal/store"
)

// ── nextReviewStatus ──────────────────────────────────────────────────────────

func TestNextReviewStatus_NoConvoy(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	// convoyID=0 → not coordinated → straight to council
	status := nextReviewStatus(db, 0)
	if status != "AwaitingCouncilReview" {
		t.Errorf("expected AwaitingCouncilReview, got %q", status)
	}
}

func TestNextReviewStatus_UncoordinatedConvoy(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	id, _ := store.CreateConvoy(db, "uncoordinated")
	status := nextReviewStatus(db, id)
	if status != "AwaitingCouncilReview" {
		t.Errorf("expected AwaitingCouncilReview for uncoordinated convoy, got %q", status)
	}
}

func TestNextReviewStatus_CoordinatedConvoy(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	id, _ := store.CreateConvoy(db, "coordinated-convoy")
	store.SetConvoyCoordinated(db, id)

	status := nextReviewStatus(db, id)
	if status != "AwaitingCaptainReview" {
		t.Errorf("expected AwaitingCaptainReview for coordinated convoy, got %q", status)
	}
}

// ── NewLogger ─────────────────────────────────────────────────────────────────

func TestNewLogger_CreatesLogger(t *testing.T) {
	dir := t.TempDir()
	orig, _ := os.Getwd()
	os.Chdir(dir)
	defer os.Chdir(orig)

	logger := NewLogger("TestAgent")
	if logger == nil {
		t.Fatal("expected non-nil logger")
	}
	// Should write to fleet.log (or stderr if file creation fails)
	logger.Printf("test message from %s", "TestAgent")

	// fleet.log should exist
	if _, statErr := os.Stat("fleet.log"); statErr != nil {
		t.Error("expected fleet.log to be created by NewLogger")
	}
}

func TestNewLogger_FallbackToStderr(t *testing.T) {
	orig, _ := os.Getwd()
	// In /, fleet.log creation will succeed on macOS for the current user...
	// Instead just verify NewLogger doesn't panic
	os.Chdir("/")
	defer os.Chdir(orig)

	logger := NewLogger("TestAgent")
	if logger == nil {
		t.Error("expected non-nil logger even on error")
	}
}

// ── buildInboxContext ─────────────────────────────────────────────────────────

func TestBuildInboxContext_Empty(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	logger := log.New(io.Discard, "", 0)
	result := buildInboxContext(db, "R2-D2", "astromech", 0, logger)
	if result != "" {
		t.Errorf("expected empty context with no mail, got %q", result)
	}
}

func TestBuildInboxContext_WithMail(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	store.SendMail(db, "operator", "astromech", "directive title", "use tabs", 0, store.MailTypeDirective)
	store.SendMail(db, "council", "R2-D2", "feedback title", "fix the tests", 5, store.MailTypeFeedback)

	logger := log.New(io.Discard, "", 0)
	result := buildInboxContext(db, "R2-D2", "astromech", 5, logger)
	if result == "" {
		t.Error("expected non-empty context with mail")
	}
	if !strings.Contains(result, "STANDING ORDERS") {
		t.Error("expected STANDING ORDERS section for directives")
	}
	if !strings.Contains(result, "PRIOR FEEDBACK") {
		t.Error("expected PRIOR FEEDBACK section for feedback mail")
	}
}

func TestBuildInboxContext_AllMailTypes(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	store.SendMail(db, "operator", "R2-D2", "alert", "system overloaded", 0, store.MailTypeAlert)
	store.SendMail(db, "operator", "R2-D2", "info", "FYI message", 0, store.MailTypeInfo)
	store.SendMail(db, "inquisitor", "R2-D2", "fix", "fixed git repo", 0, store.MailTypeRemediation)

	logger := log.New(io.Discard, "", 0)
	result := buildInboxContext(db, "R2-D2", "astromech", 0, logger)
	if !strings.Contains(result, "ALERTS") {
		t.Error("expected ALERTS section")
	}
	if !strings.Contains(result, "FLEET MESSAGES") {
		t.Error("expected FLEET MESSAGES section")
	}
	if !strings.Contains(result, "REMEDIATION") {
		t.Error("expected REMEDIATION section")
	}
}

func TestBuildInboxContext_WithTaskMail(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	id := store.AddBounty(db, 0, "CodeEdit", "task with mail")
	store.SendMail(db, "operator", "R2-D2", "hint", "do this first", id, store.MailTypeInfo)

	logger := log.New(io.Discard, "", 0)
	ctx := buildInboxContext(db, "R2-D2", "astromech", id, logger)
	if !strings.Contains(ctx, "hint") {
		t.Errorf("expected inbox context to contain mail subject, got: %s", ctx)
	}
}

// ── permanentInfraFail ────────────────────────────────────────────────────────

func TestPermanentInfraFail(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	id := store.AddBounty(db, 0, "CodeEdit", "task that infra-failed")
	db.Exec(`UPDATE BountyBoard SET target_repo = 'api' WHERE id = ?`, id)

	bounty := &store.Bounty{ID: id, TargetRepo: "api", Payload: "task that infra-failed"}
	logger := log.New(io.Discard, "", 0)

	permanentInfraFail(db, logger, "sess1", "R2-D2", bounty, "git repo not found")

	// Task should be failed
	b, _ := store.GetBounty(db, id)
	if b.Status != "Failed" {
		t.Errorf("expected task to be Failed, got %q", b.Status)
	}

	// A remediation Feature task should be spawned
	var remCount int
	db.QueryRow(`SELECT COUNT(*) FROM BountyBoard WHERE type = 'CodeEdit' AND parent_id = ?`, id).Scan(&remCount)
	if remCount != 1 {
		t.Errorf("expected 1 remediation task, got %d", remCount)
	}

	// Operator should get mail
	mails := store.ListMail(db, "operator")
	if len(mails) == 0 {
		t.Error("expected mail to operator about infra failure")
	}

	// Audit log should have an entry
	entries := store.ListAuditLog(db, 10)
	found := false
	for _, e := range entries {
		if e.Action == "infra-fail" && e.TaskID == id {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected audit log entry for infra-fail")
	}
}

// ── runAstromechTask ──────────────────────────────────────────────────────────

func TestRunAstromechTask_UnknownRepo(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	id := store.AddBounty(db, 0, "CodeEdit", "fix bug")
	db.Exec(`UPDATE BountyBoard SET status = 'Locked', owner = 'R2-D2', locked_at = datetime('now'), target_repo = 'ghost' WHERE id = ?`, id)
	b, _ := store.GetBounty(db, id)

	withStubCLIRunner(t, "", nil)
	logger := log.New(io.Discard, "", 0)
	runAstromechTask(db, "R2-D2", b, logger)

	b, _ = store.GetBounty(db, id)
	if b.Status != "Failed" {
		t.Errorf("expected Failed for unknown repo, got %q", b.Status)
	}
}

func TestRunAstromechTask_DoneSignal(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not found")
	}
	repoDir := initTestRepo(t)

	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	store.AddRepo(db, "myrepo", repoDir, "test")

	id := store.AddBounty(db, 0, "CodeEdit", "fix bug")
	db.Exec(`UPDATE BountyBoard SET status = 'Locked', owner = 'R2-D2', locked_at = datetime('now'), target_repo = 'myrepo' WHERE id = ?`, id)
	b, _ := store.GetBounty(db, id)

	withStubCLIRunner(t, "[DONE]", nil)
	logger := log.New(io.Discard, "", 0)
	runAstromechTask(db, "R2-D2", b, logger)

	b, _ = store.GetBounty(db, id)
	if b.Status != "AwaitingCouncilReview" {
		t.Errorf("expected AwaitingCouncilReview after [DONE], got %q", b.Status)
	}
}

func TestRunAstromechTask_ShardNeeded(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not found")
	}
	repoDir := initTestRepo(t)

	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	store.AddRepo(db, "myrepo", repoDir, "test")

	id := store.AddBounty(db, 0, "CodeEdit", "huge feature")
	db.Exec(`UPDATE BountyBoard SET status = 'Locked', owner = 'R2-D2', locked_at = datetime('now'), target_repo = 'myrepo' WHERE id = ?`, id)
	b, _ := store.GetBounty(db, id)

	withStubCLIRunner(t, "[SHARD_NEEDED]", nil)
	logger := log.New(io.Discard, "", 0)
	runAstromechTask(db, "R2-D2", b, logger)

	// Original task should be Completed; a Decompose task should be queued
	b, _ = store.GetBounty(db, id)
	if b.Status != "Completed" {
		t.Errorf("expected Completed after [SHARD_NEEDED], got %q", b.Status)
	}
	var count int
	db.QueryRow(`SELECT COUNT(*) FROM BountyBoard WHERE parent_id = ? AND type = 'Decompose'`, id).Scan(&count)
	if count != 1 {
		t.Errorf("expected 1 Decompose child task, got %d", count)
	}
}

func TestRunAstromechTask_RateLimit(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not found")
	}
	repoDir := initTestRepo(t)

	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	store.AddRepo(db, "myrepo", repoDir, "test")

	id := store.AddBounty(db, 0, "CodeEdit", "fix bug")
	db.Exec(`UPDATE BountyBoard SET status = 'Locked', owner = 'R2-D2', locked_at = datetime('now'), target_repo = 'myrepo' WHERE id = ?`, id)
	b, _ := store.GetBounty(db, id)

	// Rate-limit error: message triggers IsRateLimitError
	withStubCLIRunner(t, "rate limit exceeded", fmt.Errorf("claude CLI failed: 429"))
	rateLimitRetries.Delete("R2-D2")
	logger := log.New(io.Discard, "", 0)
	runAstromechTask(db, "R2-D2", b, logger)

	b, _ = store.GetBounty(db, id)
	if b.Status != "Pending" {
		t.Errorf("expected Pending after rate limit, got %q", b.Status)
	}
	rateLimitRetries.Delete("R2-D2") // cleanup
}

func TestRunAstromechTask_Escalated(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not found")
	}
	repoDir := initTestRepo(t)

	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	store.AddRepo(db, "myrepo", repoDir, "test")

	id := store.AddBounty(db, 0, "CodeEdit", "fix bug")
	db.Exec(`UPDATE BountyBoard SET status = 'Locked', owner = 'R2-D2', locked_at = datetime('now'), target_repo = 'myrepo' WHERE id = ?`, id)
	b, _ := store.GetBounty(db, id)

	withStubCLIRunner(t, "[ESCALATED:HIGH:Cannot determine correct API version]", nil)
	logger := log.New(io.Discard, "", 0)
	runAstromechTask(db, "R2-D2", b, logger)

	var count int
	db.QueryRow(`SELECT COUNT(*) FROM Escalations WHERE task_id = ?`, id).Scan(&count)
	if count != 1 {
		t.Errorf("expected escalation created, got %d", count)
	}
}

func TestRunAstromechTask_CLIError(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not found")
	}
	repoDir := initTestRepo(t)

	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	store.AddRepo(db, "myrepo", repoDir, "test")

	id := store.AddBounty(db, 0, "CodeEdit", "fix bug")
	db.Exec(`UPDATE BountyBoard SET status = 'Locked', owner = 'R2-D2', locked_at = datetime('now'), target_repo = 'myrepo' WHERE id = ?`, id)
	b, _ := store.GetBounty(db, id)

	withStubCLIRunner(t, "some output", fmt.Errorf("claude CLI failed: exit 1"))
	rateLimitRetries.Delete("R2-D2")
	logger := log.New(io.Discard, "", 0)
	runAstromechTask(db, "R2-D2", b, logger)

	b, _ = store.GetBounty(db, id)
	if b.Status != "Pending" {
		t.Errorf("expected Pending after CLI error (infra failure), got %q", b.Status)
	}
}

func TestRunAstromechTask_Timeout(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not found")
	}
	repoDir := initTestRepo(t)

	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	store.AddRepo(db, "myrepo", repoDir, "test")

	id := store.AddBounty(db, 0, "CodeEdit", "fix bug")
	db.Exec(`UPDATE BountyBoard SET status = 'Locked', owner = 'R2-D2', locked_at = datetime('now'), target_repo = 'myrepo' WHERE id = ?`, id)
	b, _ := store.GetBounty(db, id)

	withStubCLIRunner(t, "", fmt.Errorf("claude CLI timed out after 15m0s"))
	rateLimitRetries.Delete("R2-D2")
	logger := log.New(io.Discard, "", 0)
	runAstromechTask(db, "R2-D2", b, logger)

	b, _ = store.GetBounty(db, id)
	if b.Status != "Pending" {
		t.Errorf("expected Pending after timeout (infra failure), got %q", b.Status)
	}
}

// ── RunTaskForeground ──────────────────────────────────────────────────────────

func TestRunTaskForeground_DoneSignal(t *testing.T) {
	repoDir := initTestRepo(t)

	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	store.AddRepo(db, "myrepo", repoDir, "test")

	id := store.AddCodeEditTask(db, "myrepo", "fix bug", 0, 0, 0)

	withClaudeStub(t, map[string]string{"CLAUDE_STUB_OUTPUT": "[DONE]"})

	captureOutput(func() { RunTaskForeground(db, id) })

	b, _ := store.GetBounty(db, id)
	if b.Status != "AwaitingCouncilReview" {
		t.Errorf("expected AwaitingCouncilReview after [DONE], got %q", b.Status)
	}
	// Token history should be recorded
	hist := store.GetTaskHistory(db, id)
	if len(hist) == 0 {
		t.Error("expected task history entry after [DONE]")
	}
}

func TestRunTaskForeground_ShardNeeded(t *testing.T) {
	repoDir := initTestRepo(t)

	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	store.AddRepo(db, "myrepo", repoDir, "test")

	id := store.AddCodeEditTask(db, "myrepo", "massive feature", 0, 0, 0)

	withClaudeStub(t, map[string]string{"CLAUDE_STUB_OUTPUT": "[SHARD_NEEDED]"})

	captureOutput(func() { RunTaskForeground(db, id) })

	b, _ := store.GetBounty(db, id)
	if b.Status != "Completed" {
		t.Errorf("expected Completed after [SHARD_NEEDED], got %q", b.Status)
	}
	var count int
	db.QueryRow(`SELECT COUNT(*) FROM BountyBoard WHERE parent_id = ? AND type = 'Decompose'`, id).Scan(&count)
	if count != 1 {
		t.Errorf("expected 1 Decompose child task, got %d", count)
	}
}

func TestRunTaskForeground_Escalated(t *testing.T) {
	repoDir := initTestRepo(t)

	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	store.AddRepo(db, "myrepo", repoDir, "test")

	id := store.AddCodeEditTask(db, "myrepo", "ambiguous task", 0, 0, 0)

	withClaudeStub(t, map[string]string{
		"CLAUDE_STUB_OUTPUT": "[ESCALATED:HIGH:Cannot determine correct API version]",
	})

	captureOutput(func() { RunTaskForeground(db, id) })

	var count int
	db.QueryRow(`SELECT COUNT(*) FROM Escalations WHERE task_id = ?`, id).Scan(&count)
	if count != 1 {
		t.Errorf("expected 1 escalation, got %d", count)
	}
}

func TestRunTaskForeground_CheckpointRecorded(t *testing.T) {
	repoDir := initTestRepo(t)

	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	store.AddRepo(db, "myrepo", repoDir, "test")

	id := store.AddCodeEditTask(db, "myrepo", "long task", 0, 0, 0)

	// Emits a checkpoint then [DONE] so the function returns cleanly
	withClaudeStub(t, map[string]string{
		"CLAUDE_STUB_OUTPUT": "[CHECKPOINT: schema_written]\n[DONE]",
	})

	captureOutput(func() { RunTaskForeground(db, id) })

	b, _ := store.GetBounty(db, id)
	if b.Checkpoint != "schema_written" {
		t.Errorf("expected checkpoint 'schema_written', got %q", b.Checkpoint)
	}
}

func TestRunTaskForeground_NoCommits_ReturnsToPending(t *testing.T) {
	repoDir := initTestRepo(t)

	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	store.AddRepo(db, "myrepo", repoDir, "test")

	id := store.AddCodeEditTask(db, "myrepo", "fix bug", 0, 0, 0)

	// Claude exits cleanly but makes no commits and sends no signals
	withClaudeStub(t, map[string]string{"CLAUDE_STUB_OUTPUT": "nothing done"})

	captureOutput(func() { RunTaskForeground(db, id) })

	b, _ := store.GetBounty(db, id)
	if b.Status != "Pending" {
		t.Errorf("expected Pending when no commits, got %q", b.Status)
	}
}
