package claude

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"force-orchestrator/internal/store"
)

// TestLLMCallTranscriptWrapper covers the core 6B.1 wrapper invariants:
//
//   - Happy path inserts ONE row, populated with redacted prompts +
//     response, parsed token counts, completed_at set.
//   - Cancellation path leaves completed_at empty so forensic queries
//     can distinguish completed from interrupted runs (per the brief).
//   - Redaction at write time scrubs ghp_* tokens and Bearer headers
//     from the persisted row.
//   - When db is nil (no transcript backend wired), the wrapper falls
//     through transparently — the underlying CLI still runs.
//   - Idempotence: same call twice produces two rows with identical
//     redacted bodies; no shared row is overwritten.
func TestLLMCallTranscriptWrapper(t *testing.T) {
	t.Run("happy_path_records_full_row", func(t *testing.T) {
		db := store.InitHolocronDSN(":memory:")
		defer db.Close()
		SetTranscriptDB(db)
		defer SetTranscriptDB(nil)

		// Stub runner emits the embedded usage annotations the wrapper
		// parses for cost/token attribution.
		SetCLIRunner(func(_ context.Context, _, _, _, _, _ string, _ int, _ time.Duration) (string, error) {
			return "ok response\n[claude_usage: 100 input 50 output]\n[claude_model: claude-haiku-4-5]", nil
		})
		defer ResetCLIRunner()

		desc := CallDescriptor{Agent: "captain", TaskID: 42, PromptVersion: "v18"}
		out, err := CallWithTranscript(context.Background(), desc,
			"system prompt body", "user prompt body",
			"", "", "", 1)
		if err != nil {
			t.Fatalf("call: %v", err)
		}
		if !strings.Contains(out, "ok response") {
			t.Fatalf("unexpected output: %q", out)
		}

		var rows int
		var agent, pv, completedAt, sysPrompt, usrPrompt, resp string
		var tokIn, tokOut int
		var cost float64
		if err := db.QueryRow(
			`SELECT COUNT(*) FROM LLMCallTranscripts WHERE task_id=42 AND agent='captain'`,
		).Scan(&rows); err != nil {
			t.Fatalf("count: %v", err)
		}
		if rows != 1 {
			t.Fatalf("expected exactly 1 transcript row, got %d", rows)
		}
		if err := db.QueryRow(
			`SELECT agent, prompt_version, call_completed_at, system_prompt, user_prompt, response_text, input_tokens, output_tokens, cost_usd
			   FROM LLMCallTranscripts WHERE task_id=42`,
		).Scan(&agent, &pv, &completedAt, &sysPrompt, &usrPrompt, &resp, &tokIn, &tokOut, &cost); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if agent != "captain" || pv != "v18" {
			t.Errorf("descriptor mis-stamped: agent=%q pv=%q", agent, pv)
		}
		if completedAt == "" {
			t.Errorf("happy path must set call_completed_at")
		}
		if sysPrompt != "system prompt body" || usrPrompt != "user prompt body" {
			t.Errorf("prompts not persisted faithfully: sys=%q usr=%q", sysPrompt, usrPrompt)
		}
		if !strings.Contains(resp, "ok response") {
			t.Errorf("response not persisted: %q", resp)
		}
		if tokIn != 100 || tokOut != 50 {
			t.Errorf("token counts mis-parsed: in=%d out=%d", tokIn, tokOut)
		}
		if cost <= 0 {
			t.Errorf("cost not computed (model=claude-haiku-4-5): %v", cost)
		}
	})

	t.Run("nil_db_falls_through", func(t *testing.T) {
		SetTranscriptDB(nil)
		SetCLIRunner(func(_ context.Context, _, _, _, _, _ string, _ int, _ time.Duration) (string, error) {
			return "stub-output", nil
		})
		defer ResetCLIRunner()
		out, err := CallWithTranscript(context.Background(),
			CallDescriptor{Agent: "captain", TaskID: 0},
			"sys", "usr", "", "", "", 1)
		if err != nil {
			t.Fatalf("nil-db path: %v", err)
		}
		if !strings.Contains(out, "stub-output") {
			t.Fatalf("CLI still must run when db absent: %q", out)
		}
	})

	t.Run("cancellation_leaves_completed_empty", func(t *testing.T) {
		db := store.InitHolocronDSN(":memory:")
		defer db.Close()
		SetTranscriptDB(db)
		defer SetTranscriptDB(nil)

		SetCLIRunner(func(_ context.Context, _, _, _, _, _ string, _ int, _ time.Duration) (string, error) {
			return "partial", errors.New("claude CLI cancelled by caller")
		})
		defer ResetCLIRunner()

		_, _ = CallWithTranscript(context.Background(),
			CallDescriptor{Agent: "medic", TaskID: 7},
			"sys", "usr", "", "", "", 1)

		var completedAt, resp string
		if err := db.QueryRow(
			`SELECT call_completed_at, response_text FROM LLMCallTranscripts WHERE agent='medic' AND task_id=7`,
		).Scan(&completedAt, &resp); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if completedAt != "" {
			t.Errorf("cancellation must leave completed_at empty (got %q)", completedAt)
		}
		if !strings.Contains(resp, "[error]") {
			t.Errorf("cancellation response should carry [error] excerpt; got %q", resp)
		}
	})

	t.Run("redaction_scrubs_secrets", func(t *testing.T) {
		db := store.InitHolocronDSN(":memory:")
		defer db.Close()
		SetTranscriptDB(db)
		defer SetTranscriptDB(nil)

		// Stub returns a fake response carrying a Bearer token and a
		// ghp_* PAT — both must be redacted in the persisted row.
		SetCLIRunner(func(_ context.Context, _, _, _, _, _ string, _ int, _ time.Duration) (string, error) {
			return "Authorization: Bearer abcd1234efghijklmnop9999\nLog: ghp_abcdefghijklmn123456789012345678901234abcdABCD012345", nil
		})
		defer ResetCLIRunner()

		systemPromptWithSecret := "API token: ghp_topsecretvalue1234567890abcdef"
		_, err := CallWithTranscript(context.Background(),
			CallDescriptor{Agent: "captain", TaskID: 1},
			systemPromptWithSecret, "user side", "", "", "", 1)
		if err != nil {
			t.Fatalf("call: %v", err)
		}

		var sys, resp string
		if err := db.QueryRow(
			`SELECT system_prompt, response_text FROM LLMCallTranscripts WHERE task_id=1`,
		).Scan(&sys, &resp); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if strings.Contains(sys, "ghp_topsecret") {
			t.Errorf("system_prompt not redacted: %q", sys)
		}
		if !strings.Contains(sys, "[REDACTED]") {
			t.Errorf("system_prompt should contain [REDACTED] marker: %q", sys)
		}
		if strings.Contains(resp, "ghp_abcdefghij") {
			t.Errorf("response_text PAT not redacted: %q", resp)
		}
		if strings.Contains(resp, "abcd1234efghijklmnop") {
			t.Errorf("response_text Bearer not redacted: %q", resp)
		}
		if !strings.Contains(resp, "[REDACTED]") {
			t.Errorf("response_text should contain [REDACTED] marker: %q", resp)
		}
	})

	t.Run("idempotence_two_calls_two_rows", func(t *testing.T) {
		db := store.InitHolocronDSN(":memory:")
		defer db.Close()
		SetTranscriptDB(db)
		defer SetTranscriptDB(nil)
		SetCLIRunner(func(_ context.Context, _, _, _, _, _ string, _ int, _ time.Duration) (string, error) {
			return "out\n[claude_usage: 1 input 2 output]\n[claude_model: claude-haiku-4-5]", nil
		})
		defer ResetCLIRunner()

		for i := 0; i < 2; i++ {
			if _, err := CallWithTranscript(context.Background(),
				CallDescriptor{Agent: "captain", TaskID: 99, PromptVersion: "v1"},
				fmt.Sprintf("sys-%d", i), fmt.Sprintf("usr-%d", i), "", "", "", 1); err != nil {
				t.Fatalf("call %d: %v", i, err)
			}
		}
		var rows int
		db.QueryRow(`SELECT COUNT(*) FROM LLMCallTranscripts WHERE task_id=99`).Scan(&rows)
		if rows != 2 {
			t.Fatalf("expected 2 rows after idempotence run, got %d", rows)
		}
	})

	t.Run("streaming_wrapper_records_row", func(t *testing.T) {
		db := store.InitHolocronDSN(":memory:")
		defer db.Close()
		SetTranscriptDB(db)
		defer SetTranscriptDB(nil)
		SetCLIRunner(func(_ context.Context, _, _, _, _, _ string, _ int, _ time.Duration) (string, error) {
			return "stream-output", nil
		})
		defer ResetCLIRunner()

		if _, err := CallWithTranscriptStreaming(context.Background(),
			CallDescriptor{Agent: "astromech", TaskID: 11},
			"full prompt body", "", "", "", "", 1, time.Minute, io.Discard); err != nil {
			t.Fatalf("stream call: %v", err)
		}
		var rows int
		db.QueryRow(`SELECT COUNT(*) FROM LLMCallTranscripts WHERE agent='astromech' AND task_id=11`).Scan(&rows)
		if rows != 1 {
			t.Fatalf("expected 1 streaming row, got %d", rows)
		}
	})

	t.Run("oneshot_wrapper_records_row", func(t *testing.T) {
		db := store.InitHolocronDSN(":memory:")
		defer db.Close()
		SetTranscriptDB(db)
		defer SetTranscriptDB(nil)
		SetCLIRunner(func(_ context.Context, _, _, _, _, _ string, _ int, _ time.Duration) (string, error) {
			return "oneshot-output", nil
		})
		defer ResetCLIRunner()

		if _, err := CallWithTranscriptOneShot(context.Background(),
			CallDescriptor{Agent: "auditor", TaskID: 21},
			"audit prompt", "", "", "", "", 1, time.Minute); err != nil {
			t.Fatalf("oneshot call: %v", err)
		}
		var rows int
		db.QueryRow(`SELECT COUNT(*) FROM LLMCallTranscripts WHERE agent='auditor' AND task_id=21`).Scan(&rows)
		if rows != 1 {
			t.Fatalf("expected 1 oneshot row, got %d", rows)
		}
	})
}

