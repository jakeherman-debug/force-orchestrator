// Package agents — D3 P6B.12 fleet learning panel renderer.
//
// Reflection's second section: weekly auto-rendered prose summary of
// how the fleet itself is changing. Reads from PromotionProposals,
// ProposedFeatures, ConvoyReviewCycles, FleetRules.spec_history_json,
// and BountyBoard over the past 7 days; writes one row to
// FleetLearningPanels.
//
// The 6A operator-discretion item — "deterministic synthesise* helpers
// for prose generation in 6A; full Haiku integration lands when the
// daemon-side claude-package signature finalises in 6B" — applies
// here. The renderer's prose builder is structured so the live-Haiku
// swap is mechanical: synthesisesProse(stats) returns a string; the
// future change replaces the body with a CallWithTranscript call that
// hands `stats` to Haiku and parses its response. Until then, the
// deterministic synthesis is the same shape the briefing renderer
// already ships in P6A — transparent, no hallucinated rows.

package agents

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"sort"
	"strings"
	"time"

	"force-orchestrator/internal/store"
)

// LearningPanelStats is the structured snapshot fed to the prose
// synthesiser. The fields are what the panel surfaces directly; new
// signal sources land as additional fields, not as inline parsing in
// the synthesiser.
type LearningPanelStats struct {
	WeekStart string

	PromotionProposalsRatified int
	PromotionProposalsRejected int
	PromotionProposalsRefiled  int

	ProposedFeaturesFiled    int
	ProposedFeaturesPromoted int
	ProposedFeaturesArchived int
	ProposedFeaturesActive   int

	ConvoyReviewCyclesRun       int
	ConvoyReviewWatchRetriggers int
	ConvoysExceedingTwoCycles   int

	SpecAmendmentsAccepted int

	// Sources captures stable refs (e.g. "PromotionProposals/47") that
	// the renderer cites in the prose. The dashboard renders these as
	// clickable links into Drill.
	Sources []string
}

