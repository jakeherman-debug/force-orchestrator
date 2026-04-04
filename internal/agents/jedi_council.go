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

const MaxDiffBytes = 80_000

// BranchAgentName extracts the agent name from a persistent branch name like "agent/R2-D2/task-42".
// Returns empty string for legacy "agent/task-N" format.
func BranchAgentName(branchName string) string {
	parts := strings.SplitN(branchName, "/", 3)
	if len(parts) == 3 && parts[0] == "agent" {
		return parts[1]
	}
	return ""
}

func SpawnJediCouncil(db *sql.DB, name string) {
	agentName := name
	logger := NewLogger(name)
	logger.Printf("%s starting up", name)

	for {
		// Hard stop — operator activated e-stop
		if IsEstopped(db) {
			time.Sleep(5 * time.Second)
			continue
		}

		b, claimed := store.ClaimForReview(db, agentName)
		if !claimed {
			time.Sleep(time.Duration(2500+rand.Intn(1000)) * time.Millisecond)
			continue
		}

		runCouncilTask(db, agentName, b, logger)
	}
}

// runCouncilTask reviews a single task's diff and either approves (merge + complete)
// or rejects it (retry or permanent failure).
func runCouncilTask(db *sql.DB, agentName string, b *store.Bounty, logger *log.Logger) {
	sessionID := telemetry.NewSessionID()

	directive := LoadDirective("jedi-council", b.TargetRepo)
	directiveSection := ""
	if directive != "" {
		directiveSection = fmt.Sprintf("\n\nOPERATOR DIRECTIVE:\n%s", directive)
	}
	systemPrompt := `You are a member of the Jedi Council, reviewing code submitted by Astromech droids in the Galactic Fleet.
Your wisdom determines whether the work passes into the main branch of the Force — or is returned for correction.
Evaluate whether the git diff correctly and completely accomplishes the stated task.
Be pragmatic: approve work that fulfills the task even if it's not perfect.
Only reject if the task is clearly incomplete, broken, or does not match the requirements.

RESEARCH TOOLS:
You have read-only access to Jira, Confluence, Glean, and SonarQube. Use them when needed:
- If the task references a Jira ticket, look it up to read acceptance criteria before ruling.
- Use SonarQube to check for security hotspots or quality gate failures introduced by the diff.
- Search Confluence or Glean for specs or contracts the implementation should conform to.
- Only look up what is directly relevant to the ruling — do not over-research.
You may NOT create or edit any Jira tickets, Confluence pages, or SonarQube issues.

Respond in raw JSON ONLY — no markdown, no explanation outside the JSON:
{"approved": true/false, "feedback": "concise reason for rejection, or empty string if approved"}` + directiveSection

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

	worktreeDir := igit.ResolveWorktreeDir(db, branchName, repoPath, b.ID, BranchAgentName)

	diff := igit.GetDiff(repoPath, branchName)
	if diff == "" {
		msg := "Git Err: diff is empty — worktree may have no commits or branch does not exist"
		store.FailBounty(db, b.ID, msg)
		telemetry.EmitEvent(telemetry.EventTaskFailed(sessionID, agentName, b.ID, msg))
		return
	}

	diffNote := ""
	if len(diff) > MaxDiffBytes {
		logger.Printf("Task %d: diff is %d bytes — truncating to %d for council review", b.ID, len(diff), MaxDiffBytes)
		cutAt := MaxDiffBytes
		if idx := strings.LastIndex(diff[:MaxDiffBytes], "\n"); idx > 0 {
			cutAt = idx
		}
		diff = diff[:cutAt]
		diffNote = fmt.Sprintf("\n\n[NOTE: Diff was truncated at %d bytes — the full change is larger than this. "+
			"If you cannot make a confident ruling from the visible portion, reject with feedback requesting the task be broken into smaller pieces.]", MaxDiffBytes)
	}

	inboxContext := buildInboxContext(db, agentName, "jedi-council", b.ID, logger)
	reviewPrompt := fmt.Sprintf("Task: %s\n\nDiff:\n%s%s%s", b.Payload, diff, diffNote, inboxContext)

	response, err := claude.AskClaudeCLI(systemPrompt, reviewPrompt, claude.CouncilTools, 5)
	if err != nil {
		msg := fmt.Sprintf("Claude CLI Err: %v", err)
		logger.Printf("Task %d: council infra failure — %s", b.ID, msg)
		handleInfraFailure(db, agentName, "council", b, sessionID, msg, "AwaitingCouncilReview", true, logger)
		return
	}

	cleanJSON := claude.ExtractJSON(response)

	var ruling store.CouncilRuling
	if err := json.Unmarshal([]byte(cleanJSON), &ruling); err != nil {
		msg := fmt.Sprintf("JSON Parse Err: %v | Output: %.500s", err, cleanJSON)
		logger.Printf("Task %d: council JSON parse failed — returning to queue for retry: %s", b.ID, msg)
		handleInfraFailure(db, agentName, "council", b, sessionID, msg, "AwaitingCouncilReview", true, logger)
		return
	}

	telemetry.EmitEvent(telemetry.EventCouncilRuling(sessionID, agentName, b.ID, ruling.Approved, ruling.Feedback))

	tokIn, tokOut := claude.ParseTokenUsage(response)

	if ruling.Approved {
		logger.Printf("Task %d: APPROVED — merging branch %s", b.ID, branchName)
		if mergeErr := igit.MergeAndCleanup(repoPath, branchName, worktreeDir); mergeErr != nil {
			msg := fmt.Sprintf("Merge Err: %v", mergeErr)
			logger.Printf("Task %d: merge failed: %v", b.ID, mergeErr)
			errStr := mergeErr.Error()
			if strings.Contains(errStr, "conflict") || strings.Contains(errStr, "CONFLICT") {
				// True merge conflict — spawn a conflict resolution CodeEdit task so an Astromech
				// can resolve the markers automatically. Mark the original task as Failed so the
				// resolution task's parent-complete mail logic fires on success.
				logger.Printf("Task %d: true merge conflict on branch %s — spawning conflict resolution task", b.ID, branchName)
				conflictPayload := fmt.Sprintf("[CONFLICT_BRANCH: %s]\n\n%s", branchName, b.Payload)
				resTaskID := store.AddBounty(db, b.ID, "CodeEdit", conflictPayload)
				logger.Printf("Task %d: spawned conflict resolution task #%d", b.ID, resTaskID)
				failMsg := fmt.Sprintf("Merge conflict on branch %s — conflict resolution task #%d spawned", branchName, resTaskID)
				store.FailBounty(db, b.ID, failMsg)
				histID := store.RecordTaskHistory(db, b.ID, agentName, sessionID, response, "Failed")
				if tokIn > 0 || tokOut > 0 {
					store.UpdateTaskHistoryTokens(db, histID, tokIn, tokOut)
				}
				store.SendMail(db, agentName, "operator",
					fmt.Sprintf("[CONFLICT] Task #%d — conflict resolution task #%d spawned", b.ID, resTaskID),
					fmt.Sprintf("Task #%d was approved but had a merge conflict on branch %s.\n\nConflict resolution task #%d has been spawned. An Astromech will resolve the conflict markers and resubmit for council review.\n\nYou will be notified when the resolution is complete.\n\nOriginal task: %s",
						b.ID, branchName, resTaskID, util.TruncateStr(b.Payload, 400)),
					b.ID, store.MailTypeAlert)
				telemetry.EmitEvent(telemetry.EventTaskFailed(sessionID, agentName, b.ID, failMsg))
			} else {
				handleInfraFailure(db, agentName, "council", b, sessionID, msg, "AwaitingCouncilReview", true, logger)
			}
			return
		}
		store.UpdateBountyStatus(db, b.ID, "Completed")
		histID := store.RecordTaskHistory(db, b.ID, agentName, sessionID, response, "Completed")
		if tokIn > 0 || tokOut > 0 {
			store.UpdateTaskHistoryTokens(db, histID, tokIn, tokOut)
		}
		store.LogAudit(db, agentName, "council-approve", b.ID, ruling.Feedback)

		// Remove dependency edges that pointed to this task so dependents become claimable.
		if n := store.UnblockDependentsOf(db, b.ID); n > 0 {
			logger.Printf("Task %d: unblocked %d dependent(s)", b.ID, n)
		}

		changedFiles := igit.ExtractDiffFiles(diff)
		filesStr := strings.Join(changedFiles, ", ")
		// Strip the [AUDIT FINDING ...] header line from audit-fix tasks so the memory
		// summary starts with the actionable content (Title/Description) rather than metadata.
		memPayload := b.Payload
		if strings.HasPrefix(memPayload, "[AUDIT FINDING ") {
			if nl := strings.Index(memPayload, "\n"); nl != -1 {
				memPayload = strings.TrimLeft(memPayload[nl:], "\n")
			}
		}
		memorySummary := fmt.Sprintf("Task: %s", util.TruncateStr(directiveText(memPayload), 400))
		if ruling.Feedback != "" {
			memorySummary += fmt.Sprintf("\nCouncil note: %s", ruling.Feedback)
		}
		store.StoreFleetMemory(db, b.TargetRepo, b.ID, "success", memorySummary, filesStr)
		logger.Printf("Task %d: COMPLETED and merged", b.ID)

		if b.ParentID > 0 {
			var parentStatus string
			db.QueryRow(`SELECT status FROM BountyBoard WHERE id = ?`, b.ParentID).Scan(&parentStatus)
			if parentStatus == "Failed" {
				store.SendMail(db, agentName, "operator",
					fmt.Sprintf("[REMEDIATION COMPLETE] Task #%d fixed — run: force reset %d", b.ID, b.ParentID),
					fmt.Sprintf("Remediation task #%d has been approved and merged.\n\nThe infra issue blocking task #%d (%s) should now be resolved.\n\nRun the following to retry the original task:\n  force reset %d\n\nCouncil feedback: %s",
						b.ID, b.ParentID, b.TargetRepo, b.ParentID, ruling.Feedback),
					b.ID, store.MailTypeRemediation)
			}
		}
	} else {
		histID := store.RecordTaskHistory(db, b.ID, agentName, sessionID, response, "Rejected")
		if tokIn > 0 || tokOut > 0 {
			store.UpdateTaskHistoryTokens(db, histID, tokIn, tokOut)
		}
		store.LogAudit(db, agentName, "council-reject", b.ID, ruling.Feedback)
		retryCount := store.IncrementRetryCount(db, b.ID)
		logger.Printf("Task %d: REJECTED (attempt %d/%d): %s", b.ID, retryCount, MaxRetries, ruling.Feedback)

		store.StoreFleetMemory(db, b.TargetRepo, b.ID, "failure",
			fmt.Sprintf("Task: %s\nRejected (attempt %d/%d): %s",
				util.TruncateStr(directiveText(b.Payload), 300), retryCount, MaxRetries, ruling.Feedback),
			"")

		if retryCount >= MaxRetries {
			msg := fmt.Sprintf("Max retries (%d) exceeded. Final rejection: %s", MaxRetries, ruling.Feedback)
			store.FailBounty(db, b.ID, msg)
			telemetry.EmitEvent(telemetry.EventTaskFailed(sessionID, agentName, b.ID, msg))

			if b.ParentID > 0 {
				var parentStatus string
				db.QueryRow(`SELECT status FROM BountyBoard WHERE id = ?`, b.ParentID).Scan(&parentStatus)
				if parentStatus == "Failed" {
					store.SendMail(db, agentName, "operator",
						fmt.Sprintf("[REMEDIATION FAILED] Task #%d could not fix task #%d — manual intervention needed", b.ID, b.ParentID),
						fmt.Sprintf("Remediation task #%d has permanently failed after %d attempts.\n\nThe infra issue blocking original task #%d (%s) requires manual intervention.\n\nFinal rejection reason: %s\n\nTask payload:\n%s",
							b.ID, MaxRetries, b.ParentID, b.TargetRepo, ruling.Feedback, util.TruncateStr(b.Payload, 500)),
						b.ID, store.MailTypeAlert)
					return
				}
			}
			store.SendMail(db, agentName, "operator",
				fmt.Sprintf("Task #%d permanently failed — %s", b.ID, b.TargetRepo),
				fmt.Sprintf("Task #%d has been rejected %d times and is now permanently failed.\n\nRepo: %s\nFinal rejection: %s\n\nTask payload:\n%s",
					b.ID, MaxRetries, b.TargetRepo, ruling.Feedback, util.TruncateStr(b.Payload, 500)),
				b.ID, store.MailTypeAlert)
			return
		}
		newPayload := fmt.Sprintf("%s\n\nFEEDBACK (attempt %d/%d): %s", b.Payload, retryCount, MaxRetries, ruling.Feedback)
		store.ReturnTaskForRework(db, b.ID, newPayload)
		store.SendMail(db, agentName, "astromech",
			fmt.Sprintf("[REJECTED] Task #%d — attempt %d/%d", b.ID, retryCount, MaxRetries),
			fmt.Sprintf("The Jedi Council reviewed your work on task #%d and rejected it.\n\nReason: %s\n\nPlease address this feedback in your next attempt.",
				b.ID, ruling.Feedback),
			b.ID, store.MailTypeFeedback)
	}
}
