package claude

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// rateLimitPatterns matches known Claude CLI rate-limit / overload messages.
// "stream idle timeout" and "partial response received" are transient upstream CLI hiccups — treat them as retryable so they don't burn Pilot's infra_failures budget.
var rateLimitPatterns = regexp.MustCompile(
	`(?i)(rate.?limit|429|too many requests|overloaded|quota exceeded|capacity|service unavailable|stream idle timeout|partial response received)`,
)

// IsRateLimitError returns true when Claude CLI output looks like a rate-limit
// or capacity error rather than a generic infra failure. Rate limits should NOT
// burn the infra_failures circuit-breaker budget.
func IsRateLimitError(output string) bool {
	return rateLimitPatterns.MatchString(output)
}

// tokenUsageLine matches the embedded usage annotation appended by CLI runners:
//
//	"[claude_usage: 12345 input 678 output]"
var tokenUsageLine = regexp.MustCompile(`\[claude_usage:\s*(\d+)\s+input\s+(\d+)\s+output\]`)

// tokenPattern matches legacy token lines Claude CLI may emit in text mode, e.g.:
//
//	"Tokens: 1,234 input, 567 output"
//	"1234 input tokens, 567 output tokens"
var tokenInPattern  = regexp.MustCompile(`(?i)(\d[\d,]*)\s+input\s+tokens?`)
var tokenOutPattern = regexp.MustCompile(`(?i)(\d[\d,]*)\s+output\s+tokens?`)
var tokenLinePattern = regexp.MustCompile(`(?i)tokens?[:\s]+(\d[\d,]*)\s+input[,\s]+(\d[\d,]*)\s+output`)

// ParseTokenUsage scans Claude CLI output for token counts.
// Returns (input, output) tokens; both are 0 if not found.
// Checks for the embedded [claude_usage: X input Y output] annotation first
// (injected by the CLI runners from --output-format json), then falls back to
// legacy text patterns.
func ParseTokenUsage(output string) (int, int) {
	clean := func(s string) int {
		n, _ := strconv.Atoi(strings.ReplaceAll(s, ",", ""))
		return n
	}
	// Embedded usage annotation (most reliable — injected by CLI runner)
	if m := tokenUsageLine.FindStringSubmatch(output); m != nil {
		return clean(m[1]), clean(m[2])
	}
	// Legacy: "Tokens: X input, Y output" form
	if m := tokenLinePattern.FindStringSubmatch(output); m != nil {
		return clean(m[1]), clean(m[2])
	}
	// Legacy: separate patterns
	var in, out int
	if m := tokenInPattern.FindStringSubmatch(output); m != nil {
		in = clean(m[1])
	}
	if m := tokenOutPattern.FindStringSubmatch(output); m != nil {
		out = clean(m[1])
	}
	return in, out
}

