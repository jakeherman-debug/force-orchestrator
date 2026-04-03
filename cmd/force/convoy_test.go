package main

import (
	"io"
	"log"
	"strings"
	"testing"

	"force-orchestrator/internal/agents"
	"force-orchestrator/internal/store"
)

// ── CreateConvoy / ListConvoys ────────────────────────────────────────────────

func TestConvoy_CreateAndComplete(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	convoyID, err := store.CreateConvoy(db, "test-convoy")
	if err != nil {
		t.Fatalf("CreateConvoy: %v", err)
	}
	if convoyID == 0 {
		t.Fatal("expected non-zero convoy ID")
	}

	convoys := store.ListConvoys(db)
	if len(convoys) != 1 {
		t.Fatalf("expected 1 convoy, got %d", len(convoys))
	}
	if convoys[0].Name != "test-convoy" {
		t.Errorf("unexpected convoy name: %q", convoys[0].Name)
	}
	if convoys[0].Status != "Active" {
		t.Errorf("expected Active, got %q", convoys[0].Status)
	}
}

func TestListConvoys_AllPresent(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	store.CreateConvoy(db, "convoy-alpha")
	store.CreateConvoy(db, "convoy-beta")
	store.CreateConvoy(db, "convoy-gamma")

	convoys := store.ListConvoys(db)
	if len(convoys) != 3 {
		t.Fatalf("expected 3 convoys, got %d", len(convoys))
	}
	names := map[string]bool{}
	for _, c := range convoys {
		names[c.Name] = true
	}
	for _, expected := range []string{"convoy-alpha", "convoy-beta", "convoy-gamma"} {
		if !names[expected] {
			t.Errorf("expected convoy %q to be present", expected)
		}
	}
}

func TestListConvoys_Multiple(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	store.CreateConvoy(db, "convoy-alpha")
	store.CreateConvoy(db, "convoy-beta")
	store.CreateConvoy(db, "convoy-gamma")

	convoys := store.ListConvoys(db)
	if len(convoys) != 3 {
		t.Errorf("expected 3 convoys, got %d", len(convoys))
	}
}

func TestListConvoys_DBError(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	db.Close()
	result := store.ListConvoys(db)
	if result != nil {
		t.Errorf("expected nil from ListConvoys on DB error, got %v", result)
	}
}

func TestCreateConvoy_DBError(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	db.Close()
	_, err := store.CreateConvoy(db, "test")
	if err == nil {
		t.Error("expected error from CreateConvoy with closed DB")
	}
}

func TestCreateConvoy_DuplicateName(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	// First insert succeeds
	_, err := store.CreateConvoy(db, "alpha")
	if err != nil {
		t.Fatalf("unexpected error on first CreateConvoy: %v", err)
	}

	// Second insert should fail (unique constraint)
	_, err = store.CreateConvoy(db, "alpha")
	if err == nil {
		t.Error("expected error for duplicate convoy name")
	}
}

// ── GetConvoyByName ───────────────────────────────────────────────────────────

func TestGetConvoyByName_Found(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	id, _ := store.CreateConvoy(db, "my-convoy")
	c, err := store.GetConvoyByName(db, "my-convoy")
	if err != nil {
		t.Fatalf("GetConvoyByName error: %v", err)
	}
	if c.ID != id {
		t.Errorf("expected ID %d, got %d", id, c.ID)
	}
	if c.Name != "my-convoy" {
		t.Errorf("expected name 'my-convoy', got %q", c.Name)
	}
	if c.Status != "Active" {
		t.Errorf("expected status 'Active', got %q", c.Status)
	}
}

func TestGetConvoyByName_NotFound(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	_, err := store.GetConvoyByName(db, "nonexistent")
	if err == nil {
		t.Error("expected error for nonexistent convoy")
	}
}

// ── ConvoyProgress ────────────────────────────────────────────────────────────

