package claude

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// rateLimitPatterns matches known Claude CLI rate-limit / overload messages.
var rateLimitPatterns = regexp.MustCompile(
	`(?i)(rate.?limit|429|too many requests|overloaded|quota exceeded|capacity|service unavailable)`,
)

// IsRateLimitError returns true when Claude CLI output looks like a rate-limit
// or capacity error rather than a generic infra failure. Rate limits should NOT
// burn the infra_failures circuit-breaker budget.
func IsRateLimitError(output string) bool {
	return rateLimitPatterns.MatchString(output)
}

// tokenPattern matches lines Claude CLI emits with token usage, e.g.:
//
//	"Tokens: 1,234 input, 567 output"
//	"Input tokens: 1234  Output tokens: 567"
//	"1234 input tokens, 567 output tokens"
var tokenInPattern  = regexp.MustCompile(`(?i)(\d[\d,]*)\s+input\s+tokens?`)
var tokenOutPattern = regexp.MustCompile(`(?i)(\d[\d,]*)\s+output\s+tokens?`)
var tokenLinePattern = regexp.MustCompile(`(?i)tokens?[:\s]+(\d[\d,]*)\s+input[,\s]+(\d[\d,]*)\s+output`)

// ParseTokenUsage scans Claude CLI output for token counts.
// Returns (input, output) tokens; both are 0 if not found.
func ParseTokenUsage(output string) (int, int) {
	clean := func(s string) int {
		n, _ := strconv.Atoi(strings.ReplaceAll(s, ",", ""))
		return n
	}
	// Try "Tokens: X input, Y output" form first (most common)
	if m := tokenLinePattern.FindStringSubmatch(output); m != nil {
		return clean(m[1]), clean(m[2])
	}
	// Fall back to separate patterns
	var in, out int
	if m := tokenInPattern.FindStringSubmatch(output); m != nil {
		in = clean(m[1])
	}
	if m := tokenOutPattern.FindStringSubmatch(output); m != nil {
		out = clean(m[1])
	}
	return in, out
}

// claudeCLITimeout is the default timeout for simple reasoning calls (Council, etc.).
const claudeCLITimeout = 5 * time.Minute

const commanderBaseTimeout = 15 * time.Minute
const commanderMaxTimeout = 60 * time.Minute

// CommanderTimeoutForAttempt returns the timeout for a Commander decomposition run.
// infraFailures is the number of prior failures for the task (bounty.InfraFailures).
// Each prior failure increases the timeout by 50%, capped at 60 minutes:
//
//	0 failures → 15m, 1 → 22m30s, 2 → 33m45s, 3 → 50m37s, 4+ → 60m
func CommanderTimeoutForAttempt(infraFailures int) time.Duration {
	timeout := float64(commanderBaseTimeout)
	for i := 0; i < infraFailures; i++ {
		timeout *= 1.5
		if time.Duration(timeout) >= commanderMaxTimeout {
			return commanderMaxTimeout
		}
	}
	return time.Duration(timeout)
}

// Read-only Atlassian tools — look up Jira tickets and Confluence pages.
// Write tools (createJiraIssue, editJiraIssue, addCommentToJiraIssue, transitionJiraIssue,
// createConfluencePage, updateConfluencePage, etc.) are intentionally excluded.
const atlassianReadTools = "" +
	"mcp__plugin_dev-tools_atlassian__getJiraIssue," +
	"mcp__plugin_dev-tools_atlassian__searchJiraIssuesUsingJql," +
	"mcp__plugin_dev-tools_atlassian__getConfluencePage," +
	"mcp__plugin_dev-tools_atlassian__searchConfluenceUsingCql," +
	"mcp__plugin_dev-tools_atlassian__searchAtlassian"

// Read-only Glean tools — search and read internal company documents.
const gleanReadTools = "" +
	"mcp__plugin_glean_glean__search," +
	"mcp__plugin_glean_glean__read_document"

// Read-only SonarQube tools — inspect code quality and security issues.
// Write tools (change_security_hotspot_status, change_sonar_issue_status) are excluded.
const sonarReadTools = "" +
	"mcp__plugin_sonarqube_sonarqube__analyze_code_snippet," +
	"mcp__plugin_sonarqube_sonarqube__search_sonar_issues_in_projects," +
	"mcp__plugin_sonarqube_sonarqube__get_project_quality_gate_status," +
	"mcp__plugin_sonarqube_sonarqube__get_component_measures," +
	"mcp__plugin_sonarqube_sonarqube__search_security_hotspots," +
	"mcp__plugin_sonarqube_sonarqube__get_file_coverage_details"

