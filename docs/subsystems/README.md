---
audience: both
scope: Index of per-subsystem user/operator guides — one entry per major shipped subsystem.
owner: D13
last_reviewed: 2026-05-05
---

# Subsystems

This directory holds operator-facing user guides for each shipped subsystem. Closure reports under [`../closures/`](../closures/) carry the per-deliverable evidence trail; subsystem docs here are the durable user/operator reference.

Currently a stub directory — D13 Phase 2 migrates content out of `README.md` and `docs/*.md` and into the files listed below.

## Planned contents (D13 P2 fills)

- `daemon-lifecycle.md` — D12 daemon lifecycle, drain, supervisor, foreground mode
- `notification-routing.md` — D11 notification routing (`config/notifications.yaml`, `notify.Dispatch`)
- `dashboard.md` — D5.5 + D11 dashboard personalization (`config/dashboard.yaml`, tabs, themes, saved filters, Watch chip)
- `convoy-staging.md` — D5.5 staged convoys (Commander-drafted phase pipelines)
- `supply-chain.md` — D5 supply-chain hygiene (SUPPLY-001..005, CodeArtifact, allowlist refresh, deferral path)
- `cross-repo-graph.md` — D8 cross-repo dependency graph
- `arch-health.md` — D9 architecture health report (`docs/arch-health-weights.yaml`)
- `archaeologist.md` — D9 archaeologist agent (operator-gated)
- `onboarding-cli.md` — D6 synthetic onboarding CLI
- `handoff-docs.md` — D10 synthetic handoff documentation
- `model-tier-experiments.md` — D7 model-tier optimization experiments
- `paired-runs.md` — D3 experimentation primitive (overview; full design lives in `../paired-runs.md`)
- `pr-flow.md` — PR-based delivery flow (ask-branches, sub-PRs, ConvoyReview, Diplomat ship)
- `fleet-memory.md` — Fleet Memory + RAG + Librarian curator
- `mail-system.md` — Fleet mail (roles, types, automatic triggers)
- `directives.md` — Standing operator directives loaded from disk
- `dogs.md` — Watchdog dogs (cooldowns + behavior reference)
- `security.md` — Security posture overview (capability profiles, bash guard, scrubbing, repo-mode gating)
