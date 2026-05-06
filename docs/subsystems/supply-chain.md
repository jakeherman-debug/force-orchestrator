---
audience: operator
scope: D5 supply-chain hygiene (SUPPLY-001..005) — placeholder until subsystem reference is authored.
owner: D5
last_reviewed: 2026-05-05
---

# Supply chain hygiene

## Status: Stub

This page is a placeholder reserved by [D13 P1](../closures/) for the supply-chain hygiene reference. The closure report for D5 — [`closures/DELIVERABLE-5-CLOSURE.md`](../closures/DELIVERABLE-5-CLOSURE.md) — carries the design + evidence trail for SUPPLY-001 through SUPPLY-005; this stub holds the navigation slot.

## What this will cover

- SUPPLY-001..005 — the five rule families ISB enforces at commit time.
- `internal/isb/rules/license_matrix.yaml` — SPDX license-compatibility matrix.
- The token-expired non-passthrough invariant (Pattern P-SupplyDeferral).
- How `supplydeferral.RecordDeferral` keeps the audit trail durable when an upstream credential expires.

## Until then

Read the D5 closure report and the supporting pattern doc:

- [`closures/DELIVERABLE-5-CLOSURE.md`](../closures/DELIVERABLE-5-CLOSURE.md)
- [`patterns/p-supply-deferral.md`](../patterns/p-supply-deferral.md)
- [`internal/isb/rules/license_matrix.yaml`](../../internal/isb/rules/license_matrix.yaml)

## See also

- [`agents/isb.md`](../agents/isb.md) — the agent that enforces these rules.

## When this page lands

The next round of subsystem-doc authoring lifts the operator-facing prose out of the closure report into this stub.
