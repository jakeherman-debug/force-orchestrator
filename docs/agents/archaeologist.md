---
audience: both
scope: Archaeologist — debt-pattern sweeper that finds repeated-pattern hits across registered repos and proposes migrations through the librarian-emit / operator-ratify pipeline.
owner: D13
last_reviewed: 2026-05-05
---

# Archaeologist — Debt-Pattern Sweeper

## Role

The Archaeologist (Track A, Deliverable 9) is a claim-loop agent that walks every registered Pattern (under `internal/archaeologist/patterns/`) against each registered repo's working tree, persists hits into `ArchaeologistFindings`, and — for any pattern whose post-sweep open-count exceeds `Pattern.MinHitsForFeature()` — fans out an `ArchaeologistProposeMigration` task. The propose-migration handler calls `librarian.Client.EmitCandidate` with a pre-decomposed migration hypothesis. The operator ratifies via the existing `PromotionProposals` flow (anti-cheat #1: no auto-dispatch). On success, marks the cluster's findings as `status='proposed'` so the next sweep doesn't re-fire.

The Archaeologist follows the Diplomat shape: a single `SpawnArchaeologist` goroutine loops, claiming both task types in turn. **No LLM call** — the agent is pure Go pattern-scanning + librarian-client `EmitCandidate`. The librarian client is injected at spawn time via constructor injection, per the cross-agent-service-interfaces invariant.

## Responsibilities

| Task type | What it does |
|---|---|
| `ArchaeologistSweep` | Per-repo debt-pattern sweep. Walks every registered Pattern against the repo's working tree; persists hits into `ArchaeologistFindings` (status=`open`). Fans out an `ArchaeologistProposeMigration` task for any pattern whose post-sweep open-count exceeds `Pattern.MinHitsForFeature()`. |
| `ArchaeologistProposeMigration` | Calls `librarian.Client.EmitCandidate` with a pre-decomposed migration hypothesis. On successful candidate emission, marks the cluster's findings as `status='proposed'`. |

The agent is operator-gated: candidates flow through `PromotionProposals` for ratification before any code is written.

## Capability profile

The Archaeologist does not invoke `claude -p` — there is no per-call capability profile loaded. Its in-process toolset is pure Go pattern-scanning + the injected `librarian.Client`. (For completeness: there is no `agents/capabilities/archaeologist.yaml`; the agent is intentionally LLM-free.)

## Key files

- `internal/agents/archaeologist.go` — `SpawnArchaeologist(ctx, db, lib, name)` claim loop and the two task-type handlers.
- `internal/agents/archaeologist_test.go` — primary unit coverage.
- `internal/agents/archaeologist_d8_gate_test.go` — D8 gate (operator-gated promotion) coverage.
- `internal/archaeologist/patterns/` — registered Pattern implementations (one file per pattern).
- `internal/clients/librarian/` — the cross-agent service interface the Archaeologist consumes via constructor injection.

## Tests

- `internal/agents/archaeologist_test.go`, `archaeologist_d8_gate_test.go` — direct coverage.
- `internal/audittools/audit_pattern_p_archaeologist_operator_gated_test.go` — enforces that the Archaeologist cannot dispatch migrations without operator ratification (the anti-cheat #1 invariant for D9).
- `internal/audittools/audit_pattern_p16_clients_interfaces_test.go` — enforces librarian-client interface usage (no concrete-struct import).
- `internal/audittools/audit_pattern_p33_agent_memory_via_librarian_client_test.go` — memory access via the librarian client interface.

## See also

- [`docs/agents/librarian.md`](librarian.md) — `EmitCandidate` is the Archaeologist's only write path; ratification happens through the librarian-emit / operator-ratify pipeline.
- [`docs/agents/auditor.md`](auditor.md) — sibling read-only agent; produces ad-hoc structured findings, while the Archaeologist focuses on registered debt-patterns.
- [`docs/agents/engineering-corps.md`](engineering-corps.md) — runs the paired-run experiments that promote Archaeologist-emitted candidates alongside other librarian candidates.
