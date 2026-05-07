---
audience: operator
scope: cmdDaemon subscribes to the platform power-state notifier and routes Woke events through reconcilePostWake.
owner: infrastructure
last_reviewed: 2026-05-07
title: Pattern P_DaemonWakeReconcile — D12 sleep/wake survival
type: pattern-doc
pattern: P_DaemonWakeReconcile
---

# Pattern P_DaemonWakeReconcile — D12 sleep/wake survival

## Rationale

Without an explicit sleep/wake hook, suspending the host machine
while a daemon is running leaves `Locked` BountyBoard rows orphaned
(the agent that owned them was suspended mid-call; the kernel may
have closed half-open HTTP connections), and cron-driven dogs miss
their windows for the entire sleep duration. On wake the fleet looks
running but is silently degraded.

D12 P2 hardens the daemon against laptop sleep / wake transitions by
subscribing to the platform-specific power-state notifier — IOKit's
`IORegisterForSystemPower` on macOS, `org.freedesktop.login1.Manager.PrepareForSleep`
on Linux — and routing `Woke` events through an idempotent reconciler
in `cmd/force/daemon_wake.go`. The reconciler sweeps stale `Locked`
heartbeats, replays the dog scheduler's missed windows, and emits a
`system_event` ping through `notify.Dispatch`.

Pattern P_DaemonWakeReconcile catches drift where a refactor of
`fleet_cmds.go` accidentally drops the wake hookup OR the
`reconcilePostWake` wiring OR one of the four platform-specific
build-tagged files.

Closure narrative:
[`docs/closures/DELIVERABLE-12-CLOSURE.md`](../closures/DELIVERABLE-12-CLOSURE.md)
("On laptop wake from sleep, Locked tasks…") and
[`docs/subsystems/daemon-lifecycle.md`](../subsystems/daemon-lifecycle.md)
§ "Sleep/wake survival".

## What it checks

The single test `TestPattern_P_DaemonWakeReconcile` runs three
checks:

1. **`cmd/force/fleet_cmds.go` imports the wake package and calls
   `wake.Subscribe(ctx)` inside `cmdDaemon`.** AST walks the import
   list for `"force-orchestrator/internal/daemon/wake"`, then
   inspects the `cmdDaemon` `FuncDecl` body for a `CallExpr` of
   shape `wake.Subscribe(...)`.
2. **`cmd/force/daemon_wake.go` references both event constants AND
   the reconciler.** Source-level grep — the file must contain
   `wake.GoingToSleep`, `wake.Woke`, and `reconcilePostWake`. Both
   events must be observable in the file (the GoingToSleep branch
   is a log-only hook today, but routing them both keeps the
   pattern future-proof for pre-sleep snapshots).
3. **All four platform build-tagged files exist under
   `internal/daemon/wake/`.** The test enumerates:

   | File | Build tag |
   | --- | --- |
   | `wake_darwin.go` | `darwin` |
   | `wake_darwin_nocgo.go` | `darwin` |
   | `wake_linux.go` | `linux` |
   | `wake_other.go` | `!darwin && !linux` |

   Each file is read; the head (everything up to the first blank
   line) must contain a `//go:build` directive that includes the
   expected tag substring. A missing file or a wrong build tag
   fails the test.

## How it fails

```
Pattern P_DaemonWakeReconcile: cmd/force/fleet_cmds.go does not import "force-orchestrator/internal/daemon/wake" — daemon would skip sleep/wake hooks
Pattern P_DaemonWakeReconcile: cmdDaemon does not call wake.Subscribe — daemon won't receive sleep/wake events
Pattern P_DaemonWakeReconcile: cmd/force/daemon_wake.go does not reference wake.Woke — wake event routing or reconcile wiring is missing
Pattern P_DaemonWakeReconcile: missing platform file internal/daemon/wake/wake_linux.go — multi-platform coverage is incomplete: open .../wake_linux.go: no such file or directory
Pattern P_DaemonWakeReconcile: internal/daemon/wake/wake_other.go build tag does not contain "!darwin && !linux"
```

Typical violating snippet (subscribe missing):

```go
// cmd/force/fleet_cmds.go cmdDaemon — wake hook deleted
release, _ := singleton.Acquire(...)
// MISSING: events, _ := wake.Subscribe(ctx); go onWake(ctx, db, events)
agents.SpawnCaptain(ctx, db, ...)
```

## How to fix

Restore the wake subscription inside `cmdDaemon` and the routing in
`daemon_wake.go`:

```go
// cmd/force/fleet_cmds.go
events, err := wake.Subscribe(ctx)
if err != nil { /* log and continue — non-fatal */ }
go onWake(ctx, db, events)
```

```go
// cmd/force/daemon_wake.go
func onWake(ctx context.Context, db *sql.DB, events <-chan wake.Event) {
    for {
        select {
        case <-ctx.Done():
            return
        case ev := <-events:
            switch ev.Kind {
            case wake.GoingToSleep:
                log.Printf("daemon: host going to sleep")
            case wake.Woke:
                if err := reconcilePostWake(ctx, db); err != nil {
                    log.Printf("daemon: post-wake reconcile failed: %v", err)
                }
            }
        }
    }
}
```

If you add a new platform, add the corresponding
`wake_<platform>.go` file with a matching `//go:build` directive
AND extend the test's `requiredFiles` map.

## Test reference

- File: `internal/audittools/audit_pattern_p_daemon_wake_test.go`
- Core assertion: `TestPattern_P_DaemonWakeReconcile`
- Helpers: standard `go/parser` + `go/ast` walks for the import
  + `wake.Subscribe` lookup; `os.ReadFile` + `strings.Contains` for
  the daemon_wake.go reference checks; build-tag head extraction
  via `strings.Index(head, "\n\n")`.
- Companion runtime tests live in
  `cmd/force/daemon_wake_test.go` (idempotence; Locked-task sweep
  matrix; ctx-cancellation cleanup; `ErrPostWakeDBDead` sentinel).

## See also

- [P_DaemonSingleton](p-daemon-singleton.md) — the flock that ensures
  a single wake reconciler per host.
- [P_DaemonCrashBudget](p-daemon-crash-budget.md) — the sibling
  pre-spawn lifecycle guard.
- [P-NotificationDispatch](p-notification-dispatch.md) — the
  surface the wake reconciler routes its `system_event` ping
  through.
- [`docs/subsystems/daemon-lifecycle.md`](../subsystems/daemon-lifecycle.md)
  § "Sleep/wake survival".
- `internal/daemon/wake/wake.go` — the `Subscribe` API +
  `Event{Kind: GoingToSleep|Woke}` shape.
