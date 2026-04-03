package agents

import (
	"strings"
	"testing"

	"force-orchestrator/internal/store"
)

// ── ParseEscalationSignal ─────────────────────────────────────────────────────

func TestParseEscalationSignal_Low(t *testing.T) {
	output := "I ran the code. [ESCALATED:LOW:Need more context about the API] Done."
	sev, msg, ok := ParseEscalationSignal(output)
	if !ok {
		t.Fatal("expected escalation signal to be found")
	}
	if sev != store.SeverityLow {
		t.Errorf("expected LOW, got %q", sev)
	}
	if msg != "Need more context about the API" {
		t.Errorf("unexpected message: %q", msg)
	}
}

func TestParseEscalationSignal_High(t *testing.T) {
	output := "[ESCALATED:HIGH:Missing credentials — task cannot proceed]"
	sev, msg, ok := ParseEscalationSignal(output)
	if !ok {
		t.Fatal("expected escalation signal to be found")
	}
	if sev != store.SeverityHigh {
		t.Errorf("expected HIGH, got %q", sev)
	}
	if !strings.Contains(msg, "Missing credentials") {
		t.Errorf("unexpected message: %q", msg)
	}
}

func TestParseEscalationSignal_NotPresent(t *testing.T) {
	_, _, ok := ParseEscalationSignal("Task completed successfully.")
	if ok {
		t.Error("expected no escalation signal")
	}
}

func TestParseEscalationSignal_InvalidSeverity(t *testing.T) {
	// CRITICAL is not a valid severity level
	_, _, ok := ParseEscalationSignal("[ESCALATED:CRITICAL:something]")
	if ok {
		t.Error("expected no match for invalid severity")
	}
}

// ── bumpSeverity ──────────────────────────────────────────────────────────────

func TestBumpSeverity(t *testing.T) {
	tests := []struct {
		in  store.EscalationSeverity
		out store.EscalationSeverity
	}{
		{store.SeverityLow, store.SeverityMedium},
		{store.SeverityMedium, store.SeverityHigh},
		{store.SeverityHigh, store.SeverityHigh},
	}
	for _, tt := range tests {
		got := bumpSeverity(tt.in)
		if got != tt.out {
			t.Errorf("bumpSeverity(%q) = %q, want %q", tt.in, got, tt.out)
		}
	}
}

// ── CreateEscalation / ListEscalations ───────────────────────────────────────

func TestCreateEscalation(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	id := store.AddBounty(db, 0, "CodeEdit", "hard task")
	escID := CreateEscalation(db, id, store.SeverityMedium, "Need human input")
	if escID == 0 {
		t.Fatal("expected non-zero escalation ID")
	}

	// Task should now be Escalated
	b, _ := store.GetBounty(db, id)
	if b.Status != "Escalated" {
		t.Errorf("expected Escalated, got %q", b.Status)
	}

	escs := ListEscalations(db, "Open")
	if len(escs) != 1 {
		t.Fatalf("expected 1 open escalation, got %d", len(escs))
	}
	if escs[0].Severity != store.SeverityMedium {
		t.Errorf("unexpected severity: %q", escs[0].Severity)
	}
}

func TestCloseEscalation_Requeue(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	taskID := store.AddBounty(db, 0, "CodeEdit", "escalated task")
	escID := CreateEscalation(db, taskID, store.SeverityLow, "minor issue")

	CloseEscalation(db, escID, true /* requeue */)

	b, _ := store.GetBounty(db, taskID)
	if b.Status != "Pending" {
		t.Errorf("expected Pending after requeue, got %q", b.Status)
	}

	escs := ListEscalations(db, "Open")
	if len(escs) != 0 {
		t.Errorf("expected 0 open escalations after close, got %d", len(escs))
	}
}

// ── AckEscalation ────────────────────────────────────────────────────────────

func TestAckEscalation(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	taskID := store.AddBounty(db, 0, "CodeEdit", "task")
	escID := CreateEscalation(db, taskID, store.SeverityLow, "needs input")

	AckEscalation(db, escID)

	escs := ListEscalations(db, "Acknowledged")
	if len(escs) != 1 {
		t.Fatalf("expected 1 acknowledged escalation, got %d", len(escs))
	}
	if escs[0].Status != "Acknowledged" {
		t.Errorf("expected status Acknowledged, got %q", escs[0].Status)
	}
}

func TestListEscalations_AllStatuses(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	id1 := store.AddBounty(db, 0, "CodeEdit", "task1")
	id2 := store.AddBounty(db, 0, "CodeEdit", "task2")
	id3 := store.AddBounty(db, 0, "CodeEdit", "task3")

	e1 := CreateEscalation(db, id1, store.SeverityLow, "open")
	e2 := CreateEscalation(db, id2, store.SeverityMedium, "acked")
	AckEscalation(db, e2)
	e3 := CreateEscalation(db, id3, store.SeverityHigh, "closed")
	CloseEscalation(db, e3, false)

	all := ListEscalations(db, "")
	if len(all) != 3 {
		t.Errorf("expected 3 total escalations, got %d", len(all))
	}
	_ = e1
}

// ── CheckStaleEscalations ─────────────────────────────────────────────────────

