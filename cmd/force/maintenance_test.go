package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"testing"
	"force-orchestrator/internal/store"
)

// ── pruneFleet ────────────────────────────────────────────────────────────────

func TestPruneFleet_DryRun(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	// Add some data to prune — tasks completed more than 30 days ago
	id := store.AddBounty(db, 0, "CodeEdit", "old task")
	db.Exec(`UPDATE BountyBoard SET status = 'Completed', created_at = datetime('now', '-31 days') WHERE id = ?`, id)
	store.LogAudit(db, "operator", "reset", id, "old audit")
	db.Exec(`UPDATE AuditLog SET created_at = datetime('now', '-31 days')`)

	out := captureOutput(func() {
		pruneFleet(db, 30, true)
	})

	if !strings.Contains(out, "DRY RUN") {
		t.Errorf("expected DRY RUN label in output, got: %s", out)
	}
	if !strings.Contains(out, "Would delete") {
		t.Errorf("expected 'Would delete' in dry run output, got: %s", out)
	}

	// Data should still be there after dry run
	var count int
	db.QueryRow(`SELECT COUNT(*) FROM BountyBoard WHERE id = ?`, id).Scan(&count)
	if count != 1 {
		t.Error("dry run should not delete any data")
	}
}

func TestPruneFleet_ActualRun(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	// Add a task completed 31 days ago
	id := store.AddBounty(db, 0, "CodeEdit", "old task")
	db.Exec(`UPDATE BountyBoard SET status = 'Completed', created_at = datetime('now', '-31 days') WHERE id = ?`, id)

	out := captureOutput(func() {
		pruneFleet(db, 30, false)
	})

	if !strings.Contains(out, "Deleted") {
		t.Errorf("expected 'Deleted' in output, got: %s", out)
	}

	// Task should be gone
	var count int
	db.QueryRow(`SELECT COUNT(*) FROM BountyBoard WHERE id = ?`, id).Scan(&count)
	if count != 0 {
		t.Error("expected old task to be pruned")
	}
}

// ── runCleanup ────────────────────────────────────────────────────────────────

func TestRunCleanup_EmptyDB(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	// No repos, no agents registered → should complete without error
	out := captureOutput(func() { runCleanup(db) })
	if !strings.Contains(out, "Cleanup complete") {
		t.Errorf("expected 'Cleanup complete' in output, got: %s", out)
	}
}

func TestRunCleanup_RemovesStaleAgents(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	// Register an agent with a nonexistent worktree path
	db.Exec(`INSERT OR REPLACE INTO Agents (agent_name, repo, worktree_path) VALUES (?, ?, ?)`,
		"K-2SO", "my-repo", "/nonexistent/worktree")

	out := captureOutput(func() { runCleanup(db) })
	if !strings.Contains(out, "Removed stale agent entry") {
		t.Errorf("expected 'Removed stale agent entry' in output, got: %s", out)
	}

	var count int
	db.QueryRow(`SELECT COUNT(*) FROM Agents WHERE agent_name = 'K-2SO'`).Scan(&count)
	if count != 0 {
		t.Error("expected stale agent entry to be removed by runCleanup")
	}
}

func TestRunCleanup_WithRepo(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not found in PATH")
	}
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	// Register a repo with a temp dir that is a git repo
	dir := initTestRepo(t)
	store.AddRepo(db, "test-repo", dir, "test")

	out := captureOutput(func() { runCleanup(db) })
	if !strings.Contains(out, "Cleanup complete") {
		t.Errorf("expected 'Cleanup complete', got: %s", out)
	}
}

// ── exportFleet ───────────────────────────────────────────────────────────────

