package agents

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"testing"

	"force-orchestrator/internal/store"
)

// ── runAuditorTask ────────────────────────────────────────────────────────────

func TestRunAuditorTask_CLIError(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	id := store.AddBounty(db, 0, "Audit", "audit the codebase")
	// Pre-fill to MaxInfraFailures-1 so the next failure permanently fails the task
	// without triggering the sleep in handleInfraFailure.
	for i := 0; i < MaxInfraFailures-1; i++ {
		store.IncrementInfraFailures(db, id)
	}
	b, _ := store.GetBounty(db, id)

	withStubCLIRunner(t, "some output", fmt.Errorf("claude CLI failed: exit 1"))
	logger := log.New(io.Discard, "", 0)
	runAuditorTask(db, "auditor-1", b, logger)

	b, _ = store.GetBounty(db, id)
	if b.Status != "Failed" {
		t.Errorf("expected Failed after CLI error at max retries, got %q", b.Status)
	}
}

func TestRunAuditorTask_JSONParseError(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	id := store.AddBounty(db, 0, "Audit", "audit the codebase")
	for i := 0; i < MaxInfraFailures-1; i++ {
		store.IncrementInfraFailures(db, id)
	}
	b, _ := store.GetBounty(db, id)

	withStubCLIRunner(t, "this is not valid json at all", nil)
	logger := log.New(io.Discard, "", 0)
	runAuditorTask(db, "auditor-1", b, logger)

	b, _ = store.GetBounty(db, id)
	if b.Status != "Failed" {
		t.Errorf("expected Failed after JSON parse error at max retries, got %q", b.Status)
	}
}

func TestRunAuditorTask_EmptyFindings(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	id := store.AddBounty(db, 0, "Audit", "audit the codebase")
	b, _ := store.GetBounty(db, id)

	out, _ := json.Marshal(auditReport{Summary: "no issues", Findings: []AuditFinding{}})
	withStubCLIRunner(t, string(out), nil)
	logger := log.New(io.Discard, "", 0)
	runAuditorTask(db, "auditor-1", b, logger)

	b, _ = store.GetBounty(db, id)
	if b.Status != "Completed" {
		t.Errorf("expected Completed for empty findings, got %q", b.Status)
	}

	// No convoy should be created for an empty audit.
	if convoys := store.ListConvoys(db); len(convoys) != 0 {
		t.Errorf("expected no convoy for empty findings, got %d", len(convoys))
	}

	// Operator should receive a summary mail.
	if mails := store.ListMail(db, "operator"); len(mails) == 0 {
		t.Error("expected mail to operator after empty-findings audit")
	}
}

func TestRunAuditorTask_FindingsWithDependencies(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	id := store.AddBounty(db, 0, "Audit", "audit the codebase")
	b, _ := store.GetBounty(db, id)

	// Finding 2 is blocked by Finding 1 — requires wiring a TaskDependency.
	report := auditReport{
		Summary: "two findings",
		Findings: []AuditFinding{
			{ID: 1, Severity: "HIGH", Title: "Fix auth", Repo: "myrepo",
				Location: "auth.go:1", Description: "auth broken", SuggestedFix: "fix it", BlockedBy: []int{}},
			{ID: 2, Severity: "LOW", Title: "Fix style", Repo: "myrepo",
				Location: "style.go:1", Description: "style bad", SuggestedFix: "fix it", BlockedBy: []int{1}},
		},
	}
	out, _ := json.Marshal(report)
	withStubCLIRunner(t, string(out), nil)
	logger := log.New(io.Discard, "", 0)
	runAuditorTask(db, "auditor-1", b, logger)

	// Audit task should be Completed.
	b, _ = store.GetBounty(db, id)
	if b.Status != "Completed" {
		t.Errorf("expected audit task Completed, got %q", b.Status)
	}

	// Two Planned CodeEdit child tasks should exist.
	var taskCount int
	db.QueryRow(`SELECT COUNT(*) FROM BountyBoard WHERE parent_id = ? AND type = 'CodeEdit'`, id).Scan(&taskCount)
	if taskCount != 2 {
		t.Fatalf("expected 2 finding tasks, got %d", taskCount)
	}

	// Retrieve task IDs in insertion order.
	rows, err := db.Query(`SELECT id FROM BountyBoard WHERE parent_id = ? AND type = 'CodeEdit' ORDER BY id`, id)
	if err != nil {
		t.Fatalf("query finding tasks: %v", err)
	}
	var taskIDs []int
	for rows.Next() {
		var tid int
		rows.Scan(&tid)
		taskIDs = append(taskIDs, tid)
	}
	rows.Close()
	if len(taskIDs) != 2 {
		t.Fatalf("expected 2 task IDs, got %d", len(taskIDs))
	}

	// Finding 1 (taskIDs[0]) should have no dependencies.
	if deps := store.GetDependencies(db, taskIDs[0]); len(deps) != 0 {
		t.Errorf("expected finding 1 to have no deps, got %v", deps)
	}

	// Finding 2 (taskIDs[1]) should depend on Finding 1 (taskIDs[0]).
	deps := store.GetDependencies(db, taskIDs[1])
	if len(deps) != 1 || deps[0] != taskIDs[0] {
		t.Errorf("expected task #%d to depend on task #%d; got deps: %v", taskIDs[1], taskIDs[0], deps)
	}

	// A convoy should have been created.
	if convoys := store.ListConvoys(db); len(convoys) == 0 {
		t.Error("expected a convoy to be created for findings")
	}
}

func TestRunAuditorTask_Escalated(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	id := store.AddBounty(db, 0, "Audit", "audit the codebase")
	b, _ := store.GetBounty(db, id)

	withStubCLIRunner(t, "[ESCALATED:HIGH:Cannot determine correct API version]", nil)
	logger := log.New(io.Discard, "", 0)
	runAuditorTask(db, "auditor-1", b, logger)

	var count int
	db.QueryRow(`SELECT COUNT(*) FROM Escalations WHERE task_id = ?`, id).Scan(&count)
	if count != 1 {
		t.Errorf("expected 1 escalation row, got %d", count)
	}
}
