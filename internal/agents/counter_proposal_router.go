// D3 P6A.11 — Counter-proposal router.
//
// On high-stakes rejection in Briefing, the operator MUST choose one
// of three counter-proposal kinds:
//   - whole_thing       — text reason >= 20 chars
//   - different_approach — operator-drafted alternative >= 50 chars,
//                          routed back to Captain/EC for refinement
//   - defer             — kicks to Investigator's intake stream
//
// This file is the routing logic; the API layer (handlers_briefing.go)
// validates the incoming JSON and calls RouteCounterProposal.
package agents

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"force-orchestrator/internal/store"
)

// CounterProposalKind enumerates the operator's three rejection options.
type CounterProposalKind string

const (
	CounterProposalWholeThing        CounterProposalKind = "whole_thing"
	CounterProposalDifferentApproach CounterProposalKind = "different_approach"
	CounterProposalDefer             CounterProposalKind = "defer"
)

// MinTextWholeThing — the brief: rejection reason must be substantive.
const MinTextWholeThing = 20

// MinTextDifferentApproach — the alternative must be actionable.
const MinTextDifferentApproach = 50

var (
	ErrCounterKindRequired       = errors.New("counter_proposal_kind required for high-stakes rejection")
	ErrCounterKindUnknown        = errors.New("counter_proposal_kind must be whole_thing|different_approach|defer")
	ErrWholeThingTextTooShort    = errors.New("whole_thing requires text >= 20 chars")
	ErrDifferentApproachTooShort = errors.New("different_approach requires text >= 50 chars")
)

// RouteCounterProposal validates the inputs, records them on the
// BriefingRenders row, and routes downstream:
//   - whole_thing       — record only; the rejection IS the action.
//   - different_approach — insert a new BountyBoard row owned by Captain
//                          (or EC for promotion proposals) with the
//                          operator's draft as payload.
//   - defer             — insert a row in BountyBoard tagged for
//                          Investigator follow-up.
//
// Returns the new proposal/task ID or 0 for whole_thing.
func RouteCounterProposal(
	ctx context.Context, db *sql.DB,
	briefingID int64,
	decisionKind string,
	kind CounterProposalKind,
	text string,
) (newID int64, err error) {
	switch kind {
	case CounterProposalWholeThing:
		if len(text) < MinTextWholeThing {
			return 0, ErrWholeThingTextTooShort
		}
		// Record only — no downstream task.
		return 0, recordCounterProposalOnBriefing(ctx, db, briefingID, kind, text, 0)

	case CounterProposalDifferentApproach:
		if len(text) < MinTextDifferentApproach {
			return 0, ErrDifferentApproachTooShort
		}
		newID, err = insertCounterProposalTask(ctx, db, decisionKind, text, "captain")
		if err != nil {
			return 0, fmt.Errorf("route different_approach: %w", err)
		}
		return newID, recordCounterProposalOnBriefing(ctx, db, briefingID, kind, text, newID)

	case CounterProposalDefer:
		// `defer` allows empty text per the brief.
		newID, err = insertInvestigatorAttention(ctx, db, decisionKind, text)
		if err != nil {
			return 0, fmt.Errorf("route defer: %w", err)
		}
		return newID, recordCounterProposalOnBriefing(ctx, db, briefingID, kind, text, newID)

	default:
		return 0, ErrCounterKindUnknown
	}
}

func recordCounterProposalOnBriefing(ctx context.Context, db *sql.DB, briefingID int64, kind CounterProposalKind, text string, routedID int64) error {
	_, err := db.ExecContext(ctx, `UPDATE BriefingRenders
		SET counter_proposal_kind = ?, counter_proposal_text = ?, counter_proposal_routed_id = ?,
		    operator_decision = 'rejected'
		WHERE id = ?`,
		string(kind), text, routedID, briefingID)
	if err != nil {
		return fmt.Errorf("record counter on briefing: %w", err)
	}
	return nil
}

func insertCounterProposalTask(ctx context.Context, db *sql.DB, decisionKind, draft, owner string) (int64, error) {
	res, err := db.ExecContext(ctx, `INSERT INTO BountyBoard
		(type, payload, owner, status, created_at)
		VALUES (?, ?, ?, 'Pending', ?)`,
		"CodeEdit", "[counter-proposal] "+draft, owner, store.NowSQLite())
	if err != nil {
		return 0, fmt.Errorf("insert counter task: %w", err)
	}
	return res.LastInsertId()
}

func insertInvestigatorAttention(ctx context.Context, db *sql.DB, decisionKind, text string) (int64, error) {
	payload := "[REJECTED_NEEDS_INVESTIGATION] from " + decisionKind
	if text != "" {
		payload += " — " + text
	}
	res, err := db.ExecContext(ctx, `INSERT INTO BountyBoard
		(type, payload, owner, status, created_at)
		VALUES ('Investigate', ?, 'investigator', 'Pending', ?)`,
		payload, store.NowSQLite())
	if err != nil {
		return 0, fmt.Errorf("insert investigator attention: %w", err)
	}
	return res.LastInsertId()
}
