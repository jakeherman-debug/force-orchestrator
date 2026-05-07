---
audience: operator
scope: cmdDaemon consults store.RecentStartCount before spawning agents and exits on threshold breach; launchd plist + systemd unit declare the correct auto-restart contract.
owner: infrastructure
last_reviewed: 2026-05-07
title: Pattern P_DaemonCrashBudget — D12 crash-loop guard + auto-restart contract
type: pattern-doc
pattern: P_DaemonCrashBudget
---

# Pattern P_DaemonCrashBudget — D12 crash-loop guard + auto-restart contract

## Rationale

A broken binary that panics on boot would be auto-restarted forever
by launchd / systemd, chewing CPU and filling logs. D12 P3 closed
this hole with a two-layer guard:

1. **Crash budget.** Every successful daemon start records a
   `DaemonStartLog` row with `outcome='started'` BEFORE any agent
   goroutine spawns. Before recording, the daemon queries
   `store.RecentStartCount(window)` — if at least
   `daemon_crash_budget_max_starts` successful starts are within
   `daemon_crash_budget_window_minutes` (defaults: 3 starts in 5
   minutes), the next start records `outcome='crash_loop_aborted'`
   and exits 2.
2. **Auto-restart contract.** The launchd plist declares
   `KeepAlive` with `Crashed=true` + `SuccessfulExit=false`, and the
   systemd unit declares `Restart=on-failure` + `RestartSec=5`. The
   contract is "auto-restart on a crash; don't auto-restart after
   a clean `force daemon stop` or after a crash-budget exit-2".

Pattern P_DaemonCrashBudget is the AST + grep regression that
catches a refactor of `cmdDaemon` that drops the `RecentStartCount`
call, drops the exit-on-breach path, OR mangles the launchd /
systemd templates.

Closure narrative:
[`docs/closures/DELIVERABLE-12-CLOSURE.md`](../closures/DELIVERABLE-12-CLOSURE.md)
("3 crashes in 5 minutes trips the crash-budget guard") and
[`docs/subsystems/daemon-lifecycle.md`](../subsystems/daemon-lifecycle.md)
§ "Auto-restart and crash recovery".

## What it checks

The single test `TestPattern_P_DaemonCrashBudget` runs four checks:

1. **`cmdDaemon` calls `store.RecentStartCount(...)`.** AST walks
   `cmd/force/fleet_cmds.go` for the top-level `FuncDecl` named
   `cmdDaemon` and inspects its body for a `CallExpr` of shape
   `store.RecentStartCount(...)`.
2. **`RecentStartCount` is called BEFORE the first
   `agents.Spawn*`.** Source positions are monotonic. The earliest
   `RecentStartCount` position must be strictly less than the
   earliest `agents.Spawn*` position. A check that runs after a
   spawn is too late — the broken agent goroutines have already
   started consuming work.
3. **The breach path terminates the process.** The function must
   contain at least one `os.Exit(...)`, `log.Fatal`, `log.Fatalf`,
   or `log.Fatalln` call. Logging a warning and continuing is not
   sufficient — launchd / systemd would see the process stay up and
   never know to back off.
4. **launchd plist + systemd unit templates.** Reads
   `cmd/force/daemon_cmds.go` as text and asserts:
   - `<key>Crashed</key>`, `<key>SuccessfulExit</key>`, `<true/>`,
     `<false/>` are all present (launchd `KeepAlive`).
   - `Restart=on-failure` and `RestartSec=5` are both present
     (systemd unit).

## How it fails

```
Pattern P_DaemonCrashBudget: cmdDaemon does not call store.RecentStartCount(...) — crash-budget guard is missing; a broken binary would chew CPU forever via launchd/systemd auto-restart
Pattern P_DaemonCrashBudget: store.RecentStartCount (pos=N) is AFTER first agents.Spawn* (pos=M). The crash-budget check MUST run before any agent goroutine starts.
Pattern P_DaemonCrashBudget: cmdDaemon does not call os.Exit / log.Fatal — the crash-budget breach path must terminate the process, not just log a warning
Pattern P_DaemonCrashBudget: launchdPlistTemplate must contain "<key>SuccessfulExit</key>" — auto-restart contract incomplete
Pattern P_DaemonCrashBudget: systemdUnitTemplate must contain "Restart=on-failure" — auto-restart contract incomplete
```

Typical violating snippet (warning instead of exit):

```go
n, _ := store.RecentStartCount(db, window)
if n >= maxStarts {
    log.Printf("warn: %d starts in %s — crash-looping?", n, window)
    // BUG: should exit; falling through means launchd loops forever.
}
agents.SpawnCaptain(ctx, db, ...)
```

## How to fix

Run the check ahead of every spawn and exit on breach:

```go
window := dashboardCrashBudgetWindow(db)        // SystemConfig key
maxStarts := dashboardCrashBudgetMaxStarts(db)  // SystemConfig key
n, err := store.RecentStartCount(db, window)
if err != nil { return err }
if n >= maxStarts {
    _ = store.RecordDaemonStart(db, "crash_loop_aborted", "...")
    log.Fatalf("crash-budget breached: %d starts in %s; run `force daemon clear-crash-budget` to recover", n, window)
}
_ = store.RecordDaemonStart(db, "started", "...")

// Now safe to spawn agents.
agents.SpawnCaptain(ctx, db, ...)
```

Recovery for the operator: `force daemon clear-crash-budget` — see
[`docs/subsystems/daemon-lifecycle.md`](../subsystems/daemon-lifecycle.md)
§ "Auto-restart and crash recovery".

## Test reference

- File: `internal/audittools/audit_pattern_p_daemon_crash_budget_test.go`
- Core assertion: `TestPattern_P_DaemonCrashBudget`
- Helpers: standard `go/parser` + `go/ast` walk for the
  `RecentStartCount` / `agents.Spawn*` / `os.Exit` / `log.Fatal*`
  recognizers; `os.ReadFile` + `strings.Contains` for the launchd
  plist and systemd unit literal grep; `moduleRoot(t)` to resolve
  the repository root.

## See also

- [P_DaemonSingleton](p-daemon-singleton.md) — sibling pre-spawn
  guard that runs alongside the crash-budget check.
- [P_DaemonUpdateHistory](p-daemon-update-history.md) — the schema
  invariant that pairs with `DaemonStartLog`.
- [`docs/subsystems/daemon-lifecycle.md`](../subsystems/daemon-lifecycle.md)
  § "Auto-restart and crash recovery".
- SystemConfig keys `daemon_crash_budget_window_minutes` (5) and
  `daemon_crash_budget_max_starts` (3).
- `cmd/force/daemon_boot_sweep.go` — the boot-time recovery sweep
  that runs immediately after the crash-budget check.
