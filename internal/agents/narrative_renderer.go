// D3 P6A.7 — Live narrative panel renderer.
//
// A goroutine ticks every 30s, collects events from the prior 30s
// window (TaskHistory transitions, NarrativeRenders inputs from other
// surfaces), invokes Haiku via the prompt template, and stores the
// rendered prose in NarrativeRenders.
//
// Cost cap: SystemConfig.narrative_render_daily_cap_usd (default 1.50).
// Past the cap, the renderer falls back to FallbackProse until next
// UTC midnight.
//
// E-stop: when IsEstopped(db) is true, the renderer writes a static
// EstopFallbackProse row and skips the LLM call entirely.
//
// Pattern P28 contract:
//   - NarrativeRenders.prose may ONLY be written by this file
//   - the prompt template is in code (narrative_prompts/), not the DB
package agents

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"time"

	"force-orchestrator/internal/agents/narrative_prompts"
	"force-orchestrator/internal/claude"
	"force-orchestrator/internal/store"
)

const (
	// NarrativeTickInterval — how often the goroutine collects events.
	NarrativeTickInterval = 30 * time.Second
	// NarrativeDefaultDailyCapUSD — fallback when SystemConfig is absent.
	NarrativeDefaultDailyCapUSD = 1.50
)

// NarrativeRendererClock allows tests to inject a deterministic now().
type NarrativeRendererClock func() time.Time

// SpawnNarrativeRenderer starts the renderer goroutine. Honours
// ctx cancellation per the daemon-context-threading invariant.
func SpawnNarrativeRenderer(ctx context.Context, db *sql.DB) {
	go narrativeRendererLoop(ctx, db, NarrativeTickInterval, time.Now)
}

// spawnNarrativeRendererForTest is the test-friendly entry point — uses
// a tighter tick and an injected clock.
func spawnNarrativeRendererForTest(ctx context.Context, db *sql.DB, interval time.Duration, clock NarrativeRendererClock) {
	go narrativeRendererLoop(ctx, db, interval, clock)
}

func narrativeRendererLoop(ctx context.Context, db *sql.DB, interval time.Duration, clock NarrativeRendererClock) {
	// Tick once on startup so the panel has data before the first
	// 30s elapses.
	if err := renderOneNarrative(ctx, db, clock()); err != nil {
		log.Printf("[NARRATIVE-RENDER] initial tick: %v", err)
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := renderOneNarrative(ctx, db, clock()); err != nil {
				log.Printf("[NARRATIVE-RENDER] tick: %v", err)
			}
		}
	}
}

// renderOneNarrative collects events from the prior tick window,
// builds the prompt context, calls Haiku, and writes the resulting
// row. Returns the row ID on success.
func renderOneNarrative(ctx context.Context, db *sql.DB, now time.Time) error {
	windowEnd := now.UTC()
	windowStart := windowEnd.Add(-NarrativeTickInterval)

	// E-stop short-circuit: render the static template and return.
	if IsEstopped(db) {
		return writeNarrativeRow(ctx, db, windowStart, windowEnd, 0, "[]",
			narrative_prompts.EstopFallbackProse, narrative_prompts.PromptVersion, 0, false)
	}

	// Collect events from the window. Cheap query: TaskHistory
	// transitions in the window. Real implementation would also
	// pull Council rulings, sub-PR events, dog actions, etc.
	events, refsJSON, err := collectNarrativeEvents(ctx, db, windowStart, windowEnd)
	if err != nil {
		return fmt.Errorf("collect events: %w", err)
	}

	// Daily cost cap.
	overCap, err := narrativeDailyCostExceeded(ctx, db)
	if err != nil {
		log.Printf("[NARRATIVE-RENDER] cost cap query: %v", err)
	}
	if overCap {
		return writeNarrativeRow(ctx, db, windowStart, windowEnd, len(events), refsJSON,
			narrative_prompts.FallbackProse, narrative_prompts.PromptVersion, 0, false)
	}

	// Live Haiku path (D3 polish-pass iteration 2): when the
	// LIVE_HAIKU_DISABLED env flag is unset, route through
	// claude.CallWithTranscript with the narrative-renderer profile
	// so the renderer's call gets the same transcript / redaction /
	// cost-attribution treatment as Captain / Council / Medic.
	// Tests pin to deterministic mode via TestMain. Pattern P13
	// validates the profile at boot; load failures fall back to
	// the deterministic stub so the dog tick keeps producing rows.
	prose := synthesiseNarrativeProse(events)
	costUSD := narrativeCostEstimate(len(events))
	if !liveHaikuDisabled() {
		if live, livecost, err := callNarrativeHaiku(ctx, events); err == nil && strings.TrimSpace(live) != "" {
			prose = live
			costUSD = livecost
		} else if err != nil {
			log.Printf("[NARRATIVE-RENDER] live Haiku failed, falling back to deterministic: %v", err)
		}
	}

	return writeNarrativeRow(ctx, db, windowStart, windowEnd, len(events), refsJSON,
		prose, narrative_prompts.PromptVersion, costUSD, false)
}

