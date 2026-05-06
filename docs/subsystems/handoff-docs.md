---
audience: operator
scope: D10 synthetic handoff documentation (`ARCHITECTURE.md` renderer) — placeholder until subsystem reference is authored.
owner: D10
last_reviewed: 2026-05-05
---

# Synthetic handoff documentation

## Status: Stub

This page is a placeholder reserved by [D13 P1](../closures/) for the synthetic handoff-documentation reference. The closure report for D10 — [`closures/DELIVERABLE-10-CLOSURE.md`](../closures/DELIVERABLE-10-CLOSURE.md) — carries the design + evidence trail; this stub holds the navigation slot.

## What this will cover

- `dogArchitectureDocRender` — the dog that renders `ARCHITECTURE.md` on every merge to main of an enabled repo (`Repositories.handoff_synthesis_enabled=1`).
- Per-repo enablement, the AUTO-GENERATED-header convention enforced by [`scripts/pre-commit/architecture-md-check.sh`](../../scripts/pre-commit/architecture-md-check.sh).
- How the renderer selects content from FleetMemory + closure reports + per-repo metadata.
- Operator workflow: enabling/disabling the dog per repo.

## Until then

Read the D10 closure report and the pre-commit gate that fences the renderer's output:

- [`closures/DELIVERABLE-10-CLOSURE.md`](../closures/DELIVERABLE-10-CLOSURE.md)
- [`scripts/pre-commit/architecture-md-check.sh`](../../scripts/pre-commit/architecture-md-check.sh)

## See also

- [`subsystems/onboarding-cli.md`](onboarding-cli.md) — the operator-triggered counterpart (`force onboard`).

## When this page lands

The next round of subsystem-doc authoring lifts the operator-facing prose out of the closure report into this stub.
