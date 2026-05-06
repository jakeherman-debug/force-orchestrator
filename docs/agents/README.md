---
audience: both
scope: Index of per-agent reference docs — one entry per major agent in the fleet.
owner: D13
last_reviewed: 2026-05-05
---

# Agents

This directory holds one reference doc per major agent in the fleet. Each agent doc covers: role, file path, roster (where applicable), inputs, outputs, signals it can emit, capability profile, and notable invariants.

Currently a stub directory — D13 Phase 2 migrates per-agent content out of `README.md` and into the files listed below.

## Planned contents (D13 P2 fills)

- `commander.md` — Commander Cody (planner)
- `astromech.md` — worker agents
- `captain.md` — plan coherence gate
- `council.md` — Jedi Council code reviewers
- `chancellor.md` — convoy approver / conflict gate
- `librarian.md` — memory curator
- `medic.md` — failure triage
- `inquisitor.md` — background watchdog
- `boot.md` — stall triage
- `auditor.md` — codebase scanner
- `investigator.md` — research agent
- `pilot.md` — PR-flow git steward
- `diplomat.md` — draft-PR opener / ConvoyReview claimer
- `bos.md` — Bureau of Standards (commit-time invariant gate)
- `isb.md` — Imperial Security Bureau (commit-time security gate)
- `senate.md` — repo-scoped advisory layer
- `engineering-corps.md` — paired-runs experimentation orchestrator

Capability profiles for each agent live under `agents/capabilities/` (one YAML per agent + `REGISTRY.yaml`). See [`docs/architecture/claude-cli-invocation.md`](../architecture/claude-cli-invocation.md) for the invocation layering.
