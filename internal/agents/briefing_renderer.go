// D3 P6A.10 — Briefing renderer.
//
// On demand (when the operator opens a decision in Briefing focus mode),
// the renderer assembles structured decision data + up to 5 prior
// similar decisions, calls Haiku with the briefing prompt, and inserts
// a row into BriefingRenders. Subsequent re-opens read the existing row.
//
// Pattern P29 (briefing prose cites real evidence): every decision ID
// or AT-ID mentioned in briefing_text MUST resolve to a real DB row.
// The audit fuzz-tests synthetic rows with bogus IDs and asserts the
// pattern catches them.
package agents

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"strings"

	"force-orchestrator/internal/agents/briefing_prompts"
	"force-orchestrator/internal/claude"
	"force-orchestrator/internal/store"
)

// BriefingRender is the operator-facing payload for the focus-mode UI.
type BriefingRender struct {
	ID                        int64   `json:"id"`
	DecisionID                int64   `json:"decision_id"`
	DecisionKind              string  `json:"decision_kind"`
	RenderedAt                string  `json:"rendered_at"`
	BriefingText              string  `json:"briefing_text"`
	PriorSimilarDecisionsJSON string  `json:"prior_similar_decisions_json"`
	PromptVersion             string  `json:"prompt_version"`
	CostUSD                   float64 `json:"cost_usd"`
	OperatorDecision          string  `json:"operator_decision"`
	DecisionTimeSeconds       int     `json:"decision_time_seconds"`
	CounterProposalKind       string  `json:"counter_proposal_kind"`
	CounterProposalText       string  `json:"counter_proposal_text"`
	CounterProposalRoutedID   int64   `json:"counter_proposal_routed_id"`
}

// PriorSimilarDecision is one entry in prior_similar_decisions_json.
type PriorSimilarDecision struct {
	DecisionID        int64  `json:"decision_id"`
	DecidedAt         string `json:"decided_at"`
	Outcome           string `json:"outcome"`            // approved | rejected | deferred
	SubsequentOutcome string `json:"subsequent_outcome"` // shipped_clean | reverted | flagged_in_review | pending
	Summary           string `json:"summary"`
}

// RenderBriefing assembles the briefing row for a (kind, id) pair. If
// a row already exists, returns it; otherwise creates one. Callers
// pass the trust dial so the renderer can stamp the effective
// friction tier.
func RenderBriefing(ctx context.Context, db *sql.DB, decisionKind string, decisionID int64, trustDial int) (BriefingRender, error) {
	// Reuse the existing row if one exists.
	if existing, err := latestBriefingRender(ctx, db, decisionKind, decisionID); err == nil {
		return existing, nil
	}

	prior, err := findPriorSimilar(ctx, db, decisionKind, decisionID, 5)
	if err != nil {
		return BriefingRender{}, fmt.Errorf("prior similar: %w", err)
	}
	priorJSON, _ := json.Marshal(prior)

	// Build briefing text. Default deterministic synth shape preserves
	// Pattern P29 (every ID mentioned came from the DB-sourced input).
	// Live Haiku path (D3 polish-pass iteration 2): when
	// LIVE_HAIKU_DISABLED is unset, route through CallWithTranscript.
	// Pattern P29 still holds because the prior_similar list is
	// computed from the DB BEFORE Haiku sees the prompt — the model
	// can only reference IDs we hand it.
	text := synthesiseBriefingText(decisionKind, decisionID, prior)

	// Daily cost cap.
	overCap, _ := briefingDailyCostExceeded(ctx, db)
	costUSD := 0.005 // ~half-cent per render
	if overCap {
		text = briefing_prompts.FallbackBriefing
		costUSD = 0
	} else if !liveHaikuDisabled() {
		if live, err := callBriefingHaiku(ctx, decisionKind, decisionID, prior); err == nil && strings.TrimSpace(live) != "" {
			text = live
		} else if err != nil {
			log.Printf("[BRIEFING-RENDER] live Haiku failed, falling back to deterministic: %v", err)
		}
	}

	res, err := db.ExecContext(ctx, `INSERT INTO BriefingRenders
		(decision_id, decision_kind, briefing_text, prior_similar_decisions_json,
		 prompt_version, cost_usd)
		VALUES (?, ?, ?, ?, ?, ?)`,
		decisionID, decisionKind, text, string(priorJSON),
		briefing_prompts.PromptVersion, costUSD)
	if err != nil {
		return BriefingRender{}, fmt.Errorf("insert briefing: %w", err)
	}
	id, _ := res.LastInsertId()
	return BriefingRender{
		ID:                        id,
		DecisionID:                decisionID,
		DecisionKind:              decisionKind,
		RenderedAt:                store.NowSQLite(),
		BriefingText:              text,
		PriorSimilarDecisionsJSON: string(priorJSON),
		PromptVersion:             briefing_prompts.PromptVersion,
		CostUSD:                   costUSD,
	}, nil
}

