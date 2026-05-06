---
audience: both
scope: Index of per-agent reference docs — one entry per major agent in the fleet.
owner: D13
last_reviewed: 2026-05-05
---

# Agents

This directory holds one reference doc per major agent in the fleet. Each agent doc covers: role, responsibilities, the capability profile that constrains the Claude CLI invocation, key source files, the relevant pattern + unit tests, and pointers to related agent / subsystem docs.

Capability profiles for each agent live under [`agents/capabilities/`](../../agents/capabilities/) (one YAML per agent + `REGISTRY.yaml` for the fleet-wide vocabulary + `.forceblocklist.yaml` for the never-allowed denylist). See [`docs/architecture/claude-cli-invocation.md`](../architecture/claude-cli-invocation.md) for the invocation layering — which agents run with daemon CWD = `force-orchestrator/`, which run inside target-repo worktrees, and how `CLAUDE.md` is loaded in each case.

## Index

### Plan / approve / review

- [`commander.md`](commander.md) — Commander Cody — planner; decomposes Feature tasks into per-repo `CodeEdit` subtasks and proposes a convoy.
- [`chancellor.md`](chancellor.md) — Supreme Chancellor — single-instance convoy approver / conflict-resolution gate.
- [`captain.md`](captain.md) — Fleet Captain — plan-coherence gate after each astromech commit (Approve / UpdateDownstream / InsertNew / Reject / Escalate).
- [`council.md`](council.md) — Jedi Council — code reviewer; merges or returns rework.
- [`senate.md`](senate.md) — repo-scoped advisory layer; emits Verdicts; never auto-promotes its own rules.

### Worker / ship

- [`astromech.md`](astromech.md) — the coding workers; only agent class with target-repo CWD.
- [`pilot.md`](pilot.md) — PR-flow git-ops steward (deterministic shell-out; no LLM on happy path).
- [`diplomat.md`](diplomat.md) — draft-PR opener + `ConvoyReview` + `PRReviewTriage` runner.

### Triage / monitor / fail

- [`inquisitor.md`](inquisitor.md) — 5-minute heartbeat; sweeps stale tasks, closes convoys, dispatches dogs.
- [`boot.md`](boot.md) — lightweight stall-triage agent the Inquisitor calls.
- [`medic.md`](medic.md) — failure-triage; requeue / shard / escalate, plus `CIFailureTriage` under PR flow.

### Read-only analysis

- [`auditor.md`](auditor.md) — codebase scanner; produces a Planned convoy of fixes for operator approval.
- [`investigator.md`](investigator.md) — free-form research agent; produces prose, not findings.
- [`archaeologist.md`](archaeologist.md) — debt-pattern sweeper; proposes migrations through the librarian-emit / operator-ratify pipeline.

### Memory / experiment / commit-time

- [`librarian.md`](librarian.md) — memory curator; serves the `librarian.Client` cross-agent service interface.
- [`engineering-corps.md`](engineering-corps.md) — paired-runs experimentation orchestrator (six task handlers).
- [`bos.md`](bos.md) — Bureau of Standards — post-commit invariant gate; deterministic AST checks (no LLM).
- [`isb.md`](isb.md) — Imperial Security Bureau — post-commit security gate; baseline ISB rules + the SUPPLY pack.
