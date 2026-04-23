package agents

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"math/rand"
	"strings"
	"time"

	igit "force-orchestrator/internal/git"
	"force-orchestrator/internal/claude"
	"force-orchestrator/internal/store"
	"force-orchestrator/internal/telemetry"
	"force-orchestrator/internal/util"
)

const captainSystemPrompt = `You are a Fleet Captain in the Galactic Fleet — a senior engineering lead responsible for ensuring that a multi-task feature convoy stays coherent as it executes.

Your role is NOT to review code quality. That is the Jedi Council's job.
Your role IS to ensure that each completed task still fits the larger plan, and to update the remaining plan if the implementation has diverged from what was originally designed.

You will receive:
1. The full convoy state: completed tasks, pending tasks, and the current task being reviewed
2. The current task's git diff — what was actually built

YOUR JOB:
1. Review the diff in the context of the full convoy — does what was built align with what the downstream tasks expect?
2. Update downstream task payloads if the implementation revealed a better or different approach than Commander anticipated
3. Add new tasks ONLY for genuinely missing work that Commander didn't anticipate — not to remediate problems the current task itself introduced
4. Approve to forward this work to the Jedi Council for code quality review
5. Reject if the implementation introduces regressions, breaks previously fixed code, or is so far off-plan that downstream tasks cannot proceed
6. Escalate only if a fundamental problem with the entire convoy plan requires human judgment

SCOPE ENFORCEMENT — this is your highest priority check:
Examine every file changed in the diff against the task's stated scope. A task that says "add README docs" should only touch documentation files. A task that says "implement X feature" should only touch files directly required by X.

If the diff contains commits or file changes OUTSIDE the task's stated scope:
1. Identify whether the out-of-scope work has value (e.g., a real bug fix, a genuine improvement)
2. If valuable: add it as a new standalone CodeEdit task in new_tasks (with a clear description of what was done and why it's valuable)
3. Always REJECT the current task with feedback that:
   a. Names the specific out-of-scope files/commits
   b. Instructs the agent to revert those changes and resubmit only the scoped work
   c. Confirms a new task has been queued for the out-of-scope work (if applicable)

Scope discipline is non-negotiable. Even correct, valuable out-of-scope work must be extracted into its own task. The agent should never bundle unrelated changes into a single task — it makes the convoy harder to reason about and review.

APPROVAL GUIDELINES:
- Approve minor deviations, unexpected file choices, or stylistic differences
- Approve if the approach differs from the plan but achieves the correct outcome
- If in doubt about style or approach, approve with a note in feedback

REJECTION GUIDELINES — reject (do NOT create new tasks to cover for) if:
- The diff contains changes to files outside the task's stated scope (see SCOPE ENFORCEMENT above)
- The diff reverts or undoes a previously merged fix (check git history context if provided)
- The diff introduces a security regression (e.g., replacing an atomic operation with a TOCTOU-prone pattern)
- The diff is missing the core requirement of the task entirely
- The diff breaks downstream task assumptions in a way that cannot be fixed by updating their payloads
Using new_tasks to paper over a broken implementation wastes the entire council review cycle. Reject instead and let the agent try again with the correct approach.

SCOPE GUARD (for scope-violation rejections):
When you reject for out-of-scope file changes, populate "rejected_files" with the EXACT list of file paths that were outside the task's scope. The fleet will programmatically prepend a [SCOPE GUARD — DO NOT MODIFY] section to the agent's next attempt listing exactly those paths. Getting this list right is the single highest-leverage thing you do — it prevents the same scope-creep loop from repeating on retry.
- Use the file paths from the diff headers verbatim.
- Include every file you reject, even if it's only one.
- Leave rejected_files empty [] when the rejection is not about scope (e.g. wrong approach, broken logic).

Respond in raw JSON ONLY — no markdown, no explanation outside the JSON:
{
  "decision": "approve" | "reject" | "escalate",
  "feedback": "reason if rejecting or escalating, empty string if approving",
  "task_updates": [{"id": <task_id>, "new_payload": "<updated full description>"}],
  "new_tasks": [{"repo": "<repo_name>", "task": "<description>", "blocked_by": [<task_id>, ...]}],
  "rejected_files": ["path/one.go", "path/two.go"]
}`

