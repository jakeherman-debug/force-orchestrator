// Package agents — JIRA-from-UI standalone helper.
//
// QueueFeatureFromJira is the reusable core extracted from the
// `force add-jira` CLI command (cmd/force/task_cmds.go: cmdAddJira).
// It fetches a Jira ticket via the cli-jira capability profile (Atlassian
// read MCP), formats the resulting description as a Feature payload,
// inserts a BountyBoard row, and applies the optional priority + plan-
// only flag. Both the CLI and the dashboard `POST /api/feature/from-jira`
// handler call this helper so the two surfaces stay in lock-step.
//
// Design constraints satisfied here:
//
//   - Pattern P13 (capability profiles). The cli-jira profile is loaded
//     via capabilities.LoadProfile — never a hardcoded tool literal.
//     Profile failures fail closed (return an error; no Claude call).
//
//   - Pattern P31 (LLM transcripts). The Claude CLI invocation routes
//     through claude.CallWithTranscript so every fetch lands in
//     LLMCallTranscripts (when SetTranscriptDB is wired — the daemon
//     wires it; CLI one-shots silently degrade per the wrapper's
//     contract).
//
//   - LIVE_HAIKU_DISABLED env-flag pattern. When set, the helper short-
//     circuits to a deterministic stub so unit tests never hit live MCP.
//     Matches the convention used by every renderer in live_haiku.go.
//
//   - No silent failures. Every error path returns an error; the helper
//     never logs and continues. Validation of ticket_id is the caller's
//     job (the dashboard handler enforces the regex; the CLI passes raw
//     argv) — but a blank ticket id here returns an error rather than
//     queuing a meaningless task.
package agents

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"force-orchestrator/internal/agents/capabilities"
	"force-orchestrator/internal/claude"
	"force-orchestrator/internal/store"
)

// QueueFeatureFromJiraResult carries the bookkeeping the dashboard
// surface needs in its 200 response. The CLI uses a subset (id +
// summary go to fmt.Printf).
type QueueFeatureFromJiraResult struct {
	TaskID  int    // BountyBoard row id of the queued Feature task
	Summary string // first 200 chars of the fetched description (post-trim)
}

// jiraSystemPrompt is the system instruction the Atlassian read tools
// see. Kept identical to the cmdAddJira shape so behaviour is preserved
// byte-for-byte for the CLI surface; the dashboard surface gets the
// same prompt for free.
const jiraSystemPrompt = `You are a product manager assistant. Use your Atlassian Jira MCP tools to fetch the requested ticket.
Return a comprehensive feature description as plain text including: ticket title, description, acceptance criteria, and any relevant context from linked tickets.
Do not use markdown formatting. Write it as a clear feature request that a software architect can decompose into coding tasks.`

// QueueFeatureFromJira fetches a Jira ticket and queues a Feature task
// with its description as the payload. Returns the queued task id and a
// short summary suitable for an inline UI toast.
//
// priority == 0 leaves the BountyBoard default in place. planOnly == true
// prepends the [PLAN_ONLY] sentinel so Commander stops at the plan stage.
//
// Behaviour preserved from cmdAddJira:
//   - payload format: "[JIRA: TICKET-ID]\n<description>"
//   - planOnly prefix: "[PLAN_ONLY]\n" prepended when true
//   - capability profile: cli-jira (Atlassian read MCP)
//   - max-turns: 5 (matches the CLI)
//
// New, beyond cmdAddJira:
//   - LLM call routed through claude.CallWithTranscript (Pattern P31).
//   - LIVE_HAIKU_DISABLED stub path returns "(deterministic stub for tests)"
//     so unit tests can exercise the queue path without spending an
//     LLM call.
//   - Returns errors instead of os.Exit-ing — callers decide policy.
func QueueFeatureFromJira(ctx context.Context, db *sql.DB, ticketID string, priority int, planOnly bool) (QueueFeatureFromJiraResult, error) {
	ticketID = strings.TrimSpace(ticketID)
	if ticketID == "" {
		return QueueFeatureFromJiraResult{}, errors.New("QueueFeatureFromJira: ticket id is required")
	}
	if db == nil {
		return QueueFeatureFromJiraResult{}, errors.New("QueueFeatureFromJira: nil db")
	}

	description, err := fetchJiraDescription(ctx, ticketID)
	if err != nil {
		return QueueFeatureFromJiraResult{}, fmt.Errorf("fetch jira ticket %s: %w", ticketID, err)
	}

	payload := fmt.Sprintf("[JIRA: %s]\n%s", ticketID, strings.TrimSpace(description))
	if planOnly {
		payload = "[PLAN_ONLY]\n" + payload
	}
	id := store.AddBounty(db, 0, "Feature", payload)
	if id == 0 {
		return QueueFeatureFromJiraResult{}, fmt.Errorf("AddBounty returned 0 for jira ticket %s", ticketID)
	}
	if priority != 0 {
		store.SetBountyPriority(db, id, priority)
	}

	return QueueFeatureFromJiraResult{
		TaskID:  id,
		Summary: truncateForSummary(strings.TrimSpace(description), 200),
	}, nil
}

// fetchJiraDescription returns the description blob the LLM would emit,
// or the deterministic stub when LIVE_HAIKU_DISABLED is set.
//
// Splitting this from QueueFeatureFromJira lets the test harness exercise
// the queue path (AddBounty / payload formatting / priority / plan-only)
// without touching the LLM at all — the env flag short-circuits before
// any capability profile load.
func fetchJiraDescription(ctx context.Context, ticketID string) (string, error) {
	if liveHaikuDisabled() {
		// Deterministic test stub. Shape mirrors the live LLM output
		// closely enough that downstream payload formatting + summary
		// truncation are exercised faithfully.
		return fmt.Sprintf("[JIRA: %s] (deterministic stub for tests)", ticketID), nil
	}

	prof, err := capabilities.LoadProfile("cli-jira")
	if err != nil {
		return "", fmt.Errorf("load cli-jira capability profile: %w", err)
	}
	mcpConfig, _ := prof.MCPConfigArg()

	desc, err := claude.CallWithTranscript(ctx, claude.CallDescriptor{
		Agent:         "cli-jira",
		PromptVersion: "feature-from-jira-v1",
	}, jiraSystemPrompt,
		fmt.Sprintf("Fetch Jira ticket %s and return its full context as a feature description.", ticketID),
		prof.AllowedToolsArg(), prof.DisallowedToolsArg(), mcpConfig, 5)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(desc) == "" {
		return "", fmt.Errorf("empty description from Claude for ticket %s", ticketID)
	}
	return desc, nil
}

// truncateForSummary returns at most max runes (not bytes) followed by
// the universal ellipsis. Rune-aware so the inline dashboard toast never
// renders a half-encoded codepoint at the cut.
func truncateForSummary(s string, max int) string {
	if max <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "…"
}
