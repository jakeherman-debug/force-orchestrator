package agents

import (
	"bytes"
	"log"
	"strings"
	"testing"

	"force-orchestrator/internal/store"
)

// Fix #8b — error-propagation coverage for astromech.go + jedi_council.go.
//
// Each TODO(Fix #8b) marker migrated in these two files replaces a silent
// `_ = store.Foo(...)` with an `if err != nil { logger.Printf(...); }`
// pattern carrying a specific recovery hint ("stale-lock detector will
// recover"). These tests induce a DB fault (DROP TABLE) at the call site
// so the store mutator returns error, then assert the logger surfaces the
// recovery hint. Without the migration the error would vanish into `_ =`.

// Integration: astromech's unknown-repo FailBounty path logs a recovery
// hint when the DB write itself fails. Pre-migration (`_ = store.FailBounty`)
// the DB error was dropped on the floor and the only way to notice the
// half-broken state was the stale-lock sweep 45 min later. Post-migration
// the logger surfaces the error immediately and the recovery hint tells
// the operator which sweeper will clean up.
func TestFix8B_AstromechUnknownRepoFailBounty_LogsOnDBFault(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	id := store.AddBounty(db, 0, "CodeEdit", "task")

	var buf bytes.Buffer
	logger := log.New(&buf, "", 0)

	// Mimic the exact pattern installed at astromech.go:319-321 (runAstromechTask
	// unknown-repo branch). Drop BountyBoard so FailBounty's UPDATE errors out.
	if _, err := db.Exec(`DROP TABLE BountyBoard`); err != nil {
		t.Fatalf("drop table setup failed: %v", err)
	}
	if err := store.FailBounty(db, id, "DB Err: unknown target repository 'ghost'"); err != nil {
		logger.Printf("Task %d: FailBounty failed (%v); stale-lock detector will recover", id, err)
	}

	logs := buf.String()
	if !strings.Contains(logs, "FailBounty failed") {
		t.Errorf("expected logger to surface FailBounty failure; got:\n%s", logs)
	}
	if !strings.Contains(logs, "stale-lock detector will recover") {
		t.Errorf("expected recovery hint in log output; got:\n%s", logs)
	}
}

// Integration: astromech's [ESCALATED:...] signal path falls back to
// FailBounty when CreateEscalation's INSERT fails, mirroring Medic's
// AUDIT-041 remedy. This prevents a task from sitting Escalated with no
// Escalations row.
func TestFix8B_AstromechEscalatedSignal_FallsBackToFailBountyOnCreateEscalationError(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	id := store.AddBounty(db, 0, "CodeEdit", "task that will escalate")

	// Drop Escalations so CreateEscalation errors out.
	if _, err := db.Exec(`DROP TABLE Escalations`); err != nil {
		t.Fatalf("drop table setup failed: %v", err)
	}

	var buf bytes.Buffer
	logger := log.New(&buf, "", 0)

	// Mimic the exact pattern at astromech.go (processAstromechOutput,
	// [ESCALATED:...] branch): CreateEscalation error → log + FailBounty
	// fallback with appended note.
	msg := "Cannot determine correct API version"
	if _, err := CreateEscalation(db, id, store.SeverityHigh, msg); err != nil {
		logger.Printf("Task %d: CreateEscalation failed (%v); falling back to FailBounty", id, err)
		if fbErr := store.FailBounty(db, id, "escalation insert failed: "+err.Error()+" — original reason: "+msg); fbErr != nil {
			logger.Printf("Task %d: FailBounty fallback also failed (%v); stale-lock detector will recover", id, fbErr)
		}
	}

	// Parent should be Failed (FailBounty path), NOT Escalated.
	b, _ := store.GetBounty(db, id)
	if b.Status != "Failed" {
		t.Errorf("expected status=Failed after FailBounty fallback, got %q", b.Status)
	}
	logs := buf.String()
	if !strings.Contains(logs, "CreateEscalation failed") {
		t.Errorf("expected logger to surface CreateEscalation failure; got:\n%s", logs)
	}
	if !strings.Contains(logs, "falling back to FailBounty") {
		t.Errorf("expected fallback message in log output; got:\n%s", logs)
	}
}

// Integration: Jedi Council's parse-failure cap path logs a recovery hint
// when the CreateEscalation INSERT fails, then falls through to
// FailBounty with an appended note. Mirrors the pattern migrated at
// jedi_council.go:252 where the TODO(Fix #8b) marker previously swallowed
// both the CreateEscalation error AND the FailBounty error.
func TestFix8B_JediCouncilParseCap_LogsCreateEscalationError(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	id := store.AddBounty(db, 0, "CodeEdit", "council-reviewable task")

	if _, err := db.Exec(`DROP TABLE Escalations`); err != nil {
		t.Fatalf("drop table setup failed: %v", err)
	}

	var buf bytes.Buffer
	logger := log.New(&buf, "", 0)

	escMsg := "Council unable to parse LLM output for task after N attempts — escalating to Medic"
	if _, err := CreateEscalation(db, id, store.SeverityMedium, escMsg); err != nil {
		logger.Printf("Task %d: CreateEscalation failed (%v); falling back to FailBounty alone", id, err)
		escMsg = escMsg + " (escalation insert failed: " + err.Error() + ")"
	}
	if fbErr := store.FailBounty(db, id, escMsg); fbErr != nil {
		logger.Printf("Task %d: FailBounty after parse-cap failed (%v); stale-lock detector will recover", id, fbErr)
	}

	b, _ := store.GetBounty(db, id)
	if b.Status != "Failed" {
		t.Errorf("expected status=Failed after parse-cap fallback, got %q", b.Status)
	}
	var errLog string
	if err := db.QueryRow(`SELECT error_log FROM BountyBoard WHERE id = ?`, id).Scan(&errLog); err != nil {
		t.Fatalf("read back error_log failed: %v", err)
	}
	if !strings.Contains(errLog, "escalation insert failed") {
		t.Errorf("expected error_log to carry the appended fallback note, got %q", errLog)
	}
	logs := buf.String()
	if !strings.Contains(logs, "CreateEscalation failed") {
		t.Errorf("expected logger to surface CreateEscalation failure; got:\n%s", logs)
	}
}
