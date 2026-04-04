package agents

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"log"
	"math/rand"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	igit "force-orchestrator/internal/git"
	"force-orchestrator/internal/claude"
	"force-orchestrator/internal/store"
	"force-orchestrator/internal/telemetry"
	"force-orchestrator/internal/util"
)

const astromechTimeout = 15 * time.Minute

// maxOutputBytes is the circuit-breaker threshold for blown-context detection.
// If Claude's combined output exceeds this size, the task is re-queued for
// decomposition rather than sent to council review with potentially garbled output.
const maxOutputBytes = 200_000

// defaultMaxTurns is the default --max-turns value passed to claude CLI.
// Keeps tasks focused and prevents runaway sessions from exhausting context.
const defaultMaxTurns = 40

const AstromechSystemPrompt = `You are an Astromech droid unit in the Galactic Fleet — an elite, autonomous AI software engineer.
You have been assigned a mission. Your job is to complete it and commit the result. No push, no pull request. Just the work.

# PRIME DIRECTIVE
Bring the repository to the desired state described by the task. This includes ANY kind of change:
creating files, modifying files, deleting files, refactoring code, reverting changes, fixing bugs, etc.
You are not limited to creating new files — do whatever is necessary to fulfill the task.

# WORKFLOW RULES
1. Understand: Read the task carefully and the broader goal context if provided. Determine what the repo should look like when done.
2. Inspect: Use Read and Bash tools to understand the current state of the repo before making changes.
3. Execute: Make the necessary file changes using your tools. YOU MUST MAKE ACTUAL CHANGES — do not just analyze.
4. Commit: Commit your changes locally with a descriptive conventional commit message.
5. STOP: Do NOT push to origin. Do NOT open pull requests. Local commit only.

# IMPORTANT GIT RULES
- NEVER rewrite history. Do NOT use git commit --amend, git rebase, or git reset on existing commits.
- Always make a NEW forward commit that brings the repo to the desired state.
- If asked to "revert" or "undo" something, delete or restore the files in your worktree and commit that as a new change.
- If asked to "clean up" a prior commit, make the corrected state in a new commit — do not amend.

# EXTERNAL TOOLS (READ-ONLY)
You have read-only access to Jira, Confluence, Glean, SonarQube, and Datadog. Use them purposefully:
- Jira/Confluence/Glean: look up ticket acceptance criteria, design docs, API contracts, or runbooks
  relevant to the task. If the task references a ticket ID, look it up before starting.
- SonarQube: check the quality gate status and existing issues for this project before making changes.
  Use analyze_code_snippet to validate non-trivial logic before committing.
- Datadog: search logs or traces when diagnosing a bug or understanding runtime behavior of the
  system you're modifying. Check service dependencies before changing an API contract.
You may NOT create or edit Jira tickets, Confluence pages, or SonarQube issues.
Only look up what is directly relevant — do not over-research before coding.

# SIGNALS
- If you cannot proceed without human input, emit: [ESCALATED:LOW|MEDIUM|HIGH:reason]
  Example: [ESCALATED:MEDIUM:Cannot determine correct API version without reading production config]
- After completing a major step, emit: [CHECKPOINT: step_name]
  Example: [CHECKPOINT: schema_migration_written]
- If this task is too large or complex for a single focused coding session, emit ONLY: [SHARD_NEEDED]
  IMPORTANT: Evaluate this BEFORE starting any work. Emit [SHARD_NEEDED] immediately — as your very first
  output — if the task clearly spans multiple independent subsystems, requires touching more than ~5
  unrelated files across different concerns, or would take more than one focused session to complete
  correctly. Do not start coding and then abandon midway — decide upfront.
- When your work is fully committed and ready for review, you MAY emit: [DONE]
  This signals the orchestrator to immediately send your work for council review.`

// nextReviewStatus returns the appropriate post-completion status for a task.
// Tasks in a coordinated convoy route through the Captain first for plan-coherence
// review; all others (direct add-task, uncoordinated convoys) go straight to council.
func nextReviewStatus(db *sql.DB, convoyID int) string {
	if store.IsConvoyCoordinated(db, convoyID) {
		return "AwaitingCaptainReview"
	}
	return "AwaitingCouncilReview"
}

// rateLimitRetries caches per-agent rate-limit hit count in memory, keyed by agent name.
// It is initialized from SystemConfig on first access and persisted after each hit.
// sync.Map is required because multiple SpawnAstromech goroutines access this concurrently.
var rateLimitRetries sync.Map

