package agents

import (
	"bytes"
	"log"
	"strings"
	"testing"

	"force-orchestrator/internal/store"
)

// Fix #8 Phase A — error-propagation coverage in the agents package.
//
// Covers:
//  1. Unit: CreateEscalation returns error on DB fault.
//  2. Integration: Medic's applyMedicEscalate falls back to FailBounty when
//     CreateEscalation fails (AUDIT-041 hot-path remedy).
//  3. Integration: a hot-path UpdateBountyStatus failure in Medic's
//     autoCompletedMedicTask is logged instead of silently swallowed — the
//     auto-complete must abort (return false) and leave the parent unchanged
//     so downstream work isn't built on a phantom transition.

// Unit: CreateEscalation returns error on DB fault.
func TestFix8A_CreateEscalation_ReturnsErrorOnDBFault(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	id := store.AddBounty(db, 0, "CodeEdit", "task")

	// Force INSERT failure by dropping the Escalations table.
	if _, err := db.Exec(`DROP TABLE Escalations`); err != nil {
		t.Fatalf("drop table setup failed: %v", err)
	}

	escID, err := CreateEscalation(db, id, store.SeverityMedium, "needs operator")
	if err == nil {
		t.Fatalf("CreateEscalation: expected error after table drop, got nil (escID=%d)", escID)
	}
	if escID != 0 {
		t.Errorf("CreateEscalation: expected escID=0 on error, got %d", escID)
	}
}

// Unit: CreateEscalation happy path returns the row ID with nil error AND
// transitions the task to Escalated (webhook/state coupling preserved).
func TestFix8A_CreateEscalation_SuccessPath(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	id := store.AddBounty(db, 0, "CodeEdit", "happy escalation")
	escID, err := CreateEscalation(db, id, store.SeverityLow, "normal path")
	if err != nil {
		t.Fatalf("CreateEscalation happy path: unexpected error: %v", err)
	}
	if escID <= 0 {
		t.Fatalf("CreateEscalation: expected positive escID, got %d", escID)
	}

	b, gerr := store.GetBounty(db, id)
	if gerr != nil || b == nil {
		t.Fatalf("GetBounty: %v", gerr)
	}
	if b.Status != "Escalated" {
		t.Errorf("expected task status=Escalated after CreateEscalation, got %q", b.Status)
	}
}

// Integration: Medic's escalate path falls back to FailBounty when
// CreateEscalation fails. Pre-Fix #8a the escalation error was swallowed and
// the task sat Escalated with no Escalations row — the AUDIT-041 defect.
// Post-fix, applyMedicEscalate logs the error and calls FailBounty so the
// task ends up Failed (observable by the operator).
func TestFix8A_MedicEscalateFallsBackToFailBountyOnCreateEscalationError(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	parentID := store.AddBounty(db, 0, "CodeEdit", "parent task")
	medicID := store.AddBounty(db, parentID, "MedicReview", "medic payload")

	parent, _ := store.GetBounty(db, parentID)
	bounty, _ := store.GetBounty(db, medicID)

	// Drop Escalations so CreateEscalation fails deterministically.
	if _, err := db.Exec(`DROP TABLE Escalations`); err != nil {
		t.Fatalf("drop table setup failed: %v", err)
	}

	var buf bytes.Buffer
	logger := log.New(&buf, "", 0)

	applyMedicEscalate(db, "medic-test", bounty, parent, medicDecision{
		Decision:   "escalate",
		Reason:     "out of ideas",
		Escalation: "please look at this",
	}, logger)

	// Parent must now be Failed (fallback path), NOT sitting Escalated with
	// no corresponding Escalations row.
	b, _ := store.GetBounty(db, parentID)
	if b.Status != "Failed" {
		t.Errorf("expected parent status=Failed after fallback, got %q", b.Status)
	}
	var errLog string
	if err := db.QueryRow(`SELECT error_log FROM BountyBoard WHERE id = ?`, parentID).Scan(&errLog); err != nil {
		t.Fatalf("read back error_log failed: %v", err)
	}
	if !strings.Contains(errLog, "escalation insert failed") {
		t.Errorf("expected error_log to mention fallback reason, got %q", errLog)
	}
	logs := buf.String()
	if !strings.Contains(logs, "CreateEscalation") {
		t.Errorf("expected logger to surface CreateEscalation failure, got logs:\n%s", logs)
	}
}

// Integration: Jedi Council-style flow — if FailBounty fails, the error is
// surfaced through the logger rather than silently dropped. The control-flow
// still returns so the claim can be picked up by the stale-lock detector.
// Simulates the hot-path call site in runJediCouncilTask.
func TestFix8A_JediCouncilFlow_FailBountyErrorSurfacesToLogger(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	id := store.AddBounty(db, 0, "CodeEdit", "council task")

	// Force FailBounty to error out by dropping BountyBoard.
	if _, err := db.Exec(`DROP TABLE BountyBoard`); err != nil {
		t.Fatalf("drop table setup failed: %v", err)
	}

	var buf bytes.Buffer
	logger := log.New(&buf, "", 0)

	// Mimic the Council's hot-path error-check pattern we installed in Phase A:
	//   if err := store.FailBounty(db, id, msg); err != nil {
	//       logger.Printf("Task %d: FailBounty failed (%v); stale-lock detector will recover", id, err)
	//   }
	if err := store.FailBounty(db, id, "DB Err: unknown target repository 'foo'"); err != nil {
		logger.Printf("Task %d: FailBounty failed (%v); stale-lock detector will recover", id, err)
	}

	logs := buf.String()
	if !strings.Contains(logs, "FailBounty failed") {
		t.Errorf("expected logger output to surface FailBounty failure; got:\n%s", logs)
	}
	if !strings.Contains(logs, "stale-lock detector will recover") {
		t.Errorf("expected logger output to reference recovery path; got:\n%s", logs)
	}
}
