# Fix Log

Operator narrative for each audit-fix PR. Written as each fix merges to main.
Each entry answers: what broke, what shipped, how it was proved, what to
watch for next.

## Fix #0 — Protected-branch guard

**AUDIT IDs closed:** AUDIT-102, AUDIT-103, AUDIT-104, AUDIT-121, AUDIT-122, AUDIT-124

**Branch:** `fix-0-protected-branch-guard`

**What broke.** Every destructive git op in `internal/git` consumed its
`branch` argument without checking whether it named the repo's default
branch. A single DB-corrupt `ConvoyAskBranches.ask_branch = "main"` row
(from a manual edit or a migration bug) would flow through
`completeAskBranchResolution` and become `git push --force-with-lease origin
main`. In parallel, `pilot_rebase.go:77` hardcoded `defaultBranch = "main"`
as a fallback — so any master-default repo with an empty
`repos.default_branch` looped forever trying to rebase onto a nonexistent
ref, and `pr_flow.go:709` fell back to `branch := pr.Repo` when the parent
task's `branch_name` was empty — a short repo name could collide with the
default branch name and trigger the CI-rerun empty-commit push on origin/main.

**What shipped.**

- New helper in `internal/git/protected.go`:
  - `AssertNotDefaultBranch(repoPath, branch string) error` — three layers:
    empty branch rejected, hard denylist (main/master/develop/trunk/
    production/prod/HEAD, case- and ref-prefix-insensitive), and a repo-aware
    `GetDefaultBranch(repoPath)` check when the path is provided.
  - `IsValidAskBranch(branch string) bool` — checks the
    `<prefix>force/ask-<digits>-<slug>` shape.
  - `IsProtectedBranchName(branch string) bool` — exported subset for store
    ingress validators that can't import `internal/git`.
  - `ErrProtectedBranch` sentinel wrap target.
- Guard installed at the top of `ForcePushBranch`, `TriggerCIRerun`,
  `DeleteAskBranch`, `MergeAndCleanup`, and
  `completeAskBranchResolution`. Every one refuses the op before shelling
  out to git.
- `completeAskBranchResolution` additionally checks
  `IsValidAskBranch(ab.AskBranch)` — a well-formed DB row with a
  default-branch name IS still rejected.
- `pilot_rebase.go:77` replaced its `"main"` literal fallback with
  `igit.GetDefaultBranch(repo.LocalPath)` — master-default repos stop
  looping.
- `pr_flow.go:709` dropped the `branch := pr.Repo` fallback. When the
  parent task's `branch_name` is empty, we escalate instead of pushing to a
  guessed branch.
- Store ingress: `UpsertConvoyAskBranch` now rejects protected branch names
  at write time via a local `isProtectedAskBranchName` helper (duplicated
  denylist to keep the `store → git` layering intact). A corrupt or
  mis-migrated DB cannot admit a "main" row.

**How it was proved.**

- `TestAUDIT_102_103_104_121_122_124_ProtectedBranchGuardsMissing` — 7
  subtests in `internal/git/audit_protected_branch_test.go`. Red-phase
  skips removed; post-Fix assertions inverted so the test now acts as
  permanent regression protection. Also fixed a latent bug in the test's
  `extractFuncBody` helper that mis-reported function bodies when the
  signature contained an inline interface (`logger interface{ Printf... }`).
- `TestAssertNotDefaultBranch_HardDenylist` — 14 cases, table-driven
  unit coverage of the validator (canonicalisation, case-insensitivity,
  ref-prefix stripping, empty input).
- `TestAssertNotDefaultBranch_AllowsAskBranches` — 8 positive cases so
  the denylist doesn't over-broaden.
- `TestAssertNotDefaultBranch_HonoursRepoDefault` — integration; makes a
  real temp repo and confirms the discovered default is rejected.
- `TestForcePushBranch_RefusesProtectedBeforeShellout` — integration;
  calls against a non-existent repo path to prove the guard fires BEFORE
  `git -C` would ever run.
