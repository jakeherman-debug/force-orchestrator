package claude

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ── AstromechTimeoutForAttempt ────────────────────────────────────────────────

func TestAstromechTimeoutForAttempt(t *testing.T) {
	cases := []struct {
		infraFailures int
		want          time.Duration
	}{
		{0, 15 * time.Minute},
		{1, 22*time.Minute + 30*time.Second},
		{2, 33*time.Minute + 45*time.Second},
		{3, 45 * time.Minute},
		{10, 45 * time.Minute},
	}
	for _, c := range cases {
		got := AstromechTimeoutForAttempt(c.infraFailures)
		if got != c.want {
			t.Errorf("AstromechTimeoutForAttempt(%d) = %v, want %v", c.infraFailures, got, c.want)
		}
	}
}

// ── IsRateLimitError ──────────────────────────────────────────────────────────

func TestIsRateLimitError_Detected(t *testing.T) {
	cases := []string{
		"Error: rate limit exceeded",
		"claude: 429 Too Many Requests",
		"overloaded, please try again",
		"quota exceeded for this account",
	}
	for _, c := range cases {
		if !IsRateLimitError(c) {
			t.Errorf("expected rate limit detection for %q", c)
		}
	}
}

func TestIsRateLimitError_NotDetected(t *testing.T) {
	cases := []string{
		"Error: file not found",
		"claude: unexpected EOF",
		"git commit failed",
	}
	for _, c := range cases {
		if IsRateLimitError(c) {
			t.Errorf("unexpected rate limit detection for %q", c)
		}
	}
}

func TestRateLimitBackoff_ViaClaude(t *testing.T) {
	// Test RateLimitBackoff through a wrapper since it's in agents package.
	// Here we just test the claude package's behaviour stays stable.
	// (The full RateLimitBackoff tests live in internal/agents/estop_test.go)
}

// ── defaultCLIRunner E2E tests (via claude-stub) ──────────────────────────────

func withClaudeStub(t *testing.T, env map[string]string) {
	t.Helper()

	// Locate the stub script in testdata/ relative to this package's source
	stubSrc, err := filepath.Abs(filepath.Join("..", "..", "cmd", "force", "testdata", "claude-stub"))
	if err != nil || func() bool { _, e := os.Stat(stubSrc); return e != nil }() {
		t.Skip("testdata/claude-stub not found — skipping E2E stub test")
	}

	// Copy stub into a temp dir as "claude" so exec.LookPath resolves it
	stubDir := t.TempDir()
	stubDst := filepath.Join(stubDir, "claude")
	data, err := os.ReadFile(stubSrc)
	if err != nil {
		t.Fatalf("read stub: %v", err)
	}
	if err := os.WriteFile(stubDst, data, 0755); err != nil {
		t.Fatalf("write stub: %v", err)
	}

	// Prepend stub dir to PATH
	origPath := os.Getenv("PATH")
	os.Setenv("PATH", stubDir+":"+origPath)

	origVals := map[string]string{}
	for k, v := range env {
		origVals[k] = os.Getenv(k)
		os.Setenv(k, v)
	}

	t.Cleanup(func() {
		os.Setenv("PATH", origPath)
		for k, v := range origVals {
			if v == "" {
				os.Unsetenv(k)
			} else {
				os.Setenv(k, v)
			}
		}
	})
}

func TestDefaultCLIRunner_Success(t *testing.T) {
	withClaudeStub(t, map[string]string{
		"CLAUDE_STUB_OUTPUT": "[DONE] task complete",
		"CLAUDE_STUB_EXIT":   "0",
	})

	orig := cliRunner
	cliRunner = defaultCLIRunner
	t.Cleanup(func() { cliRunner = orig })

	out, err := defaultCLIRunner("test prompt", "", "", 5, 10*time.Second)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out, "[DONE]") {
		t.Errorf("expected [DONE] in output, got %q", out)
	}
}

func TestDefaultCLIRunner_ErrorExit(t *testing.T) {
	withClaudeStub(t, map[string]string{
		"CLAUDE_STUB_OUTPUT": "something went wrong",
		"CLAUDE_STUB_EXIT":   "1",
	})

	_, err := defaultCLIRunner("test prompt", "", "", 5, 10*time.Second)
	if err == nil {
		t.Fatal("expected error for non-zero exit, got nil")
	}
	if !strings.Contains(err.Error(), "claude CLI failed") {
		t.Errorf("expected 'claude CLI failed' error, got %q", err.Error())
	}
}

func TestDefaultCLIRunner_WithAllowedTools(t *testing.T) {
	withClaudeStub(t, map[string]string{
		"CLAUDE_STUB_OUTPUT": "ok",
	})

	out, err := defaultCLIRunner("prompt", "Edit,Read,Bash", "", 3, 10*time.Second)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out, "ok") {
		t.Errorf("expected 'ok' in output, got %q", out)
	}
}

func TestDefaultCLIRunner_Timeout(t *testing.T) {
	withClaudeStub(t, map[string]string{
		"CLAUDE_STUB_OUTPUT":   "never",
		"CLAUDE_STUB_SLEEP_MS": "5000", // 5 seconds
	})

	if _, err := exec.LookPath("bc"); err != nil {
		t.Skip("bc not available — skipping sleep stub test")
	}

	_, err := defaultCLIRunner("test prompt", "", "", 1, 100*time.Millisecond)
	if err == nil {
		t.Fatal("expected timeout error, got nil")
	}
	if !strings.Contains(err.Error(), "timed out") {
		t.Errorf("expected 'timed out' error, got %q", err.Error())
	}
}

