---
audience: operator
scope: D9 architecture health report (`docs/arch-health-weights.yaml`) — placeholder until subsystem reference is authored.
owner: D9
last_reviewed: 2026-05-05
---

# Architecture health report

## Status: Stub

This page is a placeholder reserved by [D13 P1](../closures/) for the architecture health report subsystem. The closure report for D9 — [`closures/DELIVERABLE-9-CLOSURE.md`](../closures/DELIVERABLE-9-CLOSURE.md) — carries the design + evidence trail; this stub holds the navigation slot.

## What this will cover

- The composite architecture-health score: per-signal inputs, per-signal weights, the score aggregation.
- [`docs/arch-health-weights.yaml`](../arch-health-weights.yaml) — the operator-tunable weights file.
- How the dashboard surfaces the score and per-signal drill-downs.
- Drift-class signals (test coverage, audit-skip count, render coherence, schema parity).

## Until then

Read the D9 closure report and the weights file:

- [`closures/DELIVERABLE-9-CLOSURE.md`](../closures/DELIVERABLE-9-CLOSURE.md)
- [`../arch-health-weights.yaml`](../arch-health-weights.yaml)

## See also

- [`subsystems/archaeologist.md`](archaeologist.md) — the operator-gated debt-pattern sweeper that feeds some signals.
- [`subsystems/dashboard.md`](dashboard.md) — where the score surfaces.

## When this page lands

The next round of subsystem-doc authoring lifts the operator-facing prose out of the closure report into this stub.
