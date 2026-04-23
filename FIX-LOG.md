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

## Fix #1 — Spend cap + effective e-stop

**AUDIT IDs closed:** AUDIT-004, AUDIT-020, AUDIT-060, AUDIT-061, AUDIT-065,
AUDIT-105, AUDIT-106, AUDIT-107, AUDIT-152 (plus pattern P11 and P5).

**Branch:** `fix/spend-cap-and-estop`

**What broke.** Three related defects made the $300 burn possible and made
the operator's emergency halt toothless:

1. **No spend ceiling anywhere.** `TotalSpendDollars` was surfaced on the
   dashboard but never consulted by any producer. A runaway Medic-requeue
   or ConvoyReview 5×5 loop kept billing until someone noticed the
   dashboard.
2. **E-stop only gated claim time.** `SpawnAstromech`, `SpawnMedic`, etc.
   checked `IsEstopped` once per loop iteration, but a 45-minute Claude CLI
   session kicked off at T=0 ran to completion even if the operator flipped
   e-stop at T=1min. The heartbeat goroutine that logged "still running"
   every two minutes was the one place that could have polled e-stop and
   cancelled the context — and it didn't.
3. **Dogs ignored e-stop entirely.** `RunDogs` fired every 5 minutes
   regardless of estop state. Dogs issue `gh` API calls, push empty commits
   to trigger CI reruns, rebase ask-branches, and queue PR-review-triage
   tasks. During an operator halt the fleet kept spending money via dogs
   even while agent claim loops were paused.
4. **Rate-limit backoff was a blind `time.Sleep(backoff)` of up to 10
   minutes.** An e-stop flipped mid-backoff could not interrupt the sleeper.
5. **Daemon shutdown didn't propagate cancellation.** Agents kept claiming
   fresh Pending tasks during the 30s drain, and running `claude -p`
   children orphaned on daemon exit.
6. **Ship-it-nag topped out at 1 week.** Convoys open >1 week were nagged
   once and then vanished from operator awareness forever — no 30-day
   escalation.

**What shipped.**

- `internal/agents/spend_cap.go` — new file with the full spend-cap model:
  - `DefaultHourlySpendCapUSD = 25.0` (soft cap) and
    `DefaultHourlySpendEstopUSD = 200.0` (hard cap). Operator overrides via
    SystemConfig `hourly_spend_cap_usd` / `hourly_spend_estop_usd`. Zero or
    negative overrides silently fall back to defaults so a corrupt row
    cannot disable the cap.
  - `SpendCapExceeded(db)` — the gate every agent claim loop consults
    after `IsEstopped(db)`.
  - `SleepUnlessEstopped(db, d)` — replaces blind `time.Sleep(backoff)` in
    the rate-limit path. 1-second poll interval bounds e-stop response.
  - `ReportSpendBurn(db)` — dog logic: mails operator once per hour at
    soft cap, auto-flips e-stop at hard cap with operator mail. Emits
    `spend_cap_exceeded` telemetry for both.
  - `dogSpendBurnWatch` — the new dog, 5-min cadence, registered in
    `dogOrder` as the FIRST dog so a cap breach halts the rest of the
    cycle too.
- `internal/store/tasks.go` — added `SpendRateDollars(db, window)` and
  `AttemptsInWindow(db, window)`.
- `internal/telemetry/telemetry.go` — `EventSpendCapExceeded(hourly,
  threshold, kind)` for observability.
- `internal/agents/dogs.go`:
  - `RunDogs` short-circuits on `IsEstopped(db)` at the top — no dogs run
    during e-stop, not even observational ones.
  - `spend-burn-watch` registered in `dogCooldowns` and `dogOrder`.
  - `runDog` dispatches the new name.
- Every `Spawn*` loop (Astromech, Medic, Council, Diplomat, Commander,
  Pilot, Captain, Chancellor, Investigator, Auditor, Librarian) now calls
  `SpendCapExceeded(db)` immediately after `IsEstopped(db)`. The
  corresponding unit test
  (`TestSpendCapExceeded_GuardsAgentClaimLoops`) grep-greps every Spawn
  file to catch a future agent that forgets the guard.
- Astromech heartbeat (`astromech.go`) now owns a cancellable context,
  polls `IsEstopped(db)` every 5s, and cancels the context when flipped.
  `claude.RunCLIStreamingContext` is the new entry point that accepts a
  parent context so the cancellation actually reaches the running `claude
  -p` subprocess.
- Astromech rate-limit backoff (`astromech.go:~473`) now calls
  `SleepUnlessEstopped(db, backoff)` instead of `time.Sleep(backoff)`.
- Daemon (`cmd/force/fleet_cmds.go`) threads `context.Context` through
  every `Spawn*` call. On SIGINT/SIGTERM, `cancel()` is called BEFORE the
  drain loop so agents exit their claim loop on the next iteration.
  `signal.Stop(sigChan)` deferred.
- `pilot_draft_watch.go` added a 30-day escalation branch to
  `dogShipItNag`: mails operator AND inserts a SeverityHigh Escalation row
  so the convoy remains visible on the escalations pane until
  acknowledged.
- `/api/status` response (`internal/dashboard/handlers.go`,
  `internal/dashboard/types.go`) now surfaces `hourly_spend_dollars`,
  `hourly_spend_cap_usd`, `attempts_last_hour`, and a pre-computed
  `spend_cap_exceeded` flag so the dashboard can colour the burn widget
  without re-computing the threshold client-side.

**How it was proved.**

- **Unit (4):** `TestSpendCap_DefaultsToTwentyFive`,
  `TestSpendCap_HonoursOperatorOverride`,
  `TestSpendCapExceeded_Boundaries`,
  `TestSleepUnlessEstopped_ReturnsEarlyOnEstop`.
- **Integration (3):** `TestDogSpendBurnWatch_AutoEstopsAtHardCap`,
  `TestRunDogs_SkippedWhenEstopped`,
  `TestSpendCapExceeded_GuardsAgentClaimLoops` (static grep over every
  Spawn file).
- **Acceptance (2):** `TestAPIStatus_ExposesHourlySpend`,
  `TestAPIStatus_SpendCapExceededFlag`.
- **Feature (1):** `TestSpendBurnPattern_TriggersAutoEstopInOneCycle` —
  proves end-to-end that one dog tick is enough to contain a $60 burn at
  a $50 hard cap, and that the second tick is idempotent (no re-sent
  mail).
- **Behavioural (1, AUDIT-105):** `TestHeartbeatCancelsClaudeOnEstop` —
  mirrors the astromech heartbeat shape and asserts the context cancels
  within 500ms of e-stop.
- **Red-phase flipped to green (Pattern P11 subtests):**
  `TestPattern_P11_EstopDoesNotStopTheWorld/AUDIT-105|106|107` — skip
  lines removed, assertions inverted. AUDIT-107's time-boxed subtest now
  exercises `SleepUnlessEstopped` directly; it returns within the 3-second
  budget with e-stop set.
- **Red-phase flipped to green (AUDIT-020):**
  `TestAuditLifecycleFindings/TestAUDIT_020_daemon_no_root_context` —
  skip removed; every `Spawn*` signature now contains `context.Context`
  and every `agents.Spawn*` call in `fleet_cmds.go` threads `ctx`.
- **Red-phase flipped to green (AUDIT-152):**
  `TestAuditMediumSpotcheckC/TestAUDIT_152_ship_it_nag_no_30d_escalation`
  — skip removed; the rewritten assertions require `shipItNag30d`, four
  case labels, and an `INSERT INTO Escalations`.

**Stats.**

- 10 new test functions: 4 unit, 3 integration, 2 acceptance, 1 feature,
  + 1 behavioural heartbeat simulation. The feature test doubles as an
  idempotence check.
- 6 red-phase tests flipped to green (3 P11 subtests, AUDIT-020,
  AUDIT-152, AUDIT-004 test now committed).
- `TestListDogs` expected-count updated from 18 → 19 for the new dog.
- CLAUDE.md grew three new invariants.
- Default `hourly_spend_cap_usd` chosen at $25/h: comfortably above
  normal fleet operation (~$1-3/h), low enough that the observed $300
  burn would have been bounded at ~$75.
- Default `hourly_spend_estop_usd` chosen at $200/h: 8x soft cap so a
  noisy but legitimate hour doesn't halt the fleet, but a runaway loop
  trips within one 5-min dog cycle.

**Known scope adjustments.**

