// Package claude — D3 P6B.1 transcript capture wrapper.
//
// Every Claude CLI invocation in production code routes through one of the
// CallWithTranscript* helpers here, which records a redacted row into
// LLMCallTranscripts before/after the underlying call. The wrapper is the
// substrate for Drill (6B.3-6B.8), Replay (6B.7), and the cost-attribution
// queries in Reflection (6B.11).
//
// Flow:
//  1. INSERT a "started" row carrying redacted system_prompt + user_prompt,
//     call_started_at = now, call_completed_at = '' (empty sentinel).
//  2. Run the underlying CLI helper (AskClaudeCLIContext / RunCLIStreamingContext).
//  3. UPDATE the row with redacted response_text, parsed token counts,
//     parsed model id, and call_completed_at = now.
//  4. On ctx cancellation or CLI failure: still UPDATE the row with whatever
//     partial output exists; call_completed_at stays '' so forensic queries
//     can distinguish "completed" from "interrupted." A best-effort error
//     message is appended to response_text so the operator drilling into
//     the row can see why it terminated.
//
// Anti-cheat (Fix #10):
//   - Redaction at write time is enforced — every prompt + response field
//     funnels through store.RedactSecrets BEFORE the SQL insert/update.
//   - When db is nil (early-boot CLI tools, unit tests that don't care
//     about transcripts), the wrapper silently degrades to the underlying
//     helper without recording anything. This is the "no DB attached"
//     case, NOT a bypass — production code wires db at daemon startup.
//   - Pattern P31 walks production code and asserts every direct
//     AskClaudeCLI* / RunCLIStreaming* / RunCLI call site is reachable
//     only through this wrapper or via an explicit allowlist entry with
//     a one-line truthful rationale.
package claude

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"force-orchestrator/internal/store"
)

// CallDescriptor identifies the agent + task + prompt version for one
// Claude CLI call. The triplet stamps the LLMCallTranscripts row so
// downstream Drill / Replay surfaces can filter by agent, task, or
// prompt-version cohort.
type CallDescriptor struct {
	Agent         string // canonical agent id (e.g. "captain", "medic", "astromech")
	TaskID        int    // 0 when the call is task-less (boot, dog-tick, etc.)
	PromptVersion string // free-form prompt-version label (e.g. "v18", "narrative-v2"); "" allowed
}

// transcriptDB is the active *sql.DB used for transcript inserts. Tests
// install a memory DB via SetTranscriptDB; production wires it at daemon
// startup. nil means "drop transcripts on the floor" — the underlying
// CLI helper still runs.
var (
	transcriptDB   *sql.DB
	transcriptDBMu sync.RWMutex
)

// SetTranscriptDB installs the *sql.DB the wrapper uses for INSERT /
// UPDATE on LLMCallTranscripts. Idempotent — repeated calls overwrite
// the active handle. Pass nil to detach (used by some tests).
func SetTranscriptDB(db *sql.DB) {
	transcriptDBMu.Lock()
	transcriptDB = db
	transcriptDBMu.Unlock()
}

// activeTranscriptDB returns the currently installed DB, or nil if none.
func activeTranscriptDB() *sql.DB {
	transcriptDBMu.RLock()
	defer transcriptDBMu.RUnlock()
	return transcriptDB
}

// CallWithTranscript wraps claude.AskClaudeCLIContext: records a transcript
// row, runs the call, updates the row with response + cost + tokens.
// Returns the same (output, error) pair the underlying helper returns.
//
// Redaction order matters: prompts are redacted via store.RedactSecrets
// at the point of write, NOT mutated for the actual CLI call. The CLI
// has its own scrubber (claude.ScrubInbound) for inbound boundary; the
// wrapper's redaction protects the stored *transcript row* — different
// chokepoint, same defense-in-depth.
//
// D7: the wrapper auto-stamps the call ctx with WithClaudeCallContext
// using the descriptor's Agent + TaskID so the treatments.Apply hook
// downstream sees the agent name when computing experiment enrollment.
// Without this, every CallWithTranscript-routed call hits
// invokeTreatmentApplyHook with agent="" and never matches an
// experiment by subject_agent. Pre-D7 the only agent threading
// WithClaudeCallContext was Captain (for context-size attribution);
// the model-override seam needs every agent's name on the hook ctx.
func CallWithTranscript(ctx context.Context, desc CallDescriptor, systemPrompt, userPrompt, allowedTools, disallowedTools, mcpConfig string, maxTurns int) (string, error) {
	db := activeTranscriptDB()
	rowID := insertTranscriptStart(db, desc, systemPrompt, userPrompt)

	ctx = ensureCallCtx(ctx, desc)
	out, err := AskClaudeCLIContext(ctx, systemPrompt, userPrompt, allowedTools, disallowedTools, mcpConfig, maxTurns)

	updateTranscriptEnd(db, rowID, out, err)
	return out, err
}

