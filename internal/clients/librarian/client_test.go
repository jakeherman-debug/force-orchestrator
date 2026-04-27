package librarian_test

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"force-orchestrator/internal/clients/librarian"
	"force-orchestrator/internal/store"
)

// newTestDB returns a real in-memory holocron with the schema applied.
// Per CLAUDE.md "Testing rules": never mock the database.
func newTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db := store.InitHolocronDSN(":memory:")
	if db == nil {
		t.Fatalf("init in-memory holocron returned nil")
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestLibrarianClient_WriteMemory_QueuesBounty(t *testing.T) {
	db := newTestDB(t)
	c := librarian.NewInProcess(db)

	id, err := c.WriteMemory(context.Background(), librarian.Memory{
		ParentTaskID: 0,
		Repo:         "force",
		Task:         "Add feature X",
		Files:        "a.go,b.go",
		Feedback:     "looks good",
		Diff:         "+something",
	})
	if err != nil {
		t.Fatalf("WriteMemory: %v", err)
	}
	if id <= 0 {
		t.Fatalf("expected positive bounty ID, got %d", id)
	}

	var taskType, status, payload string
	if scanErr := db.QueryRow(
		`SELECT type, status, payload FROM BountyBoard WHERE id = ?`, id,
	).Scan(&taskType, &status, &payload); scanErr != nil {
		t.Fatalf("read back bounty: %v", scanErr)
	}
	if taskType != "WriteMemory" {
		t.Errorf("task_type = %q, want WriteMemory", taskType)
	}
	if status != "Pending" {
		t.Errorf("status = %q, want Pending", status)
	}
	// Payload should be JSON containing every Memory field.
	for _, want := range []string{`"task":"Add feature X"`, `"files":"a.go,b.go"`,
		`"feedback":"looks good"`, `"diff":"+something"`, `"repo":"force"`} {
		if !contains(payload, want) {
			t.Errorf("payload missing %q\npayload=%s", want, payload)
		}
	}
}

func TestLibrarianClient_WriteMemoryTx_RolledBack(t *testing.T) {
	db := newTestDB(t)
	c := librarian.NewInProcess(db)

	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	id, err := c.WriteMemoryTx(context.Background(), tx, librarian.Memory{
		ParentTaskID: 42, Repo: "force", Task: "tx queue",
	})
	if err != nil {
		t.Fatalf("WriteMemoryTx: %v", err)
	}
	if id <= 0 {
		t.Fatalf("expected positive bounty ID, got %d", id)
	}

	// Rollback must drop the queued row — that's the load-bearing
	// invariant for the migrated terminalConvoyTransitionTx /
	// onSubPRMerged sites: if their surrounding tx aborts, no
	// orphaned WriteMemory bounty leaks out.
	if rbErr := tx.Rollback(); rbErr != nil {
		t.Fatalf("rollback: %v", rbErr)
	}
	var n int
	if scanErr := db.QueryRow(`SELECT COUNT(*) FROM BountyBoard WHERE id = ?`, id).Scan(&n); scanErr != nil {
		t.Fatalf("count after rollback: %v", scanErr)
	}
	if n != 0 {
		t.Fatalf("expected 0 rows after rollback, got %d (WriteMemoryTx leaked outside its tx)", n)
	}
}

func TestLibrarianClient_WriteMemoryTx_Committed(t *testing.T) {
	db := newTestDB(t)
	c := librarian.NewInProcess(db)

	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	id, err := c.WriteMemoryTx(context.Background(), tx, librarian.Memory{
		ParentTaskID: 99, Repo: "force", Task: "tx commit",
	})
	if err != nil {
		t.Fatalf("WriteMemoryTx: %v", err)
	}
	if cmErr := tx.Commit(); cmErr != nil {
		t.Fatalf("commit: %v", cmErr)
	}
	var taskType, status string
	if scanErr := db.QueryRow(
		`SELECT type, status FROM BountyBoard WHERE id = ?`, id,
	).Scan(&taskType, &status); scanErr != nil {
		t.Fatalf("read after commit: %v", scanErr)
	}
	if taskType != "WriteMemory" {
		t.Errorf("task_type = %q, want WriteMemory", taskType)
	}
	if status != "Pending" {
		t.Errorf("status = %q, want Pending", status)
	}
}

func TestLibrarianClient_WriteMemoryTx_RejectsNilTx(t *testing.T) {
	db := newTestDB(t)
	c := librarian.NewInProcess(db)
	_, err := c.WriteMemoryTx(context.Background(), nil, librarian.Memory{Repo: "x"})
	if err == nil {
		t.Fatal("expected error for nil tx, got none")
	}
}

func TestLibrarianClient_GetMemoriesForTask(t *testing.T) {
	db := newTestDB(t)
	c := librarian.NewInProcess(db)

	store.StoreFleetMemory(db, "force", 7, "success", "summary one", "a.go", "tag1")
	store.StoreFleetMemory(db, "force", 7, "failure", "summary two", "b.go", "tag2")
	store.StoreFleetMemory(db, "force", 8, "success", "unrelated", "c.go", "")

	memories, err := c.GetMemoriesForTask(context.Background(), 7)
	if err != nil {
		t.Fatalf("GetMemoriesForTask: %v", err)
	}
	if len(memories) != 2 {
		t.Fatalf("expected 2 memories for task 7, got %d", len(memories))
	}
	for _, m := range memories {
		if m.ParentTaskID != 7 {
			t.Errorf("got memory with ParentTaskID=%d, expected 7", m.ParentTaskID)
		}
		if m.Repo != "force" {
			t.Errorf("got memory with Repo=%q, expected force", m.Repo)
		}
	}
}

func TestLibrarianClient_GetMemoriesByScope_RequiresFilter(t *testing.T) {
	db := newTestDB(t)
	c := librarian.NewInProcess(db)
	_, err := c.GetMemoriesByScope(context.Background(), librarian.Scope{})
	if !errors.Is(err, librarian.ErrEmptyScope) {
		t.Fatalf("expected ErrEmptyScope, got %v", err)
	}
}

func TestLibrarianClient_GetMemoriesByScope_FiltersAndLimits(t *testing.T) {
	db := newTestDB(t)
	c := librarian.NewInProcess(db)

	store.StoreFleetMemory(db, "force", 1, "success", "s1", "", "")
	store.StoreFleetMemory(db, "force", 2, "failure", "s2", "", "")
	store.StoreFleetMemory(db, "other", 3, "success", "s3", "", "")

	all, err := c.GetMemoriesByScope(context.Background(), librarian.Scope{Repo: "force"})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("repo=force returned %d memories, want 2", len(all))
	}

	successOnly, err := c.GetMemoriesByScope(context.Background(),
		librarian.Scope{Repo: "force", Outcome: "success"})
	if err != nil {
		t.Fatalf("filtered query: %v", err)
	}
	if len(successOnly) != 1 {
		t.Fatalf("repo=force outcome=success returned %d, want 1", len(successOnly))
	}

	limited, err := c.GetMemoriesByScope(context.Background(),
		librarian.Scope{Repo: "force", Limit: 1})
	if err != nil {
		t.Fatalf("limited query: %v", err)
	}
	if len(limited) != 1 {
		t.Fatalf("limit=1 returned %d rows, want 1", len(limited))
	}

	if _, err := c.GetMemoriesByScope(context.Background(),
		librarian.Scope{Repo: "force", Limit: -1}); !errors.Is(err, librarian.ErrInvalidLimit) {
		t.Fatalf("expected ErrInvalidLimit for negative Limit, got %v", err)
	}
}