// permanentInfraFail marks a task as permanently failed and spawns a remediation
// CodeEdit task so an agent can investigate and fix the infra issue automatically.
// It also mails the operator so the failure doesn't go unnoticed.
func permanentInfraFail(db *sql.DB, logger interface{ Printf(string, ...any) },
	sessionID, agentName string, bounty *store.Bounty, msg string) {

	store.FailBounty(db, bounty.ID, msg)
	telemetry.EmitEvent(telemetry.EventTaskFailed(sessionID, agentName, bounty.ID, msg))
	store.LogAudit(db, agentName, "infra-fail", bounty.ID, msg)

	// Spawn a CodeEdit remediation task so an agent can investigate and fix the infra issue.
	remPayload := fmt.Sprintf(
		`Infra failure on task #%d (repo: %s).

Error: %s

Investigate and fix the underlying infrastructure issue so the original task can be retried.
Common causes:
- Repository path is not a git repo → run 'git init' or verify the registered path with 'force repos'
- Worktree directory is corrupted → run 'force cleanup' or manually remove the stale worktree
- Disk full or permission denied → check disk space and file permissions

After fixing, run: force reset %d`,
		bounty.ID, bounty.TargetRepo, msg, bounty.ID)
	remID := store.AddBounty(db, bounty.ID, "CodeEdit", remPayload)
	logger.Printf("Task %d: permanently failed — spawned remediation CodeEdit task #%d", bounty.ID, remID)

	// Store failure memory so future agents know this infra issue occurred
	store.StoreFleetMemory(db, bounty.TargetRepo, bounty.ID, "failure",
		fmt.Sprintf("Task: %s\nInfra failure (permanent): %s", util.TruncateStr(directiveText(bounty.Payload), 300), msg),
		"")

	// Notify operator via mail
	store.SendMail(db, agentName, "operator",
		fmt.Sprintf("[INFRA FAIL] Task #%d — %s", bounty.ID, bounty.TargetRepo),
		fmt.Sprintf("Task #%d permanently failed after %d infra errors.\n\nRepo: %s\nError: %s\n\nRemediation task #%d has been queued to investigate.\nOnce fixed, run: force reset %d",
			bounty.ID, MaxInfraFailures, bounty.TargetRepo, msg, remID, bounty.ID),
		bounty.ID, store.MailTypeAlert)
}

// conflictBranchFromPayload returns the branch name embedded in a [CONFLICT_BRANCH: name]
// prefix, or empty string if the payload is not a conflict resolution task.
func conflictBranchFromPayload(payload string) string {
	const prefix = "[CONFLICT_BRANCH: "
	if strings.HasPrefix(payload, prefix) {
		if end := strings.Index(payload, "]\n\n"); end != -1 {
			return payload[len(prefix):end]
		}
	}
	return ""
}

// directiveText returns the task-specific part of a payload, stripping any leading
// [GOAL: ...] block that Commander prefixes onto subtask payloads. The goal is already
// injected into the prompt as a separate # BROADER GOAL section via buildAstromechContext,
// so including it again in YOUR CURRENT DIRECTIVE causes Claude to treat the original
// feature request as an instruction set rather than context.
func directiveText(payload string) string {
	if strings.HasPrefix(payload, "[GOAL: ") {
		if end := strings.Index(payload, "]\n\n"); end != -1 {
			return payload[end+3:]
		}
	}
	return payload
}

// buildAstromechContext assembles the variable context sections injected into every
// Astromech prompt: parent goal, fleet memory, checkpoint resume, prior attempts (seance),
// and inbox mail. The directive is excluded — callers add it to the system prompt.
// Used by both the daemon loop (runAstromechTask) and the foreground runner (RunTaskForeground).
func buildAstromechContext(
	db *sql.DB,
	bounty *store.Bounty,
	agentName string,
	logger interface{ Printf(string, ...any) },
) (goalCtx, fleetMemCtx, checkpointCtx, seanceCtx, inboxCtx string) {

	// Parent goal context
	if bounty.ParentID > 0 {
		if parent, err := store.GetBounty(db, bounty.ParentID); err == nil {
			goalCtx = fmt.Sprintf("\n\n# BROADER GOAL\nThis task is part of a larger feature request. Keep this in mind for context:\n%s", parent.Payload)
		}
	}

	// Resume from checkpoint if a prior attempt reached one
	if bounty.Checkpoint != "" {
		checkpointCtx = fmt.Sprintf("\n\n# RESUME POINT\nA previous attempt reached checkpoint: %s\nContinue from where it left off.", bounty.Checkpoint)
	}

	// Seance — inject all prior attempt outputs so agent sees the full retry arc
	if history := store.GetTaskHistory(db, bounty.ID); len(history) > 0 {
		var parts []string
		for _, h := range history {
			out := h.ClaudeOutput
			if len(out) > 1500 {
				out = out[:1500] + "\n...[truncated]"
			}
			parts = append(parts, fmt.Sprintf("=== Attempt %d (agent: %s, outcome: %s) ===\n%s",
				h.Attempt, h.Agent, h.Outcome, out))
		}
		seanceCtx = fmt.Sprintf(
			"\n\n# PRIOR ATTEMPTS (%d total)\nLearn from what has already been tried — do not repeat failed approaches:\n\n%s",
			len(history), strings.Join(parts, "\n\n"))
	}

	// Fleet memory — recent successes and failures on this repo
	if memories := store.GetFleetMemories(db, bounty.TargetRepo, bounty.Payload, 10); len(memories) > 0 {
		var successes, failures []string
		for _, m := range memories {
			entry := fmt.Sprintf("- [Task #%d] %s", m.TaskID, util.TruncateStr(m.Summary, 250))
			if m.FilesChanged != "" {
				entry += fmt.Sprintf("\n  Files: %s", m.FilesChanged)
			}
			if m.Outcome == "failure" {
				failures = append(failures, entry)
			} else {
				successes = append(successes, entry)
			}
		}
		var sections []string
		if len(successes) > 0 {
			sections = append(sections, fmt.Sprintf("## What has worked on %s\n%s",
				bounty.TargetRepo, strings.Join(successes, "\n")))
		}
		if len(failures) > 0 {
			sections = append(sections, fmt.Sprintf("## What has failed on %s — do not repeat these approaches\n%s",
				bounty.TargetRepo, strings.Join(failures, "\n")))
		}
		if len(sections) > 0 {
			fleetMemCtx = "\n\n# FLEET MEMORY\n" + strings.Join(sections, "\n\n")
		}
	}

	// Inbox — unread mail for this agent+task, marked read on consumption
	inboxCtx = buildInboxContext(db, agentName, "astromech", bounty.ID, logger)

	return
}

