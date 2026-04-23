package agents

import (
	"io"
	"log"
	"strings"
	"testing"

	"force-orchestrator/internal/store"
)

// TestHandleInfraFailure_AtCap_BubblesToCommander replaces the old
// "remediation CodeEdit" pattern — the at-cap path now queues a Decompose
// task so Commander re-plans the work into smaller shards. Repeated infra
// crashes on the same task are almost always a "task too large" problem, and
// Commander is the agent equipped to re-decompose.
func TestHandleInfraFailure_AtCap_BubblesToCommander(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	store.AddRepo(db, "force-orchestrator", "/tmp/force", "")

	cid, _ := store.CreateConvoy(db, "[1] test")
	taskID, _ := store.AddConvoyTask(db, 0, "force-orchestrator", "do a massive refactor spanning many files", cid, 7, "Pending")
	for i := 0; i < MaxInfraFailures-1; i++ {
		store.IncrementInfraFailures(db, taskID)
	}

	b, _ := store.GetBounty(db, taskID)
	logger := log.New(io.Discard, "", 0)

	handleInfraFailure(db, "BB-8", "claude", b, "sess-1", "Claude CLI Err: exit 1", "AwaitingCouncilReview", true, logger)

	// Original task must be Failed (existing behavior).
	updated, _ := store.GetBounty(db, taskID)
	if updated.Status != "Failed" {
		t.Errorf("original task should be Failed, got %q", updated.Status)
	}

	// A Decompose bounty should have been spawned, carrying the repo / convoy /
	// priority from the failed task so Commander can re-plan in the right repo.
	var dID int
	var dType, dStatus, dRepo, dPayload string
	var dConvoy, dPriority, dParent int
	err := db.QueryRow(`SELECT id, type, status, target_repo, convoy_id, priority, parent_id, payload
		FROM BountyBoard WHERE parent_id = ? AND type = 'Decompose'`, taskID).
		Scan(&dID, &dType, &dStatus, &dRepo, &dConvoy, &dPriority, &dParent, &dPayload)
	if err != nil {
		t.Fatalf("no Decompose task found: %v", err)
	}
	if dStatus != "Pending" {
		t.Errorf("Decompose should be Pending, got %q", dStatus)
	}
	if dRepo != "force-orchestrator" {
		t.Errorf("Decompose target_repo = %q, want force-orchestrator", dRepo)
	}
	if dConvoy != cid {
		t.Errorf("Decompose convoy_id = %d, want %d", dConvoy, cid)
	}
	if dPriority != 7 {
		t.Errorf("Decompose priority = %d, want 7", dPriority)
	}
	if dParent != taskID {
		t.Errorf("Decompose parent_id = %d, want %d", dParent, taskID)
	}
	if !strings.Contains(dPayload, "INFRA_FAILURE_RESHARD") {
		t.Error("Decompose payload should be marked INFRA_FAILURE_RESHARD so Commander recognizes the context")
	}
	if !strings.Contains(dPayload, "do a massive refactor") {
		t.Error("Decompose payload should include original task text")
	}
}

// TestHandleInfraFailure_AtCap_IdempotentReshard verifies that triggering the
// at-cap path twice on the same failed task (e.g. multiple stages failing
// simultaneously) does not produce two Decompose bounties.
func TestHandleInfraFailure_AtCap_IdempotentReshard(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	store.AddRepo(db, "api", "/tmp/api", "")

	taskID, _ := store.AddConvoyTask(db, 0, "api", "do thing", 1, 5, "Pending")
	for i := 0; i < MaxInfraFailures-1; i++ {
		store.IncrementInfraFailures(db, taskID)
	}

	b, _ := store.GetBounty(db, taskID)
	logger := log.New(io.Discard, "", 0)

	// First at-cap fire spawns the Decompose.
	handleInfraFailure(db, "BB-8", "claude", b, "s1", "err1", "Pending", true, logger)
	// Reset the task to Pending with the cap-1 count so a second call re-trips
	// the at-cap branch — simulating two stages racing on the same task.
	db.Exec(`UPDATE BountyBoard SET status = 'Pending', infra_failures = ? WHERE id = ?`, MaxInfraFailures-1, taskID)
	b2, _ := store.GetBounty(db, taskID)
	handleInfraFailure(db, "BB-8", "claude", b2, "s2", "err2", "Pending", true, logger)

	var count int
	db.QueryRow(`SELECT COUNT(*) FROM BountyBoard WHERE parent_id = ? AND type = 'Decompose'`, taskID).Scan(&count)
	if count != 1 {
		t.Errorf("expected exactly 1 Decompose (idempotent), got %d", count)
	}
}

// TestHandleInfraFailure_BelowCap_NoReshard ensures that a non-terminal infra
// failure does not prematurely hand the task to Commander — we only reshard
// once all retries are exhausted.
func TestHandleInfraFailure_BelowCap_NoReshard(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	store.AddRepo(db, "api", "/tmp/api", "")
	taskID, _ := store.AddConvoyTask(db, 0, "api", "a task", 1, 5, "Pending")

	b, _ := store.GetBounty(db, taskID)
	logger := log.New(io.Discard, "", 0)
	handleInfraFailure(db, "BB-8", "claude", b, "s1", "err", "Pending", true, logger)

	var count int
	db.QueryRow(`SELECT COUNT(*) FROM BountyBoard WHERE parent_id = ? AND type = 'Decompose'`, taskID).Scan(&count)
	if count != 0 {
		t.Errorf("below cap must not queue Decompose, got %d", count)
	}
}
