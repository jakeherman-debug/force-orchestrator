---
audience: both
scope: Gas Town pattern — SQLite-only cross-agent coordination through holocron.db.
owner: architecture
last_reviewed: 2026-05-05
subsystem: gas-town
type: subsystem-doc
---

# Gas Town pattern

All cross-agent coordination in Force happens through the SQLite database `holocron.db`. There are no Go channels, no in-memory maps, no goroutine-to-goroutine pointers between agents. If two agents need to talk, one writes a row; the other reads it. This is the **Gas Town pattern** and it is the foundational architecture invariant of the fleet.

## Overview

The pattern's three properties:

1. **Any agent can crash and restart without losing state.** The DB is the single source of truth; agent goroutines are stateless workers.
2. **The operator can inspect or modify any piece of state with standard SQL.** Debugging is `sqlite3 holocron.db 'SELECT * FROM …'`. There is no hidden state in process memory.
3. **Adding more agents is just spawning more goroutines pointing at the same DB.** Concurrency is bounded by row-level claim semantics, not by in-process synchronization.

The trade-off is that every coordination must be expressible as a row mutation. Operations that "feel" like a function call between agents — "tell the Captain the convoy is ready" — must instead be: write a `Pending` `BoSReview` task; Captain's claim loop picks it up; Captain writes back its decision; Council's claim loop sees the updated status and proceeds. This is sometimes more verbose but always observable.

## Components

Core tables that back the coordination substrate (full list in [`holocron-schema.md`](holocron-schema.md)):

- **`BountyBoard`** — the task queue. Every coordination event is a row here moving through statuses.
- **`Fleet_Mail`** — role-addressed inter-agent messaging. Any agent of role X reads X's inbox.
- **`Convoys`**, **`ConvoyAskBranches`** — feature-grouped task sets and per-(convoy, repo) integration branches.
- **`Escalations`** — human-required blockers any agent can raise.
- **`Dogs`** — cooldown tracking so background watchdog tasks don't trample each other.
- **`Agents`** — persistent worktree registry; one row per agent per repo.
- **`AuditLog`** — every consequential operator/agent action.

The claim loop in every agent is a `SELECT ... FOR UPDATE`-equivalent CAS:

```go
// pseudo-shape
UPDATE BountyBoard
SET status='Locked', locked_by=?, locked_at=datetime('now')
WHERE id = (
  SELECT id FROM BountyBoard
  WHERE status='Pending' AND type IN (...) AND ...
  ORDER BY priority DESC, id ASC
  LIMIT 1
)
RETURNING *;
```

`UPDATE ... RETURNING` is a single SQLite statement, which means two agents whose scopes overlap cannot both claim the same row.

## Invariants

1. **No Go channels for cross-agent state.** If two agents communicate, the medium is a DB row. (Inside one agent, channels for goroutine coordination are fine.)
2. **No in-memory maps for cross-agent state.** Per-process caches (e.g. claim-loop memoization) are allowed; cross-agent caches are not.
3. **State-transition CAS where prior status matters.** Status changes that depend on the prior status use `store.UpdateBountyStatusFrom(db, id, fromStatus, toStatus)` — Pattern P7. Zero rows affected = lost race; caller logs and returns.
4. **No silent failures.** Every error path terminates in `store.FailBounty`, `store.UpdateBountyStatus`, or an explicit `CreateEscalation`. Never `log.Printf` and continue.
5. **Cross-agent service boundaries are interfaces.** Direct function-call dependencies between agents are forbidden — go through `internal/clients/<service>/` (Pattern P16).
6. **Idempotent task spawners.** Use `store.AddIdempotentTask` / `AddConvoyTaskIdempotent` whenever the spawner is a signal that may fire more than once (dog ticks, retry loops). Partial UNIQUE indexes back the dedup.

## Configuration

`holocron.db` lives at the repo root. SystemConfig knobs that shape Gas Town behaviour:

- `max_concurrent` — fleet-wide cap on simultaneously `Locked` rows (0 = unlimited).
- `spawn_delay_ms` — milliseconds between agent claims (smooths thundering-herd on large backlogs).
- `batch_size` — rolling-60s claim cap (0 = unlimited).
- Per-agent `num_<agent>` knobs control concurrency; takes effect on restart.

The DB itself is protected by:
- `make protect-db` — macOS ACL `everyone deny delete,delete_child` on `holocron.db` and its WAL/SHM siblings.
- `make install-snapshots` — hourly WAL-consistent `sqlite3 .backup` snapshots into `~/.force/backups/`.
- `.claude/settings.json` deny rules — block `rm` / `mv` / `unlink` / `cp` / `dd` against `holocron*` paths inside Claude Code sessions.

## Operator surface

```bash
sqlite3 holocron.db 'SELECT id, type, status, owner FROM BountyBoard WHERE status NOT IN ("Completed","Cancelled") LIMIT 50'
sqlite3 holocron.db 'SELECT * FROM Fleet_Mail WHERE recipient_role="operator" ORDER BY id DESC LIMIT 20'
sqlite3 holocron.db '.schema BountyBoard'

force bounty <id>      # raw row inspector with stable JSON shape
force audit            # AuditLog tail
force dogs             # Dogs cooldown table
```

Editing rows by hand is supported but the reconciler will treat your edit as ground truth. Producing a state that violates fleet invariants downstream is your responsibility.

## See also

- [`holocron-schema.md`](holocron-schema.md) — schema parity rules + migration discipline.
- [`worktree-isolation.md`](worktree-isolation.md) — per-agent worktrees registered in the `Agents` table.
- [`escalation-and-medic.md`](escalation-and-medic.md) — failure paths terminating in `Escalations`.
- `mail-system.md` (planned) — `Fleet_Mail` substrate.
- [`../CLAUDE.md`](../../CLAUDE.md) — Gas Town pattern as the architecture invariant.
