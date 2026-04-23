package agents

import (
	"database/sql"
	"testing"

	"force-orchestrator/internal/store"
)

// seedEscalatedTaskWithPR creates an Escalated task + an AskBranchPR row in a
// given state. Returns (escalationID, taskID, prRowID).
func seedEscalatedTaskWithPR(t *testing.T, db *sql.DB, prState string) (int, int, int) {
	t.Helper()
	store.AddRepo(db, "api", "/tmp/api", "")
	cid, _ := store.CreateConvoy(db, "[1] t")
	taskID, _ := store.AddConvoyTask(db, 0, "api", "do thing", cid, 5, "Pending")
	db.Exec(`UPDATE BountyBoard SET status = 'Escalated' WHERE id = ?`, taskID)
	prRowID, err := store.CreateAskBranchPR(db, taskID, cid, "api", "https://github.com/acme/api/pull/1", 1)
	if err != nil {
		t.Fatal(err)
	}
	db.Exec(`UPDATE AskBranchPRs SET state = ? WHERE id = ?`, prState, prRowID)
	res, _ := db.Exec(`INSERT INTO Escalations (task_id, severity, message, status)
		VALUES (?, 'MEDIUM', 'sub-PR #1: CI pending over 2h', 'Open')`, taskID)
	escID, _ := res.LastInsertId()
	return int(escID), taskID, prRowID
}

// TestDogEscalationSweeper_ClosesWhenSubPRClosed is the headline case —
// escalation #12 in production (task 371, PR #25 Closed by our terminal-
// task early-exit) is the exact pattern this test mirrors.
func TestDogEscalationSweeper_ClosesWhenSubPRClosed(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	escID, _, _ := seedEscalatedTaskWithPR(t, db, "Closed")

	if err := dogEscalationSweeper(db, testLogger{}); err != nil {
		t.Fatalf("sweeper: %v", err)
	}

	var status, ack string
	db.QueryRow(`SELECT status, acknowledged_at FROM Escalations WHERE id = ?`, escID).Scan(&status, &ack)
	if status != "Closed" {
		t.Errorf("expected escalation Closed, got %q", status)
	}
	if ack == "" {
		t.Error("acknowledged_at should be stamped when auto-closed")
	}
}

// TestDogEscalationSweeper_ClosesWhenSubPRMerged covers the other terminal
// state — sometimes a sibling rebase or human merge gets the PR in, making
// the CI-stuck escalation moot.
func TestDogEscalationSweeper_ClosesWhenSubPRMerged(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	escID, _, _ := seedEscalatedTaskWithPR(t, db, "Merged")
	_ = dogEscalationSweeper(db, testLogger{})

	var status string
	db.QueryRow(`SELECT status FROM Escalations WHERE id = ?`, escID).Scan(&status)
	if status != "Closed" {
		t.Errorf("expected Closed for merged PR, got %q", status)
	}
}

// TestDogEscalationSweeper_LeavesOpenWhenSubPRStillOpen is the conservative
// counterpart — if the PR is still live, the escalation's referenced problem
// might still be real. Never auto-close in that case.
func TestDogEscalationSweeper_LeavesOpenWhenSubPRStillOpen(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	escID, _, _ := seedEscalatedTaskWithPR(t, db, "Open")
	_ = dogEscalationSweeper(db, testLogger{})

	var status string
	db.QueryRow(`SELECT status FROM Escalations WHERE id = ?`, escID).Scan(&status)
	if status != "Open" {
		t.Errorf("must not auto-close escalation with live PR; got %q", status)
	}
}

// TestDogEscalationSweeper_SkipsWhenTaskNotTerminal ensures an escalation
// whose task is somehow still in flight (e.g. reset via operator action)
// stays Open — only TERMINAL tasks with closed PRs qualify.
func TestDogEscalationSweeper_SkipsWhenTaskNotTerminal(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	escID, taskID, _ := seedEscalatedTaskWithPR(t, db, "Closed")
	db.Exec(`UPDATE BountyBoard SET status = 'Pending' WHERE id = ?`, taskID)

	_ = dogEscalationSweeper(db, testLogger{})

	var status string
	db.QueryRow(`SELECT status FROM Escalations WHERE id = ?`, escID).Scan(&status)
	if status != "Open" {
		t.Errorf("task returned to Pending; escalation should stay Open, got %q", status)
	}
}

// TestDogEscalationSweeper_NoPRAtAll_LeavesOpen — escalations for tasks that
// never had a sub-PR (e.g. Commander-level failures, worktree contamination,
// Captain-rejection loops) are orthogonal to the sub-PR-state rule and must
// remain operator-visible.
func TestDogEscalationSweeper_NoPRAtAll_LeavesOpen(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	store.AddRepo(db, "api", "/tmp/api", "")
	cid, _ := store.CreateConvoy(db, "[1] t")
	taskID, _ := store.AddConvoyTask(db, 0, "api", "do thing", cid, 5, "Pending")
	db.Exec(`UPDATE BountyBoard SET status = 'Escalated' WHERE id = ?`, taskID)
	// Notably no CreateAskBranchPR call — escalation has no PR to reference.
	res, _ := db.Exec(`INSERT INTO Escalations (task_id, severity, message, status)
		VALUES (?, 'MEDIUM', 'Captain: max retries exceeded', 'Open')`, taskID)
	escID, _ := res.LastInsertId()

	_ = dogEscalationSweeper(db, testLogger{})

	var status string
	db.QueryRow(`SELECT status FROM Escalations WHERE id = ?`, int(escID)).Scan(&status)
	if status != "Open" {
		t.Errorf("no-PR escalation must stay Open; got %q", status)
	}
}

