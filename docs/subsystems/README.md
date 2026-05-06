---
audience: both
scope: Index of per-subsystem user/operator guides — one entry per major shipped subsystem.
owner: D13
last_reviewed: 2026-05-05
---

# Subsystems

This directory holds operator-facing user guides for each shipped subsystem. Closure reports under [`../closures/`](../closures/) carry the per-deliverable evidence trail; subsystem docs here are the durable user/operator reference.

D13 P2 Wave B authored the per-subsystem pages below. Pages tagged "(planned)" are placeholder index entries reserved for the deliverable that ships them.

## Authored pages

- [`paired-runs.md`](paired-runs.md) — D3 experimentation primitive (full design)
- [`dashboard.md`](dashboard.md) — D5.5 + D11 dashboard SPA, tabs, JSON API, security
- [`dashboard-implementation.md`](dashboard-implementation.md) — D3 Phase 6 task briefs (agent-handoff artifact)
- [`notification-routing.md`](notification-routing.md) — D11 notification substrate
- [`convoy-lifecycle.md`](convoy-lifecycle.md) — Feature → Convoy → ask-branch → ConvoyReview → Ship
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

## Planned (later deliverables fill)

- `daemon-lifecycle.md` — D12 daemon lifecycle, drain, supervisor, foreground mode
- `convoy-staging.md` — D5.5 staged convoys (Commander-drafted phase pipelines)
- `supply-chain.md` — D5 supply-chain hygiene (SUPPLY-001..005)
- `arch-health.md` — D9 architecture health report (`docs/arch-health-weights.yaml`)
- `onboarding-cli.md` — D6 synthetic onboarding CLI
- `handoff-docs.md` — D10 synthetic handoff documentation
- `model-tier-experiments.md` — D7 model-tier optimization experiments
- `fleet-memory.md` — Fleet Memory + RAG + Librarian curator
- `mail-system.md` — Fleet mail (roles, types, automatic triggers)
- `directives.md` — Standing operator directives loaded from disk
- `dogs.md` — Watchdog dogs (cooldowns + behavior reference)
- `security.md` — Security posture overview (capability profiles, bash guard, scrubbing, repo-mode gating)
