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
	"force-orchestrator/internal/telemetry"
	"force-orchestrator/internal/util"
)

const auditorTimeout = 25 * time.Minute

// AuditFinding is one issue found by the Auditor agent.
type AuditFinding struct {
	Severity     string `json:"severity"`      // HIGH | MEDIUM | LOW
	Title        string `json:"title"`         // short label, becomes CodeEdit task title
	Repo         string `json:"repo"`          // target repo for the fix task
	Location     string `json:"location"`      // file:line or package path
	Description  string `json:"description"`   // full explanation of the issue
	SuggestedFix string `json:"suggested_fix"` // what the Astromech should do
}

type auditReport struct {
	Summary  string         `json:"summary"`
	Findings []AuditFinding `json:"findings"`
}

const auditorSystemPrompt = `You are a Fleet Auditor — a specialist analysis agent in the Galactic Fleet.
Your mission is to systematically scan the codebase (and external systems) for issues, then produce a structured list of findings.
Each finding you identify will become a discrete CodeEdit task for an Astromech to fix.

# CAPABILITIES (READ-ONLY)
- Read, Glob, Grep: traverse and search the codebase
- Bash: read-only commands (git log, git blame, find, grep, wc — nothing destructive)
- Jira/Confluence/Glean: look up specs, contracts, known issues, runbooks
- SonarQube: inspect existing quality gate failures and security hotspots
- Datadog: search logs and metrics for runtime evidence of bugs

# STRICT RULES
- DO NOT modify any files.
- DO NOT run any commands that change state.
- Only report findings you have evidence for — not speculative ones.
- Each finding must be actionable: an Astromech must be able to fix it with the information you provide.
- If a finding spans multiple repos, split it into one finding per repo.

# OUTPUT FORMAT
Respond ONLY with a JSON object (no markdown, no prose outside the JSON):
{
  "summary": "one-paragraph summary of what you found",
  "findings": [
    {
      "severity": "HIGH|MEDIUM|LOW",
      "title": "Short descriptive title (max 80 chars)",
      "repo": "registered-repo-name",
      "location": "path/to/file.go:42 or package name",
      "description": "Clear explanation of the issue and why it matters",
      "suggested_fix": "Concrete instructions for the Astromech — what to change and how"
    }
  ]
}

If findings is an empty array, that is a valid result meaning no issues were found.
If you cannot complete the audit without human input, emit [ESCALATED:LOW|MEDIUM|HIGH:reason] instead of JSON.`

func SpawnAuditor(db *sql.DB, name string) {
	logger := NewLogger(name)
	logger.Printf("Auditor %s starting up", name)

	for {
		if IsEstopped(db) {
			time.Sleep(5 * time.Second)
			continue
		}

		bounty, claimed := store.ClaimBounty(db, "Audit", name)
		if !claimed {
			time.Sleep(time.Duration(2000+rand.Intn(1000)) * time.Millisecond)
			continue
		}

		runAuditorTask(db, name, bounty, logger)
	}
}