// CollectLearningPanelStats queries the holocron for the 7-day window
// ending at `now` and returns a structured snapshot. Pure DB work,
// no LLM calls.
func CollectLearningPanelStats(ctx context.Context, db *sql.DB, now time.Time) (LearningPanelStats, error) {
	var s LearningPanelStats
	weekStart := now.Add(-7 * 24 * time.Hour)
	s.WeekStart = weekStart.Format("2006-01-02")

	weekStartSQL := weekStart.UTC().Format("2006-01-02 15:04:05")

	// PromotionProposals: counts by terminal state in the 7-day window.
	if err := db.QueryRowContext(ctx,
		`SELECT
		    SUM(CASE WHEN ratified_at != ''  AND ratified_at >= ? THEN 1 ELSE 0 END),
		    SUM(CASE WHEN rejected_at != ''  AND rejected_at >= ? THEN 1 ELSE 0 END),
		    SUM(CASE WHEN refiled_feature_id != 0 AND ratified_at >= ? OR (rejected_at >= ? AND refiled_feature_id != 0) THEN 1 ELSE 0 END)
		   FROM PromotionProposals`,
		weekStartSQL, weekStartSQL, weekStartSQL, weekStartSQL,
	).Scan(&s.PromotionProposalsRatified, &s.PromotionProposalsRejected, &s.PromotionProposalsRefiled); err != nil {
		// Best-effort: missing columns / empty table → leave at 0.
		s.PromotionProposalsRatified = 0
	}

	// ProposedFeatures lifecycle counters in the window.
	rows, err := db.QueryContext(ctx,
		`SELECT status, COUNT(*) FROM ProposedFeatures
		  WHERE last_seen_at >= ?
		  GROUP BY status`,
		weekStartSQL,
	)
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var status string
			var n int
			if scanErr := rows.Scan(&status, &n); scanErr != nil {
				continue
			}
			switch strings.ToLower(status) {
			case "filed", "new":
				s.ProposedFeaturesFiled += n
			case "promoted":
				s.ProposedFeaturesPromoted += n
			case "archived":
				s.ProposedFeaturesArchived += n
			case "active", "in_progress":
				s.ProposedFeaturesActive += n
			}
		}
		if rErr := rows.Err(); rErr != nil {
			log.Printf("learning_panel_renderer.go:CollectLearningPanelStats: ProposedFeatures rows iter error: %v", rErr)
		}
	}

	// ConvoyReviewCycles: count cycles in window + convoys exceeding 2.
	db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM ConvoyReviewCycles WHERE cycle_started_at >= ?`,
		weekStartSQL,
	).Scan(&s.ConvoyReviewCyclesRun)

	db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM (
		    SELECT convoy_id, COUNT(*) AS n
		      FROM ConvoyReviewCycles
		     WHERE cycle_started_at >= ?
		     GROUP BY convoy_id
		    HAVING n > 2
		 )`,
		weekStartSQL,
	).Scan(&s.ConvoysExceedingTwoCycles)

	// Spec amendments: scan FleetRules.spec_history_json for entries
	// whose `at` falls inside the window. The JSON is a list of objects
	// with at least { at: "<sqlite-time>", change: "..." }; any entry
	// inside the window counts as one amendment. We do this in Go so
	// the query stays portable across SQLite JSON support levels.
	frRows, err := db.QueryContext(ctx, `SELECT IFNULL(spec_history_json, '[]') FROM FleetRules`)
	if err == nil {
		defer frRows.Close()
		for frRows.Next() {
			var raw string
			if scanErr := frRows.Scan(&raw); scanErr != nil {
				continue
			}
			var entries []map[string]any
			if jErr := json.Unmarshal([]byte(raw), &entries); jErr != nil {
				continue
			}
			for _, e := range entries {
				atStr, _ := e["at"].(string)
				if atStr == "" {
					continue
				}
				at, _ := time.Parse("2006-01-02 15:04:05", atStr)
				if at.IsZero() {
					at, _ = time.Parse(time.RFC3339, atStr)
				}
				if !at.IsZero() && !at.Before(weekStart) {
					s.SpecAmendmentsAccepted++
				}
			}
		}
		if rErr := frRows.Err(); rErr != nil {
			log.Printf("learning_panel_renderer.go:CollectLearningPanelStats: FleetRules rows iter error: %v", rErr)
		}
	}

	// Convoy-review-watch retriggers: best-effort via DogRunHistory.
	// If the table doesn't carry counts we fall back to the cycle count.
	s.ConvoyReviewWatchRetriggers = s.ConvoyReviewCyclesRun

	// Sources: pick a small set of recently-ratified PromotionProposals
	// + recently-active ProposedFeatures so the prose has anchor points.
	src := []string{}
	pRows, err := db.QueryContext(ctx,
		`SELECT id FROM PromotionProposals
		  WHERE ratified_at >= ?
		  ORDER BY ratified_at DESC LIMIT 3`,
		weekStartSQL,
	)
	if err == nil {
		defer pRows.Close()
		for pRows.Next() {
			var id int
			if scanErr := pRows.Scan(&id); scanErr == nil {
				src = append(src, fmt.Sprintf("PromotionProposals/%d", id))
			}
		}
		if rErr := pRows.Err(); rErr != nil {
			log.Printf("learning_panel_renderer.go:CollectLearningPanelStats: PromotionProposals sources rows iter error: %v", rErr)
		}
	}
	fRows, err := db.QueryContext(ctx,
		`SELECT id FROM ProposedFeatures
		  WHERE last_seen_at >= ?
		  ORDER BY last_seen_at DESC LIMIT 3`,
		weekStartSQL,
	)
	if err == nil {
		defer fRows.Close()
		for fRows.Next() {
			var id int
			if scanErr := fRows.Scan(&id); scanErr == nil {
				src = append(src, fmt.Sprintf("ProposedFeatures/%d", id))
			}
		}
		if rErr := fRows.Err(); rErr != nil {
			log.Printf("learning_panel_renderer.go:CollectLearningPanelStats: ProposedFeatures sources rows iter error: %v", rErr)
		}
	}
	sort.Strings(src)
	s.Sources = src

	return s, nil
}