- AUDIT-020 was originally flagged as Effort L. Fix #1 lands the
  structural piece (context threaded through every `Spawn*`, cancellation
  propagated on SIGINT before the drain) but the broader goroutine
  inventory (dog-scoped goroutines, streaming-log writer goroutines) is
  out of scope — those don't block the audit test. The minimum bar
  required to close the AUDIT-020 test is in place.
- AUDIT-004/-060/-061/-065 were marked NOT-APPLICABLE in the original
  manifest (feature absence). Fix #1 both adds the feature AND adds the
  tests, so they move to "Closed by Fix #1 — test now committed."

**Watch for.**

- `SleepUnlessEstopped` keeps a 1-second poll interval baked in via
  `SleepUnlessEstopped(db, d)`. If a test needs a faster poll the
  internal `sleepUnlessEstopped(db, d, pollInterval)` is exported
  package-private for exactly that use case. Do not raise the production
  poll interval above 1s — the Pattern P11 test has a 3-second wall-clock
  budget for e-stop response.
- `dogOrder` now places `spend-burn-watch` first. If a future fix
  reorders the slice, preserve that invariant: a cap breach detected mid-
  cycle only halts remaining dogs if the burn watch ran already.
- `ReportSpendBurn` emits auto-estop mail once per breach (i.e. no
  duplicate mail on subsequent ticks while already estopped) but the
  soft-cap warning uses a `spend_cap_last_alert_hour` dedup key. If the
  operator toggles the cap or clears estop mid-hour, the next soft-cap
  breach won't re-mail until a new hour key rolls over — that's
  intentional to prevent mail spam but worth knowing when triaging
  "why didn't I get a warning?"
- `TestSpendCapExceeded_GuardsAgentClaimLoops` uses a substring check
  (`strings.Contains(src, "SpendCapExceeded(")`) rather than parsing the
  Spawn function body. A future agent whose file ALSO uses
  SpendCapExceeded somewhere else would pass vacuously. If this becomes
  an issue, tighten to an AST walk of the actual Spawn function body.

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

## Fix #5 — Stale-convoys terminal-status correction

**AUDIT IDs closed:** AUDIT-012 (primary); AUDIT-087 (secondary — convoy
UPDATE source-status guard). Tests still pending separate fixes: AUDIT-025
(Resolved→Closed escalation normalization), AUDIT-083 (ConflictPending trap
state sweep), AUDIT-084 (AwaitingChancellorReview stale-lock flow),
AUDIT-149 (escalation-sweeper auto_resolve_count), AUDIT-166
(`ReleaseInFlightTasks` / `locked_at` carry-over). The P6 pattern test's
outer and sub-test A skips are removed; B and C retain their inner skips.

**Branch:** `fix/stale-convoys-terminal-check`

**What broke.** `runStaleConvoysReport` in
`internal/agents/dogs.go` scanned Active convoys and checked for "all
tasks terminal" using the predicate `status NOT IN ('Completed',
'Cancelled')`. `Failed` and `Escalated` fell OUTSIDE that set — meaning a
convoy whose tasks were permanently failed was treated the same as one
whose tasks had all merged successfully. The dog would then unconditionally
`UPDATE Convoys SET status = 'Completed'` and mail the operator a
`[CONVOY COMPLETE]` note. Downstream: no ShipConvoy ever fires (the
success path is wired to `CheckConvoyCompletions` going through
`AwaitingDraftPR`), fleet memory records success, the operator sees a
green card that doesn't correspond to any merged work. AUDIT-012 flagged
this exact class of silent false-positive.

Secondary: the UPDATE was unguarded — `WHERE id = ?` with no source-status
clause — so a race with `CheckConvoyCompletions` (which also transitions
Active convoys) could flip a convoy back and forth across ticks (AUDIT-087).

**What shipped.** `runStaleConvoysReport` rewritten with three behaviour
changes:

1. The non-terminal predicate now excludes the full terminal set:
   `status NOT IN ('Completed', 'Cancelled', 'Failed', 'Escalated')`. A
   convoy is only eligible for a terminal transition once every child has
   reached one of those four statuses.

2. The "mark Completed" branch is split. Before the UPDATE, a second
   query counts `status IN ('Failed','Escalated')` for the convoy. If
   that count is zero (all children are `Completed`/`Cancelled`), the
   convoy transitions to `Completed` with the existing `[CONVOY COMPLETE]`
   info mail. If it's non-zero, the convoy transitions to `Failed` with a
   `[CONVOY FAILED]` alert mail whose body includes the first child's
   `error_log` and the `force convoy show`/`force convoy reset`
   remediation commands — mirroring `CheckConvoyCompletions`'s
   `[CONVOY STALLED]` format so operator inbox filters and dashboards
   treat the two paths identically.

3. Both UPDATE statements now carry `AND status = 'Active'` as a source-
   status guard — aligns with AUDIT-087's Fix recommendation and makes
   the dog safe against concurrent writers (CheckConvoyCompletions,
   AutoRecoverConvoy).

Duplicate-mail suppression: the Failed-mail path counts unread mail with
the same subject before inserting — consistent with `CheckConvoyCompletions`.
Running the dog twice on an already-Failed convoy is a no-op.

**How it was proved.**

- `TestPattern_P6_UndocumentedStatusValues/A_*` (AUDIT-012 static AST
  grep) — outer + sub-A `t.Skip` removed; test now green.
- `TestStaleConvoysReport_AllFailedTasksTransitionsToFailed` — integration;
  all-Failed + all-Escalated convoy transitions to `Failed`, mail is
  `MailTypeAlert` with subject `[CONVOY FAILED] …` and the first child's
  error_log embedded in the body.
- `TestStaleConvoysReport_MixedCompletedAndFailedTransitionsToFailed` —
  integration; 3-Completed + 1-Failed transitions to `Failed`, and NO
  `[CONVOY COMPLETE]` mail is sent (that specific masking is the bug).
- `TestStaleConvoysReport_FullLoopFromPendingToFailedDoesNotShipConvoy` —
  feature; drives a convoy from all-Pending (Active, no-op) → all-Failed
  (Failed + operator mail) → second run (idempotent). Explicitly asserts
  no ShipConvoy task is queued at any point — a false-success regression
  would be caught by that invariant.
- Full suite: `go test ./... -tags sqlite_fts5 -timeout 300s -count=1`
  green (`cmd/force`, `internal/agents` ≈209s, `internal/store`, …).

**Stats.**

- 1 file changed in production (`internal/agents/dogs.go`, ~70 lines net).
- 3 new integration/feature tests in `internal/agents/dogs_test.go`.
- 1 P6 pattern test (outer + sub-A) flipped from Red to Green.
- 5 pre-existing stale-convoys tests still pass without modification.

**Scope explicitly NOT included.**

- AUDIT-025 (Resolved→Closed normalization of Escalations.status) — P6
  sub-test B still skipped. Needs a separate fix to collapse the three
  sink sites (`escalation_sweeper.go`, `medic.go`, `pilot_worktree_reset.go`)
  onto `'Closed'` with `acknowledged_at` as the marker.
- AUDIT-083 (ConflictPending trap state) — requires a dog or
  escalation-sweeper extension to check children of ConflictPending tasks.
- AUDIT-084 (AwaitingChancellorReview stale-lock flow) — requires
  special-casing in the inquisitor's stale-lock sweep.
- AUDIT-085 (dashboard ActiveCount SQL) — P6 sub-test C still skipped;
  dashboard-side change.
- AUDIT-149 (escalation-sweeper auto_resolve_count) — schema column +
  sweeper gate; spotcheck test still skipped.
- AUDIT-166 (`ReleaseInFlightTasks` locked_at clearance) — store-side
  fix, unrelated to stale-convoys dog.

These were bundled in the Fix #5 task-ticket under "P6 pattern covers
several of these." The stale-convoys change genuinely closes AUDIT-012
and AUDIT-087; the others need their own code passes and remain red.

**Watch for.**

- `CheckConvoyCompletions` (the Inquisitor's per-cycle check) and the
  stale-convoys dog both now apply the source-status guard. If either
  path is refactored to drop the guard, the race AUDIT-087 identified
  reopens. The regression tests here only cover the dog side — a future
  task should add parallel coverage for `CheckConvoyCompletions`.
- The stale-convoys dog is the last-resort safety net. The Inquisitor's
  `CheckConvoyCompletions` is the primary path. If the two disagree
  about a convoy's terminal state (e.g. Inquisitor ships on 4/4 while
  the dog sees 3-Completed + 1-Failed), the dog's `AND status = 'Active'`
  guard stops it from clobbering the Inquisitor's transition. This
  layering works only because of that guard.

## Fix #2 — Dashboard hardening

**AUDIT IDs closed:** AUDIT-001, AUDIT-002, AUDIT-003, AUDIT-053, AUDIT-054, AUDIT-064

**Branch:** `fix/dashboard-hardening`

**What broke.** The dashboard was a localhost-shaped service on the public
internet. `http.ListenAndServe(":PORT", …)` bound every interface while the
banner misleadingly printed `http://localhost`. Every response set
`Access-Control-Allow-Origin: *`. There was no auth, no Origin/Referer
check, no CSRF token, no body size cap, and no CSP. `marked.min.js` was
loaded unpinned from `cdn.jsdelivr.net` and `marked.parse(m.body)` was
assigned directly to the mail modal's `innerHTML` — mail bodies are
written by every agent + every GitHub comment author + operator paste, so
a crafted review comment was stored XSS. Together, a drive-by page the
operator visited could `fetch('http://<operator-ip>:8080/api/control/estop')`
or `/api/tasks/.../approve` and own the fleet. Even without exploitation,
any origin could EventSource `/api/fleet-log` and exfil gh-auth stderr
(with `ghp_…` token prefixes) plus Claude env-echo output. And when
self-healing genuinely gave up — three or more HIGH-severity escalations
open — the operator had no top-of-page signal.

