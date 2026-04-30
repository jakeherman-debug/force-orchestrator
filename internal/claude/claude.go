package claude

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"os"
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

// modelLine matches the embedded model annotation appended by CLI runners
// alongside [claude_usage: ...]: "[claude_model: claude-opus-4-5]". The
// model id flows through the per-model cost table in pricing.go so the
// agents writing TaskHistory rows can compute cost_usd_estimate without
// re-parsing the raw JSON output.
var modelLine = regexp.MustCompile(`\[claude_model:\s*([^\]]+)\]`)

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

// ParseModel scans Claude CLI output for the embedded [claude_model: <id>]
// annotation injected by the CLI runners from the JSON output's
// model field. Returns "" when no annotation is present (older CLI
// versions, stub runners in tests). Callers feed the result to
// pricing.CostUSD which gracefully handles unknown / empty models by
// returning $0.
func ParseModel(output string) string {
	if m := modelLine.FindStringSubmatch(output); m != nil {
		return strings.TrimSpace(m[1])
	}
	return ""
}

// parseJSONResult parses a claude CLI --output-format json result object.
// Returns (resultText, totalInputTokens, outputTokens, model).
// totalInputTokens sums input_tokens + cache_creation_input_tokens + cache_read_input_tokens
// for a complete picture of tokens processed.
// model is the canonical model id reported by the CLI (e.g.
// "claude-opus-4-5"); empty string when the field is absent.
// Falls back to returning the raw string with zeros / "" if parsing fails.
func parseJSONResult(raw string) (string, int, int, string) {
	var result struct {
		Type    string `json:"type"`
		Result  string `json:"result"`
		Model   string `json:"model"`
		Message struct {
			Model string `json:"model"`
		} `json:"message"`
		Usage struct {
			InputTokens              int `json:"input_tokens"`
			CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
			CacheReadInputTokens     int `json:"cache_read_input_tokens"`
			OutputTokens             int `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal([]byte(raw), &result); err != nil || result.Type != "result" {
		return raw, 0, 0, ""
	}
	tokIn := result.Usage.InputTokens + result.Usage.CacheCreationInputTokens + result.Usage.CacheReadInputTokens
	model := result.Model
	if model == "" {
		model = result.Message.Model
	}
	return result.Result, tokIn, result.Usage.OutputTokens, model
}

// parseStreamEvent parses one line from --output-format stream-json --verbose output.
// For assistant messages it returns extracted text. For the result event it returns
// total input tokens (regular + cache creation + cache reads) and output tokens.
// The result event also carries the model id (D2 T1-1) so the streaming path
// can emit a [claude_model: ...] annotation alongside [claude_usage: ...].
func parseStreamEvent(line string) (text string, tokIn, tokOut int, model string, isResult bool) {
	var event struct {
		Type    string `json:"type"`
		Model   string `json:"model"`
		Message struct {
			Model   string `json:"model"`
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
		return "", 0, 0, "", false
	}
	switch event.Type {
	case "assistant":
		var sb strings.Builder
		for _, c := range event.Message.Content {
			if c.Type == "text" {
				sb.WriteString(c.Text)
			}
		}
		return sb.String(), 0, 0, "", false
	case "result":
		total := event.Usage.InputTokens + event.Usage.CacheCreationInputTokens + event.Usage.CacheReadInputTokens
		m := event.Model
		if m == "" {
			m = event.Message.Model
		}
		return "", total, event.Usage.OutputTokens, m, true
	}
	return "", 0, 0, "", false
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
	// Parse JSON output to extract result text, token usage, and model.
	// D2 T1-1: model id is appended as a [claude_model: ...] annotation
	// so downstream agents can compute per-attempt cost via
	// pricing.CostUSD without re-parsing the raw JSON. task_id is NOT
	// threaded through this layer in T1-1; T1-2's prompt-assembly refactor
	// is the natural seam for that.
	text, tokIn, tokOut, model := parseJSONResult(strings.TrimSpace(string(rawOut)))
	if tokIn > 0 || tokOut > 0 {
		text += fmt.Sprintf("\n[claude_usage: %d input %d output]", tokIn, tokOut)
	}
	if model != "" {
		text += fmt.Sprintf("\n[claude_model: %s]", model)
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
// constants. D1 T0-2: prompt is scrubbed by ScrubInbound before reaching
// the runner — secret-bearing content (PEM blocks, .env lines, GCP keys)
// never enters the Anthropic prompt cache.
func RunCLI(ctx context.Context, prompt, allowedTools, disallowedTools, mcpConfig, dir string, maxTurns int, timeout time.Duration) (string, error) {
	scrubbed, n := ScrubInbound(prompt)
	observeInboundRedact("RunCLI", 0, n)
	return cliRunner(ctx, scrubbed, allowedTools, disallowedTools, mcpConfig, dir, maxTurns, timeout)
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
func RunCLIStreaming(prompt, allowedTools, disallowedTools, mcpConfig, dir string, maxTurns int, timeout time.Duration, w io.Writer, extraEnv ...string) (string, error) {
	// context.Background intentional: legacy non-daemon entry-point with no
	// caller-supplied ctx. Hot-path callers use RunCLIStreamingContext.
	return RunCLIStreamingContext(context.Background(), prompt, allowedTools, disallowedTools, mcpConfig, dir, maxTurns, timeout, w, extraEnv...)
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
// extraEnv is an optional list of "KEY=VALUE" entries appended to the
// claude subprocess environment. Used by the astromech (D2 T1-3) to
// prepend the bash-guard shim dir onto PATH so the Bash tool's bash
// invocations route through force-bash-guard before exec'ing the real
// shell. A nil/empty slice leaves the parent process env intact.
func RunCLIStreamingContext(parentCtx context.Context, prompt, allowedTools, disallowedTools, mcpConfig, dir string, maxTurns int, timeout time.Duration, w io.Writer, extraEnv ...string) (string, error) {
	// D3 Phase 1 — log-only treatments.Apply ingress (mirrors
	// AskClaudeCLIContext). Astromech sessions route through here.
	if err := invokeTreatmentApplyHook(parentCtx); err != nil {
		return "", fmt.Errorf("treatments.Apply: %w", err)
	}

	// D1 T0-2: scrub prompt at the boundary before any path (stub or
	// real exec) sees it. Centralised here so the streaming variant
	// catches inbound secrets the same way as the one-shot path.
	scrubbed, n := ScrubInbound(prompt)
	observeInboundRedact("RunCLIStreamingContext", 0, n)
	prompt = scrubbed

	// D2 T1-2 — per-agent context-size enforcement at the TOP of the
	// streaming ingress (after redaction, same as the one-shot path).
	// On overflow + summarizer failure, the call returns
	// ErrContextOverflow before any subprocess is spawned; the
	// astromech caller routes through handleInfraFailure.
	revised, sizeErr := CheckContextSize(parentCtx, activeContextSizeDB(), prompt)
	if sizeErr != nil {
		return "", sizeErr
	}
	prompt = revised

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
	// D2 T1-3: extraEnv appends per-call env entries (e.g., the
	// astromech's PATH=<bash-guard-shim>:<existing> override). We start
	// from the parent process environment so claude inherits HOME,
	// CLAUDE_CODE_*, and friends.
	if len(extraEnv) > 0 {
		cmd.Env = append(os.Environ(), extraEnv...)
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
	var model string
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024) // 1 MB — large events fit
	for scanner.Scan() {
		text, in, out, eventModel, isResult := parseStreamEvent(scanner.Text())
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
			model = eventModel
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
	// D2 T1-1: emit the model id as a separate annotation so downstream
	// agents can route the value into pricing.CostUSD without re-parsing
	// the raw JSON output. Stays alongside [claude_usage: ...] for
	// symmetry with the one-shot path.
	if model != "" {
		modelAnnotation := fmt.Sprintf("\n[claude_model: %s]", model)
		result += modelAnnotation
		if w != nil {
			w.Write([]byte(modelAnnotation)) //nolint:errcheck
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

	// D3 Phase 1 — log-only treatments.Apply ingress. Records the call
	// descriptor + (empty in Phase 1) assignment intent without
	// mutating the call. Phase 2 flips this live; Phase 1 ships the
	// audit trail so the live flip is a config change, not a code
	// change.
	if err := invokeTreatmentApplyHook(ctx); err != nil {
		return "", fmt.Errorf("treatments.Apply: %w", err)
	}

	// D2 T1-2 — per-agent context-size enforcement runs at the TOP of
	// the ingress, BEFORE redaction (size cap is on the bytes that
	// get sent to Claude, after redaction; we need the post-scrub
	// length for the cap check).
	// D1 T0-2: scrub the assembled prompt before it hits the runner.
	// Anti-cheat: redaction is enforcement, not advisory — the call
	// proceeds with the redacted prompt, no "warn but send original"
	// path.
	scrubbed, n := ScrubInbound(fullPrompt)
	observeInboundRedact("AskClaudeCLIContext", 0, n)

	// Context-size guard. db is sourced from the active runtime DB
	// (set by the daemon at startup); when no DB is configured, the
	// guard runs with default cap and skips persistence — tests that
	// don't care about the guard see no behaviour change.
	revised, err := CheckContextSize(ctx, activeContextSizeDB(), scrubbed)
	if err != nil {
		return "", err
	}
	scrubbed = revised

	out, err := cliRunner(ctx, scrubbed, allowedTools, disallowedTools, mcpConfig, "", maxTurns, claudeCLITimeout)
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
// any trailing [claude_usage: ...] / [claude_model: ...] annotations
// appended by the CLI runner.
func ExtractJSON(response string) string {
	// Strip trailing usage annotation before any other processing.
	if idx := strings.Index(response, "\n[claude_usage:"); idx != -1 {
		response = response[:idx]
	}
	// D2 T1-1: also strip the model annotation. ExtractJSON callers feed
	// the result to strictJSONUnmarshal which would otherwise complain
	// about trailing tokens.
	if idx := strings.Index(response, "\n[claude_model:"); idx != -1 {
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
