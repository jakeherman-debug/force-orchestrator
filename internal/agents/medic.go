package agents

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"math/rand"
	"strings"
	"time"

	"force-orchestrator/internal/claude"
	"force-orchestrator/internal/store"
	"force-orchestrator/internal/util"
)

const medicSystemPrompt = `You are the Fleet Medic — an autonomous triage agent for a fleet of AI coding agents.

A coding task has permanently failed after exhausting all retry attempts. Your job is to diagnose the root cause and decide what to do next.

You will receive:
- The original task payload
- All attempt history with outcomes and council/captain feedback
- The error that triggered permanent failure
- The last diff the agent produced (if any)

Based on this evidence, choose ONE of four actions:

requeue — The task is valid but needs clearer guidance. Reset for another attempt.
  Use when: failures were due to missing context, unclear requirements, or a specific
  technical obstacle that can be overcome with explicit additional guidance.
  Provide concrete guidance that will prevent the same mistake.

shard — The task is too large for a single agent. Break into focused sub-tasks.
  Use when: agents kept getting lost, timing out, or producing incomplete work because
  the scope was too broad. Provide 2-5 atomic, independently completable sub-tasks.

cleanup — Worktree / branch contamination is forcing every attempt to produce the
  same wrong diff. Use when: 2+ different astromechs produced nearly-identical
  diffs modifying files OUTSIDE the stated task scope (a clear structural signal,
  not an LLM quality issue). The fleet will reset the affected worktrees to the
  target branch tip, wipe untracked files, and re-queue the parent task with a
  fresh branch. Populate "cleanup_target_branch" with the branch the worktrees
  should reset to (usually the convoy's ask-branch, e.g. "force/ask-37-...").
  Populate "cleanup_agents" ONLY if you want to limit the reset to specific
  astromech names from the attempt history; leave empty to wipe all worktrees
  for the repo (safer default).

escalate — The task requires human judgment or has a fundamental blocker.
  Use when: there is an architectural ambiguity, missing external dependency, security concern,
  or the failure reveals something a coding agent cannot resolve autonomously.
  DO NOT escalate for worktree hygiene, branch contamination, uncommitted files,
  "run git checkout .", "run git clean" — those are cleanup, not escalations.
  DO NOT escalate when the work is already present in the base branch — the
  fleet detects that separately and auto-completes.

Bias HEAVILY toward requeue, shard, or cleanup. Escalation means the operator
must do something a coding agent cannot. If you can describe the fix as a
sequence of commands, it is NOT an escalation — pick cleanup or requeue.

Respond ONLY with valid JSON (no markdown, no preamble):
{
  "decision": "requeue|shard|cleanup|escalate",
  "reason": "1-2 sentences: root cause of the failures",
  "guidance": "specific corrective guidance for the next attempt (requeue only; empty string otherwise)",
  "shards": [{"task": "one-sentence description", "repo": "repo-name"}],
  "cleanup_target_branch": "branch to reset the worktrees to (cleanup only; empty string otherwise)",
  "cleanup_agents": ["astromech-name-1", "..."],
  "escalation": "clear root-cause and what human decision is needed (escalate only; empty string otherwise)"
}`

type medicPayload struct {
	FailureType string `json:"failure_type"`
	Error       string `json:"error"`
}

type medicDecision struct {
	Decision            string       `json:"decision"`
	Reason              string       `json:"reason"`
	Guidance            string       `json:"guidance"`
	Shards              []medicShard `json:"shards"`
	CleanupTargetBranch string       `json:"cleanup_target_branch"`
	CleanupAgents       []string     `json:"cleanup_agents"`
	Escalation          string       `json:"escalation"`
}

type medicShard struct {
	Task string `json:"task"`
	Repo string `json:"repo"`
}

func SpawnMedic(db *sql.DB, name string) {
	logger := NewLogger(name)
	logger.Printf("Medic %s coming online", name)

	for {
		if IsEstopped(db) {
			time.Sleep(5 * time.Second)
			continue
		}

		if bounty, claimed := store.ClaimBounty(db, "MedicReview", name); claimed {
			runMedicTask(db, name, bounty, logger)
			continue
		}
		if bounty, claimed := store.ClaimBounty(db, "CIFailureTriage", name); claimed {
			runMedicCITriage(db, name, bounty, logger)
			continue
		}

		time.Sleep(time.Duration(3000+rand.Intn(1000)) * time.Millisecond)
	}
}