// TestFormatCallSummary covers the small UI helper — Drill list-view
// shows agent · prompt-version · token counts · cost.
func TestFormatCallSummary(t *testing.T) {
	got := FormatCallSummary("captain", "v18", 100, 50, 0.0234)
	if !strings.Contains(got, "captain") || !strings.Contains(got, "v18") || !strings.Contains(got, "100/50") {
		t.Errorf("summary: %q", got)
	}
	got = FormatCallSummary("medic", "", 1, 1, 0)
	if !strings.Contains(got, "(unversioned)") {
		t.Errorf("blank pv should render as (unversioned): %q", got)
	}
}

func TestTruncateForDrill(t *testing.T) {
	short := strings.Repeat("a", 100)
	if got := TruncateForDrill(short, 200); got != short {
		t.Errorf("short string mutated: %q", got)
	}
	long := strings.Repeat("b", 500)
	got := TruncateForDrill(long, 200)
	if !strings.HasPrefix(got, strings.Repeat("b", 200)) {
		t.Errorf("truncate didn't preserve prefix: %q", got[:50])
	}
	if !strings.Contains(got, "truncated") {
		t.Errorf("truncate must announce truncation: %q", got[:50])
	}
}

func TestEnsureNoControlChars(t *testing.T) {
	in := "ok\nfine\twith\x00bad"
	got := EnsureNoControlChars(in)
	if strings.ContainsRune(got, 0) {
		t.Errorf("control char survived: %q", got)
	}
	if !strings.Contains(got, "\n") || !strings.Contains(got, "\t") {
		t.Errorf("newline/tab should survive: %q", got)
	}
}