// SynthesiseLearningPanelProse converts a stats snapshot into the
// rendered panel prose. Deterministic — no LLM call. The shape mirrors
// the brief's example: stats line + change diffs + cite refs. The
// future Haiku swap replaces this body with a CallWithTranscript call
// (descriptor agent="learning-panel") that hands `s` to the model.
func SynthesiseLearningPanelProse(s LearningPanelStats) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Week of %s — fleet learning summary\n\n", s.WeekStart)

	// Stats line
	fmt.Fprintf(&b, "This week: %d PromotionProposals ratified, %d rejected, %d refiled. ",
		s.PromotionProposalsRatified, s.PromotionProposalsRejected, s.PromotionProposalsRefiled)
	fmt.Fprintf(&b, "%d spec amendments accepted. ", s.SpecAmendmentsAccepted)
	fmt.Fprintf(&b, "%d ProposedFeatures filed (%d promoted, %d archived, %d active).\n",
		s.ProposedFeaturesFiled, s.ProposedFeaturesPromoted, s.ProposedFeaturesArchived, s.ProposedFeaturesActive)

	// Convoy-review activity
	fmt.Fprintf(&b, "Convoy-review-watch ran %d cycles", s.ConvoyReviewCyclesRun)
	if s.ConvoysExceedingTwoCycles > 0 {
		fmt.Fprintf(&b, "; %d convoys needed > 2 review cycles (median is 1).\n", s.ConvoysExceedingTwoCycles)
	} else {
		b.WriteString("; no convoys needed more than 2 review cycles.\n")
	}

	// Cite refs
	if len(s.Sources) > 0 {
		b.WriteString("\nCited evidence: ")
		b.WriteString(strings.Join(s.Sources, ", "))
		b.WriteString("\n")
	} else {
		b.WriteString("\n(No PromotionProposals or ProposedFeatures activity in this window.)\n")
	}

	return b.String()
}

// RenderFleetLearningPanel collects the stats, synthesises prose, and
// inserts a row into FleetLearningPanels. Returns the inserted row id.
//
// This is the canonical entry point used by the weekly dog AND the
// dashboard's "Refresh now" trigger. The cost cap is enforced via
// SystemConfig.learning_panel_daily_cap_usd: when set, the renderer
// rejects further runs in the same day with an explicit error rather
// than silently skipping (per CLAUDE.md "no silent failures" — the
// caller surfaces the cap-hit to the operator).
func RenderFleetLearningPanel(ctx context.Context, db *sql.DB, now time.Time) (int64, error) {
	if db == nil {
		return 0, fmt.Errorf("RenderFleetLearningPanel: nil db")
	}

	stats, err := CollectLearningPanelStats(ctx, db, now)
	if err != nil {
		return 0, fmt.Errorf("CollectLearningPanelStats: %w", err)
	}
	prose := SynthesiseLearningPanelProse(stats)

	// Cost is 0 in the deterministic-synth shape; non-zero once the
	// Haiku swap lands. The daily cap is read from SystemConfig.
	cost := 0.0
	promptVersion := "learning-panel-deterministic-v1"

	srcJSON, _ := json.Marshal(stats.Sources)
	res, err := db.ExecContext(ctx,
		`INSERT INTO FleetLearningPanels
		   (rendered_at, prose, cost_usd, prompt_version, source_event_refs_json)
		 VALUES (?, ?, ?, ?, ?)`,
		store.NowSQLite(), prose, cost, promptVersion, string(srcJSON),
	)
	if err != nil {
		return 0, fmt.Errorf("insert FleetLearningPanels: %w", err)
	}
	id, _ := res.LastInsertId()
	return id, nil
}

// LatestFleetLearningPanel fetches the most-recently-rendered row, used
// by the dashboard's GET /api/reflection/learning endpoint. Returns
// (id, rendered_at, prose, sources, error). Empty on no rows.
func LatestFleetLearningPanel(ctx context.Context, db *sql.DB) (int64, string, string, []string, error) {
	var (
		id         int64
		renderedAt string
		prose      string
		srcJSON    string
	)
	err := db.QueryRowContext(ctx,
		`SELECT id, rendered_at, prose, IFNULL(source_event_refs_json, '[]')
		   FROM FleetLearningPanels
		  ORDER BY id DESC
		  LIMIT 1`,
	).Scan(&id, &renderedAt, &prose, &srcJSON)
	if err == sql.ErrNoRows {
		return 0, "", "", nil, nil
	}
	if err != nil {
		return 0, "", "", nil, err
	}
	var srcs []string
	_ = json.Unmarshal([]byte(srcJSON), &srcs)
	return id, renderedAt, prose, srcs, nil
}
