package main

import (
	"strings"
	"testing"

	"force-orchestrator/internal/agents"
	"force-orchestrator/internal/store"
)

// ── printList ─────────────────────────────────────────────────────────────────

func TestPrintList_NoFilter(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	store.AddBounty(db, 0, "CodeEdit", "task one")
	store.AddBounty(db, 0, "Feature", "task two")

	out := captureOutput(func() { printList(db, "", "", "", 0) })
	if !strings.Contains(out, "ID") {
		t.Error("expected header in printList output")
	}
}

func TestPrintList_Empty(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	out := captureOutput(func() { printList(db, "Locked", "", "", 10) })
	if !strings.Contains(out, "no tasks") {
		t.Errorf("expected 'no tasks' for empty filtered list, got: %s", out)
	}
}

func TestPrintList_WithBlockedAndRetriedTasks(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	id1 := store.AddBounty(db, 0, "CodeEdit", "blocking task")
	id2 := store.AddBounty(db, 0, "CodeEdit", "blocked task")
	store.AddDependency(db, id2, id1)
	store.IncrementRetryCount(db, id2)

	out := captureOutput(func() { printList(db, "", "", "", 0) })
	if !strings.Contains(out, "Blocked") {
		t.Errorf("expected 'Blocked' label for blocked task, got: %s", out)
	}
}

func TestPrintList_WithLimit(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	for i := 0; i < 5; i++ {
		store.AddBounty(db, 0, "CodeEdit", "task")
	}

	out := captureOutput(func() { printList(db, "", "", "", 3) })
	// Should show header + 3 tasks (limit=3)
	lines := strings.Split(strings.TrimSpace(out), "\n")
	taskLines := 0
	for _, l := range lines {
		if strings.Contains(l, "Pend") || strings.Contains(l, "Pnd") || strings.Contains(l, "CodeEdit") {
			taskLines++
		}
	}
	if taskLines > 3 {
		t.Errorf("expected at most 3 task lines with limit=3, got %d", taskLines)
	}
}

func TestPrintList_MultiStatusFilter(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	id1 := store.AddBounty(db, 0, "CodeEdit", "pending")
	id2 := store.AddBounty(db, 0, "CodeEdit", "will complete")
	db.Exec(`UPDATE BountyBoard SET status = 'Completed' WHERE id = ?`, id2)
	_ = id1

	out := captureOutput(func() { printList(db, "Pending,Completed", "", "", 0) })
	if !strings.Contains(out, "pending") {
		t.Errorf("expected pending task in output, got: %s", out)
	}
	if !strings.Contains(out, "will complete") {
		t.Errorf("expected completed task in output, got: %s", out)
	}
}

// ── printLogs ─────────────────────────────────────────────────────────────────

func TestPrintLogs_Found(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	id := store.AddBounty(db, 0, "CodeEdit", "do stuff")
	out := captureOutput(func() { printLogs(db, id) })
	if !strings.Contains(out, "Task") {
		t.Errorf("expected task info in printLogs output, got: %s", out)
	}
}

func TestPrintLogs_NotFound(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	out := captureOutput(func() { printLogs(db, 9999) })
	if !strings.Contains(out, "not found") {
		t.Errorf("expected 'not found' for missing task, got: %s", out)
	}
}

func TestPrintLogs_WithErrorLog(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	id := store.AddBounty(db, 0, "CodeEdit", "failing task")
	store.FailBounty(db, id, "compilation failed: undefined variable x")

	out := captureOutput(func() { printLogs(db, id) })
	if !strings.Contains(out, "Error Log") {
		t.Errorf("expected 'Error Log' section for failed task, got: %s", out)
	}
	if !strings.Contains(out, "compilation failed") {
		t.Errorf("expected error message in output, got: %s", out)
	}
}