func runAuditorTask(db *sql.DB, name string, bounty *store.Bounty, logger *log.Logger) {
	sessionID := telemetry.NewSessionID()
	logger.Printf("[%s] Claimed Audit #%d: %s", sessionID, bounty.ID, util.TruncateStr(bounty.Payload, 80))
	telemetry.EmitEvent(telemetry.TelemetryEvent{
		SessionID: sessionID, Agent: name, TaskID: bounty.ID,
		EventType: "task_auditing",
		Payload:   map[string]any{"payload_preview": util.TruncateStr(bounty.Payload, 120)},
	})

	// Run from repo dir if scoped to one repo; otherwise inherit CWD.
	runDir := ""
	if bounty.TargetRepo != "" {
		runDir = store.GetRepoPath(db, bounty.TargetRepo)
	}

	// Build a list of registered repos for the agent to reference.
	repoCtx := buildAuditRepoContext(db, bounty.TargetRepo)

	directive := LoadDirective("auditor", bounty.TargetRepo)
	directiveSection := ""
	if directive != "" {
		directiveSection = fmt.Sprintf("\n\n# OPERATOR DIRECTIVE\n%s", directive)
	}

	inboxCtx := buildInboxContext(db, name, "auditor", bounty.ID, logger)

	fullPrompt := fmt.Sprintf("%s%s%s%s\n\nAUDIT TASK:\n%s",
		auditorSystemPrompt, directiveSection, repoCtx, inboxCtx, bounty.Payload)

	logger.Printf("Task %d: starting audit (timeout: %v)", bounty.ID, auditorTimeout)

	rawOut, err := claude.RunCLI(fullPrompt, claude.InvestigateTools, runDir, 40, auditorTimeout)
	outputStr := strings.TrimSpace(rawOut)
	tokIn, tokOut := claude.ParseTokenUsage(outputStr)

	// Check for escalation before error handling.
	if sev, msg, ok := ParseEscalationSignal(outputStr); ok {
		logger.Printf("Task %d: escalated (%s): %s", bounty.ID, sev, msg)
		histID := store.RecordTaskHistory(db, bounty.ID, name, sessionID, util.TruncateStr(outputStr, 4000), "Escalated")
		if tokIn > 0 || tokOut > 0 {
			store.UpdateTaskHistoryTokens(db, histID, tokIn, tokOut)
		}
		CreateEscalation(db, bounty.ID, sev, msg)
		telemetry.EmitEvent(telemetry.EventTaskEscalated(sessionID, name, bounty.ID, sev, msg))
		store.LogAudit(db, name, "audit-escalated", bounty.ID, msg)
		return
	}

	if err != nil {
		msg := fmt.Sprintf("Auditor CLI error: %v", err)
		logger.Printf("Task %d FAILED: %s", bounty.ID, msg)
		histID := store.RecordTaskHistory(db, bounty.ID, name, sessionID, util.TruncateStr(outputStr, 4000), "Failed")
		if tokIn > 0 || tokOut > 0 {
			store.UpdateTaskHistoryTokens(db, histID, tokIn, tokOut)
		}
		handleInfraFailure(db, name, "auditor", bounty, sessionID, msg, "Pending", false, logger)
		return
	}

	// Parse findings JSON.
	jsonStr := claude.ExtractJSON(outputStr)
	if jsonStr == "" {
		jsonStr = outputStr
	}
	var report auditReport
	if jsonErr := json.Unmarshal([]byte(jsonStr), &report); jsonErr != nil {
		// Retry: treat unparseable output as an infra failure so it gets re-queued.
		msg := fmt.Sprintf("Auditor JSON parse error: %v (output: %s)", jsonErr, util.TruncateStr(outputStr, 200))
		logger.Printf("Task %d: %s", bounty.ID, msg)
		histID := store.RecordTaskHistory(db, bounty.ID, name, sessionID, util.TruncateStr(outputStr, 4000), "Failed")
		if tokIn > 0 || tokOut > 0 {
			store.UpdateTaskHistoryTokens(db, histID, tokIn, tokOut)
		}
		handleInfraFailure(db, name, "auditor-parse", bounty, sessionID, msg, "Pending", false, logger)
		return
	}

	histID := store.RecordTaskHistory(db, bounty.ID, name, sessionID, util.TruncateStr(outputStr, 4000), "Completed")
	if tokIn > 0 || tokOut > 0 {
		store.UpdateTaskHistoryTokens(db, histID, tokIn, tokOut)
	}

	// Spawn a convoy of Planned CodeEdit tasks — one per finding.
	nFindings := len(report.Findings)
	if nFindings == 0 {
		logger.Printf("Task %d: audit complete — no findings", bounty.ID)
		store.UpdateBountyStatus(db, bounty.ID, "Completed")
		store.SendMail(db, name, "operator",
			fmt.Sprintf("[Audit Complete] #%d — No Issues Found", bounty.ID),
			fmt.Sprintf("Audit of task #%d completed with no findings.\n\nSummary: %s\n\nTask: %s",
				bounty.ID, report.Summary, bounty.Payload),
			bounty.ID, store.MailTypeInfo)
		store.LogAudit(db, name, "audit-complete", bounty.ID, "no findings")
		return
	}

	convoyName := fmt.Sprintf("Audit #%d fixes (%d findings)", bounty.ID, nFindings)
	convoyID, convoyErr := store.CreateConvoy(db, convoyName)
	if convoyErr != nil {
		msg := fmt.Sprintf("Failed to create audit convoy: %v", convoyErr)
		logger.Printf("Task %d: %s", bounty.ID, msg)
		store.FailBounty(db, bounty.ID, msg)
		return
	}

	var queued []string
	for i, f := range report.Findings {
		if strings.TrimSpace(f.Title) == "" || strings.TrimSpace(f.Repo) == "" {
			logger.Printf("Task %d: skipping finding %d (missing title or repo)", bounty.ID, i+1)
			continue
		}
		payload := buildFindingPayload(bounty.ID, i+1, f)
		_, err := store.AddConvoyTask(db, bounty.ID, f.Repo, payload, convoyID, bounty.Priority, "Planned")
		if err != nil {
			logger.Printf("Task %d: failed to queue finding %d: %v", bounty.ID, i+1, err)
			continue
		}
		queued = append(queued, fmt.Sprintf("[%s] %s — %s", f.Severity, f.Title, f.Location))
	}

	store.UpdateBountyStatus(db, bounty.ID, "Completed")

	// Send summary mail to operator.
	findingList := strings.Join(queued, "\n")
	mailBody := fmt.Sprintf(
		"Audit #%d complete. %d finding(s) queued as Planned tasks in Convoy #%d.\n"+
			"Approve the convoy to activate fixes: force convoy approve %d\n"+
			"Or use the dashboard Convoys tab → Activate Planned Tasks.\n\n"+
			"Summary: %s\n\nFindings:\n%s",
		bounty.ID, len(queued), convoyID, convoyID, report.Summary, findingList)
	store.SendMail(db, name, "operator",
		fmt.Sprintf("[Audit Complete] #%d — %d finding(s) queued in Convoy #%d", bounty.ID, len(queued), convoyID),
		mailBody, bounty.ID, store.MailTypeInfo)

	telemetry.EmitEvent(telemetry.TelemetryEvent{
		SessionID: sessionID, Agent: name, TaskID: bounty.ID,
		EventType: "task_completed",
		Payload:   map[string]any{"findings": len(queued), "convoy_id": convoyID, "tokens_in": tokIn, "tokens_out": tokOut},
	})
	store.LogAudit(db, name, "audit-complete", bounty.ID,
		fmt.Sprintf("%d finding(s) → convoy #%d", len(queued), convoyID))
	logger.Printf("Task %d: audit complete — %d finding(s) queued in convoy #%d", bounty.ID, len(queued), convoyID)
}

