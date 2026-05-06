---
audience: both
scope: Canonical docs index — find anything operator- or agent-facing here.
owner: D13
last_reviewed: 2026-05-05
---

# Force Documentation

This is the canonical entry point. Everything in `docs/` is reachable from here in 1–2 hops.

> **Metadata convention.** Every doc under `docs/` (except auto-rendered files like `CLAUDE.md`, `FIX-LOG.md`, `dashboard-conventions.md`, `pr-flow-invariants.md`, `self-healing.md`) carries a YAML front-matter block with four fields: `audience` (`operator` / `agent` / `both`), `scope` (one-sentence description), `owner` (deliverable id or subsystem), and `last_reviewed` (ISO date). New docs without the block fail [`TestMetadataBlockOnAllNewDocs`](../internal/audittools/audit_pattern_p_docs_test.go).

## For operators

- [Onboarding](onboarding.md) — install, first daemon run, first task, smoke flows
- [Architecture overview](overview.md) — how it all fits together (deeper than the README diagram)
- [Operator runbook](operator-runbook.md) — things-go-wrong: daemon crash, stuck convoy, runaway spend
- [Roadmap](roadmap.md) — what's planned (D0–D13+)
- [PR flow](subsystems/pr-flow.md) — operator summary; binding invariants in [pr-flow-invariants.md](pr-flow-invariants.md)
- [Dashboard](subsystems/dashboard.md) — operator reference; security invariants in [dashboard-conventions.md](dashboard-conventions.md)
- [Self-healing](subsystems/self-healing.md) — operator summary; binding invariants in [self-healing.md](self-healing.md)

## Subsystems

Per-subsystem operator/user reference. P2 fills these out from the README + closure reports.

- [Daemon lifecycle](subsystems/daemon-lifecycle.md) — D12 (stub — fills when D12 closes)
- [Notification routing](subsystems/notification-routing.md) — D11 (`config/notifications.yaml`, `notify.Dispatch`)
- [Dashboard](subsystems/dashboard.md) — D11 + D5.5 (`config/dashboard.yaml`, tabs, JSON API, security)
- [Dashboard implementation briefs](subsystems/dashboard-implementation.md) — D3 Phase 6 task briefs (agent-handoff artifact)
- [Convoy staging](subsystems/convoy-staging.md) — D5.5 (stub — design lives in closure report)
- [Convoy lifecycle](subsystems/convoy-lifecycle.md) — Feature → Convoy → ask-branch → ConvoyReview → Ship
- [Supply chain hygiene](subsystems/supply-chain.md) — D5 (SUPPLY-001..005, stub — design lives in closure report)
- [Cross-repo dependency graph](subsystems/cross-repo-graph.md) — D8
- [Architecture health report](subsystems/arch-health.md) — D9 (`docs/arch-health-weights.yaml`, stub — design lives in closure report)
- [Archaeologist](subsystems/archaeologist.md) — D9 (operator-gated incident archive)
- [Synthetic onboarding CLI](subsystems/onboarding-cli.md) — D6 (stub — design lives in closure report)
- [Synthetic handoff documentation](subsystems/handoff-docs.md) — D10 (stub — design lives in closure report)
- [Model-tier optimization experiments](subsystems/model-tier-experiments.md) — D7 (stub — design lives in closure report)
- [Paired runs](subsystems/paired-runs.md) — D3 (full design)
- [PR flow](subsystems/pr-flow.md) — operator summary; binding invariants in [pr-flow-invariants.md](pr-flow-invariants.md)
- [Self-healing](subsystems/self-healing.md) — operator summary; binding invariants in [self-healing.md](self-healing.md)
- [Gas Town pattern](subsystems/gas-town.md) — SQLite-only cross-agent coordination
- [holocron.db schema discipline](subsystems/holocron-schema.md) — schema parity, migration shape
- [Capability profiles](subsystems/capability-profiles.md) — per-agent YAML tool grants
- [MCP registry](subsystems/mcp-registry.md) — MCP server allowlist + injection
- [Worktree isolation](subsystems/worktree-isolation.md) — per-agent worktrees + base-drift
- [CLI shelling for LLM calls](subsystems/cli-shelling.md) — `claude -p` invocation layering
- [Escalation + Medic](subsystems/escalation-and-medic.md) — failure paths and the no-silent-failures rule
- [Fleet memory + RAG](subsystems/fleet-memory.md) — Librarian curator (stub)
- [Mail system](subsystems/mail-system.md) — roles, types, automatic triggers (stub)
- [Directives](subsystems/directives.md) — standing operator directives loaded from disk (stub)
- [Watchdog dogs](subsystems/dogs.md) — cooldowns + behavior reference (stub)
- [Security posture](subsystems/security.md) — capability profiles, bash guard, scrubbing, repo-mode gating (stub)

Subdirectory index: [`subsystems/README.md`](subsystems/README.md).

## Agents

Per-agent reference docs. P2 fills these out.