// CallWithTranscriptStreaming wraps claude.RunCLIStreamingContext.
// Identical bookkeeping shape to CallWithTranscript. The streaming-vs-
// one-shot split mirrors the existing CLI helper layering — astromechs
// stream (so the operator can tail progress), Captain / Medic / Council
// don't (one-shot reasoning calls). Both paths land the same row shape
// so Drill renders them uniformly.
func CallWithTranscriptStreaming(ctx context.Context, desc CallDescriptor, prompt, allowedTools, disallowedTools, mcpConfig, dir string, maxTurns int, timeout time.Duration, w io.Writer, extraEnv ...string) (string, error) {
	db := activeTranscriptDB()
	// Streaming path doesn't have a separate system_prompt / user_prompt
	// split — the caller assembles the full prompt upstream. Store the
	// whole prompt under user_prompt and leave system_prompt empty so
	// Drill renders it as a single block. The shape is consistent with
	// what astromech callers already do (full prompt assembled in
	// astromech.go:assembleAstromechPrompt).
	rowID := insertTranscriptStart(db, desc, "", prompt)

	// D7: stamp the call ctx with desc.Agent + desc.TaskID so the
	// treatments-apply hook downstream identifies the subject agent.
	ctx = ensureCallCtx(ctx, desc)
	out, err := RunCLIStreamingContext(ctx, prompt, allowedTools, disallowedTools, mcpConfig, dir, maxTurns, timeout, w, extraEnv...)

	updateTranscriptEnd(db, rowID, out, err)
	return out, err
}

// CallWithTranscriptOneShot wraps claude.RunCLI (the non-streaming
// worktree-aware path used by Auditor / Investigator). Same bookkeeping
// shape.
func CallWithTranscriptOneShot(ctx context.Context, desc CallDescriptor, prompt, allowedTools, disallowedTools, mcpConfig, dir string, maxTurns int, timeout time.Duration) (string, error) {
	db := activeTranscriptDB()
	rowID := insertTranscriptStart(db, desc, "", prompt)

	// D7: stamp the call ctx with desc.Agent + desc.TaskID. RunCLI does
	// not currently fire the treatments-apply hook (Auditor / Investigator
	// path), but stamping the ctx is a uniform shape and future-proofs
	// the seam if RunCLI gains a hook later.
	ctx = ensureCallCtx(ctx, desc)
	out, err := RunCLI(ctx, prompt, allowedTools, disallowedTools, mcpConfig, dir, maxTurns, timeout)

	updateTranscriptEnd(db, rowID, out, err)
	return out, err
}

// ensureCallCtx stamps the call ctx with the descriptor's Agent +
// TaskID via WithClaudeCallContext so downstream seams (the
// treatments-apply hook, CheckContextSize) can identify the subject
// agent. If the caller already wrapped the ctx (e.g. Captain wires it
// for byte-attribution contributions), this is a no-op — the existing
// value wins because context.WithValue layers, and the hook reads
// from the outermost layer.
//
// Empty desc.Agent leaves the ctx untouched — there are tests + early
// boot paths that pass a zero-value descriptor and we don't want to
// stamp "" over a parent layer's already-correct agent name.
func ensureCallCtx(ctx context.Context, desc CallDescriptor) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if desc.Agent == "" {
		return ctx
	}
	// Prefer an existing inner stamp — Captain wires its own with
	// SourceContribution so we don't want to overwrite that. A simple
	// "is anything stamped?" check via ClaudeCallContext keeps the
	// no-op invariant.
	if existingAgent, _, _, ok := ClaudeCallContext(ctx); ok && existingAgent != "" {
		return ctx
	}
	return WithClaudeCallContext(ctx, desc.Agent, desc.TaskID, nil)
}

