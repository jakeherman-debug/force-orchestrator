// D3 P6A.9 — "While you were away" cinematic on detected sleep wake.
//
// When the dashboard heartbeat reports a gap > 90s, infer the operator
// was offline. On dashboard reload, the cinematic plays in Pulse: a
// 30-second animated narrative replaying NarrativeRenders from the
// sleep window plus a summary card highlighting the highest-stakes
// pending decision.
//
// Compute side: aggregate events from the sleep window. The full
// animation is client-side (cinematic.js); this file produces the
// JSON the client renders.
package agents

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"force-orchestrator/internal/store"
)

// CinematicPayload is what /api/pulse/cinematic returns.
type CinematicPayload struct {
	SleepStartedAt    string                  `json:"sleep_started_at"`
	SleepEndedAt      string                  `json:"sleep_ended_at"`
	SleepDurationSec  int                     `json:"sleep_duration_sec"`
	NarrativeReplay   []NarrativeRow          `json:"narrative_replay"`
	TopDecision       *CinematicTopDecision   `json:"top_decision"`
	EventCount        int                     `json:"event_count"`
	TotalSpendUSD     float64                 `json:"total_spend_usd"`
	IsLongSleep       bool                    `json:"is_long_sleep"` // > 7 days
	Quiet             bool                    `json:"quiet"`         // no events at all
}

type CinematicTopDecision struct {
	DecisionKind string `json:"decision_kind"`
	DecisionID   int64  `json:"decision_id"`
	StakesTier   string `json:"stakes_tier"`
	Title        string `json:"title"`
}

// BuildCinematic assembles the cinematic payload for a sleep window.
// `since` is the sleep_started_at timestamp.
func BuildCinematic(ctx context.Context, db *sql.DB, since time.Time) (CinematicPayload, error) {
	now := time.Now().UTC()
	duration := now.Sub(since)

	out := CinematicPayload{
		SleepStartedAt:   since.UTC().Format(time.RFC3339),
		SleepEndedAt:     now.Format(time.RFC3339),
		SleepDurationSec: int(duration.Seconds()),
		IsLongSleep:      duration > 7*24*time.Hour,
	}

	// Replay NarrativeRenders from the window. Use the existing helper.
	rows, err := db.QueryContext(ctx, `SELECT id, IFNULL(rendered_at, ''), IFNULL(event_window_start, ''),
			IFNULL(event_window_end, ''), source_event_count, IFNULL(source_event_refs_json, '[]'),
			prose, IFNULL(prompt_version, ''), IFNULL(cost_usd, 0)
		FROM NarrativeRenders WHERE rendered_at >= ? ORDER BY rendered_at ASC LIMIT 100`,
		since.UTC().Format("2006-01-02 15:04:05"))
	if err != nil {
		return out, fmt.Errorf("query narratives: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var n NarrativeRow
		if err := rows.Scan(&n.ID, &n.RenderedAt, &n.WindowStart, &n.WindowEnd,
			&n.EventCount, &n.SourceEventRefsJSON, &n.Prose, &n.PromptVersion, &n.CostUSD); err != nil {
			return out, fmt.Errorf("scan narrative: %w", err)
		}
		out.NarrativeReplay = append(out.NarrativeReplay, n)
		out.EventCount += n.EventCount
		out.TotalSpendUSD += n.CostUSD
	}
	if err := rows.Err(); err != nil {
		return out, fmt.Errorf("iter narratives: %w", err)
	}

	if out.EventCount == 0 && len(out.NarrativeReplay) == 0 {
		out.Quiet = true
	}

	// Top decision: highest-stakes pending row, breaking ties by oldest.
	top, err := highestStakesPending(ctx, db)
	if err == nil && top != nil {
		out.TopDecision = top
	}

	return out, nil
}

func highestStakesPending(ctx context.Context, db *sql.DB) (*CinematicTopDecision, error) {
	var (
		id      int64
		ttype   string
		payload string
		status  string
	)
	err := db.QueryRowContext(ctx, `SELECT id, type, IFNULL(payload, ''), status FROM BountyBoard
		WHERE status IN ('Escalated', 'AwaitingCaptainReview', 'AwaitingCouncilReview')
		ORDER BY status DESC, id ASC LIMIT 1`).Scan(&id, &ttype, &payload, &status)
	if err != nil {
		return nil, err
	}
	stakes := "medium"
	if status == "Escalated" {
		stakes = "high"
	}
	title := payload
	if len(title) > 80 {
		title = title[:80] + "…"
	}
	return &CinematicTopDecision{
		DecisionKind: "captain_proposal",
		DecisionID:   id,
		StakesTier:   stakes,
		Title:        title,
	}, nil
}

// DetectSleepStartedAt checks DashboardHealthHeartbeats for a gap > 90s
// between consecutive ticks and returns the earlier tick's timestamp
// as the inferred sleep start. Returns (zero time, false) when no gap
// is detected.
//
// The brief: heartbeat goroutine ticks every 30s; gap > 90s = 3× the
// expected interval, so sleep is the most likely cause.
func DetectSleepStartedAt(ctx context.Context, db *sql.DB) (time.Time, bool) {
	var (
		latest, prior string
	)
	err := db.QueryRowContext(ctx, `SELECT ticked_at FROM DashboardHealthHeartbeats
		ORDER BY id DESC LIMIT 1`).Scan(&latest)
	if err != nil {
		return time.Time{}, false
	}
	err = db.QueryRowContext(ctx, `SELECT ticked_at FROM DashboardHealthHeartbeats
		WHERE id < (SELECT MAX(id) FROM DashboardHealthHeartbeats)
		ORDER BY id DESC LIMIT 1`).Scan(&prior)
	if err != nil {
		return time.Time{}, false
	}
	priorT, err := store.ParseSQLiteTime(prior)
	if err != nil {
		return time.Time{}, false
	}
	latestT, err := store.ParseSQLiteTime(latest)
	if err != nil {
		return time.Time{}, false
	}
	if latestT.Sub(priorT) > 90*time.Second {
		return priorT, true
	}
	return time.Time{}, false
}
