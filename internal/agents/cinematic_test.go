package agents

import (
	"context"
	"testing"
	"time"

	"force-orchestrator/internal/store"
)

func TestBuildCinematic_Quiet(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	ctx := context.Background()

	since := time.Now().Add(-30 * time.Minute)
	out, err := BuildCinematic(ctx, db, since)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if !out.Quiet {
		t.Errorf("empty window should be Quiet; got %+v", out)
	}
	if out.SleepDurationSec < 1700 {
		t.Errorf("duration too short: %d", out.SleepDurationSec)
	}
}

func TestBuildCinematic_WithNarratives(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	ctx := context.Background()

	now := time.Now().UTC()
	for i := 0; i < 3; i++ {
		_, err := db.Exec(`INSERT INTO NarrativeRenders
			(rendered_at, event_window_start, event_window_end, source_event_count, source_event_refs_json, prose, prompt_version, cost_usd)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			now.Add(time.Duration(i)*time.Second).Format("2006-01-02 15:04:05"),
			now.Format("2006-01-02 15:04:05"),
			now.Format("2006-01-02 15:04:05"),
			i*2, "[]", "Fleet active.", "v1.0.0", 0.001)
		if err != nil {
			t.Fatalf("seed: %v", err)
		}
	}

	since := now.Add(-30 * time.Second)
	out, err := BuildCinematic(ctx, db, since)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if out.Quiet {
		t.Errorf("non-empty window classified Quiet")
	}
	if len(out.NarrativeReplay) != 3 {
		t.Errorf("replay count=%d, want 3", len(out.NarrativeReplay))
	}
	if out.EventCount != 6 {
		t.Errorf("event count=%d, want 6", out.EventCount)
	}
}

func TestBuildCinematic_LongSleepFlag(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	ctx := context.Background()

	since := time.Now().Add(-10 * 24 * time.Hour)
	out, _ := BuildCinematic(ctx, db, since)
	if !out.IsLongSleep {
		t.Errorf("10-day window not flagged as long sleep")
	}
}

func TestDetectSleepStartedAt(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()
	ctx := context.Background()

	// No heartbeats → no detection.
	if _, ok := DetectSleepStartedAt(ctx, db); ok {
		t.Errorf("empty heartbeats classified as sleep")
	}

	// Two heartbeats with a 5-minute gap.
	now := time.Now().UTC()
	_, _ = db.Exec(`INSERT INTO DashboardHealthHeartbeats (ticked_at) VALUES (?)`,
		now.Add(-5*time.Minute).Format("2006-01-02 15:04:05"))
	_, _ = db.Exec(`INSERT INTO DashboardHealthHeartbeats (ticked_at) VALUES (?)`,
		now.Format("2006-01-02 15:04:05"))

	tStart, ok := DetectSleepStartedAt(ctx, db)
	if !ok {
		t.Fatalf("5-min gap not detected as sleep")
	}
	expected := now.Add(-5 * time.Minute)
	if tStart.Sub(expected).Abs() > 5*time.Second {
		t.Errorf("detected start %v, want ~%v", tStart, expected)
	}
}
