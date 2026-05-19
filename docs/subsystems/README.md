---
audience: both
scope: Index of per-subsystem user/operator guides — one entry per major shipped subsystem.
owner: D13
last_reviewed: 2026-05-18
---

# Subsystems

This directory holds operator-facing user guides for each shipped subsystem. Closure reports under [`../closures/`](../closures/) carry the per-deliverable evidence trail; subsystem docs here are the durable user/operator reference.

D13 P2 Wave B authored the initial set of per-subsystem pages; the post-D13 hygiene sweep authored the remaining nine pages (daemon-lifecycle, state-files, arch-health, onboarding-cli, handoff-docs, fleet-memory, directives, dogs, security) — every page below now carries fully authored content (no `## Status: Stub` markers remain). Pages tagged "(planned)" in earlier revisions have been removed or absorbed.

## Authored pages

- [`paired-runs.md`](paired-runs.md) — D3 experimentation primitive (full design)
- [`dashboard.md`](dashboard.md) — D5.5 + D11 dashboard SPA, tabs, JSON API, security
- [`dashboard-implementation.md`](dashboard-implementation.md) — D3 Phase 6 task briefs (agent-handoff artifact)
- [`notification-routing.md`](notification-routing.md) — D11 notification substrate
- [`mail-system.md`](mail-system.md) — D11 mail/notification delivery channel (Slack + Tier 1/2/3 mapping)
- [`convoy-lifecycle.md`](convoy-lifecycle.md) — Feature → Convoy → ask-branch → ConvoyReview → Ship
- [`convoy-staging.md`](convoy-staging.md) — D5.5 multi-stage convoy primitive (9 gates, dispatch fence, dashboard surface)
- [`supply-chain.md`](supply-chain.md) — D5 supply-chain hygiene (SUPPLY-001..005, license matrix, deferral, recovery dogs)
- [`model-tier-experiments.md`](model-tier-experiments.md) — D7 paired-runs Haiku-downgrade harness (8 manifests, ship gates)
- [`pr-flow.md`](pr-flow.md) — operator summary; binding invariants in `../pr-flow-invariants.md`
- [`self-healing.md`](self-healing.md) — operator summary; binding invariants in `../self-healing.md`
- [`escalation-and-medic.md`](escalation-and-medic.md) — failure paths
- [`gas-town.md`](gas-town.md) — SQLite-only cross-agent coordination
- [`holocron-schema.md`](holocron-schema.md) — schema parity + migration discipline
- [`capability-profiles.md`](capability-profiles.md) — per-agent YAML tool grants
- [`mcp-registry.md`](mcp-registry.md) — MCP server allowlist + injection
- [`worktree-isolation.md`](worktree-isolation.md) — per-agent worktrees
- [`cli-shelling.md`](cli-shelling.md) — `claude -p` invocation layering
- [`cross-repo-graph.md`](cross-repo-graph.md) — D8 dependency graph
- [`archaeologist.md`](archaeologist.md) — D9 operator-gated incident archive
- [`daemon-lifecycle.md`](daemon-lifecycle.md) — D12 daemon lifecycle (control surface, singleton, provenance, trust file, bundled dashboard)
- [`state-files.md`](state-files.md) — canonical paths for runtime state files (`forcepath` resolver + `FORCE_DIR` override)
- [`arch-health.md`](arch-health.md) — D9 monthly architecture-health report (longitudinal companion to the Archaeologist)
- [`onboarding-cli.md`](onboarding-cli.md) — D6 synthetic onboarding CLI (`force onboard <repo>` renderer)
- [`handoff-docs.md`](handoff-docs.md) — D10 synthetic handoff documentation (PRHandoffSynthesis + ARCHITECTURE.md)
- [`fleet-memory.md`](fleet-memory.md) — Librarian-mediated cross-agent memory (FleetMemory + FTS5 + weighted retrieval)
- [`directives.md`](directives.md) — FleetRules as source-of-truth for CLAUDE.md, FIX-LOG.md, and agent prompt injection
- [`dogs.md`](dogs.md) — Inquisitor dog cohort (cooldowns, dispatch order, operator overrides)
- [`security.md`](security.md) — Security posture (capability profiles, bash guard, inbound scrubbing, repo-mode gating, `.forceignore`)
