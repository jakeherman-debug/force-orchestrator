// D3 P6A.7 — Narrative renderer prompt template (Pattern P28 contract).
//
// The prompt template MUST live in code, not in a DB-stored config row.
// Pattern P28 enforces this — drift between a versioned-in-code prompt
// and a versioned-in-DB prompt is the hazard.
package narrative_prompts

// PromptVersion is the canonical version stamp written into
// NarrativeRenders.prompt_version. Bump on any prompt edit.
const PromptVersion = "v1.0.0"

// PromptTemplate is the Haiku-rendered narrative template. The renderer
// substitutes {events_json} with the structured event window and the
// model returns a 1-3 sentence narrative paragraph.
const PromptTemplate = `You are the fleet narrator for an autonomous code-orchestration system.
You are given a JSON list of events from the last 30 seconds. Render a
1-3 sentence ambient narrative the operator can read at a glance.

Style:
- Plain English, present tense, no jargon.
- Mention specific agents by name (Captain, Council, Medic, ...).
- Mention specific convoy IDs (#47), task IDs (T-1234), AT-IDs (AT-008).
- Do NOT invent events not in the input. If nothing happened, say so.

Events:
{events_json}

Narrative:`

// FallbackProse is the bounded-quality fallback used when the LLM call
// fails OR the daily budget is exhausted. The renderer logs the
// fallback path for cost-cap monitoring.
const FallbackProse = "Fleet activity recorded — narrative budget exhausted; raise SystemConfig.narrative_render_daily_cap_usd to resume Haiku rendering."

// EstopFallbackProse is rendered while e-stop is active so the panel
// communicates the operator's own decision rather than going dark.
const EstopFallbackProse = "🛑 E-STOP active — fleet paused by operator. Narrative rendering suspended until resume."