func runMedicTask(db *sql.DB, agentName string, bounty *store.Bounty, logger *log.Logger) {
	logger.Printf("Medic claimed MedicReview #%d (parent task #%d)", bounty.ID, bounty.ParentID)

	// Parse the failure context queued by permanentInfraFail / council / captain.
	var mp medicPayload
	json.Unmarshal([]byte(bounty.Payload), &mp)

	// Load the original failed task.
	parent, err := store.GetBounty(db, bounty.ParentID)
	if err != nil || parent == nil {
		logger.Printf("Medic #%d: cannot load parent task #%d — escalating", bounty.ID, bounty.ParentID)
		store.UpdateBountyStatus(db, bounty.ID, "Completed")
		return
	}

	// Collect all attempt history for the original task.
	history := store.GetTaskHistory(db, parent.ID)

	// Fetch the last diff if a branch exists. Use ask-branch baseline when
	// present so Medic's LLM analysis doesn't see main's phantom drift as
	// "the agent's work."
	var diff string
	if parent.BranchName != "" {
		repoPath := store.GetRepoPath(db, parent.TargetRepo)
		if repoPath != "" {
			diff = util.TruncateStr(reviewDiff(db, repoPath, parent), 4000)
		}
	}

	// Pre-LLM fast path: if the parent task's work is already delivered —
	// either its branch has no net diff against main, or a sibling merge
	// landed the change — auto-complete instead of paying for LLM reasoning
	// AND instead of escalating to the operator. This catches the "Medic
	// correctly identifies the work is already done but escalates anyway"
	// pattern (task 470 in production: directive fully present on ask-branch
	// baseline, all six clauses map to existing merged code).
	if autoCompletedMedicTask(db, agentName, bounty, parent, logger) {
		return
	}

	userPrompt := buildMedicPrompt(parent, mp, history, diff)

	rawOut, claudeErr := claude.AskClaudeCLI(medicSystemPrompt, userPrompt, "", 1)
	if claudeErr != nil {
		// Claude CLI flakes are infra-class failures, not task-analysis
		// failures. Route through handleInfraFailure (same backoff + retry
		// treatment as every other agent's Claude call) so transient
		// unavailability doesn't produce a premature operator escalation.
		// Only after MaxInfraFailures consecutive flakes does this path
		// escalate to the operator — and by then, something is genuinely
		// wrong with the Claude CLI itself.
		logger.Printf("Medic #%d: Claude infra failure (%v) — retrying via handleInfraFailure", bounty.ID, claudeErr)
		handleInfraFailure(db, agentName, "medic-analysis", bounty, "",
			fmt.Sprintf("Claude CLI Err: %v", claudeErr), "Pending", false, logger)
		return
	}

	jsonStr := claude.ExtractJSON(rawOut)
	var decision medicDecision
	if err := json.Unmarshal([]byte(jsonStr), &decision); err != nil {
		logger.Printf("Medic #%d: JSON parse error (%v) — escalating", bounty.ID, err)
		decision = medicDecision{
			Decision:   "escalate",
			Reason:     "Medic could not parse its own analysis.",
			Escalation: fmt.Sprintf("Auto-escalated: Medic analysis unparseable. Original error: %s", mp.Error),
		}
	}

	logger.Printf("Medic #%d: decision=%s reason=%s", bounty.ID, decision.Decision, decision.Reason)

	switch decision.Decision {
	case "requeue":
		applyMedicRequeue(db, agentName, bounty, parent, decision, logger)
	case "shard":
		applyMedicShard(db, agentName, bounty, parent, decision, logger)
	case "cleanup":
		applyMedicCleanup(db, agentName, bounty, parent, decision, logger)
	default: // escalate
		applyMedicEscalate(db, agentName, bounty, parent, decision, logger)
	}

	store.UpdateBountyStatus(db, bounty.ID, "Completed")
	store.LogAudit(db, agentName, "medic-triage", parent.ID,
		fmt.Sprintf("decision=%s reason=%s", decision.Decision, decision.Reason))
}