// insertTranscriptStart writes the pre-call row and returns the row id.
// Returns 0 when db is nil OR the insert fails — the wrapper treats 0 as
// "skip the update step" so a transient INSERT failure doesn't poison
// the live CLI call.
func insertTranscriptStart(db *sql.DB, desc CallDescriptor, systemPrompt, userPrompt string) int64 {
	if db == nil {
		return 0
	}
	// Redact at write time — Fix #10 invariant. Every field that lands
	// in the row passes through store.RedactSecrets, regardless of
	// where the prompt came from.
	redactedSys := store.RedactSecrets(systemPrompt)
	redactedUsr := store.RedactSecrets(userPrompt)
	startedAt := store.NowSQLite()

	res, err := db.Exec(
		`INSERT INTO LLMCallTranscripts
		   (task_id, agent, prompt_version, call_started_at, call_completed_at,
		    system_prompt, user_prompt, response_text, tool_calls_json,
		    cost_usd, input_tokens, output_tokens, cache_read_tokens, cache_creation_tokens, archived_at)
		 VALUES (?, ?, ?, ?, '', ?, ?, '', '[]', 0, 0, 0, 0, 0, '')`,
		desc.TaskID, desc.Agent, desc.PromptVersion, startedAt, redactedSys, redactedUsr,
	)
	if err != nil {
		// Best-effort — never fail the live CLI call because the
		// audit shadow couldn't write. Production logs surface the
		// insert error via store-level instrumentation; tests that
		// care about the row assert via SELECT.
		return 0
	}
	id, _ := res.LastInsertId()
	return id
}

// updateTranscriptEnd writes response + duration + parsed-token fields
// onto the started row. When err != nil, response_text gets a redacted
// excerpt of the failure plus whatever output the CLI produced before
// dying. call_completed_at remains '' on cancellation/error so forensic
// queries can distinguish completed from interrupted runs (per the
// brief).
func updateTranscriptEnd(db *sql.DB, rowID int64, output string, err error) {
	if db == nil || rowID == 0 {
		return
	}
	// Redact at write time on the response side too. Output may carry
	// gh stderr (token prefixes), Bash tool traces (Bearer headers),
	// etc. — all funnel through RedactSecrets.
	redactedOut := store.RedactSecrets(output)

	// Parse token usage + model from the embedded annotations
	// (parseJSONResult / parseStreamEvent injected these into output
	// per the upstream CLI runner shape). cost is computed via
	// pricing.CostUSD; on parse failure we record zeros so downstream
	// queries don't NaN.
	tokIn, tokOut := ParseTokenUsage(output)
	model := ParseModel(output)
	cost := CostUSD(model, tokIn, tokOut)

	completed := store.NowSQLite()
	if err != nil {
		// Cancellation / failure path: leave call_completed_at empty
		// so Drill's "incomplete" filter catches this row, and append
		// the redacted error to response_text so the operator sees
		// the cause when they expand the row.
		completed = ""
		errExcerpt := store.RedactSecrets(err.Error())
		if redactedOut == "" {
			redactedOut = "[error] " + errExcerpt
		} else {
			redactedOut = redactedOut + "\n[error] " + errExcerpt
		}
	}

	_, _ = db.Exec(
		`UPDATE LLMCallTranscripts
		    SET response_text     = ?,
		        cost_usd          = ?,
		        input_tokens      = ?,
		        output_tokens     = ?,
		        call_completed_at = ?
		  WHERE id = ?`,
		redactedOut, cost, tokIn, tokOut, completed, rowID,
	)
}

// FormatCallSummary returns a one-line "<agent> · <prompt_version> ·
// <input>/<output> tok · $<cost>" summary for use in Drill list views.
// Pure formatting helper — no DB access.
func FormatCallSummary(agent, promptVersion string, tokIn, tokOut int, cost float64) string {
	pv := promptVersion
	if pv == "" {
		pv = "(unversioned)"
	}
	return fmt.Sprintf("%s · %s · %d/%d tok · $%.4f",
		agent, pv, tokIn, tokOut, cost)
}

// TruncateForDrill caps a transcript text field at the size used in the
// list-view DOM. Drill expand-on-click loads the full body; the list
// view shows the head only so a 200 KB transcript doesn't blow the SPA
// memory footprint.
func TruncateForDrill(s string, max int) string {
	if max <= 0 || len(s) <= max {
		return s
	}
	return s[:max] + fmt.Sprintf("\n…[truncated %d bytes; expand to load full]\n", len(s)-max)
}

// ensureNoControlChars is a defence-in-depth pass that strips ASCII
// control characters (except newline + tab) from text destined for the
// dashboard. Kept exported because Drill's event renderer uses it on
// stdout/stderr excerpts before SPA injection.
func ensureNoControlChars(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if r == '\n' || r == '\t' || r >= 0x20 {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// EnsureNoControlChars exports ensureNoControlChars for the dashboard
// renderer. Kept as a thin alias so the unexported helper can stay in
// the same translation unit as the wrapper for inlining.
func EnsureNoControlChars(s string) string { return ensureNoControlChars(s) }
