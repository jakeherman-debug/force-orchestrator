package agents

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"force-orchestrator/internal/store"
)

// dogQuarantinedRepoWatch surfaces operator mail when tasks are pending
// against quarantined repos. The astromech claim filter
// (store.ClaimBountyForWrite) silently skips these rows; this dog is the
// loud half of the contract — the operator gets one [QUARANTINED REPO]
// mail per repo per day until either the queue drains or the operator
// restores / promotes the repo.
//
// D2 T1-4. Daily cadence (24h) keeps the inbox quiet; the per-repo
// dedupe is the trailing-day check on Fleet_Mail.subject so two ticks
// inside a single 24h window cannot double-mail.
//
// The dog runs after the spend-burn-watch + every other dog so a
// quarantine-induced backlog never blocks more critical signals.
func dogQuarantinedRepoWatch(db *sql.DB, logger interface{ Printf(string, ...any) }) error {
	// Find every quarantined repo with at least one Pending task that the
	// astromech would have claimed but for the mode filter. CodeEdit is
	// the canonical astromech work; if other agent types have queued
	// tasks against the repo, those are infrastructure tasks that route
	// through their own claim queries (Pilot, Diplomat, etc.) and are
	// out of scope for this signal.
	rows, err := db.Query(`
		SELECT r.name, COUNT(b.id) AS pending_count
		FROM Repositories r
		LEFT JOIN BountyBoard b
		  ON b.target_repo = r.name
		 AND b.type = 'CodeEdit'
		 AND b.status = 'Pending'
		WHERE r.mode = 'quarantined'
		GROUP BY r.name`)
	if err != nil {
		return fmt.Errorf("dogQuarantinedRepoWatch: query: %w", err)
	}
	defer rows.Close()

	type quarantinedRepo struct {
		name    string
		pending int
	}
	var quarantined []quarantinedRepo
	for rows.Next() {
		var qr quarantinedRepo
		if err := rows.Scan(&qr.name, &qr.pending); err != nil {
			logger.Printf("quarantined-repo-watch: scan failed: %v", err)
			continue
		}
		quarantined = append(quarantined, qr)
	}
	if rErr := rows.Err(); rErr != nil {
		logger.Printf("quarantined-repo-watch: rows iter error: %v", rErr)
	}

	for _, qr := range quarantined {
		// Per-repo dedupe: skip when a [QUARANTINED REPO] mail with this
		// repo name in the subject was sent in the last 24h. The dog's
		// own cooldown is also 24h so under normal scheduling this
		// suppresses re-mailing across daemon restarts.
		subject := fmt.Sprintf("[QUARANTINED REPO] %s — %d pending CodeEdit task(s) blocked",
			qr.name, qr.pending)
		var recent int
		_ = db.QueryRow(
			`SELECT COUNT(*) FROM Fleet_Mail
			 WHERE to_agent = 'operator' AND subject = ?
			   AND created_at > datetime('now', '-1 day')`,
			subject).Scan(&recent)
		if recent > 0 {
			continue
		}

		// Pull the quarantine reason for the mail body.
		repo := store.GetRepo(db, qr.name)
		var reason string
		if repo != nil {
			reason = strings.TrimSpace(repo.QuarantineReason)
		}
		body := fmt.Sprintf(
			"Repository %q is in mode=quarantined. The astromech claim filter is "+
				"skipping %d Pending CodeEdit task(s) targeting this repo; the work "+
				"will not progress until the repo is restored.\n\n"+
				"Quarantine reason: %s\n\n"+
				"To unblock: investigate the underlying issue, then either restore "+
				"the repo (mode → read_only) or promote it back to write via the "+
				"dashboard's per-repo controls.",
			qr.name, qr.pending, reasonOrUnset(reason))
		// SendMail returns the new row id (int64) — fleet-wide signature has
		// no error channel; a zero id surfaces nothing actionable here, the
		// dog's next tick will simply re-mail.
		// P27 burn-down: budget-gate the operator emit before SendMail.
		if allowed, _ := store.RespectNotificationBudget(
			context.Background(), db, "operator", "inquisitor", "email", "{}",
			store.StakesHigh,
		); !allowed {
			// budget exhausted (StakesHigh always punches through).
		} else {
			_ = allowed
		}
		_ = store.SendMail(db, "inquisitor", "operator", subject, body, 0, store.MailTypeAlert)
		logger.Printf("quarantined-repo-watch: mailed operator about repo %q (%d pending)",
			qr.name, qr.pending)
	}
	return nil
}

func reasonOrUnset(s string) string {
	if s == "" {
		return "(not set — quarantine flag was raised without a reason)"
	}
	return s
}
