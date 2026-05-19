package agents

import (
	"context"
	"io"
	"log"
	"testing"

	"force-orchestrator/internal/holdout"
	"force-orchestrator/internal/store"
)

// ── dogHoldoutSnapshot ────────────────────────────────────────────────────────

// TestDogHoldoutSnapshot_NoHoldouts verifies that the dog is a no-op when
// there are no GlobalHoldouts rows — no error, no panic.
func TestDogHoldoutSnapshot_NoHoldouts(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	logger := log.New(io.Discard, "", 0)
	if err := dogHoldoutSnapshot(context.Background(), db, logger); err != nil {
		t.Fatalf("expected no error with no holdout rows, got: %v", err)
	}
}

// TestDogHoldoutSnapshot_PopulatesHash verifies the happy path: after minting
// a baseline holdout and running the dog, fleet_state_hash is non-empty.
func TestDogHoldoutSnapshot_PopulatesHash(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	ctx := context.Background()
	holdoutID, err := holdout.MintBaseline2026(ctx, db)
	if err != nil {
		t.Fatalf("MintBaseline2026: %v", err)
	}

	// Seed some fleet state so the hash is over real data.
	db.Exec(`INSERT INTO BountyBoard (type, status, payload) VALUES ('CodeEdit', 'Pending', 'task-1')`)
	db.Exec(`INSERT INTO BountyBoard (type, status, payload, owner) VALUES ('CodeEdit', 'Locked', 'task-2', 'astromech-1')`)

	logger := log.New(io.Discard, "", 0)
	if err := dogHoldoutSnapshot(ctx, db, logger); err != nil {
		t.Fatalf("dogHoldoutSnapshot: %v", err)
	}

	var hash string
	if err := db.QueryRow(`SELECT IFNULL(fleet_state_hash, '') FROM GlobalHoldouts WHERE id = ?`, holdoutID).Scan(&hash); err != nil {
		t.Fatalf("query fleet_state_hash: %v", err)
	}
	if hash == "" {
		t.Error("expected fleet_state_hash to be non-empty after running holdout-snapshot dog")
	}
	// SHA-256 hex is 64 chars.
	if len(hash) != 64 {
		t.Errorf("expected 64-char hex SHA-256, got %d chars: %q", len(hash), hash)
	}
}

// TestDogHoldoutSnapshot_Determinism verifies that two runs with identical fleet
// state produce identical hashes — and that a state change produces a different
// hash.
func TestDogHoldoutSnapshot_Determinism(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	ctx := context.Background()
	holdoutID, err := holdout.MintBaseline2026(ctx, db)
	if err != nil {
		t.Fatalf("MintBaseline2026: %v", err)
	}

	// Seed stable fleet state.
	db.Exec(`INSERT INTO BountyBoard (type, status, payload) VALUES ('CodeEdit', 'Pending', 'task-1')`)
	db.Exec(`INSERT INTO TreatmentSpecs (spec_hash, model_identifier) VALUES ('abc123', 'claude-sonnet-4-5')`)

	logger := log.New(io.Discard, "", 0)

	// First run.
	if err := dogHoldoutSnapshot(ctx, db, logger); err != nil {
		t.Fatalf("first dogHoldoutSnapshot: %v", err)
	}
	var hash1 string
	db.QueryRow(`SELECT IFNULL(fleet_state_hash, '') FROM GlobalHoldouts WHERE id = ?`, holdoutID).Scan(&hash1)
	if hash1 == "" {
		t.Fatal("hash1 is empty after first run")
	}

	// Second run with identical state — hash must be identical.
	if err := dogHoldoutSnapshot(ctx, db, logger); err != nil {
		t.Fatalf("second dogHoldoutSnapshot: %v", err)
	}
	var hash2 string
	db.QueryRow(`SELECT IFNULL(fleet_state_hash, '') FROM GlobalHoldouts WHERE id = ?`, holdoutID).Scan(&hash2)
	if hash1 != hash2 {
		t.Errorf("determinism violation: same inputs produced different hashes:\n  run1=%s\n  run2=%s", hash1, hash2)
	}

	// Change fleet state (add a new task) — hash must differ.
	db.Exec(`INSERT INTO BountyBoard (type, status, payload) VALUES ('CodeEdit', 'Completed', 'task-2')`)
	if err := dogHoldoutSnapshot(ctx, db, logger); err != nil {
		t.Fatalf("third dogHoldoutSnapshot: %v", err)
	}
	var hash3 string
	db.QueryRow(`SELECT IFNULL(fleet_state_hash, '') FROM GlobalHoldouts WHERE id = ?`, holdoutID).Scan(&hash3)
	if hash2 == hash3 {
		t.Errorf("expected hash to change after state mutation, but both runs produced %s", hash2)
	}
}

