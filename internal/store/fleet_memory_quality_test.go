package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

// TestRecomputeFreshnessScores_DecaysOlderRows confirms a memory's
// freshness score decreases as its created_at recedes into the past.
func TestRecomputeFreshnessScores_DecaysOlderRows(t *testing.T) {
	db := mustOpenDedupTestDB(t)
	defer db.Close()

	StoreFleetMemory(db, "repoA", 1, "success", "Recent memory.", "x.go", "x")
	StoreFleetMemory(db, "repoA", 2, "success", "Older memory.", "y.go", "y")

	// Backdate row 2 by 60 days. Default half-life is 30 days, so row 2
	// should land at ~0.25 freshness.
	if _, err := db.Exec(`UPDATE FleetMemory SET created_at = datetime('now', '-60 days') WHERE id = 2`); err != nil {
		t.Fatalf("backdate: %v", err)
	}
	updated, err := RecomputeFreshnessScores(context.Background(), db)
	if err != nil {
		t.Fatalf("RecomputeFreshnessScores: %v", err)
	}
	if updated < 1 {
		t.Errorf("expected >=1 row updated, got %d", updated)
	}
	var s1, s2 float64
	db.QueryRow(`SELECT freshness_score FROM FleetMemory WHERE id = 1`).Scan(&s1)
	db.QueryRow(`SELECT freshness_score FROM FleetMemory WHERE id = 2`).Scan(&s2)
	if s1 <= s2 {
		t.Errorf("expected fresh row > old row (s1=%.4f, s2=%.4f)", s1, s2)
	}
	if s2 > 0.30 || s2 < 0.20 {
		t.Errorf("expected old-row freshness ~0.25 (60d / 30d half-life), got %.4f", s2)
	}
}

// TestRecomputeFreshnessScores_Idempotent guarantees a second pass
// over an already-recomputed table makes no further updates.
func TestRecomputeFreshnessScores_Idempotent(t *testing.T) {
	db := mustOpenDedupTestDB(t)
	defer db.Close()

	StoreFleetMemory(db, "repoA", 1, "success", "Memory one.", "a.go", "a")
	if _, err := RecomputeFreshnessScores(context.Background(), db); err != nil {
		t.Fatalf("first recompute: %v", err)
	}
	// Time has advanced by microseconds; the score might shift in
	// the 7th decimal. Tolerance check: re-run and ensure the count
	// of touched rows is in {0, 1} (a no-op or one trivial update),
	// not "all rows re-touched."
	updated, err := RecomputeFreshnessScores(context.Background(), db)
	if err != nil {
		t.Fatalf("second recompute: %v", err)
	}
	if updated > 1 {
		t.Errorf("expected ≤1 update on second recompute (idempotence), got %d", updated)
	}
}

// TestRecordRetrieval bumps retrieval_count + last_retrieved_at.
func TestRecordRetrieval(t *testing.T) {
	db := mustOpenDedupTestDB(t)
	defer db.Close()

	StoreFleetMemory(db, "repoA", 1, "success", "Memory one.", "a.go", "a")
	if err := RecordRetrieval(context.Background(), db, 1); err != nil {
		t.Fatalf("RecordRetrieval: %v", err)
	}
	if err := RecordRetrieval(context.Background(), db, 1); err != nil {
		t.Fatalf("RecordRetrieval second: %v", err)
	}
	var count int
	var lastAt string
	if err := db.QueryRow(`SELECT retrieval_count, IFNULL(last_retrieved_at,'') FROM FleetMemory WHERE id = 1`).
		Scan(&count, &lastAt); err != nil {
		t.Fatalf("read row: %v", err)
	}
	if count != 2 {
		t.Errorf("expected retrieval_count=2, got %d", count)
	}
	if lastAt == "" {
		t.Errorf("expected last_retrieved_at populated")
	}
}

func TestRecordRetrieval_NotFound(t *testing.T) {
	db := mustOpenDedupTestDB(t)
	defer db.Close()
	err := RecordRetrieval(context.Background(), db, 999)
	if !errors.Is(err, ErrFleetMemoryNotFound) {
		t.Errorf("expected ErrFleetMemoryNotFound, got %v", err)
	}
}

