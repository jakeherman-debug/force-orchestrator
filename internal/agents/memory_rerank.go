package agents

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"force-orchestrator/internal/claude"
	"force-orchestrator/internal/store"
	"force-orchestrator/internal/util"
)

// ── LLM re-ranker for fleet memories ────────────────────────────────────────
//
// FTS5 + BM25 gives us decent RECALL but often poor PRECISION: it ranks by
// lexical token overlap, which doesn't capture semantic relevance. A memory
// about "authentication handler" may share tokens with a completely
// unrelated task just because both contain "handler" and "fix". The result
// is that astromechs often see top-10 FTS hits where only 1-2 are actually
// on-topic — noise that dilutes the useful signal.
//
// This re-ranker is a precision stage. The pipeline becomes:
//
//   1. FTS5 returns up to N=20 candidates (what GetFleetMemories already does).
//   2. Claude receives the current task + all candidate memory summaries and
//      picks the ≤K truly-relevant ones, in the order the agent should read
//      them.
//   3. The re-ranked list is injected into the agent's context.
//
// A zero-result return is EXPECTED when none of the candidates are actually
// relevant — this is the intended behavior (see the task-248 postmortem:
// irrelevant memories poison agent reasoning more than missing ones do).
//
// Graceful degradation: if Claude errors or returns malformed output, we
// fall through to the original FTS order trimmed to K. The feature is gated
// by SystemConfig key "memory_rerank_enabled" (default "1"); set to "0" to
// bypass the LLM call and use pure FTS.

// rerankSystemPrompt is the classifier prompt. Response must be strict JSON.
const rerankSystemPrompt = `You rank fleet-memory candidates by relevance to a current task.

You will receive:
  - CURRENT TASK: the task an agent is about to work on
  - CANDIDATES: a numbered list of memories from prior tasks, each with an outcome (success/failure) and a short summary. These were retrieved via keyword search and are not necessarily relevant.

Select the candidates that are GENUINELY useful for the current task — either:
  - they describe prior work on the same subsystem/file/pattern that the agent should know about, or
  - they describe a prior failure on a related approach that the agent should avoid, or
  - they document a non-obvious constraint, decision, or gotcha relevant to the current task.

Do NOT select a memory just because it shares keywords. A memory about "fixed auth bug" is not relevant to a current task about "improve dashboard layout" even if both touch the same repo.

It is correct and expected to return ZERO selections when none of the candidates are actually relevant. Irrelevant memories mislead agents; omit them.

Order your selections by relevance: the most relevant first.

Respond ONLY with valid JSON (no markdown, no preamble):
{
  "relevant_ids": [2, 5, 1],
  "reasoning": "one short paragraph: which candidates are relevant and why"
}`

type rerankResponse struct {
	RelevantIDs []int  `json:"relevant_ids"`
	Reasoning   string `json:"reasoning"`
}

// RerankFleetMemories takes an FTS candidate list and returns the subset the
// LLM considers truly relevant to the given task, preserving the LLM's
// ranking. The keepLimit caps the final result size.
//
// If re-ranking is disabled, the LLM errors, or candidates is shorter than
// 2, the input is returned trimmed to keepLimit — re-ranking 0 or 1 items
// isn't worth a Claude call.
func RerankFleetMemories(
	db *sql.DB,
	taskPayload string,
	candidates []store.FleetMemoryEntry,
	keepLimit int,
	logger interface{ Printf(string, ...any) },
) []store.FleetMemoryEntry {
	if len(candidates) == 0 {
		return nil
	}
	if keepLimit <= 0 {
		keepLimit = 5
	}
	if len(candidates) <= 1 {
		return trimCandidates(candidates, keepLimit)
	}
	if !memoryRerankEnabled(db) {
		return trimCandidates(candidates, keepLimit)
	}

	userPrompt := buildRerankPrompt(taskPayload, candidates)

	// No tools needed — the re-ranker is purely textual. Low max-turns so we
	// don't pay for extended reasoning.
	raw, err := claude.AskClaudeCLI(rerankSystemPrompt, userPrompt, "", 2)
	if err != nil {
		logger.Printf("memory-rerank: Claude failed (%v) — falling back to FTS order", err)
		return trimCandidates(candidates, keepLimit)
	}

	var resp rerankResponse
	if parseErr := json.Unmarshal([]byte(claude.ExtractJSON(raw)), &resp); parseErr != nil {
		logger.Printf("memory-rerank: parse error (%v) — falling back to FTS order; raw=%s",
			parseErr, util.TruncateStr(raw, 200))
		return trimCandidates(candidates, keepLimit)
	}

	// Map 1-based IDs back to the candidate list. Skip out-of-range and
	// de-dup. Preserve the order the LLM chose.
	byIdx := make(map[int]bool, len(resp.RelevantIDs))
	out := make([]store.FleetMemoryEntry, 0, keepLimit)
	for _, id := range resp.RelevantIDs {
		idx := id - 1
		if idx < 0 || idx >= len(candidates) {
			continue
		}
		if byIdx[idx] {
			continue
		}
		byIdx[idx] = true
		out = append(out, candidates[idx])
		if len(out) >= keepLimit {
			break
		}
	}

	logger.Printf("memory-rerank: FTS returned %d candidates, LLM kept %d (%s)",
		len(candidates), len(out), util.TruncateStr(resp.Reasoning, 120))

	// Zero selections is a valid answer — it means none of the FTS candidates
	// were truly relevant. Trust the LLM and return empty.
	return out
}

// buildRerankPrompt formats the LLM input. Each candidate is numbered from 1
// for stable round-trip referencing (the LLM's "relevant_ids" are 1-based).
func buildRerankPrompt(taskPayload string, candidates []store.FleetMemoryEntry) string {
	var b strings.Builder
	b.WriteString("CURRENT TASK:\n")
	b.WriteString(util.TruncateStr(taskPayload, 1500))
	b.WriteString("\n\nCANDIDATES:\n")
	for i, c := range candidates {
		fmt.Fprintf(&b, "%d. [%s — task #%d] %s\n", i+1, c.Outcome, c.TaskID, util.TruncateStr(c.Summary, 400))
		if c.FilesChanged != "" {
			fmt.Fprintf(&b, "   files: %s\n", util.TruncateStr(c.FilesChanged, 200))
		}
	}
	return b.String()
}

// trimCandidates returns the first n entries of candidates. If n >= len, it
// returns candidates unchanged.
func trimCandidates(candidates []store.FleetMemoryEntry, n int) []store.FleetMemoryEntry {
	if n <= 0 || n >= len(candidates) {
		return candidates
	}
	return candidates[:n]
}

// memoryRerankEnabled reads the SystemConfig kill switch. Default on.
func memoryRerankEnabled(db *sql.DB) bool {
	return store.GetConfig(db, "memory_rerank_enabled", "1") == "1"
}

// sortMemoriesForDisplay is a stable helper to keep deterministic ordering
// when the caller doesn't care about rank (e.g. dashboard listings).
// Currently unused — left in place for future callers that want date order
// while still benefiting from the re-ranker's filter.
func sortMemoriesForDisplay(entries []store.FleetMemoryEntry) []store.FleetMemoryEntry {
	out := make([]store.FleetMemoryEntry, len(entries))
	copy(out, entries)
	sort.SliceStable(out, func(i, j int) bool { return out[i].CreatedAt > out[j].CreatedAt })
	return out
}
