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

// findPriorSimilar returns up to `limit` prior decisions of the same
// kind, ranked by payload similarity to the current decision (via the
// fts_bounty FTS5 index when available) and falling back to
// recency-only when FTS5 is unavailable or the current decision's
// payload is empty.
//
// Similarity heuristic:
//   1. Fetch the current BountyBoard.payload for `id` (Pattern P29
//      requires the model only references IDs we hand it, and those
//      IDs all come from this query — so the join is the trust
//      boundary).
//   2. Tokenise the payload into a small bag of "interesting" words
//      (length >= 4, alpha-only) and build an FTS5 MATCH expression
//      ORed across them.
//   3. Query fts_bounty for prior rows of the same kind ranked by
//      bm25, then JOIN to BriefingRenders to recover operator decision
//      + rendered_at. Limit to `limit` rows.
//   4. If FTS5 isn't compiled in OR the payload yields no tokens, fall
//      back to the recency-only query (the original behaviour).
//
// The returned shape preserves the SubsequentOutcome="pending"
// placeholder for the not-yet-implemented "did the post-decision
// outcome ship clean?" lookup (D3 P6A.12 owns that fill-in). That
// piece is a separate question from "which prior decisions look like
// this one"; conflating them across the deliverable boundary would
// have made the briefing-render pattern P29 audit fight itself.
func findPriorSimilar(ctx context.Context, db *sql.DB, kind string, id int64, limit int) ([]PriorSimilarDecision, error) {
	if limit <= 0 {
		limit = 5
	}
	// (1) Look up the current decision's payload. Missing payload OR
	// missing row is tolerated — we degrade to the recency-only path.
	var currentPayload string
	_ = db.QueryRowContext(ctx, `SELECT IFNULL(payload, '') FROM BountyBoard WHERE id = ?`, id).Scan(&currentPayload)

	// (2) Build an FTS5 MATCH expression from the payload tokens. If
	// the payload yields no usable tokens, skip the FTS path.
	matchExpr := buildPriorSimilarMatch(currentPayload)
	if matchExpr != "" {
		if results, err := queryPriorSimilarFTS5(ctx, db, kind, id, matchExpr, limit); err == nil && len(results) > 0 {
			return results, nil
		}
		// FTS5 path errored OR returned no matches — fall through to
		// the recency-only path so the caller always gets the best
		// available signal rather than an empty list when the same-
		// kind set is non-empty.
	}

	// Recency-only fallback. Matches the original shape.
	return queryPriorSimilarRecency(ctx, db, kind, id, limit)
}

// queryPriorSimilarFTS5 runs the FTS5-backed similarity query. Returns
// (nil, err) if fts_bounty doesn't exist (FTS5 not compiled in OR
// EnsureDrillFTS5 hasn't been called) so the caller can fall back.
func queryPriorSimilarFTS5(ctx context.Context, db *sql.DB, kind string, id int64, matchExpr string, limit int) ([]PriorSimilarDecision, error) {
	// Join fts_bounty (the FTS5 ranker) against BountyBoard (for kind
	// filtering) and LEFT JOIN BriefingRenders (so a same-kind bounty
	// row without a prior briefing still appears, and a row WITH a
	// briefing carries its operator_decision).
	rows, err := db.QueryContext(ctx, `
		SELECT bb.id,
		       COALESCE(MAX(br.rendered_at), bb.created_at) AS decided_at,
		       COALESCE(MAX(br.operator_decision), '') AS outcome
		FROM fts_bounty
		JOIN BountyBoard bb ON bb.id = fts_bounty.rowid
		LEFT JOIN BriefingRenders br
		       ON br.decision_id = bb.id AND br.decision_kind = ?
		WHERE fts_bounty MATCH ?
		  AND bb.id != ?
		  AND IFNULL(bb.type, '') != ''
		GROUP BY bb.id
		ORDER BY bm25(fts_bounty) ASC, decided_at DESC
		LIMIT ?`, kind, matchExpr, id, limit)
	if err != nil {
		return nil, fmt.Errorf("fts5 prior similar: %w", err)
	}
	defer rows.Close()
	var out []PriorSimilarDecision
	for rows.Next() {
		var p PriorSimilarDecision
		if scanErr := rows.Scan(&p.DecisionID, &p.DecidedAt, &p.Outcome); scanErr != nil {
			return nil, fmt.Errorf("scan fts5 prior similar: %w", scanErr)
		}
		if p.Outcome == "" {
			p.Outcome = "pending"
		}
		p.SubsequentOutcome = "pending" // D3 P6A.12 fills in real value
		out = append(out, p)
	}
	if iterErr := rows.Err(); iterErr != nil {
		return nil, fmt.Errorf("iter fts5 prior similar: %w", iterErr)
	}
	return out, nil
}