func TestPrintLogs_WithDependencies(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	id1 := store.AddBounty(db, 0, "CodeEdit", "blocker")
	id2 := store.AddBounty(db, 0, "CodeEdit", "blocked task")
	store.AddDependency(db, id2, id1)

	out := captureOutput(func() { printLogs(db, id2) })
	if !strings.Contains(out, "Blocked By") {
		t.Errorf("expected 'Blocked By' in printLogs for dependent task, got: %s", out)
	}
}

// ── printHistory ──────────────────────────────────────────────────────────────

func TestPrintHistory_NoHistory(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	out := captureOutput(func() { printHistory(db, 9999, false) })
	if !strings.Contains(out, "No history") {
		t.Errorf("expected 'No history' for missing task, got: %s", out)
	}
}

func TestPrintHistory_WithHistory(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	id := store.AddBounty(db, 0, "CodeEdit", "task")
	store.RecordTaskHistory(db, id, "R2-D2", "sess1", "did the work", "Completed")

	out := captureOutput(func() { printHistory(db, id, false) })
	if !strings.Contains(out, "Attempt") {
		t.Errorf("expected attempt info in printHistory output, got: %s", out)
	}
}

func TestPrintHistory_FullOutput(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	id := store.AddBounty(db, 0, "CodeEdit", "task")
	longOutput := strings.Repeat("x", 3000)
	store.RecordTaskHistory(db, id, "R2-D2", "sess1", longOutput, "Completed")

	// With full=false, output should be truncated
	outTrunc := captureOutput(func() { printHistory(db, id, false) })
	if !strings.Contains(outTrunc, "truncated") {
		t.Error("expected truncation message when full=false and output is long")
	}

	// With full=true, output should not be truncated
	outFull := captureOutput(func() { printHistory(db, id, true) })
	if strings.Contains(outFull, "truncated") {
		t.Error("expected no truncation when full=true")
	}
}

func TestPrintHistory_WithTokens(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	id := store.AddBounty(db, 0, "CodeEdit", "task")
	store.RecordTaskHistory(db, id, "R2-D2", "sess1", "output here", "Completed")
	// Update the history entry to have token counts
	db.Exec(`UPDATE TaskHistory SET tokens_in = 100, tokens_out = 200 WHERE task_id = ?`, id)

	out := captureOutput(func() { printHistory(db, id, false) })
	if !strings.Contains(out, "100") || !strings.Contains(out, "200") {
		t.Errorf("expected token counts in history output, got: %s", out)
	}
}

// ── printEscalations ──────────────────────────────────────────────────────────

func TestPrintEscalations_NoEscalations(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	out := captureOutput(func() { printEscalations(db, "Open") })
	if !strings.Contains(out, "No escalations") {
		t.Errorf("expected 'No escalations', got: %s", out)
	}
}

func TestPrintEscalations_WithEscalations(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	id := store.AddBounty(db, 0, "CodeEdit", "task")
	agents.CreateEscalation(db, id, store.SeverityHigh, "critical issue")

	out := captureOutput(func() { printEscalations(db, "Open") })
	if !strings.Contains(out, "HIGH") {
		t.Errorf("expected HIGH severity in output, got: %s", out)
	}
}

// ── printConvoys ──────────────────────────────────────────────────────────────

func TestPrintConvoys_NoConvoys(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	out := captureOutput(func() { printConvoys(db) })
	if !strings.Contains(out, "No convoys") {
		t.Errorf("expected 'No convoys', got: %s", out)
	}
}

func TestPrintConvoys_WithConvoys(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	store.CreateConvoy(db, "operation-alpha")

	out := captureOutput(func() { printConvoys(db) })
	if !strings.Contains(out, "operation-alpha") {
		t.Errorf("expected convoy name in output, got: %s", out)
	}
}

// ── printStats ────────────────────────────────────────────────────────────────

func TestPrintStats(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	store.AddBounty(db, 0, "CodeEdit", "task")

	out := captureOutput(func() { printStats(db) })
	if !strings.Contains(out, "Fleet Statistics") {
		t.Errorf("expected 'Fleet Statistics' header, got: %s", out)
	}
}

