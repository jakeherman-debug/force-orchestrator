package dashboard

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"force-orchestrator/internal/store"
)

// fetchStatus invokes handleStatus and returns the decoded DashboardStatus.
func fetchStatus(t *testing.T, db *sql.DB) DashboardStatus {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, "/api/status", nil)
	w := httptest.NewRecorder()
	handleStatus(db)(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status code %d: %s", w.Code, w.Body.String())
	}
	var s DashboardStatus
	if err := json.Unmarshal(w.Body.Bytes(), &s); err != nil {
		t.Fatalf("status response not JSON: %v", err)
	}
	return s
}

// TestHandleStatus_ReadyToShip_OnlyCountsQuiescedConvoys asserts the strict
// ship-gate: merely being DraftPROpen isn't enough. A convoy with pending
// fix tasks or an unresolved rebase conflict must NOT appear in the count,
// because the fleet is still doing work on it. Regression guard against the
// original bug where "DraftPROpen" alone made convoys 35/37 look ready.
func TestHandleStatus_ReadyToShip_OnlyCountsQuiescedConvoys(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	// Convoy A — DraftPROpen, zero pending work → ready.
	cidA, _ := store.CreateConvoy(db, "[1] ready")
	db.Exec(`UPDATE Convoys SET status = 'DraftPROpen' WHERE id = ?`, cidA)

	// Convoy B — DraftPROpen but has a pending CodeEdit (e.g. a ConvoyReview
	// fix task still in flight) → NOT ready.
	cidB, _ := store.CreateConvoy(db, "[2] fix-pending")
	db.Exec(`UPDATE Convoys SET status = 'DraftPROpen' WHERE id = ?`, cidB)
	_, _ = store.AddConvoyTask(db, 0, "api", "fix regression", cidB, 5, "Pending")

	// Convoy C — DraftPROpen with a Pending ConvoyReview (convoy_id=0 but
	// payload references this convoy) → NOT ready.
	cidC, _ := store.CreateConvoy(db, "[3] review-pending")
	db.Exec(`UPDATE Convoys SET status = 'DraftPROpen' WHERE id = ?`, cidC)
	db.Exec(`INSERT INTO BountyBoard (parent_id, target_repo, type, status, payload, priority, created_at)
		VALUES (0, '', 'ConvoyReview', 'Pending', ?, 5, datetime('now'))`,
		fmt.Sprintf(`{"convoy_id":%d}`, cidC))

	// Convoy D — Active state entirely → NOT ready.
	cidD, _ := store.CreateConvoy(db, "[4] active")
	db.Exec(`UPDATE Convoys SET status = 'Active' WHERE id = ?`, cidD)

	s := fetchStatus(t, db)
	if s.ReadyToShip != 1 {
		t.Errorf("ready_to_ship = %d, want 1 (only convoy A qualifies)", s.ReadyToShip)
	}
}

// TestHandleStatus_ReadyToShip_IgnoresTerminalTasks confirms that Completed /
// Cancelled / Failed tasks don't block ship readiness — those are the states
// a task ends in, not work-in-flight.
func TestHandleStatus_ReadyToShip_IgnoresTerminalTasks(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	cid, _ := store.CreateConvoy(db, "[1] done")
	db.Exec(`UPDATE Convoys SET status = 'DraftPROpen' WHERE id = ?`, cid)
	// Multiple tasks all in terminal states.
	t1, _ := store.AddConvoyTask(db, 0, "api", "t1", cid, 5, "Pending")
	t2, _ := store.AddConvoyTask(db, 0, "api", "t2", cid, 5, "Pending")
	t3, _ := store.AddConvoyTask(db, 0, "api", "t3", cid, 5, "Pending")
	db.Exec(`UPDATE BountyBoard SET status = 'Completed' WHERE id = ?`, t1)
	db.Exec(`UPDATE BountyBoard SET status = 'Cancelled' WHERE id = ?`, t2)
	db.Exec(`UPDATE BountyBoard SET status = 'Failed'    WHERE id = ?`, t3)

	s := fetchStatus(t, db)
	if s.ReadyToShip != 1 {
		t.Errorf("ready_to_ship = %d, want 1 (terminal tasks shouldn't block)", s.ReadyToShip)
	}
}