func SpawnAstromech(db *sql.DB, name string) {
	logger := NewLogger(name)
	logger.Printf("Astromech %s starting up", name)

	for {
		// Hard stop — operator activated e-stop
		if IsEstopped(db) {
			time.Sleep(5 * time.Second)
			continue
		}
		// Soft throttle — too many tasks already running
		if IsOverCapacity(db) {
			time.Sleep(2 * time.Second)
			continue
		}
		// Batch-size throttle — too many tasks claimed in the last 60s fleet-wide
		if IsThrottledByBatchSize(db) {
			time.Sleep(3 * time.Second)
			continue
		}

		bounty, claimed := store.ClaimBounty(db, "CodeEdit", name)
		if !claimed {
			// Jitter prevents all agents from hammering the DB in lockstep
			time.Sleep(time.Duration(1500+rand.Intn(1000)) * time.Millisecond)
			continue
		}

		// Restore persisted rate-limit hit count from DB on first claim by this agent
		if _, seen := rateLimitRetries.Load(name); !seen {
			rateLimitRetries.Store(name, claude.LoadRateLimitHits(db, name))
		}

		// Spawn delay — smooth out thundering-herd on large backlogs
		if delay := SpawnDelayDuration(db); delay > 0 {
			time.Sleep(delay)
		}

		runAstromechTask(db, name, bounty, logger)
	}
}

