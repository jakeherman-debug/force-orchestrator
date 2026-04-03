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

// ── BranchAgentName ───────────────────────────────────────────────────────────

func TestBranchAgentName(t *testing.T) {
	tests := []struct {
		branch string
		want   string
	}{
		{"agent/R2-D2/task-42", "R2-D2"},
		{"agent/BB-8/task-7", "BB-8"},
		{"agent/task-42", ""},  // legacy format — no agent name
		{"main", ""},
		{"", ""},
	}
	for _, tt := range tests {
		got := BranchAgentName(tt.branch)
		if got != tt.want {
			t.Errorf("BranchAgentName(%q) = %q, want %q", tt.branch, got, tt.want)
		}
	}
}

func TestBranchAgentName_Valid(t *testing.T) {
	got := BranchAgentName("agent/R2-D2/task-42")
	if got != "R2-D2" {
		t.Errorf("expected 'R2-D2', got %q", got)
	}
}

func TestBranchAgentName_InvalidPrefix(t *testing.T) {
	got := BranchAgentName("main")
	if got != "" {
		t.Errorf("expected '' for non-agent branch, got %q", got)
	}
}

// ── runCouncilTask ────────────────────────────────────────────────────────────

func TestRunCouncilTask_UnknownRepo(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	id := store.AddBounty(db, 0, "CodeEdit", "fix bug")
	db.Exec(`UPDATE BountyBoard SET status = 'AwaitingCouncilReview', target_repo = 'ghost' WHERE id = ?`, id)
	b, _ := store.GetBounty(db, id)

	withStubCLIRunner(t, "", nil)
	logger := log.New(io.Discard, "", 0)
	runCouncilTask(db, "Council-Yoda", b, logger)

	b, _ = store.GetBounty(db, id)
	if b.Status != "Failed" {
		t.Errorf("expected Failed for unknown repo, got %q", b.Status)
	}
}

func TestRunCouncilTask_CLIError(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not found")
	}
	repoDir := initTestRepo(t)
	branchName := setupBranchWithCommit(t, repoDir, "agent/R2-D2/task-99")

	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	store.AddRepo(db, "myrepo", repoDir, "test")

	id := store.AddBounty(db, 0, "CodeEdit", "fix bug")
	db.Exec(`UPDATE BountyBoard SET status = 'AwaitingCouncilReview', target_repo = 'myrepo', branch_name = ? WHERE id = ?`, branchName, id)
	b, _ := store.GetBounty(db, id)

	withStubCLIRunner(t, "", fmt.Errorf("claude CLI failed: network error"))
	logger := log.New(io.Discard, "", 0)
	runCouncilTask(db, "Council-Yoda", b, logger)

	b, _ = store.GetBounty(db, id)
	if b.Status != "AwaitingCouncilReview" {
		t.Errorf("expected AwaitingCouncilReview after CLI error, got %q", b.Status)
	}
}

func TestRunCouncilTask_JSONError(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not found")
	}
	repoDir := initTestRepo(t)
	branchName := setupBranchWithCommit(t, repoDir, "agent/R2-D2/task-100")

	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	store.AddRepo(db, "myrepo", repoDir, "test")

	id := store.AddBounty(db, 0, "CodeEdit", "fix bug")
	db.Exec(`UPDATE BountyBoard SET status = 'AwaitingCouncilReview', target_repo = 'myrepo', branch_name = ? WHERE id = ?`, branchName, id)
	b, _ := store.GetBounty(db, id)

	withStubCLIRunner(t, "This is not JSON at all", nil)
	logger := log.New(io.Discard, "", 0)
	runCouncilTask(db, "Council-Yoda", b, logger)

	b, _ = store.GetBounty(db, id)
	if b.Status != "AwaitingCouncilReview" {
		t.Errorf("expected AwaitingCouncilReview after JSON error, got %q", b.Status)
	}
}