func TestExportFleet_ValidJSON(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	store.AddBounty(db, 0, "CodeEdit", "export me")
	store.AddBounty(db, 0, "Feature", "export me too")

	outFile := t.TempDir() + "/export.json"
	if err := exportFleet(db, outFile); err != nil {
		t.Fatalf("exportFleet error: %v", err)
	}

	data, err := os.ReadFile(outFile)
	if err != nil {
		t.Fatalf("read export file: %v", err)
	}

	var export FleetExport
	if err := json.Unmarshal(data, &export); err != nil {
		t.Fatalf("export is not valid JSON: %v", err)
	}

	if len(export.Tasks) != 2 {
		t.Errorf("expected 2 tasks in export, got %d", len(export.Tasks))
	}
	if export.ExportedAt == "" {
		t.Error("expected non-empty exported_at timestamp")
	}
}

func TestExportFleet_ContainsTaskPayload(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	store.AddBounty(db, 0, "CodeEdit", "unique-payload-string-xyz")

	outFile := t.TempDir() + "/export.json"
	if err := exportFleet(db, outFile); err != nil {
		t.Fatalf("exportFleet error: %v", err)
	}

	data, err := os.ReadFile(outFile)
	if err != nil {
		t.Fatalf("read export file: %v", err)
	}
	if !strings.Contains(string(data), "unique-payload-string-xyz") {
		t.Error("expected task payload to appear in export JSON")
	}
}

func TestExportFleet_ExcludesNoTasksWhenAllPending(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	for i := 0; i < 3; i++ {
		store.AddBounty(db, 0, "CodeEdit", "task")
	}

	outFile := t.TempDir() + "/export.json"
	if err := exportFleet(db, outFile); err != nil {
		t.Fatalf("exportFleet error: %v", err)
	}

	data, _ := os.ReadFile(outFile)
	var export FleetExport
	json.Unmarshal(data, &export)

	if len(export.Tasks) != 3 {
		t.Errorf("expected 3 tasks in export (all Pending), got %d", len(export.Tasks))
	}
}

// ── importFleet ───────────────────────────────────────────────────────────────

func TestImportFleet_PendingTasksAppear(t *testing.T) {
	// Build an export with two Pending tasks and one Completed
	export := FleetExport{
		ExportedAt: "2026-01-01T00:00:00Z",
		Tasks: []TaskExport{
			{ID: 1, Type: "CodeEdit", Status: "Pending", Payload: "imported-pending-1"},
			{ID: 2, Type: "Feature", Status: "Failed", Payload: "imported-failed"},
			{ID: 3, Type: "CodeEdit", Status: "Completed", Payload: "imported-completed"},
		},
	}
	data, _ := json.Marshal(export)

	inFile := t.TempDir() + "/import.json"
	if err := os.WriteFile(inFile, data, 0644); err != nil {
		t.Fatalf("write import file: %v", err)
	}

	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	count, err := importFleet(db, inFile)
	if err != nil {
		t.Fatalf("importFleet error: %v", err)
	}

	// Completed task is skipped; Pending and Failed are imported
	if count != 2 {
		t.Errorf("expected 2 imported tasks, got %d", count)
	}

	var dbCount int
	db.QueryRow(`SELECT COUNT(*) FROM BountyBoard WHERE status = 'Pending'`).Scan(&dbCount)
	if dbCount != 2 {
		t.Errorf("expected 2 Pending tasks in DB after import, got %d", dbCount)
	}
}

func TestImportFleet_CompletedTasksSkipped(t *testing.T) {
	export := FleetExport{
		ExportedAt: "2026-01-01T00:00:00Z",
		Tasks: []TaskExport{
			{ID: 10, Type: "CodeEdit", Status: "Completed", Payload: "done"},
			{ID: 11, Type: "CodeEdit", Status: "Completed", Payload: "also done"},
		},
	}
	data, _ := json.Marshal(export)

	inFile := t.TempDir() + "/import.json"
	os.WriteFile(inFile, data, 0644)

	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	count, err := importFleet(db, inFile)
	if err != nil {
		t.Fatalf("importFleet error: %v", err)
	}
	if count != 0 {
		t.Errorf("expected 0 imported tasks (all Completed), got %d", count)
	}
}