// runAstromechTask executes a single CodeEdit task end-to-end: sets up the worktree,
// runs Claude CLI, and processes the output (signals, commits, status updates).
func runAstromechTask(db *sql.DB, name string, bounty *store.Bounty, logger *log.Logger) {
	sessionID := telemetry.NewSessionID()
	logger.Printf("[%s] Claimed task %d: [%s] %s", sessionID, bounty.ID, bounty.TargetRepo, bounty.Payload)
	telemetry.EmitEvent(telemetry.EventTaskClaimed(sessionID, name, bounty.ID, bounty.TargetRepo, bounty.Payload))

	repoPath := store.GetRepoPath(db, bounty.TargetRepo)
	if repoPath == "" {
		msg := fmt.Sprintf("DB Err: unknown target repository '%s'", bounty.TargetRepo)
		logger.Printf("Task %d FAILED: %s", bounty.ID, msg)
		store.FailBounty(db, bounty.ID, msg)
		store.RecordTaskHistory(db, bounty.ID, name, sessionID, "", "Failed")
		telemetry.EmitEvent(telemetry.EventTaskFailed(sessionID, name, bounty.ID, msg))
		return
	}

	worktreeDir, wtErr := igit.GetOrCreateAgentWorktree(db, name, repoPath)
	if wtErr != nil {
		msg := fmt.Sprintf("Worktree Err: %v", wtErr)
		logger.Printf("Task %d: infra failure — %s", bounty.ID, msg)
		handleInfraFailure(db, name, "worktree", bounty, sessionID, msg, "Pending", true, logger)
		return
	}

	var branchName string
	var isResume bool
	if cb := conflictBranchFromPayload(bounty.Payload); cb != "" {
		// Conflict resolution task — check out the existing branch and start the merge
		// so Claude sees the conflict markers and can resolve them.
		logger.Printf("Task %d: conflict resolution for branch %s", bounty.ID, cb)
		if cbErr := igit.PrepareConflictBranch(worktreeDir, repoPath, cb); cbErr != nil {
			msg := fmt.Sprintf("Conflict Branch Err: %v", cbErr)
			logger.Printf("Task %d: infra failure — %s", bounty.ID, msg)
			handleInfraFailure(db, name, "branch preparation", bounty, sessionID, msg, "Pending", true, logger)
			return
		}
		branchName = cb
	} else {
		var branchErr error
		branchName, isResume, branchErr = igit.PrepareAgentBranch(worktreeDir, repoPath, bounty.ID, name, bounty.BranchName)
		if branchErr != nil {
			msg := fmt.Sprintf("Branch Err: %v", branchErr)
			logger.Printf("Task %d: infra failure — %s", bounty.ID, msg)
			handleInfraFailure(db, name, "branch preparation", bounty, sessionID, msg, "Pending", true, logger)
			return
		}
	}
	store.SetBranchName(db, bounty.ID, branchName)
	if isResume {
		logger.Printf("Task %d: resuming existing branch %s in %s", bounty.ID, branchName, worktreeDir)
	} else {
		logger.Printf("Task %d: branch %s ready in %s", bounty.ID, branchName, worktreeDir)
	}

	// ── Build prompt ─────────────────────────────────────────────────────

	goalContext, fleetMemoryContext, checkpointContext, seanceContext, inboxContext :=
		buildAstromechContext(db, bounty, name, logger)

	directive := LoadDirective("astromech", bounty.TargetRepo)
	directiveSection := ""
	if directive != "" {
		directiveSection = fmt.Sprintf("\n\n# OPERATOR DIRECTIVE\n%s", directive)
	}

	specialCtx := ""
	if conflictBranchFromPayload(bounty.Payload) != "" {
		specialCtx = `

# CONFLICT RESOLUTION TASK
Your worktree has been set up with the feature branch checked out and a merge of the default branch already started.
There are merge conflict markers (<<<<<<< HEAD, =======, >>>>>>> ) in one or more files.

Your job:
1. Run: git status — to see which files have conflicts
2. Open each conflicted file and resolve the conflict markers by choosing the correct final content
3. Stage the resolved files: git add <file>
4. Complete the merge: git commit (use the default merge commit message or a descriptive one)
5. Do NOT use git merge --abort. Resolve and commit.

The original task directive (what the code should accomplish) is shown below.`
	} else if isResume {
		specialCtx = `

# RESUMING PRIOR WORK
Your previous work is already committed on this branch — you are NOT starting from scratch.
Run: git log --oneline -5 to see what has already been done.
Read the feedback at the bottom of YOUR CURRENT DIRECTIVE, then make a NEW forward commit that addresses only what was flagged.
Do not re-do work that is already correctly committed.`
	}

	systemPrompt := AstromechSystemPrompt + directiveSection
	fullPrompt := fmt.Sprintf("%s%s%s%s%s%s%s\n\nYOUR CURRENT DIRECTIVE:\n%s",
		systemPrompt, goalContext, fleetMemoryContext, checkpointContext, seanceContext, inboxContext,
		specialCtx, directiveText(bounty.Payload))

	maxTurns := store.GetConfig(db, "max_turns", fmt.Sprintf("%d", defaultMaxTurns))

	// Per-task timeout overrides the fleet default when set.
	sessionTimeout := astromechTimeout
	if bounty.TaskTimeout > 0 {
		sessionTimeout = time.Duration(bounty.TaskTimeout) * time.Second
	}

	maxTurnsInt, _ := strconv.Atoi(maxTurns)
	logger.Printf("Task %d: running claude CLI (timeout: %v)", bounty.ID, sessionTimeout)

	// Heartbeat goroutine — logs every 2 minutes so fleet.log confirms Claude is alive
	// during long silent runs. Stops as soon as RunCLIStreaming returns.
	heartbeatDone := make(chan struct{})
	heartbeatStart := time.Now()
	go func() {
		ticker := time.NewTicker(2 * time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-heartbeatDone:
				return
			case <-ticker.C:
				logger.Printf("Task %d: claude still running (%v elapsed)", bounty.ID, time.Since(heartbeatStart).Round(time.Second))
			}
		}
	}()

	// Per-task streaming log — written to fleet-task-<id>.log while Claude runs.
	// The file is removed on completion so it only exists for in-progress tasks.
	// `force tail <id>` tails this file to show live output.
	taskLogPath := fmt.Sprintf("fleet-task-%d.log", bounty.ID)
	taskLogFile, _ := os.Create(taskLogPath)
	var taskWriter io.Writer = io.Discard
	if taskLogFile != nil {
		taskWriter = taskLogFile
	}

	rawOut, err := claude.RunCLIStreaming(fullPrompt, "Edit,Write,Read,Bash,Glob,Grep,"+claude.AstromechExtraTools, worktreeDir, maxTurnsInt, sessionTimeout, taskWriter)

	close(heartbeatDone)
	if taskLogFile != nil {
		taskLogFile.Close()
		os.Remove(taskLogPath)
	}

	outputStr := strings.TrimSpace(rawOut)
	outputPreview := outputStr
	if len(outputPreview) > 500 {
		outputPreview = outputPreview[:500] + "...[truncated]"
	}
	logger.Printf("Task %d: claude output preview:\n%s", bounty.ID, outputPreview)

	// ── Handle Claude CLI errors ─────────────────────────────────────────

	if err != nil {
		var msg string
		if strings.HasPrefix(err.Error(), "claude CLI timed out") {
			msg = fmt.Sprintf("Timeout Err: Claude CLI exceeded %v time limit", sessionTimeout)
		} else {
			msg = fmt.Sprintf("Claude CLI Err: %s", outputStr)
		}

		// Rate limit: back off without burning the circuit breaker
		if claude.IsRateLimitError(outputStr) || claude.IsRateLimitError(err.Error()) {
			rlCountVal, _ := rateLimitRetries.Load(name)
			rlCount, _ := rlCountVal.(int)
			rateLimitRetries.Store(name, rlCount+1)
			claude.PersistRateLimitHit(db, name, rlCount+1)
			backoff := RateLimitBackoff(rlCount)
			logger.Printf("Task %d: rate limit detected (hit %d), backing off %v", bounty.ID, rlCount+1, backoff)
			telemetry.EmitEvent(telemetry.EventRateLimited(sessionID, name, bounty.ID, rlCount+1, backoff))
			store.UpdateBountyStatus(db, bounty.ID, "Pending")
			time.Sleep(backoff)
			return
		}

		// Generic infra failure
		logger.Printf("Task %d: infra failure — %s", bounty.ID, msg)
		histID := store.RecordTaskHistory(db, bounty.ID, name, sessionID, outputStr, "Failed")
		tokIn, tokOut := claude.ParseTokenUsage(outputStr)
		if tokIn > 0 || tokOut > 0 {
			store.UpdateTaskHistoryTokens(db, histID, tokIn, tokOut)
		}
		handleInfraFailure(db, name, "claude", bounty, sessionID, msg, "Pending", false, logger)
		return
	}

	// Successful Claude run — reset rate-limit counter for this agent
	rateLimitRetries.Delete(name)
	claude.ClearRateLimitHits(db, name)

	processAstromechOutput(db, name, bounty, sessionID, outputStr, worktreeDir, branchName, repoPath, logger, true)
}

