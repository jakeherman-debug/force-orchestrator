---
audience: agent
scope: Briefing prose cites real evidence (input IDs only); prompt template lives in code.
owner: D13
last_reviewed: 2026-05-05
title: Pattern P29 — Briefing cites real evidence
type: pattern-doc
pattern: P29
---

# Pattern P29 — Briefing cites real evidence

## Rationale

The briefing renderer must emit IDs sourced only from its input slice;
hallucinated row references would mislead the operator. The static
guard ensures:

1. The renderer contains a deterministic synthesis function
   (`synthesiseBriefingText`) — the contract that only references
   input IDs.
2. The renderer does not call `claude.AskClaudeCLI` without an
   explicit `// safe-llm: P29` redaction marker.
3. The renderer stamps `prompt_version` from
   `briefing_prompts.PromptVersion`.
4. The prompt template lives in code under
   `internal/agents/briefing_prompts/v1.go`, not in a SystemConfig
   row.

Originates in D3 P6A.10. Runtime fuzz of synthetic hallucinated rows
lives in the briefing renderer test suite (which has DB access);
P29 is the static guard.

## What it checks

Two sub-tests:

1. `TestPattern_P29_BriefingCitesRealEvidence` — reads
   `internal/agents/briefing_renderer.go` and asserts:
   - contains `func synthesiseBriefingText(`,
   - any `AskClaudeCLI` reference is paired with a
     `// safe-llm: P29` marker,
   - contains `briefing_prompts.PromptVersion`.
2. `TestPattern_P29_PromptInCode` — `briefing_prompts/v1.go` exists
   and declares `PromptVersion`, `PromptTemplate`, and
   `FallbackBriefing` constants.

## How it fails

```
Pattern P29: briefing_renderer.go missing synthesiseBriefingText — the deterministic-citation contract is broken
```

Or:

```
Pattern P29: AskClaudeCLI used without P29 safe-llm marker; risk of unverified ID hallucination
```

Or:

```
Pattern P29: PromptTemplate missing from briefing_prompts/v1.go
```

## How to fix

Restore the missing helper / marker / constant. If you legitimately
need to call `AskClaudeCLI` from the renderer, add a comment line
`// safe-llm: P29` adjacent to the call that names the redaction
contract you've put in place.

## Test reference

- File: `internal/audittools/audit_pattern_p29_briefing_test.go`
- Core assertions:
  - `TestPattern_P29_BriefingCitesRealEvidence` (lines 17–42)
  - `TestPattern_P29_PromptInCode` (lines 47–65)

## See also

- [P28 — Narrative is generated](p28-narrative.md)
- `internal/agents/briefing_renderer.go`
- `internal/agents/briefing_prompts/v1.go`