func TestPrintStats_WithCompletedTasks(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	id := store.AddBounty(db, 0, "CodeEdit", "task")
	histID := store.RecordTaskHistory(db, id, "R2-D2", "sess1", "output", "Completed")
	store.UpdateTaskHistoryTokens(db, histID, 1000, 200)

	out := captureOutput(func() { printStats(db) })
	if !strings.Contains(out, "R2-D2") {
		t.Errorf("expected agent in stats output, got: %s", out)
	}
}

// ── printCosts ────────────────────────────────────────────────────────────────

func TestPrintCosts(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	out := captureOutput(func() { printCosts(db) })
	if !strings.Contains(out, "Token Usage") {
		t.Errorf("expected 'Token Usage' in output, got: %s", out)
	}
}

func TestPrintCosts_WithTokenData(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	id := store.AddBounty(db, 0, "CodeEdit", "task")
	histID := store.RecordTaskHistory(db, id, "R2-D2", "sess1", "output", "Completed")
	store.UpdateTaskHistoryTokens(db, histID, 5000, 1000)

	out := captureOutput(func() { printCosts(db) })
	if !strings.Contains(out, "R2-D2") {
		t.Errorf("expected agent name in token report, got: %s", out)
	}
	if !strings.Contains(out, "5000") {
		t.Errorf("expected input token count in output, got: %s", out)
	}
}

// ── printStatus ───────────────────────────────────────────────────────────────

func TestPrintStatus(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	out := captureOutput(func() { printStatus(db) })
	if !strings.Contains(out, "Daemon") {
		t.Errorf("expected 'Daemon' in status output, got: %s", out)
	}
}

func TestPrintStatus_WithEscalations(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	id := store.AddBounty(db, 0, "CodeEdit", "task")
	agents.CreateEscalation(db, id, store.SeverityHigh, "critical issue")

	out := captureOutput(func() { printStatus(db) })
	if !strings.Contains(out, "escalation") {
		t.Errorf("expected escalation info in status output, got: %s", out)
	}
}

func TestPrintStatus_WithEstop(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	agents.SetEstop(db, true)
	defer agents.SetEstop(db, false)

	out := captureOutput(func() { printStatus(db) })
	if !strings.Contains(out, "ACTIVE") {
		t.Errorf("expected 'ACTIVE' estop in status output, got: %s", out)
	}
}

func TestPrintStatus_WithStalledTask(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	id := store.AddBounty(db, 0, "CodeEdit", "stalled")
	db.Exec(`UPDATE BountyBoard SET status = 'Locked', owner = 'R2', locked_at = '2020-01-01 00:00:00' WHERE id = ?`, id)

	out := captureOutput(func() { printStatus(db) })
	if !strings.Contains(out, "Stalled") {
		t.Errorf("expected 'Stalled' in status output, got: %s", out)
	}
}

func TestPrintStatus_WithMail(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	store.SendMail(db, "operator", "R2-D2", "test mail", "", 0, store.MailTypeInfo)

	out := captureOutput(func() { printStatus(db) })
	if !strings.Contains(out, "Fleet mail") {
		t.Errorf("expected 'Fleet mail' in status output, got: %s", out)
	}
}

// ── printWho ──────────────────────────────────────────────────────────────────

func TestPrintWho_NoAgents(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	out := captureOutput(func() { printWho(db) })
	if !strings.Contains(out, "No agents") {
		t.Errorf("expected 'No agents', got: %s", out)
	}
}

func TestPrintWho_WithActiveAgent(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	id := store.AddBounty(db, 0, "CodeEdit", "active work")
	db.Exec(`UPDATE BountyBoard SET status = 'Locked', owner = 'R2-D2', locked_at = datetime('now') WHERE id = ?`, id)

	out := captureOutput(func() { printWho(db) })
	if !strings.Contains(out, "R2-D2") {
		t.Errorf("expected active agent in printWho output, got: %s", out)
	}
}

