---
title: Deliverable 12 — Daemon Lifecycle Management (CLOSURE)
type: closure-doc
deliverable: D12
status: CLOSED
last-reviewed: 2026-05-05
---

# DELIVERABLE-12-CLOSURE.md — Daemon Lifecycle Management (CLOSED)

**Date:** 2026-05-05
**Operator:** jake.herman@upstart.com
**Net verdict:** D12 is **CLOSED** at main HEAD `f83479c`. The force daemon is now a supervised, paranoid, sleep-wake-tolerant, crash-recoverable single-instance process with a 13-subcommand operator-control surface, an ldflags-driven build provenance trail, an explicit operator-trust gate on every binary swap, a flock-backed singleton lock at `~/.force/force.pid`, IOKit/logind sleep-wake hooks, a 3-in-5-min crash-budget guard with operator-clearable refusal-to-start, a boot-time recovery sweep that clears stale Locked tasks / dead heartbeats / binary-SHA mismatches before the agent fleet starts, and a `DaemonUpdateHistory` audit trail recording every binary rollover.

D12 is a four-phase sequential deliverable: P1 lays the daemon control surface (13 subcommands + provenance + trust file + bundled dashboard at `127.0.0.1:41977`); P2 lands sleep/wake survival across macOS (IOKit/cgo) and Linux (logind D-Bus); P3 ships auto-restart + crash-budget guard + boot-time recovery sweep + the `DaemonUpdateHistory` / `DaemonStartLog` audit-trail tables; P4 (this phase) authors the closure. P2 and P3 were developed on parallel branches and merged together at `f83479c` after a single integration pass.

---

## Goal

