package agents

import (
	"testing"
	"time"

	"force-orchestrator/internal/store"
)

// ── IsEstopped / SetEstop ─────────────────────────────────────────────────────
// Note: Basic toggle is tested in holocron_test.go (TestEstop).
// These tests cover additional edge cases.

func TestSetEstop_Toggle(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	// Starts off
	if IsEstopped(db) {
		t.Error("should not be estopped initially")
	}

	// Activate
	SetEstop(db, true)
	if !IsEstopped(db) {
		t.Error("should be estopped after SetEstop(true)")
	}

	// Deactivate
	SetEstop(db, false)
	if IsEstopped(db) {
		t.Error("should not be estopped after SetEstop(false)")
	}
}

func TestSetEstop_IdempotentTrue(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	SetEstop(db, true)
	SetEstop(db, true)
	if !IsEstopped(db) {
		t.Error("should remain estopped after two SetEstop(true) calls")
	}
	SetEstop(db, false)
}

func TestSetEstop_IdempotentFalse(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	SetEstop(db, false)
	SetEstop(db, false)
	if IsEstopped(db) {
		t.Error("should remain not-estopped after two SetEstop(false) calls")
	}
}

// ── IsOverCapacity ────────────────────────────────────────────────────────────
// Basic capacity test covered in holocron_test.go (TestIsOverCapacity).
// This test covers the CodeEdit-only counting rule.

func TestIsOverCapacity_NonCodeEditTasksIgnored(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	// Set a low cap so capacity is easy to hit
	store.SetConfig(db, "max_concurrent", "2")

	// Lock 2 non-CodeEdit tasks — should not count toward capacity
	db.Exec(`INSERT INTO BountyBoard (type, status, payload) VALUES ('Feature', 'Locked', 'f1')`)
	db.Exec(`INSERT INTO BountyBoard (type, status, payload) VALUES ('Commander', 'Locked', 'c1')`)

	if IsOverCapacity(db) {
		t.Error("non-CodeEdit tasks should not count toward capacity limit")
	}

	// Now add 2 CodeEdit tasks — should hit the cap
	db.Exec(`INSERT INTO BountyBoard (type, status, payload) VALUES ('CodeEdit', 'Locked', 'e1')`)
	db.Exec(`INSERT INTO BountyBoard (type, status, payload) VALUES ('CodeEdit', 'Locked', 'e2')`)

	if !IsOverCapacity(db) {
		t.Error("should be over capacity with 2 locked CodeEdit tasks when max=2")
	}
}

// ── InfraBackoff ──────────────────────────────────────────────────────────────
// Full suite covered in holocron_test.go (TestInfraBackoff).
// This test verifies the linear growth formula directly.

func TestInfraBackoff_LinearGrowth(t *testing.T) {
	cases := []struct {
		count int
		want  time.Duration
	}{
		{1, 10 * time.Second},
		{2, 20 * time.Second},
		{3, 30 * time.Second},
		{6, 60 * time.Second}, // cap
		{10, 60 * time.Second}, // still capped
	}
	for _, tc := range cases {
		got := InfraBackoff(tc.count)
		if got != tc.want {
			t.Errorf("InfraBackoff(%d) = %v, want %v", tc.count, got, tc.want)
		}
	}
}

func TestInfraBackoff_ZeroCount(t *testing.T) {
	got := InfraBackoff(0)
	if got != 0 {
		t.Errorf("InfraBackoff(0) should be 0, got %v", got)
	}
}

// ── RateLimitBackoff ──────────────────────────────────────────────────────────

func TestRateLimitBackoff_ExponentialGrowth(t *testing.T) {
	cases := []struct {
		count int
		want  time.Duration
	}{
		{0, 60 * time.Second},
		{1, 120 * time.Second},
		{2, 240 * time.Second},
		{3, 480 * time.Second},
		{10, 10 * time.Minute}, // cap
	}
	for _, tc := range cases {
		got := RateLimitBackoff(tc.count)
		if got != tc.want {
			t.Errorf("RateLimitBackoff(%d) = %v, want %v", tc.count, got, tc.want)
		}
	}
}

// ── IsThrottledByBatchSize ────────────────────────────────────────────────────
// Disabled and under-limit cases covered in holocron_test.go.
// This test covers the case where recent claims exceed the limit.

func TestIsThrottledByBatchSize_AtLimit(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	store.SetConfig(db, "batch_size", "3")

	// Insert 3 tasks that were locked within the last 60 seconds with eligible statuses
	for i := 0; i < 3; i++ {
		db.Exec(`INSERT INTO BountyBoard (type, status, payload, locked_at)
			VALUES ('CodeEdit', 'Locked', 'recent task', datetime('now', '-5 seconds'))`)
	}

	if !IsThrottledByBatchSize(db) {
		t.Error("should be throttled when recent claims equals batch_size limit")
	}
}

func TestIsThrottledByBatchSize_OldTasksDontCount(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	store.SetConfig(db, "batch_size", "2")

	// Insert tasks locked more than 60 seconds ago — should not count
	for i := 0; i < 5; i++ {
		db.Exec(`INSERT INTO BountyBoard (type, status, payload, locked_at)
			VALUES ('CodeEdit', 'Completed', 'old task', datetime('now', '-120 seconds'))`)
	}

	if IsThrottledByBatchSize(db) {
		t.Error("old tasks (>60s ago) should not trigger batch throttle")
	}
}
