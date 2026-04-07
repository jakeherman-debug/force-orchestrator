package agents

import (
	"fmt"
	"io"
	"log"
	"strings"
	"testing"

	"force-orchestrator/internal/store"
)

// ── Inquisitor stale-lock reset ───────────────────────────────────────────────

// TestIntegration_InquisitorStaleReset_DatetimeFormat is a regression test for
// the datetime format bug: the stale-lock reset used time.Duration.String()
// (e.g. "-45m0s") as a SQLite modifier, which SQLite does not recognise
// (returns NULL), making every locked_at comparison false. The fix uses
// fmt.Sprintf("-%d seconds", ...) which SQLite accepts correctly.
func TestIntegration_InquisitorStaleReset_DatetimeFormat(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	// Task locked 46 minutes ago — past the 45-minute threshold — must be reset.
	staleID := store.AddBounty(db, 0, "CodeEdit", "stale task")
	db.Exec(`UPDATE BountyBoard SET status = 'Locked', owner = 'R2-D2',
		locked_at = datetime('now', '-46 minutes'), infra_failures = 3 WHERE id = ?`, staleID)

	// Task locked 44 minutes ago — within threshold — must NOT be reset.
	freshID := store.AddBounty(db, 0, "CodeEdit", "fresh task")
	db.Exec(`UPDATE BountyBoard SET status = 'Locked', owner = 'BB-8',
		locked_at = datetime('now', '-44 minutes') WHERE id = ?`, freshID)

	// Exact SQL from SpawnInquisitor after the datetime format fix.
	db.Exec(`
		UPDATE BountyBoard
		SET status = 'Pending', owner = '', locked_at = '',
		    infra_failures = 0,
		    error_log = 'Inquisitor: reset after stale lock timeout (infra_failures cleared)'
		WHERE status IN ('Locked', 'UnderReview', 'UnderCaptainReview')
		  AND locked_at != ''
		  AND locked_at < datetime('now', ?)
	`, fmt.Sprintf("-%d seconds", int(staleLockTimeout.Seconds())))

	var staleStatus string
	var staleInfra int
	db.QueryRow(`SELECT status, infra_failures FROM BountyBoard WHERE id = ?`, staleID).
		Scan(&staleStatus, &staleInfra)
	if staleStatus != "Pending" {
		t.Errorf("stale task: want status 'Pending', got %q", staleStatus)
	}
	if staleInfra != 0 {
		t.Errorf("stale task: want infra_failures=0 after reset, got %d", staleInfra)
	}

	var freshStatus string
	db.QueryRow(`SELECT status FROM BountyBoard WHERE id = ?`, freshID).Scan(&freshStatus)
	if freshStatus != "Locked" {
		t.Errorf("fresh task: want status 'Locked' (within threshold), got %q", freshStatus)
	}
}

// TestIntegration_InquisitorStaleReset_ClearsInfraFailures asserts that
// infra_failures is zeroed on stale-lock reset so a task killed by a laptop
// sleep (not a real code failure) doesn't burn through its infra-failure budget.
func TestIntegration_InquisitorStaleReset_ClearsInfraFailures(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	id := store.AddBounty(db, 0, "CodeEdit", "sleeping-laptop task")
	db.Exec(`UPDATE BountyBoard SET status = 'Locked', owner = 'R2-D2',
		locked_at = datetime('now', '-46 minutes'), infra_failures = 4 WHERE id = ?`, id)

	db.Exec(`
		UPDATE BountyBoard
		SET status = 'Pending', owner = '', locked_at = '',
		    infra_failures = 0,
		    error_log = 'Inquisitor: reset after stale lock timeout (infra_failures cleared)'
		WHERE status IN ('Locked', 'UnderReview', 'UnderCaptainReview')
		  AND locked_at != ''
		  AND locked_at < datetime('now', ?)
	`, fmt.Sprintf("-%d seconds", int(staleLockTimeout.Seconds())))

	var infra int
	db.QueryRow(`SELECT infra_failures FROM BountyBoard WHERE id = ?`, id).Scan(&infra)
	if infra != 0 {
		t.Errorf("want infra_failures=0 after stale-lock reset, got %d", infra)
	}
}

// ── Convoy completion + recovery ──────────────────────────────────────────────

// TestIntegration_ConvoyCompletion_CheckAndRecover exercises the full convoy
// lifecycle: tasks complete → CheckConvoyCompletions marks convoy Completed →
// operator mail is sent.
func TestIntegration_ConvoyCompletion_CheckAndRecover(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	convoyID, err := store.CreateConvoy(db, "integration-convoy")
	if err != nil {
		t.Fatalf("CreateConvoy: %v", err)
	}

	t1, _ := store.AddConvoyTask(db, 0, "repo", "task1", convoyID, 0, "Pending")
	t2, _ := store.AddConvoyTask(db, 0, "repo", "task2", convoyID, 0, "Pending")
	db.Exec(`UPDATE BountyBoard SET status = 'Completed' WHERE id IN (?, ?)`, t1, t2)

	logger := log.New(io.Discard, "", 0)
	CheckConvoyCompletions(db, logger)

	var convoyStatus string
	db.QueryRow(`SELECT status FROM Convoys WHERE id = ?`, convoyID).Scan(&convoyStatus)
	if convoyStatus != "Completed" {
		t.Errorf("want convoy Completed after all tasks done, got %q", convoyStatus)
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
		t.Error("want [CONVOY COMPLETE] mail to operator")
	}
}

