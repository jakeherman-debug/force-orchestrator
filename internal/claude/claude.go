package claude

import (
	"context"
	"database/sql"
	"fmt"
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

// claudeCLITimeout is the default timeout for Commander and Council Claude calls.
// Astromech uses its own longer timeout (astromechTimeout) since it does real coding work.
const claudeCLITimeout = 5 * time.Minute

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

// AtlassianReadTools exposes the atlassian tools for use in cmd/force (add-jira command).
const AtlassianReadTools = atlassianReadTools

// CLIRunner executes the Claude CLI. prompt is the full content of the -p flag.
// tools is --allowedTools (empty = omit the flag). dir is the working directory
// (empty = inherit current directory). maxTurns is --max-turns. timeout is the
// context deadline. Always returns raw combined output even on error (may be partial).
type CLIRunner func(prompt, tools, dir string, maxTurns int, timeout time.Duration) (string, error)

// cliRunner is the active runner used by all agents. Override in tests to inject a stub.
var cliRunner CLIRunner = defaultCLIRunner

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
}

// DefaultCLIRunner is the real CLI runner; exposed for test cleanup.
var DefaultCLIRunner CLIRunner = defaultCLIRunner

// RunCLI invokes the active CLI runner directly (for use by agents that need
// custom directories and timeouts, e.g. Astromech running in a worktree).
func RunCLI(prompt, tools, dir string, maxTurns int, timeout time.Duration) (string, error) {
	return cliRunner(prompt, tools, dir, maxTurns, timeout)
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
