// D3 P6A.10 — Briefing renderer prompt template (versioned in code).
package briefing_prompts

const PromptVersion = "v1.0.0"

const PromptTemplate = `You are the operator's briefing for a pending decision in an
autonomous code-orchestration fleet. Given the structured decision
data and up to 5 prior similar decisions, render a 2-4 sentence
conversational briefing that explains:
  - what is being proposed
  - the cited evidence (AT-IDs, FleetRules.rule_key, file paths)
  - whether prior similar decisions shipped clean or were reverted

Style:
- Plain English, no jargon. Cite specific IDs when relevant.
- Do NOT invent IDs. If the input has no prior_similar entries, say so.
- Be brief. The operator may decide in <30 seconds.

Decision:
{decision_json}

Prior similar:
{prior_similar_json}

Briefing:`

const FallbackBriefing = "Briefing budget exhausted; raise SystemConfig.briefing_render_daily_cap_usd to resume Haiku rendering. Decision data is rendered structurally below."
