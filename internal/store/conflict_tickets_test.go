package store

import (
	"context"
	"testing"
)

// TestDetectConflicts_SeededContradiction inserts two memories with
// "always" / "never" markers about the same file and asserts a
// ticket lands.
func TestDetectConflicts_SeededContradiction(t *testing.T) {
	db := mustOpenDedupTestDB(t)
	defer db.Close()

	StoreFleetMemory(db, "repoA", 1, "success",
		"Authentication middleware always validates JWTs against the public-key cache.",
		"auth.go,middleware.go", "auth")
	StoreFleetMemory(db, "repoA", 2, "success",
		"Authentication middleware never validates JWTs against the public-key cache.",
		"auth.go,middleware.go", "auth")

	inserted, err := DetectConflicts(context.Background(), db)
	if err != nil {
		t.Fatalf("DetectConflicts: %v", err)
	}
	if inserted != 1 {
		t.Errorf("expected 1 ticket inserted, got %d", inserted)
	}
	tickets, err := ListOpenConflictTickets(context.Background(), db, 10)
	if err != nil {
		t.Fatalf("ListOpenConflictTickets: %v", err)
	}
	if len(tickets) != 1 {
		t.Fatalf("expected 1 open ticket, got %d", len(tickets))
	}
	if tickets[0].MemoryAID != 1 || tickets[0].MemoryBID != 2 {
		t.Errorf("expected (1,2) pair, got (%d,%d)", tickets[0].MemoryAID, tickets[0].MemoryBID)
	}
}

// TestDetectConflicts_NoFileOverlap ensures two contradictory memories
// touching DIFFERENT files don't fire a ticket — the rules are scoped
// to the same component.
func TestDetectConflicts_NoFileOverlap(t *testing.T) {
	db := mustOpenDedupTestDB(t)
	defer db.Close()

	StoreFleetMemory(db, "repoA", 1, "success", "Always rotate keys daily.", "auth.go", "")
	StoreFleetMemory(db, "repoA", 2, "success", "Never rotate keys daily.", "router.go", "")
	inserted, err := DetectConflicts(context.Background(), db)
	if err != nil {
		t.Fatalf("DetectConflicts: %v", err)
	}
	if inserted != 0 {
		t.Errorf("expected 0 tickets (no file overlap), got %d", inserted)
	}
}

// TestDetectConflicts_Idempotent: re-running over an already-detected
// conflict pair does NOT produce duplicate tickets.
func TestDetectConflicts_Idempotent(t *testing.T) {
	db := mustOpenDedupTestDB(t)
	defer db.Close()

	StoreFleetMemory(db, "repoA", 1, "success",
		"Mailer must always retry transient SMTP errors at least twice.",
		"mailer.go", "")
	StoreFleetMemory(db, "repoA", 2, "success",
		"Mailer must not retry transient SMTP errors automatically.",
		"mailer.go", "")
	first, _ := DetectConflicts(context.Background(), db)
	second, _ := DetectConflicts(context.Background(), db)
	if first != 1 || second != 0 {
		t.Errorf("expected (1,0) tickets across runs, got (%d,%d)", first, second)
	}
}

// TestResolveConflictTicket flips status='resolved' + records the note.
func TestResolveConflictTicket(t *testing.T) {
	db := mustOpenDedupTestDB(t)
	defer db.Close()

	StoreFleetMemory(db, "repoA", 1, "success",
		"Always queue failed jobs for retry.", "queue.go", "")
	StoreFleetMemory(db, "repoA", 2, "success",
		"Never queue failed jobs for retry.", "queue.go", "")
	if _, err := DetectConflicts(context.Background(), db); err != nil {
		t.Fatalf("DetectConflicts: %v", err)
	}
	tickets, _ := ListOpenConflictTickets(context.Background(), db, 10)
	if len(tickets) == 0 {
		t.Fatal("no open ticket to resolve")
	}
	if err := ResolveConflictTicket(context.Background(), db, tickets[0].ID, "operator preferred A"); err != nil {
		t.Fatalf("ResolveConflictTicket: %v", err)
	}
	open, _ := ListOpenConflictTickets(context.Background(), db, 10)
	if len(open) != 0 {
		t.Errorf("expected 0 open after resolve, got %d", len(open))
	}
	// Double-resolve fails.
	err := ResolveConflictTicket(context.Background(), db, tickets[0].ID, "again")
	if err == nil {
		t.Errorf("expected error on double-resolve, got nil")
	}
}

func TestResolveConflictTicket_NotFound(t *testing.T) {
	db := mustOpenDedupTestDB(t)
	defer db.Close()
	if err := ResolveConflictTicket(context.Background(), db, 999, "n/a"); err == nil {
		t.Errorf("expected error for missing ticket, got nil")
	}
}
