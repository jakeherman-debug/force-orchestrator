package agents

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"force-orchestrator/internal/store"
)

// TestFix8c_AUDIT143_ClassifierCapCreatesEscalation is the behavioural
// regression for the AUDIT-143 closure. Before Fix #8c, exhausting the
// classifier retry budget (classifyAttemptsCap = 3) silently flagged the
// comment row via MarkClassifyUnrecoverable — no Escalations row, no
// operator dashboard signal. After Fix #8c, the cap branch also calls
// CreateEscalation(db, parentTaskID, SeverityMedium, msg) so the row
// lands on the escalations dashboard.
//
// The test drives the classifier with a CLI stub that always returns
// malformed JSON (classifyPRReviewComment wraps the parse error and
// returns it), seeds the row's classify_attempts at the cap boundary
// (2 — one more failing attempt will hit the cap), runs the triage
// handler once, and asserts:
//
//   - classify_attempts incremented to ≥ 3 on the comment row
//   - classification flipped to 'human' (MarkClassifyUnrecoverable side)
//   - an Escalations row exists for the parent PRReviewTriage bounty
//     with severity='MEDIUM' and a message naming the comment
func TestFix8c_AUDIT143_ClassifierCapCreatesEscalation(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	convoyID := seedDraftPROpenConvoy(t, db)
	rowID := seedPRCommentForTriage(t, db, convoyID, "bot", "some feedback")

	// Pre-seed classify_attempts at 2 so the next failing call hits the
	// cap (attempts := IncrementClassifyAttempts returns 3 >= cap 3).
	if _, err := db.Exec(
		`UPDATE PRReviewComments SET classify_attempts = 2 WHERE id = ?`, rowID,
	); err != nil {
		t.Fatalf("pre-seed attempts: %v", err)
	}

	// Stub the LLM to always produce invalid JSON. classifyPRReviewComment
	// runs strictJSONUnmarshal which routes the parse error up to the
	// dispatcher's cap branch.
	withStubCLIRunner(t, "not valid json at all", nil)

	taskID := queuePRReviewTriageTask(t, db, convoyID)
	bounty, _ := store.GetBounty(db, taskID)
	bounty.Status = "Locked"
	runPRReviewTriage(context.Background(), db, "Diplomat", bounty, mustLoadCapProfile(t, "pr-review-triage"), testLogger{})

	// ── Assertions ───────────────────────────────────────────────────
	// 1. classify_attempts reached at least the cap.
	var attempts int
	db.QueryRow(`SELECT classify_attempts FROM PRReviewComments WHERE id = ?`, rowID).Scan(&attempts)
	if attempts < 3 {
		t.Errorf("classify_attempts = %d, want >= 3 (cap)", attempts)
	}

	// 2. Row classification flipped to 'human' by MarkClassifyUnrecoverable.
	after := store.GetPRReviewComment(db, rowID)
	if after.Classification != "human" {
		t.Errorf("classification = %q after cap, want 'human'", after.Classification)
	}

	// 3. Escalations row created on the parent PRReviewTriage task.
	var (
		escID    int
		escSev   string
		escMsg   string
		escStat  string
	)
	err := db.QueryRow(
		`SELECT id, severity, message, status FROM Escalations WHERE task_id = ? ORDER BY id DESC LIMIT 1`,
		taskID,
	).Scan(&escID, &escSev, &escMsg, &escStat)
	if err != nil {
		t.Fatalf("AUDIT-143: no Escalations row created for PRReviewTriage task %d after classifier cap: %v",
			taskID, err)
	}
	if escSev != "MEDIUM" {
		t.Errorf("Escalations.severity = %q, want MEDIUM", escSev)
	}
	if escStat != "Open" {
		t.Errorf("Escalations.status = %q, want Open", escStat)
	}
	// The message should name the comment ID so the operator can pivot
	// from the escalation into the PR review dashboard.
	wantSubstr := fmt.Sprintf("comment #%d", rowID)
	if !strings.Contains(escMsg, wantSubstr) {
		t.Errorf("Escalations.message = %q, want substring %q", escMsg, wantSubstr)
	}
}
