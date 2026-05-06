---
audience: operator
scope: D6 synthetic onboarding CLI (`force onboard`) — placeholder until subsystem reference is authored.
owner: D6
last_reviewed: 2026-05-05
---

# Synthetic onboarding CLI

## Status: Stub

This page is a placeholder reserved by [D13 P1](../closures/) for the synthetic-onboarding-CLI reference. The closure report for D6 — [`closures/DELIVERABLE-6-CLOSURE.md`](../closures/DELIVERABLE-6-CLOSURE.md) — carries the design + evidence trail; this stub holds the navigation slot.

## What this will cover

- `force onboard <repo>` — what it generates, where it writes, what it reads.
- The auto-generated `ONBOARDING.md` contract — pre-commit gate refuses hand-edits (see [`scripts/pre-commit/onboarding-md-check.sh`](../../scripts/pre-commit/onboarding-md-check.sh)).
- How the CLI integrates with the Repositories table and what it reads from `agents/capabilities/`.
- Operator workflow: when to run, when to refresh, how to interpret the output.

## Until then

Read the D6 closure report and the pre-commit check that fences the renderer's output:

- [`closures/DELIVERABLE-6-CLOSURE.md`](../closures/DELIVERABLE-6-CLOSURE.md)
- [`scripts/pre-commit/onboarding-md-check.sh`](../../scripts/pre-commit/onboarding-md-check.sh)

## See also

- [`onboarding.md`](../onboarding.md) — operator onboarding for Force itself (a different doc).

## When this page lands

The next round of subsystem-doc authoring lifts the operator-facing prose out of the closure report into this stub.
