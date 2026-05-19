// Package claude — context_overflow.go (D2 T1-2).
//
// This file holds the per-agent context-size enforcement layer that
// fires at the ingress of every claude.AskClaudeCLIContext /
// RunCLIStreamingContext call. The flow is:
//
//  1. The Spawn-loop callsite assembles a prompt via
//     agents.PromptBuilder, capturing per-source byte contributions.
//  2. The callsite stamps the contributions onto the call ctx via
//     WithClaudeCallContext (claude.WithClaudeCallContext).
//  3. The ingress wrapper in claude.go calls CheckContextSize to:
//       - sum the prompt size,
//       - look up the per-agent cap from SystemConfig,
//       - persist a PromptByteAttribution row set per call,
//       - if total > cap: log [CONTEXT OVERFLOW] + emit operator mail
//         (deduped per-agent-per-day) + invoke the SummarizeFn,
//       - if the summary still exceeds the cap, return ErrContextOverflow.
//
// The Summarizer is injected by the caller (Spawn-loop wires the
// librarian client's SummarizeForContextOverflow into a closure) so
// claude has no compile-time dependency on librarian.

package claude

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"sort"
	"strings"

	"force-orchestrator/internal/store"
)

// ErrContextOverflow is returned when an LLM call's prompt exceeds the
// per-agent byte cap AND the summarizer's reduction still exceeds the
// cap. Callers route this through their existing infra-failure path
// (handleInfraFailure → retry budget); never silently drop.
var ErrContextOverflow = errors.New("claude: context-size overflow even after summarize")

// claudeCallCtxKey is an unexported type so external packages can't
// collide with our context value (Go context-key idiom).
type claudeCallCtxKey struct{}

// claudeCallCtxValue carries the agent name + per-call attribution
// from a Spawn-loop callsite down through the claude package.
type claudeCallCtxValue struct {
	Agent         string
	TaskID        int
	Contributions []store.SourceContribution
}

// WithClaudeCallContext returns a derived context carrying the agent
// name, task ID, and per-source byte contributions for the upcoming
// claude call. Pass it to AskClaudeCLIContext / RunCLIStreamingContext;
// the ingress reads it back via ClaudeCallContext to compute the size
// cap and to record PromptByteAttribution rows. contributions may be
// nil — the ingress records an "other"-tagged total as a fallback.
//
// This is the sole canonical way to thread the agent + task identity
// into a claude call. The previous "thread `agent string` parameter
// through every call site" approach was rejected as a touch-everything
// refactor; the ctx-value form lets the existing dozens of callers
// adopt incrementally.
func WithClaudeCallContext(ctx context.Context, agent string, taskID int, contributions []store.SourceContribution) context.Context {
	return context.WithValue(ctx, claudeCallCtxKey{}, claudeCallCtxValue{
		Agent:         agent,
		TaskID:        taskID,
		Contributions: contributions,
	})
}

// ClaudeCallContext extracts the agent / taskID / contributions
// stamped via WithClaudeCallContext. Returns ("", 0, nil, false) when
// no value is set. Used by CheckContextSize.
func ClaudeCallContext(ctx context.Context) (agent string, taskID int, contributions []store.SourceContribution, ok bool) {
	if ctx == nil {
		return "", 0, nil, false
	}
	v, ok := ctx.Value(claudeCallCtxKey{}).(claudeCallCtxValue)
	if !ok {
		return "", 0, nil, false
	}
	return v.Agent, v.TaskID, v.Contributions, true
}

// requestedModelCtxKey is the unexported context-value type used by
// the treatments-apply hook to communicate the model an active
// experiment treatment selected for this call. Read by the runner
// just before exec'ing the `claude` binary so the model id flows onto
// the argv as `--model <id>`. Empty / unset means "use the claude
// binary's default" — the historical pre-D7 behaviour.
//
// D7: this is the swap-point that lets a paired-runs experiment
// downgrade an agent to Haiku per-arm. Set by withRequestedModel,
// read by RequestedModel + buildClaudeArgs.
type requestedModelCtxKey struct{}

// withRequestedModel returns a derived ctx carrying the model id the
// caller wants the next claude exec to run under. Empty model means
// "no override" — the runner omits --model and the claude binary
// picks its default.
func withRequestedModel(ctx context.Context, model string) context.Context {
	if ctx == nil {
		return ctx
	}
	return context.WithValue(ctx, requestedModelCtxKey{}, model)
}