func TestAskClaudeCLI_UsesRunner(t *testing.T) {
	orig := cliRunner
	cliRunner = func(prompt, tools, dir string, maxTurns int, timeout time.Duration) (string, error) {
		return `{"approved":true,"feedback":""}`, nil
	}
	t.Cleanup(func() { cliRunner = orig })

	out, err := AskClaudeCLI("sys", "usr", "", 1)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out, "approved") {
		t.Errorf("expected stub output, got %q", out)
	}
}

// ── ExtractJSON ───────────────────────────────────────────────────────────────

func TestExtractJSON_RawJSON(t *testing.T) {
	input := `[{"id":1,"repo":"api","task":"do thing","blocked_by":0}]`
	got := ExtractJSON(input)
	if got != input {
		t.Errorf("expected raw JSON unchanged, got %q", got)
	}
}

func TestExtractJSON_JsonFence(t *testing.T) {
	input := "```json\n[{\"id\":1}]\n```"
	got := ExtractJSON(input)
	if got != `[{"id":1}]` {
		t.Errorf("unexpected result: %q", got)
	}
}

func TestExtractJSON_PlainFence(t *testing.T) {
	input := "```\n[{\"id\":1}]\n```"
	got := ExtractJSON(input)
	if got != `[{"id":1}]` {
		t.Errorf("unexpected result: %q", got)
	}
}

func TestExtractJSON_WithLeadingText(t *testing.T) {
	input := "Here is the JSON:\n```json\n{\"approved\":true}\n```\nDone."
	got := ExtractJSON(input)
	if got != `{"approved":true}` {
		t.Errorf("unexpected result: %q", got)
	}
}

func TestExtractJSON_EdgeCases(t *testing.T) {
	// Empty input
	if got := ExtractJSON(""); got != "" {
		t.Errorf("empty input: got %q", got)
	}
	// Unclosed fence — just returns what's after the opening fence
	got := ExtractJSON("```json\n{\"key\":\"val\"}")
	if !strings.Contains(got, "key") {
		t.Errorf("unclosed fence: unexpected result %q", got)
	}
}

// ── ParseTokenUsage ───────────────────────────────────────────────────────────

func TestParseTokenUsage_LineFormat(t *testing.T) {
	output := "Tokens: 1,234 input, 567 output\nDone."
	in, out := ParseTokenUsage(output)
	if in != 1234 {
		t.Errorf("expected 1234 input tokens, got %d", in)
	}
	if out != 567 {
		t.Errorf("expected 567 output tokens, got %d", out)
	}
}

func TestParseTokenUsage_SeparatePatterns(t *testing.T) {
	output := "Used 800 input tokens and 200 output tokens."
	in, out := ParseTokenUsage(output)
	if in != 800 {
		t.Errorf("expected 800 input, got %d", in)
	}
	if out != 200 {
		t.Errorf("expected 200 output, got %d", out)
	}
}

func TestParseTokenUsage_NotPresent(t *testing.T) {
	in, out := ParseTokenUsage("I made a change to the code.")
	if in != 0 || out != 0 {
		t.Errorf("expected 0,0 got %d,%d", in, out)
	}
}

func TestParseTokenUsage_EmbeddedUsageLine(t *testing.T) {
	// Matches the [claude_usage: X input Y output] line injected by CLI runners
	// after parsing --output-format json / stream-json responses.
	output := "The task is complete.\n[DONE]\n[claude_usage: 18321 input 412 output]"
	in, out := ParseTokenUsage(output)
	if in != 18321 {
		t.Errorf("expected 18321 input tokens, got %d", in)
	}
	if out != 412 {
		t.Errorf("expected 412 output tokens, got %d", out)
	}
}

func TestParseJSONResult(t *testing.T) {
	raw := `{"type":"result","subtype":"success","result":"Task done.\n[DONE]","usage":{"input_tokens":500,"cache_creation_input_tokens":2000,"cache_read_input_tokens":8000,"output_tokens":150}}`
	text, tokIn, tokOut := parseJSONResult(raw)
	if text != "Task done.\n[DONE]" {
		t.Errorf("unexpected result text: %q", text)
	}
	if tokIn != 10500 { // 500 + 2000 + 8000
		t.Errorf("expected 10500 total input tokens, got %d", tokIn)
	}
	if tokOut != 150 {
		t.Errorf("expected 150 output tokens, got %d", tokOut)
	}
}

func TestParseStreamEvent_AssistantText(t *testing.T) {
	line := `{"type":"assistant","message":{"content":[{"type":"text","text":"[CHECKPOINT: work_done]\nAll done."}]}}`
	text, tokIn, tokOut, isResult := parseStreamEvent(line)
	if text != "[CHECKPOINT: work_done]\nAll done." {
		t.Errorf("unexpected text: %q", text)
	}
	if tokIn != 0 || tokOut != 0 || isResult {
		t.Errorf("unexpected token counts or isResult: %d, %d, %v", tokIn, tokOut, isResult)
	}
}

func TestParseStreamEvent_Result(t *testing.T) {
	line := `{"type":"result","subtype":"success","usage":{"input_tokens":100,"cache_creation_input_tokens":500,"cache_read_input_tokens":4000,"output_tokens":75}}`
	_, tokIn, tokOut, isResult := parseStreamEvent(line)
	if !isResult {
		t.Error("expected isResult=true")
	}
	if tokIn != 4600 { // 100 + 500 + 4000
		t.Errorf("expected 4600 total input tokens, got %d", tokIn)
	}
	if tokOut != 75 {
		t.Errorf("expected 75 output tokens, got %d", tokOut)
	}
}
