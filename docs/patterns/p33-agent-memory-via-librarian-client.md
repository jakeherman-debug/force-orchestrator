---
audience: agent
scope: Agents read FleetMemory rows via Librarian Client surface — never via direct store calls.
owner: D13
last_reviewed: 2026-05-05
title: Pattern P33 — Agent memory via Librarian Client
type: pattern-doc
pattern: P33
---

# Pattern P33 — Agent memory via Librarian Client

## Rationale

Production agent code retrieves FleetMemory rows for prompt injection
through the Librarian Client surface (`Client.GetWeightedMemories` /
the `getMemoriesForPromptInjection` helper) — never via direct
`store.GetFleetMemories` / `store.GetFleetMemoriesByIDs` /
`store.ListAllFleetMemories` calls. The Librarian Client owns
weighting, retrieval logging (`store.RecordRetrieval`), and rerank
orchestration; bypassing it skips all three.

Originates in D4 Phase 0.

## What it checks

`TestPattern_P33_AgentMemoryInjectionViaLibrarianClient`:

1. AST-walks `internal/agents/*.go` (non-test).
2. Skips files in `p33Allowlist`:
   - `internal/agents/librarian_dogs.go` — Librarian's own
     maintenance dogs (they ARE the curator backend).
   - `internal/agents/librarian_ingress.go` — the canonical
     Client-ingress seam.
   - `internal/agents/memory_rerank.go` — LLM rerank pipeline.
   - `internal/agents/librarian.go` — the Librarian agent itself.
3. For every CallExpr whose Fun is `store.<Name>` and `<Name>` is in
   `p33ForbiddenStoreFuncs` (`GetFleetMemories`,
   `ListAllFleetMemories`, `GetFleetMemoriesByIDs`), records the
   file:line as an offender.

`TestPattern_P33_AllowlistReasonsTruthful` asserts every allowlist
entry has a non-empty rationale.

## How it fails

```
Pattern P33 (D4-P0): N agent file(s) call a forbidden direct-store FleetMemory reader. Route the call through the Librarian Client (use getMemoriesForPromptInjection or Client.GetWeightedMemories) — agents must depend on the Librarian Client surface, not on store internals:
  internal/agents/captain.go:42 — store.GetFleetMemories(...)
```

## How to fix

Construct the agent with a `librarian.Client` and use the Client
surface:

```go
mems, err := lib.GetWeightedMemories(ctx, librarian.Query{
    Agent:    "captain",
    ConvoyID: convoyID,
    K:        20,
})
// or:
mems, err := getMemoriesForPromptInjection(ctx, lib, agentName, convoyID)
```

If a new structurally-exempt file appears (a new internal Librarian
component), add it to `p33Allowlist` with a one-line rationale.

## Test reference

- File: `internal/audittools/audit_pattern_p33_agent_memory_via_librarian_client_test.go`
- Core assertions:
  - `TestPattern_P33_AgentMemoryInjectionViaLibrarianClient` (lines 61–133)
  - `TestPattern_P33_AllowlistReasonsTruthful` (lines 137–143)

## See also

- [P16 — Cross-agent service interfaces](p16-clients-interfaces.md)
- [P34 — Senate no self-promote](p34-senate-no-self-promote.md)
- `internal/clients/librarian/` — the Client interface.