func TestCheckStaleEscalations_BumpsSeverity(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	taskID := store.AddBounty(db, 0, "CodeEdit", "task")

	// Insert a LOW escalation with a clearly old created_at to ensure it exceeds the 4h threshold
	db.Exec(`INSERT INTO Escalations (task_id, severity, message, status, created_at)
		VALUES (?, 'LOW', 'stale escalation', 'Open', '2020-01-01 00:00:00')`, taskID)
	db.Exec(`UPDATE BountyBoard SET status = 'Escalated' WHERE id = ?`, taskID)

	CheckStaleEscalations(db)

	var sev, msg string
	db.QueryRow(`SELECT severity, message FROM Escalations WHERE task_id = ?`, taskID).Scan(&sev, &msg)
	if sev != "MEDIUM" {
		t.Errorf("expected severity bumped to MEDIUM, got %q", sev)
	}
	if !strings.Contains(msg, "RE-ESCALATED") {
		t.Errorf("expected message to mention RE-ESCALATED, got %q", msg)
	}

	// Should also send mail to operator
	mails := store.ListMail(db, "operator")
	if len(mails) == 0 {
		t.Error("expected mail to operator for re-escalated issue")
	}
}

func TestCheckStaleEscalations_IgnoresFresh(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	taskID := store.AddBounty(db, 0, "CodeEdit", "task")
	// Fresh escalation (just created) — should not be bumped
	CreateEscalation(db, taskID, store.SeverityLow, "fresh escalation")

	CheckStaleEscalations(db)

	var sev string
	db.QueryRow(`SELECT severity FROM Escalations WHERE task_id = ?`, taskID).Scan(&sev)
	if sev != "LOW" {
		t.Errorf("expected severity to remain LOW, got %q", sev)
	}
}

func TestCheckStaleEscalations_AlreadyHigh(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	id := store.AddBounty(db, 0, "CodeEdit", "stuck task")
	db.Exec(`INSERT INTO Escalations (task_id, severity, message, status, created_at)
		VALUES (?, 'High', 'already high', 'Open', '2020-01-01 00:00:00')`, id)

	CheckStaleEscalations(db)

	var sev, msg string
	db.QueryRow(`SELECT severity, message FROM Escalations WHERE task_id = ?`, id).Scan(&sev, &msg)
	if sev != "HIGH" {
		t.Errorf("expected severity to stay HIGH, got %q", sev)
	}
	if !strings.Contains(msg, "RE-ESCALATED") {
		t.Errorf("expected RE-ESCALATED in message, got %q", msg)
	}
}

func TestCheckStaleEscalations_DBError(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	db.Close()
	// Must not panic on DB error — covers the `if err != nil { return }` path
	CheckStaleEscalations(db)
}

// ── ListEscalations DB error / status filter ─────────────────────────────────

func TestListEscalations_DBError(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	db.Close()
	result := ListEscalations(db, "Open")
	if result != nil {
		t.Errorf("expected nil from ListEscalations on DB error, got %v", result)
	}
}

func TestListEscalations_StatusFilter(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	id := store.AddBounty(db, 0, "CodeEdit", "task")
	db.Exec(`INSERT INTO Escalations (task_id, severity, message, status) VALUES (?, 'Low', 'open esc', 'Open')`, id)
	db.Exec(`INSERT INTO Escalations (task_id, severity, message, status) VALUES (?, 'Medium', 'acked', 'Acknowledged')`, id)

	open := ListEscalations(db, "Open")
	if len(open) != 1 {
		t.Errorf("expected 1 Open escalation, got %d", len(open))
	}
	acked := ListEscalations(db, "Acknowledged")
	if len(acked) != 1 {
		t.Errorf("expected 1 Acknowledged escalation, got %d", len(acked))
	}
}

// ── CloseEscalation with and without requeue ─────────────────────────────────

func TestCloseEscalation_WithRequeue(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	id := store.AddBounty(db, 0, "CodeEdit", "escalated task")
	CreateEscalation(db, id, store.SeverityLow, "needs help")

	var escID int
	db.QueryRow(`SELECT id FROM Escalations WHERE task_id = ?`, id).Scan(&escID)

	CloseEscalation(db, escID, true)

	b, _ := store.GetBounty(db, id)
	if b.Status != "Pending" {
		t.Errorf("expected task requeueed to Pending after close+requeue, got %q", b.Status)
	}
}

func TestCloseEscalation_WithoutRequeue(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	id := store.AddBounty(db, 0, "CodeEdit", "escalated task")
	CreateEscalation(db, id, store.SeverityMedium, "human review needed")

	var escID int
	db.QueryRow(`SELECT id FROM Escalations WHERE task_id = ?`, id).Scan(&escID)

	CloseEscalation(db, escID, false)

	var status string
	db.QueryRow(`SELECT status FROM Escalations WHERE id = ?`, escID).Scan(&status)
	if status != "Closed" {
		t.Errorf("expected escalation Closed, got %q", status)
	}

	// Task should remain Escalated (not requeueed)
	b, _ := store.GetBounty(db, id)
	if b.Status != "Escalated" {
		t.Errorf("expected task to stay Escalated, got %q", b.Status)
	}
}