// scopeGuardMarker delimits the auto-prepended "DO NOT MODIFY" block so
// subsequent rejections can find and replace it instead of accumulating.
const scopeGuardMarker = "[SCOPE GUARD — DO NOT MODIFY]"

// stripScopeGuard removes any previously-prepended SCOPE GUARD block from a
// payload. The guard (if present) is always the first block before the
// original task body; we peel it off and return everything after.
func stripScopeGuard(payload string) string {
	if !strings.HasPrefix(payload, scopeGuardMarker) {
		return payload
	}
	// The guard block ends at the terminating "---\n" separator.
	sep := "\n---\n"
	if idx := strings.Index(payload, sep); idx >= 0 {
		return payload[idx+len(sep):]
	}
	return payload
}

// buildScopeGuardedPayload composes the next-attempt payload after a Captain
// rejection. If ruling.RejectedFiles is non-empty, a [SCOPE GUARD — DO NOT
// MODIFY] block is prepended listing exactly those paths; this converts a
// Captain rejection into programmatic guardrails the next agent sees up-front
// instead of relying on them to parse free-form feedback prose.
//
// Idempotent: strips any prior guard block before appending, so repeated
// rejections produce a single (latest) guard rather than accumulating.
func buildScopeGuardedPayload(rawPayload string, ruling store.CaptainRuling, attempt int) string {
	body := stripScopeGuard(rawPayload)
	feedbackBlock := fmt.Sprintf("%s\n\nCAPTAIN FEEDBACK (attempt %d/%d): %s",
		body, attempt, MaxRetries, ruling.Feedback)

	if len(ruling.RejectedFiles) == 0 {
		return feedbackBlock
	}

	var guard strings.Builder
	guard.WriteString(scopeGuardMarker)
	guard.WriteString("\nThe Captain rejected a previous attempt because these files were changed outside the task's stated scope. Do NOT modify them in this attempt, even if they look related or you see an obvious improvement:\n\n")
	for _, f := range ruling.RejectedFiles {
		trimmed := strings.TrimSpace(f)
		if trimmed == "" {
			continue
		}
		guard.WriteString("  - ")
		guard.WriteString(trimmed)
		guard.WriteString("\n")
	}
	guard.WriteString("\nIf you believe any of these files NEED to change for this task, stop and escalate via mail instead of editing them. Out-of-scope work is queued as its own task by the Captain.\n---\n")
	return guard.String() + feedbackBlock
}