func TestLibrarianClient_UpdateMemory_NotFound(t *testing.T) {
	db := newTestDB(t)
	c := librarian.NewInProcess(db)
	err := c.UpdateMemory(context.Background(), 99999,
		librarian.MemoryUpdate{Summary: "rewritten"})
	if !errors.Is(err, librarian.ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestLibrarianClient_UpdateMemory_PartialUpdate(t *testing.T) {
	db := newTestDB(t)
	c := librarian.NewInProcess(db)
	store.StoreFleetMemory(db, "force", 1, "success", "old summary", "f.go", "tag")

	var id int
	if err := db.QueryRow(`SELECT id FROM FleetMemory LIMIT 1`).Scan(&id); err != nil {
		t.Fatalf("read id: %v", err)
	}

	if err := c.UpdateMemory(context.Background(), id,
		librarian.MemoryUpdate{Summary: "new summary"}); err != nil {
		t.Fatalf("UpdateMemory: %v", err)
	}

	var summary, files, tags string
	if err := db.QueryRow(`SELECT summary, files_changed, topic_tags FROM FleetMemory WHERE id = ?`, id).
		Scan(&summary, &files, &tags); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if summary != "new summary" {
		t.Errorf("summary = %q, want 'new summary'", summary)
	}
	if files != "f.go" {
		t.Errorf("files_changed mutated unexpectedly: %q", files)
	}
	if tags != "tag" {
		t.Errorf("topic_tags mutated unexpectedly: %q", tags)
	}
}

func TestLibrarianClient_RemoveMemory(t *testing.T) {
	db := newTestDB(t)
	c := librarian.NewInProcess(db)
	store.StoreFleetMemory(db, "force", 1, "success", "to remove", "", "")
	var id int
	if err := db.QueryRow(`SELECT id FROM FleetMemory LIMIT 1`).Scan(&id); err != nil {
		t.Fatalf("read id: %v", err)
	}
	if err := c.RemoveMemory(context.Background(), id); err != nil {
		t.Fatalf("RemoveMemory: %v", err)
	}
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM FleetMemory WHERE id = ?`, id).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 0 {
		t.Fatalf("RemoveMemory did not delete row %d", id)
	}
	if err := c.RemoveMemory(context.Background(), id); !errors.Is(err, librarian.ErrNotFound) {
		t.Fatalf("expected ErrNotFound on second remove, got %v", err)
	}
}

func TestLibrarianClient_ContextCanceled(t *testing.T) {
	db := newTestDB(t)
	c := librarian.NewInProcess(db)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.WriteMemory(ctx, librarian.Memory{Repo: "x"}); err == nil {
		t.Error("WriteMemory: expected ctx canceled error")
	}
	if _, err := c.GetMemoriesForTask(ctx, 1); err == nil {
		t.Error("GetMemoriesForTask: expected ctx canceled error")
	}
	if _, err := c.GetMemoriesByScope(ctx, librarian.Scope{Repo: "x"}); err == nil {
		t.Error("GetMemoriesByScope: expected ctx canceled error")
	}
	if err := c.RemoveMemory(ctx, 1); err == nil {
		t.Error("RemoveMemory: expected ctx canceled error")
	}
}

func TestMockClient_BasicFlow(t *testing.T) {
	m := librarian.NewMock()
	m.Memories = []librarian.Memory{
		{ID: 1, ParentTaskID: 7, Repo: "force", Outcome: "success", Summary: "s"},
		{ID: 2, ParentTaskID: 8, Repo: "force", Outcome: "failure", Summary: "f"},
	}

	id, err := m.WriteMemory(context.Background(),
		librarian.Memory{ParentTaskID: 9, Repo: "force"})
	if err != nil {
		t.Fatalf("WriteMemory: %v", err)
	}
	if id != 1 {
		t.Errorf("first WriteMemory id = %d, want 1", id)
	}
	if len(m.WriteCalls) != 1 {
		t.Errorf("WriteCalls len = %d, want 1", len(m.WriteCalls))
	}

	got, err := m.GetMemoriesForTask(context.Background(), 7)
	if err != nil {
		t.Fatalf("GetMemoriesForTask: %v", err)
	}
	if len(got) != 1 || got[0].ID != 1 {
		t.Errorf("GetMemoriesForTask(7) = %+v, want one row with ID=1", got)
	}
}

// contains is a tiny helper to keep test deps minimal — strings.Contains
// would do the same but is one less import.
func contains(s, substr string) bool {
	return len(s) >= len(substr) && (substr == "" ||
		(func() bool {
			for i := 0; i+len(substr) <= len(s); i++ {
				if s[i:i+len(substr)] == substr {
					return true
				}
			}
			return false
		})())
}
