package analytics

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"force-orchestrator/internal/store"
)

// TestComputeDisagreementRates_KnownFixture seeds TaskHistory + BountyBoard
// with a hand-crafted scenario and asserts each pair's rate matches a
// hand-computed value.
//
// Fixture:
//   - Captain → Council reject pair: 4 captain-approved tasks; 1 of them
//     subsequently rejected by council. Expected rate = 0.25.
//   - Council → CI fail: 3 council-approved tasks; 2 of them later Failed.
//     Expected rate = 2/3.
//   - ConvoyReview → can't fix: 2 fix-tasks under a convoy-review parent;
//     1 of them Failed. Expected rate = 0.5.
//   - Senate → Chancellor: deferred, sample=0, rate=0.
//   - Operator revert: 2 completed tasks; 1 has a revert task within 30d.
//     Expected rate = 0.5.
func TestComputeDisagreementRates_KnownFixture(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	ctx := context.Background()

	seedDisagreementFixture(t, db)

	results, err := ComputeDisagreementRates(ctx, db, 24*time.Hour)
	if err != nil {
		t.Fatalf("ComputeDisagreementRates: %v", err)
	}

	cases := []struct {
		pair     string
		samples  int
		disagree int
		rate     float64
		deferred bool
	}{
		{PairCaptainCouncilReject, 4, 1, 0.25, false},
		{PairCouncilCIFail, 3, 2, 2.0 / 3.0, false},
		{PairConvoyReviewCantFix, 2, 1, 0.5, false},
		{PairSenateChancellor, 0, 0, 0, true},
		{PairOperatorRevert30d, 2, 1, 0.5, false},
	}
	for _, tc := range cases {
		got, ok := results[tc.pair]
		if !ok {
			t.Errorf("missing pair %q in results", tc.pair)
			continue
		}
		if got.SampleCount != tc.samples {
			t.Errorf("%s: SampleCount = %d, want %d", tc.pair, got.SampleCount, tc.samples)
		}
		if got.Disagreements != tc.disagree {
			t.Errorf("%s: Disagreements = %d, want %d", tc.pair, got.Disagreements, tc.disagree)
		}
		if !floatNear(got.Rate, tc.rate, 0.001) {
			t.Errorf("%s: Rate = %v, want %v", tc.pair, got.Rate, tc.rate)
		}
		if got.Deferred != tc.deferred {
			t.Errorf("%s: Deferred = %v, want %v", tc.pair, got.Deferred, tc.deferred)
		}
	}
}

// TestDisagreementDog_Idempotent runs the persistence path twice with
// the same fixture and asserts (a) no duplicate rows for the same
// (pair, window_start, window_end), and (b) the second tick's rate
// equals the first.
func TestDisagreementDog_Idempotent(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	ctx := context.Background()

	seedDisagreementFixture(t, db)

	// First tick.
	results1, err := ComputeDisagreementRates(ctx, db, 24*time.Hour)
	if err != nil {
		t.Fatalf("first ComputeDisagreementRates: %v", err)
	}
	if err := PersistDisagreementRates(ctx, db, results1); err != nil {
		t.Fatalf("first PersistDisagreementRates: %v", err)
	}

	// Second tick — same fixture, same data.
	results2, err := ComputeDisagreementRates(ctx, db, 24*time.Hour)
	if err != nil {
		t.Fatalf("second ComputeDisagreementRates: %v", err)
	}
	if err := PersistDisagreementRates(ctx, db, results2); err != nil {
		t.Fatalf("second PersistDisagreementRates: %v", err)
	}

	// Idempotence: per-pair row count is exactly 1 (UPSERT, not INSERT).
	for pair := range results1 {
		var n int
		if err := db.QueryRow(`SELECT COUNT(*) FROM DisagreementPairs WHERE pair_name = ?`, pair).Scan(&n); err != nil {
			t.Fatalf("count rows for %s: %v", pair, err)
		}
		if n != 1 {
			t.Errorf("pair %s: expected 1 row after two ticks, got %d", pair, n)
		}
	}

	// Rate stability across ticks.
	for pair, r1 := range results1 {
		r2 := results2[pair]
		if !floatNear(r1.Rate, r2.Rate, 0.0001) {
			t.Errorf("pair %s: rate diverged across ticks: %v vs %v", pair, r1.Rate, r2.Rate)
		}
	}
}