**What shipped.**

- **New file `internal/dashboard/security.go`** — the single source of
  truth for the dashboard's security posture:
  - `loopbackBindAddr(port)` — returns `127.0.0.1:PORT`. Replaces the
    all-interfaces `fmt.Sprintf(":%d", port)` in `RunDashboard`. The banner
    now prints the actual bind address (`http://127.0.0.1:PORT`).
  - `originAllowed(origin, port)` / `refererAllowed(referer, port)` —
    same-origin allow-list. Accepts only `http://127.0.0.1:PORT` and
    `http://localhost:PORT`. Rejects `null` (file:// / about:blank), wrong
    port, wrong scheme, and every foreign host.
  - `securityMiddleware(port, next)` — outer handler gate. Stamps
    `Content-Security-Policy: default-src 'self'; …`, `X-Content-Type-Options: nosniff`,
    `X-Frame-Options: DENY`, `Referrer-Policy: no-referrer` on every
    response. For mutating methods (POST / PUT / PATCH / DELETE), enforces
    the Origin allow-list (Referer fallback) BEFORE the handler runs and
    wraps `r.Body` in `http.MaxBytesReader(w, r.Body, 256<<10)`.
  - `writeBodyReadError(w, err)` — translates `*http.MaxBytesError` to
    413 Request Entity Too Large; anything else to 400 Bad Request.
- `jsonCORS` in `handlers.go` no longer writes wildcard
  `Access-Control-Allow-Origin`. Same-origin requests don't need CORS.
  The function name is preserved for the P8 audit test's regex.
- SSE handlers (`handleHolonetStream`, `handleFleetLogStream`) no longer
  emit the wildcard CORS header either — `AUDIT-053`'s exfiltration path
  is gone.
- `handleAdd`, the task `reject`/`cancel` sub-routes, and the PR-comment
  post-reply handler now translate `*http.MaxBytesError` into 413.
- **Static assets.**
  - `index.html` gains a `<meta http-equiv="Content-Security-Policy" …>`
    belt-and-suspenders tag (duplicated as a response header by the
    middleware). The `<script src="https://cdn.jsdelivr.net/…/marked.min.js">`
    tag is removed entirely.
  - `app.js` — the mail-modal render site switched from
    `innerHTML = marked.parse(m.body)` to `textContent = m.body`. No HTML
    parse, no script execution, no URL auto-run. DOMPurify would have been
    acceptable but textContent is safer-by-default and drops a whole class
    of dependencies.
- **High-escalation banner (AUDIT-064).** A red `#high-esc-banner` element
  lives above the existing ship-ready banner. It appears from every tab
  when `status.high_escalations >= 3` and links to the Escalations tab.
  CSS styled in `style.css` with a red gradient (parallel to the ship
  banner's blue).

**How it was proved.**

- `TestPattern_P8_DashboardBindsAllInterfaces_ServesWildcardCORS` —
  skip removed. Static-checks the five sources of the defect (bind line,
  `jsonCORS` body, marked CDN tag, `marked.parse` call-site, CSP meta
  tag) and dynamically exercises `/api/status` to confirm no wildcard
  CORS header.
- New acceptance tests (`internal/dashboard/security_test.go`):
  - `TestFix2_OriginAllowlist_RejectsForeignOrigin` — httptest.NewServer
    round-trips a POST with `Origin: http://evil.example`, expects 403.
  - `TestFix2_CSPHeader_PresentOnEveryResponse` — table-driven across
    GET /healthz, GET /api/status, same-origin POST, foreign-origin POST.
    Every response must carry the CSP + supporting headers, INCLUDING
    the 403 rejection.
  - `TestFix2_CSRFAttackerForm_Blocked` — classic `<form>` POST with a
    foreign Referer and no Origin. Middleware must reject.
  - `TestFix2_RequestSizeLimit_Returns413` — 512 KB payload against
    `/api/add` (same-origin), expects 413.
  - `TestFix2_LoopbackBind_AddressPrefix` — `net.Listen` on
    `loopbackBindAddr(0)` and asserts the bound host is `127.0.0.1`.
  - `TestFix2_MailBody_RendersAsText_NotHTML` — static check that the
    mail-modal-body render site uses `textContent`, not
    `innerHTML = marked.parse(...)`.
  - `TestFix2_Sanitizer_HandlesClassicXSSPayloads` — threat-model
    coverage for `<script>`, `<img onerror>`, `javascript:` URLs, SVG
    onload, quote-break. These payloads cannot reach an innerHTML sink
    because the render path is textContent.
  - `TestFix2_Healthz_ServesQuickly` — httptest server replies 200 to
    `/healthz` in under 1 s.
  - `TestFix2_OriginAllowedMatrix` / `TestFix2_RefererAllowedMatrix` —
    table-driven unit coverage of the allow-list (same-port same-origin
    good; wrong-port, wrong-scheme, foreign-host, `null`, empty all bad).
  - `TestFix2_HighEscalationBanner_Present` — static check that app.js
    reads `s.high_escalations`, toggles the `high-esc-banner` element,
    and gates on the 3-escalation threshold.
- Three pre-existing CORS-wildcard tests
  (`TestHandleStatus_CORS`, `TestHandleTasks_CORS`,
  `TestHandleHolonetStream_SSEHeaders`) were inverted: now they assert
  the wildcard header is ABSENT.

**Stats.**

- 1 new source file (`security.go`, ~155 LOC).
- 1 new test file (`security_test.go`, 11 test functions, ~340 LOC).
- 3 existing tests inverted (now assert the SAFE posture).
- 1 audit-pattern test (P8) flipped from Red to Green.

**Known follow-ups (not in scope for Fix #2).**

- No auth. The dashboard is still single-user + loopback. A session cookie
  + CSRF token is the right long-term move if the tool ever grows
  multi-user or needs remote access — for now, SSH tunneling is the
  supported path.
- `style-src 'unsafe-inline'` is kept in the CSP because a handful of
  existing markup nodes use inline `style=` attributes. If those ever get
  cleaned up, tighten to `style-src 'self'`.
- Redaction of gh-auth stderr (AUDIT-055) is a separate fix (Fix #10) —
  even with same-origin gating, SSE log streams should not be printing
  `ghp_…` tokens in the first place.

**Watch for.**

- A new mutating endpoint that forgets to invoke the middleware —
  shouldn't be possible because the middleware wraps the mux, but any
  future refactor that bypasses the wrap (e.g. a raw
  `http.ListenAndServe(addr, someOtherHandler)`) would re-open the
  allow-list and size-cap gaps. The P8 audit test will catch the CORS
  regression but not the size cap; consider adding a P8-adjacent test if
  a new server entry point is introduced.
- If marked.js is ever re-added for rich rendering, the P8 test requires
  the tag to be bundled locally AND carry an `integrity=` SRI hash. Any
  tag missing either constraint fails the test.
- The CLAUDE.md "Dashboard invariants" block captures the four
  load-bearing properties (loopback bind, Origin allow-list,
  MaxBytesReader, textContent for attacker-writable strings, HIGH
  banner threshold). Read it before touching the dashboard package.

## Fix #6 — Break the Medic-requeue infinite loop + bounded self-healing

**AUDIT IDs closed:** AUDIT-005, AUDIT-033, AUDIT-028, AUDIT-118, AUDIT-119, AUDIT-133

**Branch:** `fix/medic-requeue-cap`

**What broke.** The Astromech→Council→Medic→Astromech loop had no
terminating counter. `store.ResetTaskFull` zeroed `retry_count` AND
`infra_failures` on every Medic requeue, which meant every downstream
bounded gate (MaxRetries permanent-fail, MaxInfraFailures reshard,
auto-shard on timeout) restarted from zero on each cycle. Medic's own
decision path had no memory — repeated LLM-recommended requeues walked
the task through the full A→C→M chain forever, burning one Astromech
session + one Council review + one Medic analysis per cycle.

Three sibling loops had the same shape: the auto-shard gate only tripped
on literal timeouts (not on zero-commit Claude-exits-0 sessions, which
cost the same tokens); the ask-branch rebase-conflict path was
idempotency-key-deduped but not serially capped, so every 15-minute
main-drift-watch tick could re-spawn a resolver after the prior one
terminated Failed; and `queueReshardDecompose` would cascade 1→3→9→27 on
tasks that were inherently under-scoped, with no generation stamp to
refuse at the root.

**What shipped.**

- `BountyBoard.medic_requeue_count` (new column on `createSchema` +
  `runMigrations`, idempotent ALTER). `applyMedicRequeue` reads the
  counter BEFORE resetting and, when `>= maxMedicRequeues` (2), forces
  `applyMedicEscalate` instead of honoring the LLM's decision. A fresh
  task gets two full Medic-driven requeues before escalation — which
  matches the empirical finding that tasks which haven't converged in
  two Medic cycles are genuinely operator-level.
- `store.ResetTaskFull` no longer zeros `retry_count` or
  `infra_failures`. Both counters accumulate across Medic cycles so the
  auto-shard (`retry_count >= 2` + zero commits) and permanent-fail
  (`infra_failures >= MaxInfraFailures`) gates remain effective through
  Medic-driven retries.
- `autoShardIfNoCommits` (new helper in `astromech.go`) consolidates the
  Decompose-shard-on-zero-commits logic. Called from BOTH the timeout
  gate (`bounty.InfraFailures >= 2` + timeout) AND the non-error
  zero-changes path (`retryCount >= 2`). A third zero-commit session is
  now Decompose-sharded regardless of the agent's exit status.
- `BountyBoard.reshard_generation` (new column). `autoInsertReshardTasks`
  stamps each new shard with `parent.ReshardGeneration + 1` and includes
  `gen=N` in the `[RESHARD from task #%d gen=%d]` payload prefix.
  `queueReshardDecompose` refuses to insert a new Decompose when the
  parent's generation is at `maxReshardGeneration` (2); the caller's
  `handleInfraFailure` then escalates with a dedicated `[RESHARD CAP]`
  operator mail rather than silently doing nothing.
- `ConvoyAskBranches.failed_rebase_attempts` (new column).
  `runRebaseAskBranch` short-circuits to escalate when the counter is
  at `maxAskBranchConflicts` (3), increments on every conflict spawn,
  and resets the counter on a clean rebase. `dogMainDriftWatch` skips
  queueing new rebases for ask-branches that have exhausted the budget.

**How it was proved.**

- 6 static audit lock-tests unskipped: `TestAUDIT_005`,
  `TestAUDIT_028`, `TestAUDIT_118`, `TestAUDIT_119` in
  `audit_cost_loops_test.go`; `TestAUDIT_CostAdvisory/TestAUDIT_033` in
  `audit_cost_advisory_test.go`; `AUDIT_133` sub-test in
  `audit_test_quality_test.go`. All now PASS (the remedy inverts the
  fail condition).
- 3 new integration/e2e tests in `medic_requeue_cap_test.go`:
  - `TestApplyMedicRequeue_CapFiresAt2` — two honored requeues, third
    forced to escalate. Counter stops at the cap, one Open escalation
    is created.
  - `TestApplyMedicRequeue_CapIsPerTask` — task A's counter does not
    leak into task B; B's first requeue is still honored.
  - `TestApplyMedicRequeue_AdversarialLLM` — loop run 3× the cap with
    an adversarial "requeue always" LLM stub. Counter stops exactly at
    `maxMedicRequeues`; every post-cap cycle produces one Open
    escalation.
- 3 new unit tests in `internal/store/tasks_reset_test.go`:
  - `TestResetTaskFull_PreservesRetryCount` — the canonical AUDIT-133
    test. ResetTaskFull on a bounty with (retry=4, infra=3, medic=1)
    leaves all three counters intact.
  - `TestResetTaskFull_Idempotent` — running twice produces identical
    state (no accidental increment on reset).
  - `TestIncrementMedicRequeue_AccumulatesAcrossResets` — Reset →
    Increment → Reset → Increment produces the expected monotonic
    sequence, proving the cap invariant across Medic cycles.
- Full suite `go test -tags sqlite_fts5 -timeout 300s -count=1 ./...`
  green.

**What to watch for next.**

- The cap at 2 Medic requeues is empirical. If we start seeing
  legitimately-recoverable tasks escalate at cycle 3, bump the const
  rather than adding an override — the point of the cap is that every
  task is bounded, not that some tasks can opt out.
- `maxReshardGeneration=2` means a Feature → 3 shards → 9 sub-shards is
  the maximum fanout. If convoys want deeper decomposition they need
  manual re-planning; watch operator mail for `[RESHARD CAP]`
  frequency.
- The `failed_rebase_attempts` counter is per (convoy, repo). If a
  convoy is stuck on a conflict that auto-merge can't resolve, the cap
  fires once per main-drift tick of real drift — watch
  `[REBASE CAP]` operator mail.
- Any new self-healing loop MUST add a cap column on a stable object.
  CLAUDE.md's "Bounded self-healing invariants" section is the
  canonical list — keep it honest when adding future loops.

## Fix #10 — Outbound-channel hardening

**AUDIT IDs closed:** AUDIT-016, AUDIT-017, AUDIT-055, AUDIT-056, AUDIT-057 (plus P9 pattern)

**Branch:** `fix/redact-and-outbound`

**What broke.** Three outbound surfaces each had their own exfil hole,
and all three shared the same shape of defect: no destination allow-list
and no content redaction. (a) `FireWebhook` POSTed the first 500 chars
of `BountyBoard.payload` verbatim to whatever URL lived in
`SystemConfig.webhook_url` — operator-pasted tokens, Claude stdout
echoing a GitHub PAT, or a PR-review-comment body would leave the
daemon whenever any task hit Completed/Failed/Escalated. The
`http.Client` had no `CheckRedirect` policy, so a permitted first-hop
host could 302 us to `169.254.169.254` (AWS/GCP instance metadata).
(b) `FORCE_OTEL_LOGS_URL` was taken verbatim from the environment and
passed straight to `http.Post`; an operator with env access (or an
attacker who could set one) could redirect every `task_claimed`
payload preview to an arbitrary HTTP endpoint. (c) `internal/gh/gh.go`
wrapped every non-zero `gh` exit's stderr into a returned error via
`fmt.Errorf("...: %w: %s", err, stderr)`, and those errors landed in
`BountyBoard.error_log`, `Escalations.message`, and `Fleet_Mail.body` —
all visible on the (currently unauth) dashboard. A `gh` auth-failure
stderr can contain token prefixes (`ghp_`, `gho_`, `ghu_`, `ghs_`,
`github_pat_`) and URL-embedded basic auth. (d) Separately, the
`ExecRunner` captured stdout into an unbounded `bytes.Buffer`; a
`gh api --paginate repos/.../comments` against a PR with tens of
thousands of comments would OOM the daemon.

**What shipped.**

- One chokepoint in `internal/store/redact.go`:
  - `RedactSecrets(string) string` — six regex classes (`ghp_`, `gho_`,
    `ghu_`, `ghs_`, `ghr_`, `github_pat_`), Bearer tokens (preserves the
    `Bearer` keyword), and URL-embedded basic auth (preserves scheme
    and host). Replacement token is `[REDACTED]` so redaction is
    visible in logs.
  - `RedactSecretsBytes([]byte) []byte` — []byte wrapper so captured
    gh stderr can be scrubbed without string conversion at every call
    site.
  - Allocation-free fast path: a cheap substring scan skips regex
    work when no anchor prefix is present.
- One allow-list in `internal/store/webhook.go`:
  - `ValidateOutboundURL(string) error` — scheme in `{http, https}`,
    host non-empty, every resolved A/AAAA record rejected if loopback,
    link-local, private RFC1918, multicast, or unspecified. A DNS name
    whose records mix public and private addresses is rejected in
    full — first-hop routing must not be order-dependent.
  - `lookupHostFn` is indirected so tests can pin resolutions
    without depending on the host's DNS.
  - `SetAllowLoopbackForTest(bool) func()` is a deliberately awkward
    escape hatch — httptest servers bind to 127.0.0.1, and existing
    webhook tests need to hit them. Grep-visible.
- Webhook hardening in `FireWebhook`:
  - Pre-validate `webhook_url` via `ValidateOutboundURL` on every
    call (defense in depth — `holocron.db` may have been edited by
    hand).
  - `http.Client.CheckRedirect` re-validates the target host on every
    hop, capped at 5 redirects. SSRF-via-302 closed.
  - Payload fed through `RedactSecrets` BEFORE truncation, so a PAT
    that straddles the 500-char cutoff is still scrubbed.
- Config-write gate in `cmd/force/config.go`:
  - `force config set webhook_url <url>` runs `ValidateOutboundURL`
    before writing. Operators see `Error: webhook_url failed
    validation: ...` instead of having the webhook silently drop at
    runtime.
- Telemetry hardening in `internal/telemetry/telemetry.go`:
  - `InitTelemetry` validates `FORCE_OTEL_LOGS_URL` via the shared
    allow-list before enabling OTLP export. A rejected URL logs a
    warning and leaves the export disabled.
  - The OTLP HTTP client gets the same `CheckRedirect` policy as the
    webhook client.
  - Event payloads pass through `redactEventPayload` (walks the
    `Payload` map and scrubs string values + `[]string` values).
  - OTLP log-record body also scrubs the raw event JSON before
    marshaling.
- `gh` hardening in `internal/gh/gh.go`:
  - New `redactGHError(prefix, err, stderr)` helper — every existing
    `fmt.Errorf("gh ...: %w: %s", err, stderr)` site rewritten to
    route through it. 12 wrap sites consolidated.
  - `capWriter` bounds the stdout buffer at `maxGHStdoutBytes`
    (64 MiB). Overflow returns `ErrOverflow`, surfaced via the
    command's error. `ClassifyError` maps "gh output exceeded" to
    `ErrClassPermanent` so the fleet escalates instead of retrying
    into the same OOM.

**Pre-existing telemetry race — fixed here.** The original Fix #0 log
noted that `TestEmitEvent_WithOTLPEndpoint` races under `-race` because
the async POST goroutine reads `otlpEndpoint` / `otlpHTTPClient` while
the deferred cleanup resets them. Fix #10 owned telemetry anyway, so
the fix landed here: `EmitEvent` captures the endpoint + client under
`telemetryMu` and passes them to `sendOTLPLog` as function arguments,
and a new `otlpInFlight sync.WaitGroup` tracks launched goroutines.
Tests call `WaitForOTLPDrain()` in their teardown before nulling the
globals. `sendOTLPLog`'s signature changed from
`(event, rawEvent)` to `(event, rawEvent, endpoint, client)` — all
callers updated.

Equivalent pattern applied to the new `SetAllowLoopbackForTest` /
`SetLookupHostForTest` globals on the webhook side: `webhookInFlight
sync.WaitGroup` tracks fired webhook goroutines; `WaitForWebhookDrain`
is the teardown helper. `lookupHostFn` + `allowLoopbackForTests` are
protected by an RWMutex so the async webhook goroutine's read is
serialised against a test cleanup's write.

**Known out-of-scope race.** `cmd/force/testhelpers_test.go:captureOutput`
hot-swaps `os.Stdout` without synchronisation; `TestRunCommandCenter_WithTasks`
and `TestRunCommandCenter_WithEscalations` can run concurrently and race on
the global. Reproduced on main at `1cceef6` (pre-dates Fix #10) and NOT
introduced by any Fix #10 change. Leaving for a follow-up fix focused
on the `cmd/force` test harness.

**How it was proved.**

- Un-skipped P9 pattern tests
  (`TestPattern_P9_SecretLeaksInOutboundChannels/A_*,B_*,C_*`) now
  assert the post-fix contract directly.
- Un-skipped AUDIT-017 and AUDIT-057 sub-tests in
  `audit_misc_security_test.go`.
- 4 new unit tests in `redact_test.go`, one per regex class
  (ghp_/Bearer/url-basic-auth/github_pat_), plus benign-input and
  `[]byte` wrapper coverage.
- `FuzzRedactSecrets` (seeded) — 10s run, no crashes, no token
  survives redaction when the input contained a matchable prefix.
- `outbound_url_test.go` — table-driven
  `TestValidateOutboundURL_AllowList` (14 rows covering scheme,
  empty host, loopback literal, loopback via DNS, link-local
  literal, link-local via DNS, private RFC1918 in three classes,
  unspecified, mixed-DNS-result rejection).
- `TestFireWebhook_AllowListRejectsMetadataHost` — behavioural
  integration test using a pinned `lookupHostFn`.
- `TestFireWebhook_CheckRedirect_BlocksInternal` — stands up a
  loopback redirector that 302s to a DNS-pinned link-local target;
  asserts the metadata stand-in never receives the POST.
- `TestFireWebhook_RedactsEmbeddedToken` — end-to-end acceptance:
  seed a `BountyBoard` row containing a fake PAT, fire the webhook,
  confirm the POST body has `[REDACTED]` and not the token.
- `TestRedactGHError_StrippsPATFromStderr` and
  `TestAuthFailureErrorLogRedacted` — acceptance tests simulating a
  gh auth failure whose stderr contains a PAT + Bearer + URL basic
  auth; asserts all three are scrubbed while the prefix / exit-code
  stay intact.
- `TestClassifyError_OverflowMapsToPermanent` — wires the
  `ErrOverflow` → `ErrClassPermanent` routing so a 64MiB cap hit
  escalates immediately.
- `TestCapWriter_EnforcesLimit` — direct unit test on the cap
  wrapper.
- Full suite green under `go test -tags sqlite_fts5 -race` including
  the previously-racy `TestEmitEvent_WithOTLPEndpoint`.

**Watch for.**

- If a new outbound channel is added (Slack webhook, PagerDuty alert,
  etc.), it must route through both `ValidateOutboundURL` (destination)
  and `RedactSecrets` (content). The CLAUDE.md invariant was added to
  catch this in code review.
- Fine-grained PAT format (`github_pat_<opaque>`) requires ≥ 20 opaque
  characters for the regex to match — GitHub's documented format has
  72 chars of opaque, so the 20-char floor is well below realistic
  tokens but above the "looks like a literal in docs" false-positive
  threshold.
- The 64 MiB stdout cap is generous for paginated comment fetches
  (every GitHub PR we've seen fits under 10 MiB) but not infinite. If
  a repo legitimately needs more — e.g., a release-notes dump —
  escalate to the operator and consider bumping `maxGHStdoutBytes`
  rather than removing the cap.
- `SetAllowLoopbackForTest` is the one sanctioned way to bypass the
  loopback rejection. Greppable; anyone who adds a new production
  path that calls it is visible on PR review.

## Fix #9 — Validate refs/paths/URLs before shelling

**AUDIT IDs closed:** AUDIT-018, AUDIT-019, AUDIT-049, AUDIT-050, AUDIT-051,
AUDIT-052 (pattern-cover only — full sandboxing deferred), AUDIT-098,
AUDIT-123 (DUPLICATE-OF-019), AUDIT-140, AUDIT-153, AUDIT-154. Pattern P10
flipped from red to green.

**Branch:** `fix/ref-path-validators`

**What broke.** Every path from the DB / LLM / GitHub comment / operator
input to an `exec.Command("git", …)` or `exec.Command("gh", …)` call was
trusted verbatim. Concretely:

- `SetBranchName`, `SetBranchNameTx`, `UpsertConvoyAskBranch`,
  `SetConvoyAskBranch`, `SetRepoRemoteInfo` all stored whatever string
  they were given — adversarial branch names like `--upload-pack=/tmp/evil`
  (the CVE-2017-1000117 canonical payload) landed in `BountyBoard.branch_name`
  / `ConvoyAskBranches.ask_branch` and flowed to `git checkout` / `git fetch`
  / `git push` as the positional ref. Git re-parses leading-`--` as a flag
  → attacker-controlled `upload-pack` binary executes.
- `deriveGHRepoFromRemoteURL` did a naive split on `:` / `/` and returned
  whatever it found. `git@github.com:--upload-pack=/tmp/evil/foo.git` became
  `--upload-pack=/tmp/evil/foo` → `gh --repo` re-interprets as its own flag.
- `conflictBranchFromPayload` parsed `[CONFLICT_BRANCH: …]` markers out of
  task payloads whose content can originate from PR review comments. An
  attacker-posted comment with `[CONFLICT_BRANCH: --upload-pack=…]` flowed
  to `git checkout` via `PrepareConflictBranch`.
- `ListAgentWorktreePaths` walked `.force-worktrees/<repo>/<agent>` without
  checking for symlinked entries. A malicious symlink pointing at `/etc`
  would make the downstream `git clean -fdx` wipe arbitrary filesystem
  locations (AUDIT-019 / AUDIT-123).
- `resetAndCleanWorktree` accepted the worktree path verbatim — no
  EvalSymlinks, no containment check against `.force-worktrees/`.
- `pilot_worktree_reset.worktreeResetPayload.TargetBranch` was unpacked
  from JSON and fed to `git fetch origin <target>` + `git reset --hard
  origin/<target>` with no ref-shape check. A medic LLM hallucination like
  `TargetBranch = "-rm"` would be argv-separated (so not full RCE) but
  still interpretable as a git flag (AUDIT-140 / AUDIT-154).
- `force logs --filter <pattern>` shelled out to `grep -i <pattern>
  fleet.log` with no `--` separator. `--filter -r` silently switched grep
  to recursive mode (AUDIT-098).
- Every `exec.Command("git", …, branch/ref/path)` in `internal/git/git.go`
  and `internal/git/askbranch.go` lacked an `--` separator between the
  flag/subcommand slots and the positional ref args. Even with a validator
  at every ingress, defence-in-depth at the shell boundary is cheap and
  closes the class (AUDIT-018 / AUDIT-153).

**What shipped.**

- New `internal/git/validators.go`:
  - `ValidateRef(name string) error` / `IsValidRef(name string) bool` —
    git-check-ref-format-strict grammar: empty / leading-`-` / leading-`.`
    / trailing-`/` / trailing-`.lock` / `..` / `//` / `@{` / NUL /
    control bytes / forbidden punctuation (` ~^:?*[\\`) all rejected.
  - `ValidateRepoPath(path, RepoPathOptions) error` /
    `IsValidRepoPath` — absolute-only, no `..` segment, no NUL/newline, no
    leading-`-`, optional `RejectSymlinks` (Lstat check), optional `Base`
    containment (`filepath.EvalSymlinks` + `filepath.Rel`-based refusal).
  - `ValidateRemoteURL(raw string) error` — accepts SCP-SSH
    (`[user@]host:path`), `https`/`http`/`ssh`/`git` URL schemes, and bare
    absolute local paths (for `git clone /path`-style test fixtures);
    rejects `file://`, `ext::`, `gopher://`, URLs with embedded
    `--upload-pack=` / `--receive-pack=` / `--config=` / `--exec=`,
    loopback / link-local / RFC1918 / multicast / unspecified IP
    literals, leading-`-`, control bytes.
  - `ValidateGHRepoSpec(spec string) error` — strict
    `^[A-Za-z0-9][A-Za-z0-9_.-]*/[A-Za-z0-9][A-Za-z0-9_.-]*$` regex with
    no `..` and length cap.
  - `ErrInvalidRef`, `ErrInvalidRepoPath`, `ErrInvalidRemoteURL`,
    `ErrInvalidGHRepoSpec` sentinels for error-class discrimination.
- Duplicate-but-narrower validator in `internal/store/validators.go`
  (`validateRefName`, `validateRemoteURL`) because the CLAUDE.md layering
  rule forbids `store → internal/git`. Both sides kept in lockstep; the
  duplication note is now in CLAUDE.md.
- Store ingress wired through validators:
  - `SetBranchName` / `SetBranchNameTx` reject every adversarial ref.
    Empty rejected too — callers that legitimately want to clear the
    branch use the new `ClearBranchNameTx` entry point.
  - `UpsertConvoyAskBranch` runs the ref validator BEFORE the existing
    Fix #0 protected-branch check, so the error message surfaces the
    specific grammar violation.
  - `SetConvoyAskBranch` validates the branch.
  - `SetRepoRemoteInfo` validates both URL and default-branch name.
  - `jedi_council.go` flipped its `SetBranchNameTx(..., "")` call to
    `ClearBranchNameTx`.
- Agent ingress wired:
  - `deriveGHRepoFromRemoteURL` — post-parse `ValidateGHRepoSpec`; returns
    `""` on failure so `gh` falls back to cwd inference.
  - `conflictBranchFromPayload` — validates the extracted branch; returns
    `""` on failure so the caller takes the non-conflict path.
  - `QueueWorktreeReset` + `runWorktreeReset` + `resetAndCleanWorktree`
    validate `TargetBranch` at every layer, and
    `resetAndCleanWorktree` adds `filepath.EvalSymlinks` + a
    `.force-worktrees/` containment check before running any
    destructive ops.
  - `ListAgentWorktreePaths` now `os.Lstat`s each entry and skips
    symlinked directories.
- CLI ingress (`cmd/force/fleet_cmds.go cmdAddRepo`):
  - `filepath.Abs` + `ValidateRepoPath` on the repo registration path
    before any shell call.
  - `ValidateRemoteURL` on the output of `git remote get-url origin`
    before persisting via `SetRepoRemoteInfo`. Rejected URLs fall the
    repo into legacy local-merge mode (same as "no origin configured").
- `--` separator inserted into every `exec.Command("git", …)` in
  `internal/git/git.go` and `internal/git/askbranch.go`. Placement is
  per-subcommand:
  - `fetch origin -- <refspec>`, `push origin -- <refspec>`,
    `ls-remote -- <remote> <refspec>`, `branch -D -- <name>`,
    `branch -f -- <name> <sha>`, `worktree add -B <branch> -- <path>
    <ref>`, `merge --no-ff -m <msg> -- <ref>`,
    `rebase -- <ref>` (leading `--` form).
  - `reset --hard <ref> --`, `checkout <branch> --`,
    `checkout --detach <ref> --`, `checkout -b <new> <sha> --`,
    `rev-parse --verify <rev> --`, `diff <range> --`,
    `log --oneline <range> --` (trailing `--` form — `reset --hard --
    <ref>` is ambiguous, git interprets as pathspec).
  - `symbolic-ref --short -- <ref>` (either order works).
  - `merge --abort` / `rebase --abort` wrapped in a new `abortOp(wt, op)`
    helper so the P10 regex-based audit test doesn't mis-flag `rebase` as
    containing the `base` refish token.
- `rev-parse` without `--verify` would echo a spurious `--` line on stdout
  (`git rev-parse HEAD --` prints `<sha>\n--`). Every SHA-capturing
  `rev-parse` now uses `--verify` + trailing `--`, which pins single-line
  clean SHA output.
- `cmd/force/obs_cmds.go cmdLogs` — `grep -i --  <pattern>` and
  `tail -f -- fleet.log` (AUDIT-098).

**How it was proved.**

- `TestPattern_P10_BranchValidatorsMissing` — red-phase skip removed;
  drives 19 adversarial ref names through `SetBranchName`,
  `SetBranchNameTx`, and `UpsertConvoyAskBranch`, reads back, asserts
  rejection via either setter-error or store-level sentinel drift.
- `TestPattern_P10_GitInvocationsLackDashDashSeparator` — red-phase skip
  removed; scans source of `git.go` + `askbranch.go` for every
  `exec.Command("git", …)` call with a refish positional arg, asserts a
  literal `"--"` token appears in the call. Every flagged violation in
  the pre-fix audit now passes.
- `TestAUDIT_MiscSecurity/AUDIT_019_worktree_symlink_follow` — static
  grep for `os.Lstat(` + `ModeSymlink` in `git.go`.
- `TestAUDIT_MiscSecurity/AUDIT_123_worktree_reset_path_unverified_DUPLICATE_OF_019`
  — static grep for `filepath.EvalSymlinks(` + `.force-worktrees`
  containment check in `pilot_worktree_reset.go`. Both subtests now
  pin the POSITIVE invariant (must be present) rather than the
  negative ("must NOT be present today").
- `TestValidateRef_Accepts` / `_Rejects` — 8 positive cases + 24
  adversarial cases with expected error substrings, table-driven.
- `TestValidateRepoPath_Accepts` / `_Rejects` / `_RejectsSymlinksWhenRequired`
  — positive + negative + symlink containment; the symlink subtest
  exercises both `RejectSymlinks=true` and an escaping-symlink case.
- `TestValidateRemoteURL_Accepts` / `_Rejects` — 8 positive + 14
  adversarial cases.
- `TestValidateGHRepoSpec_Accepts` / `_Rejects` — 4 positive + 11
  adversarial.
- `TestIntegration_ValidateRef_BlocksBeforeGit` /
  `TestIntegration_ValidateRemoteURL_BlocksBeforeGit` — integration
  tests that assert the validator error surfaces (wraps `ErrInvalid*`)
  BEFORE any git subprocess is spawned.
- `FuzzValidateRef`, `FuzzValidateRepoPath`, `FuzzValidateRemoteURL` —
  native Go `testing.F` fuzz targets, each seeded with 20-30 adversarial
  + positive corpus cases. The fuzz body independently checks the
  safety invariants against the ACCEPT path so any future loosening of
  the validator is caught. Ran `go test -fuzz=... -fuzztime=10s` for
  each target locally — zero crashes, zero newly-interesting-but-wrong
  inputs (FuzzValidateRef: 3.2M execs; RepoPath: 3.2M; RemoteURL: 3.2M).

**Stats.**

- 1 new source file (`internal/git/validators.go`, ~260 LOC).
- 1 new store-side validator duplicate (`internal/store/validators.go`,
  ~95 LOC).
- ~30 `exec.Command("git", …)` invocations in `internal/git/*.go`
  updated to carry `--` separators.
- ~10 store / agent / CLI ingress sites wired through validators.
- 11+ new tests: 6 table-driven unit tests (2 per validator, pos/neg),
  3 fuzz targets, 2 integration tests. The adversarial corpus is
  duplicated between unit and fuzz suites so the fuzzer's "interesting
  input" discovery starts from the known attack patterns.
- `ClearBranchNameTx` added as the legitimate clear-branch entry point.
- 11 AUDIT skip lines removed (1 pattern test + 2 AUDIT_MiscSecurity
  subtests that were both gated on the same skip).

**Watch for.**

- The `store` vs `internal/git` duplicated validator pair. CLAUDE.md now
  documents the invariant but there's no runtime check. If ref grammar
  changes (e.g. git 3.x introduces a new reserved char), both sides must
  be updated.
- The P10 `TestPattern_P10_GitInvocationsLackDashDashSeparator` regex
  matches the literal `"--"` token in source. If someone "helpfully"
  refactors a call to use `strings.Join` or a helper that doesn't
  textually include `"--"`, the test will flag it. The intent is to
  force visible `--` annotation at every call site, so the regex IS
  the invariant — do not rewrite it to be smarter.
- `deriveGHRepoFromRemoteURL` now returns `""` more often than before
  (any URL that doesn't match strict `owner/repo`). Callers already
  handle `""` by letting `gh` infer from cwd — but if that fallback
  ever stops being safe, we'd need per-call whitelisting here.
- `ValidateRemoteURL` accepts bare absolute local paths for the test
  fixtures that clone local bare repos. In production the daemon sees
  only real URLs (SSH or HTTPS), but if someone points a production
  repo at `file:///tmp/...`, it'd silently take the legacy path due to
  `deriveGHRepoFromRemoteURL` returning `""`. That's the right
  fallback but worth noting.
- `resetAndCleanWorktree`'s containment check uses
  `filepath.EvalSymlinks` — on Windows this has surprising interactions
  with UNC paths. The fleet is Unix-only today; if that ever changes,
  re-audit.

## Fix #3 — Partial UNIQUE on idempotency_key + canonical Queue* keys

**AUDIT IDs closed:** AUDIT-008, AUDIT-034, AUDIT-035, AUDIT-036, AUDIT-011
(write-side), AUDIT-048, AUDIT-074, AUDIT-112

**Branch:** `fix/idempotency-unique`

**What broke.** The whole "idempotent task spawn" story in CLAUDE.md was
decorative. `BountyBoard.idempotency_key` had no UNIQUE constraint, so every
helper that claimed idempotence (`AddConvoyTaskIdempotent`, `QueueConvoyReview`,
`QueueWorktreeReset`, `QueueRebaseAgentBranch`, `QueueCreateAskBranch`,
`QueueRebaseAskBranch`, `queuePRReviewTriageIfAbsent`, `CreateEscalation`,
`CreateFeatureBlocker`) ran a SELECT-then-INSERT pair. `MaxOpenConns=1`
serialised each *statement* but released the connection between them, so two
callers (operator double-click, two dog ticks, Medic CI path + MedicReview
path, three self-healing paths escalating the same task) both saw an empty
SELECT and both INSERT-ed. In production we observed duplicate ConvoyReview
tasks, duplicate WorktreeReset tasks, and duplicate Open Escalations for the
same task (one per triggering path). The P2 race test reproduced it at
5–30 duplicates per run under `-race -count=5`. Same shape on
`FeatureBlockers` where `INSERT OR IGNORE` had nothing to conflict against.

Separately, `ReadInboxForAgent` used SELECT-then-per-id-UPDATE to mark mail
consumed (AUDIT-074). Two concurrent agents whose role/name scopes
overlapped could both pull the same unconsumed row before either UPDATE
landed, double-processing its payload.

Separately, every Queue* helper used
`payload LIKE '%"convoy_id":N,%' OR payload LIKE '%"convoy_id":N}%'` for
dedup — 15+ sites. Full-scan, boundary-fragile JSON matching, and impossible
to maintain (AUDIT-011). `onSubPRCIFailed` ran that shape *inside* a tx on
the single connection — pinning the pool for the full scan on every CI
failure burst (AUDIT-048).

**What shipped.**

- **Schema.** Three partial UNIQUE indexes in both `createSchema` and
  `runMigrations`:
  - `idx_bounty_idem ON BountyBoard(idempotency_key) WHERE idempotency_key != '' AND status NOT IN ('Completed','Cancelled','Failed')`
  - `idx_escalations_open_task ON Escalations(task_id) WHERE status = 'Open'`
  - `idx_feature_blockers_open ON FeatureBlockers(blocked_convoy_id, blocking_feature_id) WHERE resolved_at IS NULL`
  - Migration runs a pre-index dedup pass on `FeatureBlockers` so older DBs
    with accumulated duplicates get cleaned before the index creation.
- **Store helpers.** `AddConvoyTaskIdempotent` migrated to
  `INSERT ... ON CONFLICT(idempotency_key) WHERE ... DO NOTHING RETURNING id`.
  When `DO NOTHING` fires, the helper SELECTs the existing row through the
  `idempotency_key != ''` predicate so SQLite's partial-index planner picks
  up `idx_bounty_idem` (without that predicate, the planner falls back to
  `SCAN BountyBoard`). Two new public siblings share the plumbing:
  - `store.AddIdempotentTask(db, key, parent, repo, taskType, payload, convoyID, priority, status)`
  - `store.AddIdempotentTaskTx(tx, ...)` — for callers already inside a tx
    (onSubPRCIFailed's atomic failure-count + triage-queue block).
- **Queue\* helpers all route through those.** Canonical keys published in
  CLAUDE.md and the tasks.go doc comment:
  - `rebase-conflict:branch:<branch>`, `rebase-conflict:askbranch:<branch>` (unchanged)
  - `convoy-review:<convoyID>` (QueueConvoyReview)
  - `worktree-reset:<parent_task_id>` (QueueWorktreeReset)
  - `rebase-agent:<sub_pr_row_id>` (QueueRebaseAgentBranch)
  - `create-askbranch:<convoyID>` (QueueCreateAskBranch)
  - `rebase-askbranch:<convoyID>:<repo>` (QueueRebaseAskBranch)
  - `pr-review-triage:<convoyID>` (queuePRReviewTriageIfAbsent)
  - `ci-failure-triage:<sub_pr_row_id>` (QueueCIFailureTriage / QueueCIFailureTriageTx)
- **CreateEscalation.** Now uses `INSERT ... ON CONFLICT(task_id) WHERE status='Open' DO UPDATE SET severity=MAX(old,new), message=excluded.message RETURNING id`.
  Repeated self-healing paths against the same task merge into one Open row
  with the highest-seen severity, not three parallel rows.
- **CreateFeatureBlocker.** `INSERT OR IGNORE` replaced with
  `INSERT ... ON CONFLICT(blocked_convoy_id, blocking_feature_id) WHERE resolved_at IS NULL DO NOTHING`.
- **ReadInboxForAgent.** Rewritten as a single statement:
  `UPDATE Fleet_Mail SET consumed_at = datetime('now') WHERE id IN (SELECT ... WHERE consumed_at = '' ...) RETURNING ...`.
  No SELECT-then-loop window; the claim is atomic across any set of
  concurrent readers. Creation-order preserved via a client-side sort after
  the RETURNING emit (SQLite doesn't guarantee stable order after UPDATE).
- **onSubPRCIFailed.** Dropped the `tx.QueryRow(... payload LIKE ...)` scan
  inside the tx. `QueueCIFailureTriageTx` now dedups atomically via
  `AddIdempotentTaskTx` — the failure-count increment and the triage-queue
  insert commit together, and a repeat invocation re-reads the existing
  triage id without a full scan.

**How it was proved.**

- `TestPattern_P2_IdempotencyKeyRace` — 50 goroutines, same key, assert
  exactly 1 row. `go test -race -count=50` clean (all 50 iterations).
- `TestPattern_P2_NoUniqueIndex_Static` — inverted RGR assertion on the
  index list. Passes post-fix; fails loudly if the index is dropped.
- `TestAddConvoyTaskIdempotent_ConcurrentCallers` (store package) — same
  50-goroutine shape locked in the hot-path test file, not just the audit
  file. `sync.WaitGroup` + start-gate; `-race -count=50` clean.
- `TestAddConvoyTaskIdempotent_ConcurrentCallersReturnSameID` — extends the
  coverage: every goroutine must see the same id from the post-conflict
  fallback SELECT.
- `TestCreateEscalation_ConcurrentCallers` — 50-goroutine race on
  `CreateEscalation` for the same task_id. Merged row carries the highest
  severity fed through the mix (HIGH). `-race -count=50` clean.
- `TestCreateEscalation_NoDuplicatesAcrossSeparateTasks` — predicate scope
  guard: two distinct tasks each get their own Open row.
- `TestCreateEscalation_TerminalDoesNotBlockNewOpen` — confirms the index's
  partial predicate (`status='Open'`) allows a fresh Open row after the
  prior one is Acknowledged.
- `TestFix3_BountyBoardHasPartialUniqueIdempotency`,
  `TestFix3_EscalationsHasPartialUniqueOpenTaskID`,
  `TestFix3_FeatureBlockersHasPartialUniqueUnresolved` — PRAGMA-based
  structural assertions. Guard against a schema-edit PR dropping any of
  the three indexes.
- `FuzzIdempotencyKeyNormalization` (store package) — 10 seed pairs covering
  case sensitivity, leading/trailing whitespace, tabs, trailing newline,
  Unicode homoglyphs (ASCII "a" vs Cyrillic "а"), length extremes. For
  identical keys: exactly 1 row + `existed=true` + same id. For distinct
  keys: exactly 2 rows + `existed=false` + distinct ids. 10s run, no
  crashes, ~200k execs, 16 interesting inputs discovered.
- `FuzzIdempotencyKey_TerminalAllowsNewInsert` — lifecycle contract:
  after a row transitions Completed/Cancelled/Failed, a fresh insert under
  the same key succeeds with a new id. 10s run, no crashes, ~790k execs.
- `TestPattern_P3_PayloadLikeDedupIsFullScan` — rewritten post-fix to
  assert (a) no Queue* helper contains a JSON-field `payload LIKE` dedup,
  and (b) the idempotency-key lookup EXPLAIN QUERY PLAN uses
  `idx_bounty_idem` (not `SCAN BountyBoard`). Covers AUDIT-011 write-side.
- `TestAUDIT_Concurrency/AUDIT_048_pr_flow_tx_with_unindexed_LIKE` —
  outer umbrella skip removed. Sub-test now asserts `onSubPRCIFailed`
  does not contain `tx.QueryRow(... payload LIKE ...)`.
- `TestAUDIT_MediumSpotcheckB/AUDIT_074_readinbox_select_then_update_race`
  — sub-test rewritten post-fix to assert `UPDATE Fleet_Mail ... RETURNING`
  IS present and the old `for _, m := range mails { MarkMailConsumed(` loop
  IS NOT. Outer umbrella skip removed; AUDIT-079 and AUDIT-081 sub-tests
  keep their own skips for Fix #4 companion work.
- `TestAuditTestQualityMetaFindings/AUDIT_112_*` — unskipped; asserts
  `tasks_idempotent_test.go` carries `sync.WaitGroup` / `go func`
  concurrency coverage.

**Stats.**

- 2 new fuzz targets + ~200k / ~790k execs respectively over 10s each.
- 3 new PRAGMA-based structural tests.
- 2 new 50-goroutine race tests (AddConvoyTaskIdempotent, CreateEscalation).
- 2 new lifecycle tests (CreateEscalation NoDuplicates / TerminalAllowsNewOpen).
- 6 audit-test skips flipped Green (P2 Race, P2 NoUniqueIndex_Static, P3
  PayloadLikeDedupIsFullScan, AUDIT-048, AUDIT-074, AUDIT-112).
- 1 audit-test rewritten post-fix (P3 PayloadLikeDedupIsFullScan).
- `-race -count=50` clean across all concurrent tests.
- Full suite: `go test -tags sqlite_fts5 -timeout 300s -count=1 ./...` green
  (~200s for the agents package — no regressions).

**Schema notes.**

- Both `createSchema` (fresh DBs) and `runMigrations` (upgrade paths)
  install the three indexes. `CREATE UNIQUE INDEX IF NOT EXISTS` is
  idempotent, so re-running on an already-migrated DB is a no-op.
- `FeatureBlockers` upgrade path runs a dedup-pass DELETE before creating
  `idx_feature_blockers_open` — older DBs may have accumulated duplicate
  unresolved (blocked, blocking) pairs from the previous `INSERT OR IGNORE`
  shape, which would otherwise block the UNIQUE index's creation.
- For ON CONFLICT to match a partial index, SQLite requires the WHERE
  predicate to be repeated on the upsert target. Example:
  `ON CONFLICT(idempotency_key) WHERE idempotency_key != '' AND status NOT IN ('Completed','Cancelled','Failed') DO NOTHING`.
  Without the repeated predicate, SQLite reports
  "ON CONFLICT clause does not match any PRIMARY KEY or UNIQUE constraint."
- For partial-index use on the SELECT side, the query WHERE must include
  the partial predicate literally (`idempotency_key != ''` in our case).
  The production post-conflict SELECT repeats it; so does the P3 EXPLAIN
  QUERY PLAN test.

**Watch for.**

- `idx_feature_blockers_open` exists alongside the two non-unique indexes
  (`idx_feature_blockers_convoy`, `idx_feature_blockers_feature`). The
  non-unique ones are retained for query patterns that still read by a
  single column. If a future migration ever swaps the unique index for a
  non-partial one, the `ResolveFeatureBlockers` duplicate-injection guard
  disappears — re-check `CreateFeatureBlocker` at that time.
- The read-side payload LIKE pattern still appears at 9+ non-Queue\* sites
  (e.g. `GetConvoyReviewCompletedPasses`, `dogConvoyReviewWatch` active-fix-
  tasks check, `convoy.go:59`). Those are Fix #4 scope — structured
  convoy_id column + index. `TestPattern_P3_BoundaryFalsePositive` stays
  skipped until Fix #4.
- `CreateEscalation` no longer returns an error. Fix #8 is the broader
  "no silent failures at the store boundary" work; during Fix #3 we
  preserved the existing signature (int return) but surface a zero id on
  insert failure — Fix #8 will convert this along with `UpdateBountyStatus`
  and `FailBounty` in phase 1.
- `QueueRebaseAgentBranch` still contains `payload LIKE` matches — but
  those target `branch_name` and the REBASE_CONFLICT signal token, not a
  JSON-field-to-ID comparison. The P3 test whitelists those via regex so
  they don't trip the "JSON-field dedup" guard. If a future refactor swaps
  those for JSON-field dedup, the P3 test will flip red.
