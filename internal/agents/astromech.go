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
	"force-orchestrator/internal/agents/capabilities"
	"force-orchestrator/internal/claude"
	"force-orchestrator/internal/store"
	"force-orchestrator/internal/telemetry"
	"force-orchestrator/internal/util"
)

// shortGitTimeout bounds short-lived git subprocess invocations (ownership
// detach, branch delete, status/add/commit) so a hung git subprocess cannot
// wedge the astromech goroutine while it holds the Locked row. AUDIT-158 /
// AUDIT-127 (Fix #8d): pre-fix these were chained invocations of the
// exec.Command constructor directly terminated by Run or CombinedOutput,
// with no context-based timeout.
const shortGitTimeout = 60 * time.Second

// runShortGit runs a git command with a 60s context timeout. Replaces the
// pre-fix chained-and-Run form. AUDIT-158 (Fix #8d).
// Fix #8e: ctx threads from the caller (typically SpawnAstromech's daemon ctx)
// so daemon shutdown / e-stop cancels in-flight subprocesses; the prior
// fabricated context.Background root made the helper deaf to daemon
// cancellation.
func runShortGit(ctx context.Context, args ...string) error {
	ctx, cancel := context.WithTimeout(ctx, shortGitTimeout)
	defer cancel()
	return exec.CommandContext(ctx, "git", args...).Run()
}

// combinedShortGit runs a git command with a 60s context timeout and
// returns combined output. Replaces the pre-fix chained-and-CombinedOutput
// form. AUDIT-158 (Fix #8d). Fix #8e: ctx threads from the caller.
func combinedShortGit(ctx context.Context, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, shortGitTimeout)
	defer cancel()
	return exec.CommandContext(ctx, "git", args...).CombinedOutput()
}

// combinedShortGitArgs is identical to combinedShortGit but accepts a
// pre-built arg slice (for callers assembling positional slots).
// Fix #8e: ctx threads from the caller.
func combinedShortGitArgs(ctx context.Context, args []string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, shortGitTimeout)
	defer cancel()
	return exec.CommandContext(ctx, "git", args...).CombinedOutput()
}

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

// pruneRateLimitRetries drops entries for agent names no longer in the
// Agents table. AUDIT-096 (Fix #8d): the rateLimitRetries sync.Map grew
// unbounded across fleet scale-ups/downs because retired agent names
// left dangling counter entries. Called from the Inquisitor tick.
func pruneRateLimitRetries(db *sql.DB) {
	live := map[string]bool{}
	rows, err := db.Query(`SELECT DISTINCT agent_name FROM Agents`)
	if err != nil {
		return
	}
	for rows.Next() {
		var n string
		if sErr := rows.Scan(&n); sErr == nil {
			live[n] = true
		}
	}
	if rErr := rows.Err(); rErr != nil {
		log.Printf("astromech.go:pruneRateLimitRetries: rows iter error: %v", rErr)
	}
	rows.Close()
	rateLimitRetries.Range(func(key, _ any) bool {
		if name, ok := key.(string); ok && !live[name] {
			rateLimitRetries.Delete(name)
		}
		return true
	})
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

	if err := store.FailBounty(db, bounty.ID, msg); err != nil {
		logger.Printf("Task %d: FailBounty failed (%v); stale-lock detector will recover", bounty.ID, err)
	}
	telemetry.EmitEvent(telemetry.EventTaskFailed(sessionID, agentName, bounty.ID, msg))
	store.LogAudit(db, agentName, "infra-fail", bounty.ID, msg)

	// Don't spawn a Medic to investigate a Medic — that creates infinite chains.
	// Infrastructure tasks that fail hard go straight to escalation instead.
	if store.IsInfrastructureTask(bounty.Type) {
		escMsg := fmt.Sprintf("Infrastructure task #%d (%s) failed permanently: %s", bounty.ID, bounty.Type, msg)
		if _, err := CreateEscalation(db, bounty.ID, store.SeverityMedium, escMsg); err != nil {
			// Fix #8a (AUDIT-041) pattern: escalation insert failed. The task has already been
			// FailBounty'd above, so the operator mail below still fires; the stale-lock detector
			// will pick up any downstream weirdness on the next tick.
			logger.Printf("Task %d: CreateEscalation failed (%v); task already Failed, operator mail still fires", bounty.ID, err)
		}
		store.SendMail(db, agentName, "operator",
			fmt.Sprintf("[INFRA FAIL] Task #%d (%s) failed", bounty.ID, bounty.Type),
			msg, bounty.ID, store.MailTypeAlert)
		logger.Printf("Task %d: infrastructure task failed permanently — escalated (no Medic cascade)", bounty.ID)
		return
	}

	// Queue a MedicReview task — the Medic will analyze the failure and decide whether to
	// requeue, shard, or escalate. No operator mail here; if human attention is needed
	// the Medic sends one quality escalation rather than a raw error alert.
	remID := store.QueueMedicReview(db, bounty, "infra", msg)
	logger.Printf("Task %d: permanently failed (infra) — MedicReview #%d queued", bounty.ID, remID)
}