// processAstromechOutput handles all post-Claude-run logic shared between the daemon
// loop (runAstromechTask) and the foreground runner (RunTaskForeground): checkpoint
// scanning, optional ownership check, output size circuit breaker, signal parsing
// (ESCALATED, SHARD_NEEDED, DONE), commit inference, and history recording.
// checkOwnership should be true for daemon runs to guard against inquisitor races.
func processAstromechOutput(
	db *sql.DB,
	name string,
	bounty *store.Bounty,
	sessionID string,
	outputStr string,
	worktreeDir string,
	branchName string,
	repoPath string,
	logger interface{ Printf(string, ...any) },
	checkOwnership bool,
) {
	taskID := bounty.ID

	// Scan output for checkpoint signals
	for _, line := range strings.Split(outputStr, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "[CHECKPOINT:") && strings.HasSuffix(line, "]") {
			cp := strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(line, "[CHECKPOINT:"), "]"))
			if cp != "" {
				store.UpdateCheckpoint(db, taskID, cp)
				logger.Printf("Task %d: checkpoint recorded: %s", taskID, cp)
			}
		}
	}

	// ── Ownership check (atomic, daemon only) ────────────────────────────
	// A SELECT followed by a separate UPDATE is a TOCTOU race: another agent or
	// the inquisitor can claim the row between the two statements. Instead, issue
	// a no-op UPDATE that only matches when we still own the row. Zero rows
	// affected means ownership was lost while Claude was running.
	if checkOwnership {
		ownerRes, _ := db.Exec(
			`UPDATE BountyBoard SET status = 'Locked' WHERE id = ? AND owner = ? AND status = 'Locked'`,
			taskID, name,
		)
		if n, _ := ownerRes.RowsAffected(); n == 0 {
			logger.Printf("Task %d: ownership lost mid-run — recording and discarding result", taskID)
			store.RecordTaskHistory(db, taskID, name, sessionID, outputStr, "Discarded")
			store.LogAudit(db, name, "ownership-lost", taskID,
				"Claude completed work but ownership changed mid-run — work discarded")
			base := igit.GetDefaultBranch(repoPath)
			exec.Command("git", "-C", worktreeDir, "checkout", "--detach", base).Run()
			exec.Command("git", "-C", repoPath, "branch", "-D", branchName).Run()
			return
		}
	}

	// ── Output size circuit breaker ──────────────────────────────────────
	if len(outputStr) > maxOutputBytes {
		logger.Printf("Task %d: output too large (%d bytes) — likely context-blown, re-queuing as Decompose", taskID, len(outputStr))
		store.RecordTaskHistory(db, taskID, name, sessionID, outputStr[:min(len(outputStr), 4000)]+"...[truncated for history]", "ContextBlown")
		store.StoreFleetMemory(db, bounty.TargetRepo, taskID, "failure",
			fmt.Sprintf("Task produced %d bytes of output (context blown) — was re-queued for decomposition.\nTask: %s",
				len(outputStr), util.TruncateStr(directiveText(bounty.Payload), 300)),
			"")
		base := igit.GetDefaultBranch(repoPath)
		exec.Command("git", "-C", worktreeDir, "checkout", "--detach", base).Run()
		exec.Command("git", "-C", repoPath, "branch", "-D", branchName).Run()
		store.AddBounty(db, taskID, "Decompose", bounty.Payload)
		store.UpdateBountyStatus(db, taskID, "Completed")
		telemetry.EmitEvent(telemetry.EventTaskSharded(sessionID, name, taskID))
		store.SendMail(db, name, "operator",
			fmt.Sprintf("[SHARD] Task #%d context-blown — re-queued for decomposition", taskID),
			fmt.Sprintf("Task #%d produced %d bytes of output, exceeding the %d-byte limit.\n\nThis indicates Claude hit the context limit mid-task. The task has been re-queued as a Decompose request so Commander can break it into smaller pieces.\n\nRepo: %s\nOriginal task:\n%s",
				taskID, len(outputStr), maxOutputBytes, bounty.TargetRepo, util.TruncateStr(bounty.Payload, 500)),
			taskID, store.MailTypeAlert)
		return
	}

	// ── Parse signals ────────────────────────────────────────────────────

	// Astromech signaled it needs human input
	if sev, msg, ok := ParseEscalationSignal(outputStr); ok {
		logger.Printf("Task %d: escalated [%s] %s", taskID, sev, msg)
		store.RecordTaskHistory(db, taskID, name, sessionID, outputStr, "Escalated")
		CreateEscalation(db, taskID, sev, msg)
		telemetry.EmitEvent(telemetry.EventTaskEscalated(sessionID, name, taskID, sev, msg))
		store.SendMail(db, name, "operator",
			fmt.Sprintf("[%s] Task #%d escalated — %s", string(sev), taskID, bounty.TargetRepo),
			fmt.Sprintf("Agent %s escalated task #%d at %s severity.\n\nRepo: %s\nReason: %s\n\nTask payload:\n%s",
				name, taskID, string(sev), bounty.TargetRepo, msg, util.TruncateStr(bounty.Payload, 500)),
			taskID, store.MailTypeAlert)
		return
	}

	// Astromech detected the task is too large — hand off to Commander
	if strings.Contains(outputStr, "[SHARD_NEEDED]") {
		logger.Printf("Task %d: task too large, re-queuing as Decompose for Commander", taskID)
		store.RecordTaskHistory(db, taskID, name, sessionID, outputStr, "Sharded")
		store.StoreFleetMemory(db, bounty.TargetRepo, taskID, "failure",
			fmt.Sprintf("Task scope was too large for a single session — sharded for re-decomposition.\nTask: %s",
				util.TruncateStr(directiveText(bounty.Payload), 300)),
			"")
		base := igit.GetDefaultBranch(repoPath)
		exec.Command("git", "-C", worktreeDir, "checkout", "--detach", base).Run()
		exec.Command("git", "-C", repoPath, "branch", "-D", branchName).Run()
		store.AddBounty(db, taskID, "Decompose", bounty.Payload)
		store.UpdateBountyStatus(db, taskID, "Completed")
		telemetry.EmitEvent(telemetry.EventTaskSharded(sessionID, name, taskID))
		store.SendMail(db, name, "operator",
			fmt.Sprintf("[SHARD] Task #%d too large — re-queued for decomposition", taskID),
			fmt.Sprintf("Task #%d was determined to be too large for a single session and has been re-queued as a Decompose request for Commander to break into smaller pieces.\n\nRepo: %s\nOriginal task:\n%s",
				taskID, bounty.TargetRepo, util.TruncateStr(bounty.Payload, 500)),
			taskID, store.MailTypeAlert)
		return
	}

	// Explicit [DONE] signal — agent is confident work is committed, skip inference
	if strings.Contains(outputStr, "[DONE]") {
		nextStatus := nextReviewStatus(db, bounty.ConvoyID)
		logger.Printf("Task %d: [DONE] signal received — submitting for review (%s)", taskID, nextStatus)
		histID := store.RecordTaskHistory(db, taskID, name, sessionID, outputStr, "Completed")
		tokIn, tokOut := claude.ParseTokenUsage(outputStr)
		if tokIn > 0 || tokOut > 0 {
			store.UpdateTaskHistoryTokens(db, histID, tokIn, tokOut)
		}
		telemetry.EmitEvent(telemetry.EventTaskDoneSignal(sessionID, name, taskID))
		telemetry.EmitEvent(telemetry.EventTaskCompleted(sessionID, name, taskID))
		store.UpdateBountyStatus(db, taskID, nextStatus)
		return
	}

	// ── Commit inference ─────────────────────────────────────────────────

	gitAddOut, _ := exec.Command("git", "-C", worktreeDir, "add", ".").CombinedOutput()
	logger.Printf("Task %d: git add output: %s", taskID, strings.TrimSpace(string(gitAddOut)))

	gitStatusOut, _ := exec.Command("git", "-C", worktreeDir, "status", "--short").CombinedOutput()
	logger.Printf("Task %d: git status after add: %s", taskID, strings.TrimSpace(string(gitStatusOut)))

	// Truncate payload to 72 chars for the commit subject line
	commitSubject := util.TruncateStr(strings.SplitN(bounty.Payload, "\n", 2)[0], 72)
	commitOut, commitErr := exec.Command("git", "-C", worktreeDir, "commit", "-m",
		fmt.Sprintf("task(%d): %s", taskID, commitSubject)).CombinedOutput()
	logger.Printf("Task %d: git commit output: %s", taskID, strings.TrimSpace(string(commitOut)))

	if commitErr != nil {
		// Claude may have already committed — check for commits ahead of the default branch
		base := igit.GetDefaultBranch(repoPath)
		aheadOut, _ := exec.Command("git", "-C", worktreeDir, "log", base+"..HEAD", "--oneline").CombinedOutput()
		logger.Printf("Task %d: commits ahead of base: %s", taskID, strings.TrimSpace(string(aheadOut)))

		if len(strings.TrimSpace(string(aheadOut))) == 0 {
			// Claude finished but made no commits. This is often caused by the host going
			// to sleep mid-run, leaving the agent in a confused state on wake. Re-queue
			// with a note up to MaxRetries so the agent gets a clean attempt; only
			// permanently fail if it keeps producing zero changes across all retries.
			retryCount := store.IncrementRetryCount(db, taskID)
			msg := fmt.Sprintf("Zero file changes (attempt %d/%d) — re-queuing for retry", retryCount, MaxRetries)
			logger.Printf("Task %d: %s", taskID, msg)
			store.RecordTaskHistory(db, taskID, name, sessionID, outputStr, "ZeroChanges")
			if retryCount >= MaxRetries {
				failMsg := fmt.Sprintf("Git Commit Err: Claude CLI finished but made zero file changes after %d attempts", MaxRetries)
				store.FailBounty(db, taskID, failMsg)
				telemetry.EmitEvent(telemetry.EventTaskFailed(sessionID, name, taskID, failMsg))
			} else {
				note := "\n\nNOTE (from orchestrator): A prior attempt of this task completed without making any file changes. " +
					"If the work described already exists in the codebase, verify that and confirm with a commit. " +
					"If it does not exist yet, implement it from scratch — do not rely on prior seance context about what was 'already done'."
				store.ReturnTaskForRework(db, taskID, bounty.Payload+note)
			}
			return
		}
		logger.Printf("Task %d: Claude already committed, treating as success", taskID)
	}

	histID := store.RecordTaskHistory(db, taskID, name, sessionID, outputStr, "Completed")
	tokIn, tokOut := claude.ParseTokenUsage(outputStr)
	if tokIn > 0 || tokOut > 0 {
		store.UpdateTaskHistoryTokens(db, histID, tokIn, tokOut)
	}
	telemetry.EmitEvent(telemetry.EventTaskCompleted(sessionID, name, taskID))
	nextStatus := nextReviewStatus(db, bounty.ConvoyID)
	logger.Printf("Task %d: SUCCESS, status -> %s", taskID, nextStatus)
	store.UpdateBountyStatus(db, taskID, nextStatus)
}

