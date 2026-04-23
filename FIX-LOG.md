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
