package main

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"force-orchestrator/internal/agents"
	"force-orchestrator/internal/claude"
	"force-orchestrator/internal/store"
)

// ── IsEstopped / SetEstop ─────────────────────────────────────────────────────

func TestEstop(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	if agents.IsEstopped(db) {
		t.Error("should not be estopped initially")
	}

	agents.SetEstop(db, true)
	if !agents.IsEstopped(db) {
		t.Error("should be estopped after SetEstop(true)")
	}

	agents.SetEstop(db, false)
	if agents.IsEstopped(db) {
		t.Error("should not be estopped after SetEstop(false)")
	}
}

// ── IsOverCapacity ────────────────────────────────────────────────────────────

func TestIsOverCapacity(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	// Default max is 0 (unlimited) — should never be over capacity without explicit config
	if agents.IsOverCapacity(db) {
		t.Error("should not be over capacity with default max_concurrent=0 (unlimited)")
	}

	// Lock 4 CodeEdit tasks — still unlimited, so not over capacity
	for i := 0; i < 4; i++ {
		db.Exec(`INSERT INTO BountyBoard (type, status, payload) VALUES ('CodeEdit', 'Locked', 'task')`)
	}
	if agents.IsOverCapacity(db) {
		t.Error("should not be over capacity with default unlimited (max_concurrent=0)")
	}

	// Explicitly set cap to 4 — now we are at capacity
	store.SetConfig(db, "max_concurrent", "4")
	if !agents.IsOverCapacity(db) {
		t.Error("should be over capacity with 4 locked tasks and max_concurrent=4")
	}

	// Raise the cap — should no longer be over capacity
	store.SetConfig(db, "max_concurrent", "10")
	if agents.IsOverCapacity(db) {
		t.Error("should not be over capacity after raising max_concurrent to 10")
	}

	// Setting max_concurrent=0 should re-enable unlimited mode
	store.SetConfig(db, "max_concurrent", "0")
	if agents.IsOverCapacity(db) {
		t.Error("should not be over capacity after setting max_concurrent=0 (unlimited)")
	}
}

// ── IsThrottledByBatchSize ────────────────────────────────────────────────────

func TestIsThrottledByBatchSize_Disabled(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	// batch_size=0 (default) → never throttled
	if agents.IsThrottledByBatchSize(db) {
		t.Error("should not be throttled when batch_size=0")
	}
}

func TestIsThrottledByBatchSize_UnderLimit(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	store.SetConfig(db, "batch_size", "5")
	// No tasks locked recently → not throttled
	if agents.IsThrottledByBatchSize(db) {
		t.Error("should not be throttled with no recent locks")
	}
}

// ── SpawnDelayDuration ────────────────────────────────────────────────────────

func TestSpawnDelayDuration(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	if agents.SpawnDelayDuration(db) != 0 {
		t.Error("default spawn delay should be 0")
	}

	store.SetConfig(db, "spawn_delay_ms", "500")
	if agents.SpawnDelayDuration(db) != 500*time.Millisecond {
		t.Errorf("expected 500ms, got %v", agents.SpawnDelayDuration(db))
	}
}

// ── Rate-limit backoff persistence ───────────────────────────────────────────

func TestPersistAndLoadRateLimitHits(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	// Initially zero
	if n := claude.LoadRateLimitHits(db, "R2-D2"); n != 0 {
		t.Errorf("expected 0 initial hits, got %d", n)
	}

	claude.PersistRateLimitHit(db, "R2-D2", 3)
	if n := claude.LoadRateLimitHits(db, "R2-D2"); n != 3 {
		t.Errorf("expected 3 persisted hits, got %d", n)
	}

	// Other agent unaffected
	if n := claude.LoadRateLimitHits(db, "BB-8"); n != 0 {
		t.Errorf("expected 0 hits for BB-8, got %d", n)
	}
}

func TestClearRateLimitHits(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	claude.PersistRateLimitHit(db, "R2-D2", 5)
	claude.ClearRateLimitHits(db, "R2-D2")
	if n := claude.LoadRateLimitHits(db, "R2-D2"); n != 0 {
		t.Errorf("expected 0 after clear, got %d", n)
	}
}

// ── export / import ───────────────────────────────────────────────────────────

func TestExportAndImport(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	store.AddRepo(db, "my-repo", "/tmp/my-repo", "test repo")
	id1 := store.AddBounty(db, 0, "CodeEdit", "pending task")
	id2 := store.AddBounty(db, 0, "CodeEdit", "another task")
	store.FailBounty(db, id2, "it broke")
	_ = store.AddBounty(db, 0, "Feature", "completed task")
	db.Exec(`UPDATE BountyBoard SET status = 'Completed' WHERE id = ?`, id1+2) // mark the 3rd as completed

	tmpFile := t.TempDir() + "/export.json"
	if err := exportFleet(db, tmpFile); err != nil {
		t.Fatalf("exportFleet: %v", err)
	}

	// Import into a fresh DB
	db2 := store.InitHolocronDSN(":memory:")
	defer db2.Close()

	n, err := importFleet(db2, tmpFile)
	if err != nil {
		t.Fatalf("importFleet: %v", err)
	}
	// Should import 2 non-Completed tasks (Pending + Failed)
	if n != 2 {
		t.Errorf("expected 2 imported tasks, got %d", n)
	}

	// All imported tasks should be Pending
	var nonPending int
	db2.QueryRow(`SELECT COUNT(*) FROM BountyBoard WHERE status != 'Pending'`).Scan(&nonPending)
	if nonPending != 0 {
		t.Errorf("expected all imported tasks to be Pending, got %d non-pending", nonPending)
	}

	// Repo should be imported
	path := store.GetRepoPath(db2, "my-repo")
	if path != "/tmp/my-repo" {
		t.Errorf("expected repo to be imported, got path %q", path)
	}
}

