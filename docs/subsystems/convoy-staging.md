---
audience: operator
scope: D5.5 staged convoys (Commander-drafted phase pipelines) — placeholder until subsystem reference is authored.
owner: D5.5
last_reviewed: 2026-05-05
---

# Convoy staging

## Status: Stub

This page is a placeholder reserved by [D13 P1](../closures/) for the staged-convoy reference. The closure report for D5.5 — [`closures/DELIVERABLE-5.5-CLOSURE.md`](../closures/DELIVERABLE-5.5-CLOSURE.md) — already carries the design + evidence trail; this stub holds the navigation slot until the operator-facing user guide is split out.

## What this will cover

- The `ConvoyStages` table and the per-stage gate (`stage_id IS NULL` predicate enforced by Pattern P-StageGate).
- How Commander drafts phase pipelines into staged convoys.
- The post-hoc promotion gate (Pattern P-StagingPromotionConfirm) — `SetConvoyStaging` has zero ungated production callers.
- Operator-facing stage-skip discipline.

## Until then

Read the D5.5 closure report and the two pattern docs that fence the substrate:

- [`closures/DELIVERABLE-5.5-CLOSURE.md`](../closures/DELIVERABLE-5.5-CLOSURE.md)
- [`patterns/p-stage-gate.md`](../patterns/p-stage-gate.md)
- [`patterns/p-staging-promotion-confirm.md`](../patterns/p-staging-promotion-confirm.md)

## See also

- [`subsystems/convoy-lifecycle.md`](convoy-lifecycle.md) — the broader Feature → Convoy → ask-branch lifecycle this slots into.

## When this page lands

The next round of subsystem-doc authoring lifts the operator-facing prose out of the closure report into this stub.
