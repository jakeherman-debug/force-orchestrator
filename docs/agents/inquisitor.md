---
audience: both
scope: Inquisitor — the background watchdog that runs every 5 minutes to reset stale tasks, flag stalls, close convoys, re-escalate stale escalations, and dispatch the dog cohort.
owner: D13
last_reviewed: 2026-05-05
---

# Inquisitor — Monitor

## Role

The Inquisitor is the fleet's heartbeat. It runs on a 5-minute cycle, sweeping the holocron for tasks that are stuck, escalations that have aged, convoys that are now complete, and dogs that are due for another tick. When it detects a stall it does *not* decide unilaterally — it spawns a Boot triage call (see [`docs/agents/boot.md`](boot.md)) and lets the lightweight Boot LLM verdict drive the action. The Inquisitor itself is mostly deterministic Go.

## Responsibilities

- **Reset stale tasks** — any task `Locked` or `UnderReview` for longer than `staleLockTimeout` (45 min) is returned to `Pending`.
- **Detect stalls** — tasks locked longer than `stallWarnTimeout` (20 min) with no new commits are flagged; after `stallEscTimeout` (30 min) the Boot agent decides RESET / ESCALATE / WARN / IGNORE.
- **Close convoys** — marks a convoy `Completed` once all its tasks are done and mails the operator.
- **Re-escalate** — bumps severity on escalations unacknowledged for 4+ hours; mails the operator.
- **Clean orphaned branches** — deletes git branches for permanently-failed / escalated tasks.
- **Run dogs** — dispatches all background maintenance dogs whose cooldown has expired (`cooldown_scheduler.go`). The dog-cohort reference will land in `docs/subsystems/dogs.md` when D13 P2 Wave B finishes; in the meantime, the legacy archive section "Watchdog Dogs" enumerates the cohort.

## Capability profile

Profile: [`agents/capabilities/inquisitor.yaml`](../../agents/capabilities/inquisitor.yaml). Inquisitor also loads [`agents/capabilities/boot.yaml`](../../agents/capabilities/boot.yaml) at spawn time so it can hand off to the Boot triage call without re-loading per cycle.

## Key files

- `internal/agents/inquisitor.go` — `SpawnInquisitor(ctx, db, cfg)` and the 5-minute tick loop.
- `internal/agents/inquisitor_test.go` — unit coverage.
- `internal/agents/cooldown_scheduler.go` — dog cooldown bookkeeping the Inquisitor reads on every tick.
- `internal/agents/cooldown_scheduler_test.go` — cooldown-scheduler unit coverage.
- `agents/capabilities/inquisitor.yaml`, `agents/capabilities/boot.yaml` — capability profiles.

## Tests

- `internal/agents/inquisitor_test.go` — sweep + stall-detect + convoy-close + dog-dispatch coverage.
- `internal/agents/cooldown_scheduler_test.go` — dog cooldown invariants.
- `internal/agents/dogs_arch_health_hook_test.go` and other `dogs_*_test.go` — coverage of dogs the Inquisitor dispatches.
- `internal/audittools/audit_pattern_p30_cooldown_test.go` — cooldown-scheduler discipline (no clock-skew tricks).
- `internal/audittools/audit_pattern_p13_capability_profiles_test.go` — capability profile invariant.

## See also

- [`docs/agents/boot.md`](boot.md) — the lightweight triage agent the Inquisitor calls on stall detection.
- [`docs/agents/medic.md`](medic.md) — handles tasks Inquisitor sweeps to permanently-failed.
- [`docs/architecture/claude-cli-invocation.md`](../architecture/claude-cli-invocation.md) — Inquisitor itself rarely calls Claude; the dogs and Boot do.
