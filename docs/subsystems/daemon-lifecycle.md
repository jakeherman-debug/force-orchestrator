---
audience: operator
scope: D12 daemon lifecycle — control surface, singleton, provenance, trust file, bundled dashboard.
owner: D12
last_reviewed: 2026-05-05
---

# Daemon lifecycle

The force daemon is a single Go binary that supervises the agent fleet, the dog scheduler, and the bundled dashboard. D12 P1 establishes the operator-facing control surface and the lifecycle invariants every later phase (sleep/wake survival in P2, auto-restart + crash recovery in P3) builds on top of.

## Overview

`force daemon` is the long-running process that:

- holds the singleton lock at `~/.force/force.pid` (`flock(LOCK_EX | LOCK_NB)`) so two daemons cannot race;
- spawns every agent goroutine (Astromechs, Captains, Council, Commanders, etc.) under the daemon `ctx` so a single SIGINT/SIGTERM cascades cleanly;
- runs the bundled dashboard goroutine on `127.0.0.1:41977` when `dashboard_enabled=true` (default);
- serves operator commands via the `force daemon <subcommand>` family.

The daemon is intentionally a single binary rather than a daemon-plus-helper split: the flock + ctx-cancellation pair gives a clean, observable shutdown without an init system in the loop. Operators who want supervised restarts use `force daemon install` (launchd on macOS, systemd user-unit on Linux); P3 will land the in-binary supervisor for environments where running an init system is overkill.

## Components

### 1. Singleton lock — `internal/daemon/singleton`

`singleton.Acquire(pidPath)` opens the PID file, takes a non-blocking exclusive flock, writes the current PID, and returns a release closure. The flock is the source of truth — the PID file is observability only:

- A second `force daemon` invocation calls `Acquire`, gets `ErrAlreadyRunning`, and exits 1.
- A daemon that crashed (kill -9, panic, laptop power-off) leaves the file behind, but the flock dies with the process. The next start succeeds and logs "stale PID file from PID N — taking over".
- The clean-shutdown path drops the flock + removes the file via the release closure.

`IsLocked(pidPath)` is the read-only probe used by `force daemon status` / `force daemon stop`.

### 2. Provenance — `internal/daemon/provenance`

Every binary built via `make build` carries three `-ldflags`-injected vars:

```
-X main.GitSHA=<sha>
-X main.BuildTime=<UTC RFC3339>
-X main.GitBranch=<branch>
```

`cmd/force/main.go` declares the vars at package level and calls `provenance.Set(GitSHA, BuildTime, GitBranch)` from `init()` so non-main code (dashboard, daemon status, trust file writes) can read them via `provenance.Get()`. A binary built outside the Makefile keeps the default `"unknown"` markers — `force version` and `force daemon status` surface those as a hint that the binary's history is unverified.

### 3. Control surface — `force daemon <subcommand>`

