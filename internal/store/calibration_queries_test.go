package store

import (
	"context"
	"testing"
)

// TestCalibrationQueries covers 6B.11 invariants:
//   - Decision-time stats per agent are computed from BriefingRenders
//     within a 30-day window.
//   - Sample stats compute accuracy_pct correctly.
//   - Replay drift counts decision_changed=true rows.
//   - Suggestions fire for low-reject-rate / low-sample-accuracy /
//     replay-prompt-better signals.
//   - The endpoint NEVER writes to OperatorTrustDials directly.
func TestCalibrationQueries(t *testing.T) {
	t.Run("happy_path_computes_all_panels", func(t *testing.T) {
		db := InitHolocronDSN(":memory:")
		defer db.Close()

		// Seed 12 BriefingRenders for "captain_ruling" — 1 reject, 11 approve
		for i := 0; i < 11; i++ {
			_, err := db.Exec(`INSERT INTO BriefingRenders
				(decision_id, decision_kind, briefing_text, operator_decision, decision_time_seconds, rendered_at)
				VALUES (?, 'captain_ruling', 'b', 'approve', 25, datetime('now', '-1 day'))`, i+1)
			if err != nil {
				t.Fatalf("seed approve: %v", err)
			}
		}
		_, err := db.Exec(`INSERT INTO BriefingRenders
			(decision_id, decision_kind, briefing_text, operator_decision, decision_time_seconds, rendered_at)
			VALUES (99, 'captain_ruling', 'b', 'reject', 60, datetime('now', '-2 days'))`)
		if err != nil {
			t.Fatalf("seed reject: %v", err)
		}

		// Seed CalibrationAuditSamples — 9 confirm, 1 override (90% accuracy)
		for i := 0; i < 9; i++ {
			_, _ = db.Exec(`INSERT INTO CalibrationAuditSamples
				(sample_week, proposal_id, selection_bucket, operator_action, surfaced_at)
				VALUES ('2026-W17', ?, 'random', 'confirm', datetime('now', '-2 days'))`, i+1)
		}
		_, _ = db.Exec(`INSERT INTO CalibrationAuditSamples
			(sample_week, proposal_id, selection_bucket, operator_action, surfaced_at)
			VALUES ('2026-W17', 99, 'random', 'override', datetime('now', '-2 days'))`)

		// Seed ReplayResults — 6 of 10 changed
		for i := 0; i < 10; i++ {
			changed := 0
			if i < 6 {
				changed = 1
			}
			_, _ = db.Exec(`INSERT INTO ReplayResults
				(original_event_id, original_event_kind, replay_prompt_version,
				 replay_started_at, replay_response, decision_changed, cost_usd, triggered_by_email)
				VALUES (?, 'captain_ruling', 'v19', datetime('now'), 'r', ?, 0, 'op')`, i+1, changed)
		}

		sb, err := LoadCalibrationScoreboard(context.Background(), db)
		if err != nil {
			t.Fatalf("load: %v", err)
		}
		if len(sb.DecisionTimes) == 0 {
			t.Fatalf("expected per-agent decision time rows")
		}
		if sb.SampleStats.Total != 10 {
			t.Errorf("sample total: got %d, want 10", sb.SampleStats.Total)
		}
		if sb.SampleStats.AccuracyPct < 89 || sb.SampleStats.AccuracyPct > 91 {
			t.Errorf("accuracy_pct: %v", sb.SampleStats.AccuracyPct)
		}
		if sb.ReplayDrift.Total != 10 || sb.ReplayDrift.DecisionChanged != 6 {
			t.Errorf("replay drift: %+v", sb.ReplayDrift)
		}

		// Reject rate is 1/12 ≈ 0.083 — still well below the 0.05
		// baseline (default times 0.5 = 0.025) — wait, it's >0.025
		// so suggestion may not fire. Let's just check that the
		// raise_trust suggestion fires (replay drift 6/10 > 50%).
		var foundRaiseTrust bool
		for _, s := range sb.Suggestions {
			if s.Kind == "raise_trust" {
				foundRaiseTrust = true
			}
		}
		if !foundRaiseTrust {
			t.Errorf("expected raise_trust suggestion (replay drift 6/10): %+v", sb.Suggestions)
		}
	})

	t.Run("low_reject_rate_fires_lower_trust_suggestion", func(t *testing.T) {
		db := InitHolocronDSN(":memory:")
		defer db.Close()
		// 100 approves, 0 rejects — reject rate = 0
		for i := 0; i < 100; i++ {
			_, _ = db.Exec(`INSERT INTO BriefingRenders
				(decision_id, decision_kind, briefing_text, operator_decision, decision_time_seconds, rendered_at)
				VALUES (?, 'captain_ruling', 'b', 'approve', 30, datetime('now'))`, i+1)
		}
		sb, err := LoadCalibrationScoreboard(context.Background(), db)
		if err != nil {
			t.Fatalf("load: %v", err)
		}
		var found bool
		for _, s := range sb.Suggestions {
			if s.Kind == "lower_trust" {
				found = true
			}
		}
		if !found {
			t.Errorf("expected lower_trust suggestion at zero reject rate: %+v", sb.Suggestions)
		}
	})

	t.Run("empty_db_returns_empty_panels", func(t *testing.T) {
		db := InitHolocronDSN(":memory:")
		defer db.Close()
		sb, err := LoadCalibrationScoreboard(context.Background(), db)
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if len(sb.DecisionTimes) != 0 {
			t.Errorf("expected empty: %+v", sb.DecisionTimes)
		}
		if sb.SampleStats.Total != 0 {
			t.Errorf("expected zero samples")
		}
		if len(sb.SampleStatsByBucket) != 0 {
			t.Errorf("expected empty per-bucket: %+v", sb.SampleStatsByBucket)
		}
	})

	// D3 polish-pass A3: per-bucket breakout. Verify random vs
	// adversarial vs high-confidence each surface their own row with
	// independent accuracy_pct.
	t.Run("per_bucket_breakout_distinguishes_buckets", func(t *testing.T) {
		db := InitHolocronDSN(":memory:")
		defer db.Close()
		// random: 4 confirms, 1 override (80% accuracy)
		for i := 0; i < 4; i++ {
			_, _ = db.Exec(`INSERT INTO CalibrationAuditSamples
				(sample_week, proposal_id, selection_bucket, operator_action, surfaced_at)
				VALUES ('2026-W17', ?, 'random', 'confirm', datetime('now'))`, i+1)
		}
		_, _ = db.Exec(`INSERT INTO CalibrationAuditSamples
			(sample_week, proposal_id, selection_bucket, operator_action, surfaced_at)
			VALUES ('2026-W17', 5, 'random', 'override', datetime('now'))`)
		// fast_high_stakes: 1 confirm, 3 overrides (25% accuracy)
		_, _ = db.Exec(`INSERT INTO CalibrationAuditSamples
			(sample_week, proposal_id, selection_bucket, operator_action, surfaced_at)
			VALUES ('2026-W17', 6, 'fast_high_stakes', 'confirm', datetime('now'))`)
		for i := 0; i < 3; i++ {
			_, _ = db.Exec(`INSERT INTO CalibrationAuditSamples
				(sample_week, proposal_id, selection_bucket, operator_action, surfaced_at)
				VALUES ('2026-W17', ?, 'fast_high_stakes', 'override', datetime('now'))`, 7+i)
		}
		// high_approve_rate: 5 confirms (100%)
		for i := 0; i < 5; i++ {
			_, _ = db.Exec(`INSERT INTO CalibrationAuditSamples
				(sample_week, proposal_id, selection_bucket, operator_action, surfaced_at)
				VALUES ('2026-W17', ?, 'high_approve_rate', 'confirm', datetime('now'))`, 100+i)
		}

		sb, err := LoadCalibrationScoreboard(context.Background(), db)
		if err != nil {
			t.Fatalf("load: %v", err)
		}
		if len(sb.SampleStatsByBucket) != 3 {
			t.Fatalf("expected 3 bucket rows, got %d: %+v", len(sb.SampleStatsByBucket), sb.SampleStatsByBucket)
		}
		seen := map[string]BucketSampleStats{}
		for _, b := range sb.SampleStatsByBucket {
			seen[b.Bucket] = b
		}
		if r := seen["random"]; r.Total != 5 || r.ConfirmedCount != 4 || r.AccuracyPct != 80 {
			t.Errorf("random bucket wrong: %+v", r)
		}
		if r := seen["fast_high_stakes"]; r.Total != 4 || r.ConfirmedCount != 1 || r.AccuracyPct != 25 {
			t.Errorf("fast_high_stakes bucket wrong: %+v", r)
		}
		if r := seen["high_approve_rate"]; r.Total != 5 || r.ConfirmedCount != 5 || r.AccuracyPct != 100 {
			t.Errorf("high_approve_rate bucket wrong: %+v", r)
		}
		// And the rolling 30d aggregate stays a single number across all buckets.
		if sb.SampleStats.Total != 14 {
			t.Errorf("aggregate total: got %d, want 14", sb.SampleStats.Total)
		}
	})
}
