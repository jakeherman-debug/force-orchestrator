package agents

import (
	"io"
	"log"
	"testing"

	"force-orchestrator/internal/store"
)

// TestHandleInfraFailure_RemediationCarriesRepoAndConvoy is the regression
// test for the task-273 stuck-state bug. The remediation CodeEdit spawned
// on permanent infra failure must carry target_repo and convoy_id forward
// — otherwise astromechs claiming it fail with "DB Err: unknown target
// repository ''" and the task is permanently unclaimable.
func TestHandleInfraFailure_RemediationCarriesRepoAndConvoy(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	store.AddRepo(db, "force-orchestrator", "/tmp/force", "")

	cid, _ := store.CreateConvoy(db, "[1] test")
	taskID, _ := store.AddConvoyTask(db, 0, "force-orchestrator", "do thing", cid, 7, "Pending")
	// Pre-fill infra_failures so the next call trips the MaxInfraFailures cap.
	for i := 0; i < MaxInfraFailures-1; i++ {
		store.IncrementInfraFailures(db, taskID)
	}

	b, _ := store.GetBounty(db, taskID)
	logger := log.New(io.Discard, "", 0)

	handleInfraFailure(db, "BB-8", "claude", b, "sess-1", "Claude CLI Err: exit 1", "AwaitingCouncilReview", true, logger)

	// Find the spawned remediation task.
	var remID int
	var remRepo string
	var remConvoy int
	var remPriority int
	var remParent int
	err := db.QueryRow(`SELECT id, target_repo, convoy_id, priority, parent_id
		FROM BountyBoard
		WHERE parent_id = ? AND type = 'CodeEdit' AND status = 'Pending'
		ORDER BY id DESC LIMIT 1`, taskID).
		Scan(&remID, &remRepo, &remConvoy, &remPriority, &remParent)
	if err != nil {
		t.Fatalf("no remediation task found: %v", err)
	}

	if remRepo != "force-orchestrator" {
		t.Errorf("remediation task.target_repo = %q, want force-orchestrator", remRepo)
	}
	if remConvoy != cid {
		t.Errorf("remediation task.convoy_id = %d, want %d", remConvoy, cid)
	}
	if remPriority != 7 {
		t.Errorf("remediation task.priority = %d, want 7", remPriority)
	}
	if remParent != taskID {
		t.Errorf("remediation task.parent_id = %d, want %d", remParent, taskID)
	}
}

