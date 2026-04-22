package agents

import (
	"encoding/json"
	"fmt"
	"testing"

	"force-orchestrator/internal/store"
)

// seedConvoyReviewTask inserts a ConvoyReview BountyBoard row for the given convoy.
func seedConvoyReviewTask(t *testing.T, db interface {
	Exec(string, ...any) (interface{ LastInsertId() (int64, error) }, error)
}, convoyID int) int {
	t.Helper()
	return 0 // placeholder — use raw sql.DB below
}

// stubConvoyReviewLLM installs a stub CLI runner returning the given result JSON.
func stubConvoyReviewLLM(t *testing.T, result convoyReviewResult) {
	t.Helper()
	raw, _ := json.Marshal(result)
	withStubCLIRunner(t, string(raw), nil)
}

// ── QueueConvoyReview dedup ──────────────────────────────────────────────────

func TestQueueConvoyReview_Idempotent(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	id1, err := QueueConvoyReview(db, 7)
	if err != nil || id1 == 0 {
		t.Fatalf("first queue: id=%d err=%v", id1, err)
	}
	id2, err := QueueConvoyReview(db, 7)
	if err != nil {
		t.Fatalf("second queue: %v", err)
	}
	if id2 != 0 {
		t.Errorf("expected dedup (id=0), got id=%d", id2)
	}
}

func TestQueueConvoyReview_AllowsAfterCompleted(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	id1, _ := QueueConvoyReview(db, 8)
	// Mark completed so a new one can be queued.
	db.Exec(`UPDATE BountyBoard SET status = 'Completed' WHERE id = ?`, id1)

	id2, err := QueueConvoyReview(db, 8)
	if err != nil || id2 == 0 {
		t.Errorf("expected new task after completed; id=%d err=%v", id2, err)
	}
}

// ── runConvoyReview — clean pass ─────────────────────────────────────────────

func TestRunConvoyReview_CleanPass_Completes(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	convoyID := seedDraftPROpenConvoy(t, db)
	// Seed a CodeEdit task so summarizeConvoyTasks has something.
	db.Exec(`INSERT INTO BountyBoard (parent_id, target_repo, type, status, payload, convoy_id, priority, created_at)
		VALUES (0, 'api', 'CodeEdit', 'Completed', 'add rate limit patterns', ?, 5, datetime('now'))`, convoyID)

	stubConvoyReviewLLM(t, convoyReviewResult{Status: "clean", Findings: nil})

	payload, _ := json.Marshal(convoyReviewPayload{ConvoyID: convoyID})
	bounty := &store.Bounty{
		ID:      999,
		Type:    "ConvoyReview",
		Payload: string(payload),
	}
	db.Exec(`INSERT INTO BountyBoard (id, parent_id, target_repo, type, status, payload, priority, created_at)
		VALUES (999, 0, '', 'ConvoyReview', 'Locked', ?, 5, datetime('now'))`, string(payload))

	runConvoyReview(db, "Diplomat-1", bounty, testLogger{})

	var status string
	db.QueryRow(`SELECT status FROM BountyBoard WHERE id = 999`).Scan(&status)
	if status != "Completed" {
		t.Errorf("expected Completed, got %s", status)
	}

	// No fix tasks should have been spawned.
	var fixCount int
	db.QueryRow(`SELECT COUNT(*) FROM BountyBoard WHERE parent_id = 999 AND type = 'CodeEdit'`).Scan(&fixCount)
	if fixCount != 0 {
		t.Errorf("expected 0 fix tasks, got %d", fixCount)
	}
}

// ── runConvoyReview — needs_work spawns fix tasks ───────────────────────────

func TestRunConvoyReview_NeedsWork_SpawnsFixTasks(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	convoyID := seedDraftPROpenConvoy(t, db)

	stubConvoyReviewLLM(t, convoyReviewResult{
		Status: "needs_work",
		Findings: []convoyReviewFinding{
			{Type: "regression", Description: "Flusher removed", Fix: "Restore Flusher flush in handleHolonetStream", Repo: "api"},
			{Type: "gap", Description: "rateLimitPatterns not updated", Fix: "Add stream idle timeout to rateLimitPatterns", Repo: "api"},
		},
	})

	payload, _ := json.Marshal(convoyReviewPayload{ConvoyID: convoyID})
	bounty := &store.Bounty{
		ID:      998,
		Type:    "ConvoyReview",
		Payload: string(payload),
	}
	db.Exec(`INSERT INTO BountyBoard (id, parent_id, target_repo, type, status, payload, priority, created_at)
		VALUES (998, 0, '', 'ConvoyReview', 'Locked', ?, 5, datetime('now'))`, string(payload))

	runConvoyReview(db, "Diplomat-1", bounty, testLogger{})

	var status string
	db.QueryRow(`SELECT status FROM BountyBoard WHERE id = 998`).Scan(&status)
	if status != "Completed" {
		t.Errorf("expected Completed after spawning fix tasks, got %s", status)
	}

	var fixCount int
	db.QueryRow(`SELECT COUNT(*) FROM BountyBoard WHERE parent_id = 998 AND type = 'CodeEdit'`).Scan(&fixCount)
	if fixCount != 2 {
		t.Errorf("expected 2 fix tasks, got %d", fixCount)
	}

	// Fix tasks should be pinned to the ask-branch.
	var branchCount int
	db.QueryRow(`SELECT COUNT(*) FROM BountyBoard WHERE parent_id = 998 AND branch_name = 'force/ask-1-test'`).Scan(&branchCount)
	if branchCount != 2 {
		t.Errorf("expected fix tasks pinned to ask-branch, got %d with branch set", branchCount)
	}
}