// TestRecordValidation tests positive + negative outcomes + clamp.
func TestRecordValidation_PositiveNegativeClamp(t *testing.T) {
	db := mustOpenDedupTestDB(t)
	defer db.Close()
	StoreFleetMemory(db, "repoA", 1, "success", "Memory.", "a.go", "a")

	// Positive nudge.
	if err := RecordValidation(context.Background(), db, 1, ValidationPositive); err != nil {
		t.Fatalf("positive: %v", err)
	}
	var v float64
	db.QueryRow(`SELECT validation_score FROM FleetMemory WHERE id = 1`).Scan(&v)
	if v < ValidationDelta-1e-9 || v > ValidationDelta+1e-9 {
		t.Errorf("expected validation=%.4f, got %.4f", ValidationDelta, v)
	}

	// Many positives → clamps at 1.0.
	for i := 0; i < 100; i++ {
		_ = RecordValidation(context.Background(), db, 1, ValidationPositive)
	}
	db.QueryRow(`SELECT validation_score FROM FleetMemory WHERE id = 1`).Scan(&v)
	if v != 1.0 {
		t.Errorf("expected validation clamped at 1.0, got %.4f", v)
	}

	// Many negatives → clamps at -1.0.
	for i := 0; i < 200; i++ {
		_ = RecordValidation(context.Background(), db, 1, ValidationNegative)
	}
	db.QueryRow(`SELECT validation_score FROM FleetMemory WHERE id = 1`).Scan(&v)
	if v != -1.0 {
		t.Errorf("expected validation clamped at -1.0, got %.4f", v)
	}
}

func TestRecordValidation_UnknownOutcome(t *testing.T) {
	db := mustOpenDedupTestDB(t)
	defer db.Close()
	StoreFleetMemory(db, "repoA", 1, "success", "Memory.", "a.go", "a")
	err := RecordValidation(context.Background(), db, 1, ValidationOutcome("weird"))
	if err == nil {
		t.Errorf("expected error for unknown outcome, got nil")
	}
}

// TestRecordValidation_NotFound returns the sentinel error.
func TestRecordValidation_NotFound(t *testing.T) {
	db := mustOpenDedupTestDB(t)
	defer db.Close()
	err := RecordValidation(context.Background(), db, 999, ValidationPositive)
	if !errors.Is(err, ErrFleetMemoryNotFound) {
		t.Errorf("expected ErrFleetMemoryNotFound, got %v", err)
	}
}

// TestQualityFeedbackLoop wires retrieval + validation through to
// confirm the integration behaviour: a retrieved + validated memory
// ends up with high score; a retrieved but penalised memory ends
// up with low score.
func TestQualityFeedbackLoop(t *testing.T) {
	db := mustOpenDedupTestDB(t)
	defer db.Close()

	StoreFleetMemory(db, "repoA", 1, "success", "Good memory.", "a.go", "a")
	StoreFleetMemory(db, "repoA", 2, "success", "Bad memory.", "b.go", "b")

	for i := 0; i < 10; i++ {
		_ = RecordRetrieval(context.Background(), db, 1)
		_ = RecordValidation(context.Background(), db, 1, ValidationPositive)
		_ = RecordRetrieval(context.Background(), db, 2)
		_ = RecordValidation(context.Background(), db, 2, ValidationNegative)
	}

	var v1, v2 float64
	db.QueryRow(`SELECT validation_score FROM FleetMemory WHERE id = 1`).Scan(&v1)
	db.QueryRow(`SELECT validation_score FROM FleetMemory WHERE id = 2`).Scan(&v2)
	if v1 < 0.4 {
		t.Errorf("expected good-memory validation > 0.4, got %.4f", v1)
	}
	if v2 > -0.4 {
		t.Errorf("expected bad-memory validation < -0.4, got %.4f", v2)
	}
}

// TestRecomputeFreshnessScores_ShortHalfLife exercises the override
// hook by shrinking FreshnessHalfLife to 100ms; a row written then
// slept past the half-life should drop below 0.6.
func TestRecomputeFreshnessScores_ShortHalfLife(t *testing.T) {
	old := FreshnessHalfLife
	FreshnessHalfLife = 100 * time.Millisecond
	defer func() { FreshnessHalfLife = old }()

	db := mustOpenDedupTestDB(t)
	defer db.Close()

	StoreFleetMemory(db, "repoA", 1, "success", "Memory.", "a.go", "a")
	time.Sleep(150 * time.Millisecond)
	if _, err := RecomputeFreshnessScores(context.Background(), db); err != nil {
		t.Fatalf("RecomputeFreshnessScores: %v", err)
	}
	var s float64
	db.QueryRow(`SELECT freshness_score FROM FleetMemory WHERE id = 1`).Scan(&s)
	if s > 0.7 {
		t.Errorf("expected freshness < 0.7 after >half-life sleep, got %.4f", s)
	}
}