// RequestedModel returns the model id the treatments-apply hook (or
// any other in-flight caller) has requested for this Claude call, or
// "" if none is set. Exported so call-site tests can assert that the
// hook stashed an experiment-arm's model id in the ctx; production
// code reads it indirectly through buildClaudeArgs.
func RequestedModel(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	if v, ok := ctx.Value(requestedModelCtxKey{}).(string); ok {
		return v
	}
	return ""
}

// SystemConfig keys for the per-agent prompt-byte budget. The default
// cap is conservative (200 KB) but plenty of headroom for healthy
// captain / medic prompts. Per-agent overrides land at agent_max_prompt_bytes_<agent>.
const (
	configKeyDefaultMaxPromptBytes = "agent_max_prompt_bytes_default"
	defaultMaxPromptBytes          = 200_000
)

// AgentMaxPromptBytes returns the byte cap applied to the named
// agent's assembled prompt. Per-agent override key is
// `agent_max_prompt_bytes_<agent>`; falls back to
// `agent_max_prompt_bytes_default`; falls back to
// defaultMaxPromptBytes (200 KB). Negative or unparseable values are
// ignored (returns the default).
func AgentMaxPromptBytes(db *sql.DB, agent string) int {
	if db == nil {
		return defaultMaxPromptBytes
	}
	if agent != "" {
		key := "agent_max_prompt_bytes_" + agent
		raw := store.GetConfig(db, key, "")
		if v, ok := parsePositiveInt(raw); ok {
			return v
		}
	}
	raw := store.GetConfig(db, configKeyDefaultMaxPromptBytes, "")
	if v, ok := parsePositiveInt(raw); ok {
		return v
	}
	return defaultMaxPromptBytes
}

func parsePositiveInt(s string) (int, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, false
	}
	var n int
	if _, err := fmt.Sscanf(s, "%d", &n); err != nil {
		return 0, false
	}
	if n <= 0 {
		return 0, false
	}
	return n, true
}

// SummarizeFn is the contract the librarian client satisfies for
// SummarizeForContextOverflow. claude.go calls this when a prompt
// exceeds the per-agent cap; the implementation is expected to do a
// single-turn Claude call (Haiku-tier model when available) and return
// a shorter prompt at or below targetBytes.
//
// claude has no compile-time dependency on librarian; the Spawn-loop
// (or test) wires a closure into SetSummarizer at startup.
type SummarizeFn func(ctx context.Context, prompt string, targetBytes int) (string, error)

// activeSummarizer is the installed summarizer. nil means
// CheckContextSize falls through to ErrContextOverflow on overflow
// without attempting a reduction. Tests install via SetSummarizerForTest.
var activeSummarizer SummarizeFn

// activeContextSizeDBHandle is the DB the ingress check uses to read
// SystemConfig per-agent caps and to persist PromptByteAttribution
// rows. cmdDaemon stamps this at startup via SetContextSizeDB; when
// nil, the ingress runs with the default cap and skips persistence
// (graceful fallback for unit tests that don't fixture a DB).
var activeContextSizeDBHandle *sql.DB

// SetContextSizeDB installs the DB the context-size guard uses for
// SystemConfig reads + PromptByteAttribution writes. cmdDaemon wires
// this once at startup. Pass nil to clear (tests use t.Cleanup ->
// SetContextSizeDB(nil)).
func SetContextSizeDB(db *sql.DB) {
	activeContextSizeDBHandle = db
}

// activeContextSizeDB returns the installed handle (or nil).
func activeContextSizeDB() *sql.DB { return activeContextSizeDBHandle }