func TestPrintWho_DBError(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	db.Close()
	out := captureOutput(func() { printWho(db) })
	if !strings.Contains(out, "DB error") {
		t.Errorf("expected 'DB error' output from printWho with closed DB, got: %s", out)
	}
}

// ── printTree ─────────────────────────────────────────────────────────────────

func TestPrintTree_NotFound(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	out := captureOutput(func() { printTree(db, 9999, 0) })
	if !strings.Contains(out, "not found") {
		t.Errorf("expected 'not found' for missing task, got: %s", out)
	}
}

func TestPrintTree_LeafTask(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	id := store.AddBounty(db, 0, "Feature", "leaf task with no children")

	out := captureOutput(func() { printTree(db, id, 0) })
	if !strings.Contains(out, "Feature") {
		t.Errorf("expected task type in tree output, got: %s", out)
	}
}

func TestPrintTree_WithChild(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	parentID := store.AddBounty(db, 0, "CodeEdit", "parent task")
	childID := store.AddBounty(db, parentID, "CodeEdit", "child task")
	_ = childID

	out := captureOutput(func() { printTree(db, parentID, 0) })
	if !strings.Contains(out, "parent task") {
		t.Errorf("expected parent task in output, got: %s", out)
	}
	if !strings.Contains(out, "child task") {
		t.Errorf("expected child task in output, got: %s", out)
	}
}

func TestPrintTree_WithDependency(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	dep := store.AddBounty(db, 0, "CodeEdit", "dependency task")
	main := store.AddBounty(db, 0, "CodeEdit", "main task")
	db.Exec(`INSERT INTO TaskDependencies (task_id, depends_on) VALUES (?, ?)`, main, dep)

	out := captureOutput(func() { printTree(db, main, 0) })
	if !strings.Contains(out, "blocked by") {
		t.Errorf("expected 'blocked by' indicator in tree output, got: %s", out)
	}
}

// ── printAgents ───────────────────────────────────────────────────────────────

func TestPrintAgents_NoAgents(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	out := captureOutput(func() { printAgents(db) })
	if !strings.Contains(out, "No agent") {
		t.Errorf("expected 'No agent worktrees' message, got: %s", out)
	}
}

func TestPrintAgents_WithAgent(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	dir := t.TempDir()
	db.Exec(`INSERT OR REPLACE INTO Agents (agent_name, repo, worktree_path) VALUES (?, ?, ?)`,
		"R2-D2", "api", dir)

	out := captureOutput(func() { printAgents(db) })
	if !strings.Contains(out, "R2-D2") {
		t.Errorf("expected agent name in printAgents output, got: %s", out)
	}
	if !strings.Contains(out, "OK") {
		t.Errorf("expected 'OK' status for valid worktree path, got: %s", out)
	}
}

func TestPrintAgents_MissingWorktree(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	db.Exec(`INSERT OR REPLACE INTO Agents (agent_name, repo, worktree_path) VALUES (?, ?, ?)`,
		"BB-8", "frontend", "/nonexistent/path")

	out := captureOutput(func() { printAgents(db) })
	if !strings.Contains(out, "MISSING") {
		t.Errorf("expected 'MISSING' for nonexistent worktree path, got: %s", out)
	}
}

func TestPrintAgents_DBError(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	db.Close()
	out := captureOutput(func() { printAgents(db) })
	if !strings.Contains(out, "DB error") {
		t.Errorf("expected 'DB error' output from printAgents with closed DB, got: %s", out)
	}
}

// ── printUsage ────────────────────────────────────────────────────────────────

func TestPrintUsage(t *testing.T) {
	out := captureOutput(func() { printUsage() })
	if !strings.Contains(out, "Usage: force") {
		t.Errorf("expected usage header, got: %s", out)
	}
}
