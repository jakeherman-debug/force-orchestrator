---
audience: operator
scope: cmdDaemon acquires the singleton flock at ~/.force/force.pid before spawning any agent goroutine.
owner: infrastructure
last_reviewed: 2026-05-07
title: Pattern P_DaemonSingleton — D12 single-instance enforcement
type: pattern-doc
pattern: P_DaemonSingleton
---

# Pattern P_DaemonSingleton — D12 single-instance enforcement

## Rationale

The force daemon supervises the agent fleet and the dog scheduler from
a single process. A second concurrent daemon means two agents can race
to claim the same `Pending` BountyBoard row, two dog schedulers can
fire the same cron window, and two trust-file writes can interleave —
all silent failures whose only symptom is a downstream contention bug.

D12 P1 closed this hole by adding a non-blocking exclusive flock on
`~/.force/force.pid` (see
[`internal/daemon/singleton/singleton.go`](../../internal/daemon/singleton/singleton.go)).
The flock dies with the process, so a crashed daemon's stale PID file
is reclaimed on the next start. The flock semantics are correct every
time — but only if the boot path actually calls `singleton.Acquire`.
Pattern P_DaemonSingleton is the AST regression that catches a
refactor of `cmdDaemon` (or a brand-new daemon entry point) that
silently drops the guard.

Closure narrative: see
[`docs/closures/DELIVERABLE-12-CLOSURE.md`](../closures/DELIVERABLE-12-CLOSURE.md)
("Single-instance enforced via `~/.force/force.pid` flock") and
[`docs/subsystems/daemon-lifecycle.md`](../subsystems/daemon-lifecycle.md)
§ "Singleton lock".

## What it checks

The single test `TestPattern_P_DaemonSingleton` AST-walks
`cmd/force/fleet_cmds.go` and asserts:

1. **Import wired.** The file imports
   `"force-orchestrator/internal/daemon/singleton"`. Without the
   import the daemon could not call `Acquire` even if a comment
   pretended otherwise.
2. **`cmdDaemon` calls `singleton.Acquire`.** The function-level walk
   finds at least one `CallExpr` of the form
   `singleton.Acquire(...)` inside the body of the
   top-level `FuncDecl` named `cmdDaemon`.
3. **Acquire happens BEFORE the first `agents.Spawn*` call.** Source
   positions are monotonic, so the test records the earliest
   `singleton.Acquire` position and the earliest `agents.Spawn*`
   position and asserts `acquirePos < firstSpawnPos`. Acquiring the
   flock after a spawn would let a second daemon's agents race for
   work during the gap.
4. **Sanity probe.** `cmdDaemon` must call at least one
   `agents.Spawn*` — if it doesn't, the audit's "before-spawn"
   assertion is vacuous and the test fails fast with a sanity-check
   error rather than a silent green.

## How it fails

A typical failure surfaces as:

```
Pattern P_DaemonSingleton: cmdDaemon does not call singleton.Acquire(...) — second concurrent daemon would silently boot
```

Other failure shapes the test emits:

```
Pattern P_DaemonSingleton: cmd/force/fleet_cmds.go does not import "force-orchestrator/internal/daemon/singleton" — daemon entry would skip the flock guard
Pattern P_DaemonSingleton: cmdDaemon function not found in cmd/force/fleet_cmds.go
Pattern P_DaemonSingleton: singleton.Acquire (pos=N) is AFTER first agents.Spawn* (pos=M). Acquire must run BEFORE any agent goroutine starts, so a second daemon never gets to claim work.
```

Typical violating snippet (Acquire ordered after a spawn):

```go
func cmdDaemon(ctx context.Context, ...) {
    agents.SpawnCaptain(ctx, db, ...)         // BUG: spawn before flock
    release, err := singleton.Acquire("~/.force/force.pid")
    ...
}
```

## How to fix

Move the `singleton.Acquire` call to the very top of `cmdDaemon`,
ahead of any `agents.Spawn*`:

```go
func cmdDaemon(ctx context.Context, ...) error {
    release, err := singleton.Acquire(singleton.DefaultPidPath())
    if err != nil {
        if errors.Is(err, singleton.ErrAlreadyRunning) {
            return fmt.Errorf("another force daemon is already running: %w", err)
        }
        return err
    }
    defer release()
    // ... bootstraps, then agents.Spawn* ...
}
```

If a brand-new daemon entry-point lands (e.g.
`force daemon foreground-supervised`), it must EITHER call
`singleton.Acquire` itself before any spawn OR delegate to
`cmdDaemon`, which already does.

## Test reference

- File: `internal/audittools/audit_pattern_p_daemon_singleton_test.go`
- Core assertion: `TestPattern_P_DaemonSingleton`
- Helpers: standard `go/parser` + `go/ast` walk; uses
  `moduleRoot(t)` from the audittools shared helper to resolve
  the repository root in-process.

## See also

- [P_DaemonCrashBudget](p-daemon-crash-budget.md) — the sibling
  pre-spawn guard that bounds restart loops.
- [P_DaemonProvenance](p-daemon-provenance.md) — what the singleton
  trusts the binary to be.
- [`docs/subsystems/daemon-lifecycle.md`](../subsystems/daemon-lifecycle.md)
  § "Singleton lock" and § "Lifecycle".
- `internal/daemon/singleton/singleton.go` — flock implementation.