// conflictBranchFromPayload returns the branch name embedded in a [CONFLICT_BRANCH: name]
// prefix, or empty string if the payload is not a conflict resolution task.
//
// Fix #9 (AUDIT-051): the extracted branch flows straight into `git checkout`.
// An attacker who can post a GitHub review comment with a forged
// `[CONFLICT_BRANCH: --upload-pack=/tmp/evil]` prefix would otherwise reach
// git as a flag. The ref validator below rejects that — if the extracted
// name fails ValidateRef, we return "" so the downstream path takes the
// "not a conflict resolution task" branch.
func conflictBranchFromPayload(payload string) string {
	const prefix = "[CONFLICT_BRANCH: "
	if !strings.HasPrefix(payload, prefix) {
		return ""
	}
	end := strings.Index(payload, "]\n\n")
	if end == -1 {
		return ""
	}
	name := payload[len(prefix):end]
	if err := igit.ValidateRef(name); err != nil {
		return ""
	}
	return name
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

// buildNotesBlock returns the formatted operator-notes block for injection before a
// task directive, or empty string if no notes exist or the query errors.
func buildNotesBlock(db *sql.DB, taskID int, logger interface{ Printf(string, ...any) }) string {
	notes, err := store.GetTaskNotes(db, taskID)
	if err != nil {
		logger.Printf("Task %d: failed to load operator notes (proceeding without): %v", taskID, err)
		return ""
	}
	if len(notes) == 0 {
		return ""
	}
	return "--- Operator Notes ---\n" + strings.Join(notes, "\n") + "\n--- End Notes ---\n\n"
}

// buildAstromechContext assembles the variable context sections injected into every
// Astromech prompt: parent goal, fleet memory, checkpoint resume, prior attempts (seance),
// and inbox mail. The directive is excluded — callers add it to the system prompt.
// Used by both the daemon loop (runAstromechTask) and the foreground runner (RunTaskForeground).
func buildAstromechContext(
	db *sql.DB,
	bounty *store.Bounty,
	agentName string,
	librarianProfile *capabilities.Profile,
	logger interface{ Printf(string, ...any) },
) (goalCtx, fleetMemCtx, checkpointCtx, seanceCtx, inboxCtx string, injectedMemoryIDs []int) {

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

	// Fleet memory — retrieve FTS candidates over-fetched at 20, then let
	// the LLM re-ranker filter to the 5 genuinely relevant ones. FTS gives
	// us recall; the re-ranker gives us precision. If the re-ranker is
	// disabled or errors, we fall through to the FTS order trimmed to 5.
	candidates := store.GetFleetMemories(db, bounty.TargetRepo, bounty.Payload, 20)
	if memories := RerankFleetMemories(db, bounty.Payload, candidates, 5, librarianProfile, logger); len(memories) > 0 {
		var successes, failures []string
		for _, m := range memories {
			injectedMemoryIDs = append(injectedMemoryIDs, m.ID)
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

func SpawnAstromech(ctx context.Context, db *sql.DB, name string) {
	logger := NewLogger(name)

	// D1 T0-1: load Astromech's profile (the worker tool grant) and
	// Librarian's profile (used by the rerank LLM call inside
	// buildAstromechContext) once at spawn-time.
	profile, err := capabilities.LoadProfile("astromech")
	if err != nil {
		logger.Printf("Astromech %s cannot start: %v", name, err)
		return
	}
	librarianProfile, err := capabilities.LoadProfile("librarian")
	if err != nil {
		logger.Printf("Astromech %s cannot start: %v", name, err)
		return
	}
	logger.Printf("Astromech %s starting up", name)

	for {
		// Daemon-level cancellation — AUDIT-020 self-heal path. When the
		// root context is cancelled (shutdown or signal), every agent
		// claim loop exits cleanly instead of racing the 30s drain.
		if ctx.Err() != nil {
			logger.Printf("Astromech %s exiting: %v", name, ctx.Err())
			return
		}
		// Hard stop — operator activated e-stop
		if IsEstopped(db) {
			time.Sleep(5 * time.Second)
			continue
		}
		// Spend cap — skip-and-sleep when trailing-hour spend exceeds soft cap
		if SpendCapExceeded(db) {
			time.Sleep(10 * time.Second)
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

		// AUDIT-130 (Fix #8d): check Repositories.quarantined_at before
		// spending a Claude session. Pre-fix, enforcement lived only in
		// openSubPRForApprovedTask — so a quarantined repo burned a full
		// astromech session before the PR step rejected. Post-fix, we
		// requeue Pending with an error_log and skip to the next claim.
		if bounty.TargetRepo != "" {
			if repo := store.GetRepo(db, bounty.TargetRepo); repo != nil && repo.QuarantinedAt != "" {
				logger.Printf("Task %d: repo %s is quarantined (%s) — requeuing Pending without Claude session",
					bounty.ID, bounty.TargetRepo, repo.QuarantineReason)
				if _, err := db.Exec(`UPDATE BountyBoard SET status = 'Pending', owner = '', locked_at = '',
					error_log = 'Astromech: repo quarantined (' || ? || ') — requeued without Claude session'
					WHERE id = ?`, repo.QuarantineReason, bounty.ID); err != nil {
					logger.Printf("Task %d: quarantine-requeue UPDATE failed: %v — stale-lock detector will recover", bounty.ID, err)
				}
				continue
			}
		}

		// Restore persisted rate-limit hit count from DB on first claim by this agent
		if _, seen := rateLimitRetries.Load(name); !seen {
			rateLimitRetries.Store(name, claude.LoadRateLimitHits(db, name))
		}

		// Spawn delay — smooth out thundering-herd on large backlogs
		if delay := SpawnDelayDuration(db); delay > 0 {
			time.Sleep(delay)
		}

		runAstromechTask(ctx, db, name, bounty, profile, librarianProfile, logger)
	}
}

// runAstromechTask executes a single CodeEdit task end-to-end: sets up the worktree,
// runs Claude CLI, and processes the output (signals, commits, status updates).
// Fix #8e: ctx threads from SpawnAstromech (the daemon ctx) so subprocess
// invocations cancel on shutdown / e-stop.
func runAstromechTask(ctx context.Context, db *sql.DB, name string, bounty *store.Bounty, profile, librarianProfile *capabilities.Profile, logger *log.Logger) {
	sessionID := telemetry.NewSessionID()
	logger.Printf("[%s] Claimed task %d: [%s] %s", sessionID, bounty.ID, bounty.TargetRepo, bounty.Payload)
	telemetry.EmitEvent(telemetry.EventTaskClaimed(sessionID, name, bounty.ID, bounty.TargetRepo, bounty.Payload))

	repoPath := store.GetRepoPath(db, bounty.TargetRepo)
	if repoPath == "" {
		msg := fmt.Sprintf("DB Err: unknown target repository '%s'", bounty.TargetRepo)
		logger.Printf("Task %d FAILED: %s", bounty.ID, msg)
		if err := store.FailBounty(db, bounty.ID, msg); err != nil {
			logger.Printf("Task %d: FailBounty failed (%v); stale-lock detector will recover", bounty.ID, err)
		}
		store.RecordTaskHistory(db, bounty.ID, name, sessionID, "", "Failed")
		telemetry.EmitEvent(telemetry.EventTaskFailed(sessionID, name, bounty.ID, msg))
		return
	}

	worktreeDir, wtErr := igit.GetOrCreateAgentWorktree(ctx, db, name, repoPath)
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
		if cbErr := igit.PrepareConflictBranch(ctx, worktreeDir, repoPath, cb); cbErr != nil {
			msg := fmt.Sprintf("Conflict Branch Err: %v", cbErr)
			logger.Printf("Task %d: infra failure — %s", bounty.ID, msg)
			handleInfraFailure(db, name, "branch preparation", bounty, sessionID, msg, "Pending", true, logger)
			return
		}
		branchName = cb
	} else {
		// Under the PR flow, astromechs branch off the convoy's ask-branch for this
		// repo. If the convoy has no ask-branch (legacy convoys, repos with
		// pr_flow_enabled=0, or non-convoy tasks), baseBranch is "" and we fall
		// back to the repo's default branch.
		baseBranch := ""
		if bounty.ConvoyID > 0 {
			if ab := store.GetConvoyAskBranch(db, bounty.ConvoyID, bounty.TargetRepo); ab != nil {
				baseBranch = ab.AskBranch
			}
		}
		var branchErr error
		branchName, isResume, branchErr = igit.PrepareAgentBranch(ctx, worktreeDir, repoPath, bounty.ID, name, bounty.BranchName, baseBranch)
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

	goalContext, fleetMemoryContext, checkpointContext, seanceContext, inboxContext, injectedMemoryIDs :=
		buildAstromechContext(db, bounty, name, librarianProfile, logger)

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

	notesBlock := buildNotesBlock(db, bounty.ID, logger)

	systemPrompt := AstromechSystemPrompt + directiveSection
	fullPrompt := fmt.Sprintf("%s%s%s%s%s%s%s\n\nYOUR CURRENT DIRECTIVE:\n%s%s",
		systemPrompt, goalContext, fleetMemoryContext, checkpointContext, seanceContext, inboxContext,
		specialCtx, notesBlock, directiveText(bounty.Payload))

	maxTurns := store.GetConfig(db, "max_turns", fmt.Sprintf("%d", defaultMaxTurns))

	// Per-task timeout overrides the fleet default when set.
	sessionTimeout := claude.AstromechTimeoutForAttempt(bounty.InfraFailures)
	if bounty.TaskTimeout > 0 {
		sessionTimeout = time.Duration(bounty.TaskTimeout) * time.Second
	}

	maxTurnsInt, _ := strconv.Atoi(maxTurns)
	logger.Printf("Task %d: using timeout %v (infra_failures=%d)", bounty.ID, sessionTimeout, bounty.InfraFailures)
	logger.Printf("Task %d: running claude CLI (timeout: %v)", bounty.ID, sessionTimeout)

	// Heartbeat goroutine — logs every 2 minutes so fleet.log confirms Claude is alive
	// during long silent runs. Stops as soon as RunCLIStreamingContext returns.
	//
	// AUDIT-105 (Fix #1): the heartbeat also polls IsEstopped every tick. When
	// the operator flips e-stop, the context is cancelled and the in-flight
	// Claude CLI is killed — without this, a 45-minute Claude session kicked
	// off before e-stop would run to completion, burning tokens during an
	// emergency halt. Poll interval is short (5s) so e-stop response is bounded
	// at ~5s regardless of how long Claude has been running.
	// Fix #8e: derive the Claude session ctx from the daemon ctx so SIGINT/
	// e-stop cancellation propagates without relying on the heartbeat poll
	// alone. Pre-fix this fabricated context.Background, leaving Claude
	// sessions deaf to daemon shutdown until the explicit estop poll fired.
	claudeCtx, cancelClaude := context.WithCancel(ctx)
	defer cancelClaude()
	heartbeatDone := make(chan struct{})
	heartbeatStart := time.Now()
	go func() {
		logTicker := time.NewTicker(2 * time.Minute)
		defer logTicker.Stop()
		estopTicker := time.NewTicker(5 * time.Second)
		defer estopTicker.Stop()
		for {
			select {
			case <-heartbeatDone:
				return
			case <-logTicker.C:
				logger.Printf("Task %d: claude still running (%v elapsed)", bounty.ID, time.Since(heartbeatStart).Round(time.Second))
			case <-estopTicker.C:
				if IsEstopped(db) {
					logger.Printf("Task %d: e-stop detected mid-Claude — cancelling context", bounty.ID)
					cancelClaude()
					return
				}
			}
		}
	}()
	// AUDIT-125 (Fix #8d): defer close(heartbeatDone) AFTER the goroutine
	// spawn — so a panic inside RunCLIStreamingContext cannot leak the
	// heartbeat goroutine + its two tickers. The explicit close() call
	// that used to sit post-RunCLIStreaming was removed; the defer is the
	// single signal path. Defer order: this runs BEFORE the enclosing
	// defer cancelClaude() (LIFO), so the goroutine sees heartbeatDone
	// closed, returns, and only then does cancelClaude fire.
	defer close(heartbeatDone)

	// Per-task streaming log — written to fleet-task-<id>.log while Claude runs.
	// The file is removed on completion so it only exists for in-progress tasks.
	// `force tail <id>` tails this file to show live output.
	// AUDIT-126 (Fix #8d): defer Close + Remove immediately so an early
	// return / panic cannot leak the FD or the stale log file.
	taskLogPath := fmt.Sprintf("fleet-task-%d.log", bounty.ID)
	taskLogFile, _ := os.Create(taskLogPath)
	var taskWriter io.Writer = io.Discard
	if taskLogFile != nil {
		// AUDIT-100 (Fix #8d): 0600 so the on-disk stream (which captures
		// Claude output including injected inbox mail and prior-attempt
		// transcripts) is operator-private on multi-user hosts. os.Create
		// uses 0666 & ^umask, which is typically 0644 — too open.
		_ = os.Chmod(taskLogPath, 0600)
		taskWriter = taskLogFile
		defer taskLogFile.Close()
		defer os.Remove(taskLogPath)
	}

	mcpConfig, mcpErr := profile.MCPConfigArg()
	if mcpErr != nil {
		logger.Printf("Task %d: astromech MCP config write failed (%v) — proceeding without --mcp-config", bounty.ID, mcpErr)
	}
	rawOut, err := claude.RunCLIStreamingContext(claudeCtx, fullPrompt,
		profile.AllowedToolsArg(), profile.DisallowedToolsArg(), mcpConfig,
		worktreeDir, maxTurnsInt, sessionTimeout, taskWriter)

	// Heartbeat goroutine is stopped via deferred stopHeartbeat above; no
	// explicit close here any more.

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
			// Use err.Error() for the actual failure reason (e.g. "exit status
			// 1") and append a short stdout preview only when it's likely to
			// carry a real error message. Dumping the full stdout as the error
			// poisons error_log with Claude's stream-of-thought narration when
			// the CLI exits non-zero mid-stream.
			msg = fmt.Sprintf("Claude CLI Err: %s", err.Error())
			if excerpt := extractClaudeErrorExcerpt(outputStr); excerpt != "" {
				msg += " — " + excerpt
			}
		}

		// Rate limit: back off without burning the circuit breaker
		if claude.IsRateLimitError(outputStr) || claude.IsRateLimitError(err.Error()) {
			// AUDIT-096 (Fix #8d): atomic compare-and-swap so two goroutines
			// for the same agent name can't lose an increment. The spin
			// retries until we win the race; without the CAS loop a
			// concurrent Load+Store could drop bumps (e.g., during a
			// scale-up or restart that temporarily has two goroutines for
			// the same agent name).
			rlCount := 0
			for {
				var loaded int
				v, seen := rateLimitRetries.Load(name)
				if seen {
					loaded, _ = v.(int)
				}
				rlCount = loaded
				if rateLimitRetries.CompareAndSwap(name, v, rlCount+1) {
					break
				}
				// Initial Store for the not-seen case.
				if !seen {
					if _, loadedAfter := rateLimitRetries.LoadOrStore(name, rlCount+1); !loadedAfter {
						break
					}
					// Someone else stored first — retry the CAS.
				}
			}
			claude.PersistRateLimitHit(db, name, rlCount+1)
			backoff := RateLimitBackoff(rlCount)
			logger.Printf("Task %d: rate limit detected (hit %d), backing off %v", bounty.ID, rlCount+1, backoff)
			telemetry.EmitEvent(telemetry.EventRateLimited(sessionID, name, bounty.ID, rlCount+1, backoff))
			if err := store.UpdateBountyStatus(db, bounty.ID, "Pending"); err != nil {
				logger.Printf("Task %d: rate-limit requeue to Pending failed (%v); stale-lock detector will recover", bounty.ID, err)
			}
			// AUDIT-107: honour e-stop during the backoff. A blind time.Sleep
			// leaves an emergency halt ineffective for up to 10 minutes while
			// the agent sleeps; this helper short-sleeps and re-checks each
			// iteration so e-stop response time is ≤ 1s.
			if SleepUnlessEstopped(db, backoff) {
				logger.Printf("Task %d: rate-limit backoff interrupted by e-stop", bounty.ID)
			}
			return
		}

		// Auto-shard check: if this is a timeout and the agent has failed repeatedly
		// without making any commits, re-queue as Decompose instead of retrying.
		// Fix #6 (AUDIT-033): the prior gate only tripped on literal timeouts,
		// but "Claude CLI exit 1, no commits" and "Claude exits 0 with zero
		// commits" both look identical from the cost-vector perspective —
		// three cycles of zero commits is three Astromech sessions burned.
		// The `autoShardIfNoCommits` helper inspects CommitsAhead (via `git
		// log base..branch --oneline` which is the cheap equivalent of
		// `CommitsAhead > 0`) and is now also invoked from the zero-commits
		// branch in the non-error path (see `if retryCount >= 2` below, which
		// calls autoShardIfNoCommits with kind="zero-commits" to Decompose
		// shard the task rather than return-for-rework forever).
		if strings.HasPrefix(err.Error(), "claude CLI timed out") && bounty.InfraFailures >= 2 {
			// Fix #6: centralised auto-shard helper so the zero-commits branch
			// and the timeout branch both route through the same decomposition
			// logic. Error propagation of the inner FailBounty is a Fix #8b
			// concern (marked in the helper).
			if autoShardIfNoCommits(ctx, db, bounty, name, sessionID, repoPath, branchName, outputStr, injectedMemoryIDs, "timeout", logger) {
				return
			}
		}

		// Generic infra failure
		logger.Printf("Task %d: infra failure — %s", bounty.ID, msg)
		histID := store.RecordTaskHistory(db, bounty.ID, name, sessionID, outputStr, "Failed")
		store.StampHistoryMemoryIDs(db, histID, injectedMemoryIDs)
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

	processAstromechOutput(ctx, db, name, bounty, sessionID, outputStr, worktreeDir, branchName, repoPath, logger, true, injectedMemoryIDs)
}

// autoShardIfNoCommits fails the current bounty and spawns a Decompose shard
// if the agent's branch has no CommitsAhead vs the base. Returns true iff
// the shard fired (caller should return immediately). kind is a short label
// ("timeout", "zero-commits") interpolated into the audit record so the
// dashboard distinguishes the failure signature.
//
// Fix #6 (AUDIT-033): zero-commit loops now auto-shard to Decompose (prior
// code only sharded on timeouts; zero-commit paths burned tokens forever
// via IncrementRetryCount/ReturnTaskForRework). The helper is called with
// kind="timeout" from the error branch and kind="zero-commits" from the
// non-error retry-path once retry_count >= 2, giving both failure modes
// the same bounded Decompose shard handling.
// Fix #8e: ctx threads from the caller so igit lookups cancel on shutdown.
func autoShardIfNoCommits(ctx context.Context, db *sql.DB, bounty *store.Bounty, name, sessionID, repoPath, branchName, outputStr string, injectedMemoryIDs []int, kind string, logger interface{ Printf(string, ...any) }) bool {
	base := igit.GetDefaultBranch(ctx, repoPath)
	logOut, _ := igit.RunCmd(ctx, repoPath, "log", base+".."+branchName, "--oneline")
	if strings.TrimSpace(logOut) != "" {
		return false
	}
	newID := store.AddBounty(db, bounty.ID, "Decompose", bounty.Payload)
	failMsg := fmt.Sprintf("Auto-sharded after repeated %s failures with no commits — re-queued as Decompose #%d (infra=%d retry=%d)",
		kind, newID, bounty.InfraFailures, bounty.RetryCount)
	shardHistID := store.RecordTaskHistory(db, bounty.ID, name, sessionID, outputStr, "Failed")
	store.StampHistoryMemoryIDs(db, shardHistID, injectedMemoryIDs)
	// Fix #8d (CLAUDE.md "No silent failures"): observe the terminator error.
	// If FailBounty fails the task stays Locked — the operator mail below and
	// the stale-lock detector are the two recovery channels; the log line
	// names both so an on-call can correlate.
	if err := store.FailBounty(db, bounty.ID, failMsg); err != nil {
		logger.Printf("autoShardIfNoCommits #%d: FailBounty failed (%v); operator mail below surfaces the Decompose reshard, stale-lock detector will clear the Locked row", bounty.ID, err)
	}
	telemetry.EmitEvent(telemetry.EventTaskFailed(sessionID, name, bounty.ID, failMsg))
	store.SendMail(db, name, "operator",
		fmt.Sprintf("[AUTO-SHARDED] Task #%d — re-queued as Decompose #%d after repeated %s failures with no progress",
			bounty.ID, newID, kind),
		fmt.Sprintf("Task #%d has %s-failed %d times (%d infra-failures, %d retries) without making any commits on branch %s in repo %s.\n\nThis indicates the task is too large for a single session. It has been automatically re-queued as Decompose #%d so Commander can break it into smaller pieces.\n\nOriginal task:\n%s",
			bounty.ID, kind, bounty.InfraFailures+bounty.RetryCount, bounty.InfraFailures, bounty.RetryCount,
			branchName, bounty.TargetRepo, newID, util.TruncateStr(bounty.Payload, 500)),
		bounty.ID, store.MailTypeAlert)
	return true
}

// processAstromechOutput handles all post-Claude-run logic shared between the daemon
// loop (runAstromechTask) and the foreground runner (RunTaskForeground): checkpoint
// scanning, optional ownership check, output size circuit breaker, signal parsing
// (ESCALATED, SHARD_NEEDED, DONE), commit inference, and history recording.
// checkOwnership should be true for daemon runs to guard against inquisitor races.
// Fix #8e: ctx threads from the caller (daemon ctx or CLI ctx) so subprocess
// invocations from cleanup paths cancel on shutdown.
func processAstromechOutput(
	ctx context.Context,
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
	injectedMemoryIDs []int,
) {
	taskID := bounty.ID
	// recordHist wraps RecordTaskHistory with automatic memory-id stamping so
	// the dashboard can later display EXACTLY which memories were injected
	// for this attempt (rather than re-querying, which would show today's
	// matches instead of what the agent actually saw).
	recordHist := func(output, outcome string) int64 {
		histID := store.RecordTaskHistory(db, taskID, name, sessionID, output, outcome)
		store.StampHistoryMemoryIDs(db, histID, injectedMemoryIDs)
		return histID
	}

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
		// AUDIT-094 (Fix #8d): distinguish a real Exec error (SQLITE_BUSY,
		// schema drift, etc.) from a legitimate rows=0 "ownership lost".
		// Pre-fix, a transient DB flake right after a long Claude session
		// was indistinguishable from the inquisitor reclaiming the row,
		// so perfectly good work was discarded. Post-fix, a driver error
		// logs + returns (leaves the row as-is for the next claim cycle);
		// only an err==nil, rows==0 result routes to the discard path.
		ownerRes, execErr := db.Exec(
			`UPDATE BountyBoard SET status = 'Locked' WHERE id = ? AND owner = ? AND status = 'Locked'`,
			taskID, name,
		)
		if execErr != nil {
			logger.Printf("Task %d: ownership-check UPDATE failed (%v); NOT treating as lost ownership — stale-lock detector will recover", taskID, execErr)
			return
		}
		n, raErr := ownerRes.RowsAffected()
		if raErr != nil {
			logger.Printf("Task %d: ownership-check RowsAffected failed (%v); NOT treating as lost ownership — stale-lock detector will recover", taskID, raErr)
			return
		}
		if n == 0 {
			logger.Printf("Task %d: ownership lost mid-run — recording and discarding result", taskID)
			recordHist(outputStr, "Discarded")
			store.LogAudit(db, name, "ownership-lost", taskID,
				"Claude completed work but ownership changed mid-run — work discarded")
			base := igit.GetDefaultBranch(ctx, repoPath)
			_ = runShortGit(ctx, "-C", worktreeDir, "checkout", "--detach", base)
			_ = runShortGit(ctx, "-C", repoPath, "branch", "-D", branchName)
			return
		}
	}

	// ── Output size circuit breaker ──────────────────────────────────────
	if len(outputStr) > maxOutputBytes {
		logger.Printf("Task %d: output too large (%d bytes) — likely context-blown, re-queuing as Decompose", taskID, len(outputStr))
		recordHist(outputStr[:min(len(outputStr), 4000)]+"...[truncated for history]", "ContextBlown")
		store.StoreFleetMemory(db, bounty.TargetRepo, taskID, "failure",
			fmt.Sprintf("Task produced %d bytes of output (context blown) — was re-queued for decomposition.\nTask: %s",
				len(outputStr), util.TruncateStr(directiveText(bounty.Payload), 300)),
			"", "context-blown, scope-too-large")
		base := igit.GetDefaultBranch(ctx, repoPath)
		_ = runShortGit(ctx, "-C", worktreeDir, "checkout", "--detach", base)
		_ = runShortGit(ctx, "-C", repoPath, "branch", "-D", branchName)
		store.AddBounty(db, taskID, "Decompose", bounty.Payload)
		if err := store.UpdateBountyStatus(db, taskID, "Completed"); err != nil {
			// Decompose shard is already queued; if this status update fails the
			// stale-lock detector will free the row so Medic can retriage on the next cycle.
			logger.Printf("Task %d: mark-Completed after context-blown shard failed (%v); stale-lock detector will recover", taskID, err)
		}
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
		recordHist(outputStr, "Escalated")
		if _, err := CreateEscalation(db, taskID, sev, msg); err != nil {
			// Fix #8a (AUDIT-041) pattern: escalation insert failed. Fall back to FailBounty
			// so the task doesn't sit Escalated with no Escalations row. Operator mail still
			// fires below, so the signal is visible to humans either way.
			logger.Printf("Task %d: CreateEscalation failed (%v); falling back to FailBounty", taskID, err)
			if fbErr := store.FailBounty(db, taskID, fmt.Sprintf("escalation insert failed: %v — original reason: %s", err, msg)); fbErr != nil {
				logger.Printf("Task %d: FailBounty fallback also failed (%v); stale-lock detector will recover", taskID, fbErr)
			}
		}
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
		recordHist(outputStr, "Sharded")
		store.StoreFleetMemory(db, bounty.TargetRepo, taskID, "failure",
			fmt.Sprintf("Task scope was too large for a single session — sharded for re-decomposition.\nTask: %s",
				util.TruncateStr(directiveText(bounty.Payload), 300)),
			"", "scope-too-large, shard-needed")
		base := igit.GetDefaultBranch(ctx, repoPath)
		_ = runShortGit(ctx, "-C", worktreeDir, "checkout", "--detach", base)
		_ = runShortGit(ctx, "-C", repoPath, "branch", "-D", branchName)
		store.AddBounty(db, taskID, "Decompose", bounty.Payload)
		if err := store.UpdateBountyStatus(db, taskID, "Completed"); err != nil {
			// Decompose shard is already queued; if this status update fails the
			// stale-lock detector will free the row so Medic can retriage on the next cycle.
			logger.Printf("Task %d: mark-Completed after [SHARD_NEEDED] shard failed (%v); stale-lock detector will recover", taskID, err)
		}
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
		histID := recordHist(outputStr, "Completed")
		tokIn, tokOut := claude.ParseTokenUsage(outputStr)
		if tokIn > 0 || tokOut > 0 {
			store.UpdateTaskHistoryTokens(db, histID, tokIn, tokOut)
		}
		telemetry.EmitEvent(telemetry.EventTaskDoneSignal(sessionID, name, taskID))
		telemetry.EmitEvent(telemetry.EventTaskCompleted(sessionID, name, taskID))
		if err := store.UpdateBountyStatus(db, taskID, nextStatus); err != nil {
			logger.Printf("Task %d: [DONE] status transition to %s failed (%v); stale-lock detector will recover", taskID, nextStatus, err)
		}
		return
	}

	// ── Commit inference ─────────────────────────────────────────────────

	gitAddOut, _ := combinedShortGit(ctx, "-C", worktreeDir, "add", ".")
	logger.Printf("Task %d: git add output: %s", taskID, strings.TrimSpace(string(gitAddOut)))

	gitStatusOut, _ := combinedShortGit(ctx, "-C", worktreeDir, "status", "--short")
	logger.Printf("Task %d: git status after add: %s", taskID, strings.TrimSpace(string(gitStatusOut)))

	// Truncate payload to 72 chars for the commit subject line
	commitSubject := util.TruncateStr(strings.SplitN(bounty.Payload, "\n", 2)[0], 72)
	commitOut, commitErr := combinedShortGitArgs(ctx, []string{"-C", worktreeDir, "commit", "-m",
		fmt.Sprintf("task(%d): %s", taskID, commitSubject)})
	logger.Printf("Task %d: git commit output: %s", taskID, strings.TrimSpace(string(commitOut)))

	if commitErr != nil {
		// Claude may have already committed — check for commits ahead of the default branch
		base := igit.GetDefaultBranch(ctx, repoPath)
		aheadOut, _ := combinedShortGit(ctx, "-C", worktreeDir, "log", base+"..HEAD", "--oneline")
		logger.Printf("Task %d: commits ahead of base: %s", taskID, strings.TrimSpace(string(aheadOut)))

		if len(strings.TrimSpace(string(aheadOut))) == 0 {
			// Claude finished but made no commits. This is often caused by the host going
			// to sleep mid-run, leaving the agent in a confused state on wake. Re-queue
			// with a note up to MaxRetries so the agent gets a clean attempt; only
			// permanently fail if it keeps producing zero changes across all retries.
			retryCount := store.IncrementRetryCount(db, taskID)
			msg := fmt.Sprintf("Zero file changes (attempt %d/%d) — re-queuing for retry", retryCount, MaxRetries)
			logger.Printf("Task %d: %s", taskID, msg)
			recordHist(outputStr, "ZeroChanges")
			// Fix #6 (AUDIT-033): two successive zero-commit attempts are the
			// non-timeout equivalent of the timeout-based auto-shard gate
			// above. Both mean "the scope is too big for a single session" —
			// the dashboard can't see the difference and Claude's exit status
			// doesn't change the diagnosis. Promote to auto-shard at the same
			// threshold (retryCount >= 2) before burning a third cycle.
			if retryCount >= 2 {
				if autoShardIfNoCommits(ctx, db, bounty, name, sessionID, repoPath, branchName, outputStr, injectedMemoryIDs, "zero-commits", logger) {
					return
				}
			}
			if retryCount >= MaxRetries {
				failMsg := fmt.Sprintf("Git Commit Err: Claude CLI finished but made zero file changes after %d attempts", MaxRetries)
				if err := store.FailBounty(db, taskID, failMsg); err != nil {
					logger.Printf("Task %d: FailBounty after zero-changes retries failed (%v); stale-lock detector will recover", taskID, err)
				}
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

	histID := recordHist(outputStr, "Completed")
	tokIn, tokOut := claude.ParseTokenUsage(outputStr)
	if tokIn > 0 || tokOut > 0 {
		store.UpdateTaskHistoryTokens(db, histID, tokIn, tokOut)
	}
	telemetry.EmitEvent(telemetry.EventTaskCompleted(sessionID, name, taskID))
	nextStatus := nextReviewStatus(db, bounty.ConvoyID)
	logger.Printf("Task %d: SUCCESS, status -> %s", taskID, nextStatus)
	if err := store.UpdateBountyStatus(db, taskID, nextStatus); err != nil {
		logger.Printf("Task %d: success status transition to %s failed (%v); stale-lock detector will recover", taskID, nextStatus, err)
	}
}

// RunTaskForeground claims a specific task by ID and runs it in the foreground,
// streaming Claude output directly to stdout. This is a one-shot mode for
// debugging or manually re-running a specific task.
// Fix #8e: ctx threads from cmd/force/main.go (the CLI command ctx) so a
// stuck `git worktree add` or claude session can be cancelled by SIGINT.
func RunTaskForeground(ctx context.Context, db *sql.DB, taskID int) {
	// D1 T0-1: load Astromech + Librarian profiles for the foreground run.
	profile, profErr := capabilities.LoadProfile("astromech")
	if profErr != nil {
		fmt.Printf("force run: cannot load astromech capability profile: %v\n", profErr)
		os.Exit(1)
	}
	librarianProfile, libErr := capabilities.LoadProfile("librarian")
	if libErr != nil {
		fmt.Printf("force run: cannot load librarian capability profile: %v\n", libErr)
		os.Exit(1)
	}

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

	worktreeDir, wtErr := igit.GetOrCreateAgentWorktree(ctx, db, fgAgent, repoPath)
	if wtErr != nil {
		db.Exec(`UPDATE BountyBoard SET status = 'Pending', owner = '', locked_at = '' WHERE id = ?`, taskID)
		fmt.Printf("Worktree error: %v\n", wtErr)
		os.Exit(1)
	}

	// Foreground runs always start fresh — pass empty existingBranch to ignore any prior branch.
	// Honor the convoy ask-branch if one exists, same as the normal astromech path.
	baseBranch := ""
	if bounty, bErr := store.GetBounty(db, taskID); bErr == nil && bounty.ConvoyID > 0 {
		if ab := store.GetConvoyAskBranch(db, bounty.ConvoyID, bounty.TargetRepo); ab != nil {
			baseBranch = ab.AskBranch
		}
	}
	branchName, _, branchErr := igit.PrepareAgentBranch(ctx, worktreeDir, repoPath, taskID, fgAgent, "", baseBranch)
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
	goalCtx, fleetMemCtx, _, seanceCtx, inboxCtx, injectedMemoryIDs := buildAstromechContext(db, b, fgAgent, librarianProfile, fgLogger)

	directive := LoadDirective("astromech", b.TargetRepo)
	directiveSection := ""
	if directive != "" {
		directiveSection = fmt.Sprintf("\n\n# OPERATOR DIRECTIVE\n%s", directive)
	}

	notesBlock := buildNotesBlock(db, taskID, fgLogger)

	systemPrompt := AstromechSystemPrompt + directiveSection
	fullPrompt := fmt.Sprintf("%s%s%s%s%s\n\nYOUR CURRENT DIRECTIVE:\n%s%s",
		systemPrompt, goalCtx, fleetMemCtx, seanceCtx, inboxCtx, notesBlock, directiveText(b.Payload))

	maxTurns := store.GetConfig(db, "max_turns", fmt.Sprintf("%d", defaultMaxTurns))

	sessionTimeout := claude.AstromechTimeoutForAttempt(b.InfraFailures)
	if b.TaskTimeout > 0 {
		sessionTimeout = time.Duration(b.TaskTimeout) * time.Second
	}

	fgLogger.Printf("Task %d: using timeout %v (infra_failures=%d)", taskID, sessionTimeout, b.InfraFailures)
	fmt.Printf("=== force run: task #%d ===\n", taskID)
	fmt.Printf("Repo:    %s\n", b.TargetRepo)
	fmt.Printf("Branch:  %s\n", branchName)
	fmt.Printf("Dir:     %s\n", worktreeDir)
	fmt.Printf("Timeout: %v\n\n", sessionTimeout)

	sessionID := telemetry.NewSessionID()

	// Fix #8e: derive session ctx from caller ctx (CLI command ctx) so
	// SIGINT cancels the claude subprocess instead of letting it run for
	// the full sessionTimeout. Pre-fix this fabricated context.Background.
	sessionCtx, cancel := context.WithTimeout(ctx, sessionTimeout)
	defer cancel()

	mcpConfig, mcpErr := profile.MCPConfigArg()
	if mcpErr != nil {
		fgLogger.Printf("force run: astromech MCP config write failed (%v) — proceeding without --mcp-config", mcpErr)
	}
	cmdArgs := []string{"-p", fullPrompt, "--dangerously-skip-permissions", "--max-turns", maxTurns}
	if at := profile.AllowedToolsArg(); at != "" {
		cmdArgs = append(cmdArgs, "--allowedTools", at)
	}
	if dt := profile.DisallowedToolsArg(); dt != "" {
		cmdArgs = append(cmdArgs, "--disallowedTools", dt)
	}
	if mcpConfig != "" {
		cmdArgs = append(cmdArgs, "--mcp-config", mcpConfig, "--strict-mcp-config")
	}
	var outputBuf strings.Builder
	cmd := exec.CommandContext(sessionCtx, "claude", cmdArgs...)
	cmd.Dir = worktreeDir
	cmd.Stdout = io.MultiWriter(os.Stdout, &outputBuf)
	cmd.Stderr = os.Stderr
	runErr := cmd.Run()
	outputStr := outputBuf.String()

	if runErr != nil {
		fmt.Printf("\n[force run] claude exited with error: %v\n", runErr)
		fgHistID := store.RecordTaskHistory(db, taskID, fgAgent, sessionID, outputStr, "Failed")
		store.StampHistoryMemoryIDs(db, fgHistID, injectedMemoryIDs)
		db.Exec(`UPDATE BountyBoard SET status = 'Pending', owner = '', locked_at = '' WHERE id = ?`, taskID)
		fmt.Println("[force run] Task returned to Pending.")
		os.Exit(1)
	}

	processAstromechOutput(ctx, db, fgAgent, b, sessionID, outputStr, worktreeDir, branchName, repoPath, fgLogger, false, injectedMemoryIDs)
}