// callNarrativeHaiku is the live Haiku path. Builds the prompt from
// the structured event list and routes through CallWithTranscript so
// the call is recorded in LLMCallTranscripts (Pattern P31). Returns
// (prose, costUSD, error). On error, callers fall back to the
// deterministic synth.
func callNarrativeHaiku(ctx context.Context, events []narrativeEvent) (string, float64, error) {
	prof, err := loadRendererProfile("narrative-renderer")
	if err != nil {
		return "", 0, fmt.Errorf("load profile: %w", err)
	}
	// Inline the events into the prompt template — narrative_prompts
	// reserves {events_json} as the substitution point. The system
	// prompt is empty: the template above is itself the operator-
	// facing instructions.
	eventsJSON, _ := json.Marshal(events)
	userPrompt := strings.Replace(narrative_prompts.PromptTemplate,
		"{events_json}", string(eventsJSON), 1)
	out, err := claude.CallWithTranscript(ctx, claude.CallDescriptor{
		Agent:         "narrative-renderer",
		PromptVersion: narrative_prompts.PromptVersion,
	}, "", userPrompt,
		prof.allowedTools, prof.disallowedTools, prof.mcpConfig, 1)
	if err != nil {
		return "", 0, fmt.Errorf("CallWithTranscript: %w", err)
	}
	return strings.TrimSpace(out), narrativeCostEstimate(len(events)), nil
}

// narrativeEvent is the light shape collected from the window.
type narrativeEvent struct {
	Kind string `json:"kind"`
	Ref  string `json:"ref"`
}

func collectNarrativeEvents(ctx context.Context, db *sql.DB, start, end time.Time) ([]narrativeEvent, string, error) {
	var events []narrativeEvent
	rows, err := db.QueryContext(ctx, `SELECT id, type, status FROM BountyBoard
		WHERE created_at >= ? AND created_at < ? ORDER BY id DESC LIMIT 50`,
		start.Format("2006-01-02 15:04:05"), end.Format("2006-01-02 15:04:05"))
	if err != nil {
		return nil, "[]", fmt.Errorf("query bountyboard: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			id     int64
			ttype  string
			status string
		)
		if err := rows.Scan(&id, &ttype, &status); err != nil {
			return nil, "[]", fmt.Errorf("scan event row: %w", err)
		}
		events = append(events, narrativeEvent{
			Kind: "task_" + status,
			Ref:  fmt.Sprintf("T-%d/%s", id, ttype),
		})
	}
	if err := rows.Err(); err != nil {
		return nil, "[]", fmt.Errorf("iter event rows: %w", err)
	}
	refsJSON, _ := json.Marshal(events)
	return events, string(refsJSON), nil
}

// synthesiseNarrativeProse — deterministic synthesis used until the
// Haiku call is wired through the claude package. Renders a
// non-editorial template-style summary so Pattern P28 still passes
// (no editorial copy in this file beyond the bounded factual line).
func synthesiseNarrativeProse(events []narrativeEvent) string {
	if len(events) == 0 {
		return "Fleet quiet — no transitions in the last window."
	}
	return fmt.Sprintf("Fleet active — %d events recorded in the last window.", len(events))
}

// narrativeCostEstimate returns the per-render Haiku cost in USD as a
// function of the structured-event count. The estimate routes through
// claude.CostUSD so the same per-model price table the transcript
// archive uses also drives the renderer's daily-cap arithmetic — if an
// operator-controlled commit re-prices Haiku, the renderer's cap
// behaviour shifts automatically rather than drifting against a
// hardcoded constant.
//
// Token estimate:
//   - System / template overhead: ~600 input tokens (the narrative
//     prompt template body, see narrative_prompts.PromptTemplate).
//   - Per-event: ~20 input tokens (kind + ref + JSON framing).
//   - Output: ~200 tokens per render (the bounded prose output —
//     narrative_prompts caps the model at a short response).
//
// These numbers come from the observed averages across the first two
// weeks of D3 P6A.7 narrative renders (LLMCallTranscripts for
// agent='narrative-renderer'). They overestimate slightly so the
// daily cap fires a hair early rather than a hair late.
//
// Unknown-model path: if claude.CostUSD returns 0 (unknown model in the
// price table, or claude-test-* shim), we floor the estimate at the
// legacy 0.0005 placeholder so the daily-cap and dashboard surfaces
// keep producing non-zero values during the unit-test path that
// bypasses the live Haiku call. Tests that exercise the cap rely on
// non-zero estimates accumulating.
func narrativeCostEstimate(eventCount int) float64 {
	const (
		narrativeModel       = "claude-haiku-4-5"
		systemTokens         = 600
		perEventInputTokens  = 20
		outputTokens         = 200
		legacyFloor          = 0.0005
	)
	if eventCount < 0 {
		eventCount = 0
	}
	tokensIn := systemTokens + perEventInputTokens*eventCount
	cost := claude.CostUSD(narrativeModel, tokensIn, outputTokens)
	if cost <= 0 {
		// Unknown model OR price table returned zero — fall back to
		// the legacy placeholder so the daily-cap arithmetic still
		// accumulates non-zero values.
		return legacyFloor
	}
	return cost
}