// autoCompletedMedicTask handles the "work is already delivered" short-circuit.
// Returns true when Medic's analysis isn't needed — the task has no net diff
// (work landed via a sibling / the ConvoyReview loop / manual intervention)
// so the correct action is to mark the parent Completed, resolve any Open
// escalations it triggered, unblock dependents, and move on. Returns false
// when the task still has real work to debug (normal Medic flow continues).
func autoCompletedMedicTask(db *sql.DB, agentName string, medicBounty, parent *store.Bounty, logger *log.Logger) bool {
	if parent.BranchName == "" {
		return false
	}
	repoPath := store.GetRepoPath(db, parent.TargetRepo)
	if repoPath == "" {
		return false
	}
	// Two orthogonal signals, both meaning "nothing to land":
	//   1. No diff vs its base (ask-branch tip or main) — whatever the agent
	//      produced is a net-zero change against the work that's already been
	//      integrated into the convoy's ask-branch (or main).
	//   2. No commits ahead of that base — the branch never diverged, or the
	//      change already rode into the ask-branch via a sibling merge.
	// Using the ask-branch baseline (via reviewDiff/reviewCommitsAhead) rather
	// than main is critical: main drifts independently, so "no diff vs main"
	// would incorrectly fail for convoys whose ask-branch hasn't been rebased
	// in hours.
	if reviewDiff(db, repoPath, parent) != "" {
		return false
	}
	if reviewCommitsAhead(db, repoPath, parent) != "" {
		return false
	}

	logger.Printf("Medic #%d: parent task #%d has no diff and no unique commits — auto-completing (work already delivered)",
		medicBounty.ID, parent.ID)

	store.UpdateBountyStatus(db, parent.ID, "Completed")
	store.RecordTaskHistory(db, parent.ID, agentName, "medic-auto",
		"Medic auto-completed: no diff vs main, no unique commits — work already delivered.",
		"Completed")
	store.LogAudit(db, agentName, "medic-auto-complete", parent.ID,
		"no diff and no unique commits — parent work already in main/ask-branch")
	store.UnblockDependentsOf(db, parent.ID)
	store.AutoRecoverConvoy(db, parent.ConvoyID, logger)

	// Resolve any Open escalations the parent triggered. They were valid
	// concerns at the time, but the underlying problem is gone.
	if _, err := db.Exec(`UPDATE Escalations
		SET status = 'Resolved', acknowledged_at = datetime('now')
		WHERE task_id = ? AND status = 'Open'`, parent.ID); err != nil {
		logger.Printf("Medic #%d: escalation cleanup query failed: %v", medicBounty.ID, err)
	}

	store.SendMail(db, agentName, "operator",
		fmt.Sprintf("[AUTO-COMPLETED] Task #%d — work already delivered", parent.ID),
		fmt.Sprintf("Medic found no net diff for task #%d on %s — the stated work is already present in the base branch. Marked Completed without LLM analysis.",
			parent.ID, parent.TargetRepo),
		parent.ID, store.MailTypeInfo)

	store.UpdateBountyStatus(db, medicBounty.ID, "Completed")
	return true
}

func applyMedicRequeue(db *sql.DB, agentName string, bounty, parent *store.Bounty, d medicDecision, logger *log.Logger) {
	store.ResetTaskFull(db, parent.ID)
	store.SendMail(db, agentName, "astromech",
		fmt.Sprintf("[MEDIC GUIDANCE] Task #%d — requeued with updated guidance", parent.ID),
		fmt.Sprintf("The Fleet Medic has analyzed this task's failure history and requeued it.\n\nRoot cause: %s\n\nGuidance for your next attempt:\n%s",
			d.Reason, d.Guidance),
		parent.ID, store.MailTypeFeedback)
	logger.Printf("Medic: requeued task #%d — %s", parent.ID, d.Reason)
}