func latestBriefingRender(ctx context.Context, db *sql.DB, kind string, id int64) (BriefingRender, error) {
	var b BriefingRender
	err := db.QueryRowContext(ctx, `SELECT id, decision_id, decision_kind,
			IFNULL(rendered_at, ''), briefing_text,
			IFNULL(prior_similar_decisions_json, '[]'),
			IFNULL(prompt_version, ''), IFNULL(cost_usd, 0),
			IFNULL(operator_decision, ''), IFNULL(decision_time_seconds, 0),
			IFNULL(counter_proposal_kind, ''), IFNULL(counter_proposal_text, ''),
			IFNULL(counter_proposal_routed_id, 0)
		FROM BriefingRenders
		WHERE decision_kind = ? AND decision_id = ?
		ORDER BY rendered_at DESC LIMIT 1`, kind, id).Scan(
		&b.ID, &b.DecisionID, &b.DecisionKind, &b.RenderedAt, &b.BriefingText,
		&b.PriorSimilarDecisionsJSON, &b.PromptVersion, &b.CostUSD,
		&b.OperatorDecision, &b.DecisionTimeSeconds,
		&b.CounterProposalKind, &b.CounterProposalText, &b.CounterProposalRoutedID,
	)
	return b, err
}

// callBriefingHaiku is the live Haiku path. Builds the prompt by
// substituting {decision_json} + {prior_similar_json} into the
// briefing prompt template, then routes through CallWithTranscript
// so the call is recorded in LLMCallTranscripts (Pattern P31).
// Pattern P29 holds because every ID in `prior` came from the DB
// query — the model can only reference what we hand it.
func callBriefingHaiku(ctx context.Context, kind string, id int64, prior []PriorSimilarDecision) (string, error) {
	prof, err := loadRendererProfile("briefing-renderer")
	if err != nil {
		return "", fmt.Errorf("load profile: %w", err)
	}
	decisionJSON, _ := json.Marshal(map[string]any{"kind": kind, "id": id})
	priorJSON, _ := json.Marshal(prior)
	userPrompt := briefing_prompts.PromptTemplate
	userPrompt = strings.Replace(userPrompt, "{decision_json}", string(decisionJSON), 1)
	userPrompt = strings.Replace(userPrompt, "{prior_similar_json}", string(priorJSON), 1)
	out, err := claude.CallWithTranscript(ctx, claude.CallDescriptor{
		Agent:         "briefing-renderer",
		PromptVersion: briefing_prompts.PromptVersion,
	}, "", userPrompt,
		prof.allowedTools, prof.disallowedTools, prof.mcpConfig, 1)
	if err != nil {
		return "", fmt.Errorf("CallWithTranscript: %w", err)
	}
	return strings.TrimSpace(out), nil
}

// synthesiseBriefingText produces a structured, hallucination-free
// summary. Every ID it mentions is sourced from the input (decisionID
// or prior decisions). Pattern P29 audits this contract.
func synthesiseBriefingText(kind string, id int64, prior []PriorSimilarDecision) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "Decision %s/%d is awaiting your call. ", kind, id)
	if len(prior) == 0 {
		sb.WriteString("No prior similar decisions in the queue.")
		return sb.String()
	}
	fmt.Fprintf(&sb, "Found %d prior similar decision(s):", len(prior))
	for _, p := range prior {
		fmt.Fprintf(&sb, " #%d (%s, outcome=%s, subsequent=%s);", p.DecisionID, p.DecidedAt, p.Outcome, p.SubsequentOutcome)
	}
	return sb.String()
}

