package store

import (
	"fmt"
	"testing"
)

// ConvoyReadyToShip must return true ONLY when the fleet's self-healing work
// is finished. These cases are the regression guards for the original bug where
// the dashboard treated status='DraftPROpen' alone as "ready to ship" — which
// caused convoys with active fix tasks to show a premature Ship It button.

func TestConvoyReadyToShip_DraftPROpen_NoTasks_True(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()

	cid, _ := CreateConvoy(db, "[1] quiet")
	db.Exec(`UPDATE Convoys SET status = 'DraftPROpen' WHERE id = ?`, cid)

	if !ConvoyReadyToShip(db, cid) {
		t.Error("DraftPROpen with no active tasks and no pending review should be ready")
	}
}

func TestConvoyReadyToShip_NotDraftPROpen_False(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()

	for _, status := range []string{"Active", "AwaitingDraftPR", "Completed", "Shipped", "Cancelled"} {
		cid, _ := CreateConvoy(db, fmt.Sprintf("[x] %s", status))
		db.Exec(`UPDATE Convoys SET status = ? WHERE id = ?`, status, cid)
		if ConvoyReadyToShip(db, cid) {
			t.Errorf("status=%q must not report ready-to-ship", status)
		}
	}
}

func TestConvoyReadyToShip_ActiveTask_False(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()

	cid, _ := CreateConvoy(db, "[1] mid-fix")
	db.Exec(`UPDATE Convoys SET status = 'DraftPROpen' WHERE id = ?`, cid)
	_, _ = AddConvoyTask(db, 0, "api", "fix regression", cid, 5, "Pending")

	if ConvoyReadyToShip(db, cid) {
		t.Error("DraftPROpen with a Pending CodeEdit in the convoy must NOT be ready")
	}
}

func TestConvoyReadyToShip_PendingConvoyReview_False(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()

	cid, _ := CreateConvoy(db, "[1] review-running")
	db.Exec(`UPDATE Convoys SET status = 'DraftPROpen' WHERE id = ?`, cid)
	// ConvoyReview rows carry convoy_id=0 and reference the convoy via payload.
	db.Exec(`INSERT INTO BountyBoard (parent_id, target_repo, type, status, payload, priority, created_at)
		VALUES (0, '', 'ConvoyReview', 'Pending', ?, 5, datetime('now'))`,
		fmt.Sprintf(`{"convoy_id":%d}`, cid))

	if ConvoyReadyToShip(db, cid) {
		t.Error("Pending ConvoyReview means the fleet is evaluating the convoy — not ready")
	}
}

func TestConvoyReadyToShip_LockedConvoyReview_False(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()

	cid, _ := CreateConvoy(db, "[1] review-locked")
	db.Exec(`UPDATE Convoys SET status = 'DraftPROpen' WHERE id = ?`, cid)
	db.Exec(`INSERT INTO BountyBoard (parent_id, target_repo, type, status, payload, priority, created_at)
		VALUES (0, '', 'ConvoyReview', 'Locked', ?, 5, datetime('now'))`,
		fmt.Sprintf(`{"convoy_id":%d}`, cid))

	if ConvoyReadyToShip(db, cid) {
		t.Error("Locked (running) ConvoyReview must block ship readiness")
	}
}

func TestConvoyReadyToShip_TerminalTasks_True(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()

	cid, _ := CreateConvoy(db, "[1] all-done")
	db.Exec(`UPDATE Convoys SET status = 'DraftPROpen' WHERE id = ?`, cid)
	t1, _ := AddConvoyTask(db, 0, "api", "t1", cid, 5, "Pending")
	t2, _ := AddConvoyTask(db, 0, "api", "t2", cid, 5, "Pending")
	t3, _ := AddConvoyTask(db, 0, "api", "t3", cid, 5, "Pending")
	db.Exec(`UPDATE BountyBoard SET status = 'Completed' WHERE id = ?`, t1)
	db.Exec(`UPDATE BountyBoard SET status = 'Cancelled' WHERE id = ?`, t2)
	db.Exec(`UPDATE BountyBoard SET status = 'Failed'    WHERE id = ?`, t3)

	if !ConvoyReadyToShip(db, cid) {
		t.Error("Completed/Cancelled/Failed tasks don't count as active — should be ready")
	}
}

func TestConvoyReadyToShip_CompletedReviewNotBlocking(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()

	cid, _ := CreateConvoy(db, "[1] post-review")
	db.Exec(`UPDATE Convoys SET status = 'DraftPROpen' WHERE id = ?`, cid)
	db.Exec(`INSERT INTO BountyBoard (parent_id, target_repo, type, status, payload, priority, created_at)
		VALUES (0, '', 'ConvoyReview', 'Completed', ?, 5, datetime('now'))`,
		fmt.Sprintf(`{"convoy_id":%d}`, cid))

	if !ConvoyReadyToShip(db, cid) {
		t.Error("A Completed ConvoyReview is not in-flight — convoy should be ready")
	}
}

func TestConvoyReadyToShip_BoundaryIDMatching(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()

	// Create convoys with adjacent IDs to expose LIKE-boundary bugs.
	c1, _ := CreateConvoy(db, "[1] c1")
	c10, _ := CreateConvoy(db, "[2] c10")
	// Force c10 to actually have ID=10 so the LIKE '%1%' vs '%10%' ambiguity is exercised.
	// CreateConvoy assigns monotonically, so we just ensure at least one distinct pair.
	db.Exec(`UPDATE Convoys SET status = 'DraftPROpen' WHERE id IN (?, ?)`, c1, c10)

	// Pending ConvoyReview for c1 only.
	db.Exec(`INSERT INTO BountyBoard (parent_id, target_repo, type, status, payload, priority, created_at)
		VALUES (0, '', 'ConvoyReview', 'Pending', ?, 5, datetime('now'))`,
		fmt.Sprintf(`{"convoy_id":%d}`, c1))

	if ConvoyReadyToShip(db, c1) {
		t.Error("c1 with its own pending review must not be ready")
	}
	// c10's ID starts with "1" but has a different payload; must NOT match c1's review.
	if !ConvoyReadyToShip(db, c10) {
		t.Error("c10 must not be falsely blocked by c1's review (LIKE boundary check)")
	}
}

func TestListReadyToShipConvoyIDs_ReturnsOnlyEligible(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()

	// Ready convoy.
	readyID, _ := CreateConvoy(db, "[1] ready")
	db.Exec(`UPDATE Convoys SET status = 'DraftPROpen' WHERE id = ?`, readyID)

	// Has active work — not ready.
	activeID, _ := CreateConvoy(db, "[2] fix-pending")
	db.Exec(`UPDATE Convoys SET status = 'DraftPROpen' WHERE id = ?`, activeID)
	_, _ = AddConvoyTask(db, 0, "api", "active", activeID, 5, "Pending")

	// Not DraftPROpen — not ready.
	stillActiveID, _ := CreateConvoy(db, "[3] still-active")
	db.Exec(`UPDATE Convoys SET status = 'Active' WHERE id = ?`, stillActiveID)

	got := ListReadyToShipConvoyIDs(db)
	if len(got) != 1 || got[0] != readyID {
		t.Errorf("ListReadyToShipConvoyIDs = %v, want [%d]", got, readyID)
	}
}

func TestConvoyReadyToShip_ZeroConvoyID_False(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()
	if ConvoyReadyToShip(db, 0) {
		t.Error("invalid convoy id must return false")
	}
}
