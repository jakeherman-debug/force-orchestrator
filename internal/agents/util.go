package agents

import (
	"database/sql"
	"fmt"
	"time"

	"force-orchestrator/internal/store"
	"force-orchestrator/internal/telemetry"
)

func truncateStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// handleInfraFailure handles a transient or permanent CLI/infra error for an agent stage.
// It increments the infra failure counter, applies backoff on retry, and permanently fails
// the task (with operator mail) once MaxInfraFailures is reached.
//
//   - retryStatus is the status to restore when retrying (e.g. "Pending", "AwaitingCouncilReview").
//   - stageName labels the stage in log and mail subjects (e.g. "commander", "council").
//   - recordHistory: if true, records a "Failed" TaskHistory entry on permanent failure.
//
// Caller must return immediately after calling this function.
func handleInfraFailure(
	db *sql.DB,
	agentName, stageName string,
	b *store.Bounty,
	sessionID, msg, retryStatus string,
	recordHistory bool,
	logger interface{ Printf(string, ...any) },
) {
	count := store.IncrementInfraFailures(db, b.ID)
	telemetry.EmitEvent(telemetry.EventInfraFailure(sessionID, agentName, b.ID, count, msg))

	if count >= MaxInfraFailures {
		if recordHistory {
			store.RecordTaskHistory(db, b.ID, agentName, sessionID, "", "Failed")
		}
		store.FailBounty(db, b.ID, msg)
		store.SendMail(db, agentName, "operator",
			fmt.Sprintf("[INFRA FAIL] Task #%d %s — %s", b.ID, stageName, b.TargetRepo),
			fmt.Sprintf("Task #%d permanently failed in %s after %d infra errors.\n\nError: %s\n\nRun: force reset %d",
				b.ID, stageName, MaxInfraFailures, msg, b.ID),
			b.ID, store.MailTypeAlert)
	} else {
		store.UpdateBountyStatus(db, b.ID, retryStatus)
		backoff := InfraBackoff(count)
		logger.Printf("Task %d: %s infra failure %d/%d, backing off %v", b.ID, stageName, count, MaxInfraFailures, backoff)
		time.Sleep(backoff)
	}
}