// TestDogHoldoutSnapshot_MultipleHoldouts verifies that the dog writes the same
// hash to all active GlobalHoldouts rows.
func TestDogHoldoutSnapshot_MultipleHoldouts(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	ctx := context.Background()

	// Insert two distinct holdout rows.
	res1, _ := db.Exec(`INSERT INTO GlobalHoldouts (name, fleet_state_hash) VALUES ('holdout-a', '')`)
	id1, _ := res1.LastInsertId()
	res2, _ := db.Exec(`INSERT INTO GlobalHoldouts (name, fleet_state_hash) VALUES ('holdout-b', '')`)
	id2, _ := res2.LastInsertId()

	logger := log.New(io.Discard, "", 0)
	if err := dogHoldoutSnapshot(ctx, db, logger); err != nil {
		t.Fatalf("dogHoldoutSnapshot: %v", err)
	}

	var hash1, hash2 string
	db.QueryRow(`SELECT IFNULL(fleet_state_hash, '') FROM GlobalHoldouts WHERE id = ?`, id1).Scan(&hash1)
	db.QueryRow(`SELECT IFNULL(fleet_state_hash, '') FROM GlobalHoldouts WHERE id = ?`, id2).Scan(&hash2)

	if hash1 == "" {
		t.Error("holdout-a fleet_state_hash is empty after dog run")
	}
	if hash2 == "" {
		t.Error("holdout-b fleet_state_hash is empty after dog run")
	}
	if hash1 != hash2 {
		t.Errorf("expected both holdouts to receive the same fleet hash, got:\n  holdout-a=%s\n  holdout-b=%s", hash1, hash2)
	}
}

// TestDogHoldoutSnapshot_RetiredHoldoutsSkipped verifies that rows with a
// non-empty retired_at timestamp are not updated.
func TestDogHoldoutSnapshot_RetiredHoldoutsSkipped(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	ctx := context.Background()

	// Insert a retired holdout — retired_at is set to a past timestamp.
	res, _ := db.Exec(`INSERT INTO GlobalHoldouts (name, fleet_state_hash, retired_at)
		VALUES ('retired-holdout', '', '2020-01-01 00:00:00')`)
	retiredID, _ := res.LastInsertId()

	// Insert an active holdout.
	res2, _ := db.Exec(`INSERT INTO GlobalHoldouts (name, fleet_state_hash, retired_at)
		VALUES ('active-holdout', '', '')`)
	activeID, _ := res2.LastInsertId()

	logger := log.New(io.Discard, "", 0)
	if err := dogHoldoutSnapshot(ctx, db, logger); err != nil {
		t.Fatalf("dogHoldoutSnapshot: %v", err)
	}

	var retiredHash, activeHash string
	db.QueryRow(`SELECT IFNULL(fleet_state_hash, '') FROM GlobalHoldouts WHERE id = ?`, retiredID).Scan(&retiredHash)
	db.QueryRow(`SELECT IFNULL(fleet_state_hash, '') FROM GlobalHoldouts WHERE id = ?`, activeID).Scan(&activeHash)

	if retiredHash != "" {
		t.Errorf("expected retired holdout fleet_state_hash to remain empty, got %q", retiredHash)
	}
	if activeHash == "" {
		t.Error("expected active holdout fleet_state_hash to be populated")
	}
}

// ── computeFleetStateHash unit tests ─────────────────────────────────────────

