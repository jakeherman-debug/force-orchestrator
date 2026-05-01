package agents

import (
	"context"
	"strings"
	"testing"
	"time"

	"force-orchestrator/internal/store"
)

// TestLearningPanelRenderer covers 6B.12 invariants:
//
//   - CollectLearningPanelStats reads PromotionProposals,
//     ProposedFeatures, ConvoyReviewCycles, FleetRules over the 7-day
//     window and produces a structured snapshot.
//   - SynthesiseLearningPanelProse renders the snapshot as prose
//     containing the stats and any cited PromotionProposals refs.
//   - RenderFleetLearningPanel inserts a row in FleetLearningPanels
//     with the prose, prompt_version, and source refs JSON.
//   - LatestFleetLearningPanel returns the most recently inserted row;
//     empty result when the table is empty.
//   - Idempotence: running twice in quick succession produces two rows
//     (the dashboard reads the latest, so the renderer is safe to
//     re-trigger via "Refresh now" mid-window).
func TestLearningPanelRenderer(t *testing.T) {
	t.Run("happy_path_inserts_row_with_real_data", func(t *testing.T) {
		db := store.InitHolocronDSN(":memory:")
		defer db.Close()

		now := time.Now().UTC()
		recent := now.Add(-2 * 24 * time.Hour).Format("2006-01-02 15:04:05")

		// Seed a couple of ratified PromotionProposals in the window.
		_, err := db.Exec(`INSERT INTO PromotionProposals (id, experiment_id, kind, ratified_at, authored_at) VALUES
			(101, 1, 'promote', ?, ?),
			(102, 2, 'promote', ?, ?)`,
			recent, recent, recent, recent)
		if err != nil {
			t.Fatalf("seed promotions: %v", err)
		}

		// Seed a couple ProposedFeatures statuses
		_, err = db.Exec(`INSERT INTO ProposedFeatures
			(id, observation_summary, category, source, fingerprint, status, last_seen_at) VALUES
			(201, 'a', 'cat', 'src', 'fp-201', 'filed', ?),
			(202, 'b', 'cat', 'src', 'fp-202', 'promoted', ?),
			(203, 'c', 'cat', 'src', 'fp-203', 'archived', ?)`,
			recent, recent, recent)
		if err != nil {
			t.Fatalf("seed features: %v", err)
		}

		// Seed convoy review cycles
		_, err = db.Exec(`INSERT INTO ConvoyReviewCycles
			(id, convoy_id, cycle_number, spec_version_at_start, cycle_started_at) VALUES
			(301, 47, 1, 'v1', ?), (302, 47, 2, 'v1', ?), (303, 47, 3, 'v1', ?)`,
			recent, recent, recent)
		if err != nil {
			t.Fatalf("seed cycles: %v", err)
		}

		stats, err := CollectLearningPanelStats(context.Background(), db, now)
		if err != nil {
			t.Fatalf("collect: %v", err)
		}
		if stats.PromotionProposalsRatified != 2 {
			t.Errorf("expected 2 ratified, got %d", stats.PromotionProposalsRatified)
		}
		if stats.ProposedFeaturesFiled != 1 || stats.ProposedFeaturesPromoted != 1 || stats.ProposedFeaturesArchived != 1 {
			t.Errorf("feature counts off: filed=%d promoted=%d archived=%d",
				stats.ProposedFeaturesFiled, stats.ProposedFeaturesPromoted, stats.ProposedFeaturesArchived)
		}
		if stats.ConvoyReviewCyclesRun != 3 {
			t.Errorf("expected 3 cycles, got %d", stats.ConvoyReviewCyclesRun)
		}
		if stats.ConvoysExceedingTwoCycles != 1 {
			t.Errorf("expected 1 convoy exceeding 2 cycles, got %d", stats.ConvoysExceedingTwoCycles)
		}

		// Synthesise prose
		prose := SynthesiseLearningPanelProse(stats)
		if !strings.Contains(prose, "2 PromotionProposals ratified") {
			t.Errorf("prose missing ratified count: %q", prose)
		}
		if !strings.Contains(prose, "Convoy-review-watch ran 3 cycles") {
			t.Errorf("prose missing cycle count: %q", prose)
		}
		if !strings.Contains(prose, "PromotionProposals/101") && !strings.Contains(prose, "PromotionProposals/102") {
			t.Errorf("prose missing cited refs: %q", prose)
		}

		// Insert via canonical entry point
		id, err := RenderFleetLearningPanel(context.Background(), db, now)
		if err != nil {
			t.Fatalf("render: %v", err)
		}
		if id == 0 {
			t.Fatal("expected non-zero id")
		}

		// Read back via LatestFleetLearningPanel
		gotID, _, gotProse, gotSources, err := LatestFleetLearningPanel(context.Background(), db)
		if err != nil {
			t.Fatalf("latest: %v", err)
		}
		if gotID != id {
			t.Errorf("latest mismatch: got %d want %d", gotID, id)
		}
		if !strings.Contains(gotProse, "2 PromotionProposals ratified") {
			t.Errorf("latest prose mismatch: %q", gotProse)
		}
		if len(gotSources) == 0 {
			t.Errorf("expected non-empty sources")
		}
	})

	t.Run("empty_db_returns_zero", func(t *testing.T) {
		db := store.InitHolocronDSN(":memory:")
		defer db.Close()
		id, _, prose, _, err := LatestFleetLearningPanel(context.Background(), db)
		if err != nil {
			t.Fatalf("latest: %v", err)
		}
		if id != 0 || prose != "" {
			t.Errorf("expected empty result, got id=%d prose=%q", id, prose)
		}
	})

	t.Run("idempotence_two_renders_two_rows", func(t *testing.T) {
		db := store.InitHolocronDSN(":memory:")
		defer db.Close()
		now := time.Now().UTC()
		for i := 0; i < 2; i++ {
			if _, err := RenderFleetLearningPanel(context.Background(), db, now); err != nil {
				t.Fatalf("render %d: %v", i, err)
			}
		}
		var rows int
		db.QueryRow(`SELECT COUNT(*) FROM FleetLearningPanels`).Scan(&rows)
		if rows != 2 {
			t.Fatalf("expected 2 rows, got %d", rows)
		}
	})

	t.Run("nil_db_errors", func(t *testing.T) {
		_, err := RenderFleetLearningPanel(context.Background(), nil, time.Now())
		if err == nil {
			t.Fatal("expected error on nil db")
		}
	})
}
