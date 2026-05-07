---
audience: operator
scope: cmdDaemon spawns dashboard.RunDashboardCtx on 127.0.0.1:41977 when SystemConfig dashboard_enabled is true.
owner: infrastructure
last_reviewed: 2026-05-07
title: Pattern P_DashboardBundled — D12 bundled-dashboard wiring
type: pattern-doc
pattern: P_DashboardBundled
---

# Pattern P_DashboardBundled — D12 bundled-dashboard wiring

## Rationale

The bundled dashboard is the operator's primary view into a running
fleet. The daemon owns the dashboard goroutine — running them as
separate processes loses the daemon-driven lifecycle (no shared
shutdown, no shared SIGUSR1 hot-scale, two flock candidates for the
same `holocron.db`). D12 P1 made the dashboard daemon-internal:
spawned on `127.0.0.1:41977` (operator-mnemonic, loopback only) by
`cmdDaemon` when SystemConfig key `dashboard_enabled` is unset or
truthy.

Two specific failure modes Pattern P_DashboardBundled defends
against:

- **Refactor drops the spawn.** A future cleanup pass deletes the
  `dashboard.RunDashboardCtx(ctx, db, port)` line from `cmdDaemon`.
  The daemon boots; agents work; the operator sees a blank tab. No
  other test catches it because the SPA tests run against a
  stand-alone dashboard process.
- **Refactor calls the wrong entry-point.** `dashboard.RunDashboard`
  (no `Ctx`) `os.Exit`s on a port collision; calling it from
  `cmdDaemon` would torpedo the daemon's drain loop on any
  bind-error. The cancellable form `RunDashboardCtx` is the only
  daemon-safe variant.

Closure narrative:
[`docs/closures/DELIVERABLE-12-CLOSURE.md`](../closures/DELIVERABLE-12-CLOSURE.md)
("Bundled dashboard at `127.0.0.1:41977`") and
[`docs/subsystems/daemon-lifecycle.md`](../subsystems/daemon-lifecycle.md)
§ "Bundled dashboard".

## What it checks

The single test `TestPattern_P_DashboardBundled` runs three
source-level checks:

1. **`cmd/force/fleet_cmds.go` calls `dashboard.RunDashboardCtx(`.**
   Substring grep — the cancellable form must be present.
2. **No invocation of legacy `dashboard.RunDashboard(` from a
   non-comment line in `fleet_cmds.go`.** Because the substring
   `RunDashboard` matches inside `RunDashboardCtx`, the test scans
   line-by-line, skipping `//`-prefixed lines, and rejects any line
   that contains `dashboard.RunDashboard(` without
   `dashboard.RunDashboardCtx(`. The legacy form `os.Exit`s on
   bind failure and would torpedo the daemon's drain loop.
3. **Default port literal `41977` is present in `fleet_cmds.go`.**
   Operator-mnemonic default (Star Wars: A New Hope, 1977). A
   refactor that re-routes through a different port would have to
   re-justify the change.
4. **`cmd/force/daemon_cmds.go` carries the SystemConfig helpers.**
   The file must contain `func dashboardPortFromConfig`,
   `func dashboardEnabledFromConfig`, plus the literals `41977`,
   `dashboard_port`, and `dashboard_enabled`. These are the
   contract for how the daemon reads the per-instance overrides.

## How it fails

```
Pattern P_DashboardBundled: cmd/force/fleet_cmds.go does not call dashboard.RunDashboardCtx(...) — daemon will boot without the bundled dashboard
Pattern P_DashboardBundled: cmd/force/fleet_cmds.go:142 invokes legacy dashboard.RunDashboard — daemon path must use RunDashboardCtx so SIGTERM cleanly drains the HTTP server
Pattern P_DashboardBundled: cmd/force/fleet_cmds.go does not contain default port 41977 — operator-mnemonic default missing
Pattern P_DashboardBundled: cmd/force/daemon_cmds.go missing "func dashboardEnabledFromConfig"
```

Typical violating snippet (legacy invocation in the daemon body):

```go
// cmd/force/fleet_cmds.go
go dashboard.RunDashboard(db, port)  // BUG: os.Exits on bind error.
```

## How to fix

Use the cancellable form and feed it the SystemConfig values:

```go
if dashboardEnabledFromConfig(db) {
    port := dashboardPortFromConfig(db) // defaults to 41977
    go func() {
        if err := dashboard.RunDashboardCtx(ctx, db, port); err != nil {
            log.Printf("dashboard goroutine exited: %v", err)
        }
    }()
}
```

`RunDashboardCtx` honors `ctx.Done()` and drains the HTTP server via
`srv.Shutdown(...)` with a 5 s deadline, matching the daemon's own
SIGTERM contract.

## Test reference

- File: `internal/audittools/audit_pattern_p_dashboard_bundled_test.go`
- Core assertion: `TestPattern_P_DashboardBundled`
- Helpers: `os.ReadFile` + `strings.Contains` for the substring
  checks; line-by-line split + `strings.HasPrefix(trimmed, "//")`
  comment skip for the legacy-call rejection; uses `moduleRoot(t)`
  to resolve the repository root.

## See also

- [P_DaemonSingleton](p-daemon-singleton.md) — the same `cmdDaemon`
  flock that gates the dashboard spawn.
- [`docs/subsystems/daemon-lifecycle.md`](../subsystems/daemon-lifecycle.md)
  § "Bundled dashboard".
- `internal/dashboard/` — the `RunDashboardCtx` implementation.
- SystemConfig keys `dashboard_port` and `dashboard_enabled`
  (set via `force config set …`).
