package agents

import (
	"fmt"
	"io"
	"log"
	"strings"
	"testing"

	"force-orchestrator/internal/store"
)

// ── runInvestigatorTask ───────────────────────────────────────────────────────

func TestRunInvestigatorTask_CLIError(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	id := store.AddBounty(db, 0, "Investigate", "investigate the incident")
	// Pre-fill to MaxInfraFailures-1 so the next failure permanently fails the task
	// without triggering the sleep in handleInfraFailure.
	for i := 0; i < MaxInfraFailures-1; i++ {
		store.IncrementInfraFailures(db, id)
	}
	b, _ := store.GetBounty(db, id)

	withStubCLIRunner(t, "some output", fmt.Errorf("claude CLI failed: exit 1"))
	logger := log.New(io.Discard, "", 0)
	runInvestigatorTask(db, "investigator-1", b, logger)

	b, _ = store.GetBounty(db, id)
	if b.Status != "Failed" {
		t.Errorf("expected Failed after CLI error at max retries, got %q", b.Status)
	}
}

func TestRunInvestigatorTask_DoneSignal(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	id := store.AddBounty(db, 0, "Investigate", "what caused the outage")
	b, _ := store.GetBounty(db, id)

	withStubCLIRunner(t, "Investigation findings here.\n\n[DONE]", nil)
	logger := log.New(io.Discard, "", 0)
	runInvestigatorTask(db, "investigator-1", b, logger)

	b, _ = store.GetBounty(db, id)
	if b.Status != "Completed" {
		t.Errorf("expected Completed after [DONE], got %q", b.Status)
	}

	// Report should be delivered as mail to the operator.
	mails := store.ListMail(db, "operator")
	if len(mails) == 0 {
		t.Fatal("expected mail to operator with investigation report")
	}
	if !strings.Contains(mails[0].Body, "Investigation findings here.") {
		t.Errorf("expected report body in mail, got: %s", mails[0].Body)
	}
}

func TestRunInvestigatorTask_Escalated(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	id := store.AddBounty(db, 0, "Investigate", "investigate the bug")
	b, _ := store.GetBounty(db, id)

	withStubCLIRunner(t, "[ESCALATED:MEDIUM:Need access to production logs]", nil)
	logger := log.New(io.Discard, "", 0)
	runInvestigatorTask(db, "investigator-1", b, logger)

	var count int
	db.QueryRow(`SELECT COUNT(*) FROM Escalations WHERE task_id = ?`, id).Scan(&count)
	if count != 1 {
		t.Errorf("expected 1 escalation row, got %d", count)
	}
}
