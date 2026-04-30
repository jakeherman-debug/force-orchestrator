## Campaign 4 — Fix #8c: schema/time/parser cleanup

**AUDIT IDs closed:** AUDIT-077, AUDIT-078, AUDIT-080, AUDIT-082, AUDIT-143,
AUDIT-146, AUDIT-147, AUDIT-148.

**Branch:** `fix/8c-schema-time-parser`

**What broke.** Eight loose ends in schema + time handling + parser
robustness that the earlier fix campaigns left in place, each small on
its own and together a class of "works by coincidence, fails silently on
the second try" defects:

- **AUDIT-077.** `ALTER TABLE BountyBoard DROP COLUMN blocked_by` ran
  unconditionally on every startup. SQLite 3.35+ supports DROP COLUMN but
  the second call errors "no such column." `db.Exec` swallowed the error
  (unchecked return value), so the defect was invisible in the daemon log.
- **AUDIT-078.** `createSchema`'s `created_at TEXT DEFAULT (datetime('now'))`
  vs `runMigrations`'s `ALTER TABLE BountyBoard ADD COLUMN created_at TEXT
  DEFAULT ''`. Upgraded DBs inserted empty-string `created_at` on any INSERT
  that didn't name the column — those rows never aged out via
  `WHERE created_at < datetime('now','-12 hours')` and sat at the bottom of
  the priority queue forever.
- **AUDIT-080.** `schema/schema.sql` (the reference doc) had drifted from
  `internal/store/schema.go` (the source of truth). Stall-retrigger column,
  classify_attempts column, failed_rebase_attempts column were out of sync
  — anyone consulting schema.sql as docs got a false picture.
- **AUDIT-082.** `cmd/force/integration_test.go` inserted into
  `Escalations.reason` — a column that doesn't exist. The real column is
  `message`. `db.Exec` swallowed the "no such column" error; the test was
  asserting "no panic" against an empty Escalations table, not against a
  successful INSERT. The panic-free assertion was vacuously true.
- **AUDIT-143.** PR review classifier had a bounded retry counter
  (`classifyAttemptsCap=3`) but on exhaustion just silently flagged the
  comment row as `classification='human'` via `MarkClassifyUnrecoverable`.
  Operators had to know to look at the dashboard's "unclassified human"
  bucket; no `Escalations` row ever landed.
- **AUDIT-146.** `ListDogs` in `internal/agents/dogs.go` used
  `time.Now().Before(next)` (local wall-clock) against a
  `ParseInLocation("…", time.UTC)` time. Worked today because time.Time
  values carry their Location through `.Before`/`.Sub`, but one `.UTC()`
  refactor away from a real TZ bug.
- **AUDIT-147.** `detectStalledTasks` in `internal/agents/inquisitor.go`
  used raw `time.Parse("2006-01-02 15:04:05", lockedAt)` — stdlib returns
  UTC by default but the implicit contract coupled every caller.
- **AUDIT-148.** `RateLimitBackoff(count)` doubled `d` `count` times
  BEFORE the 10m cap check. A corrupted large `count` (e.g. 62) overflowed
  `int64` nanoseconds to a negative duration; `d > 10*time.Minute` failed
  for negatives; the function returned the wrapped value and callers
  spun with near-zero sleep.

**What shipped.**

- **AUDIT-077** — new `columnExists(db, table, column) bool` helper in
  `internal/store/schema.go` using `pragma_table_info`. The DROP COLUMN
  block is gated on the helper and additionally wraps the companion
  `INSERT ... SELECT ... FROM blocked_by` backfill. Second startup is a
  no-op; no swallowed error.
- **AUDIT-078** — the ALTER default is unchanged (SQLite cannot change a
  column default retroactively without a table rebuild) but a follow-up
  `UPDATE BountyBoard SET created_at = datetime('now') WHERE created_at =
  '' OR created_at IS NULL` re-stamps any drifted rows. Idempotent: the
  second pass matches zero rows.
- **AUDIT-080** — new `TestSchemaParity` in
  `internal/store/schema_parity_test.go`. Parses every
  `CREATE TABLE IF NOT EXISTS` block in both files and asserts each
  table's column set is identical. Ratchets the invariant so a future
  column added to `createSchema` without updating `schema.sql` fails CI.
  Handles the twice-declared-by-design tables (ProposedConvoys,
  FeatureBlockers, ConvoyHolds, ConvoyAskBranches, AskBranchPRs,
  TaskNotes, PRReviewComments) by only honouring the first occurrence
  (createSchema is authoritative over runMigrations's IF-NOT-EXISTS
  compat re-declarations).
- **AUDIT-082** — `cmd/force/integration_test.go` now inserts into
  `Escalations.message` (real column) with status `'Open'` (the
  canonical state, matching Campaign 2's Resolved → Closed migration).
- **AUDIT-143** — `pr_review_triage.go`'s `classifyAttemptsCap` branch
  now calls `CreateEscalation(db, bounty.ID, store.SeverityMedium, msg)`
  in addition to the existing `MarkClassifyUnrecoverable`. If escalation
  creation fails, falls back to operator mail via `store.SendMail` with
  `MailTypeAlert`. The comment row still lands in the 'human' bucket for
  operator triage; the escalation is the observability surface the
  operator sees on the dashboard.
- **AUDIT-146** — `ListDogs` reads the dog timestamp via
  `store.ParseSQLiteTime`, does its comparison against
  `time.Now().UTC()`. Both sides UTC, no accidental-TZ coupling.
- **AUDIT-147** — `detectStalledTasks` routes through
  `store.ParseSQLiteTime`. The new `internal/store/time.go` holds two
  canonical helpers:
  - `NowSQLite() string` — returns UTC time formatted as
    `"2006-01-02 15:04:05"`, suitable for a SQLite-comparable timestamp.
  - `ParseSQLiteTime(s) (time.Time, error)` — UTC-located parse.
- **AUDIT-148** — new const `rateLimitBackoffMaxShifts = 10` (the 60s→10m
  ramp already caps at shift 4; anything above 10 is noise). Pre-loop:
  `if count < 0 { count = 0 }; if count > rateLimitBackoffMaxShifts {
  count = rateLimitBackoffMaxShifts }`. Overflow impossible for any input.

**How it was proved.**

- `internal/store/audit_schema_time_test.go` — the 8 Fix #8c-owned
  subtests had their `t.Skip` lines removed and the assertions flipped
  to post-fix contracts. Each test now includes a behavioural
  observation (not just a static grep) where feasible: AUDIT-077 runs
  migrations twice on a real `:memory:` DB and confirms `blocked_by`
  stays absent; AUDIT-078 seeds a `''` row then re-runs migrations and
  confirms the row is repaired; AUDIT-143 greps inside the
  `if attempts >= classifyAttemptsCap` branch for `CreateEscalation`.
- `internal/store/schema_parity_test.go::TestSchemaParity` — new,
  validates schema.go and schema.sql column parity. Symmetric diff so
  drift in either direction fails.
- `internal/agents/estop_test.go::TestRateLimitBackoff_NoOverflowOnCorruptedCount`
  — new, seeds counts 0..100 plus pathological extremes (62, 63, 1000,
  1<<30) and asserts the returned duration is always >0 and ≤10m. Also
  covers negative inputs (clamp-negative-to-zero).

**Stats.**

- 8 AUDIT IDs closed (all from the Fix #8c schema+time batch).
- 8 `t.Skip("AUDIT-…")` markers removed from `audit_schema_time_test.go`;
  8 allowlist entries removed from `internal/audittools/audittools_test.go`.
- 1 new test file (`schema_parity_test.go`).
- 1 new production file (`internal/store/time.go`).
- 1 new production helper (`columnExists` in `schema.go`).
- 2 new tests (`TestSchemaParity` + `TestRateLimitBackoff_NoOverflowOnCorruptedCount`).

**Watch for.**

- `TestSchemaParity` is the ratchet. If a future PR adds a column to
  `createSchema` without updating `schema.sql`, the test fails with a
  sorted list of the missing columns. Same if a column is added to
  `schema.sql` without landing in `createSchema`.
- `store.NowSQLite` and `store.ParseSQLiteTime` are the canonical
  UTC-aware time helpers for any Go-side comparison against a
  SQLite-written timestamp. Reviewers rejecting `time.Parse("2006-01-02
  15:04:05", …)` in new code pays the invariant forward.
- The `rateLimitBackoffMaxShifts` const is at 10 because the 60s→10m cap
  already triggers at shift 4; keep it well above that so the const
  itself doesn't become load-bearing for the sequence shape.
- Open AUDIT-131 / AUDIT-132 still ship raw `time.Parse` in two remaining
  call sites (`dogs.go` RunDogs dispatch, `pr_flow.go` handleSubPRPoll).
  Those are Fix #8b allowlist entries, not in Campaign 4's scope.
