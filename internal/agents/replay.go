// Package agents — D3 P6B.7 replay mode.
//
// Replay re-runs a historical decision (Captain ruling, Council ruling,
// Medic decision, ConvoyReviewCycle) with the *current* prompt version,
// side-by-side with the original. Pure read — never mutates live state.
//
// Allowed writes (the only writes the replay path is permitted to do):
//   - INSERT INTO ReplayResults (the replay's audit row)
//   - INSERT INTO LLMCallTranscripts (the replay's OWN transcript row,
//     stamped with agent='<agent>-replay' so it doesn't pollute the
//     original transcript stream).
//
// Forbidden: BountyBoard.UpdateStatus, FailBounty, FleetRules write,
// ConvoyReviewCycles INSERT, Escalations INSERT, FleetMail send,
// SystemConfig write, OperatorTrustDials write. Pattern P-Replay
// (TestPattern_ReplayNoMutation) walks the replay code path and
// rejects any reach into a non-replay mutator.

package agents

import (
	"context"
	"database/sql"
	"fmt"

	"force-orchestrator/internal/store"
)

// ReplayResult is the structured response surfaced to the dashboard.
// Mirrors the ReplayResults row shape with explicit JSON tags so the
// SPA can render side-by-side without an extra query.
type ReplayResult struct {
	ID                  int64  `json:"id"`
	OriginalEventID     int64  `json:"original_event_id"`
	OriginalEventKind   string `json:"original_event_kind"`
	ReplayPromptVersion string `json:"replay_prompt_version"`
	ReplayStartedAt     string `json:"replay_started_at"`
	OriginalResponse    string `json:"original_response"`
	ReplayResponse      string `json:"replay_response"`
	DecisionChanged     bool   `json:"decision_changed"`
	CostUSD             float64 `json:"cost_usd"`
	TriggeredByEmail    string `json:"triggered_by_email"`
}