func applyMedicShard(db *sql.DB, agentName string, bounty, parent *store.Bounty, d medicDecision, logger *log.Logger) {
	if len(d.Shards) == 0 {
		// Malformed shard response — fall back to requeue.
		logger.Printf("Medic: shard response had no tasks — falling back to requeue for task #%d", parent.ID)
		applyMedicRequeue(db, agentName, bounty, parent, medicDecision{
			Reason:   d.Reason,
			Guidance: "Task was too large; please focus on the core objective and leave peripheral concerns for follow-up tasks.",
		}, logger)
		return
	}

	// Cancel + insert shards atomically. Without a tx, a partial-shard state
	// (parent Cancelled, only some shards inserted) cannot be recovered by
	// retry — the parent task is already terminal and a re-run would create
	// duplicate shards alongside the existing incomplete set.
	tx, err := db.Begin()
	if err != nil {
		logger.Printf("Medic: begin shard tx for task #%d failed: %v", parent.ID, err)
		return
	}
	defer tx.Rollback() //nolint:errcheck

	if _, err := tx.Exec(`UPDATE BountyBoard SET status='Cancelled', error_log=? WHERE id=?`,
		fmt.Sprintf("Medic sharded into %d sub-tasks: %s", len(d.Shards), d.Reason), parent.ID); err != nil {
		logger.Printf("Medic: cancel parent #%d failed: %v", parent.ID, err)
		return
	}

	var ids []int
	for _, s := range d.Shards {
		repo := s.Repo
		if repo == "" {
			repo = parent.TargetRepo
		}
		id, addErr := store.AddConvoyTaskTx(tx, parent.ID, repo, s.Task, parent.ConvoyID, parent.Priority, "Pending")
		if addErr != nil {
			logger.Printf("Medic: add shard failed for task #%d (repo=%s): %v — rolling back", parent.ID, repo, addErr)
			return
		}
		ids = append(ids, id)
	}

	if err := tx.Commit(); err != nil {
		logger.Printf("Medic: commit shard for task #%d failed: %v", parent.ID, err)
		return
	}
	logger.Printf("Medic: sharded task #%d into %d sub-tasks %v", parent.ID, len(ids), ids)
}

// applyMedicCleanup queues a WorktreeReset infrastructure task for Pilot to
// execute, rather than punting the git commands to the operator. The parent
// task is NOT directly requeued here — WorktreeReset itself resets the parent
// to Pending after the wipe lands, so the cleanup-and-retry sequence is
// atomic from the operator's perspective (one Medic decision, one recovery).
//
// If Medic's LLM omits cleanup_target_branch, we infer from the convoy's
// ask-branch — that's the right reset target in >95% of real cases (the
// contamination usually IS the ask-branch's pre-merged state that agents
// keep re-committing). Only fall back to escalation when we genuinely can't
// figure out where to reset to.
func applyMedicCleanup(db *sql.DB, agentName string, bounty, parent *store.Bounty, d medicDecision, logger *log.Logger) {
	target := strings.TrimSpace(d.CleanupTargetBranch)
	if target == "" && parent.ConvoyID > 0 {
		if ab := store.GetConvoyAskBranch(db, parent.ConvoyID, parent.TargetRepo); ab != nil {
			target = ab.AskBranch
		}
	}
	if target == "" {
		// No convoy, no ask-branch, and no explicit target — fall back to the
		// repo's default branch. Safer than escalating; worst case the worktree
		// gets reset to main, which is a recoverable state.
		repo := store.GetRepo(db, parent.TargetRepo)
		if repo != nil {
			target = repo.DefaultBranch
		}
	}
	if target == "" {
		logger.Printf("Medic #%d: cleanup decision for task %d but no reset target available — falling back to escalate",
			bounty.ID, parent.ID)
		applyMedicEscalate(db, agentName, bounty, parent, medicDecision{
			Decision:   "escalate",
			Reason:     d.Reason,
			Escalation: "Medic detected contamination but no reset target could be resolved (no convoy ask-branch, no default branch on repo). Manual review needed: " + d.Reason,
		}, logger)
		return
	}

	payload := worktreeResetPayload{
		ParentTaskID: parent.ID,
		Repo:         parent.TargetRepo,
		TargetBranch: target,
		Agents:       d.CleanupAgents,
		Reason:       d.Reason,
	}
	resetID, err := QueueWorktreeReset(db, payload)
	if err != nil {
		logger.Printf("Medic #%d: failed to queue WorktreeReset for task %d: %v — falling back to escalate", bounty.ID, parent.ID, err)
		applyMedicEscalate(db, agentName, bounty, parent, medicDecision{
			Decision:   "escalate",
			Reason:     d.Reason,
			Escalation: fmt.Sprintf("Medic detected contamination on task #%d but could not queue cleanup: %v", parent.ID, err),
		}, logger)
		return
	}

	logger.Printf("Medic: queued WorktreeReset #%d for task #%d (repo %s, reset to %s) — %s",
		resetID, parent.ID, parent.TargetRepo, target, d.Reason)
	store.SendMail(db, agentName, "operator",
		fmt.Sprintf("[AUTO-CLEANUP] Task #%d — worktree contamination detected", parent.ID),
		fmt.Sprintf("Medic detected worktree contamination on task #%d and queued WorktreeReset #%d to recover.\n\nRoot cause: %s\n\nReset target: %s\n\nThe task will be re-queued once the wipe completes. No operator action needed.",
			parent.ID, resetID, d.Reason, target),
		parent.ID, store.MailTypeInfo)
}

