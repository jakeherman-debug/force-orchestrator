// dogs_notification_override_cleanup.go — D11 Phase 2 sub-task C.
//
// notification-override-cleanup deletes ConvoyNotificationOverrides rows
// for convoys that have been closed for more than 7 days. The 7-day
// retention preserves the override row for post-incident debugging
// ("what was operator watching during the migration?") while preventing
// indefinite accumulation. Daily cadence is plenty — the rows are tiny
// and we tolerate a few days of staleness.
//
// Coupled change: terminal convoy transitions (Shipped / Abandoned /
// Failed) call MarkConvoyOverrideClosed which stamps convoy_closed_at;
// this dog is the eventual purge. See the onConvoyTerminalTransition
// hook callsites in internal/agents/pilot_draft_watch.go (Shipped /
// Abandoned via terminalConvoyTransitionTx) and internal/agents/convoy.go
// (Failed via CheckConvoyCompletions).
//
// Anti-cheat: cleanup is silent (no operator-mail, no Slack ping).
// It's pure bookkeeping — surfacing every override-row purge would be
// noise. The 7-day retention boundary is hard-coded; if we want it
// tunable later, add a SystemConfig key in a follow-up.

package agents

import (
	"database/sql"
	"fmt"
)

// dogNotificationOverrideCleanup deletes ConvoyNotificationOverrides
// rows whose convoy_closed_at stamp is older than 7 days. The
// retention window preserves enough history for post-incident
// debugging without letting the table accumulate indefinitely.
//
// The query treats NULL and '' identically as "still open" so rows
// stamped before vs after the column's nullability convention both
// stay alive until terminal-transition fires.
func dogNotificationOverrideCleanup(db *sql.DB, logger interface{ Printf(string, ...any) }) error {
	res, err := db.Exec(
		`DELETE FROM ConvoyNotificationOverrides
		 WHERE convoy_closed_at IS NOT NULL
		   AND convoy_closed_at != ''
		   AND convoy_closed_at < datetime('now', '-7 days')`,
	)
	if err != nil {
		return fmt.Errorf("dogNotificationOverrideCleanup: delete: %w", err)
	}
	n, _ := res.RowsAffected()
	if n > 0 {
		logger.Printf("Dog notification-override-cleanup: deleted %d stale override row(s) (closed >7d ago)", n)
	}
	return nil
}