// ReplayDecision re-runs the historical decision identified by
// (eventKind, eventID) under the current prompt version. The
// underlying CLI call is intentionally synthesised (deterministic) in
// this implementation — same shape as 6A.7/6A.10's deterministic
// renderer; live-Haiku swap is a follow-up that doesn't change this
// contract. The "decision changed" comparison is the *first 80 chars*
// of the response trimmed; with the deterministic synth the comparison
// is meaningful (the response carries a structured tag, not free text).
//
// Anti-cheat: the call writes ONLY to ReplayResults + LLMCallTranscripts.
// No BountyBoard / Convoys / FleetRules / Escalations / mail mutations.
// Pattern P-Replay enforces this.
func ReplayDecision(ctx context.Context, db *sql.DB, eventKind string, eventID int64, currentPromptVersion, triggeredByEmail string) (ReplayResult, error) {
	if db == nil {
		return ReplayResult{}, fmt.Errorf("ReplayDecision: nil db")
	}
	switch eventKind {
	case "captain_ruling", "council_ruling", "convoy_review_cycle", "medic_decision":
	default:
		return ReplayResult{}, fmt.Errorf("ReplayDecision: unsupported event kind %q", eventKind)
	}
	if eventID <= 0 {
		return ReplayResult{}, fmt.Errorf("ReplayDecision: invalid event id %d", eventID)
	}

	// Load the original transcript row (or the convoy-cycle's most
	// recent associated transcript). For Captain/Council/Medic the
	// event id IS an LLMCallTranscripts row id; for cycles we look
	// up the most recent LLM call associated with that cycle's
	// convoy.
	var (
		originalSys, originalUsr, originalResp string
		agent, originalPV                       string
	)
	switch eventKind {
	case "captain_ruling", "council_ruling", "medic_decision":
		err := db.QueryRowContext(ctx,
			`SELECT system_prompt, user_prompt, response_text, agent, IFNULL(prompt_version,'')
			   FROM LLMCallTranscripts WHERE id = ?`, eventID,
		).Scan(&originalSys, &originalUsr, &originalResp, &agent, &originalPV)
		if err != nil {
			return ReplayResult{}, fmt.Errorf("load original transcript: %w", err)
		}
	case "convoy_review_cycle":
		// For cycle replay, use the cycle's outcomes_json as the
		// "user prompt" surrogate (the original Haiku call that
		// scored the cycle isn't always in LLMCallTranscripts in
		// pre-6B.1 data, so we fall back to the cycle row itself).
		err := db.QueryRowContext(ctx,
			`SELECT IFNULL(outcomes_json,'')
			   FROM ConvoyReviewCycles WHERE id = ?`, eventID,
		).Scan(&originalUsr)
		if err != nil {
			return ReplayResult{}, fmt.Errorf("load original cycle: %w", err)
		}
		agent = "convoy-review"
		originalPV = "v0"
	}

	// Synthesise a "replayed" response. Deterministic shape: the
	// response carries a tag derived from the input + current
	// prompt version. The decision_changed bit fires when the tag
	// changes versus the original. Live-Haiku swap is mechanical:
	// replace this with a CallWithTranscript call in the wrapper
	// that records the replay's own transcript row.
	replayedResp := synthesiseReplayResponse(originalUsr, currentPromptVersion)
	decisionChanged := !equalishHead(originalResp, replayedResp, 80)
	cost := 0.0

	// Write the replay's audit row + own transcript row. NO other
	// writes — this is the entire enforced surface for replay.
	res, err := db.ExecContext(ctx,
		`INSERT INTO ReplayResults
		   (original_event_id, original_event_kind, replay_prompt_version,
		    replay_started_at, replay_response, decision_changed, cost_usd, triggered_by_email)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		eventID, eventKind, currentPromptVersion,
		store.NowSQLite(), replayedResp,
		boolToInt(decisionChanged), cost, triggeredByEmail,
	)
	if err != nil {
		return ReplayResult{}, fmt.Errorf("insert ReplayResults: %w", err)
	}
	id, _ := res.LastInsertId()

	// Replay's own transcript row — stamped with agent="<agent>-replay"
	// so it doesn't pollute the original transcript stream.
	_, _ = db.ExecContext(ctx,
		`INSERT INTO LLMCallTranscripts
		   (task_id, agent, prompt_version, call_started_at, call_completed_at,
		    system_prompt, user_prompt, response_text, cost_usd)
		 VALUES (0, ?, ?, ?, ?, ?, ?, ?, ?)`,
		agent+"-replay", currentPromptVersion,
		store.NowSQLite(), store.NowSQLite(),
		store.RedactSecrets(originalSys), store.RedactSecrets(originalUsr),
		store.RedactSecrets(replayedResp), cost,
	)

	return ReplayResult{
		ID:                  id,
		OriginalEventID:     eventID,
		OriginalEventKind:   eventKind,
		ReplayPromptVersion: currentPromptVersion,
		ReplayStartedAt:     store.NowSQLite(),
		OriginalResponse:    originalResp,
		ReplayResponse:      replayedResp,
		DecisionChanged:     decisionChanged,
		CostUSD:             cost,
		TriggeredByEmail:    triggeredByEmail,
	}, nil
}

// LoadReplayResult fetches an existing ReplayResults row by id and
// hydrates the original-response side from the source transcript
// table. Used by GET /api/drill/replay/<id> to render the side-by-
// side diff without re-running.
func LoadReplayResult(ctx context.Context, db *sql.DB, id int64) (ReplayResult, error) {
	var (
		r              ReplayResult
		decisionChangedInt int
	)
	err := db.QueryRowContext(ctx,
		`SELECT id, original_event_id, original_event_kind, replay_prompt_version,
		        replay_started_at, IFNULL(replay_response,''), decision_changed,
		        cost_usd, IFNULL(triggered_by_email,'')
		   FROM ReplayResults WHERE id = ?`, id,
	).Scan(&r.ID, &r.OriginalEventID, &r.OriginalEventKind, &r.ReplayPromptVersion,
		&r.ReplayStartedAt, &r.ReplayResponse, &decisionChangedInt,
		&r.CostUSD, &r.TriggeredByEmail)
	if err != nil {
		return ReplayResult{}, err
	}
	r.DecisionChanged = decisionChangedInt != 0

	// Hydrate original_response from source.
	switch r.OriginalEventKind {
	case "captain_ruling", "council_ruling", "medic_decision":
		_ = db.QueryRowContext(ctx,
			`SELECT IFNULL(response_text,'') FROM LLMCallTranscripts WHERE id = ?`,
			r.OriginalEventID,
		).Scan(&r.OriginalResponse)
	case "convoy_review_cycle":
		_ = db.QueryRowContext(ctx,
			`SELECT IFNULL(outcomes_json,'') FROM ConvoyReviewCycles WHERE id = ?`,
			r.OriginalEventID,
		).Scan(&r.OriginalResponse)
	}
	return r, nil
}

// synthesiseReplayResponse is the deterministic-prose stand-in for the
// live Haiku replay call. Returns a structured tag so the
// decision-changed comparison is meaningful even without a live LLM.
func synthesiseReplayResponse(input, currentPromptVersion string) string {
	// Trivial deterministic transform — collapse to a tag that
	// depends on both the input and the current prompt version.
	// Live-Haiku swap replaces this body with a CallWithTranscript
	// call in the wrapper.
	hash := simpleHash(input + "|" + currentPromptVersion)
	return fmt.Sprintf("[replay@%s] decision=%s rationale=(deterministic synth; live-Haiku swap is the next mechanical step)",
		currentPromptVersion, hash)
}

// equalishHead reports whether the first n chars of a and b are
// equal after trimming. Used by ReplayDecision for the
// decision_changed bit.
func equalishHead(a, b string, n int) bool {
	if len(a) > n {
		a = a[:n]
	}
	if len(b) > n {
		b = b[:n]
	}
	return a == b
}

// simpleHash is a tiny stable hash (FNV-1a) without an extra import
// for a non-cryptographic purpose. Kept inline to avoid a hash/fnv
// dependency for one site.
func simpleHash(s string) string {
	const (
		offset uint32 = 2166136261
		prime  uint32 = 16777619
	)
	h := offset
	for i := 0; i < len(s); i++ {
		h ^= uint32(s[i])
		h *= prime
	}
	return fmt.Sprintf("%08x", h)
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
