---
audience: both
scope: Canonical paths for every runtime state file the daemon and CLI touch — the forcepath resolver + override chain.
owner: architecture
last_reviewed: 2026-05-18
subsystem: state-files
type: subsystem-doc
---

# Canonical state-file paths (`internal/forcepath`)

Pre-Sweep-F, `holocron.db` / `fleet.log` / `holonet.jsonl` / `fleet.pid` / `fleet-task-<id>.log` all resolved from CWD. A daemon launched from `~/code/force-orchestrator/` and a CLI invoked from `~/code/` silently opened **different files** — the operator hit "`force repos` shows nothing" because the CLI created an empty DB in its own CWD instead of seeing the daemon's data.

The fix is one resolver, one canonical home, one resolution chain. Every runtime state file the daemon and CLI touch routes through [`internal/forcepath`](../../internal/forcepath/forcepath.go).

## Default layout

```
~/.force/
├── holocron.db              # SQLite source of truth (+ -wal, -shm sidecars)
├── force.pid                # singleton lock (D12)
├── trusted-binary-hashes    # D12 trust file (operator-curated SHAs)
├── fleet.log                # process-wide human-readable log
├── holonet.jsonl            # structured event stream (rotated → holonet-<stamp>.jsonl at 50 MB)
├── backups/                 # `make install-snapshots` hourly sqlite3 .backup output
├── logs/                    # per-agent log subdirectory (planned per-agent stream)
│   └── astromech-<name>.log
└── scratch/                 # per-task scratch logs (removed on completion)
    └── fleet-task-<id>.log
```

`~/.force/` is created at mode `0700` on first call (operator-private — the trust file and event streams may contain redacted-but-not-perfectly-scrubbed material).

## Resolution chain

Every helper in [`internal/forcepath`](../../internal/forcepath/forcepath.go) (`Holocron()`, `HolocronFile()`, `PIDFile()`, `FleetLog()`, `HolonetEventStream()`, `AstromechLog(agent)`, `ScratchTaskFile(taskID)`, `Dir()`) follows the same chain:

1. **`FORCE_HOLOCRON_DSN`** env (DB only) — verbatim SQLite DSN. Supports `:memory:`, custom file paths, and pragma query strings. Tests use this to keep CI hermetic.
2. **`FORCE_DIR`** env — operator state directory. Every helper appends its specific filename (`$FORCE_DIR/holocron.db`, `$FORCE_DIR/fleet.log`, etc.).
3. **`~/.force/<file>`** — the default.

A test that needs to twiddle `FORCE_DIR` mid-run must call `forcepath.ResetDirCacheForTests()` between mutations; the resolved Dir is memoised after first use.

## Migration from legacy CWD layout

Pre-Sweep-F daemons wrote `./holocron.db` to the directory they were launched from. `forcepath.MigrateLegacyHolocronDB(ctx, db, candidateDir)` safely promotes a legacy DB to the canonical location when:

- the canonical file is missing (or zero-sized — half-created scratch from an aborted boot),
- the legacy file under `candidateDir` exists and is non-empty,
- the candidate dir is not itself the canonical dir.

Both files present with data is the **AMBIGUOUS** case — the migration helper returns an error and refuses to choose. The operator picks the winner manually.

## Operator workflow — Makefile targets

`make protect-db`, `make unprotect-db`, and `make db-status` resolve the DB path via `$(DB_PATH)`, which honours `$FORCE_DIR` and defaults to `~/.force/holocron.db`. They work from any cwd:

```bash
# default: ~/.force/holocron.db
make protect-db
make db-status

# operator override (also works for testing the workflow)
FORCE_DIR=/tmp/staging-state make protect-db
FORCE_DIR=/tmp/staging-state make db-status
```

Hourly snapshots (`make install-snapshots`) and the recovery flow (copy from `~/.force/backups/` → `~/.force/holocron.db`) live in [`operator-runbook.md`](../operator-runbook.md) § Holocron corrupted.

## Code contract — Pattern P_CanonicalPaths

[`internal/audittools/audit_pattern_p_canonical_paths_test.go`](../../internal/audittools/audit_pattern_p_canonical_paths_test.go) walks every production `.go` file under `cmd/` and `internal/` and rejects any string literal whose shape matches a CWD-relative state file:

- `"./holocron.db"`, `"holocron.db"`, `"./holocron.db-wal"`, `"holocron.db-shm"`, …
- `"./fleet.log"`, `"fleet.log"`
- `"./holonet.jsonl"`, `"holonet.jsonl"`
- `"./fleet.pid"`, `"force.pid"`
- `"./fleet-task-<n>.log"`, `"fleet-task-<n>.log"`

Exempt packages:

- `internal/forcepath/*` — the resolver owns the bare filenames it joins onto `Dir()`.
- `internal/daemon/singleton/*` — D12's PID-path construction; the resolver forwards to it.

A drift-sentinel sibling test feeds a synthetic source snippet with every forbidden shape to the matcher and asserts each one is flagged. If anyone weakens the matcher into a no-op the sentinel fails first.

## Why the resolver, not a global?

Two reasons:

1. **Env-var override is a hard test contract.** A package-level `var holocronDSN string` initialised at import time bakes the resolution to the test process's first-call moment — `t.Setenv` after that point is invisible. Function-form helpers re-evaluate on every call.
2. **Mkdir-on-first-call needs to be lazy.** A daemon process should create `~/.force/` when it actually opens the DB, not when a CLI command happens to import the package transitively. The resolver creates the directory at most once per process, behind a mutex.

## Related

- [`gas-town.md`](gas-town.md) — why `holocron.db` is the sole coordination substrate.
- [`holocron-schema.md`](holocron-schema.md) — schema parity + migration discipline (separate concern from where the file lives).
- [`daemon-lifecycle.md`](daemon-lifecycle.md) — singleton PID file + drain.
- [`../operator-runbook.md`](../operator-runbook.md) — operational levers, ACLs, snapshots, recovery.