// Read-only Datadog tools — observe logs, metrics, traces, and service topology.
// All Datadog tools are inherently read-only (observability only).
const datadogReadTools = "" +
	"mcp__plugin_dev-tools_datadog-mcp__search_datadog_logs," +
	"mcp__plugin_dev-tools_datadog-mcp__analyze_datadog_logs," +
	"mcp__plugin_dev-tools_datadog-mcp__search_datadog_monitors," +
	"mcp__plugin_dev-tools_datadog-mcp__get_datadog_metric," +
	"mcp__plugin_dev-tools_datadog-mcp__search_datadog_spans," +
	"mcp__plugin_dev-tools_datadog-mcp__get_datadog_trace," +
	"mcp__plugin_dev-tools_datadog-mcp__search_datadog_services," +
	"mcp__plugin_dev-tools_datadog-mcp__search_datadog_service_dependencies"

// CommanderTools — tools granted to Commander for decomposing feature requests.
// Needs ticket context and docs; does not write code so no file or Datadog tools.
const CommanderTools = atlassianReadTools + "," + gleanReadTools

// CouncilTools — tools granted to Jedi Council for reviewing diffs.
// Needs ticket context, docs, and quality signals to make informed rulings.
const CouncilTools = atlassianReadTools + "," + gleanReadTools + "," + sonarReadTools

// AstromechExtraTools — additional tools granted to Astromechs on top of the
// standard file tools (Edit,Write,Read,Bash,Glob,Grep). Provides ticket context,
// docs, quality signals, and observability for debugging and implementation.
const AstromechExtraTools = atlassianReadTools + "," + gleanReadTools + "," + sonarReadTools + "," + datadogReadTools

// InvestigateTools — tools for Investigator and Auditor agents.
// Read-only access to the codebase and all external observability/doc systems.
// No Edit or Write tools — these agents must never modify files.
const InvestigateTools = "Read,Grep,Glob,Bash," + atlassianReadTools + "," + gleanReadTools + "," + sonarReadTools + "," + datadogReadTools

// AtlassianReadTools exposes the atlassian tools for use in cmd/force (add-jira command).
const AtlassianReadTools = atlassianReadTools

// CLIRunner executes the Claude CLI. prompt is the full content of the -p flag.
// tools is --allowedTools (empty = omit the flag). dir is the working directory
// (empty = inherit current directory). maxTurns is --max-turns. timeout is the
// context deadline. Always returns raw combined output even on error (may be partial).
type CLIRunner func(prompt, tools, dir string, maxTurns int, timeout time.Duration) (string, error)

// cliRunner is the active runner used by all agents. Override in tests to inject a stub.
var cliRunner CLIRunner = defaultCLIRunner

// cliRunnerIsDefault tracks whether cliRunner is the real default or a test stub.
// RunCLIStreaming uses this to decide whether to stream or fall back to buffered output.
var cliRunnerIsDefault = true

func defaultCLIRunner(prompt, tools, dir string, maxTurns int, timeout time.Duration) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	args := []string{"-p", prompt, "--dangerously-skip-permissions", "--max-turns", fmt.Sprintf("%d", maxTurns)}
	if tools != "" {
		args = append(args, "--allowedTools", tools)
	}
	cmd := exec.CommandContext(ctx, "claude", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	rawOut, err := cmd.CombinedOutput()
	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return string(rawOut), fmt.Errorf("claude CLI timed out after %v", timeout)
		}
		return string(rawOut), fmt.Errorf("claude CLI failed: %v", err)
	}
	return string(rawOut), nil
}

// SetCLIRunner replaces the active CLI runner. Used by tests to inject stubs.
func SetCLIRunner(r CLIRunner) {
	cliRunner = r
	cliRunnerIsDefault = (r == nil) // nil resets to false; tests pass non-nil stubs
}

// ResetCLIRunner restores the default runner. Called by test cleanup.
func ResetCLIRunner() {
	cliRunner = defaultCLIRunner
	cliRunnerIsDefault = true
}

// DefaultCLIRunner is the real CLI runner; exposed for test cleanup.
var DefaultCLIRunner CLIRunner = defaultCLIRunner

// RunCLI invokes the active CLI runner directly (for use by agents that need
// custom directories and timeouts, e.g. Astromech running in a worktree).
func RunCLI(prompt, tools, dir string, maxTurns int, timeout time.Duration) (string, error) {
	return cliRunner(prompt, tools, dir, maxTurns, timeout)
}

