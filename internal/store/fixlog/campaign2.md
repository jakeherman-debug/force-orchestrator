## Campaign 2 — Scope deferrals (AUDIT-011 read-side, -025, -085, -149)

**AUDIT IDs closed:** AUDIT-011 (read-side), AUDIT-025, AUDIT-085, AUDIT-149

**Branch:** `fix/scope-deferrals`

**What broke.** Four correctness defects the first-wave fixes explicitly
deferred:

- **AUDIT-011 read-side.** Fix #3 migrated the *write*-side Queue* helpers
  off `payload LIKE '%"convoy_id":N,%'` onto `idempotency_key` +
  `idx_bounty_idem`. Seven production *read*-sites still ran the same
  brittle LIKE pattern — full-table scans with JSON-boundary matching that
  mis-matched nested `{"prev":{"convoy_id":5}}` references.
- **AUDIT-025.** Three sinks (escalation_sweeper, medic auto-complete,
  pilot_worktree_reset) wrote `Escalations.status='Resolved'` — a value no
  read-side consumer recognised (the dashboard counted Open, the maintenance
  cleanup deleted Closed). `Resolved` rows accumulated forever, invisible.
- **AUDIT-085.** The dashboard `ActiveCount` query on `/api/stats` omitted
  `Classifying`, `AwaitingChancellorReview`, `ConflictPending`, and
  `Planned` — so the operator could see `0 active` while 50 tasks were
  mid-LLM-classification.
- **AUDIT-149.** The escalation-sweeper issued an unconditional
  `UPDATE Escalations SET status='Resolved' WHERE ... AND status='Open'`
  every tick. An operator who re-opened a previously-auto-closed escalation
  (`UPDATE Escalations SET status='Open' WHERE id=X`) had it silently
  re-closed 10 minutes later by the next sweeper tick.

**What shipped.**

- **AUDIT-011 read-side.** Seven production read-sites migrated from
  `payload LIKE '%"convoy_id":N,%'` to `WHERE convoy_id = ?`:
  - `convoy_review.go`: `runConvoyReview` completed-passes loop cap,
    `lastCompletedFindingsFingerprint`, `hasPriorCleanPass`,
    `dogConvoyReviewWatch` pending-gate.
  - `convoy.go`: `CheckConvoyCompletions` ShipConvoy dedup.
  - `pilot_backfill.go`: `backfillMissingAskBranches` CreateAskBranch dedup.
  - `convoy_ask_branches.go`: `ConvoyReadyToShip.reviewPending`,
    `ListReadyToShipConvoyIDs` NOT EXISTS ConvoyReview subquery.
  - Bonus correctness fix: `QueueShipConvoy` was not stamping `convoy_id`
    on the row it inserted — migrated it so the ShipConvoy dedup at
    `convoy.go:59` works against the structured column.
- **AUDIT-025.** Every sink site flipped to `SET status='Closed'`;
  docstring legacy comment pruned from `convoy_ask_branches.go`; schema
  migration added to `runMigrations`:
  ```sql
  UPDATE Escalations
     SET status='Closed',
         acknowledged_at=COALESCE(NULLIF(acknowledged_at,''), datetime('now'))
   WHERE status='Resolved'
  ```
  Idempotent — second run finds zero rows.
- **AUDIT-085.** The `ActiveCount` SQL now enumerates ten statuses:
  `Locked, Classifying, Planned, ConflictPending, AwaitingCaptainReview,
  UnderCaptainReview, AwaitingCouncilReview, UnderReview,
  AwaitingChancellorReview, AwaitingSubPRCI`.
- **AUDIT-149.** New column `Escalations.auto_resolve_count INTEGER
  DEFAULT 0` (createSchema + runMigrations, both additive). Sweeper UPDATE
  extended to `SET ..., auto_resolve_count = auto_resolve_count + 1` with
  a `WHERE ... AND auto_resolve_count < 1` gate. First auto-close bumps
  0 → 1; operator re-open leaves the counter at 1; next tick's UPDATE
  matches zero rows and the row stays Open.

**How it was proved.**

- `internal/agents/convoy_id_read_path_test.go` (new) — table-driven
  EXPLAIN QUERY PLAN assertion against all 7 migrated read-sites. Seeds
  10k rows across 10 convoys, confirms each query plan uses an index
  (typically `idx_bounty_convoy_status`) and does not fall back to
  `SCAN BountyBoard`. Separate test covers the `ListReadyToShipConvoyIDs`
  correlated subquery which has two indexed NOT EXISTS blocks.
- `TestPattern_P3_BoundaryFalsePositive` (existing, un-skipped + rewritten
  to post-fix semantics) — asserts two rows with the same nested-JSON
  shape but different `convoy_id` columns do not collide.
- `internal/store/campaign2_migration_test.go` (new) —
  `TestAUDIT_025_ResolvedToClosedMigration` builds an ancestor-schema DB,
  seeds a mix of `Resolved`/`Open`/`Closed`/`Acknowledged` rows, runs
  migrations, asserts `Resolved`→`Closed` flip, `acknowledged_at` populated
  when empty and preserved when stamped, idempotent on a second run.
  `TestAUDIT_149_AutoResolveCountColumnAdded` probes PRAGMA table_info to
  confirm the column + default.
- `TestDogEscalationSweeper_RespectsOperatorReopen` (new) — headline
  AUDIT-149 behavioural test. Auto-close, operator re-open, second tick:
  status stays Open, counter stays at 1.
- `internal/dashboard/active_count_test.go` (new) —
  `TestAUDIT_085_ActiveCountCoversAllInFlightStatuses` seeds one row per
  previously-counted status + one per AUDIT-085 addition, hits
  `/api/stats`, asserts `ActiveCount` reflects the full union and that
  non-in-flight statuses (`Pending`, `Completed`, `Cancelled`, `Failed`,
  `Escalated`) are not counted.
- Pattern P6 sub-tests B + C un-skipped; audit-spotcheck C un-skipped.
- Existing `escalation_sweeper_test.go` / `medic_recovery_test.go` /
  `audit_silent_failures_test.go` expectations updated from `Resolved`
  → `Closed`.
- 4 entries removed from `internal/audittools/audittools_test.go`
  allowlist; `make test-audit` stays green.

**Stats.**

- 3 new test files (`convoy_id_read_path_test.go`,
  `campaign2_migration_test.go`, `active_count_test.go`).
- 14 new test cases across those three files (7 EXPLAIN sites + 2
  migration tests + 1 behavioural re-open test + 4 acceptance cases).
- 4 `t.Skip("AUDIT-*")` lines removed; 0 `t.Skip` added.
- 7 production read-sites migrated; 1 production insert site
  (`QueueShipConvoy`) had convoy_id stamping backfilled.

**Watch for.**

- Any new spawner that writes an Escalations UPDATE MUST use `Closed`,
  never `Resolved`. The P6 sub-test B now catches this as a regression.
- The `auto_resolve_count < 1` gate is a one-shot budget. If we ever
  need the sweeper to auto-close the *same* escalation twice (e.g. after
  a distinct re-escalation cycle), the counter needs to be reset at the
  reopen point, not the close point. Current design: operator re-open
  leaves the counter at 1; only `CloseEscalation`/`AckEscalation` moves
  the row off `Open` cleanly.
- `idx_bounty_convoy_status` is already present from Fix #4. Do not
  drop it — six production reads depend on it.
- Any new ConvoyReview-shaped row (convoy-scoped infrastructure task
  like ShipConvoy, CreateAskBranch, PRReviewTriage) MUST set `convoy_id`
  at insert time. The EXPLAIN QUERY PLAN test will catch a write that
  leaves it at 0.