| Subcommand | Purpose |
| --- | --- |
| `foreground` | Explicit foreground. `force daemon` (no subcommand) is kept as a back-compat alias that prints a one-line deprecation pointer on TTY. |
| `install [--dry-run]` | Install launchd plist (`~/Library/LaunchAgents/com.force-orchestrator.daemon.plist`) or systemd user unit (`~/.config/systemd/user/force-orchestrator.service`). |
| `uninstall` | Remove the installed plist/unit (calls `launchctl unload` / `systemctl --user disable --now` first). |
| `status` | PID, provenance, dashboard URL, trust file presence. |
| `stop` | SIGTERM the running daemon, wait up to 60 s for a clean exit. |
| `logs [-f] [-n N]` | Tail `fleet.log`. |
| `update [--binary <path>] [--assume-yes]` | Replace the running binary with a 4-diff preview + trust-file gate. |
| `rollback` | Restore the previous binary (`<binary>.previous`). |
| `trust list/add <path>/remove <sha>` | Manage `~/.force/trusted-binary-hashes`. |
| `history [--limit N]` | Show DaemonUpdateHistory (P3 schema; falls back to the trust file in P1). |
| `validate-config` | Parse `config/*.yaml` without starting the daemon. |
| `validate-schema` | Lightweight schema parity check (the comprehensive gate is `make test`'s `TestSchemaParity`). |

Read-only subcommands (`status`, `history`, `logs`, `trust list`) do NOT acquire the singleton lock — the flock guard fires only on the start paths (`force daemon`, `force daemon foreground`).

### 4. Trust file — `~/.force/trusted-binary-hashes`

Append-only, one entry per line:

```
<sha256> <UTC-RFC3339> <trusted-by> <git-sha-at-build-time> <git-branch>
```

`force daemon update` enforces a default-on paranoia gate:

1. Compute SHA256 of the new binary.
2. If the SHA is in the trust file: proceed.
3. If NOT: print the four diff inspection commands (`git log`, `git diff --stat`, config drift, `internal/` drift), prompt `"Trust this binary and proceed with update? [yes/no]"`, and only proceed on `yes`.
4. On `yes` (or `--assume-yes`), append a new entry to the trust file.

`force daemon rollback` walks back to the second-most-recent entry and verifies its SHA matches `<binary>.previous` before swapping.

### 5. Bundled dashboard — `127.0.0.1:41977`

The daemon spawns the dashboard goroutine on startup unless `dashboard_enabled=false` in SystemConfig. The default port (`dashboard_port` SystemConfig key) is `41977` — Star Wars: A New Hope, 1977 — operator-mnemonic, low collision risk. The dashboard binds loopback only (`127.0.0.1`); remote access requires an SSH tunnel.

`internal/dashboard.RunDashboardCtx(ctx, db, port)` is the cancellable form used by the daemon; the standalone `force dashboard` command still calls `RunDashboard(db, port)` (which wraps `RunDashboardCtx` with `context.Background()`). On daemon shutdown the dashboard goroutine drains via `srv.Shutdown(...)` with a 5 s deadline.

## Lifecycle

```
install  →  start  →  run  →  stop  →  uninstall
           (managed via launchd/systemd or `daemon foreground`)
```

1. **install** — `force daemon install` writes the plist/unit. On `--dry-run` the rendered file is printed; nothing is written.
2. **start** — the unit (or `force daemon foreground`) launches the binary. The daemon acquires the singleton lock, writes the PID file, runs PR-flow + reconcile bootstraps, spawns agent goroutines, spawns the bundled dashboard goroutine, and blocks in the signal loop.
3. **run** — agents claim BountyBoard rows; SIGUSR1 hot-scales the agent pool; the dashboard surfaces state.
4. **stop** — SIGINT/SIGTERM cancels the daemon ctx (so agent claim loops stop issuing new work BEFORE `ReleaseInFlightTasks` sweeps), drains in-flight tasks for up to 30 s, then exits. The release closure drops the flock and removes the PID file.
5. **uninstall** — `force daemon uninstall` unloads/disables the plist/unit and removes the file. Stop the daemon first (or the unit will SIGTERM it on disable).

## Update flow

```
force daemon update [--binary <path>] [--assume-yes]
```

Step-by-step:

1. Identify the new binary path (default: same as the running daemon's binary; `--binary <path>` overrides).
2. Compute SHA256 of both the live and new binary.
3. Look up the new SHA in `~/.force/trusted-binary-hashes`.
4. **If NOT trusted (default-on paranoia):**
   - Print the four diff inspection commands the operator should run BEFORE proceeding (version history, file change summary, config drift, production-code drift).
   - Prompt `"Trust this binary and proceed with update? [yes/no]"`.
   - On `yes`: append a new entry to the trust file, then proceed. On anything else: abort with exit 1.
   - `--assume-yes` skips the prompt for non-interactive invocations (CI), but still appends the trust entry.
5. **If trusted:** proceed (still print the SHA + a one-line audit trail).
6. If a daemon is running, send SIGTERM and wait for clean exit.
7. Atomic rollover: `os.Rename(<live>, <live>.previous)`, then copy new binary to `<live>` with mode 0755.
8. Print "Update complete. Start the daemon via …" — the operator (or the launchd/systemd unit) restarts.

## Configuration

### SystemConfig keys

| Key | Default | Purpose |
| --- | --- | --- |
| `dashboard_port` | `41977` | Port the bundled dashboard binds. |
| `dashboard_enabled` | `true` | If `false`/`0`/`no`, daemon starts without the dashboard goroutine. |

### `~/.force/` files

| Path | Purpose |
| --- | --- |
| `~/.force/force.pid` | Singleton lock + PID file. flock-protected; the file content is observability only. |
| `~/.force/trusted-binary-hashes` | Append-only ratification log for the update flow. Each line: `<sha256> <UTC-RFC3339> <trusted-by> <git-sha-at-build> <git-branch>`. |

### Installed unit files

| Platform | Path |
| --- | --- |
| macOS | `~/Library/LaunchAgents/com.force-orchestrator.daemon.plist` |
| Linux | `~/.config/systemd/user/force-orchestrator.service` |

## Operator surface

`force daemon` reference — see the table in [Components](#components) above. The full per-subcommand help is reachable via `force daemon help`.

## Sleep/wake survival

D12 P2 hardens the daemon against laptop sleep/wake transitions. Without this layer, sleeping the host while a daemon is running leaves Locked tasks orphaned (the agent that owned them was suspended mid-call, and the kernel may have closed half-open HTTP connections), and cron-driven dogs miss their windows for the duration of sleep.

### Subscribe API — `internal/daemon/wake`

```go
events, err := wake.Subscribe(ctx)
// events delivers wake.GoingToSleep / wake.Woke until ctx cancels
```

Platform support matrix:

| Platform | Implementation | Source of events |
| --- | --- | --- |
| macOS (cgo enabled) | `wake_darwin.go` — IOKit `IORegisterForSystemPower` | kIOMessageSystemWillSleep / kIOMessageSystemHasPoweredOn |
| macOS (`CGO_ENABLED=0`) | `wake_darwin_nocgo.go` — graceful no-op | none — `Subscribe` returns `(nil, nil)` |
| Linux (systemd) | `wake_linux.go` — D-Bus subscription | `org.freedesktop.login1.Manager.PrepareForSleep` |
| Other (Windows, *BSD) | `wake_other.go` — graceful no-op | none — `Subscribe` returns `(nil, nil)` |

`Subscribe` returns `(nil, nil)` on platforms with no power hook. The daemon still runs — the wiring code in `cmdDaemon` checks for the nil channel and skips spawning the reconcile goroutine. Operators on unsupported platforms get a warning-free start; the daemon just won't auto-reconcile after sleep. (Manual `force daemon stop && force daemon` after a long sleep recovers the same way.)

### Post-wake reconciliation — `cmd/force/daemon_wake.go`

When a `Woke` event arrives, `reconcilePostWake(ctx, db)` runs four idempotent steps:

1. **DB liveness ping** — 5 s timeout. A dead handle returns `ErrPostWakeDBDead` and the goroutine driver calls `log.Fatalf`. P3's auto-restart relies on this exit.
2. **Singleton-lock recheck** — if a different live PID holds `~/.force/force.pid`, return `ErrPostWakeForeignSingleton`. The driver exits so the new owner stays authoritative. (Rare on local disk; possible on networked filesystems where flock state can be reaped during sleep.)
3. **Stuck-task sweep** — `store.ReleaseInFlightTasks` resets every Locked / UnderReview / UnderCaptainReview row back to Pending. Idempotent: a re-run finds nothing to release and is a no-op.
4. **Notification kick** — emit a `system_event` (Tier 2, mail-default) ping describing the resume. Operators see "Daemon resumed from sleep at <ts>; reconciliation complete (released=N)" in their inbox.

`GoingToSleep` events log only — the daemon does not block sleep. (TODO: a future P3 hook may snapshot the holocron pre-sleep into DaemonUpdateHistory; today `store.SnapshotHolocron` does not exist.)

### Idempotence guarantee

`reconcilePostWake` is idempotent: running it N times in a row produces the same fleet state as running it once. The pattern test [`TestPattern_P_DaemonWakeReconcile`](../../internal/audittools/audit_pattern_p_daemon_wake_test.go) is the AST-level regression — it confirms the wake import, the `wake.Subscribe(ctx)` call inside `cmdDaemon`, the routing of both `wake.GoingToSleep` and `wake.Woke` events, and the existence of all four platform build-tagged files.

The unit tests in `cmd/force/daemon_wake_test.go` cover: happy path, idempotence (3x re-run), the Locked-task sweep matrix, OperatorAttentionTags survival, the goroutine driver routing, `ctx` cancellation cleanup, and the `ErrPostWakeDBDead` sentinel.

### Limitations

- **No pre-sleep snapshot.** `wake.GoingToSleep` is observed but not acted on beyond a log line. A future deliverable can wire holocron snapshots here.
- **No agent-attention clear.** The original spec called for an "AgentAttention" table reset — that table doesn't exist in this codebase. `OperatorAttentionTags` (operator-pinned UI hints) is preserved across sleep/wake since it represents operator intent, not agent runtime state.
- **No Windows / *BSD support.** Only macOS (cgo) and Linux (systemd-logind) have real implementations. Other platforms degrade to a no-op.

## See also

- [`subsystems/escalation-and-medic.md`](escalation-and-medic.md) — failure paths on stuck or crashed agents.
- [`subsystems/gas-town.md`](gas-town.md) — why daemon restart is safe.
- [`subsystems/dashboard.md`](dashboard.md) — the bundled dashboard's API + SPA layout.
