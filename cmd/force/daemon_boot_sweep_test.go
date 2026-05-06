package main

import (
	"context"
	"testing"
	"time"

	"force-orchestrator/internal/store"
)

// TestRunBootSweep_NilDB returns an error.
func TestRunBootSweep_NilDB(t *testing.T) {
	if err := runBootSweep(context.Background(), nil); err == nil {
		t.Fatalf("expected error for nil db, got nil")
	}
}

// TestRunBootSweep_HappyPath_Empty returns nil on a fresh DB with no
// stale rows in any of the four sweep classes.
func TestRunBootSweep_HappyPath_Empty(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	if err := runBootSweep(context.Background(), db); err != nil {
		t.Errorf("clean DB sweep returned error: %v", err)
	}
}

// TestRunBootSweep_ReleasesStaleLockedTasks: tasks marked Locked before
// the sweep are released back to Pending.
func TestRunBootSweep_ReleasesStaleLockedTasks(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	// Seed a Locked row.
	if _, err := db.Exec(`INSERT INTO BountyBoard
		(target_repo, branch_name, type, owner, status, locked_at)
		VALUES ('repo', 'b', 'Test', 'AgentX', 'Locked', datetime('now'))`); err != nil {
		t.Fatalf("seed: %v", err)
	}

	if err := runBootSweep(context.Background(), db); err != nil {
		t.Fatalf("runBootSweep: %v", err)
	}

	var status string
	var owner string
	if err := db.QueryRow(`SELECT status, owner FROM BountyBoard LIMIT 1`).Scan(&status, &owner); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if status != "Pending" {
		t.Errorf("status = %q, want Pending", status)
	}
	if owner != "" {
		t.Errorf("owner = %q, want empty", owner)
	}
}

// TestClearStaleDogHeartbeats: heartbeats older than the threshold are
// cleared; recent ones are preserved.
func TestClearStaleDogHeartbeats(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	// One stale (15 min ago) + one fresh (now).
	if _, err := db.Exec(`INSERT INTO Dogs (name, last_run_at, run_count, heartbeat_at)
		VALUES ('stale-dog', datetime('now', '-1 hour'), 1, datetime('now', '-15 minutes'))`); err != nil {
		t.Fatalf("seed stale: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO Dogs (name, last_run_at, run_count, heartbeat_at)
		VALUES ('fresh-dog', datetime('now'), 1, datetime('now'))`); err != nil {
		t.Fatalf("seed fresh: %v", err)
	}

	cleared, err := clearStaleDogHeartbeats(db, 10*time.Minute)
	if err != nil {
		t.Fatalf("clearStaleDogHeartbeats: %v", err)
	}
	if cleared != 1 {
		t.Errorf("expected 1 cleared, got %d", cleared)
	}

	var hb string
	_ = db.QueryRow(`SELECT IFNULL(heartbeat_at, '') FROM Dogs WHERE name = 'stale-dog'`).Scan(&hb)
	if hb != "" {
		t.Errorf("stale-dog heartbeat = %q, want empty", hb)
	}
	_ = db.QueryRow(`SELECT IFNULL(heartbeat_at, '') FROM Dogs WHERE name = 'fresh-dog'`).Scan(&hb)
	if hb == "" {
		t.Errorf("fresh-dog heartbeat cleared, want preserved")
	}
}

// TestFindHalfBakedDraftPROpenConvoys: a DraftPROpen convoy with
// draft_pr_url set but no PRHandoffSyntheses row is detected.
func TestFindHalfBakedDraftPROpenConvoys(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	// Half-baked: DraftPROpen + URL + no synthesis.
	if _, err := db.Exec(`INSERT INTO Convoys (name, status, draft_pr_url)
		VALUES ('half-baked', 'DraftPROpen', 'https://example/pr/1')`); err != nil {
		t.Fatalf("seed half-baked: %v", err)
	}
	// Fully baked: DraftPROpen + URL + synthesis row.
	res, err := db.Exec(`INSERT INTO Convoys (name, status, draft_pr_url)
		VALUES ('fully-baked', 'DraftPROpen', 'https://example/pr/2')`)
	if err != nil {
		t.Fatalf("seed fully-baked: %v", err)
	}
	bakedID, _ := res.LastInsertId()
	if _, err := db.Exec(`INSERT INTO PRHandoffSyntheses (convoy_id, pr_url, posted_at)
		VALUES (?, 'https://example/pr/2', datetime('now'))`, bakedID); err != nil {
		t.Fatalf("seed synthesis: %v", err)
	}

	half, err := findHalfBakedDraftPROpenConvoys(db)
	if err != nil {
		t.Fatalf("findHalfBakedDraftPROpenConvoys: %v", err)
	}
	if len(half) != 1 {
		t.Errorf("expected 1 half-baked convoy, got %d (%v)", len(half), half)
	}
}

// TestCheckBinarySHADrift_NoHistory: an empty DaemonUpdateHistory
// returns nil (no drift to check).
func TestCheckBinarySHADrift_NoHistory(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	if err := checkBinarySHADrift(db); err != nil {
		t.Errorf("expected nil for empty history, got %v", err)
	}
}

// TestCheckBinarySHADrift_Mismatch: when the most recent successful
// row's new_binary_sha doesn't match the live binary, the function
// logs a warning but does not return an error (drift is informational).
func TestCheckBinarySHADrift_Mismatch(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	// Record an obviously-wrong "successful" row.
	if err := store.RecordDaemonUpdate(db,
		"oldhash", "completelywrongnewhash",
		"oldgit", "newgit", "op", "success", "test-only mismatch row",
	); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := checkBinarySHADrift(db); err != nil {
		t.Errorf("expected nil (drift is logged, not fatal), got %v", err)
	}
}

// TestRunBootSweep_Idempotent: running twice is safe — the second run
// finds no stale state.
func TestRunBootSweep_Idempotent(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	// Seed one Locked row.
	if _, err := db.Exec(`INSERT INTO BountyBoard
		(target_repo, branch_name, type, owner, status, locked_at)
		VALUES ('repo', 'b', 'Test', 'AgentX', 'Locked', datetime('now'))`); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := runBootSweep(context.Background(), db); err != nil {
		t.Fatalf("first sweep: %v", err)
	}
	if err := runBootSweep(context.Background(), db); err != nil {
		t.Fatalf("second sweep: %v", err)
	}
	// Still Pending.
	var status string
	if err := db.QueryRow(`SELECT status FROM BountyBoard LIMIT 1`).Scan(&status); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if status != "Pending" {
		t.Errorf("after idempotent sweep, status = %q, want Pending", status)
	}
}
