package agents

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"math/rand"
	"strings"
	"time"

	"force-orchestrator/internal/agents/capabilities"
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

func SpawnInvestigator(ctx context.Context, db *sql.DB, name string) {
	logger := NewLogger(name)

	// D1 T0-1: load Investigator's capability profile once at spawn-time.
	profile, err := capabilities.LoadProfile("investigator")
	if err != nil {
		logger.Printf("Investigator %s cannot start: %v", name, err)
		return
	}
	logger.Printf("Investigator %s starting up", name)

	for {
		if ctx.Err() != nil {
			logger.Printf("Investigator %s exiting: %v", name, ctx.Err())
			return
		}
		if IsEstopped(db) {
			time.Sleep(5 * time.Second)
			continue
		}
		if SpendCapExceeded(db) {
			time.Sleep(10 * time.Second)
			continue
		}

		bounty, claimed := store.ClaimBounty(db, "Investigate", name)
		if !claimed {
			time.Sleep(time.Duration(2000+rand.Intn(1000)) * time.Millisecond)
			continue
		}

		runInvestigatorTask(ctx, db, name, bounty, profile, logger)
	}
}

// Fix #8e: ctx threads from SpawnInvestigator's claim ctx.
func runInvestigatorTask(ctx context.Context, db *sql.DB, name string, bounty *store.Bounty, profile *capabilities.Profile, logger *log.Logger) {
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

	mcpConfig, mcpErr := profile.MCPConfigArg()
	if mcpErr != nil {
		logger.Printf("Task %d: investigator MCP config write failed (%v) — proceeding without --mcp-config", bounty.ID, mcpErr)
	}
	rawOut, err := claude.RunCLI(ctx, fullPrompt,
		profile.AllowedToolsArg(), profile.DisallowedToolsArg(), mcpConfig,
		runDir, 30, investigatorTimeout)
	outputStr := strings.TrimSpace(rawOut)
	tokIn, tokOut := claude.ParseTokenUsage(outputStr)

	// Check for escalation before error handling — agent may have escalated cleanly.
	if sev, msg, ok := ParseEscalationSignal(outputStr); ok {
		logger.Printf("Task %d: escalated (%s): %s", bounty.ID, sev, msg)
		histID := store.RecordTaskHistory(db, bounty.ID, name, sessionID, util.TruncateStr(outputStr, 4000), "Escalated")
		RecordUsageAndCost(db, histID, outputStr)
		if _, err := CreateEscalation(db, bounty.ID, sev, msg); err != nil {
			// Escalation row didn't land; fall back to FailBounty + operator
			// mail so the task isn't left sitting in an Escalated-but-no-row
			// state (the AUDIT-041 defect).
			logger.Printf("Investigator #%d: CreateEscalation failed: %v — falling back to FailBounty + operator mail", bounty.ID, err)
			if failErr := store.FailBounty(db, bounty.ID, fmt.Sprintf("investigate-escalated (escalation insert failed: %v): %s", err, msg)); failErr != nil {
				logger.Printf("Investigator #%d: FailBounty fallback also failed: %v — stale-lock detector will recover", bounty.ID, failErr)
			}
			store.SendMail(db, name, "operator",
				fmt.Sprintf("[INVESTIGATE ESC FALLBACK] Task #%d — %s", bounty.ID, bounty.TargetRepo),
				fmt.Sprintf("Investigator escalation insert failed (%v). Original message:\n\n%s", err, msg),
				bounty.ID, store.MailTypeAlert)
		}
		telemetry.EmitEvent(telemetry.EventTaskEscalated(sessionID, name, bounty.ID, sev, msg))
		store.LogAudit(db, name, "investigate-escalated", bounty.ID, msg)
		return
	}

	if err != nil {
		msg := fmt.Sprintf("Investigator CLI error: %v", err)
		logger.Printf("Task %d FAILED: %s", bounty.ID, msg)
		histID := store.RecordTaskHistory(db, bounty.ID, name, sessionID, util.TruncateStr(outputStr, 4000), "Failed")
		RecordUsageAndCost(db, histID, outputStr)
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

	if err := store.UpdateBountyStatus(db, bounty.ID, "Completed"); err != nil {
		logger.Printf("Investigator #%d: Completed status update failed: %v — report already mailed; stale-lock detector will recover", bounty.ID, err)
	}
	histID := store.RecordTaskHistory(db, bounty.ID, name, sessionID, util.TruncateStr(outputStr, 4000), "Completed")
	RecordUsageAndCost(db, histID, outputStr)

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