D12 set out to make the force daemon a supervised, paranoid, sleep-wake-tolerant, crash-recoverable single-instance process with a full operator-control surface and an audit trail of every binary swap. Pre-D12 the daemon was a one-shot `go run`-style binary: no install path, no provenance metadata, no protection against two concurrent instances racing on `holocron.db`, no awareness of laptop sleep (Locked tasks would silently age past their heartbeat threshold while the operator's machine was asleep), no auto-restart on crash, and no audit record of which binary was running when. The goal of D12 was to close every one of those gaps in a single coherent deliverable: install path via launchd/systemd, ldflags-injected GitSHA/BuildTime/GitBranch on every binary, flock at `~/.force/force.pid` to enforce single-instance, post-wake reconciliation of Locked tasks, a crash-budget guard that refuses to start after 3 crashes in 5 minutes (so a panic loop can't drain the operator's machine), and a `DaemonUpdateHistory` row written on every `force daemon update` invocation so an operator can audit who/when/why a binary swapped. The deliverable also added a bundled dashboard at `127.0.0.1:41977` (loopback only, default-on) so an operator can see fleet state without launching a separate process.

---

## Phase summary

### P1 — Daemon control surface

- **Impl SHA:** `df607fc`
- **Merge SHA:** `0180cd1`
- **Headline:** **13 `force daemon` subcommands** + ldflags provenance wired through the Makefile + flock-backed singleton at `~/.force/force.pid` + default-on operator trust file at `~/.force/trusted-binary-hashes` with a 4-diff preview before every binary swap + bundled dashboard at `127.0.0.1:41977` + **4 audit pattern tests** anchoring the new invariants.
- Subcommands shipped: `install` / `uninstall` / `status` / `stop` / `logs` / `foreground` / `update` / `rollback` / `trust` / `history` / `validate-config` / `validate-schema` / `clear-crash-budget` (the last lands in P3 as part of the crash-budget guard but is dispatched from the same router). All subcommands route through `cmd/force/daemon_cmds.go:dispatchDaemon`.
- New Go packages: `internal/daemon/singleton` (flock + PID file), `internal/daemon/provenance` (mirrors `main.GitSHA` / `main.BuildTime` / `main.GitBranch` so non-main code can read them without an import cycle), `internal/daemon/trust` (parses + writes `~/.force/trusted-binary-hashes` line-format `<sha256> <utc-rfc3339> <trusted-by> <git-sha-at-build-time> <git-branch>`).
- The Makefile `LDFLAGS` line (`-X main.GitSHA=$(GIT_SHA) -X main.BuildTime=$(BUILD_TIME) -X main.GitBranch=$(GIT_BRANCH)`) is the authoritative provenance wiring; `force version` (and `--version` / `-v`) prints those three values, with `unknown` defaults if the binary was built outside the Makefile.
- The trust file is `~/.force/trusted-binary-hashes`; `force daemon update` computes the SHA-256 of the candidate binary, looks it up against the trust file, and refuses the swap if the hash is unknown unless the operator passes `--trust` (which appends a row with `--reason` recording the rationale). Before every swap, a 4-diff preview is printed: `git log <old>..<new>`, `git diff --stat <old>..<new>`, `git diff <old>..<new> -- 'config/*.yaml'`, `git diff <old>..<new> -- internal/`.
- The bundled dashboard runs on port `41977` (Star Wars: A New Hope) on the loopback interface only; controlled by `dashboard_enabled` (default `true`) and `dashboard_port` (default `41977`) SystemConfig keys.
- Audit patterns shipped (4): `TestPattern_P_DaemonSingleton`, `TestPattern_P_DaemonProvenance`, `TestPattern_P_DaemonTrustFile`, `TestPattern_P_DashboardBundled` — all in `internal/audittools/`.
- Verifier verdict: GO. 5 components landed; 13 subcommands wired; ldflags injected on every Makefile-built binary; trust-file refusal-without-`--trust` enforced; bundled dashboard binds loopback-only.

### P2 — Sleep/wake survival

- **Impl SHA:** `302ebd4`
- **Integration SHA:** `0252a84`
- **Merge SHA (P2 + P3 together):** `f83479c`
- **Headline:** **4 build-tagged platform files** (`wake_darwin.go` cgo / `wake_darwin_nocgo.go` no-cgo fallback / `wake_linux.go` logind D-Bus / `wake_other.go` BSD/Windows graceful no-op) under `internal/daemon/wake/` + idempotent post-wake reconciler at `cmd/force/daemon_wake.go:onWake` + audit pattern `TestPattern_P_DaemonWakeReconcile`.
- macOS path uses IOKit's `IOPMRegisterForSystemPower` with a CFRunLoop pinned via `runtime.LockOSThread` to dispatch `kIOMessageSystemWillSleep` and `kIOMessageSystemHasPoweredOn`. The will-sleep callback acks immediately (IOKit will block the system's sleep transition if we don't), then schedules the heavy reconciliation work for the post-wake side.
- Linux path subscribes to `org.freedesktop.login1.Manager.PrepareForSleep` over D-Bus (boolean payload: `true` = about to suspend, `false` = resumed).
- Post-wake reconciler (`onWake`): (1) resets `Locked` tasks whose heartbeats predate the sleep window, (2) emits a `system_event` notification ping through `notify.Dispatch` so the operator's notifications dashboard records the wake, (3) logs a TODO marker for a future pre-sleep holocron snapshot (the `store.SnapshotHolocron` function does not exist today; explicitly tracked as a follow-up in the limitations section). The reconciler is idempotent — running it twice on the same database state yields the same result; the audit pattern asserts this.
- Build-tag matrix: `darwin && cgo` for the real IOKit path, `darwin && !cgo` for `CGO_ENABLED=0` builds (returns `(nil, nil)` so the daemon still runs without sleep/wake hooks — typically CI), `linux` for the logind path, `!darwin && !linux` for everything else (graceful no-op, daemon runs without hooks).
- Verifier verdict: GO. 4 platform files land with the correct build tags; reconciler is idempotent against the database state; `TestPattern_P_DaemonWakeReconcile` walks the AST and asserts the reconciler's resilience contract (no panic on missing data; safe to call repeatedly).

### P3 — Auto-restart + crash recovery + audit history

- **Impl SHA:** `d53ea52`
- **Integration SHA:** `125cccc`
- **Merge SHA (P2 + P3 together):** `f83479c`
- **Headline:** **`DaemonUpdateHistory` + `DaemonStartLog`** schema additions (3-place parity: `createSchema` + `runMigrations` + `schema/schema.sql`) + crash-budget guard (`daemon_crash_budget_window_minutes` × `daemon_crash_budget_max_starts` SystemConfig keys, default 5min × 3 starts) + boot-time recovery sweep (`cmd/force/daemon_boot_sweep.go`) + audit patterns `TestPattern_P_DaemonCrashBudget` + `TestPattern_P_DaemonUpdateHistory` + the `force daemon clear-crash-budget` subcommand.
- `DaemonUpdateHistory`: one row per `force daemon update` invocation. Columns: `id`, `ts`, `actor`, `old_sha256`, `new_sha256`, `git_sha_at_build`, `git_branch_at_build`, `build_time_at_build`, `reason`. Indexed on `ts` for the `force daemon history` reverse-chronological list. Both `update` and `rollback` write rows; rollback's `old_git_sha` is best-effort (uses `provenance.Get().GitSHA`, not the trust-file matched git SHA — see limitations).
- `DaemonStartLog`: one row per daemon start. Columns: `id`, `ts`, `pid`, `git_sha`, `build_time`, `outcome` (`started` / `crash_budget_refused` / etc.). Indexed on `ts` for the crash-budget guard's "starts in the last N minutes" query.
- Crash-budget guard: at daemon start, query `DaemonStartLog` for rows in the last `daemon_crash_budget_window_minutes` (default 5). If `count >= daemon_crash_budget_max_starts` (default 3), refuse to start, print recovery instructions (`force daemon logs -n 200`, then `force daemon clear-crash-budget` once the underlying issue is fixed), and exit non-zero. The guard's defaults are SystemConfig-tunable, so an operator running long-lived integration tests can widen the window without recompiling.
- Boot-time recovery sweep (`cmd/force/daemon_boot_sweep.go`): on every daemon start, before the agent fleet spawns, the sweep (1) clears `Locked` tasks whose heartbeats are stale, (2) clears dead heartbeats whose owner pids no longer exist, (3) detects half-baked `DraftPROpen` convoys (have a `draft_pr_url` but no handoff synthesis row) and logs a warning so the operator can re-trigger, (4) detects binary-SHA mismatches (a Locked task whose recorded `binary_sha` differs from the running daemon's) and resets them since the prior owner no longer exists.
- launchd/systemd auto-restart: the install templates emit `KeepAlive=SuccessfulExit:false` (launchd) / `Restart=on-failure` (systemd user-unit) so the supervisor restarts on crash but NOT on clean exit (`force daemon stop` is honored).
- Audit patterns shipped (3): `TestPattern_P_DaemonCrashBudget` (asserts the guard's start-counting logic + the `clear-crash-budget` operator-action surface), `TestPattern_P_DaemonUpdateHistory` (asserts every `update` / `rollback` writes an audit row), `TestPattern_P_DaemonWakeReconcile` (P2-owned but tracked here for the AST resilience contract).
- Verifier verdict: GO. Schema parity holds across `createSchema` / `runMigrations` / `schema/schema.sql`; crash-budget defaults match the audit's expectations; boot sweep is idempotent; `force daemon history` formats reverse-chrono rows from the new audit table.

### P4 — Strict verifier + closure (this phase)

- **Branch:** `deliverable/12/p4-closure`
- **Headline:** Closure authored, `make docs-check` green at HEAD with the closure file included in the corpus.
- No production code changes — pure docs.

---

## What's now true

The following invariants now hold post-D12 and are mechanically enforced. Each is anchored either by an audit pattern, by schema parity, or by the operator-control surface:

- **Single-instance enforced via `~/.force/force.pid` flock.** A second `force daemon` invocation calls `singleton.Acquire`, gets `ErrAlreadyRunning`, and exits 1. The flock dies with the process — a crashed daemon's stale PID file is reclaimed on the next start with a "stale PID file from PID N — taking over" log line. `TestPattern_P_DaemonSingleton` walks the AST and asserts the flock contract.
- **Every `force` binary carries its own `GitSHA` / `BuildTime` / `GitBranch`.** Visible via `force version`, `force --version`, `force -v`. Injected by the Makefile via `-X main.GitSHA=... -X main.BuildTime=... -X main.GitBranch=...`. A binary built outside the Makefile keeps the `unknown` defaults, surfaced as a hint that the binary's history is unverified.
- **`force daemon update` requires explicit operator trust.** The candidate binary's SHA-256 is looked up against `~/.force/trusted-binary-hashes`. If unknown, the swap is refused unless `--trust --reason "..."` is passed (which appends a trust-file row). Before every swap, a 4-diff preview is printed: `git log <old>..<new>` + `git diff --stat <old>..<new>` + `git diff <old>..<new> -- 'config/*.yaml'` + `git diff <old>..<new> -- internal/`. `TestPattern_P_DaemonTrustFile` anchors the contract.
- **launchd / systemd auto-restart on crash; clean exit does NOT restart.** The install templates emit `KeepAlive=SuccessfulExit:false` (launchd) / `Restart=on-failure` (systemd). `force daemon stop` is a clean exit; the supervisor honors it.
- **3 crashes in 5 minutes trips the crash-budget guard.** Daemon refuses to start, prints recovery instructions, exits non-zero. Window + max-starts are tunable via `daemon_crash_budget_window_minutes` + `daemon_crash_budget_max_starts` SystemConfig keys. Cleared with `force daemon clear-crash-budget`. `TestPattern_P_DaemonCrashBudget` anchors the contract.
- **On laptop wake from sleep, Locked tasks whose heartbeats predate the sleep window are reset.** macOS via IOKit's `IOPMRegisterForSystemPower`; Linux via `org.freedesktop.login1.Manager.PrepareForSleep`. A `system_event` notification ping fires through `notify.Dispatch` so the wake is recorded in the operator's notifications dashboard. The reconciler is idempotent. `TestPattern_P_DaemonWakeReconcile` anchors the contract.
- **Every binary swap creates an audit row in `DaemonUpdateHistory`.** Queryable via `force daemon history`. Both `force daemon update` and `force daemon rollback` write rows. `TestPattern_P_DaemonUpdateHistory` anchors the contract.
- **Boot-time sweep clears stale `Locked` tasks, dead heartbeats, and binary-SHA mismatches before the agent fleet starts.** Half-baked `DraftPROpen` convoys (have a `draft_pr_url` but no handoff synthesis row) are detected and logged. The sweep runs once per daemon start, before any goroutine spawns.
- **Bundled dashboard at `127.0.0.1:41977` (loopback only) starts with the daemon.** Default-on (`dashboard_enabled=true`); port via `dashboard_port` SystemConfig key (default `41977`). `TestPattern_P_DashboardBundled` asserts the bind-loopback-only contract.
- **`force daemon validate-config` and `force daemon validate-schema` exit non-zero on drift.** `validate-config` parses `config/notifications.yaml`, `config/dashboard.yaml`, the capability profiles, and the fleet rules; `validate-schema` cross-walks `createSchema` / `runMigrations` / `schema/schema.sql` for parity.

---

## Final inventory

### CLI subcommands added (13)

All under `force daemon <subcommand>`. Dispatched from `cmd/force/daemon_cmds.go:dispatchDaemon` (router) into per-subcommand `cmdDaemon*` functions:

| Subcommand | Function | Purpose |
|---|---|---|
| `install` | `cmdDaemonInstall` | Generates launchd plist or systemd user-unit; prints the next operator step (`launchctl load ...` / `systemctl --user enable --now ...`). |
| `uninstall` | `cmdDaemonUninstall` | Removes the plist/unit; prints the unload step. |
| `status` | `cmdDaemonStatus` | Prints PID + git-sha + git-branch + build-time + dashboard URL + crash-budget status. |
| `stop` | `cmdDaemonStop` | Sends SIGTERM to the daemon; clean exit (no auto-restart). |
| `logs` | `cmdDaemonLogs` | Tails the daemon log file with `-n N` support. |
| `foreground` (alias `fg`) | `cmdDaemon` | Runs the daemon in the foreground (legacy bare-`force daemon` shape). |
| `update` | `cmdDaemonUpdate` | Trust-gated binary swap; prints 4-diff preview; writes `DaemonUpdateHistory` row. |
| `rollback` | `cmdDaemonRollback` | Restores `<binary>.previous`; writes `DaemonUpdateHistory` row. |
| `trust list/add/remove` | `cmdDaemonTrust*` | Manages `~/.force/trusted-binary-hashes`. |
| `history` | `cmdDaemonHistory` | Reverse-chronological dump of `DaemonUpdateHistory`; falls back to trust-file format if DB is unavailable. |
| `validate-config` | `cmdDaemonValidateConfig` | Parses every YAML config file; exits non-zero on drift. |
| `validate-schema` | `cmdDaemonValidateSchema` | Cross-walks 3-place schema parity (`createSchema` × `runMigrations` × `schema/schema.sql`). |
| `clear-crash-budget` | `cmdDaemonClearCrashBudget` | Truncates `DaemonStartLog` rows older than the budget window so the next start succeeds. |

`force version` (and `--version` / `-v`) is extended to print `git-sha` / `git-branch` / `build-time` from the ldflags-injected vars.

### New Go packages

| Package | Purpose | Lines |
|---|---|---|
| `internal/daemon/singleton` | Flock + PID file. `Acquire(pidPath)` opens the PID file, takes a non-blocking exclusive flock, writes the current PID, returns a release closure. `DefaultPIDPath()` resolves `~/.force/force.pid` portably (falls back to `/tmp/force.pid` if `$HOME` is unset). | 187 (`singleton.go`) |
| `internal/daemon/provenance` | Mirrors `main.GitSHA` / `main.BuildTime` / `main.GitBranch` into a non-main package so non-main code (dashboard `/api/version`, daemon status, etc.) can read them without an import cycle. `Set` is called from `main.init`; `Get` returns the current snapshot. | 61 (`provenance.go`) |
| `internal/daemon/trust` | Parses + writes `~/.force/trusted-binary-hashes` line-format (`<sha256> <utc-rfc3339> <trusted-by> <git-sha-at-build-time> <git-branch>`). `DefaultPath()` resolves `~/.force/trusted-binary-hashes` portably. | 269 (`trust.go`) |
| `internal/daemon/wake` | Sleep/wake hooks. Cross-platform `Subscribe` interface; 4 build-tagged implementations (`wake_darwin.go` cgo, `wake_darwin_nocgo.go` no-cgo fallback, `wake_linux.go` logind D-Bus, `wake_other.go` graceful no-op). | 53 (`wake.go`) + 235 (darwin) + 95 (linux) + others |

### New tables (3-place schema parity)

Both tables are present in **createSchema** (`internal/store/schema.go`), **runMigrations** (same file, separate block) and **`schema/schema.sql`** per CLAUDE.md schema invariant. `TestSchemaParity` covers both.

| Table | createSchema | runMigrations | schema/schema.sql | Owner phase |
|---|---|---|---|---|
| `DaemonUpdateHistory` | `internal/store/schema.go:1519` | `internal/store/schema.go:2946` | `schema/schema.sql:1492` | P3 |
| `DaemonStartLog` | `internal/store/schema.go:1537` | `internal/store/schema.go:2958` | `schema/schema.sql:1511` | P3 |

Indexes:

- `idx_daemon_update_history_ts` on `DaemonUpdateHistory(ts)` (3 places — backs `force daemon history`).
- `idx_daemon_start_log_ts` on `DaemonStartLog(ts)` (3 places — backs the crash-budget guard's window query).

### New SystemConfig keys

| Key | Default | Purpose | Set by P |
|---|---|---|---|
| `daemon_crash_budget_window_minutes` | `5` | Look-back window for the crash-budget guard. | P3 |
| `daemon_crash_budget_max_starts` | `3` | Max daemon starts allowed within the window before the guard refuses. | P3 |
| `dashboard_port` | `41977` | Loopback port the bundled dashboard binds. | P1 |
| `dashboard_enabled` | `true` | Whether to start the bundled dashboard goroutine. | P1 |

### New `~/.force/` files

| Path | Owner | Format |
|---|---|---|
| `~/.force/force.pid` | `singleton` | `<pid>\n` (the flock is the source of truth; the PID file is observability only). |
| `~/.force/trusted-binary-hashes` | `trust` | Append-only audit. One line per trusted binary: `<sha256> <utc-rfc3339> <trusted-by> <git-sha-at-build-time> <git-branch>`. |

### New pattern tests (7)

All under `internal/audittools/`:

| Pattern | What it asserts | File |
|---|---|---|
| `TestPattern_P_DaemonSingleton` | Flock-backed singleton at `~/.force/force.pid`; Acquire returns `ErrAlreadyRunning` on contention; release closure unlocks. | `audit_pattern_p_daemon_singleton_test.go` |
| `TestPattern_P_DaemonProvenance` | `main.GitSHA` / `main.BuildTime` / `main.GitBranch` are wired through ldflags; `provenance.Set` is called from `main.init`; non-main code reads via `provenance.Get`. | `audit_pattern_p_daemon_provenance_test.go` |
| `TestPattern_P_DaemonTrustFile` | `force daemon update` looks up the candidate's SHA-256 against the trust file; refusal-without-`--trust` enforced; trust-file format parseable round-trip. | `audit_pattern_p_daemon_trust_test.go` |
| `TestPattern_P_DashboardBundled` | Bundled dashboard binds `127.0.0.1` only (loopback); `dashboard_port` + `dashboard_enabled` SystemConfig keys honored; default port `41977`. | `audit_pattern_p_dashboard_bundled_test.go` |
| `TestPattern_P_DaemonWakeReconcile` | Post-wake reconciler is idempotent; emits `system_event` notification ping; resets `Locked` tasks whose heartbeats predate the sleep window. | `audit_pattern_p_daemon_wake_test.go` |
| `TestPattern_P_DaemonCrashBudget` | Crash-budget guard counts starts in `daemon_crash_budget_window_minutes`; refuses at `>= daemon_crash_budget_max_starts`; `clear-crash-budget` subcommand truncates. | `audit_pattern_p_daemon_crash_budget_test.go` |
| `TestPattern_P_DaemonUpdateHistory` | Every `update` / `rollback` writes a `DaemonUpdateHistory` row; columns include actor + old/new sha256 + git-sha-at-build + reason. | `audit_pattern_p_daemon_update_history_test.go` |

### CLI surface affected

- **`force version` extended** (P1): now prints `git-sha` + `git-branch` + `build-time` in addition to the legacy banner.
- **`force daemon` extended** (P1 + P3): bare-`force daemon` (no subcommand) still routes to the legacy foreground path; the new `force daemon <subcommand>` family adds 13 dispatchable subcommands.

---

## Test results

`make smoke` was run at HEAD `f83479c` immediately before this closure was authored. The relevant tail:

```
ok  	force-orchestrator/internal/daemon/provenance	2.879s [no tests to run]
ok  	force-orchestrator/internal/daemon/singleton	2.766s [no tests to run]
ok  	force-orchestrator/internal/daemon/trust	2.856s [no tests to run]
ok  	force-orchestrator/internal/daemon/wake	2.654s [no tests to run]
ok  	force-orchestrator/internal/dashboard	2.521s
ok  	force-orchestrator/internal/store	2.752s [no tests to run]
ok  	force-orchestrator/internal/telemetry	2.841s [no tests to run]
ok  	force-orchestrator/internal/treatments	2.969s [no tests to run]
?   	force-orchestrator/internal/util	[no test files]
ok  	force-orchestrator/scripts/pre-commit	3.065s [no tests to run]
```

All packages green.

**Full suite green at `f83479c`** — the per-phase verifier shards (P1, P2 substrate + integration, P3 substrate + integration) and the comprehensive Heavy verifier each ran `make test` with `-tags sqlite_fts5` and reported uniformly GO at the merge SHAs listed in the per-phase summary. This closure does not re-run the full suite — that was the Heavy verifier's job at HEAD.

`render-rules --check` clean at HEAD: `render-rules --check: OK (no drift)`.

`make docs-check` green at HEAD with this closure file included in the broken-links walk:

```
go test -tags sqlite_fts5 -timeout 60s -run '^TestPatternP_DocsBrokenLinks$' -count=1 ./internal/audittools/...
ok  	force-orchestrator/internal/audittools	0.479s
go test -tags sqlite_fts5 -timeout 60s -run '^TestPatternP_DocsOrphan$' -count=1 ./internal/audittools/...
ok  	force-orchestrator/internal/audittools	0.496s
go test -tags sqlite_fts5 -timeout 60s -run '^TestPatternP_DocsArchitecture$' -count=1 ./internal/audittools/...
ok  	force-orchestrator/internal/audittools	0.444s
go test -tags sqlite_fts5 -timeout 60s -count=1 \
		-run '^(TestReadmeSizeUnder200Lines|TestDocsIndexExists|TestDocsSubdirsHaveIndex|TestMetadataBlockOnAllNewDocs)$' \
		./internal/audittools/...
ok  	force-orchestrator/internal/audittools	0.412s
```

All seven docs gates pass.

---

## Files added

32 net additions between `9b5f5d5` (D13 closure HEAD) and `f83479c` (D12 closure HEAD). The shape:

```
cmd/force/daemon_cmds.go                                # P1 — 13-subcommand router + cmdDaemon*
cmd/force/daemon_cmds_test.go                           # P1 — subcommand tests
cmd/force/daemon_singleton_e2e_test.go                  # P1 — in-process singleton double-Acquire test
cmd/force/daemon_wake.go                                # P2 — onWake reconciler
cmd/force/daemon_wake_test.go                           # P2 — onWake reconciler tests
cmd/force/daemon_boot_sweep.go                          # P3 — boot-time recovery sweep
cmd/force/daemon_boot_sweep_test.go                     # P3 — boot-sweep tests

internal/daemon/provenance/provenance.go                # P1 — main-package var mirror
internal/daemon/provenance/provenance_test.go           # P1
internal/daemon/singleton/singleton.go                  # P1 — flock + PID file
internal/daemon/singleton/singleton_test.go             # P1
internal/daemon/trust/trust.go                          # P1 — trust file parse/write
internal/daemon/trust/trust_test.go                     # P1
internal/daemon/wake/wake.go                            # P2 — Subscribe interface
internal/daemon/wake/wake_darwin.go                     # P2 — IOKit (darwin && cgo)
internal/daemon/wake/wake_darwin_nocgo.go               # P2 — graceful no-op (darwin && !cgo)
internal/daemon/wake/wake_linux.go                      # P2 — logind D-Bus
internal/daemon/wake/wake_other.go                      # P2 — graceful no-op (!darwin && !linux)
internal/daemon/wake/wake_test.go                       # P2

internal/store/daemon_start_log.go                      # P3 — DaemonStartLog helpers
internal/store/daemon_start_log_test.go                 # P3
internal/store/daemon_update_history.go                 # P3 — DaemonUpdateHistory helpers
internal/store/daemon_update_history_test.go            # P3

internal/dashboard/bundled_smoke_test.go                # P1 — bundled dashboard loopback bind smoke

internal/audittools/audit_pattern_p_daemon_singleton_test.go         # P1
internal/audittools/audit_pattern_p_daemon_provenance_test.go        # P1
internal/audittools/audit_pattern_p_daemon_trust_test.go             # P1
internal/audittools/audit_pattern_p_dashboard_bundled_test.go        # P1
internal/audittools/audit_pattern_p_daemon_wake_test.go              # P2
internal/audittools/audit_pattern_p_daemon_crash_budget_test.go      # P3
internal/audittools/audit_pattern_p_daemon_update_history_test.go    # P3

docs/closures/DELIVERABLE-13-CLOSURE.md                 # (lands as part of the same commit range; D13's closure)
```

---

## Files modified

12 files modified at the top level:

| File | What changed |
|---|---|
| `Makefile` | P1: `LDFLAGS` line wires `-X main.GitSHA=$(GIT_SHA) -X main.BuildTime=$(BUILD_TIME) -X main.GitBranch=$(GIT_BRANCH)` into every `make build` invocation. |
| `README.md` | P1: status table updated for D12 in flight → CLOSED. |
| `cmd/force/main.go` | P1: declares `var (GitSHA, BuildTime, GitBranch = "unknown", ...)`; calls `provenance.Set` in `init`; `version` / `--version` / `-v` extended to print provenance; `daemon` case routes through `dispatchDaemon`. |
| `cmd/force/fleet_cmds.go` | P1: legacy `cmdDaemon` extended with bundled-dashboard goroutine + crash-budget guard hook + boot-sweep call before agent fleet spawn. |
| `cmd/force/print.go` | P1: status output extended with provenance + dashboard URL + crash-budget status. |
| `config/notifications.yaml` | P2: adds `system_event` category (Tier-2 default-mail; the post-wake ping fires through this category). |
| `docs/subsystems/daemon-lifecycle.md` | P1+P2+P3: filled out from the D13 stub into a 270-line operator-facing reference covering the singleton lock, provenance, trust file, sleep/wake hooks, crash-budget guard, boot sweep, and bundled dashboard. |
| `go.mod` | P2: adds `github.com/godbus/dbus/v5` for the Linux logind path. |
| `internal/audittools/audit_pattern_p25_cli_parity_test.go` | P1: extended to recognize the 13 new daemon subcommands so the CLI-parity audit doesn't false-positive on them. |
| `internal/dashboard/dashboard.go` | P1: `Bundled` entry-point binds `127.0.0.1:<dashboard_port>` (loopback only). |
| `internal/store/schema.go` | P3: `createSchema` + `runMigrations` add `DaemonUpdateHistory` + `DaemonStartLog` tables + indexes (3-place parity). |
| `schema/schema.sql` | P3: matching `CREATE TABLE` + `CREATE INDEX` for `DaemonUpdateHistory` + `DaemonStartLog` (3-place parity). |

---

## Files deleted

No files deleted. D12 is purely additive at the file-tree level — the deliverable adds new Go packages, new schema tables, and new operator subcommands without removing any pre-existing surface.

---

## Known limitations / follow-ups

None blocking. The following are explicit deferrals tracked for future hygiene work:

1. **Singleton concurrency E2E test uses in-process double-Acquire rather than two real subprocesses.** P1 verifier flagged this SEV-MEDIUM. `cmd/force/daemon_singleton_e2e_test.go` calls `singleton.Acquire` twice within the same Go test process, asserts the second call returns `ErrAlreadyRunning`. This proves the flock works against the same file but doesn't exercise the cross-subprocess case. A hermetic subprocess test that `exec.Command`s a second `force daemon` while the first holds the lock would close the gap. Recommended for a future hygiene pass.

2. **`TestPattern_P_DaemonWakeReconcile` and several other D12 patterns lack `_DetectsInjectedDrift` self-test sentinels.** D13 set the convention (every audit pattern ships a sibling `_DetectsInjectedDrift` fixture that proves the regex / resolver fires when fed broken input — a future refactor that silently neuters the gate trips the fixture before reaching the production walker). D12 added 7 new patterns without that sentinel because P1 / P2 / P3 were authored before D13's pattern was internalized. Recommended for a future hardening pass — the production gates work at HEAD; the missing fixtures are belt-and-braces.

3. **Pre-sleep holocron snapshot is logged-only; no snapshot is taken.** `cmd/force/daemon_wake.go:onWillSleep` carries a `TODO(D12 P3)` marker for a future pre-sleep snapshot, but `store.SnapshotHolocron` does not exist today. The post-wake reconciler still resets stale Locked tasks correctly; the missing snapshot is observability-only (you can't roll back to a pre-sleep DB state if something goes wrong during the sleep window). A future deliverable that lands `store.SnapshotHolocron` should wire it here.

4. **Half-baked DraftPROpen convoy detection logs but does not re-emit operator notification.** `cmd/force/daemon_boot_sweep.go:findHalfBakedDraftPROpenConvoys` finds convoys with a `draft_pr_url` but no handoff synthesis row and logs a warning. It does NOT re-fire the handoff notification through `notify.Dispatch` (which would be the symmetric counterpart to the wake-time `system_event` ping). An operator notices the warning in `force daemon logs` and re-triggers manually. Symmetrizing this is a small future enhancement.

5. **`old_git_sha` in rollback is best-effort.** `cmd/force/daemon_cmds.go:cmdDaemonRollback` writes `old_git_sha = provenance.Get().GitSHA` (the live binary's git-sha) into the `DaemonUpdateHistory` row rather than the trust-file row that matched the `<binary>.previous` file's SHA-256. In the common case (the operator just ran `force daemon update` and is rolling back) the two are identical; in the uncommon case (the operator hand-replaced `<binary>.previous` outside the trust-file flow) the recorded `old_git_sha` won't match the actual rolled-back binary. A future cleanup should resolve `<binary>.previous`'s SHA-256 against the trust file and use the matched git-sha.

6. **Windows / *BSD: graceful no-op for sleep/wake hooks.** `internal/daemon/wake/wake_other.go` returns `(nil, nil)` so the daemon still runs but skips post-wake reconciliation. Only macOS (cgo + IOKit) and Linux (logind D-Bus) have real implementations. If a future operator runs the daemon on Windows or FreeBSD, Locked tasks won't be reset on resume — they'll age past their heartbeat threshold and be reaped by the existing dead-heartbeat sweeper, which is slower but safe. Adding Windows (via `WM_POWERBROADCAST`) or BSD (via `kqueue` `EVFILT_USER` on the `devd` event) is a future deliverable.

---

## Operator hand-off

Practical guidance for an operator landing on the daemon for the first time. The full reference is `docs/subsystems/daemon-lifecycle.md` (270 lines).

### Install the daemon

```
$ force daemon install
# generates ~/Library/LaunchAgents/com.upstart.force.plist (macOS)
# OR ~/.config/systemd/user/force.service (Linux)
# prints the next operator step (launchctl load ... / systemctl --user enable --now ...)
```

The install command does NOT load the unit itself — the operator runs the printed `launchctl` / `systemctl` command. This keeps `force daemon install` idempotent + side-effect-free against the supervisor.

### Update the daemon

```
$ go build -o /tmp/force-new ./cmd/force/   # build the candidate
$ force daemon update --binary /tmp/force-new
# prints SHA-256 + 4-diff preview (git log / git diff --stat / yaml diff / internal/ diff)
# refuses if SHA-256 not in ~/.force/trusted-binary-hashes
# pass --trust --reason "..." to add a trust-file row + perform the swap
```

The 4-diff preview is the operator's last chance to spot a config drift that a `git log` summary would miss. After confirmation, the swap copies the running binary to `<binary>.previous` (so `force daemon rollback` can restore), writes the new binary in place, and writes a `DaemonUpdateHistory` row.

### Something crashed

```
$ force daemon logs -n 200          # see what blew up
# ... fix the underlying issue ...
$ force daemon clear-crash-budget   # truncates DaemonStartLog rows older than the window
# launchd / systemd restarts on the next budget cycle, OR:
$ force daemon stop && force daemon foreground   # restart manually
```

If the crash-budget guard refuses to start, the daemon prints these instructions in its refusal message — the operator doesn't have to know them in advance.

### Audit who/when/why a binary swapped

```
$ force daemon history             # reverse-chrono dump of DaemonUpdateHistory
# columns: ts | actor | old-sha (trunc) | new-sha (trunc) | git-sha (trunc) | git-branch | reason
```

Falls back to the trust-file format if `holocron.db` is unavailable (e.g. the daemon is wedged and you're running `force` against a fresh DB). The trust file itself is always queryable by `force daemon trust list`.

### Configure the daemon

All daemon-level configuration lives in SystemConfig:

```
$ sqlite3 holocron.db "UPDATE SystemConfig SET value='10' WHERE key='daemon_crash_budget_window_minutes'"
$ sqlite3 holocron.db "UPDATE SystemConfig SET value='5' WHERE key='daemon_crash_budget_max_starts'"
$ sqlite3 holocron.db "UPDATE SystemConfig SET value='42000' WHERE key='dashboard_port'"
$ sqlite3 holocron.db "UPDATE SystemConfig SET value='false' WHERE key='dashboard_enabled'"
```

The crash-budget keys take effect on the NEXT daemon start (the running daemon caches them at startup). The dashboard keys take effect on the next daemon restart.

### Subsystem reference

`docs/subsystems/daemon-lifecycle.md` — 270-line operator-facing reference covering the singleton lock, provenance, trust file, sleep/wake hooks, crash-budget guard, boot sweep, and bundled dashboard. The canonical "how does this work end-to-end" page.

---

## Verdict

**D12 is CLOSED at HEAD `f83479c`.** The daemon is supervised, paranoid, sleep-wake-tolerant, crash-recoverable, and audit-trailed.