// TreatmentApplyHook is the function signature the daemon installs to
// route every Claude CLI invocation through `treatments.Apply` (D3
// Phase 2 live mode — holdout check + experiment enrollment + descriptor
// rewrite). Called once per Claude call at the top of
// AskClaudeCLIContext / RunCLIStreamingContext.
//
// The hook receives the (agent, taskID) tuple already on the call ctx
// via WithClaudeCallContext + the daemon's DB handle. It returns:
//
//   - modelOverride: the model id the active experiment treatment
//     selected for this call (e.g. "claude-haiku-4-5-20251001"), or
//     "" when no experiment is enrolling this unit (descriptor
//     unchanged). The runner threads this onto the argv as
//     `--model <id>`. D7 makes this the swap-point that lets a
//     paired-runs experiment downgrade an agent to Haiku per-arm.
//   - err: non-nil aborts the call. Phase 1 always returns nil even
//     on a journal write failure (fail-open). Phase 2+ may return a
//     real error if a treatment apply must hard-fail (rare).
//
// Internal/claude does not import internal/treatments to avoid binding
// the runtime package to a specific treatment implementation. The
// daemon wires the closure at startup.
type TreatmentApplyHook func(ctx context.Context, agent string, taskID int) (modelOverride string, err error)

var activeTreatmentApplyHook TreatmentApplyHook

// SetTreatmentApplyHook installs the treatment-apply hook. Called by
// cmdDaemon at startup (after SetContextSizeDB). Pass nil to clear
// (tests use t.Cleanup -> SetTreatmentApplyHook(nil)).
func SetTreatmentApplyHook(fn TreatmentApplyHook) {
	activeTreatmentApplyHook = fn
}

// invokeTreatmentApplyHook fires the installed hook with the call's
// (agent, taskID) tuple. No-op when no hook is installed (tests, early
// boot). The returned ctx carries any modelOverride the hook produced
// so the downstream runner sees it via RequestedModel(ctx). Errors are
// propagated to the caller — Phase 1 always returns nil so the
// upstream caller never sees a non-nil error from this path.
func invokeTreatmentApplyHook(ctx context.Context) (context.Context, error) {
	hook := activeTreatmentApplyHook
	if hook == nil {
		return ctx, nil
	}
	agent, taskID, _, _ := ClaudeCallContext(ctx)
	model, err := hook(ctx, agent, taskID)
	if err != nil {
		return ctx, err
	}
	if model != "" {
		ctx = withRequestedModel(ctx, model)
	}
	return ctx, nil
}

// SetSummarizer installs the active summarizer. cmdDaemon wires the
// librarian.Client.SummarizeForContextOverflow closure here at startup.
// Pass nil to clear (tests use ResetSummarizerForTest).
func SetSummarizer(fn SummarizeFn) {
	activeSummarizer = fn
}

// SetSummarizerForTest is the test alias for SetSummarizer; named
// distinctly so a grep for SetSummarizer in production wiring stays
// clean.
func SetSummarizerForTest(fn SummarizeFn) {
	activeSummarizer = fn
}

// ResetSummarizerForTest clears the installed summarizer. t.Cleanup() it.
func ResetSummarizerForTest() {
	activeSummarizer = nil
}

