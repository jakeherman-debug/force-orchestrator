---
audience: operator
scope: D7 model-tier optimization experiments — placeholder until subsystem reference is authored.
owner: D7
last_reviewed: 2026-05-05
---

# Model-tier optimization experiments

## Status: Stub

This page is a placeholder reserved by [D13 P1](../closures/) for the model-tier-experiments reference. The closure report for D7 — [`closures/DELIVERABLE-7-CLOSURE.md`](../closures/DELIVERABLE-7-CLOSURE.md) — carries the design + evidence trail; this stub holds the navigation slot.

## What this will cover

- Per-agent model-tier defaults and how Engineering Corps experiments perturb them.
- Cost-vs-quality trade-off analysis for Opus / Sonnet / Haiku across the agent fleet.
- Holdout discipline (the global holdout never participates in model-tier experiments).
- Operator-facing tier overrides via `SystemConfig`.

## Until then

Read the D7 closure report and the paired-runs primitive that backs the experiment substrate:

- [`closures/DELIVERABLE-7-CLOSURE.md`](../closures/DELIVERABLE-7-CLOSURE.md)
- [`subsystems/paired-runs.md`](paired-runs.md)

## See also

- [`agents/engineering-corps.md`](../agents/engineering-corps.md) — the orchestrator that runs these experiments.

## When this page lands

The next round of subsystem-doc authoring lifts the operator-facing prose out of the closure report into this stub.