// TestHandleStatus_ReadyToShip_ZeroWhenNoDraftPROpen confirms the count stays
// at zero when no convoy is actually awaiting an operator ship-it click.
func TestHandleStatus_ReadyToShip_ZeroWhenNoDraftPROpen(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	_, _ = store.CreateConvoy(db, "[1] still active")

	s := fetchStatus(t, db)
	if s.ReadyToShip != 0 {
		t.Errorf("ready_to_ship = %d, want 0", s.ReadyToShip)
	}
}

// TestTaskDetail_ConvoyStatus_Populated verifies the parent convoy's status
// is surfaced on task detail. ConvoyStatus is informational; ConvoyReadyToShip
// is the flag the UI should branch on for the Ship It shortcut.
func TestTaskDetail_ConvoyStatus_Populated(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	store.AddRepo(db, "api", "/tmp/api", "")
	cid, _ := store.CreateConvoy(db, "[1] ship ready")
	db.Exec(`UPDATE Convoys SET status = 'DraftPROpen' WHERE id = ?`, cid)
	taskID, _ := store.AddConvoyTask(db, 0, "api", "task", cid, 5, "Pending")

	d := fetchTaskDetail(t, db, taskID)
	if d.ConvoyStatus != "DraftPROpen" {
		t.Errorf("convoy_status = %q, want DraftPROpen", d.ConvoyStatus)
	}
	// Because the task itself is Pending, the convoy is NOT ready to ship —
	// the task IS the active work. The UI must not render the Ship button.
	if d.ConvoyReadyToShip {
		t.Error("convoy_ready_to_ship must be false while the owning task is still Pending")
	}
}

// TestTaskDetail_ConvoyReadyToShip_TrueWhenQuiesced verifies the ship shortcut
// flag flips to true only once every task in the convoy is terminal.
func TestTaskDetail_ConvoyReadyToShip_TrueWhenQuiesced(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	store.AddRepo(db, "api", "/tmp/api", "")
	cid, _ := store.CreateConvoy(db, "[1] quiesced")
	db.Exec(`UPDATE Convoys SET status = 'DraftPROpen' WHERE id = ?`, cid)
	// One task, already Completed — simulates a convoy whose work is done.
	taskID, _ := store.AddConvoyTask(db, 0, "api", "task", cid, 5, "Pending")
	db.Exec(`UPDATE BountyBoard SET status = 'Completed' WHERE id = ?`, taskID)

	d := fetchTaskDetail(t, db, taskID)
	if !d.ConvoyReadyToShip {
		t.Error("convoy_ready_to_ship must be true when every convoy task is terminal")
	}
}

// TestTaskDetail_ConvoyStatus_EmptyWhenNoConvoy ensures tasks with no parent
// convoy don't try to look up a convoy row (convoy_id=0) and produce a bogus
// value the frontend would then use to render a Ship It button.
func TestTaskDetail_ConvoyStatus_EmptyWhenNoConvoy(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	store.AddRepo(db, "api", "/tmp/api", "")
	// convoy_id stays 0 — task is a bare Feature/standalone CodeEdit.
	res, _ := db.Exec(`INSERT INTO BountyBoard (parent_id, target_repo, type, status, payload, created_at)
		VALUES (0, 'api', 'CodeEdit', 'Pending', 'p', datetime('now'))`)
	id, _ := res.LastInsertId()

	d := fetchTaskDetail(t, db, int(id))
	if d.ConvoyStatus != "" {
		t.Errorf("convoy_status should be empty for convoy-less tasks, got %q", d.ConvoyStatus)
	}
}
