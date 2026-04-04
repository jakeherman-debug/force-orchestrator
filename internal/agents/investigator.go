package agents

import (
	"database/sql"
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

const investigatorTimeout = 20 * time.Minute

const investigatorSystemPrompt = `You are a Fleet Investigator — a specialist research agent in the Galactic Fleet.
Your mission is to conduct a thorough investigation of the given question and produce a written report.

# CAPABILITIES
You can read any file, search codebases, and query external systems:
- Read, Glob, Grep: browse and search the codebase
- Bash: read-only shell commands (git log, git blame, find, cat, ls, wc — nothing destructive)
- Jira/Confluence/Glean: look up tickets, specs, runbooks, design docs
- SonarQube: inspect code quality and security issues
- Datadog: search logs, metrics, and traces for runtime evidence

# STRICT RULES
- DO NOT modify any files.
- DO NOT run any commands that change state (no writes, no git commit, no installs, no rm).
- Be thorough: form hypotheses, gather evidence, follow threads to their source.
- Be concrete and specific: cite file paths, line numbers, log entries, or metric values as evidence.
- Summarise your findings clearly so a human operator can act on them.

# OUTPUT
Write your full investigation report as plain prose. At the very end emit:

[DONE]

The text before [DONE] becomes the report delivered to the operator.
If you truly cannot complete the investigation without human input, emit [ESCALATED:LOW|MEDIUM|HIGH:reason] instead of [DONE].`

func SpawnInvestigator(db *sql.DB, name string) {
	logger := NewLogger(name)
	logger.Printf("Investigator %s starting up", name)

	for {
		if IsEstopped(db) {
			time.Sleep(5 * time.Second)
			continue
		}

		bounty, claimed := store.ClaimBounty(db, "Investigate", name)
		if !claimed {
			time.Sleep(time.Duration(2000+rand.Intn(1000)) * time.Millisecond)
			continue
		}

		runInvestigatorTask(db, name, bounty, logger)
	}
}

func runInvestigatorTask(db *sql.DB, name string, bounty *store.Bounty, logger *log.Logger) {
	sessionID := telemetry.NewSessionID()
	logger.Printf("[%s] Claimed Investigate #%d: %s", sessionID, bounty.ID, util.TruncateStr(bounty.Payload, 80))
	telemetry.EmitEvent(telemetry.TelemetryEvent{
		SessionID: sessionID, Agent: name, TaskID: bounty.ID,
		EventType: "task_investigating",
		Payload:   map[string]any{"payload_preview": util.TruncateStr(bounty.Payload, 120)},
	})

	// Run from the repo directory if one is registered; otherwise inherit CWD.
	runDir := ""
	if bounty.TargetRepo != "" {
		runDir = store.GetRepoPath(db, bounty.TargetRepo)
	}

	directive := LoadDirective("investigator", bounty.TargetRepo)
	directiveSection := ""
	if directive != "" {
		directiveSection = fmt.Sprintf("\n\n# OPERATOR DIRECTIVE\n%s", directive)
	}

	inboxCtx := buildInboxContext(db, name, "investigator", bounty.ID, logger)

	repoCtx := ""
	if bounty.TargetRepo != "" {
		repoCtx = fmt.Sprintf("\n\n# SCOPE\nFocus your investigation on the '%s' repository at: %s", bounty.TargetRepo, runDir)
	}

	fullPrompt := fmt.Sprintf("%s%s%s%s\n\nINVESTIGATION TASK:\n%s",
		investigatorSystemPrompt, directiveSection, repoCtx, inboxCtx, bounty.Payload)

	logger.Printf("Task %d: starting investigation (timeout: %v)", bounty.ID, investigatorTimeout)

	rawOut, err := claude.RunCLI(fullPrompt, claude.InvestigateTools, runDir, 30, investigatorTimeout)
	outputStr := strings.TrimSpace(rawOut)
	tokIn, tokOut := claude.ParseTokenUsage(outputStr)

	// Check for escalation before error handling — agent may have escalated cleanly.
	if sev, msg, ok := ParseEscalationSignal(outputStr); ok {
		logger.Printf("Task %d: escalated (%s): %s", bounty.ID, sev, msg)
		histID := store.RecordTaskHistory(db, bounty.ID, name, sessionID, util.TruncateStr(outputStr, 4000), "Escalated")
		if tokIn > 0 || tokOut > 0 {
			store.UpdateTaskHistoryTokens(db, histID, tokIn, tokOut)
		}
		CreateEscalation(db, bounty.ID, sev, msg)
		telemetry.EmitEvent(telemetry.EventTaskEscalated(sessionID, name, bounty.ID, sev, msg))
		store.LogAudit(db, name, "investigate-escalated", bounty.ID, msg)
		return
	}

	if err != nil {
		msg := fmt.Sprintf("Investigator CLI error: %v", err)
		logger.Printf("Task %d FAILED: %s", bounty.ID, msg)
		histID := store.RecordTaskHistory(db, bounty.ID, name, sessionID, util.TruncateStr(outputStr, 4000), "Failed")
		if tokIn > 0 || tokOut > 0 {
			store.UpdateTaskHistoryTokens(db, histID, tokIn, tokOut)
		}
		handleInfraFailure(db, name, "investigator", bounty, sessionID, msg, "Pending", false, logger)
		return
	}

	// Extract report: everything before the [DONE] signal.
	report := outputStr
	if idx := strings.Index(outputStr, "[DONE]"); idx != -1 {
		report = strings.TrimSpace(outputStr[:idx])
	}
	if report == "" {
		report = outputStr
	}

	// Deliver report as fleet mail to the operator.
	subject := fmt.Sprintf("[Investigation Complete] #%d: %s",
		bounty.ID, util.TruncateStr(bounty.Payload, 60))
	store.SendMail(db, name, "operator", subject, report, bounty.ID, store.MailTypeInfo)

	store.UpdateBountyStatus(db, bounty.ID, "Completed")
	histID := store.RecordTaskHistory(db, bounty.ID, name, sessionID, util.TruncateStr(outputStr, 4000), "Completed")
	if tokIn > 0 || tokOut > 0 {
		store.UpdateTaskHistoryTokens(db, histID, tokIn, tokOut)
	}

	telemetry.EmitEvent(telemetry.TelemetryEvent{
		SessionID: sessionID, Agent: name, TaskID: bounty.ID,
		EventType: "task_completed",
		Payload:   map[string]any{"report_chars": len(report), "tokens_in": tokIn, "tokens_out": tokOut},
	})
	store.LogAudit(db, name, "investigate-complete", bounty.ID,
		fmt.Sprintf("report delivered (%d chars)", len(report)))
	logger.Printf("Task %d: investigation complete — report delivered (%d chars, %d/%d tokens)",
		bounty.ID, len(report), tokIn, tokOut)
}