// buildConvoyContext assembles a full convoy state summary for the Captain prompt.
func buildConvoyContext(db *sql.DB, b *store.Bounty) string {
	if b.ConvoyID == 0 {
		return ""
	}

	var convoyName string
	db.QueryRow(`SELECT name FROM Convoys WHERE id = ?`, b.ConvoyID).Scan(&convoyName)
	completed, total := store.ConvoyProgress(db, b.ConvoyID)

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("# CONVOY CONTEXT\nConvoy #%d: %s\nProgress: %d/%d tasks complete\n\n",
		b.ConvoyID, convoyName, completed, total))

	rows, err := db.Query(`
		SELECT id, target_repo, payload
		FROM BountyBoard
		WHERE convoy_id = ? AND status = 'Completed' AND type = 'CodeEdit'
		ORDER BY id DESC LIMIT 5`, b.ConvoyID)
	if err == nil {
		var lines []string
		for rows.Next() {
			var id int
			var repo, payload string
			rows.Scan(&id, &repo, &payload)
			firstLine := payload
			if nl := strings.Index(payload, "\n"); nl != -1 {
				firstLine = payload[:nl]
			}
			lines = append(lines, fmt.Sprintf("  #%d [%s] %s", id, repo, util.TruncateStr(firstLine, 100)))
		}
		rows.Close()
		if len(lines) > 0 {
			sb.WriteString("## Completed tasks (already merged into main):\n")
			sb.WriteString(strings.Join(lines, "\n"))
			sb.WriteString("\n\n")
		}
	}

	rows, err = db.Query(`
		SELECT id, target_repo, payload,
		    COALESCE((SELECT MIN(td.depends_on) FROM TaskDependencies td
		              JOIN BountyBoard dep ON dep.id = td.depends_on
		              WHERE td.task_id = bb.id AND dep.status != 'Completed'), 0) AS active_dep
		FROM BountyBoard bb
		WHERE convoy_id = ? AND status IN ('Pending', 'Planned') AND type = 'CodeEdit' AND id != ?
		ORDER BY id ASC`, b.ConvoyID, b.ID)
	if err == nil {
		var lines []string
		for rows.Next() {
			var id, activeDep int
			var repo, payload string
			rows.Scan(&id, &repo, &payload, &activeDep)
			firstLine := payload
			if nl := strings.Index(payload, "\n"); nl != -1 {
				firstLine = payload[:nl]
			}
			line := fmt.Sprintf("  #%d [%s] %s", id, repo, util.TruncateStr(firstLine, 100))
			if activeDep > 0 {
				line += fmt.Sprintf(" (blocked by #%d)", activeDep)
			}
			lines = append(lines, line)
		}
		rows.Close()
		if len(lines) > 0 {
			sb.WriteString("## Remaining tasks (not yet started — update these if needed):\n")
			sb.WriteString(strings.Join(lines, "\n"))
			sb.WriteString("\n\n")
		}
	}

	sb.WriteString(fmt.Sprintf("## Current task under review: #%d [%s]\n%s\n\n",
		b.ID, b.TargetRepo, util.TruncateStr(b.Payload, 500)))

	return sb.String()
}

// isKnownRepo reports whether a repo name is registered in the fleet.
func isKnownRepo(db *sql.DB, repoName string) bool {
	var count int
	db.QueryRow(`SELECT COUNT(*) FROM Repositories WHERE name = ?`, repoName).Scan(&count)
	return count > 0
}

func SpawnCaptain(db *sql.DB, name string) {
	agentName := name
	logger := NewLogger(name)
	logger.Printf("Captain %s standing by", name)

	for {
		if IsEstopped(db) {
			time.Sleep(5 * time.Second)
			continue
		}

		b, claimed := store.ClaimForCaptainReview(db, agentName)
		if !claimed {
			time.Sleep(time.Duration(2500+rand.Intn(1000)) * time.Millisecond)
			continue
		}

		runCaptainTask(db, agentName, b, logger)
	}
}

