---
audience: both
scope: Supreme Chancellor — single-instance convoy approver / conflict-resolution gate between Commander's proposed plan and convoy materialization.
owner: D13
last_reviewed: 2026-05-05
---

# Supreme Chancellor — Convoy Approver

## Role

The Chancellor is the conflict-resolution gate that sits between Commander's planning output and the actual creation of a convoy. After Commander writes a `ProposedConvoys` row and transitions the Feature task to `AwaitingChancellorReview`, the Chancellor reviews the plan against all currently active work and other pending proposals. It is a single-instance agent (`Supreme-Chancellor-Palpatine`) — a deliberate serialization point so the global plan stays coherent.

Rulings:

| Ruling | Action |
|---|---|
| `APPROVE` | Plan is safe to execute. Creates the convoy and tasks immediately. |
| `SEQUENCE` | Plan is correct but depends on an active convoy finishing first. Creates the convoy with cross-convoy blocking dependencies on the upstream convoy's tail tasks. |
| `REJECT` | Fundamental design conflict. Resets the Feature to `Pending` and mails Commander with the rejection reason for replanning. |
| `MERGE` | This plan overlaps significantly with another pending proposal. The Chancellor calls Claude to synthesize a single unified task list and creates one combined convoy for both features. |

## Responsibilities

- Claim `AwaitingChancellorReview` bounties (single-instance — `SpawnChancellor` does not take a `name`).
- Compute the plan-conflict picture (active convoys + other pending proposals).
- Call Claude for the ruling; on Claude failure, **auto-approve** to avoid blocking the pipeline.
- Apply the ruling — create / sequence / reject / merge convoys.
- Honor the two optional ordering directives on any ruling: `sequence_after_feature_ids` (block the new convoy until a queued Feature gets its own convoy and completes) and `hold_convoy_ids` (retroactively place currently active convoys on hold when the Chancellor determines they depend on the new proposal).
- Compute blast radius for any ruling that crosses convoys (`chancellor_blast_radius.go`).

## Capability profile

Profile: [`agents/capabilities/chancellor.yaml`](../../agents/capabilities/chancellor.yaml). Loaded via `capabilities.LoadProfile("chancellor")` in `internal/agents/chancellor.go`.

## Key files

- `internal/agents/chancellor.go` — `SpawnChancellor(ctx, db)` claim loop and ruling-application flow.
- `internal/agents/chancellor_blast_radius.go` — blast-radius computation for cross-convoy rulings.
- `internal/agents/chancellor_blast_radius_test.go` — blast-radius coverage.
- `internal/agents/chancellor_ctx_cancellation_test.go` — context-cancellation coverage (per the daemon-context-threading invariant in CLAUDE.md).
- `agents/capabilities/chancellor.yaml` — capability profile.

## Tests

- `internal/agents/chancellor_blast_radius_test.go`, `chancellor_ctx_cancellation_test.go` — direct coverage.
- `internal/audittools/audit_pattern_p13_capability_profiles_test.go` — capability profile invariant.
- `internal/audittools/audit_pattern_p23_proposer_write_discipline_test.go` — Chancellor's `ProposedConvoys` → `Convoys` materialization is the *only* legitimate convoy-creation path; this pattern enforces it.
- `internal/audittools/audit_pattern_p31_llm_transcripts_test.go` — Chancellor LLM calls write transcripts.
- `internal/audittools/audit_pattern_p34_senate_no_self_promote_test.go` — adjacent: Senate verdicts feed into Chancellor input, but Senate cannot write FleetRules itself.

## See also

- [`docs/agents/commander.md`](commander.md) — produces the `ProposedConvoys` rows the Chancellor rules on.
- [`docs/agents/senate.md`](senate.md) — emits Verdict rows that the Chancellor reads as advisory input.
- [`docs/agents/captain.md`](captain.md) — runs *inside* the convoy after Chancellor approval.
