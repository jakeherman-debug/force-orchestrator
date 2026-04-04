package agents

import (
	"io"
	"log"
	"strings"
	"testing"

	"force-orchestrator/internal/store"
)

func TestCheckConvoyCompletions_AllDone(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	convoyID, err := store.CreateConvoy(db, "all-done-convoy")
	if err != nil {
		t.Fatalf("CreateConvoy: %v", err)
	}
	for i := 0; i < 3; i++ {
		id, err := store.AddConvoyTask(db, 0, "repo", "task payload", convoyID, 0, "Pending")
		if err != nil {
			t.Fatalf("AddConvoyTask: %v", err)
		}
		db.Exec(`UPDATE BountyBoard SET status = 'Completed' WHERE id = ?`, id)
	}

	logger := log.New(io.Discard, "", 0)
	CheckConvoyCompletions(db, logger)

	var status string
	db.QueryRow(`SELECT status FROM Convoys WHERE id = ?`, convoyID).Scan(&status)
	if status != "Completed" {
		t.Errorf("expected convoy status 'Completed', got %q", status)
	}

	mails := store.ListMail(db, "operator")
	found := false
	for _, m := range mails {
		if strings.Contains(m.Subject, "[CONVOY COMPLETE]") {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected [CONVOY COMPLETE] mail to be sent to operator")
	}
}

func TestCheckConvoyCompletions_AnyFailed(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	convoyID, err := store.CreateConvoy(db, "failed-convoy")
	if err != nil {
		t.Fatalf("CreateConvoy: %v", err)
	}

	// 1 completed task
	doneID, err := store.AddConvoyTask(db, 0, "repo", "done task", convoyID, 0, "Pending")
	if err != nil {
		t.Fatalf("AddConvoyTask: %v", err)
	}
	db.Exec(`UPDATE BountyBoard SET status = 'Completed' WHERE id = ?`, doneID)

	// 1 failed task with a specific error
	failID, err := store.AddConvoyTask(db, 0, "repo", "fail task", convoyID, 0, "Pending")
	if err != nil {
		t.Fatalf("AddConvoyTask: %v", err)
	}
	db.Exec(`UPDATE BountyBoard SET status = 'Failed', error_log = 'deployment exploded' WHERE id = ?`, failID)

	logger := log.New(io.Discard, "", 0)
	CheckConvoyCompletions(db, logger)

	var status string
	db.QueryRow(`SELECT status FROM Convoys WHERE id = ?`, convoyID).Scan(&status)
	if status != "Failed" {
		t.Errorf("expected convoy status 'Failed', got %q", status)
	}

	mails := store.ListMail(db, "operator")
	var stalledMail *store.FleetMail
	for i, m := range mails {
		if strings.Contains(m.Subject, "[CONVOY STALLED]") {
			stalledMail = &mails[i]
			break
		}
	}
	if stalledMail == nil {
		t.Fatal("expected [CONVOY STALLED] mail to be sent to operator")
	}
	if !strings.Contains(stalledMail.Body, "deployment exploded") {
		t.Errorf("expected mail body to contain task error_log, got %q", stalledMail.Body)
	}
}

func TestCheckConvoyCompletions_MixDoneAndPending(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	convoyID, err := store.CreateConvoy(db, "mix-convoy")
	if err != nil {
		t.Fatalf("CreateConvoy: %v", err)
	}

	// 2 completed tasks
	for i := 0; i < 2; i++ {
		id, err := store.AddConvoyTask(db, 0, "repo", "done", convoyID, 0, "Pending")
		if err != nil {
			t.Fatalf("AddConvoyTask: %v", err)
		}
		db.Exec(`UPDATE BountyBoard SET status = 'Completed' WHERE id = ?`, id)
	}
	// 1 pending task
	if _, err := store.AddConvoyTask(db, 0, "repo", "pending", convoyID, 0, "Pending"); err != nil {
		t.Fatalf("AddConvoyTask: %v", err)
	}

	logger := log.New(io.Discard, "", 0)
	CheckConvoyCompletions(db, logger)

	var status string
	db.QueryRow(`SELECT status FROM Convoys WHERE id = ?`, convoyID).Scan(&status)
	if status != "Active" {
		t.Errorf("expected convoy status 'Active', got %q", status)
	}

	mails := store.ListMail(db, "operator")
	if len(mails) != 0 {
		t.Errorf("expected no mail to be sent, got %d mail(s)", len(mails))
	}
}

func TestCheckConvoyCompletions_StalledLockedTask(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	convoyID, err := store.CreateConvoy(db, "locked-convoy")
	if err != nil {
		t.Fatalf("CreateConvoy: %v", err)
	}

	// 1 completed task
	doneID, err := store.AddConvoyTask(db, 0, "repo", "done", convoyID, 0, "Pending")
	if err != nil {
		t.Fatalf("AddConvoyTask: %v", err)
	}
	db.Exec(`UPDATE BountyBoard SET status = 'Completed' WHERE id = ?`, doneID)

	// 1 task locked for 150 minutes (>2 hours) — stuck but not Failed
	lockedID, err := store.AddConvoyTask(db, 0, "repo", "locked", convoyID, 0, "Pending")
	if err != nil {
		t.Fatalf("AddConvoyTask: %v", err)
	}
	db.Exec(`UPDATE BountyBoard SET status = 'Locked', owner = 'R2-D2', locked_at = datetime('now', '-150 minutes') WHERE id = ?`, lockedID)

	logger := log.New(io.Discard, "", 0)
	CheckConvoyCompletions(db, logger)

	var status string
	db.QueryRow(`SELECT status FROM Convoys WHERE id = ?`, convoyID).Scan(&status)
	if status != "Active" {
		t.Errorf("expected convoy status 'Active' (not falsely closed), got %q", status)
	}

	// No [CONVOY COMPLETE] mail should be sent for a convoy with a stuck task
	mails := store.ListMail(db, "operator")
	for _, m := range mails {
		if strings.Contains(m.Subject, "[CONVOY COMPLETE]") {
			t.Error("expected no [CONVOY COMPLETE] mail when a task is still locked")
		}
	}
}