// TestComputeFleetStateHash_EmptyDB verifies that an empty DB produces a
// consistent, non-empty hash (the empty-state hash is deterministic).
func TestComputeFleetStateHash_EmptyDB(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	hash1, err := computeFleetStateHash(db)
	if err != nil {
		t.Fatalf("computeFleetStateHash: %v", err)
	}
	if hash1 == "" {
		t.Fatal("expected non-empty hash from empty DB")
	}
	if len(hash1) != 64 {
		t.Errorf("expected 64-char hex SHA-256, got %d chars", len(hash1))
	}

	// Second call — same result.
	hash2, err := computeFleetStateHash(db)
	if err != nil {
		t.Fatalf("computeFleetStateHash (2nd call): %v", err)
	}
	if hash1 != hash2 {
		t.Errorf("empty-DB hash is not deterministic: %s != %s", hash1, hash2)
	}
}

// TestComputeFleetStateHash_ModelTiersIncluded verifies that adding a
// TreatmentSpecs row changes the hash, proving the model_identifier dimension
// participates in the hash.
func TestComputeFleetStateHash_ModelTiersIncluded(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	before, err := computeFleetStateHash(db)
	if err != nil {
		t.Fatalf("before: %v", err)
	}

	db.Exec(`INSERT INTO TreatmentSpecs (spec_hash, model_identifier) VALUES ('h1', 'claude-opus-4')`)

	after, err := computeFleetStateHash(db)
	if err != nil {
		t.Fatalf("after: %v", err)
	}
	if before == after {
		t.Error("expected hash to change after adding a TreatmentSpecs model_identifier")
	}
}

// TestComputeFleetStateHash_TaskDistributionIncluded verifies that changing
// task status distribution changes the hash.
func TestComputeFleetStateHash_TaskDistributionIncluded(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	before, err := computeFleetStateHash(db)
	if err != nil {
		t.Fatalf("before: %v", err)
	}

	db.Exec(`INSERT INTO BountyBoard (type, status, payload) VALUES ('CodeEdit', 'Pending', 'p1')`)

	after, err := computeFleetStateHash(db)
	if err != nil {
		t.Fatalf("after: %v", err)
	}
	if before == after {
		t.Error("expected hash to change after adding a BountyBoard task")
	}
}

// ── agentPrefix unit test ─────────────────────────────────────────────────────

func TestAgentPrefix(t *testing.T) {
	cases := []struct {
		owner  string
		expect string
	}{
		{"", "unknown"},
		{"astromech-1", "astromech"},
		{"captain:force-orchestrator", "captain"},
		{"council/task-42", "council"},
		{"medic_retry", "medic"},
		{"inquisitor", "inquisitor"},
	}
	for _, tc := range cases {
		got := agentPrefix(tc.owner)
		if got != tc.expect {
			t.Errorf("agentPrefix(%q) = %q, want %q", tc.owner, got, tc.expect)
		}
	}
}

// ── store.UpdateHoldoutFleetStateHash unit tests ──────────────────────────────

func TestUpdateHoldoutFleetStateHash_HappyPath(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	res, _ := db.Exec(`INSERT INTO GlobalHoldouts (name) VALUES ('test-holdout')`)
	id, _ := res.LastInsertId()

	if err := store.UpdateHoldoutFleetStateHash(db, int(id), "deadbeef"); err != nil {
		t.Fatalf("UpdateHoldoutFleetStateHash: %v", err)
	}

	var hash string
	db.QueryRow(`SELECT fleet_state_hash FROM GlobalHoldouts WHERE id = ?`, id).Scan(&hash)
	if hash != "deadbeef" {
		t.Errorf("expected hash=deadbeef, got %q", hash)
	}
}

func TestUpdateHoldoutFleetStateHash_NotFound(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	err := store.UpdateHoldoutFleetStateHash(db, 9999, "abc")
	if err == nil {
		t.Error("expected error for non-existent holdout ID, got nil")
	}
}

func TestUpdateHoldoutFleetStateHash_NilDB(t *testing.T) {
	err := store.UpdateHoldoutFleetStateHash(nil, 1, "abc")
	if err == nil {
		t.Error("expected error for nil db, got nil")
	}
}