- `TestTriggerCIRerun_RefusesProtectedBeforeShellout` — ditto for the
  CI-rerun path.
- `TestAddRepo_ProtectedBranchFlow` — acceptance; drives the real
  `cmdAddRepo` CLI helper against a live git repo, then proves post-
  registration the store still rejects `ask_branch = "main"`.

**Stats.**

- 14 new unit sub-cases + 8 allow-case sub-cases in
  `protected_test.go` (all t.Parallel).
- 1 repo-aware unit test + 2 integration tests in same file.
- 1 acceptance test in `cmd/force/fix0_addrepo_protected_test.go`.
- 7 audit-test subtests flipped from Red to Green in
  `audit_protected_branch_test.go`.

**Known pre-existing issue surfaced during Fix #0 verification.**

`TestEmitEvent_WithOTLPEndpoint` in `internal/telemetry/telemetry_test.go`
races under `go test -race -count=1` (reproduced against bare main before
any Fix #0 change). The test launches an async HTTP POST goroutine and
resets `otlpEndpoint` / `otlpHTTPClient` in a deferred cleanup without
waiting for the goroutine. This is unrelated to the protected-branch
guard — noted here because the original fix prompt asked for `-race`
cleanliness. The project's canonical `make test` runs without `-race`,
and the full suite is green there. The race belongs in the Fix #10
outbound-channels scope (same file owns OTLP export).

**Watch for.**

- If a future pair of agents needs to rewrite a protected branch for a
  legitimate reason (e.g. repository-init flow creating the default branch
  as a first commit), they'll need to bypass the guard explicitly. That
  bypass must go through a new entry point, not a loosening of
  `AssertNotDefaultBranch` — adding an explicit opt-in argument is
  preferable to relaxing the denylist.
- The store-ingress duplicated denylist
  (`store.isProtectedAskBranchName`) drifts if anyone updates
  `git.protectedBranchNames` without updating `store.protectedAskBranchNames`.
  A cross-package CLAUDE.md directive should probably be added if more
  names land on either side.

## Fix #4 — Hot-table indexes

**AUDIT IDs closed:** AUDIT-009, AUDIT-010, AUDIT-024, AUDIT-058, AUDIT-059,
AUDIT-134, AUDIT-023 (schema-drift companion), AUDIT-079 (PRAGMA
foreign_keys companion), AUDIT-081 (AddRepo UPSERT companion).

**Branch:** `fix/hot-table-indexes`

**What broke.** Every hot query in the fleet was full-scanning its table:

- `BountyBoard` had zero indexes. `ClaimBounty` fires every 2–5s from ~10
  agent loops; `EXPLAIN QUERY PLAN` reported `SCAN BountyBoard`. At 50k
  rows that's 50–100ms per poll, and because `MaxOpenConns=1` serialises
  every statement, dashboard refreshes and dog ticks compound into
  fleet-wide stall.
- `TaskHistory` was unindexed too, so `handleTasks`'s per-row correlated
  subqueries for tokens-in / tokens-out cost ~100 full scans per dashboard
  page at 100k history rows.
- `Fleet_Mail`, `Escalations`, `AuditLog`, `FleetMemory` were all
  unindexed despite being scanned by the agent claim loop, the
  escalation-sweeper dog, and the dashboard `/api/status` endpoint every
  5s.
- `AskBranchPRs(task_id)` existed, but the escalation-sweeper's
  `GROUP BY task_id / MAX(id)` subquery had to sort — a composite
  `(task_id, id DESC)` index lets SQLite jump straight to the latest
  row per task.
- `createSchema` was missing `Fleet_Mail.consumed_at` and
  `Repositories.pr_review_enabled`, both of which only `runMigrations`
  added. Fresh installs were silently drifting from the authoritative
  schema (AUDIT-023).
- `InitHolocronDSN` never executed `PRAGMA foreign_keys=ON`. SQLite
  defaults FK enforcement OFF per connection, so the single
  `REFERENCES BountyBoard(id)` clause on `TaskNotes` was advisory; the
  maintenance prune `DELETE FROM BountyBoard` created orphan notes
  silently (AUDIT-079).
- `AddRepo` used `INSERT OR REPLACE INTO Repositories`, which SQLite
  specifies as DELETE-then-INSERT on PRIMARY KEY collision. Every
  re-registration churned the row's identity; under FK enforcement, it
  would have cascade-deleted every row referencing `Repositories.name`
  (AUDIT-081).

**What shipped.**

- One migration block in `schema.go` adding 16 indexes total:
  - `BountyBoard` — `idx_bounty_status_type`, `idx_bounty_convoy_status`,
    `idx_bounty_parent_id`, `idx_bounty_created_at`.
  - `TaskHistory` — `idx_taskhistory_task_id`,
    `idx_taskhistory_created_at`, `idx_taskhistory_outcome_agent`.
  - `Fleet_Mail` — `idx_mail_to_consumed`, `idx_mail_task_id`,
    `idx_mail_created_at`.
  - `Escalations` — `idx_escalations_status`, `idx_escalations_task_id`.
  - `AuditLog` — `idx_auditlog_created_at`, `idx_auditlog_task_id`.
  - `FleetMemory` — `idx_fleet_memory_repo_created`.
  - `AskBranchPRs` — `idx_ask_branch_prs_task_id_id_desc` (composite for
    the escalation-sweeper's `MAX(id) GROUP BY task_id`).
- Every index is declared in BOTH `createSchema` (fresh installs) and
  `runMigrations` (upgrades). Every `CREATE INDEX` uses `IF NOT EXISTS`
  so re-running the migration is a no-op.
- `createSchema`'s Fleet_Mail definition gained `consumed_at`; its
  Repositories definition gained `pr_review_enabled`. Closes the
  AUDIT-023 drift.
- `InitHolocronDSN` now executes `PRAGMA foreign_keys=ON` **after**
  createSchema + runMigrations so the table-rebuild path can't trip
  FK checks mid-migration.
- `TaskNotes.task_id` gained `ON DELETE CASCADE` in the createSchema
  definition. A one-shot migration (gated on `sqlite_master.sql`
  containing `ON DELETE CASCADE`) rebuilds any pre-existing TaskNotes
  table that lacks the cascade clause — the standard 12-step
  rebuild-and-rename. Maintenance prune of BountyBoard now cascades
  cleanly instead of failing (FK on) or silently orphaning (FK off).
- `AddRepo` switched from `INSERT OR REPLACE` to
  `INSERT ... ON CONFLICT(name) DO UPDATE SET local_path=excluded…,
  description=excluded…`. Row identity is preserved across
  re-registration, and the previous defensive read-back-then-overwrite
  scaffolding (which was there precisely because REPLACE clobbered
  PR-flow fields) goes away entirely — UPSERT leaves non-updated
  columns alone by construction.
- `schema/schema.sql` reference file updated to mirror `schema.go`:
  `PRAGMA foreign_keys=ON`, all new indexes, Fleet_Mail.consumed_at,
  Repositories.pr_review_enabled, TaskNotes ON DELETE CASCADE, and
  AskBranchPRs.stall_retrigger_count (AUDIT-080's reference-drift was
  adjacent enough to fix in the same pass — the column was always in
  `schema.go`).

**How it was proved.**

- `TestPattern_P4_HotTablesMissingIndexes` — skip removed; the 13
  existing sub-cases all report `USING INDEX` now.
- `TestPattern_P4_ClaimQueryUsesIndex` — skip removed; the full
  ClaimBounty SQL (with dependency + FeatureBlockers subqueries)
  reads as `SEARCH BountyBoard USING INDEX idx_bounty_status_type`.
- `TestAUDIT_023_createSchema_drift` — skip removed; createSchema now
  contains `consumed_at` and `pr_review_enabled` inline.
- `TestAUDIT_MediumSpotcheckB/AUDIT_079_foreign_keys_pragma_never_enabled`
  — skip removed; test body inverted from red-phase grep-for-defect to
  green-phase assert-pragma-present-and-live.
- `TestAUDIT_MediumSpotcheckB/AUDIT_081_…cascading_delete` — skip
  removed; test asserts UPSERT shape in source AND behavioural check
  that `QuarantineRepo` state survives a subsequent `AddRepo`.
- Five new tests in `internal/store/hot_table_indexes_test.go`:
  1. `TestHotTableIndexes_CreateAndMigrateAgree` — iterates every
     expected `(table, column-prefix)` pair against both a fresh DB
     (createSchema path) and a migrated DB (createSchema +
     runMigrations); reports a diff if the two disagree.
  2. `TestHotTableIndexes_ForeignKeysEnforcedAndCascade` — live
     `PRAGMA foreign_keys` returns 1, plus a real
     `DELETE FROM BountyBoard` that cascades to TaskNotes.
  3. `TestHotTableIndexes_ClaimQueryUsesIndex_10kRows` — seeds 10k
     realistic rows, asserts EXPLAIN PLAN shows the index, and checks
     the query completes in < 50ms.
  4. `TestHotTableIndexes_EscalationSweeperGroupByUsesIndex` — runs
     EXPLAIN on the exact 4-way JOIN in `escalation_sweeper.go`;
     asserts both `AskBranchPRs` and `Escalations` accesses hit a
     hot-table index.
  5. `TestHotTableIndexes_OnDiskFreshAndRerunIdempotent` — boots
     `InitHolocronDSN` against an on-disk DB in `t.TempDir()`,
     snapshots all indexes + tables, reboots on the same DSN, and
     asserts set-equality. Catches any non-idempotent migration.

**Stats.**

- 16 new indexes created in schema.go / schema.sql.
- 2 schema-drift columns added to createSchema.
- 1 AddRepo UPSERT refactor.
- 1 TaskNotes ON DELETE CASCADE + table-rebuild migration.
- 1 PRAGMA foreign_keys=ON enablement.
- 5 new tests (7 sub-cases counting the index map iteration).
- 4 previously-skipped audit tests flipped Red → Green.

**Watch for.**

- SQLite doesn't support ALTER TABLE to change FK clauses, so the
  TaskNotes ON DELETE CASCADE migration uses a DROP+RENAME path. The
  rebuild is gated on `sqlite_master.sql NOT LIKE '%ON DELETE CASCADE%'`,
  so it fires once per old DB and then becomes a no-op. A future
  refactor of the TaskNotes CREATE statement must keep the exact
  `ON DELETE CASCADE` tokens in the sqlite_master SQL or the gate will
  misfire. The `TestHotTableIndexes_OnDiskFreshAndRerunIdempotent` test
  will catch any regression here.
- The AddRepo UPSERT no longer preserves `remote_url`, `default_branch`,
  `pr_template_path`, `pr_flow_enabled`, `quarantined_at`, or
  `quarantine_reason` via the previous SELECT-then-reinsert dance —
  instead they're simply never updated (the ON CONFLICT clause only
  updates `local_path` and `description`). This is the correct
  semantics: those fields are owned by Layer B backfill,
  FindPRTemplate, QuarantineRepo, etc. — AddRepo should not reach into
  them. Verified by the new `AUDIT-081` behavioural check:
  QuarantineRepo, then AddRepo, then assert `quarantined_at` is still set.
- Enabling FK enforcement could surface latent cascade-breakage anywhere
  a DELETE touches a table with a REFERENCES clause. TaskNotes is the
  only such clause in the store today (audit found exactly one
  `REFERENCES BountyBoard(id)` pattern repo-wide). Future tables that
  add a REFERENCES clause MUST also include an explicit ON DELETE
  policy — SQLite's default is NO ACTION, which under FK enforcement
  turns maintenance DELETEs into errors.
- The 10k-row latency bound (<50ms) is a ceiling for in-memory SQLite
  on developer hardware. CI machines vary; if the test starts going
  yellow the right move is usually to increase the bound rather than
  chase an optimisation — SQLite's query planner changes between
  minor releases.
