---
audience: both
scope: Senate — repo-scoped advisory layer that emits Verdicts on proposed convoys and contributes context to the Chancellor without ever auto-promoting its own rules.
owner: D13
last_reviewed: 2026-05-05
---

# Senate — Repo-Scoped Advisory Layer

## Role

The Senate is the repo-scoped advisory review layer. Each Senator owns one registered repo (the shakedown Senator is `force-orchestrator` itself, self-onboarded at daemon startup). When the Chancellor receives a `ProposedConvoys` plan that touches a Senator's repo, a `SenateReview` task is enqueued between the proposal write and the `AwaitingChancellorReview` transition. The Senator reviews the plan against its persistent context (FleetRules where `agent_scope='senate:<repo>'`, `SenateMemory` rows accumulated by the `senate-refresh` dog, recent-commits digest from the Librarian) and emits a Verdict (`concur` / `dissent` / `abstain`) with rationale.

The Senate package contains **no direct `INSERT INTO FleetRules`**. Senator rules promote ONLY via the operator-ratified pipeline: Librarian emits a candidate (`BootstrapSenatorRules`), Engineering Corps runs a paired-run experiment, the operator ratifies a `PromotionProposals` row, and the materialization step inserts the `FleetRules` row with `category='senate'`. This is the D4 Phase 3 "no Senator auto-editing its own rules" anti-cheat directive made mechanical.

Roster: Senate-Mothma, Senate-Bail, Senate-Padme.

## Responsibilities

- Claim `SenateReview` bounties.
- Assemble the per-Senator context: FleetRules at `agent_scope='senate:<repo>'`, recent `SenateMemory` rows, recent-commits digest from `librarian.RecentCommitsDigest`.
- Call Claude with the Senate capability profile to render a Verdict + rationale.
- Write the Verdict row; the Chancellor reads it as advisory input.
- Self-onboard the `force-orchestrator` Senator at daemon startup so the shakedown Senator is always present.
- **Never** insert into `FleetRules` directly — emit candidates through the librarian-emit / paired-run / operator-ratify pipeline.

## Capability profile

Profile: [`agents/capabilities/senate.yaml`](../../agents/capabilities/senate.yaml). The profile is `builtin_tools: []` — Senate's review is a pure-reasoning LLM call assembled from per-Senator persistent context, not from at-review-time worktree access. Loaded via `capabilities.LoadProfile("senate")` in `internal/agents/senate.go`. Live-Haiku gating applies; tests run under `LIVE_HAIKU_DISABLED=1` and exercise the deterministic-stub Verdict path.

## Key files

- `internal/agents/senate.go` — `SpawnSenate(ctx, db, name, lib)` claim loop.
- `agents/capabilities/senate.yaml` — capability profile (empty tool surface).
- `internal/clients/librarian/` — Senate consumes the librarian client for `RecentCommitsDigest`.

## Tests

- `internal/audittools/audit_pattern_p34_senate_no_self_promote_test.go` — walks the Senate package's AST and rejects any direct `INSERT INTO FleetRules`. **The defining invariant for this agent.**
- `internal/audittools/audit_pattern_p13_capability_profiles_test.go` — capability profile invariant.
- `internal/audittools/audit_pattern_p33_agent_memory_via_librarian_client_test.go` — Senate's memory access goes through the librarian client interface, not direct DB queries.
- `internal/audittools/audit_pattern_p31_llm_transcripts_test.go` — Senate LLM calls write transcripts.

## See also

- [`docs/agents/chancellor.md`](chancellor.md) — primary consumer of Senate Verdicts.
- [`docs/agents/librarian.md`](librarian.md) — emits `BootstrapSenatorRules` candidates the Senate ratification pipeline consumes.
- [`docs/agents/engineering-corps.md`](engineering-corps.md) — runs the paired-run experiments that promote Senator rule candidates.
- [`docs/architecture/claude-cli-invocation.md`](../architecture/claude-cli-invocation.md) — invocation layering (Senate runs with `force-orchestrator/` CWD).
