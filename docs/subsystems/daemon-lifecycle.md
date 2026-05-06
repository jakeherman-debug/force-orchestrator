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

## Auto-restart and crash recovery

D12 P3 lands the in-binary supervisor companion to launchd / systemd's auto-restart. The contract is: the OS supervisor restarts the daemon on crash, the daemon refuses to restart when it's in a crash-loop, and every binary swap leaves an audit row the operator can read with `force daemon history`.

### Auto-restart contract

Both unit files declare "restart on crash, do not restart on clean exit":

- **launchd** (`~/Library/LaunchAgents/com.force-orchestrator.daemon.plist`) — `KeepAlive` dict carries `Crashed=true` and `SuccessfulExit=false`. A `force daemon stop` SIGTERM that exits cleanly will NOT trigger a restart. A panic / kill -9 / OOM-kill WILL.
- **systemd user unit** (`~/.config/systemd/user/force-orchestrator.service`) — `Restart=on-failure` with `RestartSec=5`. Same semantics.

### Crash budget

A broken binary that panics on boot would otherwise be auto-restarted forever, chewing CPU. The crash-budget guard short-circuits this. On every successful daemon start, before any agent goroutine starts, the daemon records a row in `DaemonStartLog` with `outcome='started'`. Before recording, it queries `RecentStartCount(window)` — if at least N successful starts are within the window, the next start treats the system as crash-looping, records `outcome='crash_loop_aborted'`, and exits 2.

Defaults:

| `SystemConfig` key | Default | Purpose |
| --- | --- | --- |
| `daemon_crash_budget_window_minutes` | `5` | Window over which starts are counted. |
| `daemon_crash_budget_max_starts` | `3` | Threshold — if reached, the next start aborts. |

The operator clears the budget after fixing the underlying issue:

```
force daemon clear-crash-budget [--assume-yes]
```

This truncates `DaemonStartLog`, writes an `AuditLog` row, and re-arms the guard at zero.

### `DaemonUpdateHistory` table

`force daemon update` records every invocation in `DaemonUpdateHistory`:

| Column | Purpose |
| --- | --- |
| `ts` | `datetime('now')` at write time. |
| `old_binary_sha`, `new_binary_sha` | SHA256 before / after the swap. |
| `old_git_sha`, `new_git_sha` | Best-effort `provenance.Get().GitSHA` snapshots. |
| `operator` | `$USER` (or `$LOGNAME`, falling back to `unknown`). |
| `outcome` | One of `success`, `rolled_back`, `failed`. |
| `notes` | Short free-text reason / context. |

Both `cmdDaemonUpdate` and `cmdDaemonRollback` use a `defer`-based recorder so every exit path lands a row — pattern `P_DaemonUpdateHistory` (`internal/audittools/audit_pattern_p_daemon_update_history_test.go`) walks the AST and confirms it.

### `force daemon history`

Replaces P1's trust-file fallback view. Default output reads `DaemonUpdateHistory` (newest first). `--from-trust-file` switches to the legacy ratification log:

```
force daemon history [--limit N] [--from-trust-file]
```

### Boot-time recovery sweep

After the crash-budget check passes and BEFORE the agent spawn loop, `runBootSweep` (in `cmd/force/daemon_boot_sweep.go`) puts the fleet in a clean state:

1. **Stale Locked / UnderReview / UnderCaptainReview tasks** — released back to `Pending` via `store.ReleaseInFlightTasks`. (Same primitive used on shutdown; calling it on boot recovers from a daemon that vanished without unwinding its locks.)
2. **Stale `Dogs.heartbeat_at`** — entries older than 10 minutes are cleared so the liveness banner stops showing dead dogs as alive.
3. **Half-baked `DraftPROpen` convoys** — convoys with `draft_pr_url` populated but no `PRHandoffSyntheses` row are logged; the operator can re-trigger the synthesis-handoff post.
4. **Mid-update binary** — if the live binary's SHA doesn't match the most recent `DaemonUpdateHistory.outcome='success'` row's `new_binary_sha`, log a warning ("running binary doesn't match last recorded successful update — check trust file").

Step failures are logged, never fatal — one stale row shouldn't block the daemon from booting.

### Pattern tests

| Test | Asserts |
| --- | --- |
| `TestPattern_P_DaemonCrashBudget` | `cmdDaemon` calls `store.RecentStartCount` BEFORE first `agents.Spawn*`; the threshold-breach path calls `os.Exit` / `log.Fatalf`; `launchdPlistTemplate` emits `Crashed`/`SuccessfulExit` keys; `systemdUnitTemplate` emits `Restart=on-failure`/`RestartSec=5`. |
| `TestPattern_P_DaemonUpdateHistory` | `cmdDaemonUpdate` and `cmdDaemonRollback` invoke `store.RecordDaemonUpdate`; `DaemonUpdateHistory` and `DaemonStartLog` are declared in all three schema locations. |

## See also

- [`subsystems/escalation-and-medic.md`](escalation-and-medic.md) — failure paths on stuck or crashed agents.
- [`subsystems/gas-town.md`](gas-town.md) — why daemon restart is safe.
- [`subsystems/dashboard.md`](dashboard.md) — the bundled dashboard's API + SPA layout.
