package agents

import (
	"strings"
	"testing"
)

func TestExtractClaudeErrorExcerpt_EmptyStdout(t *testing.T) {
	if got := extractClaudeErrorExcerpt(""); got != "" {
		t.Errorf("empty stdout → %q, want empty", got)
	}
	if got := extractClaudeErrorExcerpt("   \n\t  \n"); got != "" {
		t.Errorf("whitespace-only → %q, want empty", got)
	}
}

func TestExtractClaudeErrorExcerpt_PrefersRateLimitLine(t *testing.T) {
	in := `Let me investigate the failing test.
Looking at the code...
rate limit exceeded for org_xyz
Now let me continue analyzing.`
	got := extractClaudeErrorExcerpt(in)
	if !strings.Contains(got, "rate limit exceeded") {
		t.Errorf("should pick the rate-limit line, got %q", got)
	}
}

func TestExtractClaudeErrorExcerpt_PrefersExplicitErrorPrefix(t *testing.T) {
	in := `Now I'll implement the change.
Step 1: add the function.
Error: undefined symbol 'foo' on line 42
And then I will...`
	got := extractClaudeErrorExcerpt(in)
	if !strings.Contains(got, "undefined symbol") {
		t.Errorf("should pick the Error: line, got %q", got)
	}
}

func TestExtractClaudeErrorExcerpt_NarrationGivesLastLine(t *testing.T) {
	// The bug we care about: output is pure narration. Fallback should be
	// the last non-markdown line, bounded.
	in := `Now let me look at types.go to understand placement:
Now I have enough context. Let me start implementing.
First, add the table to schema.go:
Now schema.sql:`
	got := extractClaudeErrorExcerpt(in)
	if got == "" {
		t.Error("should fall back to a last-line excerpt")
	}
	if len(got) > 180 {
		t.Errorf("excerpt should be bounded at 180 chars, got %d", len(got))
	}
}

func TestExtractClaudeErrorExcerpt_SkipsMarkdownFragments(t *testing.T) {
	in := `Step 1
` + "```go" + `
func foo() {}
` + "```" + `
- bullet point
# heading
Some real final line here`
	got := extractClaudeErrorExcerpt(in)
	if got != "Some real final line here" {
		t.Errorf("should skip markdown and return real final line, got %q", got)
	}
}

func TestExtractClaudeErrorExcerpt_Truncation(t *testing.T) {
	long := strings.Repeat("x", 300)
	got := extractClaudeErrorExcerpt(long)
	// util.TruncateStr caps at 180 runes + a trailing ellipsis; the whole
	// output stays bounded well under 200 bytes.
	if len(got) > 200 {
		t.Errorf("excerpt length %d exceeds 200", len(got))
	}
	if !strings.Contains(got, "xxxxx") {
		t.Errorf("truncated excerpt should contain source characters, got %q", got)
	}
}
