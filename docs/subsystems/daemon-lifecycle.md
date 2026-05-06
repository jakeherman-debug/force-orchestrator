---
audience: operator
scope: D12 daemon lifecycle, drain, supervisor, foreground mode — placeholder until D12 ships.
owner: D12
last_reviewed: 2026-05-05
---

# Daemon lifecycle

## Status: Stub

This page is a placeholder reserved by [D13 P1](../closures/) for the daemon-lifecycle subsystem that ships under D12. The link from [`docs/README.md`](../README.md) and the planned-subsystems list resolves here so the navigation contract holds; the actual content lands when D12 closes.

## What this will cover

D12 is the daemon control surface + lifecycle deliverable. Once it lands, this page will document:

- The single-binary daemon model (`force daemon`) and the foreground vs background invocation modes.
- Startup sequencing: schema bootstrap, FleetRules audit-slice rehydration, dog-scheduler init, agent-pool warmup.
- Shutdown discipline: SIGINT/SIGTERM cancels the agent context BEFORE `ReleaseInFlightTasks` sweeps; no agent claims new work during drain.
- The supervisor / auto-restart layer (D12 P3): crash recovery, `DaemonUpdateHistory`, and how the operator distinguishes a planned restart from an unplanned crash.
- Sleep/wake survival (D12 P2): how the daemon recovers when the laptop suspends mid-task.

## Until then

The current shape of the daemon is described inline in `cmd/force/cmd_daemon.go` and the `daemon context threading` paragraph of [`CLAUDE.md`](../../CLAUDE.md). The lifecycle invariants you should rely on today are:

- Every `agents.Spawn*` takes `ctx context.Context` as its first parameter.
- `cmdDaemon` cancels the context BEFORE the drain loop on shutdown signals.
- All cross-agent state lives in `holocron.db` (Gas Town pattern); restarts are recoverable because state is on disk.

## See also

- [`subsystems/escalation-and-medic.md`](escalation-and-medic.md) — failure paths on stuck or crashed agents.
- [`subsystems/gas-town.md`](gas-town.md) — why daemon restart is safe.

## When this page lands

D12 P4 (verifier + closure) will replace this stub with the full lifecycle reference.
