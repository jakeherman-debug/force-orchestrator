package agents

import (
	"context"
	"database/sql"
	"log"
	"os"
	"testing"
	"time"

	"force-orchestrator/internal/analytics"
	"force-orchestrator/internal/store"
)

// TestDisagreementDog_PersistsRatesAcrossWindows runs the dog once and
// asserts DisagreementPairs has rows for all five pair_names across all
// three rolling windows.
func TestDisagreementDog_PersistsRatesAcrossWindows(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	logger := log.New(os.Stderr, "", 0)

	seedAgentDisagreementFixture(t, db)

	if err := dogDisagreementTracker(context.Background(), db, logger); err != nil {
		t.Fatalf("dogDisagreementTracker: %v", err)
	}

	// All five canonical pair names should be present.
	var n int
	if err := db.QueryRow(`SELECT COUNT(DISTINCT pair_name) FROM DisagreementPairs`).Scan(&n); err != nil {
		t.Fatalf("count distinct pair_name: %v", err)
	}
	if n != 5 {
		t.Errorf("expected 5 distinct pair_names after one tick, got %d", n)
	}

	// Three rolling windows × five pairs = 15 rows on first tick.
	if err := db.QueryRow(`SELECT COUNT(*) FROM DisagreementPairs`).Scan(&n); err != nil {
		t.Fatalf("count rows: %v", err)
	}
	if n != 15 {
		t.Errorf("expected 15 rows (5 pairs × 3 windows) after one tick, got %d", n)
	}
}

// TestDisagreementDog_Idempotent runs the dog twice with no fixture
// changes and asserts the row count stays at 15 (UPSERT, not INSERT).
func TestDisagreementDog_Idempotent(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	logger := log.New(os.Stderr, "", 0)

	seedAgentDisagreementFixture(t, db)
	ctx := context.Background()

	if err := dogDisagreementTracker(ctx, db, logger); err != nil {
		t.Fatalf("first dogDisagreementTracker: %v", err)
	}
	var rowsAfterFirst int
	if err := db.QueryRow(`SELECT COUNT(*) FROM DisagreementPairs`).Scan(&rowsAfterFirst); err != nil {
		t.Fatalf("count rows after first: %v", err)
	}

	if err := dogDisagreementTracker(ctx, db, logger); err != nil {
		t.Fatalf("second dogDisagreementTracker: %v", err)
	}
	var rowsAfterSecond int
	if err := db.QueryRow(`SELECT COUNT(*) FROM DisagreementPairs`).Scan(&rowsAfterSecond); err != nil {
		t.Fatalf("count rows after second: %v", err)
	}

	if rowsAfterSecond != rowsAfterFirst {
		t.Errorf("idempotent re-tick added rows: first=%d, second=%d (UPSERT broken)",
			rowsAfterFirst, rowsAfterSecond)
	}
}

// TestDisagreementDog_RespectsEstop returns immediately when e-stop is
// flipped — no rows written.
func TestDisagreementDog_RespectsEstop(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	logger := log.New(os.Stderr, "", 0)

	SetEstop(db, true)
	defer SetEstop(db, false)

	if err := dogDisagreementTracker(context.Background(), db, logger); err != nil {
		t.Fatalf("dog returned error during e-stop: %v", err)
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM DisagreementPairs`).Scan(&n); err != nil {
		t.Fatalf("count rows: %v", err)
	}
	if n != 0 {
		t.Errorf("expected 0 rows during e-stop, got %d", n)
	}
}

// seedAgentDisagreementFixture inserts a small fixture so the dog has
// data to aggregate. Mirrors analytics.seedDisagreementFixture but
// lives in the agents package (Go does not let tests cross packages).
func seedAgentDisagreementFixture(t *testing.T, db *sql.DB) {
	t.Helper()
	now := time.Now().UTC()
	addHistory := func(taskID int, agent, outcome string, offset time.Duration) {
		ts := now.Add(-offset).Format("2006-01-02 15:04:05")
		_, err := db.Exec(`
			INSERT INTO TaskHistory (task_id, attempt, agent, session_id, claude_output, outcome, created_at)
			VALUES (?, 1, ?, 'session', '', ?, ?)
		`, taskID, agent, outcome, ts)
		if err != nil {
			t.Fatalf("insert TaskHistory: %v", err)
		}
	}
	addBounty := func(id, parentID int, taskType, status string, offset time.Duration) {
		ts := now.Add(-offset).Format("2006-01-02 15:04:05")
		_, err := db.Exec(`
			INSERT INTO BountyBoard (id, parent_id, type, status, created_at)
			VALUES (?, ?, ?, ?, ?)
		`, id, parentID, taskType, status, ts)
		if err != nil {
			t.Fatalf("insert BountyBoard: %v", err)
		}
	}

	// Captain approves four; council rejects one.
	for _, tid := range []int{100, 101, 102, 103} {
		addHistory(tid, "Captain-1", "Completed", 1*time.Hour)
	}
	addHistory(100, "Council-1", "Rejected", 30*time.Minute)

	// Council approves three with AwaitingSubPRCI; two later Failed.
	for _, tid := range []int{200, 201, 202} {
		addHistory(tid, "Council-1", "AwaitingSubPRCI", 1*time.Hour)
	}
	addHistory(200, "astromech-1", "Failed", 30*time.Minute)
	addHistory(201, "astromech-1", "Failed", 30*time.Minute)

	// ConvoyReview parent + two child fix tasks; one Failed.
	addBounty(300, 0, "ConvoyReview", "Reviewing", 2*time.Hour)
	addHistory(300, "ConvoyReview-1", "Completed", 2*time.Hour)
	addBounty(301, 300, "CodeEdit", "Pending", 1*time.Hour)
	addBounty(302, 300, "CodeEdit", "Pending", 1*time.Hour)
	addHistory(301, "astromech-1", "Failed", 30*time.Minute)
	addHistory(302, "astromech-1", "Completed", 30*time.Minute)

	// Sanity: ensure analytics package is reachable so a build-time
	// regression in the dog → analytics import path fails this test.
	_ = analytics.PairCaptainCouncilReject
}