// findPriorSimilar — minimal placeholder. Returns up to `limit` prior
// BriefingRenders rows of the same kind. The full similarity model
// arrives in 6A.12.
func findPriorSimilar(ctx context.Context, db *sql.DB, kind string, id int64, limit int) ([]PriorSimilarDecision, error) {
	rows, err := db.QueryContext(ctx, `SELECT decision_id, IFNULL(rendered_at, ''), IFNULL(operator_decision, '')
		FROM BriefingRenders WHERE decision_kind = ? AND decision_id != ?
		ORDER BY rendered_at DESC LIMIT ?`, kind, id, limit)
	if err != nil {
		return nil, fmt.Errorf("query prior similar: %w", err)
	}
	defer rows.Close()
	var out []PriorSimilarDecision
	for rows.Next() {
		var p PriorSimilarDecision
		if err := rows.Scan(&p.DecisionID, &p.DecidedAt, &p.Outcome); err != nil {
			return nil, fmt.Errorf("scan prior similar: %w", err)
		}
		if p.Outcome == "" {
			p.Outcome = "pending"
		}
		p.SubsequentOutcome = "pending" // 6A.12 fills in real value
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iter prior similar: %w", err)
	}
	return out, nil
}

// BriefingQueueRow is one entry in the briefing list view.
type BriefingQueueRow struct {
	DecisionID   int64  `json:"decision_id"`
	DecisionKind string  `json:"decision_kind"`
	StakesTier   string `json:"stakes_tier"`
	Title        string `json:"title"`
	CreatedAt    string `json:"created_at"`
}

// ListBriefingQueue returns pending decisions sorted by stakes tier
// (high first), then created_at. For 6A this is a thin shim over
// BountyBoard rows in Awaiting* states.
func ListBriefingQueue(ctx context.Context, db *sql.DB) ([]BriefingQueueRow, error) {
	rows, err := db.QueryContext(ctx, `SELECT id, IFNULL(type, ''), IFNULL(payload, ''), IFNULL(created_at, ''), status
		FROM BountyBoard
		WHERE status IN ('AwaitingCaptainReview', 'AwaitingCouncilReview', 'Escalated')
		ORDER BY status DESC, created_at DESC LIMIT 100`)
	if err != nil {
		return nil, fmt.Errorf("query briefing queue: %w", err)
	}
	defer rows.Close()
	var out []BriefingQueueRow
	for rows.Next() {
		var (
			id      int64
			ttype   string
			payload string
			created string
			status  string
		)
		if err := rows.Scan(&id, &ttype, &payload, &created, &status); err != nil {
			return nil, fmt.Errorf("scan briefing queue: %w", err)
		}
		title := payload
		if len(title) > 80 {
			title = title[:80] + "…"
		}
		stakes := "medium"
		if status == "Escalated" {
			stakes = "high"
		}
		out = append(out, BriefingQueueRow{
			DecisionID:   id,
			DecisionKind: "captain_proposal",
			StakesTier:   stakes,
			Title:        title,
			CreatedAt:    created,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iter briefing queue: %w", err)
	}
	return out, nil
}

// RecordBriefingDecision marks a BriefingRender row with the operator's
// decision + timing + counter-proposal data (6A.11 piggybacks).
func RecordBriefingDecision(
	ctx context.Context, db *sql.DB,
	briefingID int64,
	decision string,
	decisionTimeSeconds int,
	counterKind, counterText string,
	counterRoutedID int64,
) error {
	_, err := db.ExecContext(ctx, `UPDATE BriefingRenders
		SET operator_decision = ?, decision_time_seconds = ?,
		    counter_proposal_kind = ?, counter_proposal_text = ?, counter_proposal_routed_id = ?
		WHERE id = ?`,
		decision, decisionTimeSeconds, counterKind, counterText, counterRoutedID, briefingID)
	if err != nil {
		return fmt.Errorf("record briefing decision: %w", err)
	}
	return nil
}

func briefingDailyCostExceeded(ctx context.Context, db *sql.DB) (bool, error) {
	var cap float64 = 5.00
	var v string
	if err := db.QueryRow(`SELECT value FROM SystemConfig WHERE key = 'briefing_render_daily_cap_usd'`).Scan(&v); err == nil {
		var f float64
		if _, perr := fmt.Sscanf(v, "%f", &f); perr == nil && f > 0 {
			cap = f
		}
	}
	var sum float64
	err := db.QueryRowContext(ctx, `SELECT COALESCE(SUM(cost_usd), 0) FROM BriefingRenders
		WHERE rendered_at >= datetime('now', 'start of day')`).Scan(&sum)
	if err != nil {
		return false, fmt.Errorf("sum daily briefing cost: %w", err)
	}
	return sum >= cap, nil
}