// RunTaskForeground claims a specific task by ID and runs it in the foreground,
// streaming Claude output directly to stdout. This is a one-shot mode for
// debugging or manually re-running a specific task.
func RunTaskForeground(db *sql.DB, taskID int) {
	b, err := store.GetBounty(db, taskID)
	if err != nil {
		fmt.Printf("Task %d not found.\n", taskID)
		os.Exit(1)
	}
	if b.Status != "Pending" {
		fmt.Printf("Task %d is not Pending (status: %s).\nRun 'force reset %d' first if you want to re-run it.\n",
			taskID, b.Status, taskID)
		os.Exit(1)
	}

	repoPath := store.GetRepoPath(db, b.TargetRepo)
	if repoPath == "" {
		fmt.Printf("Unknown repo '%s'.\n", b.TargetRepo)
		os.Exit(1)
	}

	// Claim the task for the foreground runner
	const fgAgent = "force-run"
	res, _ := db.Exec(`UPDATE BountyBoard SET status = 'Locked', owner = ?, locked_at = datetime('now')
		WHERE id = ? AND status = 'Pending'`, fgAgent, taskID)
	if n, _ := res.RowsAffected(); n == 0 {
		fmt.Printf("Task %d could not be claimed (race condition — try again).\n", taskID)
		os.Exit(1)
	}
	store.LogAudit(db, "operator", "run", taskID, "one-shot foreground run")

	worktreeDir, wtErr := igit.GetOrCreateAgentWorktree(db, fgAgent, repoPath)
	if wtErr != nil {
		db.Exec(`UPDATE BountyBoard SET status = 'Pending', owner = '', locked_at = '' WHERE id = ?`, taskID)
		fmt.Printf("Worktree error: %v\n", wtErr)
		os.Exit(1)
	}

	// Foreground runs always start fresh — pass empty existingBranch to ignore any prior branch.
	branchName, _, branchErr := igit.PrepareAgentBranch(worktreeDir, repoPath, taskID, fgAgent, "")
	if branchErr != nil {
		db.Exec(`UPDATE BountyBoard SET status = 'Pending', owner = '', locked_at = '' WHERE id = ?`, taskID)
		fmt.Printf("Branch error: %v\n", branchErr)
		os.Exit(1)
	}
	store.SetBranchName(db, taskID, branchName)

	// Clear stale checkpoints — foreground runs always start fresh, not from a resume point.
	db.Exec(`UPDATE BountyBoard SET checkpoint = '' WHERE id = ?`, taskID)
	b.Checkpoint = ""

	fgLogger := log.New(os.Stderr, "[force-run] ", log.LstdFlags)

	// Build the same rich context as the daemon loop; checkpoint is intentionally skipped (_).
	goalCtx, fleetMemCtx, _, seanceCtx, inboxCtx := buildAstromechContext(db, b, fgAgent, fgLogger)

	directive := LoadDirective("astromech", b.TargetRepo)
	directiveSection := ""
	if directive != "" {
		directiveSection = fmt.Sprintf("\n\n# OPERATOR DIRECTIVE\n%s", directive)
	}

	systemPrompt := AstromechSystemPrompt + directiveSection
	fullPrompt := fmt.Sprintf("%s%s%s%s%s\n\nYOUR CURRENT DIRECTIVE:\n%s",
		systemPrompt, goalCtx, fleetMemCtx, seanceCtx, inboxCtx, directiveText(b.Payload))

	maxTurns := store.GetConfig(db, "max_turns", fmt.Sprintf("%d", defaultMaxTurns))

	sessionTimeout := astromechTimeout
	if b.TaskTimeout > 0 {
		sessionTimeout = time.Duration(b.TaskTimeout) * time.Second
	}

	fmt.Printf("=== force run: task #%d ===\n", taskID)
	fmt.Printf("Repo:    %s\n", b.TargetRepo)
	fmt.Printf("Branch:  %s\n", branchName)
	fmt.Printf("Dir:     %s\n", worktreeDir)
	fmt.Printf("Timeout: %v\n\n", sessionTimeout)

	sessionID := telemetry.NewSessionID()

	ctx, cancel := context.WithTimeout(context.Background(), sessionTimeout)
	defer cancel()

	var outputBuf strings.Builder
	cmd := exec.CommandContext(ctx, "claude", "-p", fullPrompt,
		"--dangerously-skip-permissions",
		"--allowedTools", "Edit,Write,Read,Bash,Glob,Grep,"+claude.AstromechExtraTools,
		"--max-turns", maxTurns)
	cmd.Dir = worktreeDir
	cmd.Stdout = io.MultiWriter(os.Stdout, &outputBuf)
	cmd.Stderr = os.Stderr
	runErr := cmd.Run()
	outputStr := outputBuf.String()

	if runErr != nil {
		fmt.Printf("\n[force run] claude exited with error: %v\n", runErr)
		store.RecordTaskHistory(db, taskID, fgAgent, sessionID, outputStr, "Failed")
		db.Exec(`UPDATE BountyBoard SET status = 'Pending', owner = '', locked_at = '' WHERE id = ?`, taskID)
		fmt.Println("[force run] Task returned to Pending.")
		os.Exit(1)
	}

	processAstromechOutput(db, fgAgent, b, sessionID, outputStr, worktreeDir, branchName, repoPath, fgLogger, false)
}