func applyMedicEscalate(db *sql.DB, agentName string, bounty, parent *store.Bounty, d medicDecision, logger *log.Logger) {
	msg := d.Escalation
	if msg == "" {
		msg = d.Reason
	}
	CreateEscalation(db, parent.ID, store.SeverityMedium, msg)
	store.SendMail(db, agentName, "operator",
		fmt.Sprintf("[ESCALATED] Task #%d requires a decision — %s", parent.ID, parent.TargetRepo),
		fmt.Sprintf("Task #%d has been analyzed by the Fleet Medic and requires your attention.\n\nRepo: %s\nRoot cause: %s\n\nRecommendation:\n%s\n\nTask:\n%s",
			parent.ID, parent.TargetRepo, d.Reason, msg, util.TruncateStr(parent.Payload, 500)),
		parent.ID, store.MailTypeAlert)
	logger.Printf("Medic: escalated task #%d — %s", parent.ID, msg)
}

func buildMedicPrompt(parent *store.Bounty, mp medicPayload, history []store.TaskHistoryEntry, diff string) string {
	var sb strings.Builder

	sb.WriteString("ORIGINAL TASK:\n")
	sb.WriteString(util.TruncateStr(parent.Payload, 800))
	sb.WriteString("\n\nFAILURE TYPE: ")
	sb.WriteString(mp.FailureType)
	sb.WriteString("\nFINAL ERROR: ")
	sb.WriteString(mp.Error)

	if len(history) > 0 {
		sb.WriteString("\n\nATTEMPT HISTORY:\n")
		for _, h := range history {
			sb.WriteString(fmt.Sprintf("Attempt %d (%s, outcome=%s):\n", h.Attempt, h.Agent, h.Outcome))
			// Extract just the council/captain feedback lines from the full Claude output
			// to keep the prompt focused on rejection reasons rather than code dumps.
			feedback := extractFeedbackLines(h.ClaudeOutput)
			if feedback != "" {
				sb.WriteString(feedback)
			}
			sb.WriteString("\n")
		}
	}

	if diff != "" {
		sb.WriteString("\nLAST DIFF (truncated):\n")
		sb.WriteString(diff)
	}

	return sb.String()
}

// extractFeedbackLines extracts council/captain rejection feedback from a full Claude output blob.
// Looks for lines containing "feedback", "rejected", or "reason" (case-insensitive) to keep
// the Medic's prompt focused without dumping entire diffs.
func extractFeedbackLines(output string) string {
	var lines []string
	for _, line := range strings.Split(output, "\n") {
		lower := strings.ToLower(line)
		if strings.Contains(lower, "feedback") ||
			strings.Contains(lower, "rejected") ||
			strings.Contains(lower, "reason") ||
			strings.Contains(lower, "approval") ||
			strings.Contains(lower, "approved") {
			lines = append(lines, strings.TrimSpace(line))
		}
	}
	if len(lines) == 0 {
		return util.TruncateStr(output, 300)
	}
	return strings.Join(lines, "\n")
}
