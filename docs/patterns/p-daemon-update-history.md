---
audience: operator
scope: cmdDaemonUpdate and cmdDaemonRollback record a DaemonUpdateHistory row on every exit path; schema is parity-declared in createSchema, runMigrations, and schema/schema.sql.
owner: infrastructure
last_reviewed: 2026-05-07
title: Pattern P_DaemonUpdateHistory — D12 binary-swap audit-trail integrity
type: pattern-doc
pattern: P_DaemonUpdateHistory
---

# Pattern P_DaemonUpdateHistory — D12 binary-swap audit-trail integrity

## Rationale

`DaemonUpdateHistory` is the operator-facing audit trail for every
binary swap. Each row records the actor, the old and new SHA-256s,
the `provenance.Get().GitSHA` snapshots, the trigger reason, and the
outcome. Together with `DaemonStartLog` it answers "why is this
binary running, and is its history clean?".

D12 P3 added the table and made every exit path of `cmdDaemonUpdate`
and `cmdDaemonRollback` write a row via `store.RecordDaemonUpdate`.
The implementation uses a `defer`-based recorder pattern so a future
refactor that adds a new `return` branch automatically lands a row
without further code changes — provided the deferred call survives.
A regression where the deferred call is dropped (or the schema falls
out of parity across the three declaration sites) is the failure
mode this audit defends against.

Closure narrative:
[`docs/closures/DELIVERABLE-12-CLOSURE.md`](../closures/DELIVERABLE-12-CLOSURE.md)
("Every binary swap creates an audit row in `DaemonUpdateHistory`")
and [`docs/subsystems/daemon-lifecycle.md`](../subsystems/daemon-lifecycle.md)
§ "DaemonUpdateHistory table".

## What it checks

The single test `TestPattern_P_DaemonUpdateHistory` runs two checks:

1. **Both update entry-points record an audit row.** AST walks
   `cmd/force/daemon_cmds.go` and asserts that the top-level
   `FuncDecl`s named `cmdDaemonUpdate` and `cmdDaemonRollback` each
   contain at least one `CallExpr` of shape
   `store.RecordDaemonUpdate(...)`. The walk accepts both direct
   calls and deferred calls (a `defer func() { ... store.RecordDaemonUpdate(...) ... }()`
   wraps a `CallExpr` inside the inner func body, which `ast.Inspect`
   reaches).
2. **Schema parity across all three locations.** For each of the
   tables `DaemonUpdateHistory` and `DaemonStartLog`:
   - `internal/store/schema.go` must contain
     `CREATE TABLE IF NOT EXISTS <table>` at least twice — once in
     `createSchema` (fresh DBs) and once in `runMigrations` (in-place
     migrations).
   - `schema/schema.sql` must contain the same `CREATE TABLE IF NOT
     EXISTS <table>` substring at least once (the canonical schema
     reference doc).

## How it fails

```
Pattern P_DaemonUpdateHistory: cmdDaemonRollback does not call store.RecordDaemonUpdate — operator-facing history will silently drop this exit path
Pattern P_DaemonUpdateHistory: DaemonUpdateHistory appears 1 time(s) in schema.go, want 2 (createSchema + runMigrations)
Pattern P_DaemonUpdateHistory: DaemonStartLog missing from schema/schema.sql
```

Typical violating snippet (deferred recorder dropped from the
rollback path):

```go
func cmdDaemonRollback(ctx context.Context, db *sql.DB, ...) error {
    // BUG: recorder removed, so a successful rollback (or a failed one)
    // leaves no audit row.
    if err := os.Rename(backupBinary, currentBinary); err != nil {
        return err
    }
    return nil
}
```

A schema-parity failure typically looks like:

```go
// internal/store/schema.go — runMigrations
// (Forgot to add the new ALTER for the table.)
if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS DaemonUpdateHistory (...)`); err != nil {
    return err
}
// MISSING: matching declaration in createSchema, OR missing schema/schema.sql entry.
```

## How to fix

Restore the deferred recorder pattern in both functions:

```go
func cmdDaemonUpdate(ctx context.Context, db *sql.DB, ...) (err error) {
    actor := operatorActor()
    oldSHA := provenance.Get().GitSHA
    var newSHA, outcome string
    defer func() {
        if cerr := store.RecordDaemonUpdate(db, store.DaemonUpdateRow{
            Actor: actor, OldSHA: oldSHA, NewSHA: newSHA, Outcome: outcome,
            Reason: reason, Err: err,
        }); cerr != nil {
            log.Printf("warn: RecordDaemonUpdate failed: %v", cerr)
        }
    }()
    // ... swap logic, set newSHA / outcome along the way ...
}
```

When you add a new column to either `DaemonUpdateHistory` or
`DaemonStartLog`, update all three declaration sites in the same
commit:

- `createSchema` in `internal/store/schema.go`
- `runMigrations` in `internal/store/schema.go`
- `schema/schema.sql`

`TestSchemaParity` is the companion regression that catches a fresh
DB + migrated DB schema drift.

## Test reference

- File: `internal/audittools/audit_pattern_p_daemon_update_history_test.go`
- Core assertion: `TestPattern_P_DaemonUpdateHistory`
- Helpers: standard `go/parser` + `go/ast` walk for the
  `RecordDaemonUpdate` lookup; `mustReadAudit(t, path)` (defined in
  the same file) for the schema-source string reads;
  `strings.Count` for the parity-count assertion.

## See also

- [P_DaemonCrashBudget](p-daemon-crash-budget.md) — the sibling
  D12 P3 audit; together they pin the start / update / rollback
  audit trail end-to-end.
- [P_DaemonTrustFile](p-daemon-trust-file.md) — the trust gate that
  precedes every recorded update.
- [`docs/subsystems/daemon-lifecycle.md`](../subsystems/daemon-lifecycle.md)
  § "DaemonUpdateHistory table" and § "Boot-time recovery sweep".
- `internal/store/schema.go` — `createSchema` + `runMigrations`.
- `schema/schema.sql` — canonical schema reference.
- `force daemon history [--limit N] [--from-trust-file]` —
  operator query surface.