// RunCLIStreaming is like RunCLI but also writes Claude's live output to w as
// it arrives, so the caller can tail or display progress in real-time. The full
// combined output is still returned as a string on completion.
//
// When a test stub is installed via SetCLIRunner, streaming is not meaningful
// (the stub returns immediately), so this falls back to the stub and writes
// the stub's output to w after it returns.
func RunCLIStreaming(prompt, tools, dir string, maxTurns int, timeout time.Duration, w io.Writer) (string, error) {
	if !cliRunnerIsDefault {
		// Stub installed — call it and write its output to w for consistency.
		out, err := cliRunner(prompt, tools, dir, maxTurns, timeout)
		if w != nil && out != "" {
			w.Write([]byte(out)) //nolint:errcheck
		}
		return out, err
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	args := []string{"-p", prompt, "--dangerously-skip-permissions", "--max-turns", fmt.Sprintf("%d", maxTurns)}
	if tools != "" {
		args = append(args, "--allowedTools", tools)
	}
	cmd := exec.CommandContext(ctx, "claude", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	var buf strings.Builder
	cmd.Stdout = io.MultiWriter(&buf, w)
	cmd.Stderr = io.MultiWriter(&buf, w)
	err := cmd.Run()
	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return buf.String(), fmt.Errorf("claude CLI timed out after %v", timeout)
		}
		return buf.String(), fmt.Errorf("claude CLI failed: %v", err)
	}
	return buf.String(), nil
}

// AskClaudeCLI is a convenience wrapper for simple (non-worktree) Claude calls.
// tools is a comma-separated list of allowed tool names; pass empty string for
// pure reasoning calls. maxTurns caps the session length.
func AskClaudeCLI(systemPrompt, userPrompt, tools string, maxTurns int) (string, error) {
	fullPrompt := fmt.Sprintf("SYSTEM INSTRUCTIONS:\n%s\n\nUSER PROMPT:\n%s", systemPrompt, userPrompt)
	out, err := cliRunner(fullPrompt, tools, "", maxTurns, claudeCLITimeout)
	if err != nil {
		return "", err
	}
	return out, nil
}

// validTaskTypes is the set of accepted classification outputs.
var validTaskTypes = map[string]bool{
	"Feature":     true,
	"CodeEdit":    true,
	"Investigate": true,
	"Audit":       true,
}

const classifySystemPrompt = `You are a task classifier. Given a task prompt, respond with EXACTLY one line in this format:
TypeName — one-sentence reason

Choose TypeName from exactly one of:
- Feature: large or multi-system change that benefits from decomposition into subtasks
- CodeEdit: targeted, well-defined code change in known files
- Investigate: open-ended research question with no clear code change yet
- Audit: broad codebase scan looking for issues to fix

Respond with only the single line. No preamble, no explanation, no markdown.`

// ClassifyTaskType calls Claude to classify a task prompt into one of four types.
// Returns (taskType, reason, err). taskType is one of: Feature, CodeEdit, Investigate, Audit.
// reason is a one-sentence explanation. Returns an error if the response cannot be parsed
// or the type is not one of the four valid values.
func ClassifyTaskType(prompt string) (string, string, error) {
	out, err := AskClaudeCLI(classifySystemPrompt, prompt, "", 1)
	if err != nil {
		return "", "", fmt.Errorf("classification failed: %w", err)
	}

	// Find the classification line — Claude may emit preamble; scan for a valid type.
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, " — ", 2)
		if len(parts) != 2 {
			continue
		}
		taskType := strings.TrimSpace(parts[0])
		reason := strings.TrimSpace(parts[1])
		if validTaskTypes[taskType] {
			return taskType, reason, nil
		}
	}

	return "", "", fmt.Errorf("could not parse classification from response: %q", strings.TrimSpace(out))
}

// ExtractJSON safely pulls JSON out of Claude's markdown wrappers
func ExtractJSON(response string) string {
	start := strings.Index(response, "```json")
	if start != -1 {
		response = response[start+7:]
		end := strings.Index(response, "```")
		if end != -1 {
			response = response[:end]
		}
	} else {
		start = strings.Index(response, "```")
		if start != -1 {
			response = response[start+3:]
			end := strings.Index(response, "```")
			if end != -1 {
				response = response[:end]
			}
		}
	}
	return strings.TrimSpace(response)
}

// PersistRateLimitHit stores an agent's rate-limit hit count in SystemConfig
// so backoff state survives daemon restarts.
func PersistRateLimitHit(db *sql.DB, agentName string, count int) {
	db.Exec(`INSERT OR REPLACE INTO SystemConfig (key, value) VALUES (?, ?)`,
		"rl_hits_"+agentName, fmt.Sprintf("%d", count))
}

// LoadRateLimitHits loads a persisted rate-limit hit count for an agent.
// Returns 0 if no entry exists (agent has not been rate-limited or backoff expired).
func LoadRateLimitHits(db *sql.DB, agentName string) int {
	var v string
	db.QueryRow(`SELECT value FROM SystemConfig WHERE key = ?`, "rl_hits_"+agentName).Scan(&v)
	if v == "" {
		return 0
	}
	var n int
	fmt.Sscanf(v, "%d", &n)
	return n
}

// ClearRateLimitHits removes a persisted rate-limit counter after a successful run.
func ClearRateLimitHits(db *sql.DB, agentName string) {
	db.Exec(`DELETE FROM SystemConfig WHERE key = ?`, "rl_hits_"+agentName)
}
