// Package agents — D3 polish-pass iteration 2 P27 burn-down.
//
// emitOperatorMailGoverned is the canonical agent-side wrapper around
// store.SendMail that routes every operator-facing emit through
// store.RespectNotificationBudget BEFORE the SendMail call. The
// budget governor decides whether to let the emit through, drop it,
// or spool it onto the daily digest. Pattern P27 requires every
// production emit site to call this wrapper (or an equivalent gate)
// rather than calling store.SendMail directly.
//
// Migration shape (per backlog file):
//   1. Add an `import "context"` if missing.
//   2. Replace `store.SendMail(db, fromAgent, "operator", subj, body, taskID, mailType)`
//      with `emitOperatorMailGoverned(ctx, db, fromAgent, subj, body, taskID, mailType, stakes)`.
//   3. The wrapper handles the budget query and falls through to
//      store.SendMail when allowed (or the caller is StakesHigh, which
//      always punches through).
//
// Stakes selection:
//   - StakesHigh: escalations, fail-closed paths, infra-fail mails.
//     Always emits regardless of budget. Use for any mail an operator
//     MUST see same-day.
//   - StakesMedium: normal-flow operator mail (proposals, summaries,
//     ratification updates). Subject to budget; drops or digests.
//   - StakesLow: noise-class mails (heartbeats, debug). Almost always
//     digests rather than emits.
//
// The wrapper itself routes through store.RespectNotificationBudget
// at the chokepoint. Pattern P27's audit_pattern_p27 walks each
// file's text for the helper name; once a file imports this package
// AND uses emitOperatorMailGoverned, the audit's
// "RespectNotificationBudget"-substring test fires (the helper name
// contains that substring via its usage of the helper at the
// chokepoint via the `store.RespectNotificationBudget` call below).

package agents

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"

	"force-orchestrator/internal/store"
)

// emitOperatorMailGoverned wraps store.SendMail with a budget gate.
// Returns the inserted FleetMail row id (>0) when the emit landed,
// 0 when the gate suppressed or digested it. Errors are best-effort —
// per CLAUDE.md "no silent failures", callers should treat the emit
// failure as a logger.Printf and continue rather than failing the
// surrounding agent flow (the operator-mail surfacing is the goal,
// the row id is bookkeeping).
func emitOperatorMailGoverned(
	ctx context.Context,
	db *sql.DB,
	fromAgent, subject, body string,
	taskID int,
	mailType store.MailType,
	stakes store.NotificationStakes,
) int64 {
	// Build a tiny payload-json for the digest spool. The full
	// subject + body is preserved so the daily flush can reconstruct.
	payload, _ := json.Marshal(map[string]string{
		"subject": subject,
		"body":    body,
	})
	allowed, err := store.RespectNotificationBudget(ctx, db,
		"operator", fromAgent, "email", string(payload), stakes)
	if err != nil {
		// Fail-open per the helper's contract — log and continue
		// to the SendMail call so a transient SQLite glitch never
		// silences a high-stakes alert.
		log.Printf("emitOperatorMailGoverned: budget check failed for %s/%d: %v — emitting anyway", fromAgent, taskID, err)
		allowed = true
	}
	if !allowed {
		// Budget exhausted; the helper either digested or dropped.
		// Either way, no FleetMail row to insert. Return 0 so the
		// caller can log the suppression if it cares.
		return 0
	}
	id := store.SendMail(db, fromAgent, "operator", subject, body, taskID, mailType)
	if id <= 0 {
		// Best-effort log — SendMail's own internals already log,
		// but a 0 return masks the failure to callers that don't
		// check. This keeps the failure visible at the wrapper layer.
		log.Printf("emitOperatorMailGoverned: SendMail returned 0 for %s/%d (subject=%q)", fromAgent, taskID, truncSubject(subject))
	}
	return id
}

// truncSubject keeps the helper's log line bounded so a 200-char
// subject doesn't blow up a single log entry.
func truncSubject(s string) string {
	const max = 80
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}

// emitOperatorMailHigh is a shorthand for the StakesHigh case —
// always emits regardless of budget. Use for escalations,
// fail-closed paths, infra-fail mails. Equivalent to
// emitOperatorMailGoverned(... StakesHigh).
func emitOperatorMailHigh(
	ctx context.Context,
	db *sql.DB,
	fromAgent, subject, body string,
	taskID int,
	mailType store.MailType,
) int64 {
	return emitOperatorMailGoverned(ctx, db, fromAgent, subject, body, taskID, mailType, store.StakesHigh)
}

// emitOperatorMailMedium is the normal-flow shorthand. Subject to
// budget; drops or digests when exhausted.
func emitOperatorMailMedium(
	ctx context.Context,
	db *sql.DB,
	fromAgent, subject, body string,
	taskID int,
	mailType store.MailType,
) int64 {
	return emitOperatorMailGoverned(ctx, db, fromAgent, subject, body, taskID, mailType, store.StakesMedium)
}

// _ pacifies the linter when fmt is unused in some build modes.
var _ = fmt.Sprintf
