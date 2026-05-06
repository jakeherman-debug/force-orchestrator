---
audience: both
scope: Architecture deep-dive — how the agent fleet, holocron, dashboard, and operator loop fit together.
owner: D13
last_reviewed: 2026-05-05
---

# Architecture overview

This is the depth-doc that the top-level `README.md` links to for "how does this actually work." The README's text-art diagram is a one-screen summary; this doc fills in the layers behind it.

## Stub

Currently a placeholder — D13 Phase 2 migrates the appropriate sections out of `README.md` (Architecture, Task Lifecycle, How a Feature Request Flows End-to-End) into this file. The README will retain a one-screen diagram + a pointer here.

## Planned sections (P2 fills)

- **The Gas Town pattern** — why the SQLite ledger is the only coordination surface (no channels, no in-memory cross-agent state)
- **CLI shelling for LLM calls** — every agent calls `claude -p` through `internal/claude`, never the Anthropic HTTP API
- **Per-agent capability profiles** — static YAML grants under `agents/capabilities/`, fail-closed loader, fleet-wide blocklist
- **Daemon context threading + drain** — `Spawn*` takes `ctx` as first param; SIGINT cancels before drain so claim loops stop before `ReleaseInFlightTasks` sweeps (D12)
- **Cross-agent service interfaces** — `internal/clients/<service>/` Go interfaces; agents never import concrete client structs
- **Key tables** — full reference with schema deltas and what each row means
- **Task lifecycle state machines** — Feature, CodeEdit, WriteMemory, MedicReview, Planned
- **Startup reconciliation** — five divergence cases; fatal-on-fail
- **Review pipeline** — Astromech → BoS + ISB (parallel) → Captain → Council → merge → Librarian
- **PR-based delivery flow** — ask-branches, sub-PRs, ConvoyReview, ship-it gating
- **Reference**: [Claude CLI invocation layering](architecture/claude-cli-invocation.md) is the authoritative companion; this file frames the reader before sending them there

The README's existing architecture/lifecycle/end-to-end-flow sections (~250 lines today) are the primary content source for the migration.
