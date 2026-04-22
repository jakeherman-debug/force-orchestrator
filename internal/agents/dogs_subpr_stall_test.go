package agents

import (
	"strings"
	"testing"

	"force-orchestrator/internal/store"
)

// TestDogStalledReviews_SubPRCIStallEscalates verifies the added code path in
// dogStalledReviews: tasks in AwaitingSubPRCI with an old AskBranchPR (>12h)
// must generate an operator mail. Regression test for the original gap where
// AwaitingSubPRCI had no timeout and could hang forever.
func TestDogStalledReviews_SubPRCIStallEscalates(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	store.AddRepo(db, "api", "/tmp/api", "")
	cid, _ := store.CreateConvoy(db, "[1] t")
	tid, _ := store.AddConvoyTask(db, 0, "api", "fix", cid, 0, "Pending")
	db.Exec(`UPDATE BountyBoard SET status = 'AwaitingSubPRCI' WHERE id = ?`, tid)
	prID, _ := store.CreateAskBranchPR(db, tid, cid, "api", "u", 1)

	// Backdate the PR row by 13 hours.
	db.Exec(`UPDATE AskBranchPRs SET created_at = datetime('now', '-13 hours') WHERE id = ?`, prID)

	if err := dogStalledReviews(db, testLogger{}); err != nil {
		t.Fatalf("stalled-reviews: %v", err)
	}

	// Operator mail must reference the stalled task.
	var mailCount int
	db.QueryRow(`SELECT COUNT(*) FROM Fleet_Mail WHERE subject LIKE '[STALLED REVIEWS]%'`).Scan(&mailCount)
	if mailCount != 1 {
		t.Fatalf("expected 1 stalled-reviews mail, got %d", mailCount)
	}
	var body string
	db.QueryRow(`SELECT body FROM Fleet_Mail WHERE subject LIKE '[STALLED REVIEWS]%' LIMIT 1`).Scan(&body)
	if !strings.Contains(body, "AwaitingSubPRCI") {
		t.Errorf("mail body should mention AwaitingSubPRCI: %q", body)
	}
}

// TestDogStalledReviews_FreshSubPRCINoAlert proves that a recent sub-PR (inside
// the 12h threshold) does NOT trigger the stall mail.
func TestDogStalledReviews_FreshSubPRCINoAlert(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	store.AddRepo(db, "api", "/tmp/api", "")
	cid, _ := store.CreateConvoy(db, "[1] fresh")
	tid, _ := store.AddConvoyTask(db, 0, "api", "fix", cid, 0, "Pending")
	db.Exec(`UPDATE BountyBoard SET status = 'AwaitingSubPRCI' WHERE id = ?`, tid)
	_, _ = store.CreateAskBranchPR(db, tid, cid, "api", "u", 2)
	// Don't backdate — default created_at is now.

	_ = dogStalledReviews(db, testLogger{})
	var mailCount int
	db.QueryRow(`SELECT COUNT(*) FROM Fleet_Mail WHERE subject LIKE '[STALLED REVIEWS]%'`).Scan(&mailCount)
	if mailCount != 0 {
		t.Errorf("fresh sub-PR should not trigger stall mail, got %d", mailCount)
	}
}

// TestDogStalledReviews_MergedSubPRIgnored proves that a sub-PR whose state is
// already Merged doesn't count as "stalled" even if old.
func TestDogStalledReviews_MergedSubPRIgnored(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	store.AddRepo(db, "api", "/tmp/api", "")
	cid, _ := store.CreateConvoy(db, "[1] merged")
	tid, _ := store.AddConvoyTask(db, 0, "api", "done", cid, 0, "Pending")
	db.Exec(`UPDATE BountyBoard SET status = 'AwaitingSubPRCI' WHERE id = ?`, tid)
	prID, _ := store.CreateAskBranchPR(db, tid, cid, "api", "u", 3)
	db.Exec(`UPDATE AskBranchPRs SET created_at = datetime('now', '-13 hours') WHERE id = ?`, prID)
	_ = store.MarkAskBranchPRMerged(db, prID)

	_ = dogStalledReviews(db, testLogger{})
	var mailCount int
	db.QueryRow(`SELECT COUNT(*) FROM Fleet_Mail WHERE subject LIKE '[STALLED REVIEWS]%'`).Scan(&mailCount)
	if mailCount != 0 {
		t.Errorf("merged sub-PR should not be stalled, got %d mails", mailCount)
	}
}