// CheckContextSize is the ingress guard called from
// AskClaudeCLIContext / RunCLIStreamingContext after the prompts are
// concatenated. It returns:
//
//   - (revisedPrompt, nil) on success — either the prompt was under cap
//     OR the summarizer reduced it to under cap. revisedPrompt is what
//     the runner should send (== fullPrompt if no summarize was needed).
//   - ("", ErrContextOverflow) when cap was exceeded AND the summarizer
//     was unavailable OR couldn't reduce below cap.
//   - the error from the summarizer (wrapped) on summarizer failure;
//     the caller routes this through handleInfraFailure.
//
// db may be nil — when so, the cap check uses the default (200 KB)
// and PromptByteAttribution rows are NOT persisted (graceful fallback
// for tests that don't fixture a DB).
func CheckContextSize(ctx context.Context, db *sql.DB, fullPrompt string) (string, error) {
	agent, taskID, contributions, _ := ClaudeCallContext(ctx)
	if agent == "" {
		// Unattributed call — record under "unknown" so the
		// surface is visible. Cap still applies; we use the
		// default since no per-agent key exists.
		agent = "unknown"
	}
	cap := AgentMaxPromptBytes(db, agent)
	totalBytes := len(fullPrompt)

	// Persist per-source byte attribution. If the caller didn't
	// supply contributions, record a single "other" row with the
	// total — the operator still sees there was a call of this size.
	if db != nil {
		toRecord := contributions
		if len(toRecord) == 0 {
			toRecord = []store.SourceContribution{
				{SourceTag: "other", Bytes: totalBytes},
			}
		}
		if err := store.RecordSourceTags(db, taskID, agent, store.NowSQLite(), toRecord); err != nil {
			// Non-fatal: attribution is observability, not
			// correctness. Log and continue.
			log.Printf("[CLAUDE] RecordSourceTags failed agent=%s task=%d: %v", agent, taskID, err)
		}
	}

	if totalBytes <= cap {
		return fullPrompt, nil
	}

	// Overflow — log + operator mail (deduped per-agent-per-day) +
	// summarize attempt.
	top3 := topNContributions(contributions, totalBytes, 3)
	log.Printf("[CONTEXT OVERFLOW] agent=%s task=%d size=%d cap=%d top3=%s",
		agent, taskID, totalBytes, cap, top3)

	if db != nil {
		emitContextOverflowMail(db, agent, taskID, totalBytes, cap, top3)
	}

	if activeSummarizer == nil {
		return "", ErrContextOverflow
	}
	summarized, err := activeSummarizer(ctx, fullPrompt, cap)
	if err != nil {
		return "", fmt.Errorf("claude: SummarizeForContextOverflow: %w", err)
	}
	if len(summarized) > cap {
		log.Printf("[CONTEXT OVERFLOW] agent=%s task=%d summarize-still-too-large size=%d cap=%d",
			agent, taskID, len(summarized), cap)
		return "", ErrContextOverflow
	}
	// Record the summarized version too — the post-summarize prompt
	// is what the runner actually sends, and its byte total is what
	// the cost projection should use.
	if db != nil {
		// post-summarize attribution rolls up under "other" — we
		// don't know the source-tag breakdown of the librarian's
		// summary output.
		_ = store.RecordSourceTags(db, taskID, agent, store.NowSQLite(),
			[]store.SourceContribution{{SourceTag: "other", Bytes: len(summarized)}})
	}
	return summarized, nil
}

// topNContributions renders the largest n source contributions as a
// "tag=Xkb(Y%)" list. n=0 returns "".
func topNContributions(contribs []store.SourceContribution, total, n int) string {
	if n <= 0 || len(contribs) == 0 || total <= 0 {
		return ""
	}
	// Copy + sort descending by bytes.
	sorted := make([]store.SourceContribution, len(contribs))
	copy(sorted, contribs)
	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].Bytes != sorted[j].Bytes {
			return sorted[i].Bytes > sorted[j].Bytes
		}
		return sorted[i].SourceTag < sorted[j].SourceTag
	})
	if len(sorted) < n {
		n = len(sorted)
	}
	parts := make([]string, n)
	for i := 0; i < n; i++ {
		pct := int(float64(sorted[i].Bytes) / float64(total) * 100)
		parts[i] = fmt.Sprintf("%s=%dKB(%d%%)", sorted[i].SourceTag, sorted[i].Bytes/1024, pct)
	}
	return strings.Join(parts, ",")
}

// emitContextOverflowMail sends a single operator mail per agent per
// day. Dedup uses Fleet_Mail subject prefix `[CONTEXT OVERFLOW]
// agent=<agent>` and the day stamp.
func emitContextOverflowMail(db *sql.DB, agent string, taskID, total, cap int, top3 string) {
	day := store.NowSQLite()[:10] // YYYY-MM-DD
	subject := fmt.Sprintf("[CONTEXT OVERFLOW] agent=%s day=%s", agent, day)

	// Dedup: skip if Fleet_Mail already has a row with this subject prefix today.
	var existing int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM Fleet_Mail WHERE subject = ? AND date(created_at) = ?`,
		subject, day,
	).Scan(&existing); err == nil && existing > 0 {
		return
	}

	body := fmt.Sprintf(
		"Agent %s assembled a prompt of %d bytes (cap=%d) on task %d.\nTop sources: %s\n\n"+
			"Summarize attempt was triggered; if successful, the call proceeded "+
			"with a reduced prompt. If unsuccessful, the call returned "+
			"ErrContextOverflow and the task was routed through handleInfraFailure.\n\n"+
			"This mail is deduplicated per-agent-per-day; only the first overflow "+
			"of the day for this agent generates a notification.",
		agent, total, cap, taskID, top3)
	store.SendMail(db, "claude-context-guard", "operator", subject, body, taskID, store.MailTypeAlert)
}
