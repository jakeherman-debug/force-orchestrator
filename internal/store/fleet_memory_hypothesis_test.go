package store

import (
	"context"
	"testing"
)

// TestEmitHypothesisCandidates_HighSignalEmits ensures a memory above
// both retrieval and validation thresholds produces a candidate.
func TestEmitHypothesisCandidates_HighSignalEmits(t *testing.T) {
	db := mustOpenDedupTestDB(t)
	defer db.Close()

	StoreFleetMemory(db, "repoA", 1, "success", "High-signal memory.", "x.go", "x, signal")
	if _, err := db.Exec(`UPDATE FleetMemory SET retrieval_count = 10, validation_score = 0.5 WHERE id = 1`); err != nil {
		t.Fatalf("seed: %v", err)
	}

	emitted, err := EmitHypothesisCandidates(context.Background(), db)
	if err != nil {
		t.Fatalf("EmitHypothesisCandidates: %v", err)
	}
	if emitted != 1 {
		t.Errorf("expected 1 candidate, got %d", emitted)
	}

	var count int
	db.QueryRow(`SELECT COUNT(*) FROM PromotionProposals WHERE kind='candidate' AND authored_by='librarian' AND source_memory_id = 1`).Scan(&count)
	if count != 1 {
		t.Errorf("expected 1 candidate row for memory 1, got %d", count)
	}

	var stamped string
	db.QueryRow(`SELECT IFNULL(hypothesis_emitted_at,'') FROM FleetMemory WHERE id = 1`).Scan(&stamped)
	if stamped == "" {
		t.Errorf("expected hypothesis_emitted_at stamped, got empty")
	}
}

// TestEmitHypothesisCandidates_LowSignalSkipped ensures memories below
// thresholds do NOT emit.
func TestEmitHypothesisCandidates_LowSignalSkipped(t *testing.T) {
	db := mustOpenDedupTestDB(t)
	defer db.Close()

	StoreFleetMemory(db, "repoA", 1, "success", "Low-signal memory.", "x.go", "x")
	if _, err := db.Exec(`UPDATE FleetMemory SET retrieval_count = 1, validation_score = 0.05 WHERE id = 1`); err != nil {
		t.Fatalf("seed: %v", err)
	}
	emitted, err := EmitHypothesisCandidates(context.Background(), db)
	if err != nil {
		t.Fatalf("EmitHypothesisCandidates: %v", err)
	}
	if emitted != 0 {
		t.Errorf("expected 0 candidates (low signal), got %d", emitted)
	}
}

// TestEmitHypothesisCandidates_Idempotent runs the emit twice and
// asserts the second run is a no-op.
func TestEmitHypothesisCandidates_Idempotent(t *testing.T) {
	db := mustOpenDedupTestDB(t)
	defer db.Close()

	StoreFleetMemory(db, "repoA", 1, "success", "Memory.", "x.go", "x")
	if _, err := db.Exec(`UPDATE FleetMemory SET retrieval_count = 10, validation_score = 0.5 WHERE id = 1`); err != nil {
		t.Fatalf("seed: %v", err)
	}
	first, _ := EmitHypothesisCandidates(context.Background(), db)
	second, _ := EmitHypothesisCandidates(context.Background(), db)
	if first != 1 || second != 0 {
		t.Errorf("expected (1,0), got (%d,%d)", first, second)
	}
}