func TestRunCouncilTask_Rejected_RetryRemaining(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not found")
	}
	repoDir := initTestRepo(t)
	branchName := setupBranchWithCommit(t, repoDir, "agent/R2-D2/task-101")

	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	store.AddRepo(db, "myrepo", repoDir, "test")

	id := store.AddBounty(db, 0, "CodeEdit", "fix bug")
	db.Exec(`UPDATE BountyBoard SET status = 'AwaitingCouncilReview', target_repo = 'myrepo', branch_name = ? WHERE id = ?`, branchName, id)
	b, _ := store.GetBounty(db, id)

	ruling := `{"approved":false,"feedback":"missing test coverage"}`
	withStubCLIRunner(t, ruling, nil)
	logger := log.New(io.Discard, "", 0)
	runCouncilTask(db, "Council-Yoda", b, logger)

	b, _ = store.GetBounty(db, id)
	if b.Status != "Pending" {
		t.Errorf("expected Pending after rejection with retries remaining, got %q", b.Status)
	}
	if !strings.Contains(b.Payload, "FEEDBACK") {
		t.Errorf("expected feedback appended to payload, got %q", b.Payload)
	}
}

func TestRunCouncilTask_Approved(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not found")
	}
	repoDir := initTestRepo(t)
	branchName := setupBranchWithCommit(t, repoDir, "agent/R2-D2/task-200")

	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	store.AddRepo(db, "myrepo", repoDir, "test")

	id := store.AddBounty(db, 0, "CodeEdit", "fix bug")
	db.Exec(`UPDATE BountyBoard SET status = 'AwaitingCouncilReview', target_repo = 'myrepo', branch_name = ? WHERE id = ?`, branchName, id)
	b, _ := store.GetBounty(db, id)

	ruling := `{"approved":true,"feedback":""}`
	withStubCLIRunner(t, ruling, nil)
	logger := log.New(io.Discard, "", 0)
	runCouncilTask(db, "Council-Yoda", b, logger)

	b, _ = store.GetBounty(db, id)
	if b.Status != "Completed" {
		t.Errorf("expected Completed after approval, got %q", b.Status)
	}

	// Task history must record Completed outcome after the merge (not before)
	var histCount int
	db.QueryRow(`SELECT COUNT(*) FROM TaskHistory WHERE task_id = ? AND outcome = 'Completed'`, id).Scan(&histCount)
	if histCount == 0 {
		t.Error("expected TaskHistory entry with outcome=Completed after approval")
	}

	// Fleet memory must have a success entry
	var memCount int
	db.QueryRow(`SELECT COUNT(*) FROM FleetMemory WHERE task_id = ? AND outcome = 'success'`, id).Scan(&memCount)
	if memCount == 0 {
		t.Error("expected FleetMemory success entry after approval")
	}

	// AuditLog must record the council-approve action
	var auditCount int
	db.QueryRow(`SELECT COUNT(*) FROM AuditLog WHERE action = 'council-approve' AND task_id = ?`, id).Scan(&auditCount)
	if auditCount == 0 {
		t.Error("expected AuditLog entry for council-approve")
	}
}

func TestRunCouncilTask_Rejected_MaxRetries(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not found")
	}
	repoDir := initTestRepo(t)
	branchName := setupBranchWithCommit(t, repoDir, "agent/R2-D2/task-102")

	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	store.AddRepo(db, "myrepo", repoDir, "test")

	id := store.AddBounty(db, 0, "CodeEdit", "fix bug")
	// Simulate already at max retries
	for i := 0; i < MaxRetries-1; i++ {
		store.IncrementRetryCount(db, id)
	}
	db.Exec(`UPDATE BountyBoard SET status = 'AwaitingCouncilReview', target_repo = 'myrepo', branch_name = ? WHERE id = ?`, branchName, id)
	b, _ := store.GetBounty(db, id)

	ruling := `{"approved":false,"feedback":"still broken"}`
	withStubCLIRunner(t, ruling, nil)
	logger := log.New(io.Discard, "", 0)
	runCouncilTask(db, "Council-Yoda", b, logger)

	b, _ = store.GetBounty(db, id)
	if b.Status != "Failed" {
		t.Errorf("expected Failed at max retries, got %q", b.Status)
	}
}