// TestIntegration_FailedConvoy_ManualResetRecovery simulates an operator
// resetting tasks in a Failed convoy. RecoverStaleConvoys should return it to Active.
func TestIntegration_FailedConvoy_ManualResetRecovery(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	convoyID, _ := store.CreateConvoy(db, "recovering-convoy")
	db.Exec(`UPDATE Convoys SET status = 'Failed' WHERE id = ?`, convoyID)

	t1, _ := store.AddConvoyTask(db, 0, "repo", "task1", convoyID, 0, "Pending")
	t2, _ := store.AddConvoyTask(db, 0, "repo", "task2", convoyID, 0, "Pending")
	// Both tasks reset to Pending by operator — no Failed/Escalated remaining.
	db.Exec(`UPDATE BountyBoard SET status = 'Pending' WHERE id IN (?, ?)`, t1, t2)

	store.RecoverStaleConvoys(db)

	var status string
	db.QueryRow(`SELECT status FROM Convoys WHERE id = ?`, convoyID).Scan(&status)
	if status != "Active" {
		t.Errorf("want convoy Active after operator task reset, got %q", status)
	}
}

// ── E-stop guard ──────────────────────────────────────────────────────────────

// TestIntegration_EstopBlocksClaiming verifies that IsEstopped returns the
// correct value through the full SetEstop → GetConfig round-trip, and that
// ClaimBounty itself remains unblocked (the guard is in the agent loop, not
// in the store layer).
func TestIntegration_EstopBlocksClaiming(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	store.AddBounty(db, 0, "CodeEdit", "pending task")

	if IsEstopped(db) {
		t.Error("want e-stop off at start")
	}

	// ClaimBounty is unaffected by e-stop at the store layer.
	b, claimed := store.ClaimBounty(db, "CodeEdit", "test-agent")
	if !claimed {
		t.Error("want ClaimBounty to succeed when e-stop is off")
	}
	db.Exec(`UPDATE BountyBoard SET status = 'Pending', owner = '', locked_at = '' WHERE id = ?`, b.ID)

	SetEstop(db, true)
	if !IsEstopped(db) {
		t.Error("want e-stop active after SetEstop(true)")
	}

	SetEstop(db, false)
	if IsEstopped(db) {
		t.Error("want e-stop off after SetEstop(false)")
	}
}

// TestIntegration_EstopPreventsClaimLoop verifies that IsEstopped integrates
// correctly with the claim-loop guard. When e-stop is active, the agent loop
// calls IsEstopped first and skips ClaimBounty — confirmed by checking no task
// transitions from Pending to Locked while e-stop is on.
func TestIntegration_EstopPreventsClaimLoop(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	taskID := store.AddBounty(db, 0, "CodeEdit", "task")
	SetEstop(db, true)

	// Simulate what SpawnAstromech does at the top of each loop iteration.
	// If IsEstopped, the loop sleeps and continues — ClaimBounty is never called.
	for i := 0; i < 10; i++ {
		if IsEstopped(db) {
			continue // mirrors: time.Sleep(5s); continue
		}
		store.ClaimBounty(db, "CodeEdit", "agent")
	}

	// Task must still be Pending — ClaimBounty was never reached.
	var status string
	db.QueryRow(`SELECT status FROM BountyBoard WHERE id = ?`, taskID).Scan(&status)
	if status != "Pending" {
		t.Errorf("want task Pending (e-stop blocked claim), got %q", status)
	}

	// After clearing e-stop, ClaimBounty proceeds normally.
	SetEstop(db, false)
	_, claimed := store.ClaimBounty(db, "CodeEdit", "agent")
	if !claimed {
		t.Error("want ClaimBounty to succeed after e-stop cleared")
	}
}

// ── classifyPendingTasks integration ─────────────────────────────────────────

// TestIntegration_ClassifyPendingTasks_UpdatesType verifies the full
// Classifying → Pending transition with a stubbed Claude response.
func TestIntegration_ClassifyPendingTasks_UpdatesType(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	withStubCLIRunner(t, "Feature — targeted fix to the login handler", nil)

	id, _ := store.AddBountyClassifying(db, "", "fix the login bug in auth.go", 0, "key-1")

	logger := log.New(io.Discard, "", 0)
	classifyPendingTasks(db, logger)

	var status, taskType string
	db.QueryRow(`SELECT status, type FROM BountyBoard WHERE id = ?`, id).Scan(&status, &taskType)
	if status != "Pending" {
		t.Errorf("want status 'Pending' after classification, got %q", status)
	}
	if taskType != "Feature" {
		t.Errorf("want type 'Feature', got %q", taskType)
	}
}

// TestIntegration_ClassifyPendingTasks_StaleTimeout verifies that a task stuck
// in Classifying beyond staleClassifyingTimeout is failed, not left in limbo.
func TestIntegration_ClassifyPendingTasks_StaleTimeout(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	id, _ := store.AddBountyClassifying(db, "", "classify me", 0, "key-2")
	db.Exec(`UPDATE BountyBoard SET created_at = datetime('now', '-31 minutes') WHERE id = ?`, id)

	logger := log.New(io.Discard, "", 0)
	classifyPendingTasks(db, logger)

	var status string
	db.QueryRow(`SELECT status FROM BountyBoard WHERE id = ?`, id).Scan(&status)
	if status != "Failed" {
		t.Errorf("want stale Classifying task to be Failed, got %q", status)
	}
}