// TestDogEscalationSweeper_ResolvesWhenTaskCompleted covers the broader
// rule: any path that transitions the task to Completed (Medic auto-
// complete, WorktreeReset re-queue that succeeds, operator ResetTask, etc.)
// makes the original escalation moot. No sub-PR involvement needed.
func TestDogEscalationSweeper_ResolvesWhenTaskCompleted(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	store.AddRepo(db, "api", "/tmp/api", "")
	cid, _ := store.CreateConvoy(db, "[1] t")
	taskID, _ := store.AddConvoyTask(db, 0, "api", "do thing", cid, 5, "Pending")
	db.Exec(`UPDATE BountyBoard SET status = 'Completed' WHERE id = ?`, taskID)
	res, _ := db.Exec(`INSERT INTO Escalations (task_id, severity, message, status)
		VALUES (?, 'MEDIUM', 'Captain: max retries exceeded', 'Open')`, taskID)
	escID, _ := res.LastInsertId()

	_ = dogEscalationSweeper(db, testLogger{})

	var status string
	db.QueryRow(`SELECT status FROM Escalations WHERE id = ?`, escID).Scan(&status)
	if status != "Closed" {
		t.Errorf("escalation for a Completed task must auto-close; got %q", status)
	}
}

// TestDogEscalationSweeper_LeavesOpenWhenTaskFailed ensures the success-rule
// is strictly Completed/Cancelled — a task still stuck at Failed keeps its
// escalation Open (the operator may still need to act).
func TestDogEscalationSweeper_LeavesOpenWhenTaskFailed(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	store.AddRepo(db, "api", "/tmp/api", "")
	cid, _ := store.CreateConvoy(db, "[1] t")
	taskID, _ := store.AddConvoyTask(db, 0, "api", "do thing", cid, 5, "Pending")
	db.Exec(`UPDATE BountyBoard SET status = 'Failed' WHERE id = ?`, taskID)
	res, _ := db.Exec(`INSERT INTO Escalations (task_id, severity, message, status)
		VALUES (?, 'MEDIUM', 'something broke', 'Open')`, taskID)
	escID, _ := res.LastInsertId()

	_ = dogEscalationSweeper(db, testLogger{})

	var status string
	db.QueryRow(`SELECT status FROM Escalations WHERE id = ?`, escID).Scan(&status)
	if status != "Open" {
		t.Errorf("Failed task must keep its escalation Open; got %q", status)
	}
}

// TestDogEscalationSweeper_RespectsOperatorReopen is the headline AUDIT-149
// regression test. Flow:
//   1. Sweeper auto-closes an Escalation (auto_resolve_count 0→1).
//   2. Operator re-opens it for deeper investigation
//      (UPDATE Escalations SET status='Open' WHERE id=X).
//   3. Sweeper runs again on the same tick conditions that originally fired.
//   4. Assert: status stays Open, auto_resolve_count stays at 1, no rewrite.
//
// Without the `auto_resolve_count < 1` gate, the sweeper would silently
// re-close the operator's re-opened row every 10 minutes — the exact
// behaviour the audit flagged.
func TestDogEscalationSweeper_RespectsOperatorReopen(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	escID, _, _ := seedEscalatedTaskWithPR(t, db, "Closed")

	// First tick — closes.
	if err := dogEscalationSweeper(db, testLogger{}); err != nil {
		t.Fatalf("tick 1: %v", err)
	}
	var status string
	var count int
	db.QueryRow(`SELECT status, auto_resolve_count FROM Escalations WHERE id = ?`, escID).
		Scan(&status, &count)
	if status != "Closed" || count != 1 {
		t.Fatalf("tick 1: expected Closed + count=1, got status=%q count=%d", status, count)
	}

	// Operator re-opens — represents "I want to look at this again".
	// Acknowledged_at deliberately left stamped — don't assume operators clear it.
	if _, err := db.Exec(`UPDATE Escalations SET status='Open' WHERE id = ?`, escID); err != nil {
		t.Fatalf("operator reopen: %v", err)
	}

	// Second tick — must NOT re-close.
	if err := dogEscalationSweeper(db, testLogger{}); err != nil {
		t.Fatalf("tick 2: %v", err)
	}
	db.QueryRow(`SELECT status, auto_resolve_count FROM Escalations WHERE id = ?`, escID).
		Scan(&status, &count)
	if status != "Open" {
		t.Errorf("AUDIT-149 regression: sweeper silently re-closed operator-reopened "+
			"escalation (got status=%q, want Open)", status)
	}
	if count != 1 {
		t.Errorf("AUDIT-149 regression: auto_resolve_count incremented past 1 on "+
			"re-open (got %d, want 1) — the one-shot budget is not enforced", count)
	}
}

// TestDogEscalationSweeper_IgnoresAlreadyClosed confirms the UPDATE guard
// against double-closing (status must be Open to flip). Prevents mangled
// acknowledged_at timestamps on hand-closed rows.
func TestDogEscalationSweeper_IgnoresAlreadyClosed(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	escID, _, _ := seedEscalatedTaskWithPR(t, db, "Closed")
	db.Exec(`UPDATE Escalations SET status = 'Closed', acknowledged_at = '2025-01-01 00:00:00' WHERE id = ?`, escID)

	_ = dogEscalationSweeper(db, testLogger{})

	var ack string
	db.QueryRow(`SELECT acknowledged_at FROM Escalations WHERE id = ?`, escID).Scan(&ack)
	if ack != "2025-01-01 00:00:00" {
		t.Errorf("sweeper rewrote acknowledged_at on already-Closed row: %q", ack)
	}
}
