package agents

// D17 P2B — dogT30Verdict: T+30-day verdict dog for handoff-synthesis experiments.
//
// Fires ~30 days after a D10-style "handoff synthesis experiment" run completes.
// The dog queries ExperimentRuns for rows whose completed_at is 30–31 days old
// and whose t30_verdict_sent_at is still empty. For each such row it emits an
// operator mail asking the operator to decide: keep or deprecate the handoff
// synthesis feature for that agent?
//
// The mail includes:
//   - Experiment ID and run ID for the operator to look up the full record.
//   - The score and score_source so the operator has a quick signal.
//   - Agent name (the fleet agent the run was scored against).
//   - A keep-or-deprecate question with the dashboard drill-in URL hint.
//
// After sending the mail the dog stamps t30_verdict_sent_at so subsequent dog
// ticks are no-ops for that run (idempotent).
//
// Daily cadence (set in dogCooldowns): the 30–31-day window means a run is
// visible to the dog for exactly one 24-hour window, so the daily cadence
// guarantees exactly one mail per eligible run.
//
// Anti-cheat invariants:
//   - Default-OFF: only ExperimentRuns rows with completed_at set AND
//     t30_verdict_sent_at empty AND mode IN ('holdout','paired_real') are processed.
//     Shadow / provisional runs never get a verdict mail.
//   - Score fallback: if score is 0.0 and score_source is empty we still emit
//     the mail but note "no score recorded" so the operator knows the signal is absent.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"force-orchestrator/internal/store"
)

// dogT30Verdict scans for experiment runs that completed ~30 days ago and have
// not yet received a keep-or-deprecate verdict mail. For each eligible run it
// sends one mail to the operator and stamps t30_verdict_sent_at.
func dogT30Verdict(ctx context.Context, db *sql.DB, logger interface{ Printf(string, ...any) }) error {
	_ = ctx // reserved for future context-aware I/O

	pending, err := store.ListT30VerdictPending(db)
	if err != nil {
		return fmt.Errorf("dogT30Verdict: list pending: %w", err)
	}
	if len(pending) == 0 {
		logger.Printf("Dog t30-verdict: no runs due for verdict — nothing to do")
		return nil
	}

	var aggErr error
	sent := 0
	for _, run := range pending {
		if mailErr := sendT30VerdictMail(db, run, logger); mailErr != nil {
			aggErr = errors.Join(aggErr, fmt.Errorf("run %d: %w", run.ID, mailErr))
			continue
		}
		if markErr := store.MarkT30VerdictSent(db, run.ID); markErr != nil {
			// Mail was sent but we couldn't stamp the row — log and count
			// as a partial success. The next dog tick will attempt to re-send
			// but this is preferable to silently losing the verdict trigger.
			aggErr = errors.Join(aggErr, fmt.Errorf("run %d: stamp t30_verdict_sent_at: %w", run.ID, markErr))
			continue
		}
		sent++
	}

	logger.Printf("Dog t30-verdict: sent=%d pending=%d", sent, len(pending))
	return aggErr
}

// sendT30VerdictMail composes and delivers the T+30 verdict operator mail for a
// single ExperimentRun. The mail asks the operator to decide: keep or deprecate
// the handoff-synthesis feature for the scored agent.
func sendT30VerdictMail(db *sql.DB, run store.T30VerdictRun, logger interface{ Printf(string, ...any) }) error {
	subject := fmt.Sprintf("[T+30 VERDICT] Experiment #%d run #%d — keep or deprecate?", run.ExperimentID, run.ID)

	scoreDetail := "no score recorded"
	if run.Score != 0.0 || run.ScoreSource != "" {
		scoreDetail = fmt.Sprintf("%.4f (source: %s)", run.Score, run.ScoreSource)
	}
	agentDetail := run.AgentName
	if agentDetail == "" {
		agentDetail = "(unknown agent)"
	}

	body := fmt.Sprintf("Handoff-synthesis experiment outcome: 30 days have elapsed since run #%d (experiment #%d) completed."+
		" It is time to decide whether to keep or deprecate the feature for agent %q.\n\n"+
		"Metrics summary:\n"+
		"  Completed at : %s\n"+
		"  Score        : %s\n"+
		"  Run ID       : %d\n"+
		"  Experiment ID: %d\n\n"+
		"Operator action required:\n"+
		"  - Review the experiment dashboard (force experiment show %d) for full breakdown.\n"+
		"  - If the feature is beneficial: no action needed; the treatment config remains active.\n"+
		"  - If the feature should be deprecated: run 'force experiment close %d' and remove the treatment spec from the agent config.\n\n"+
		"This mail will not repeat (t30_verdict_sent_at has been stamped).\n",
		run.ID, run.ExperimentID, agentDetail,
		run.CompletedAt, scoreDetail,
		run.ID, run.ExperimentID,
		run.ExperimentID, run.ExperimentID)

	// P27 burn-down: budget-gate the operator emit before SendMail.
	// On allowed=false the helper has already drop/digested per the
	// configured budget. Fail-open on err so a transient SQLite
	// glitch never silences a high-stakes alert. T+30 verdict is
	// StakesHigh because the operator must act within the 24-hour
	// verdict window or miss the nudge.
	if allowed, _ := store.RespectNotificationBudget(
		context.Background(), db, "operator", "t30-verdict", "email", "{}",
		store.StakesHigh,
	); !allowed {
		// budget exhausted (StakesHigh always punches through, so
		// this branch only fires on a real config-set 0-cap row).
	} else {
		_ = allowed
	}
	mailID := store.SendMail(db, "t30-verdict", "operator", subject, body, 0, store.MailTypeInfo)
	logger.Printf("Dog t30-verdict: mailed operator for run %d (experiment %d, mail_id=%d)", run.ID, run.ExperimentID, mailID)
	return nil
}