func TestConvoyProgress(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	convoyID, _ := store.CreateConvoy(db, "progress-test")

	// Insert 3 CodeEdit tasks in this convoy
	for i := 0; i < 3; i++ {
		db.Exec(`INSERT INTO BountyBoard (parent_id, type, status, payload, convoy_id) VALUES (0, 'CodeEdit', 'Pending', 'task', ?)`, convoyID)
	}

	completed, total := store.ConvoyProgress(db, convoyID)
	if total != 3 {
		t.Errorf("expected 3 total, got %d", total)
	}
	if completed != 0 {
		t.Errorf("expected 0 completed, got %d", completed)
	}

	// Mark 2 of the 3 as Completed by ID
	rows, _ := db.Query(`SELECT id FROM BountyBoard WHERE convoy_id = ? LIMIT 2`, convoyID)
	var ids []int
	for rows.Next() {
		var rowID int
		rows.Scan(&rowID)
		ids = append(ids, rowID)
	}
	rows.Close()
	for _, rowID := range ids {
		db.Exec(`UPDATE BountyBoard SET status = 'Completed' WHERE id = ?`, rowID)
	}
	completed, total = store.ConvoyProgress(db, convoyID)
	if completed != 2 {
		t.Errorf("expected 2 completed, got %d", completed)
	}
	if total != 3 {
		t.Errorf("expected 3 total, got %d", total)
	}
}

// ── CheckConvoyCompletions ────────────────────────────────────────────────────

func TestCheckConvoyCompletions_SendsMail(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	// Create a convoy with one completed task
	convoyID, _ := store.CreateConvoy(db, "test-convoy")
	db.Exec(`INSERT INTO BountyBoard (parent_id, target_repo, type, status, payload, convoy_id) VALUES (0, 'repo', 'CodeEdit', 'Completed', 'do thing', ?)`, convoyID)

	logger := log.New(io.Discard, "", 0)
	agents.CheckConvoyCompletions(db, logger)

	mails := store.ListMail(db, "operator")
	if len(mails) != 1 {
		t.Fatalf("expected 1 mail to operator, got %d", len(mails))
	}
	if mails[0].MessageType != store.MailTypeInfo {
		t.Errorf("expected MailTypeInfo, got %s", mails[0].MessageType)
	}
	if !strings.Contains(mails[0].Subject, "test-convoy") {
		t.Errorf("subject should mention convoy name, got %q", mails[0].Subject)
	}

	// Running again should NOT send duplicate mail (convoy is now Completed)
	agents.CheckConvoyCompletions(db, logger)
	mails2 := store.ListMail(db, "operator")
	if len(mails2) != 1 {
		t.Errorf("expected no additional mail on second run, got %d total", len(mails2))
	}
}

func TestCheckConvoyCompletions_StalledConvoy(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	convoyID, _ := store.CreateConvoy(db, "stalled-convoy")
	// Add one completed and one failed task
	db.Exec(`INSERT INTO BountyBoard (parent_id, target_repo, type, status, payload, convoy_id)
		VALUES (0, 'repo', 'CodeEdit', 'Completed', 'done', ?)`, convoyID)
	db.Exec(`INSERT INTO BountyBoard (parent_id, target_repo, type, status, payload, convoy_id)
		VALUES (0, 'repo', 'CodeEdit', 'Failed', 'broken', ?)`, convoyID)

	logger := log.New(io.Discard, "", 0)
	agents.CheckConvoyCompletions(db, logger)

	// Should send stall alert mail to operator
	mails := store.ListMail(db, "operator")
	if len(mails) == 0 {
		t.Error("expected stall alert mail to operator")
	}
	found := false
	for _, m := range mails {
		if strings.Contains(m.Subject, "STALLED") {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected STALLED subject in operator mail")
	}

	// Second call should NOT send duplicate stall mail
	agents.CheckConvoyCompletions(db, logger)
	mails2 := store.ListMail(db, "operator")
	if len(mails2) != len(mails) {
		t.Errorf("expected no duplicate stall mail, got %d vs %d", len(mails2), len(mails))
	}
}

func TestCheckConvoyCompletions_EmptyConvoy(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	// Active convoy with no tasks → should skip (total=0)
	store.CreateConvoy(db, "empty-convoy")

	logger := log.New(io.Discard, "", 0)
	agents.CheckConvoyCompletions(db, logger)

	// No mail should be sent
	_, total := store.MailStats(db, "")
	if total != 0 {
		t.Errorf("expected no mail for empty convoy, got %d", total)
	}
}

func TestCheckConvoyCompletions_AllComplete(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	convoyID, _ := store.CreateConvoy(db, "finish-line")
	db.Exec(`INSERT INTO BountyBoard (parent_id, target_repo, type, status, payload, convoy_id)
		VALUES (0, 'repo', 'CodeEdit', 'Completed', 'task1', ?)`, convoyID)

	logger := log.New(io.Discard, "", 0)
	agents.CheckConvoyCompletions(db, logger)

	var status string
	db.QueryRow(`SELECT status FROM Convoys WHERE id = ?`, convoyID).Scan(&status)
	if status != "Completed" {
		t.Errorf("expected convoy status 'Completed', got %q", status)
	}
}
