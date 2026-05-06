---
audience: operator
scope: Standing operator directives loaded from disk — placeholder until subsystem reference is authored.
owner: directives
last_reviewed: 2026-05-05
---

# Operator directives

## Status: Stub

This page is a placeholder reserved by [D13 P1](../closures/) for the standing-operator-directives reference.

## What this will cover

- The on-disk directives file format (one directive per row; persistent across daemon restarts).
- The loader path — which agents read directives, when they re-read, and how operator updates propagate.
- The relationship between standing directives and the FleetRules audit slice (different scopes; directives are operator-managed and not auto-rendered).
- Conflict resolution between a standing directive and an in-flight task instruction.

## Until then

Standing directives are currently load-bearing inside individual agent prompts. The full surface area lives in `internal/agents/` constants and per-agent capability profiles ([`agents/capabilities/`](../../agents/capabilities/)).

## See also

- [`subsystems/capability-profiles.md`](capability-profiles.md) — per-agent YAML tool grants.
- [`CLAUDE.md`](../../CLAUDE.md) — the FleetRules-rendered universal-load directives.

## When this page lands

The next round of subsystem-doc authoring fills this in once the directives file format stabilizes.
