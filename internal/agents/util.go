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
				util.TruncateStr(directiveText(b.Payload), 300), stageName, msg), "")

		// Spawn a CodeEdit remediation task so an agent can investigate the infra issue
		// and re-queue the original task once fixed. We use AddConvoyTask (not
		// AddBounty) so target_repo, convoy_id, and priority carry forward —
		// otherwise astromechs fail to claim with "unknown target repository ''"
		// and the remediation task is permanently stuck.
		remPayload := fmt.Sprintf(
			"Infra failure on task #%d (repo: %s, stage: %s).\n\nError: %s\n\n"+
				"Investigate and fix the underlying infrastructure issue so the original task can be retried.\n"+
				"Common causes:\n"+
				"- Claude API temporarily unavailable → verify API key and network connectivity\n"+
				"- Repository path misconfigured → run 'force repos' to verify\n"+
				"- Worktree corruption → run 'force cleanup' or manually remove stale worktrees\n"+
				"- Disk full or permission denied → check disk space and file permissions\n\n"+
				"After fixing, run: force reset %d",
			b.ID, b.TargetRepo, stageName, msg, b.ID)
		remID, addErr := store.AddConvoyTask(db, b.ID, b.TargetRepo, remPayload, b.ConvoyID, b.Priority, "Pending")
		if addErr != nil {
			logger.Printf("Task %d: failed to spawn remediation task: %v", b.ID, addErr)
			remID = 0
		} else {
			logger.Printf("Task %d: permanently failed in %s — spawned remediation CodeEdit task #%d", b.ID, stageName, remID)
		}

		store.SendMail(db, agentName, "operator",
			fmt.Sprintf("[INFRA FAIL] Task #%d %s — %s", b.ID, stageName, b.TargetRepo),
			fmt.Sprintf("Task #%d permanently failed in %s after %d infra errors.\n\nError: %s\n\nRemediation task #%d has been queued to investigate.\nOnce fixed, run: force reset %d",
				b.ID, stageName, MaxInfraFailures, msg, remID, b.ID),
			b.ID, store.MailTypeAlert)
	} else {
		store.UpdateBountyStatus(db, b.ID, retryStatus)
		backoff := InfraBackoff(count)
		logger.Printf("Task %d: %s infra failure %d/%d, backing off %v", b.ID, stageName, count, MaxInfraFailures, backoff)
		time.Sleep(backoff)
	}
}