- [Commander](agents/commander.md) — planner
- [Astromech](agents/astromech.md) — workers
- [Captain](agents/captain.md) — plan coherence gate
- [Council](agents/council.md) — Jedi Council reviewers
- [Chancellor](agents/chancellor.md) — convoy approver / conflict gate
- [Librarian](agents/librarian.md) — memory curator
- [Medic](agents/medic.md) — failure triage
- [Inquisitor](agents/inquisitor.md) — background watchdog
- [Boot](agents/boot.md) — stall triage
- [Auditor](agents/auditor.md) — codebase scanner
- [Investigator](agents/investigator.md) — research
- [Pilot](agents/pilot.md) — PR-flow git steward
- [Diplomat](agents/diplomat.md) — draft-PR opener
- [Bureau of Standards](agents/bos.md) — commit-time invariant gate
- [Imperial Security Bureau](agents/isb.md) — commit-time security gate
- [Senate](agents/senate.md) — repo-scoped advisory
- [Engineering Corps](agents/engineering-corps.md) — paired-runs orchestrator

Subdirectory index: [`agents/README.md`](agents/README.md).

## Audit patterns

Pattern tests are grep- / AST-based regressions in `internal/audittools/` that fail CI when an architectural invariant drifts. One doc per pattern explains the rule, the rationale, and the contract callers must obey.

Subdirectory index: [`patterns/README.md`](patterns/README.md). Currently a stub — P2 fills.

## For AI agents working on this codebase

- [CLAUDE.md](../CLAUDE.md) — invariants + commit discipline + schema conventions (auto-rendered from `internal/store/fleet_rules_audit.go` via `make render-rules`; do not edit by hand)
- [Architecture: Claude CLI invocation layering](architecture/claude-cli-invocation.md) — which CWD each agent runs from, what CLAUDE.md auto-loads, where the fleet's invariants reach the model
- [Patterns index](patterns/README.md) — every audit pattern with rationale + enforcement
- [Closure reports](closures/) — per-deliverable evidence trails (D0–D11)

## References

- [Configuration files index](references/README.md) — `config/notifications.yaml`, `config/dashboard.yaml`, `arch-health-weights.yaml`, capability profiles, license matrix
- SystemConfig knob index — see [`references/README.md`](references/README.md); per-knob explainers land in subsequent deliverables

## Closure reports

Per-deliverable evidence trails — `closures/DELIVERABLE-N-CLOSURE.md`:

- [D0](closures/DELIVERABLE-0-CLOSURE.md) — Interface Layer Foundation
- [D1](closures/DELIVERABLE-1-CLOSURE.md) — Pre-Restart Security Closure
- [D2](closures/DELIVERABLE-2-CLOSURE.md) — Operational Risk Hardening
- [D3](closures/DELIVERABLE-3-CLOSURE.md) — Paired Runs + Engineering Corps + Global Holdout
- [D4](closures/DELIVERABLE-4-CLOSURE.md) — BoS + ISB + Senate
- [D5](closures/DELIVERABLE-5-CLOSURE.md) — Supply Chain Hygiene
- [D5.5](closures/DELIVERABLE-5.5-CLOSURE.md) — Staged Convoys
- [D6](closures/DELIVERABLE-6-CLOSURE.md) — Synthetic Onboarding CLI
- [D7](closures/DELIVERABLE-7-CLOSURE.md) — Model-Tier Optimization Experiments
- [D8](closures/DELIVERABLE-8-CLOSURE.md) — Cross-Repo Dependency Graph
- [D9](closures/DELIVERABLE-9-CLOSURE.md) — Archaeologist + Architecture Health Report
- [D10](closures/DELIVERABLE-10-CLOSURE.md) — Synthetic Handoff Documentation
- [D11](closures/DELIVERABLE-11-CLOSURE.md) — Notification Routing + Dashboard Personalization

Plus campaign-fix closures: [Fix #8d](closures/FIX-8D-CLOSURE.md), [Fix #8e](closures/FIX-8E-CLOSURE.md), [Fix #8f](closures/FIX-8F-CLOSURE.md).

## Strategic

- [Roadmap](roadmap.md) — full ordered deliverable list with merge-order appendix
- [Paired runs](subsystems/paired-runs.md) — D3 measurement substrate + full experimentation design
- [Next-gen agents](next-gen-agents.md) — Senate, ISB, BoS, Engineering Corps design notes

## Operator archives

Historical investigation artifacts preserved for audit — see [`operator-archives/`](operator-archives/) (Code Red audit, FIX-* working notes).

## Implementation specs

- [Dashboard implementation](subsystems/dashboard-implementation.md) — D3 Phase 6 task briefs
- [PAIRED-RUNS-ROLLOUT](../PAIRED-RUNS-ROLLOUT.md) — D3 phase sign-off log
- [FIX-LOG.md](../FIX-LOG.md) — operator narrative for each audit-fix PR (auto-rendered)
