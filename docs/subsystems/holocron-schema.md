---
audience: agent
scope: holocron.db schema discipline — schema parity, migration shape, idempotence rules.
owner: architecture
last_reviewed: 2026-05-05
subsystem: holocron-schema
type: subsystem-doc
---

# holocron.db schema discipline

`holocron.db` is the SQLite database that backs the [Gas Town pattern](gas-town.md). Every column the fleet relies on must be present in *both* the fresh-DB path AND the migration path, with bytes that match. This document captures the schema-coherence invariants — the rules a CodeEdit agent must obey when adding or modifying a column.

## Overview

Schema management in Force has two ingress points, and `TestSchemaParity` fails CI if they disagree:

- **`createSchema`** — `CREATE TABLE IF NOT EXISTS …` for every table; used on a fresh DB.
- **`runMigrations`** — `ALTER TABLE … ADD COLUMN …` for every additive change; used to upgrade an existing DB.
- **`schema/schema.sql`** — the canonical reference DDL; tests parse it and compare against `createSchema`'s output.

Both `createSchema` and `runMigrations` run automatically from `InitHolocronDSN`. Migrations are additive only — the project never destructively rewrites tables in production.

## Components

- **`internal/store/schema.go`** — `createSchema`, `runMigrations`, table list, `RenderTo` decision rationale.
- **`schema/schema.sql`** — canonical DDL reference; round-trips through `TestSchemaParity`.
- **`internal/store/store.go`** — `InitHolocronDSN` (the entry point), `NowSQLite`, `ParseSQLiteTime`.
- **`internal/store/migrations.go`** — `columnExists`, helpers for gated migrations.
- **`TestSchemaParity`** — the regression that fails CI if `createSchema` and `runMigrations` produce divergent schemas.
- **Pattern P3** (`audit_pattern_p3_*`) — convoy-scoped queries use `convoy_id`, never `payload LIKE '%"convoy_id":N%'`.
- **Pattern P4** (`audit_pattern_p4_*`) — hot-table indexes present; claim queries use `idx_bounty_*`.

## Invariants

1. **Schema parity.** When adding a column, add it to `createSchema` AND `runMigrations` AND `schema/schema.sql` in the same commit. `TestSchemaParity` will fail otherwise.
2. **Idempotent migrations.** Re-running the same migration twice is a no-op. Use `IF NOT EXISTS` for tables; rely on `ALTER TABLE ADD COLUMN`'s silent failure on duplicates. For destructive ALTERs, gate on `columnExists(db, table, column)` (Fix #8c / AUDIT-077).
3. **Default coherence on backfill.** When the upgrade-path `ALTER TABLE ADD COLUMN col TEXT DEFAULT ''` lands but `createSchema` uses a non-empty default (e.g. `DEFAULT (datetime('now'))`), follow the ALTER with a backfill `UPDATE` so drifted rows are repaired.
4. **NULL-tolerant SELECTs.** Use `IFNULL(col, '')` when reading columns that might be NULL on rows written before the column existed.
5. **Canonical timestamp helpers.** `store.NowSQLite()` and `store.ParseSQLiteTime(s)` are the only blessed shapes for SQLite UTC timestamps. Any new code comparing `datetime('now')` against a Go-side "now" must route through these.
6. **Convoy-scoped queries use `convoy_id`.** Never `payload LIKE '%"convoy_id":N%'` — that's a full-table scan with brittle JSON-boundary matching. New convoy-scoped infra task types must populate `convoy_id` at insert time. Pattern P3 fails CI on regressions.
7. **Partial UNIQUE indexes back idempotency.** Three indexes in particular:
   - `idx_bounty_idem ON BountyBoard(idempotency_key) WHERE idempotency_key != '' AND status NOT IN ('Completed','Cancelled','Failed')`
   - `idx_escalations_open_task ON Escalations(task_id) WHERE status = 'Open'`
   - `idx_feature_blockers_open ON FeatureBlockers(blocked_convoy_id, blocking_feature_id) WHERE resolved_at IS NULL`
8. **No `void` mutators.** New store mutator functions must return `error`. Pattern enforced indirectly via the "no silent failures" rule (CLAUDE.md).

## Configuration

There are no operator-facing knobs for schema; the DB is opened with WAL mode and busy-timeout defaults baked into `InitHolocronDSN`. Operator-facing concerns:

- **WAL files**: `holocron.db-wal` and `holocron.db-shm` always live alongside the DB. The `make protect-db` ACL covers all three.
- **Snapshots**: `make install-snapshots` installs hourly WAL-consistent `.backup` snapshots into `~/.force/backups/`; daily 04:00 cron prunes >30 days. `make uninstall-snapshots` reverses.
- **Status**: `make db-status` shows the current ACL, snapshot crontab entries, and most recent snapshots.
- **Vacuum**: the `db-vacuum` dog (6h cooldown) runs `PRAGMA wal_checkpoint`, `ANALYZE`, `VACUUM`.

## Operator surface

```bash
sqlite3 holocron.db '.schema'                                  # full schema dump
sqlite3 holocron.db 'PRAGMA integrity_check'                   # integrity probe
sqlite3 holocron.db 'PRAGMA wal_checkpoint(TRUNCATE)'          # manual checkpoint
make db-status                                                 # ACL + snapshot status
make protect-db / make unprotect-db                            # accidental-delete guard
make install-snapshots / make uninstall-snapshots              # WAL-consistent backup cadence
```

For diagnosing schema drift between two DBs:

```bash
sqlite3 db1.db .schema > a.sql
sqlite3 db2.db .schema > b.sql
diff a.sql b.sql
```

For migration rollback (DESTRUCTIVE — daemon must be stopped):

```bash
ls -1 holocron.db.pre-pr-flow.* | tail -1     # find latest snapshot
cp holocron.db.pre-pr-flow.<ts> holocron.db   # restore
# OR for the PR-flow migration specifically:
force migrate pr-flow --rollback --confirm
```

## See also

- [`gas-town.md`](gas-town.md) — the architecture pattern this schema serves.
- [`../CLAUDE.md`](../../CLAUDE.md) — schema conventions in the auto-rendered architecture invariants.
- [`../FIX-LOG.md`](../../FIX-LOG.md) — Fix #8c / AUDIT-077 narrative on `columnExists`-gated destructive ALTERs.
- [`../../schema/schema.sql`](../../schema/schema.sql) — canonical DDL reference.