// TestComputeDisagreementRates_EmptyDB returns zero samples for every
// pair on an empty database (no panics, no errors, all rates 0).
func TestComputeDisagreementRates_EmptyDB(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	ctx := context.Background()

	results, err := ComputeDisagreementRates(ctx, db, 24*time.Hour)
	if err != nil {
		t.Fatalf("ComputeDisagreementRates: %v", err)
	}
	for pair, r := range results {
		if r.SampleCount != 0 {
			t.Errorf("%s: empty-DB SampleCount = %d, want 0", pair, r.SampleCount)
		}
		if r.Rate != 0 {
			t.Errorf("%s: empty-DB Rate = %v, want 0", pair, r.Rate)
		}
	}
}

// TestComputeDisagreementRates_RejectsBadInput
func TestComputeDisagreementRates_RejectsBadInput(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	if _, err := ComputeDisagreementRates(context.Background(), nil, 24*time.Hour); err == nil {
		t.Errorf("expected error on nil db")
	}
	if _, err := ComputeDisagreementRates(context.Background(), db, 0); err == nil {
		t.Errorf("expected error on zero window")
	}
}

// seedDisagreementFixture inserts the canonical fixture used by
// TestComputeDisagreementRates_KnownFixture and
// TestDisagreementDog_Idempotent.
func seedDisagreementFixture(t *testing.T, db *sql.DB) {
	t.Helper()
	now := time.Now().UTC()

	// Helper: insert a TaskHistory row at a given offset before now.
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
	addRevertBounty := func(id, targetID int, offset time.Duration) {
		ts := now.Add(-offset).Format("2006-01-02 15:04:05")
		_, err := db.Exec(`
			INSERT INTO BountyBoard (id, parent_id, type, status, revert_target_task_id, created_at)
			VALUES (?, 0, 'Revert', 'Pending', ?, ?)
		`, id, targetID, ts)
		if err != nil {
			t.Fatalf("insert revert: %v", err)
		}
	}

	// ── Captain → Council reject (4 captain-approves, 1 council-rejects) ───────
	// Captain approved tasks 100..103, all 1h ago.
	for _, tid := range []int{100, 101, 102, 103} {
		addHistory(tid, "Captain-1", "Completed", 1*time.Hour)
	}
	// Of those, only task 100 was rejected by council 30m later.
	// Tasks 101..103 received no Council row — they're samples for
	// captain-council-reject but not Council-CI samples (keeping the
	// council-ci-fail denominator independent).
	addHistory(100, "Council-1", "Rejected", 30*time.Minute)

	// ── Council → CI fail (3 council-approves, 2 CI-fails) ─────────────────────
	// Use AwaitingSubPRCI on 200..202 (a real Council outcome) so these
	// rows do NOT also satisfy the captain-council-reject denominator.
	for _, tid := range []int{200, 201, 202} {
		addHistory(tid, "Council-1", "AwaitingSubPRCI", 1*time.Hour)
	}
	// 200 and 201 later Failed; 202 didn't.
	addHistory(200, "astromech-1", "Failed", 30*time.Minute)
	addHistory(201, "astromech-1", "Failed", 30*time.Minute)

	// ── ConvoyReview → can't fix (2 fix tasks, 1 failed) ───────────────────────
	// Parent task 300 was a ConvoyReview decision. Use status='Reviewing' so
	// it does NOT contaminate the operator-revert-30d sample set (which
	// counts BountyBoard.status='Completed' rows).
	addBounty(300, 0, "ConvoyReview", "Reviewing", 2*time.Hour)
	addHistory(300, "ConvoyReview-1", "Completed", 2*time.Hour)
	// Two CodeEdit fix tasks parented to 300, both inside the window.
	addBounty(301, 300, "CodeEdit", "Pending", 1*time.Hour)
	addBounty(302, 300, "CodeEdit", "Pending", 1*time.Hour)
	// Only 301 ended up Failed.
	addHistory(301, "astromech-1", "Failed", 30*time.Minute)
	addHistory(302, "astromech-1", "Completed", 30*time.Minute)

	// ── Operator revert 30d (2 completed tasks, 1 reverted) ────────────────────
	// Two tasks completed inside the window.
	addBounty(400, 0, "CodeEdit", "Completed", 1*time.Hour)
	addBounty(401, 0, "CodeEdit", "Completed", 1*time.Hour)
	// 400 was reverted 5 days later (still inside 30d).
	addRevertBounty(500, 400, 30*time.Minute)
	// 401 was NOT reverted.
}

func floatNear(a, b, tol float64) bool {
	d := a - b
	if d < 0 {
		d = -d
	}
	return d <= tol
}
