package agents

import (
	"database/sql"
	"fmt"
	"strings"
	"time"

	"force-orchestrator/internal/store"
	"force-orchestrator/internal/telemetry"
	"force-orchestrator/internal/util"
)

// extractClaudeErrorExcerpt pulls a short, diagnostic-looking excerpt out of
// Claude's stdout when the CLI exits non-zero. We're NOT trying to summarize —
// we just want a bounded hint that's likely to be real error output rather
// than mid-thought narration. Heuristics:
//
//   - look for lines matching rate-limit / auth / permission patterns first,
//   - otherwise last non-empty line (often the exit-time error),
//   - clamp to 180 chars.
//
// Returns "" when nothing useful is found (caller just uses err.Error()).
func extractClaudeErrorExcerpt(stdout string) string {
	stdout = strings.TrimSpace(stdout)
	if stdout == "" {
		return ""
	}
	lines := strings.Split(stdout, "\n")

	// Priority 1: known error patterns.
	needles := []string{
		"rate limit", "rate_limit", "RateLimit",
		"authentication", "unauthorized", "401",
		"permission denied", "403",
		"error:", "Error:", "ERROR:",
		"failed:", "FAILED",
		"panic:",
	}
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if line == "" {
			continue
		}
		for _, n := range needles {
			if strings.Contains(line, n) {
				return util.TruncateStr(line, 180)
			}
		}
	}

	// Priority 2: the last non-empty line. Skip if it clearly looks like
	// markdown/code fragments (headings, bullets, fenced code).
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "#") || strings.HasPrefix(line, "- ") ||
			strings.HasPrefix(line, "```") || strings.HasPrefix(line, "* ") {
			continue
		}
		return util.TruncateStr(line, 180)
	}
	return ""
}

// queueReshardDecompose creates a Decompose bounty that sends a permanently-
// failed task back to Commander for re-planning into smaller shards.
// Idempotent per failed task: if a Decompose for the same parent task already
// exists (any status except Cancelled), this is a no-op. Returns the existing
// or new bounty ID, or 0 on insert failure.
//
// The payload is shaped as a Commander-style request: it tells Commander that
// the original task was too large and names the target repo explicitly so the
// re-plan stays in the same repo.
func queueReshardDecompose(db *sql.DB, b *store.Bounty, stageName, errMsg string) int {
	// Dedup: one outstanding reshard per failed task.
	var existing int
	db.QueryRow(`SELECT id FROM BountyBoard
		WHERE type = 'Decompose' AND parent_id = ? AND status != 'Cancelled'
		LIMIT 1`, b.ID).Scan(&existing)
	if existing > 0 {
		return existing
	}

	// Shape the payload as an operator-style feature request so Commander's
	// standard decomposition prompt handles it naturally. Repo name is named
	// explicitly because Commander keys on it.
	payload := fmt.Sprintf(
		"[INFRA_FAILURE_RESHARD from task #%d]\n\n"+
			"The following task repeatedly crashed the agent (%d infra failures in %s). "+
			"Most likely cause: the scope is too large to complete in a single agent session.\n\n"+
			"Re-plan this work as 2-5 smaller, focused sub-tasks that each fit comfortably in a single session. "+
			"Target repo: %s. Preserve the original goal.\n\n"+
			"Last error:\n%s\n\n"+
			"Original task payload:\n%s",
		b.ID, MaxInfraFailures, stageName, b.TargetRepo, util.TruncateStr(errMsg, 400),
		util.TruncateStr(b.Payload, 2000))

	res, err := db.Exec(
		`INSERT INTO BountyBoard (parent_id, target_repo, type, status, payload, convoy_id, priority, created_at)
		 VALUES (?, ?, 'Decompose', 'Pending', ?, ?, ?, datetime('now'))`,
		b.ID, b.TargetRepo, payload, b.ConvoyID, b.Priority)
	if err != nil {
		return 0
	}
	id, _ := res.LastInsertId()
	return int(id)
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
		telemetry.EmitEvent(telemetry.EventTaskFailed(sessionID, agentName, b.ID, msg))
		store.LogAudit(db, agentName, "infra-fail", b.ID, msg)
		store.StoreFleetMemory(db, b.TargetRepo, b.ID, "failure",
			fmt.Sprintf("Task: %s\nInfra failure in %s (permanent): %s",
				util.TruncateStr(directiveText(b.Payload), 300), stageName, msg), "", "")

		// Bubble to Commander for re-decomposition. Repeated infra failures on
		// the same task usually mean the scope is too large for a single agent
		// session (claude CLI crashes mid-execution on big multi-file tasks).
		// Commander re-plans the work into smaller shards that each fit.
		// If it turns out to be a true infrastructure outage, Commander will
		// produce a similar plan and the next agent cycle will either succeed
		// (transient issue resolved) or hit the same cap and re-bubble.
		reshardID := queueReshardDecompose(db, b, stageName, msg)
		if reshardID > 0 {
			logger.Printf("Task %d: permanently failed in %s — queued Decompose #%d for Commander reshard", b.ID, stageName, reshardID)
		} else {
			logger.Printf("Task %d: permanently failed in %s — Decompose reshard skipped (insert failed)", b.ID, stageName)
		}

		store.SendMail(db, agentName, "operator",
			fmt.Sprintf("[INFRA FAIL] Task #%d %s — %s", b.ID, stageName, b.TargetRepo),
			fmt.Sprintf("Task #%d permanently failed in %s after %d infra errors.\n\nError: %s\n\nDecompose task #%d has been queued — Commander will re-plan this work into smaller shards.",
				b.ID, stageName, MaxInfraFailures, msg, reshardID),
			b.ID, store.MailTypeInfo)
	} else {
		store.UpdateBountyStatus(db, b.ID, retryStatus)
		backoff := InfraBackoff(count)
		logger.Printf("Task %d: %s infra failure %d/%d, backing off %v", b.ID, stageName, count, MaxInfraFailures, backoff)
		time.Sleep(backoff)
	}
}