func TestImportFleet_FileNotFound(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	_, err := importFleet(db, "/nonexistent/path/export.json")
	if err == nil {
		t.Error("expected error for nonexistent file")
	}
}

func TestImportFleet_InvalidJSON(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	f, _ := os.CreateTemp(t.TempDir(), "bad-*.json")
	f.WriteString("not valid json{{{")
	f.Close()

	_, err := importFleet(db, f.Name())
	if err == nil {
		t.Error("expected error for invalid JSON")
	}
}

func TestImportFleet_WithMemories(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	// Build an export with memories
	store.AddRepo(db, "api", "/tmp/api", "api service")
	id := store.AddBounty(db, 0, "CodeEdit", "task with memory")
	store.StoreFleetMemory(db, "api", id, "success", "added endpoint", "handler.go", "")

	tmpFile := t.TempDir() + "/export.json"
	if err := exportFleet(db, tmpFile); err != nil {
		t.Fatalf("exportFleet: %v", err)
	}

	db2 := store.InitHolocronDSN(":memory:")
	defer db2.Close()

	n, err := importFleet(db2, tmpFile)
	if err != nil {
		t.Fatalf("importFleet: %v", err)
	}
	if n != 1 {
		t.Errorf("expected 1 imported task, got %d", n)
	}

	// Memories should be imported
	mems := store.ListAllFleetMemories(db2, "api", 10)
	if len(mems) != 1 {
		t.Errorf("expected 1 imported memory, got %d", len(mems))
	}
}

func TestImportFleet_SkipsDuplicateMemories(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	store.AddRepo(db, "api", "/tmp/api", "service")
	store.StoreFleetMemory(db, "api", 1, "success", "added endpoint", "handler.go", "")

	tmpFile := t.TempDir() + "/export.json"
	exportFleet(db, tmpFile)

	// Import twice into the same db
	importFleet(db, tmpFile)
	importFleet(db, tmpFile)

	// Should still have only 1 memory (duplicate skipped)
	mems := store.ListAllFleetMemories(db, "api", 10)
	if len(mems) != 1 {
		t.Errorf("expected 1 memory after duplicate import, got %d", len(mems))
	}
}

func TestExportFleet_WithAllData(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	store.AddRepo(db, "api", "/tmp/api", "service")
	id := store.AddBounty(db, 0, "CodeEdit", "export me")

	// Escalation
	agents.CreateEscalation(db, id, store.SeverityMedium, "test escalation")
	// Audit log
	store.LogAudit(db, "test-actor", "test-action", id, "detail")
	// Memory
	store.StoreFleetMemory(db, "api", id, "success", "fixed bug", "handler.go", "")

	tmpFile := t.TempDir() + "/export.json"
	err := exportFleet(db, tmpFile)
	if err != nil {
		t.Fatalf("exportFleet: %v", err)
	}

	data, _ := os.ReadFile(tmpFile)
	content := string(data)
	if !strings.Contains(content, "test escalation") {
		t.Errorf("expected escalation in export, got: %s", content[:min(200, len(content))])
	}
	if !strings.Contains(content, "test-actor") {
		t.Errorf("expected audit log in export, got: %s", content[:min(200, len(content))])
	}
	if !strings.Contains(content, "fixed bug") {
		t.Errorf("expected memory in export, got: %s", content[:min(200, len(content))])
	}
}

// ── printList multi-status filter ─────────────────────────────────────────────

func TestPrintListMultiStatus(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	store.AddBounty(db, 0, "CodeEdit", "pending task")
	id2 := store.AddBounty(db, 0, "CodeEdit", "failed task")
	store.FailBounty(db, id2, "broke")
	id3 := store.AddBounty(db, 0, "CodeEdit", "completed task")
	db.Exec(`UPDATE BountyBoard SET status = 'Completed' WHERE id = ?`, id3)

	// Multi-status query should only return Pending + Failed rows
	statuses := strings.Split("Pending,Failed", ",")
	placeholders := make([]string, len(statuses))
	args := make([]any, len(statuses))
	for i, s := range statuses {
		placeholders[i] = "?"
		args[i] = strings.TrimSpace(s)
	}
	query := fmt.Sprintf(`SELECT COUNT(*) FROM BountyBoard WHERE status IN (%s)`,
		strings.Join(placeholders, ","))
	var count int
	db.QueryRow(query, args...).Scan(&count)
	if count != 2 {
		t.Errorf("expected 2 tasks matching Pending,Failed filter, got %d", count)
	}
}

// ── infraBackoff ──────────────────────────────────────────────────────────────

func TestInfraBackoff(t *testing.T) {
	tests := []struct {
		count int
		want  string // duration string
	}{
		{1, "10s"},
		{3, "30s"},
		{6, "1m0s"}, // capped at 60s
		{10, "1m0s"},
	}
	for _, tt := range tests {
		got := agents.InfraBackoff(tt.count).String()
		if got != tt.want {
			t.Errorf("InfraBackoff(%d) = %s, want %s", tt.count, got, tt.want)
		}
	}
}