// buildFindingPayload formats one audit finding as a CodeEdit task payload.
func buildFindingPayload(auditTaskID, num int, f AuditFinding) string {
	parts := []string{
		fmt.Sprintf("[AUDIT FINDING from task #%d — Finding %d of type %s]", auditTaskID, num, f.Severity),
		fmt.Sprintf("Title: %s", f.Title),
	}
	if f.Location != "" {
		parts = append(parts, fmt.Sprintf("Location: %s", f.Location))
	}
	parts = append(parts, "", f.Description, "", "Suggested fix:", f.SuggestedFix)
	return strings.Join(parts, "\n")
}

// buildAuditRepoContext returns a context string listing registered repositories.
func buildAuditRepoContext(db *sql.DB, scopedRepo string) string {
	if scopedRepo != "" {
		path := store.GetRepoPath(db, scopedRepo)
		return fmt.Sprintf("\n\n# SCOPE\nFocus your audit on the '%s' repository at: %s\nReport findings using repo: \"%s\"",
			scopedRepo, path, scopedRepo)
	}
	rows, err := db.Query(`SELECT name, local_path, description FROM Repositories ORDER BY name`)
	if err != nil {
		return ""
	}
	defer rows.Close()
	var entries []string
	for rows.Next() {
		var name, path, desc string
		rows.Scan(&name, &path, &desc)
		entries = append(entries, fmt.Sprintf("- %s (%s): %s", name, path, desc))
	}
	if len(entries) == 0 {
		return ""
	}
	return fmt.Sprintf("\n\n# REGISTERED REPOSITORIES\nUse exact repo names in your findings JSON:\n%s",
		strings.Join(entries, "\n"))
}
