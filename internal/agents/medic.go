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
	igit "force-orchestrator/internal/git"
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

Based on this evidence, choose ONE of three actions:

requeue — The task is valid but needs clearer guidance. Reset for another attempt.
  Use when: failures were due to missing context, unclear requirements, or a specific
  technical obstacle that can be overcome with explicit additional guidance.
  Provide concrete guidance that will prevent the same mistake.

shard — The task is too large for a single agent. Break into focused sub-tasks.
  Use when: agents kept getting lost, timing out, or producing incomplete work because
  the scope was too broad. Provide 2-5 atomic, independently completable sub-tasks.

escalate — The task requires human judgment or has a fundamental blocker.
  Use when: there is an architectural ambiguity, missing external dependency, security concern,
  or the failure reveals something a coding agent cannot resolve autonomously.

Bias toward requeue or shard — escalate only when an agent genuinely cannot proceed without a human decision.

Respond ONLY with valid JSON (no markdown, no preamble):
{
  "decision": "requeue|shard|escalate",
  "reason": "1-2 sentences: root cause of the failures",
  "guidance": "specific corrective guidance for the next attempt (requeue only; empty string otherwise)",
  "shards": [{"task": "one-sentence description", "repo": "repo-name"}],
  "escalation": "clear root-cause and what human decision is needed (escalate only; empty string otherwise)"
}`

type medicPayload struct {
	FailureType string `json:"failure_type"`
	Error       string `json:"error"`
}

type medicDecision struct {
	Decision  string          `json:"decision"`
	Reason    string          `json:"reason"`
	Guidance  string          `json:"guidance"`
	Shards    []medicShard    `json:"shards"`
	Escalation string         `json:"escalation"`
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

	// Fetch the last diff if a branch exists.
	var diff string
	if parent.BranchName != "" {
		repoPath := store.GetRepoPath(db, parent.TargetRepo)
		if repoPath != "" {
			diff = util.TruncateStr(igit.GetDiff(repoPath, parent.BranchName), 4000)
		}
	}

	userPrompt := buildMedicPrompt(parent, mp, history, diff)

	rawOut, claudeErr := claude.AskClaudeCLI(medicSystemPrompt, userPrompt, "", 1)
	if claudeErr != nil {
		// Claude itself failed — escalate directly without looping.
		logger.Printf("Medic #%d: Claude failed (%v) — escalating task #%d to operator", bounty.ID, claudeErr, parent.ID)
		CreateEscalation(db, parent.ID, store.SeverityMedium,
			fmt.Sprintf("Medic could not analyze failure (Claude error: %v). Original error: %s", claudeErr, mp.Error))
		store.SendMail(db, agentName, "operator",
			fmt.Sprintf("[ESCALATED] Task #%d requires attention — %s", parent.ID, parent.TargetRepo),
			fmt.Sprintf("Task #%d permanently failed and the Medic could not analyze it (Claude unavailable).\n\nRepo: %s\nOriginal error: %s\n\nTask:\n%s",
				parent.ID, parent.TargetRepo, mp.Error, util.TruncateStr(parent.Payload, 500)),
			parent.ID, store.MailTypeAlert)
		store.UpdateBountyStatus(db, bounty.ID, "Completed")
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
	default: // escalate
		applyMedicEscalate(db, agentName, bounty, parent, decision, logger)
	}

	store.UpdateBountyStatus(db, bounty.ID, "Completed")
	store.LogAudit(db, agentName, "medic-triage", parent.ID,
		fmt.Sprintf("decision=%s reason=%s", decision.Decision, decision.Reason))
}

func applyMedicRequeue(db *sql.DB, agentName string, bounty, parent *store.Bounty, d medicDecision, logger *log.Logger) {
	store.ResetTask(db, parent.ID)
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

	// Cancel the original task.
	db.Exec(`UPDATE BountyBoard SET status='Cancelled', error_log=? WHERE id=?`,
		fmt.Sprintf("Medic sharded into %d sub-tasks: %s", len(d.Shards), d.Reason), parent.ID)

	// Create sub-tasks in the same convoy.
	var ids []int
	for _, s := range d.Shards {
		repo := s.Repo
		if repo == "" {
			repo = parent.TargetRepo
		}
		id, _ := store.AddConvoyTask(db, parent.ID, repo, s.Task, parent.ConvoyID, parent.Priority, "Pending")
		ids = append(ids, id)
	}
	logger.Printf("Medic: sharded task #%d into %d sub-tasks %v", parent.ID, len(ids), ids)
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