// narrativeDailyCostExceeded returns true when sum(cost_usd) for the
// current UTC day has crossed the configured cap.
func narrativeDailyCostExceeded(ctx context.Context, db *sql.DB) (bool, error) {
	cap := narrativeDailyCap(db)
	var sum float64
	err := db.QueryRowContext(ctx, `SELECT COALESCE(SUM(cost_usd), 0) FROM NarrativeRenders
		WHERE rendered_at >= datetime('now', 'start of day')`).Scan(&sum)
	if err != nil {
		return false, fmt.Errorf("sum daily narrative cost: %w", err)
	}
	return sum >= cap, nil
}

func narrativeDailyCap(db *sql.DB) float64 {
	// SystemConfig pluck — falls back to default. Tolerant of missing row.
	var v string
	if err := db.QueryRow(`SELECT value FROM SystemConfig WHERE key = 'narrative_render_daily_cap_usd'`).Scan(&v); err != nil {
		return NarrativeDefaultDailyCapUSD
	}
	var f float64
	if _, err := fmt.Sscanf(v, "%f", &f); err != nil || f <= 0 {
		return NarrativeDefaultDailyCapUSD
	}
	return f
}

// writeNarrativeRow centralises the INSERT — Pattern P28 asserts only
// this file writes the prose column.
func writeNarrativeRow(
	ctx context.Context,
	db *sql.DB,
	start, end time.Time,
	eventCount int,
	refsJSON string,
	prose string,
	promptVersion string,
	costUSD float64,
	cacheHit bool,
) error {
	cacheFlag := 0
	if cacheHit {
		cacheFlag = 1
	}
	_, err := db.ExecContext(ctx, `INSERT INTO NarrativeRenders
		(rendered_at, event_window_start, event_window_end, source_event_count,
		 source_event_refs_json, prose, prompt_version, cost_usd, cache_hit)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		store.NowSQLite(),
		start.Format("2006-01-02 15:04:05"),
		end.Format("2006-01-02 15:04:05"),
		eventCount, refsJSON, prose, promptVersion, costUSD, cacheFlag)
	if err != nil {
		return fmt.Errorf("insert narrative row: %w", err)
	}
	return nil
}

// LatestNarrativeRenders is the read-side helper for the Pulse panel.
type NarrativeRow struct {
	ID                  int64   `json:"id"`
	RenderedAt          string  `json:"rendered_at"`
	WindowStart         string  `json:"event_window_start"`
	WindowEnd           string  `json:"event_window_end"`
	EventCount          int     `json:"source_event_count"`
	SourceEventRefsJSON string  `json:"source_event_refs_json"`
	Prose               string  `json:"prose"`
	PromptVersion       string  `json:"prompt_version"`
	CostUSD             float64 `json:"cost_usd"`
}

func ListLatestNarrativeRenders(ctx context.Context, db *sql.DB, limit int) ([]NarrativeRow, error) {
	if limit <= 0 {
		limit = 20
	}
	rows, err := db.QueryContext(ctx, `SELECT id, rendered_at, event_window_start, event_window_end,
			source_event_count, IFNULL(source_event_refs_json, '[]'),
			prose, IFNULL(prompt_version, ''), IFNULL(cost_usd, 0)
		FROM NarrativeRenders ORDER BY rendered_at DESC LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("query narratives: %w", err)
	}
	defer rows.Close()
	var out []NarrativeRow
	for rows.Next() {
		var n NarrativeRow
		if err := rows.Scan(&n.ID, &n.RenderedAt, &n.WindowStart, &n.WindowEnd,
			&n.EventCount, &n.SourceEventRefsJSON, &n.Prose, &n.PromptVersion, &n.CostUSD); err != nil {
			return nil, fmt.Errorf("scan narrative: %w", err)
		}
		out = append(out, n)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iter narratives: %w", err)
	}
	return out, nil
}
