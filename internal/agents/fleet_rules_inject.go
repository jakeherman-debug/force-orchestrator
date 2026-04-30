package agents

import (
	"context"
	"database/sql"
	"log"
	"strings"
)

// InjectFleetRulesAgentPrompt appends every active FleetRules row with
// render_to='agent-prompt' that scopes to the named agent into the
// supplied PromptBuilder under SourceTagFleetRules.
//
// Fail-open semantics: a missing FleetRules table (pre-Phase-1 DB) or
// a query error logs but does not interrupt agent startup. The legacy
// const-based system prompt remains in place; this helper is purely
// additive. Runtime PromptByteAttribution will record the bytes
// against the same `fleet_rules` source tag the legacy const uses.
//
// Call site contract: invoke AFTER the PromptBuilder has been seeded
// with the agent's static system-prompt content but BEFORE Build() —
// so the FleetRules content is concatenated into the system prompt.
func InjectFleetRulesAgentPrompt(ctx context.Context, db *sql.DB, pb *PromptBuilder, agent string, logger *log.Logger) {
	if db == nil || pb == nil {
		return
	}
	extras, err := AssemblePerAgentPrompt(ctx, db, agent)
	if err != nil {
		// Fail open: a missing FleetRules table or query failure must
		// not stop an agent from running. Log so operators see drift.
		if logger != nil {
			logger.Printf("[FLEET-RULES-INJECT] %s: %v (agent will run with legacy const-only prompt)", agent, err)
		}
		return
	}
	if strings.TrimSpace(extras) == "" {
		return
	}
	pb.Add(SourceTagFleetRules, extras)
}

// AppendFleetRulesToPrompt is the string-shaped sibling of
// InjectFleetRulesAgentPrompt: returns systemPrompt with active
// agent-prompt FleetRules content concatenated. Used at call sites that
// don't go through PromptBuilder (legacy direct-AskClaudeCLI paths).
// Fail-open: returns the unmodified prompt on any DB error.
func AppendFleetRulesToPrompt(ctx context.Context, db *sql.DB, agent, systemPrompt string, logger *log.Logger) string {
	if db == nil {
		return systemPrompt
	}
	extras, err := AssemblePerAgentPrompt(ctx, db, agent)
	if err != nil {
		if logger != nil {
			logger.Printf("[FLEET-RULES-INJECT] %s: %v (agent will run with legacy const-only prompt)", agent, err)
		}
		return systemPrompt
	}
	if strings.TrimSpace(extras) == "" {
		return systemPrompt
	}
	return systemPrompt + "\n\n" + extras
}
