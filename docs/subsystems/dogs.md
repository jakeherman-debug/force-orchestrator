---
audience: both
scope: Watchdog dogs (cooldowns + behavior reference) — placeholder until subsystem reference is authored.
owner: dogs
last_reviewed: 2026-05-05
---

# Watchdog dogs

## Status: Stub

This page is a placeholder reserved by [D13 P1](../closures/) for the dogs reference. Dogs are the dispatched watchdogs that run on a cadence (5-minute heartbeat from Inquisitor; per-dog cooldowns to prevent stampede).

## What this will cover

- The full per-dog inventory: name, cadence, what it sweeps, what it dispatches, where it logs.
- Cooldown discipline — the `agents.ScheduleCooldown` helper family ([Pattern P30](../patterns/p30-cooldown.md)).
- The Inquisitor heartbeat loop and how it dispatches dogs.
- Operator overrides for per-dog enable / disable / cooldown via `SystemConfig`.

## Until then

Read:

- [`agents/inquisitor.md`](../agents/inquisitor.md) — the heartbeat agent that dispatches dogs.
- [`patterns/p30-cooldown.md`](../patterns/p30-cooldown.md) — the cooldown contract.

## See also

- [`subsystems/escalation-and-medic.md`](escalation-and-medic.md) — failure-handling dogs and the no-silent-failures rule.

## When this page lands

The next round of subsystem-doc authoring lifts the per-dog inventory out of inline source comments into this stub.
