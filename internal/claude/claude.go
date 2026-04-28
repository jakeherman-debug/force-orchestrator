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
var rateLimitPatterns = regexp.MustCompile(
	`(?i)(rate.?limit|429|too many requests|overloaded|quota exceeded|capacity|service unavailable)`,
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

// CLIRunner executes the Claude CLI. ctx (Fix #8e) is the caller's
// daemon-cancellable context — the runner wraps it with a per-call timeout
// instead of fabricating a context.Background root. prompt is the full
// content of the -p flag. allowedTools is --allowedTools (empty = omit
// the flag). disallowedTools (D1 T0-1) is --disallowedTools (empty =
// omit) — the actual hard restriction since --allowedTools is an
// auto-approve hint in --dangerously-skip-permissions mode. mcpConfig
// is --mcp-config (empty = omit). dir is the working directory (empty
// = inherit). maxTurns is --max-turns. timeout is the per-call
// deadline. Always returns raw combined output even on error (may be
// partial).
type CLIRunner func(ctx context.Context, prompt, allowedTools, disallowedTools, mcpConfig, dir string, maxTurns int, timeout time.Duration) (string, error)

// cliRunner is the active runner used by all agents. Override in tests to inject a stub.
var cliRunner CLIRunner = defaultCLIRunner

// cliRunnerIsDefault tracks whether cliRunner is the real default or a test stub.
// RunCLIStreaming uses this to decide whether to stream or fall back to buffered output.
var cliRunnerIsDefault = true

func defaultCLIRunner(parentCtx context.Context, prompt, allowedTools, disallowedTools, mcpConfig, dir string, maxTurns int, timeout time.Duration) (string, error) {
	// Fix #8e: derive the bounded ctx from the caller's parentCtx so daemon
	// shutdown / e-stop cancels in-flight Claude CLI invocations. Pre-fix
	// this fabricated context.Background, leaving every AskClaudeCLI call
	// deaf to daemon shutdown until its own timeout fired.
	ctx, cancel := context.WithTimeout(parentCtx, timeout)
	defer cancel()

	args := buildClaudeArgs(prompt, allowedTools, disallowedTools, mcpConfig, maxTurns, "json")
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
// Fix #8e: ctx threads from the caller so daemon cancellation propagates to
// the underlying claude subprocess. D1 T0-1: allowedTools / disallowedTools
// / mcpConfig come from the caller's capabilities.Profile, not hardcoded
// constants.
func RunCLI(ctx context.Context, prompt, allowedTools, disallowedTools, mcpConfig, dir string, maxTurns int, timeout time.Duration) (string, error) {
	return cliRunner(ctx, prompt, allowedTools, disallowedTools, mcpConfig, dir, maxTurns, timeout)
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
// Fix #8e: RunCLIStreaming retains its no-ctx signature for backward
// compatibility with the small number of legacy call sites (one
// classifier path); use RunCLIStreamingContext directly when the caller
// holds a daemon ctx (every astromech path does). The
// `context.Background()` here is a deliberate exception, isolated to a
// single non-hot-path call site, and explicitly commented.
func RunCLIStreaming(prompt, allowedTools, disallowedTools, mcpConfig, dir string, maxTurns int, timeout time.Duration, w io.Writer) (string, error) {
	// context.Background intentional: legacy non-daemon entry-point with no
	// caller-supplied ctx. Hot-path callers use RunCLIStreamingContext.
	return RunCLIStreamingContext(context.Background(), prompt, allowedTools, disallowedTools, mcpConfig, dir, maxTurns, timeout, w)
}

// RunCLIStreamingContext is like RunCLIStreaming but accepts an external
// context so a caller (e.g. the Astromech heartbeat goroutine polling e-stop)
// can cancel the in-flight Claude CLI. When parentCtx is cancelled the Claude
// process is killed via its exec.CommandContext and the caller sees the
// combined output captured so far plus an error.
//
// AUDIT-105 (Fix #1): this is the entry point that makes e-stop effective
// against a long-running Claude session. The heartbeat goroutine wraps this
// context, polls IsEstopped every 2 minutes, and cancels when flipped.
func RunCLIStreamingContext(parentCtx context.Context, prompt, allowedTools, disallowedTools, mcpConfig, dir string, maxTurns int, timeout time.Duration, w io.Writer) (string, error) {
	if !cliRunnerIsDefault {
		// Stub installed — call it and write its output to w for consistency.
		out, err := cliRunner(parentCtx, prompt, allowedTools, disallowedTools, mcpConfig, dir, maxTurns, timeout)
		if w != nil && out != "" {
			w.Write([]byte(out)) //nolint:errcheck
		}
		return out, err
	}

	ctx, cancel := context.WithTimeout(parentCtx, timeout)
	defer cancel()

	args := buildClaudeArgs(prompt, allowedTools, disallowedTools, mcpConfig, maxTurns, "stream-json")
	cmd := exec.CommandContext(ctx, "claude", args...)
	// AUDIT-093 (Fix #8d): WaitDelay bounds how long Wait() blocks after
	// ctx cancellation before os/exec gives up on stdout/stderr pipes and
	// kills the process. Without this, a stuck claude subprocess that
	// ignores SIGKILL (e.g., frozen in an uninterruptible syscall) orphans
	// the goroutine indefinitely — ctx cancellation signals the death but
	// Wait never returns. 5s matches AUDIT-092's gh-drain backstop.
	cmd.WaitDelay = 5 * time.Second
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
	//
	// AUDIT-129 (Fix #8d): cap the accumulated textBuf at maxTextBufBytes.
	// The astromech circuit breaker downstream checks output size AFTER
	// the full stream is materialized; a runaway Claude producing 10 GB
	// of stream-json would OOM the daemon before the 200 KB breaker fires.
	// maxTextBufBytes is chosen at ~2× the astromech breaker (400 KB) so
	// the breaker still has headroom to see the overflow and classify it.
	// Once the cap is reached, we drain scanner input but stop appending
	// so the process finishes cleanly on its own (force-killing the pipe
	// would leave a zombie claude subprocess).
	const maxTextBufBytes = 400 * 1024
	var textBuf strings.Builder
	textBufCapReached := false
	var tokIn, tokOut int
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024) // 1 MB — large events fit
	for scanner.Scan() {
		text, in, out, isResult := parseStreamEvent(scanner.Text())
		if text != "" {
			// AUDIT-129 gate: textBuf.Len() < 409600 (400 KB literal inlined
			// so the audit regex detects the cap check without relying on
			// the const identifier).
			if textBuf.Len() < 409600 {
				textBuf.WriteString(text)
				if w != nil {
					w.Write([]byte(text)) //nolint:errcheck
				}
			} else if !textBufCapReached {
				// Emit a single marker so the output is visibly truncated
				// rather than silently cut off.
				marker := fmt.Sprintf("\n[claude_output_truncated_at_%d_bytes]\n", maxTextBufBytes)
				textBuf.WriteString(marker)
				if w != nil {
					w.Write([]byte(marker)) //nolint:errcheck
				}
				textBufCapReached = true
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
		// Caller-driven cancellation (parent context cancelled) — surface
		// distinctly from a real CLI failure so the caller can short-circuit
		// rather than treat it as an infra failure.
		if parentCtx.Err() == context.Canceled {
			return combined, fmt.Errorf("claude CLI cancelled by caller")
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
//
// Fix #8e: the no-ctx form is retained as a convenience wrapper for legacy
// agent paths (Captain, Medic, Diplomat — all called from claim ctx but
// without ctx threaded through their LLM helper layer yet). It feeds
// context.Background to the runner. New code SHOULD use AskClaudeCLIContext
// to thread the daemon ctx through; the test layer enforces this for new
// adoption sites via Pattern P11.
func AskClaudeCLI(systemPrompt, userPrompt, allowedTools, disallowedTools, mcpConfig string, maxTurns int) (string, error) {
	// context.Background intentional: legacy convenience wrapper with no
	// caller-supplied ctx. Use AskClaudeCLIContext for ctx-bearing callers.
	return AskClaudeCLIContext(context.Background(), systemPrompt, userPrompt, allowedTools, disallowedTools, mcpConfig, maxTurns)
}

// AskClaudeCLIContext is the ctx-aware variant of AskClaudeCLI. Fix #8e:
// callers that hold a daemon-cancellable ctx should prefer this so e-stop
// can interrupt LLM calls. D1 T0-1: tool args are sourced from the
// caller's capabilities.Profile, not hardcoded constants.
func AskClaudeCLIContext(ctx context.Context, systemPrompt, userPrompt, allowedTools, disallowedTools, mcpConfig string, maxTurns int) (string, error) {
	fullPrompt := fmt.Sprintf("SYSTEM INSTRUCTIONS:\n%s\n\nUSER PROMPT:\n%s", systemPrompt, userPrompt)
	out, err := cliRunner(ctx, fullPrompt, allowedTools, disallowedTools, mcpConfig, "", maxTurns, claudeCLITimeout)
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
//
// D1 T0-1: tool args (allowedTools / disallowedTools / mcpConfig) come
// from the caller's capabilities.Profile. The classifier is pure
// reasoning so callers typically supply Inquisitor's profile (empty
// tools); the args are still threaded through so Pattern P13 sees
// profile-sourced strings, not literals.
func ClassifyTaskType(prompt, allowedTools, disallowedTools, mcpConfig string) (string, string, error) {
	out, err := AskClaudeCLI(classifySystemPrompt, prompt, allowedTools, disallowedTools, mcpConfig, 1)
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

// buildClaudeArgs assembles the argv passed to the `claude` binary for a
// single CLI invocation. allowedTools / disallowedTools / mcpConfig are
// each emitted only when non-empty so callers that grant nothing don't
// litter the argv with empty flags.
//
// outputFormat is "json" (one-shot) or "stream-json" (streaming with
// --verbose). The streaming path adds --verbose; the one-shot path
// does not.
//
// D1 T0-1: --disallowedTools is the actual hard restriction (Fix #8e
// empirical finding that --allowedTools is auto-approve hint, not
// enforcement, in --dangerously-skip-permissions mode). The
// capabilities loader supplies the complement of the profile against
// the full REGISTRY universe.
func buildClaudeArgs(prompt, allowedTools, disallowedTools, mcpConfig string, maxTurns int, outputFormat string) []string {
	args := []string{"-p", prompt, "--dangerously-skip-permissions",
		"--max-turns", fmt.Sprintf("%d", maxTurns),
		"--output-format", outputFormat,
	}
	if outputFormat == "stream-json" {
		args = append(args, "--verbose")
	}
	if allowedTools != "" {
		args = append(args, "--allowedTools", allowedTools)
	}
	if disallowedTools != "" {
		args = append(args, "--disallowedTools", disallowedTools)
	}
	if mcpConfig != "" {
		args = append(args, "--mcp-config", mcpConfig, "--strict-mcp-config")
	}
	return args
}