// runCaptainTask reviews a single task's diff for convoy plan coherence and routes
// it to council (approve), back to Pending (reject), or escalates to a human.
func runCaptainTask(db *sql.DB, agentName string, b *store.Bounty, logger *log.Logger) {
	sessionID := telemetry.NewSessionID()
	logger.Printf("[%s] Claimed task %d for captain review [convoy %d]", sessionID, b.ID, b.ConvoyID)
	telemetry.EmitEvent(telemetry.TelemetryEvent{
		SessionID: sessionID, Agent: agentName, TaskID: b.ID,
		EventType: "captain_claimed",
		Payload:   map[string]any{"convoy_id": b.ConvoyID},
	})

	// Hard-reject if the Chancellor has placed a hold on this convoy.
	if reason, held := store.GetConvoyHold(db, b.ConvoyID); held {
		retryCount := store.IncrementRetryCount(db, b.ID)
		holdMsg := fmt.Sprintf("CONVOY HOLD: %s", reason)
		newPayload := fmt.Sprintf("%s\n\nCAPTAIN FEEDBACK (attempt %d/%d): %s", stripScopeGuard(b.Payload), retryCount, MaxRetries, holdMsg)
		store.ReturnTaskForRework(db, b.ID, newPayload)
		store.SendMail(db, agentName, "astromech",
			fmt.Sprintf("[CAPTAIN REJECTED] Task #%d — convoy on hold", b.ID),
			fmt.Sprintf("Task #%d returned — its convoy is on hold by Chancellor directive.\n\nReason: %s\n\nDo not retry until the hold is lifted.", b.ID, reason),
			b.ID, store.MailTypeFeedback)
		logger.Printf("Task %d: hard-rejected — convoy #%d on Chancellor hold", b.ID, b.ConvoyID)
		return
	}

	directive := LoadDirective("captain", b.TargetRepo)
	directiveSection := ""
	if directive != "" {
		directiveSection = fmt.Sprintf("\n\nOPERATOR DIRECTIVE:\n%s", directive)
	}

	repoPath := store.GetRepoPath(db, b.TargetRepo)
	if repoPath == "" {
		msg := fmt.Sprintf("DB Err: unknown target repository '%s'", b.TargetRepo)
		store.FailBounty(db, b.ID, msg)
		telemetry.EmitEvent(telemetry.EventTaskFailed(sessionID, agentName, b.ID, msg))
		return
	}

	branchName := b.BranchName
	if branchName == "" {
		branchName = fmt.Sprintf("agent/task-%d", b.ID)
	}

	diff := igit.GetDiff(repoPath, branchName)
	if diff == "" {
		if igit.CommitsAhead(repoPath, branchName) == "" {
			// No diff and no unique commits — work was already merged into main.
			// Auto-complete rather than failing; unblock dependents and recover convoy.
			store.UpdateBountyStatus(db, b.ID, "Completed")
			store.RecordTaskHistory(db, b.ID, agentName, sessionID, "auto-completed: work already merged into main", "Completed")
			store.LogAudit(db, agentName, "captain-auto-complete", b.ID, "diff empty, commits already in main")
			store.UnblockDependentsOf(db, b.ID)
			store.AutoRecoverConvoy(db, b.ConvoyID, logger)
			logger.Printf("Task %d: captain auto-completed (work already merged into main)", b.ID)
			return
		}
		msg := "Git Err: diff is empty — branch has commits but no net changes vs main"
		store.FailBounty(db, b.ID, msg)
		telemetry.EmitEvent(telemetry.EventTaskFailed(sessionID, agentName, b.ID, msg))
		return
	}

	if len(diff) > MaxDiffBytes {
		cutAt := MaxDiffBytes
		if idx := strings.LastIndex(diff[:MaxDiffBytes], "\n"); idx > 0 {
			cutAt = idx
		}
		diff = diff[:cutAt] + fmt.Sprintf("\n\n[NOTE: Diff truncated at %d bytes — evaluate what is visible]", MaxDiffBytes)
	}

	convoyContext := buildConvoyContext(db, b)
	inboxContext := buildInboxContext(db, agentName, "captain", b.ID, logger)

	systemPrompt := captainSystemPrompt + directiveSection
	reviewPrompt := fmt.Sprintf("%s\n# CURRENT TASK DIFF\n%s%s", convoyContext, diff, inboxContext)

	response, err := claude.AskClaudeCLI(systemPrompt, reviewPrompt, claude.CouncilTools, 5)
	if err != nil {
		msg := fmt.Sprintf("Claude CLI Err: %v", err)
		logger.Printf("Task %d: captain infra failure — %s", b.ID, msg)
		handleInfraFailure(db, agentName, "captain", b, sessionID, msg, "AwaitingCaptainReview", true, logger)
		return
	}

	cleanJSON := claude.ExtractJSON(response)
	var ruling store.CaptainRuling
	if err := json.Unmarshal([]byte(cleanJSON), &ruling); err != nil {
		msg := fmt.Sprintf("JSON Parse Err: %v | Output: %.500s", err, cleanJSON)
		logger.Printf("Task %d: captain JSON parse failed — returning to queue: %s", b.ID, msg)
		handleInfraFailure(db, agentName, "captain", b, sessionID, msg, "AwaitingCaptainReview", true, logger)
		return
	}

	// Apply downstream task payload updates before routing
	for _, update := range ruling.TaskUpdates {
		if update.ID == 0 || update.NewPayload == "" {
			continue
		}
		res, _ := db.Exec(
			`UPDATE BountyBoard SET payload = ? WHERE id = ? AND convoy_id = ? AND status IN ('Pending', 'Planned')`,
			update.NewPayload, update.ID, b.ConvoyID)
		if n, _ := res.RowsAffected(); n > 0 {
			logger.Printf("Task %d: captain updated downstream task #%d", b.ID, update.ID)
			store.LogAudit(db, agentName, "captain-update-task", update.ID,
				fmt.Sprintf("payload updated by captain reviewing task #%d", b.ID))
		}
	}

	// Insert new tasks the captain identified as missing
	for _, nt := range ruling.NewTasks {
		if nt.Repo == "" || nt.Task == "" {
			continue
		}
		if !isKnownRepo(db, nt.Repo) {
			msg := fmt.Sprintf("captain plan references unknown repository '%s' — convoy cannot proceed without human review", nt.Repo)
			logger.Printf("Task %d: %s", b.ID, msg)
			CreateEscalation(db, b.ID, store.SeverityMedium, msg)
			return
		}
		newID, insertErr := store.AddConvoyTask(db, b.ParentID, nt.Repo, nt.Task, b.ConvoyID, b.Priority, "Pending")
		if insertErr != nil {
			logger.Printf("Task %d: failed to insert convoy task for repo %s: %v", b.ID, nt.Repo, insertErr)
			continue
		}
		for _, depID := range nt.BlockedBy {
			if depID <= 0 {
				continue
			}
			// Validate that the referenced task actually exists to prevent phantom dependency edges.
			var exists int
			db.QueryRow(`SELECT COUNT(*) FROM BountyBoard WHERE id = ?`, depID).Scan(&exists)
			if exists == 0 {
				logger.Printf("Task %d: captain referenced non-existent blocked_by ID %d for new task #%d — skipping dependency", b.ID, depID, newID)
				continue
			}
			store.AddDependency(db, newID, depID)
		}
		logger.Printf("Task %d: captain added new task #%d [%s]: %s", b.ID, newID, nt.Repo, util.TruncateStr(nt.Task, 60))
		store.LogAudit(db, agentName, "captain-add-task", newID,
			fmt.Sprintf("added by captain reviewing task #%d", b.ID))
	}

	var captainOutcome string
	switch ruling.Decision {
	case "approve":
		captainOutcome = "Completed"
	case "reject":
		captainOutcome = "Rejected"
	case "escalate":
		captainOutcome = "Escalated"
	default:
		captainOutcome = "Completed"
	}
	histID := store.RecordTaskHistory(db, b.ID, agentName, sessionID, response, captainOutcome)
	tokIn, tokOut := claude.ParseTokenUsage(response)
	if tokIn > 0 || tokOut > 0 {
		store.UpdateTaskHistoryTokens(db, histID, tokIn, tokOut)
	}

	switch ruling.Decision {
	case "approve":
		logger.Printf("Task %d: captain APPROVED — forwarding to council", b.ID)
		store.UpdateBountyStatus(db, b.ID, "AwaitingCouncilReview")
		telemetry.EmitEvent(telemetry.TelemetryEvent{
			SessionID: sessionID, Agent: agentName, TaskID: b.ID,
			EventType: "captain_approved",
			Payload:   map[string]any{"updates": len(ruling.TaskUpdates), "new_tasks": len(ruling.NewTasks)},
		})

	case "reject":
		retryCount := store.IncrementRetryCount(db, b.ID)
		logger.Printf("Task %d: captain REJECTED (attempt %d/%d): %s", b.ID, retryCount, MaxRetries, ruling.Feedback)
		telemetry.EmitEvent(telemetry.TelemetryEvent{
			SessionID: sessionID, Agent: agentName, TaskID: b.ID,
			EventType: "captain_rejected",
			Payload:   map[string]any{"feedback": ruling.Feedback, "attempt": retryCount},
		})

		if retryCount >= MaxRetries {
			msg := fmt.Sprintf("Captain: max retries (%d) exceeded. Final rejection: %s", MaxRetries, ruling.Feedback)
			store.FailBounty(db, b.ID, msg)

			// Send rejection history to librarian for memory synthesis.
			store.SendMail(db, agentName, "librarian",
				fmt.Sprintf("[CAPTAIN REJECTED] Task #%d — attempt %d/%d (final)", b.ID, retryCount, MaxRetries),
				fmt.Sprintf("Fleet Captain %s permanently rejected task #%d (attempt %d/%d).\n\nReason: %s",
					agentName, b.ID, retryCount, MaxRetries, ruling.Feedback),
				b.ID, store.MailTypeFeedback)

			// Queue a MedicReview — the Medic decides whether to requeue, shard, or escalate.
			medicID := store.QueueMedicReview(db, b, "captain_rejection", msg)
			logger.Printf("Task %d: permanently failed (captain) — MedicReview #%d queued", b.ID, medicID)
			return
		}

		newPayload := buildScopeGuardedPayload(b.Payload, ruling, retryCount)
		store.ReturnTaskForRework(db, b.ID, newPayload)
		store.SendMail(db, agentName, "astromech",
			fmt.Sprintf("[CAPTAIN REJECTED] Task #%d — attempt %d/%d", b.ID, retryCount, MaxRetries),
			fmt.Sprintf("Fleet Captain %s reviewed your work on task #%d and returned it for rework.\n\nReason: %s\n\nPlease address this feedback in your next attempt.",
				agentName, b.ID, ruling.Feedback),
			b.ID, store.MailTypeFeedback)
		store.SendMail(db, agentName, "librarian",
			fmt.Sprintf("[CAPTAIN REJECTED] Task #%d — attempt %d/%d", b.ID, retryCount, MaxRetries),
			fmt.Sprintf("Fleet Captain %s rejected task #%d (attempt %d/%d).\n\nReason: %s",
				agentName, b.ID, retryCount, MaxRetries, ruling.Feedback),
			b.ID, store.MailTypeFeedback)

	case "escalate":
		logger.Printf("Task %d: captain ESCALATED: %s", b.ID, ruling.Feedback)
		CreateEscalation(db, b.ID, store.SeverityMedium, ruling.Feedback)
		telemetry.EmitEvent(telemetry.EventTaskEscalated(sessionID, agentName, b.ID, store.SeverityMedium, ruling.Feedback))
		store.SendMail(db, agentName, "operator",
			fmt.Sprintf("[CAPTAIN ESCALATED] Task #%d — %s", b.ID, b.TargetRepo),
			fmt.Sprintf("Captain %s escalated task #%d during convoy plan review.\n\nConvoy: #%d\nReason: %s\n\nTask payload:\n%s",
				agentName, b.ID, b.ConvoyID, ruling.Feedback, util.TruncateStr(b.Payload, 500)),
			b.ID, store.MailTypeAlert)

	default:
		logger.Printf("Task %d: captain returned unknown decision '%s' — defaulting to approve", b.ID, ruling.Decision)
		store.UpdateBountyStatus(db, b.ID, "AwaitingCouncilReview")
	}
}
