---
audience: both
scope: Fleet mail (roles, types, automatic triggers) — placeholder until subsystem reference is authored.
owner: D11
last_reviewed: 2026-05-05
---

# Mail system

## Status: Stub

This page is a placeholder reserved by [D13 P1](../closures/) for the fleet-mail reference. The notification routing layer ([`subsystems/notification-routing.md`](notification-routing.md)) covers the operator-facing dispatch surface; this page covers the inter-agent mail substrate.

## What this will cover

- Mail roles — sender / recipient / subscriber categories.
- Mail types — operator, agent-to-agent, broadcast, urgent.
- Automatic triggers — which lifecycle events emit mail without explicit code.
- The `SendMail` call-site contract (Pattern P27 — every forward-going `SendMail` routes through `RespectNotificationBudget` or an `emitOperatorMail*` wrapper).
- The cleanup dog that ages out delivered mail.

## Until then

Read:

- [`subsystems/notification-routing.md`](notification-routing.md) — operator-facing dispatch.
- [`patterns/p27-notification-budget-routing.md`](../patterns/p27-notification-budget-routing.md) — budget-routing contract.

## See also

- [`closures/DELIVERABLE-11-CLOSURE.md`](../closures/DELIVERABLE-11-CLOSURE.md) — D11 (notification routing) closure trail.

## When this page lands

The next round of subsystem-doc authoring lifts the inter-agent mail prose out of the D11 closure report into this stub.