// ── runConvoyReview — active convoy tasks block fix spawning ─────────────────

func TestRunConvoyReview_ActiveConvoyTasks_NoSpawn(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	convoyID := seedDraftPROpenConvoy(t, db)

	// Seed an active (Pending) non-infrastructure task in the convoy.
	db.Exec(`INSERT INTO BountyBoard (parent_id, target_repo, type, status, payload, convoy_id, priority, created_at)
		VALUES (0, 'api', 'CodeEdit', 'Pending', 'in-flight work', ?, 5, datetime('now'))`, convoyID)

	stubConvoyReviewLLM(t, convoyReviewResult{
		Status: "needs_work",
		Findings: []convoyReviewFinding{
			{Type: "gap", Description: "missing feature", Fix: "add it", Repo: "api"},
		},
	})

	payload, _ := json.Marshal(convoyReviewPayload{ConvoyID: convoyID})
	bounty := &store.Bounty{ID: 994, Type: "ConvoyReview", Payload: string(payload)}
	db.Exec(`INSERT INTO BountyBoard (id, parent_id, target_repo, type, status, payload, priority, created_at)
		VALUES (994, 0, '', 'ConvoyReview', 'Locked', ?, 5, datetime('now'))`, string(payload))

	runConvoyReview(db, "Diplomat-1", bounty, testLogger{})

	// Should complete without spawning any fix tasks.
	var status string
	db.QueryRow(`SELECT status FROM BountyBoard WHERE id = 994`).Scan(&status)
	if status != "Completed" {
		t.Errorf("expected Completed, got %s", status)
	}
	var fixCount int
	db.QueryRow(`SELECT COUNT(*) FROM BountyBoard WHERE parent_id = 994`).Scan(&fixCount)
	if fixCount != 0 {
		t.Errorf("expected 0 fix tasks (diff still moving), got %d", fixCount)
	}
}

func TestDogConvoyReviewWatch_SkipsWhenActiveConvoyTasks(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	convoyID := seedDraftPROpenConvoy(t, db)

	// Seed an active non-infrastructure task directly in the convoy (not via a ConvoyReview parent).
	db.Exec(`INSERT INTO BountyBoard (parent_id, target_repo, type, status, payload, convoy_id, priority, created_at)
		VALUES (0, 'api', 'CodeEdit', 'Pending', 'in-flight work', ?, 5, datetime('now'))`, convoyID)

	dogConvoyReviewWatch(db, testLogger{})

	var count int
	db.QueryRow(`SELECT COUNT(*) FROM BountyBoard WHERE type = 'ConvoyReview' AND status IN ('Pending','Locked')
		AND (payload LIKE '%"convoy_id":' || ? || ',%' OR payload LIKE '%"convoy_id":' || ? || '}%')`,
		convoyID, convoyID).Scan(&count)
	if count != 0 {
		t.Errorf("expected no ConvoyReview queued while active tasks exist, got %d", count)
	}
}

// ── runConvoyReview — max findings cap ──────────────────────────────────────

func TestRunConvoyReview_MaxFindingsCap(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	convoyID := seedDraftPROpenConvoy(t, db)

	// Return 8 findings — cap is 5 by default.
	findings := make([]convoyReviewFinding, 8)
	for i := range findings {
		findings[i] = convoyReviewFinding{
			Type: "gap", Description: fmt.Sprintf("gap %d", i),
			Fix: fmt.Sprintf("fix %d", i), Repo: "api",
		}
	}
	stubConvoyReviewLLM(t, convoyReviewResult{Status: "needs_work", Findings: findings})

	payload, _ := json.Marshal(convoyReviewPayload{ConvoyID: convoyID})
	bounty := &store.Bounty{ID: 997, Type: "ConvoyReview", Payload: string(payload)}
	db.Exec(`INSERT INTO BountyBoard (id, parent_id, target_repo, type, status, payload, priority, created_at)
		VALUES (997, 0, '', 'ConvoyReview', 'Locked', ?, 5, datetime('now'))`, string(payload))

	runConvoyReview(db, "Diplomat-1", bounty, testLogger{})

	var fixCount int
	db.QueryRow(`SELECT COUNT(*) FROM BountyBoard WHERE parent_id = 997 AND type = 'CodeEdit'`).Scan(&fixCount)
	if fixCount != 5 {
		t.Errorf("expected 5 fix tasks (cap), got %d", fixCount)
	}
}

