---
audience: agent
scope: Senate package and senate*.go files MUST NOT mutate FleetRules directly.
owner: D13
last_reviewed: 2026-05-05
title: Pattern P34 — Senate no self-promote
type: pattern-doc
pattern: P34
---

# Pattern P34 — Senate no self-promote

## Rationale

The Senate proposes; the operator ratifies. The Senate must NEVER
mutate FleetRules directly — rule changes route through the
operator-ratified PromotionProposal pipeline (Librarian emits
candidates → Engineering Corps experiments → operator ratifies →
rule lands in FleetRules). Roadmap reference: docs/roadmap.md
§ Deliverable 4 anti-cheat directive "No Senator auto-editing own
rules". Allowlist is empty by design; the spec is unambiguous.

Originates in D4 Phase 3.

## What it checks

`TestPattern_P34_SenateNoSelfPromote` walks:

- every `*.go` (non-test) under `internal/senate/`,
- every `internal/agents/senate*.go` (non-test).

For each file (skipping `p34Allowlist`), AST-walks for any CallExpr
whose Fun is `store.<Name>` where `<Name>` is in the forbidden set:

- `SetActiveFleetRule`
- `UpsertFleetRule`
- `InsertFleetRule`
- `DeleteFleetRule`
- `UpdateFleetRule`
- `DeactivateFleetRule`
- `RatifyPromotionProposal` (gateway from EC pipeline)
- `BootstrapFleetRules` (seed-time only)

The legitimate path — emitting a candidate via
`Librarian.Client.EmitCandidate` — is not in the forbidden set.

`TestPattern_P34_AllowlistReasonsTruthful` ensures any future
allowlist entry has a non-empty rationale.

## How it fails

```
Pattern P34 (D4-P3): N Senate file(s) call a forbidden FleetRules-mutating helper. Senate's only path to FleetRules is via Librarian.EmitCandidate → operator ratification (per docs/roadmap.md § D4 anti-cheat "No Senator auto-editing own rules"):
  internal/senate/foo.go:42 — store.UpsertFleetRule(...)
```

## How to fix

Replace the direct mutation with a Librarian candidate emission:

```go
err := lib.EmitCandidate(ctx, librarian.Candidate{
    Source: "senate",
    Topic:  "rule-tightening:foo",
    // ... canonical input fields ...
})
```

The candidate flows through Engineering Corps experiments and ends
in the operator's ratification queue.

## Test reference

- File: `internal/audittools/audit_pattern_p34_senate_no_self_promote_test.go`
- Core assertions:
  - `TestPattern_P34_SenateNoSelfPromote` (lines 73–133)
  - `TestPattern_P34_AllowlistReasonsTruthful` (lines 169–175)
- Helper: `scanFileForP34` (lines 137–165).

## See also

- [P33 — Agent memory via Librarian Client](p33-agent-memory-via-librarian-client.md)
- [P-ArchaeologistOperatorGated](p-archaeologist-operator-gated.md) — same shape for archaeologist.
- `internal/clients/librarian/::Client.EmitCandidate`.
