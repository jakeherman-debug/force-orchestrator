package librarian

import (
	"context"

	"force-orchestrator/internal/claude"
	"force-orchestrator/internal/store"
)

// summarizeAgentName is the agent identity recorded against the
// SummarizeForContextOverflow Claude call. The librarian's own per-
// agent prompt cap (agent_max_prompt_bytes_librarian, falling back to
// the default) bounds the summarize call's prompt size; we stamp the
// ctx so PromptByteAttribution rows attribute the bytes to the
// librarian, not the calling agent.
const summarizeAgentName = "librarian"

// contextOverflowCallContext wraps the caller's ctx with a
// claude-callsite attribution stamped to "librarian". The whole
// prompt counts as a single librarian_memory contribution — we don't
// have a finer per-source breakdown for the summarizer's input
// because the input IS the overflow blob being compressed.
func contextOverflowCallContext(parent context.Context, prompt string) context.Context {
	contribs := []store.SourceContribution{
		{SourceTag: string(librarianMemoryTag), Bytes: len(prompt)},
	}
	return claude.WithClaudeCallContext(parent, summarizeAgentName, 0, contribs)
}

// librarianMemoryTag mirrors agents.SourceTagLibrarianMemory without
// importing internal/agents (which would create a cycle —
// internal/agents already imports internal/clients/librarian via
// the in-process client construction). Keep this single literal in
// lockstep with the agents-side constant.
const librarianMemoryTag = "librarian_memory"

// callClaudeForSummarize is the per-package alias for
// claude.AskClaudeCLIContext that the summarize path uses. Hard-coded
// to no-tool / no-MCP — summarization is pure-reasoning. Wrapped here
// so the in-process backing has one place to swap models when the
// CLI wrapper grows a model arg.
func callClaudeForSummarize(ctx context.Context, systemPrompt, userPrompt string) (string, error) {
	// allowedTools / disallowedTools / mcpConfig empty: the summarize
	// is pure reasoning. Pattern P13's allowlist explicitly carves
	// out internal/clients/librarian for this reason.
	return claude.AskClaudeCLIContext(ctx, systemPrompt, userPrompt, "", "", "", 1)
}