// ── runConvoyReview — loop cap escalates ────────────────────────────────────

func TestRunConvoyReview_LoopCapEscalates(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	convoyID := seedDraftPROpenConvoy(t, db)

	// Seed 5 prior completed ConvoyReview tasks for this convoy.
	for i := 0; i < 5; i++ {
		p, _ := json.Marshal(convoyReviewPayload{ConvoyID: convoyID})
		db.Exec(`INSERT INTO BountyBoard (parent_id, target_repo, type, status, payload, priority, created_at)
			VALUES (0, '', 'ConvoyReview', 'Completed', ?, 5, datetime('now'))`, string(p))
	}

	payload, _ := json.Marshal(convoyReviewPayload{ConvoyID: convoyID})
	bounty := &store.Bounty{ID: 996, Type: "ConvoyReview", Payload: string(payload)}
	db.Exec(`INSERT INTO BountyBoard (id, parent_id, target_repo, type, status, payload, priority, created_at)
		VALUES (996, 0, '', 'ConvoyReview', 'Locked', ?, 5, datetime('now'))`, string(payload))

	// No LLM stub needed — loop cap check fires before LLM call.
	runConvoyReview(db, "Diplomat-1", bounty, testLogger{})

	var status string
	db.QueryRow(`SELECT status FROM BountyBoard WHERE id = 996`).Scan(&status)
	if status != "Failed" {
		t.Errorf("expected Failed (escalated), got %s", status)
	}

	var escCount int
	db.QueryRow(`SELECT COUNT(*) FROM Escalations WHERE task_id = 996`).Scan(&escCount)
	if escCount != 1 {
		t.Errorf("expected 1 escalation, got %d", escCount)
	}
}

// ── dogConvoyReviewWatch ─────────────────────────────────────────────────────

func TestDogConvoyReviewWatch_QueuesForDraftPROpen(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	convoyID := seedDraftPROpenConvoy(t, db)

	if err := dogConvoyReviewWatch(db, testLogger{}); err != nil {
		t.Fatalf("dog error: %v", err)
	}

	var count int
	db.QueryRow(`SELECT COUNT(*) FROM BountyBoard WHERE type = 'ConvoyReview' AND status IN ('Pending','Locked')
		AND (payload LIKE '%"convoy_id":' || ? || ',%' OR payload LIKE '%"convoy_id":' || ? || '}%')`,
		convoyID, convoyID).Scan(&count)
	if count != 1 {
		t.Errorf("expected 1 ConvoyReview queued, got %d", count)
	}
}

func TestDogConvoyReviewWatch_SkipsWhenPendingExists(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	convoyID := seedDraftPROpenConvoy(t, db)
	// Pre-queue one.
	QueueConvoyReview(db, convoyID)

	dogConvoyReviewWatch(db, testLogger{})

	var count int
	db.QueryRow(`SELECT COUNT(*) FROM BountyBoard WHERE type = 'ConvoyReview'
		AND (payload LIKE '%"convoy_id":' || ? || ',%' OR payload LIKE '%"convoy_id":' || ? || '}%')`,
		convoyID, convoyID).Scan(&count)
	if count != 1 {
		t.Errorf("expected exactly 1 ConvoyReview (dedup), got %d", count)
	}
}

func TestDogConvoyReviewWatch_SkipsWhenActiveFixTasks(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	convoyID := seedDraftPROpenConvoy(t, db)

	// Simulate a completed ConvoyReview that spawned a still-active fix task.
	p, _ := json.Marshal(convoyReviewPayload{ConvoyID: convoyID})
	res, _ := db.Exec(`INSERT INTO BountyBoard (parent_id, target_repo, type, status, payload, priority, created_at)
		VALUES (0, '', 'ConvoyReview', 'Completed', ?, 5, datetime('now'))`, string(p))
	reviewID, _ := res.LastInsertId()

	db.Exec(`INSERT INTO BountyBoard (parent_id, target_repo, type, status, payload, convoy_id, priority, created_at)
		VALUES (?, 'api', 'CodeEdit', 'Pending', 'fix regression', ?, 5, datetime('now'))`, reviewID, convoyID)

	dogConvoyReviewWatch(db, testLogger{})

	// No new ConvoyReview should be queued while fix task is still running.
	var count int
	db.QueryRow(`SELECT COUNT(*) FROM BountyBoard WHERE type = 'ConvoyReview' AND status IN ('Pending','Locked')
		AND (payload LIKE '%"convoy_id":' || ? || ',%' OR payload LIKE '%"convoy_id":' || ? || '}%')`,
		convoyID, convoyID).Scan(&count)
	if count != 0 {
		t.Errorf("expected 0 new ConvoyReview while fix tasks active, got %d", count)
	}
}
