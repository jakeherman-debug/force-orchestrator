package holdout

import (
	"context"
	"database/sql"
	"math"
	"testing"
	"time"

	"force-orchestrator/internal/store"
)

func openDB(t *testing.T) *sql.DB {
	t.Helper()
	db := store.InitHolocronDSN(":memory:")
	if db == nil {
		t.Fatalf("InitHolocronDSN returned nil")
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// TestMintBaseline2026_HappyPath — first call inserts a row with
// the documented defaults; the returned id matches the row's id.
func TestMintBaseline2026_HappyPath(t *testing.T) {
	db := openDB(t)
	ctx := context.Background()
	id, err := MintBaseline2026(ctx, db)
	if err != nil {
		t.Fatalf("MintBaseline2026: %v", err)
	}
	if id <= 0 {
		t.Fatalf("returned id should be positive, got %d", id)
	}
	var name string
	var ramp, fadeDays int
	var plateau float64
	var fadeStart string
	err = db.QueryRowContext(ctx, `
		SELECT name, ramp_up_days, plateau_fraction, fade_start_at, fade_days
		FROM GlobalHoldouts WHERE id = ?
	`, id).Scan(&name, &ramp, &plateau, &fadeStart, &fadeDays)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if name != BaselineHoldoutName {
		t.Errorf("name: got %q, want %q", name, BaselineHoldoutName)
	}
	if ramp != 7 {
		t.Errorf("ramp_up_days: got %d, want 7", ramp)
	}
	if math.Abs(plateau-0.02) > 1e-12 {
		t.Errorf("plateau_fraction: got %v, want 0.02", plateau)
	}
	if fadeStart != "" {
		t.Errorf("fade_start_at should be empty at mint, got %q", fadeStart)
	}
	if fadeDays != 90 {
		t.Errorf("fade_days: got %d, want 90", fadeDays)
	}
}

// TestMintBaseline2026_Idempotent — second call returns the same id
// and does not create a duplicate row.
func TestMintBaseline2026_Idempotent(t *testing.T) {
	db := openDB(t)
	ctx := context.Background()
	id1, err := MintBaseline2026(ctx, db)
	if err != nil {
		t.Fatalf("first mint: %v", err)
	}
	id2, err := MintBaseline2026(ctx, db)
	if err != nil {
		t.Fatalf("second mint: %v", err)
	}
	if id1 != id2 {
		t.Errorf("idempotent mint: got id1=%d id2=%d, want equal", id1, id2)
	}
	var count int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM GlobalHoldouts WHERE name = ?`,
		BaselineHoldoutName,
	).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 1 {
		t.Errorf("row count after two mints: got %d, want 1", count)
	}
}

// TestIsInHoldout_DeterministicAssignment — the same (kind, id) input
// returns the same answer when re-queried. This is the determinism
// guarantee the entire holdout discipline rests on.
func TestIsInHoldout_DeterministicAssignment(t *testing.T) {
	db := openDB(t)
	ctx := context.Background()
	id, err := MintBaseline2026(ctx, db)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	// Far past the ramp window so plateau is in effect.
	farFuture := time.Now().UTC().Add(60 * 24 * time.Hour)
	for unitID := 1; unitID <= 50; unitID++ {
		first, err := IsInHoldoutAt(ctx, db, id, "feature", unitID, farFuture)
		if err != nil {
			t.Fatalf("first check unit %d: %v", unitID, err)
		}
		for i := 0; i < 5; i++ {
			again, err := IsInHoldoutAt(ctx, db, id, "feature", unitID, farFuture)
			if err != nil {
				t.Fatalf("repeat check unit %d: %v", unitID, err)
			}
			if again != first {
				t.Errorf("unit %d: assignment flipped between calls (first=%v, again=%v)", unitID, first, again)
			}
		}
	}
}

// TestIsInHoldout_TwoPercentPlateau — Monte Carlo over 10000 synthetic
// feature ids: the assignment rate at plateau should be 0.02 ± 0.005
// (a generous tolerance for sampling noise — exact rate from a finite
// hash sample is not guaranteed to be exactly 0.02).
func TestIsInHoldout_TwoPercentPlateau(t *testing.T) {
	db := openDB(t)
	ctx := context.Background()
	id, err := MintBaseline2026(ctx, db)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	farFuture := time.Now().UTC().Add(30 * 24 * time.Hour)
	const N = 10000
	hits := 0
	for unitID := 1; unitID <= N; unitID++ {
		in, err := IsInHoldoutAt(ctx, db, id, "feature", unitID, farFuture)
		if err != nil {
			t.Fatalf("unit %d: %v", unitID, err)
		}
		if in {
			hits++
		}
	}
	rate := float64(hits) / float64(N)
	if math.Abs(rate-0.02) > 0.005 {
		t.Errorf("plateau rate: got %v (hits=%d / N=%d), want 0.02 ± 0.005", rate, hits, N)
	}
}

// TestIsInHoldout_RampUpInterpolation — pin reference_date 3 days in
// the past against a 7-day ramp; expected rate is (3/7) × 0.02 ≈
// 0.0086. Tolerance widened (±0.004) because the smaller plateau
// makes the sampling-noise-relative-to-target gap wider.
func TestIsInHoldout_RampUpInterpolation(t *testing.T) {
	db := openDB(t)
	ctx := context.Background()
	id, err := MintBaseline2026(ctx, db)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	now := time.Now().UTC()
	// Backdate reference_date to 3 days before now.
	ref := now.Add(-3 * 24 * time.Hour).Format("2006-01-02 15:04:05")
	if _, err := db.ExecContext(ctx, `UPDATE GlobalHoldouts SET reference_date = ? WHERE id = ?`, ref, id); err != nil {
		t.Fatalf("backdate: %v", err)
	}

	const N = 10000
	hits := 0
	for unitID := 1; unitID <= N; unitID++ {
		in, err := IsInHoldoutAt(ctx, db, id, "feature", unitID, now)
		if err != nil {
			t.Fatalf("unit %d: %v", unitID, err)
		}
		if in {
			hits++
		}
	}
	rate := float64(hits) / float64(N)
	const expected = (3.0 / 7.0) * 0.02
	if math.Abs(rate-expected) > 0.004 {
		t.Errorf("ramp rate: got %v (hits=%d / N=%d), want %v ± 0.004", rate, hits, N, expected)
	}
}

// TestIsInHoldout_BeforeReferenceDate_AlwaysFalse — at t before the
// holdout's reference_date, current_fraction is 0 — no unit is in.
func TestIsInHoldout_BeforeReferenceDate_AlwaysFalse(t *testing.T) {
	db := openDB(t)
	ctx := context.Background()
	id, err := MintBaseline2026(ctx, db)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	past := time.Now().UTC().Add(-365 * 24 * time.Hour)
	for unitID := 1; unitID <= 100; unitID++ {
		in, err := IsInHoldoutAt(ctx, db, id, "feature", unitID, past)
		if err != nil {
			t.Fatalf("unit %d: %v", unitID, err)
		}
		if in {
			t.Errorf("unit %d: expected not-in-holdout before reference_date, got in", unitID)
		}
	}
}

// TestIsInHoldout_AfterFade_AlwaysFalse — set fade_start_at + fade
// already complete; current_fraction is 0 again.
func TestIsInHoldout_AfterFade_AlwaysFalse(t *testing.T) {
	db := openDB(t)
	ctx := context.Background()
	id, err := MintBaseline2026(ctx, db)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	now := time.Now().UTC()
	fadeStart := now.Add(-180 * 24 * time.Hour).Format("2006-01-02 15:04:05")
	if _, err := db.ExecContext(ctx, `
		UPDATE GlobalHoldouts SET fade_start_at = ?, fade_days = 90 WHERE id = ?
	`, fadeStart, id); err != nil {
		t.Fatalf("fade: %v", err)
	}
	// Backdate reference well before fade so plateau was reached.
	ref := now.Add(-300 * 24 * time.Hour).Format("2006-01-02 15:04:05")
	if _, err := db.ExecContext(ctx, `UPDATE GlobalHoldouts SET reference_date = ? WHERE id = ?`, ref, id); err != nil {
		t.Fatalf("backdate ref: %v", err)
	}
	for unitID := 1; unitID <= 100; unitID++ {
		in, err := IsInHoldoutAt(ctx, db, id, "feature", unitID, now)
		if err != nil {
			t.Fatalf("unit %d: %v", unitID, err)
		}
		if in {
			t.Errorf("unit %d: expected not-in-holdout after fade complete, got in", unitID)
		}
	}
}

// TestCurrentFraction_LifecyclePhases — pure-function test over
// CurrentFraction without touching the DB. Verifies each branch of
// the lifecycle math.
func TestCurrentFraction_LifecyclePhases(t *testing.T) {
	ref := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	h := Holdout{
		ReferenceDate:   ref,
		RampUpDays:      7,
		PlateauFraction: 0.02,
		FadeDays:        90,
	}
	cases := []struct {
		name string
		t    time.Time
		want float64
		tol  float64
	}{
		{"before reference", ref.Add(-24 * time.Hour), 0, 0},
		{"midway through ramp", ref.Add(3*24*time.Hour + 12*time.Hour), 0.02 * 3.5 / 7.0, 1e-9},
		{"plateau", ref.Add(60 * 24 * time.Hour), 0.02, 1e-9},
	}
	for _, c := range cases {
		got := h.CurrentFraction(c.t)
		if math.Abs(got-c.want) > c.tol {
			t.Errorf("%s: got %v, want %v ± %v", c.name, got, c.want, c.tol)
		}
	}

	// Fade phase.
	hf := h
	hf.FadeStartAt = ref.Add(365 * 24 * time.Hour)
	gotMidFade := hf.CurrentFraction(hf.FadeStartAt.Add(45 * 24 * time.Hour))
	wantMidFade := 0.02 * (1 - 45.0/90.0)
	if math.Abs(gotMidFade-wantMidFade) > 1e-9 {
		t.Errorf("mid-fade: got %v, want %v", gotMidFade, wantMidFade)
	}
	gotPostFade := hf.CurrentFraction(hf.FadeStartAt.Add(91 * 24 * time.Hour))
	if gotPostFade != 0 {
		t.Errorf("post-fade: got %v, want 0", gotPostFade)
	}
}

// TestMintBaseline2026_NilDB — programmer error returns an error.
func TestMintBaseline2026_NilDB(t *testing.T) {
	if _, err := MintBaseline2026(context.Background(), nil); err == nil {
		t.Errorf("expected error for nil db")
	}
}
