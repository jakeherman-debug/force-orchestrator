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

// BranchAgentName extracts the agent name from a persistent branch name.
// Handles both the bare format "agent/R2-D2/task-42" and the username-prefixed
// format "<user>/agent/R2-D2/task-42". Returns "" for legacy "agent/task-N"
// or any branch that doesn't contain an "agent" segment.
//
// The agent name is always the segment immediately following "agent".
func BranchAgentName(branchName string) string {
	parts := strings.Split(branchName, "/")
	for i, seg := range parts {
		if seg == "agent" && i+1 < len(parts)-1 {
			// +1 must exist AND there must be a trailing task-N segment
			// — otherwise we'd match the legacy "agent/task-N" format
			// where there is no agent name between "agent" and "task-N".
			candidate := parts[i+1]
			if strings.HasPrefix(candidate, "task-") {
				return "" // legacy format with no agent name
			}
			return candidate
		}
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

	// Hard-reject if the Chancellor has placed a hold on this convoy.
	if reason, held := store.GetConvoyHold(db, b.ConvoyID); held {
		retryCount := store.IncrementRetryCount(db, b.ID)
		holdMsg := fmt.Sprintf("CONVOY HOLD: %s", reason)
		newPayload := fmt.Sprintf("%s\n\nFEEDBACK (attempt %d/%d): %s", b.Payload, retryCount, MaxRetries, holdMsg)
		store.ReturnTaskForRework(db, b.ID, newPayload)
		store.SendMail(db, agentName, "astromech",
			fmt.Sprintf("[REJECTED] Task #%d — convoy on Chancellor hold", b.ID),
			fmt.Sprintf("Task #%d returned — its convoy is on hold by Chancellor directive.\n\nReason: %s\n\nDo not retry until the hold is lifted.", b.ID, reason),
			b.ID, store.MailTypeFeedback)
		logger.Printf("Task %d: hard-rejected — convoy #%d on Chancellor hold", b.ID, b.ConvoyID)
		return
	}

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

	// CI circuit-breaker early-bailout: if the target repo's breaker is open,
	// this task's sub-PR creation would fail anyway. Rather than run an
	// expensive LLM review and THEN re-queue (which we do in the approval
	// branch later), skip review entirely — just put the task back to
	// AwaitingCouncilReview and release the lock. Under a 30min breaker
	// cooldown with ~3s poll cadence this saves hundreds of Claude calls.
	//
	// We only check this when the repo is PR-flow-enabled; legacy repos
	// don't interact with the breaker.
	if repoCfg := store.GetRepo(db, b.TargetRepo); repoCfg != nil && repoCfg.PRFlowEnabled {
		if IsCIBreakerOpen(db, b.TargetRepo) {
			logger.Printf("Task %d: CI breaker OPEN for %s — deferring review", b.ID, b.TargetRepo)
			if _, err := db.Exec(`UPDATE BountyBoard SET status = 'AwaitingCouncilReview', owner = '', locked_at = '' WHERE id = ?`, b.ID); err != nil {
				logger.Printf("Task %d: deferral UPDATE failed (%v); stale-lock detector will recover", b.ID, err)
			}
			return
		}
	}

	branchName := b.BranchName
	if branchName == "" {
		branchName = fmt.Sprintf("agent/task-%d", b.ID)
	}

	worktreeDir := igit.ResolveWorktreeDir(db, branchName, repoPath, b.ID, BranchAgentName)

	diff := igit.GetDiff(repoPath, branchName)
	if diff == "" {
		// Check whether the branch's commits are already in main — this happens when
		// multiple convoy tasks touch overlapping files and one agent merges first,
		// making subsequent agents' three-dot diffs empty.
		if igit.CommitsAhead(repoPath, branchName) == "" {
			// All commits already in main — auto-complete rather than fail.
			logger.Printf("Task %d: diff empty and no unique commits — work already merged, auto-completing", b.ID)
			store.UpdateBountyStatus(db, b.ID, "Completed")
			store.RecordTaskHistory(db, b.ID, agentName, sessionID, "auto-completed: work already merged into main", "Completed")
			store.LogAudit(db, agentName, "council-auto-complete", b.ID, "diff empty, commits already in main")
			if n := store.UnblockDependentsOf(db, b.ID); n > 0 {
				logger.Printf("Task %d: unblocked %d dependent(s)", b.ID, n)
			}
			store.AutoRecoverConvoy(db, b.ConvoyID, logger)
			telemetry.EmitEvent(telemetry.TelemetryEvent{
				SessionID: sessionID, Agent: agentName, TaskID: b.ID,
				EventType: "task_completed",
				Payload:   map[string]any{"reason": "already_merged"},
			})
		} else {
			msg := "Git Err: diff is empty — branch has commits but no net changes; may need manual inspection"
			store.FailBounty(db, b.ID, msg)
			telemetry.EmitEvent(telemetry.EventTaskFailed(sessionID, agentName, b.ID, msg))
		}
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
		// Split on pr_flow_enabled: if the repo is on the PR flow, open a sub-PR
		// instead of local-merging.
		// If the ask-branch isn't ready yet (Pilot hasn't finished CreateAskBranch),
		// requeue to AwaitingCouncilReview rather than falling through to main —
		// merging to main bypasses the entire human-gate invariant.
		repoCfg := store.GetRepo(db, b.TargetRepo)
		if repoCfg != nil && repoCfg.PRFlowEnabled && b.ConvoyID > 0 {
			if store.GetConvoyAskBranch(db, b.ConvoyID, b.TargetRepo) == nil {
				// Ask-branch not ready yet — requeue and wait for Pilot.
				logger.Printf("Task %d: ask-branch not yet created for convoy %d/%s — requeuing", b.ID, b.ConvoyID, b.TargetRepo)
				if _, err := db.Exec(`UPDATE BountyBoard SET status = 'AwaitingCouncilReview', owner = '', locked_at = '' WHERE id = ?`, b.ID); err != nil {
					logger.Printf("Task %d: requeue failed (%v); stale-lock detector will recover", b.ID, err)
				}
				return
			}
		}
		usePRFlow := repoCfg != nil && repoCfg.PRFlowEnabled && b.ConvoyID > 0

		if usePRFlow {
			// CI circuit-breaker gate: if the breaker is open for this repo,
			// don't pile up more sub-PRs on a CI system that's failing. Re-queue
			// the task back to AwaitingCouncilReview — the next council tick
			// will retry, and by then the breaker will have closed (30min cooldown).
			if IsCIBreakerOpen(db, b.TargetRepo) {
				logger.Printf("Task %d: CI breaker OPEN for %s — requeuing to AwaitingCouncilReview", b.ID, b.TargetRepo)
				if _, err := db.Exec(`UPDATE BountyBoard SET status = 'AwaitingCouncilReview', owner = '', locked_at = '' WHERE id = ?`, b.ID); err != nil {
					// Re-queue failed; Inquisitor's stale-lock detector will free the task
					// after 45 min, but log so the operator can correlate.
					logger.Printf("Task %d: re-queue UPDATE failed (%v); stale-lock detector will recover", b.ID, err)
				}
				return
			}
			prTitle := buildSubPRTitle(b)
			prBody := buildSubPRBody(b, ruling)
			errClass, prErr := openSubPRForApprovedTask(db, b, agentName, branchName, prTitle, prBody, logger)
			if prErr != nil {
				// Transient/rate-limit → retry via AwaitingCouncilReview (handleInfraFailure).
				// Auth-expired / BranchProtection / Permanent → escalate immediately so the
				// operator sees the gh problem and can fix auth or policy.
				if errClass.ShouldRetry() {
					handleInfraFailure(db, agentName, "council", b, sessionID, prErr.Error(), "AwaitingCouncilReview", true, logger)
					return
				}
				// "No commits between" means the agent's branch has no net diff vs the
				// ask-branch — the work is already incorporated via a parallel task.
				// Self-cancel rather than escalate; no operator action required.
				if strings.Contains(prErr.Error(), "No commits between") {
					cancelMsg := fmt.Sprintf("Cancelled: no net diff vs ask-branch — work already incorporated (%v)", prErr)
					db.Exec(`UPDATE BountyBoard SET status = 'Cancelled', owner = '', locked_at = '', error_log = ? WHERE id = ?`, cancelMsg, b.ID)
					logger.Printf("Task %d: cancelled — no net diff vs ask-branch (work already in convoy)", b.ID)
					return
				}
				escMsg := fmt.Sprintf("Sub-PR flow failed (class=%s): %v", errClass, prErr)
				logger.Printf("Task %d: escalating — %s", b.ID, escMsg)
				if _, uErr := db.Exec(`UPDATE BountyBoard SET status = 'Escalated', owner = '', locked_at = '', error_log = ? WHERE id = ?`, escMsg, b.ID); uErr != nil {
					logger.Printf("Task %d: status update failed (%v); escalation still recorded", b.ID, uErr)
				}
				CreateEscalation(db, b.ID, store.SeverityMedium, escMsg)
				return
			}
			// Record history — the task is now AwaitingSubPRCI; sub-pr-ci-watch drives
			// the rest. WriteMemory is spawned later when the sub-PR actually merges.
			histID := store.RecordTaskHistory(db, b.ID, agentName, sessionID, response, "AwaitingSubPRCI")
			if tokIn > 0 || tokOut > 0 {
				store.UpdateTaskHistoryTokens(db, histID, tokIn, tokOut)
			}
			store.LogAudit(db, agentName, "council-approve-subpr", b.ID, ruling.Feedback)
			logger.Printf("Task %d: APPROVED — sub-PR opened, waiting for CI", b.ID)
			return
		}

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

				// Atomic bundle: create resolution task, stamp its branch, mark
				// original ConflictPending, re-activate convoy, record history.
				// Partial failure here risks duplicate resolution tasks on retry.
				tx, txErr := db.Begin()
				if txErr != nil {
					logger.Printf("Task %d: begin conflict-handling tx failed: %v — falling back to non-atomic path", b.ID, txErr)
					handleInfraFailure(db, agentName, "council", b, sessionID, msg, "AwaitingCouncilReview", true, logger)
					return
				}
				resTaskID, addErr := store.AddConvoyTaskTx(tx, b.ID, b.TargetRepo, conflictPayload, b.ConvoyID, b.Priority, "Pending")
				if addErr != nil {
					tx.Rollback()
					logger.Printf("Task %d: add conflict resolution task failed: %v", b.ID, addErr)
					handleInfraFailure(db, agentName, "council", b, sessionID, msg, "AwaitingCouncilReview", true, logger)
					return
				}
				if err := store.SetBranchNameTx(tx, resTaskID, ""); err != nil {
					tx.Rollback()
					logger.Printf("Task %d: set branch name on conflict task failed: %v", b.ID, err)
					handleInfraFailure(db, agentName, "council", b, sessionID, msg, "AwaitingCouncilReview", true, logger)
					return
				}
				failMsg := fmt.Sprintf("Merge conflict on branch %s — conflict resolution task #%d spawned", branchName, resTaskID)
				if err := store.MarkConflictPendingTx(tx, b.ID, failMsg); err != nil {
					tx.Rollback()
					logger.Printf("Task %d: mark ConflictPending failed: %v", b.ID, err)
					handleInfraFailure(db, agentName, "council", b, sessionID, msg, "AwaitingCouncilReview", true, logger)
					return
				}
				if b.ConvoyID > 0 {
					if _, err := tx.Exec(`UPDATE Convoys SET status = 'Active' WHERE id = ? AND status = 'Failed'`, b.ConvoyID); err != nil {
						tx.Rollback()
						logger.Printf("Task %d: convoy re-activate failed: %v", b.ID, err)
						handleInfraFailure(db, agentName, "council", b, sessionID, msg, "AwaitingCouncilReview", true, logger)
						return
					}
				}
				histID, histErr := store.RecordTaskHistoryTx(tx, b.ID, agentName, sessionID, response, "ConflictPending")
				if histErr != nil {
					tx.Rollback()
					logger.Printf("Task %d: record history failed: %v", b.ID, histErr)
					handleInfraFailure(db, agentName, "council", b, sessionID, msg, "AwaitingCouncilReview", true, logger)
					return
				}
				if tokIn > 0 || tokOut > 0 {
					if _, err := tx.Exec(`UPDATE TaskHistory SET tokens_in = ?, tokens_out = ? WHERE id = ?`, tokIn, tokOut, histID); err != nil {
						tx.Rollback()
						logger.Printf("Task %d: update history tokens failed: %v", b.ID, err)
						handleInfraFailure(db, agentName, "council", b, sessionID, msg, "AwaitingCouncilReview", true, logger)
						return
					}
				}
				if err := tx.Commit(); err != nil {
					logger.Printf("Task %d: conflict-handling commit failed: %v", b.ID, err)
					handleInfraFailure(db, agentName, "council", b, sessionID, msg, "AwaitingCouncilReview", true, logger)
					return
				}

				logger.Printf("Task %d: spawned conflict resolution task #%d", b.ID, resTaskID)
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
		writeMemJSON, _ := json.Marshal(map[string]string{
			"task":     util.TruncateStr(directiveText(memPayload), 800),
			"files":    filesStr,
			"feedback": ruling.Feedback,
			"diff":     util.TruncateStr(diff, 4000),
			"repo":     b.TargetRepo,
		})
		store.AddBounty(db, b.ID, "WriteMemory", string(writeMemJSON))
		logger.Printf("Task %d: COMPLETED and merged", b.ID)

		if b.ParentID > 0 {
			var parentStatus string
			db.QueryRow(`SELECT status FROM BountyBoard WHERE id = ?`, b.ParentID).Scan(&parentStatus)
			if parentStatus == "ConflictPending" {
				// Walk up the full ConflictPending chain — a conflict resolution task can itself
				// hit a conflict and spawn another resolution task, leaving multiple ancestors
				// stuck in ConflictPending. Mark all of them Completed and unblock their dependents.
				chain := []int{b.ParentID}
				for {
					var grandParentID int
					var grandParentStatus string
					db.QueryRow(`SELECT parent_id, status FROM BountyBoard WHERE id = ?`, chain[len(chain)-1]).
						Scan(&grandParentID, &grandParentStatus)
					if grandParentID == 0 || grandParentStatus != "ConflictPending" {
						break
					}
					chain = append(chain, grandParentID)
				}
				for _, ancestorID := range chain {
					store.UpdateBountyStatus(db, ancestorID, "Completed")
					if n := store.UnblockDependentsOf(db, ancestorID); n > 0 {
						logger.Printf("Task %d: unblocked %d dependent(s) of #%d after conflict chain resolution", b.ID, n, ancestorID)
					}
				}
				rootID := chain[len(chain)-1]
				store.SendMail(db, agentName, "operator",
					fmt.Sprintf("[CONFLICT RESOLVED] Task #%d is complete — merged via #%d", rootID, b.ID),
					fmt.Sprintf("Conflict resolution task #%d has been approved and merged.\n\nTask #%d (%s) is now complete — the feature is in main. No further action needed.\n\nCouncil feedback: %s",
						b.ID, rootID, b.TargetRepo, ruling.Feedback),
					b.ID, store.MailTypeInfo)
			} else if parentStatus == "Failed" {
				store.ResetTask(db, b.ParentID)
				store.SendMail(db, agentName, "operator",
					fmt.Sprintf("[REMEDIATION COMPLETE] Task #%d fixed — original task #%d requeued", b.ID, b.ParentID),
					fmt.Sprintf("Remediation task #%d has been approved and merged.\n\nThe infra issue blocking task #%d (%s) has been resolved and the original task has been automatically requeued.\n\nCouncil feedback: %s",
						b.ID, b.ParentID, b.TargetRepo, ruling.Feedback),
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
			"", "council-rejection")

		if retryCount >= MaxRetries {
			msg := fmt.Sprintf("Max retries (%d) exceeded. Final rejection: %s", MaxRetries, ruling.Feedback)
			store.FailBounty(db, b.ID, msg)
			telemetry.EmitEvent(telemetry.EventTaskFailed(sessionID, agentName, b.ID, msg))

			// Send rejection history to librarian for memory synthesis.
			store.SendMail(db, agentName, "librarian",
				fmt.Sprintf("[REJECTED] Task #%d — attempt %d/%d (final)", b.ID, retryCount, MaxRetries),
				fmt.Sprintf("The Jedi Council permanently rejected task #%d (attempt %d/%d).\n\nReason: %s",
					b.ID, retryCount, MaxRetries, ruling.Feedback),
				b.ID, store.MailTypeFeedback)

			// Queue a MedicReview — the Medic decides whether to requeue, shard, or escalate.
			// No operator mail here; noisy raw-failure alerts are replaced by Medic escalations
			// which include root-cause analysis and a concrete recommendation.
			medicID := store.QueueMedicReview(db, b, "council_rejection", msg)
			logger.Printf("Task %d: permanently failed — MedicReview #%d queued", b.ID, medicID)
			return
		}
		newPayload := fmt.Sprintf("%s\n\nFEEDBACK (attempt %d/%d): %s", b.Payload, retryCount, MaxRetries, ruling.Feedback)
		store.ReturnTaskForRework(db, b.ID, newPayload)
		store.SendMail(db, agentName, "astromech",
			fmt.Sprintf("[REJECTED] Task #%d — attempt %d/%d", b.ID, retryCount, MaxRetries),
			fmt.Sprintf("The Jedi Council reviewed your work on task #%d and rejected it.\n\nReason: %s\n\nPlease address this feedback in your next attempt.",
				b.ID, ruling.Feedback),
			b.ID, store.MailTypeFeedback)
		store.SendMail(db, agentName, "librarian",
			fmt.Sprintf("[REJECTED] Task #%d — attempt %d/%d", b.ID, retryCount, MaxRetries),
			fmt.Sprintf("The Jedi Council rejected task #%d (attempt %d/%d).\n\nReason: %s",
				b.ID, retryCount, MaxRetries, ruling.Feedback),
			b.ID, store.MailTypeFeedback)
	}
}