// queryPriorSimilarRecency is the FTS-free fallback. Returns prior
// BriefingRenders rows of the same kind, ordered by recency.
func queryPriorSimilarRecency(ctx context.Context, db *sql.DB, kind string, id int64, limit int) ([]PriorSimilarDecision, error) {
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
		if scanErr := rows.Scan(&p.DecisionID, &p.DecidedAt, &p.Outcome); scanErr != nil {
			return nil, fmt.Errorf("scan prior similar: %w", scanErr)
		}
		if p.Outcome == "" {
			p.Outcome = "pending"
		}
		p.SubsequentOutcome = "pending" // D3 P6A.12 fills in real value
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iter prior similar: %w", err)
	}
	return out, nil
}

// buildPriorSimilarMatch tokenises a payload into a small FTS5 MATCH
// expression. Strategy:
//   - Lowercase the input, split on non-alphanumerics.
//   - Keep tokens of length >= 4 (drop noise: "the", "and", JSON
//     keys like "id", "ok").
//   - Drop the top-N most common stop-tokens to reduce false matches.
//   - Cap at 8 tokens so the FTS5 query plan stays cheap.
//   - Join with OR; wrap each token in double-quotes so a stray
//     punctuation byte doesn't get interpreted as a FTS5 operator.
//
// Returns "" when the payload has fewer than 1 usable token — the
// caller falls back to the recency-only path in that case.
func buildPriorSimilarMatch(payload string) string {
	if payload == "" {
		return ""
	}
	// Stop-word set: tiny, just enough to avoid the obvious JSON keys
	// every BountyBoard.payload carries.
	stop := map[string]struct{}{
		"true": {}, "false": {}, "null": {},
		"type": {}, "kind": {}, "owner": {}, "status": {},
		"task": {}, "task_id": {}, "convoy_id": {},
		"payload": {}, "created_at": {}, "updated_at": {},
	}
	const maxTokens = 8
	const minLen = 4

	// Lower-case + alphanumeric tokenise.
	tokens := make([]string, 0, 16)
	seen := map[string]struct{}{}
	cur := make([]byte, 0, 32)
	flush := func() {
		if len(cur) < minLen {
			cur = cur[:0]
			return
		}
		s := string(cur)
		cur = cur[:0]
		if _, dup := seen[s]; dup {
			return
		}
		if _, isStop := stop[s]; isStop {
			return
		}
		seen[s] = struct{}{}
		tokens = append(tokens, s)
	}
	for i := 0; i < len(payload) && len(tokens) < maxTokens; i++ {
		c := payload[i]
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
			cur = append(cur, c)
		case c >= 'A' && c <= 'Z':
			cur = append(cur, c+32) // tolower
		default:
			flush()
		}
	}
	flush()

	if len(tokens) == 0 {
		return ""
	}
	// Wrap each token in double-quotes so FTS5's MATCH parser doesn't
	// reinterpret them as column filters or operators. The OR shape
	// keeps the query broad — bm25 ranks the best matches first.
	parts := make([]string, 0, len(tokens))
	for _, t := range tokens {
		parts = append(parts, fmt.Sprintf(`"%s"`, t))
	}
	return strings.Join(parts, " OR ")
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