// parseJSONResult parses a claude CLI --output-format json result object.
// Returns (resultText, totalInputTokens, outputTokens).
// totalInputTokens sums input_tokens + cache_creation_input_tokens + cache_read_input_tokens
// for a complete picture of tokens processed.
// Falls back to returning the raw string with 0 tokens if parsing fails.
func parseJSONResult(raw string) (string, int, int) {
	var result struct {
		Type   string `json:"type"`
		Result string `json:"result"`
		Usage  struct {
			InputTokens              int `json:"input_tokens"`
			CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
			CacheReadInputTokens     int `json:"cache_read_input_tokens"`
			OutputTokens             int `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal([]byte(raw), &result); err != nil || result.Type != "result" {
		return raw, 0, 0
	}
	tokIn := result.Usage.InputTokens + result.Usage.CacheCreationInputTokens + result.Usage.CacheReadInputTokens
	return result.Result, tokIn, result.Usage.OutputTokens
}

// parseStreamEvent parses one line from --output-format stream-json --verbose output.
// For assistant messages it returns extracted text. For the result event it returns
// total input tokens (regular + cache creation + cache reads) and output tokens.
func parseStreamEvent(line string) (text string, tokIn, tokOut int, isResult bool) {
	var event struct {
		Type    string `json:"type"`
		Message struct {
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"message"`
		Usage struct {
			InputTokens              int `json:"input_tokens"`
			CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
			CacheReadInputTokens     int `json:"cache_read_input_tokens"`
			OutputTokens             int `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal([]byte(line), &event); err != nil {
		return "", 0, 0, false
	}
	switch event.Type {
	case "assistant":
		var sb strings.Builder
		for _, c := range event.Message.Content {
			if c.Type == "text" {
				sb.WriteString(c.Text)
			}
		}
		return sb.String(), 0, 0, false
	case "result":
		total := event.Usage.InputTokens + event.Usage.CacheCreationInputTokens + event.Usage.CacheReadInputTokens
		return "", total, event.Usage.OutputTokens, true
	}
	return "", 0, 0, false
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

const astromechBaseTimeout = 15 * time.Minute
const astromechMaxTimeout = 45 * time.Minute

// AstromechTimeoutForAttempt returns the timeout for an Astromech task run.
// infraFailures is the number of prior failures for the task (bounty.InfraFailures).
// Each prior failure increases the timeout by 50%, capped at 45 minutes:
//
//	0 failures → 15m, 1 → 22m30s, 2 → 33m45s, 3+ → 45m
func AstromechTimeoutForAttempt(infraFailures int) time.Duration {
	timeout := float64(astromechBaseTimeout)
	for i := 0; i < infraFailures; i++ {
		timeout *= 1.5
		if time.Duration(timeout) >= astromechMaxTimeout {
			return astromechMaxTimeout
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

// Read-only Glean tools — search and read documents.
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

	args := []string{"-p", prompt, "--dangerously-skip-permissions", "--max-turns", fmt.Sprintf("%d", maxTurns), "--output-format", "json"}
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
	// Parse JSON output to extract result text and token usage.
	text, tokIn, tokOut := parseJSONResult(strings.TrimSpace(string(rawOut)))
	if tokIn > 0 || tokOut > 0 {
		text += fmt.Sprintf("\n[claude_usage: %d input %d output]", tokIn, tokOut)
	}
	return text, nil
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
// Uses --output-format stream-json --verbose to capture token usage from the
// final result event. Only the assistant's text content is written to w (not
// raw JSON events), keeping the live display readable.
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

	args := []string{"-p", prompt, "--dangerously-skip-permissions", "--max-turns", fmt.Sprintf("%d", maxTurns), "--output-format", "stream-json", "--verbose"}
	if tools != "" {
		args = append(args, "--allowedTools", tools)
	}
	cmd := exec.CommandContext(ctx, "claude", args...)
	if dir != "" {
		cmd.Dir = dir
	}

	// Stderr holds error text; stdout holds newline-delimited JSON events.
	var stderrBuf strings.Builder
	cmd.Stderr = &stderrBuf

	stdout, pipeErr := cmd.StdoutPipe()
	if pipeErr != nil {
		return "", fmt.Errorf("claude CLI stdout pipe: %v", pipeErr)
	}
	if startErr := cmd.Start(); startErr != nil {
		return "", fmt.Errorf("claude CLI start: %v", startErr)
	}

	// Process stream-json events: extract text for display and capture token counts.
	var textBuf strings.Builder
	var tokIn, tokOut int
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024) // 1 MB — large events fit
	for scanner.Scan() {
		text, in, out, isResult := parseStreamEvent(scanner.Text())
		if text != "" {
			textBuf.WriteString(text)
			if w != nil {
				w.Write([]byte(text)) //nolint:errcheck
			}
		}
		if isResult {
			tokIn, tokOut = in, out
		}
	}

	runErr := cmd.Wait()
	if runErr != nil {
		combined := textBuf.String() + stderrBuf.String()
		if ctx.Err() == context.DeadlineExceeded {
			return combined, fmt.Errorf("claude CLI timed out after %v", timeout)
		}
		return combined, fmt.Errorf("claude CLI failed: %v", runErr)
	}

	result := textBuf.String()
	if tokIn > 0 || tokOut > 0 {
		tokenLine := fmt.Sprintf("\n[claude_usage: %d input %d output]", tokIn, tokOut)
		result += tokenLine
		if w != nil {
			w.Write([]byte(tokenLine)) //nolint:errcheck
		}
	}
	return result, nil
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
// CodeEdit is intentionally excluded — all code changes must flow through
// Commander → Chancellor to prevent clobbering and ensure conflict review.
var validTaskTypes = map[string]bool{
	"Feature":     true,
	"Investigate": true,
	"Audit":       true,
}

const classifySystemPrompt = `You are a task classifier. Given a task prompt, respond with EXACTLY one line in this format:
TypeName — one-sentence reason

Choose TypeName from exactly one of:
- Feature: any code change, bug fix, or feature — even small targeted ones. All code changes go through Commander for decomposition and conflict review.
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

// ExtractJSON safely pulls JSON out of Claude's markdown wrappers and strips
// any trailing [claude_usage: ...] annotation appended by the CLI runner.
func ExtractJSON(response string) string {
	// Strip trailing usage annotation before any other processing.
	if idx := strings.Index(response, "\n[claude_usage:"); idx != -1 {
		response = response[:idx]
	}
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
