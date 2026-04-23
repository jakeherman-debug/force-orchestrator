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

## Fix #8a — No silent failures: three terminator signatures return error (Phase A of three)

**AUDIT IDs closed:** AUDIT-013, AUDIT-014, AUDIT-022, AUDIT-041, plus the P1
pattern row in the manifest.

**Branch:** `fix/error-signatures-phase-a`

**Scope.** This is the FIRST of three planned phases for Fix #8. Phase A
establishes the signatures; Phase 8b walks per-package converting `_ =
fn(...)` TODO markers into real error handling; Phase 8c finishes the
long-tail void-returning store mutators called out in AUDIT-070 et al.

**What broke.** CLAUDE.md's headline "No silent failures" invariant was
honored in prose and violated at ~200 call sites. The root cause was
structural: three store-boundary terminators had no error return, so every
caller was forced to drop the failure on the floor.

- `store.UpdateBountyStatus(db, id, status)` — void. A failed UPDATE (wrong
  id, `SQLITE_BUSY`, locked row) left the task at its prior status while
  the webhook fired unconditionally. The stale-lock resetter would pick it
  up 45 min later and re-run the same path.
- `store.FailBounty(db, id, reason)` — void. Same blast radius: a task the
  fleet believed had failed might still be `Pending` in the DB.
- `agents.CreateEscalation(...)` — returned bare `int`. A failed
  `INSERT INTO Escalations` produced zero id, the caller marked the task
  `Escalated` anyway, and the row never appeared in the operator inbox.
  Task permanently out of the scheduler, no sweeper to sweep.

Plus two one-liners from the same pattern:

- `medic.go:120` — `json.Unmarshal([]byte(bounty.Payload), &mp)` dropped
  its error. Malformed Medic payloads produced a zero-valued `mp` and the
  LLM hallucinated a verdict (usually "shard") against empty context.
  (AUDIT-013)
- `pilot_worktree_reset.go:121-129` — both the parent-requeue UPDATE and
  the escalation-resolve UPDATE used `_, _ = db.Exec(...)`. A failed
  requeue left the parent stuck `Failed`/`Escalated` while the
  WorktreeReset still reported success. (AUDIT-014)

**What shipped.**

- `store.UpdateBountyStatus(db *sql.DB, id int, newStatus string) error`
  — wraps the UPDATE error with id/status context; webhook only fires on
  success. (`internal/store/tasks.go:184-202`)
- `store.FailBounty(db *sql.DB, id int, errorMsg string) error` — same
  pattern. (`internal/store/tasks.go:270-285`)
- `agents.CreateEscalation(...) (int, error)` — both the INSERT and the
  downstream `store.UpdateBountyStatus` errors are observable. When the
  INSERT fails, callers fall back to `FailBounty` + operator mail so the
  task ends up in a state the operator can see. (`internal/agents/escalation.go:31-54`)
- Hot-path callers updated: `jedi_council.go`, `medic.go`, `medic_ci.go`,
  `diplomat.go`, and `pilot_worktree_reset.go`. Each checks the error and
  either propagates, logs a recovery hint ("stale-lock detector will
  recover"), or falls back to a secondary self-heal (FailBounty after
  CreateEscalation fails; operator mail after a post-merge status update
  fails).
- `medic.go` `runMedicTask` — `json.Unmarshal` on bounty.Payload now
  guarded by `if err :=`; on parse failure it calls `store.FailBounty` and
  returns before any LLM call. Matches the pattern `runMedicCITriage`
  already used.
- `pilot_worktree_reset.go` — both `_, _ = db.Exec(...)` sites replaced
  with `if _, err := db.Exec(...); err != nil { store.FailBounty(...) }`.
  On either failure the WorktreeReset itself fails so Medic re-examines.
- **Non-hot-path annotations.** Every remaining statement-form call in
  captain/chancellor/commander/pilot/astromech/auditor/librarian/
  pr_review_triage/pilot_askbranch/pilot_rebase*/pilot_repo_config/
  investigator/inquisitor/convoy_review/util (agents) plus dashboard
  handlers and the `force task` CLI commands was converted to an explicit
  `_ = fn(...) // TODO(Fix #8b): propagate error` form. 108 markers
  total — Phase 8b's per-package sweep has an exact grep-able worklist.
  The hot-path callers do NOT use these markers; they propagate or
  fall back per the policy above.

**How it was proved.**

- `TestPattern_P1_UpdateBountyStatusSwallowsDBError` (unskipped, re-
  written to assert the green contract) — reflects on
  `UpdateBountyStatus` and asserts it returns `error`, then induces a
  guaranteed UPDATE failure (DROP TABLE BountyBoard) and asserts the
  caller receives a non-nil error.
- `TestAUDIT_013_MedicPayloadJSONSwallow` — greps `medic.go` for the
  `json.Unmarshal(...&mp)` call and asserts a preceding `if err :=` guard.
- `TestAUDIT_014_WorktreeResetParentRequeueSilent` — counts
  `_, _ = db.Exec(` occurrences in `pilot_worktree_reset.go` and fails
  if both parent-requeue and escalation-resolve sites still have them.
- `TestAUDIT_041_CreateEscalationNoErrorReturn` — unskipped; asserts the
  old bare-`int` signature + silent insert patterns are absent.
- `internal/store/fix8a_error_propagation_test.go` — four new unit tests:
  UpdateBountyStatus and FailBounty each tested for (a) returns-error-on-
  DB-fault via DROP TABLE, (b) happy-path nil error + correct post-
  condition.
- `internal/agents/fix8a_error_propagation_test.go` — four new tests:
  CreateEscalation's error path + happy path (unit), the Medic escalate
  fallback to FailBounty when CreateEscalation fails (integration), and
  the Jedi-Council-style logger surfacing pattern (integration).

**Stats.**

- 3 terminator signatures changed (store + agents).
- 2 one-liner swallows fixed (AUDIT-013, AUDIT-014).
- 5 hot-path files updated: `jedi_council.go`, `medic.go`, `medic_ci.go`,
  `diplomat.go`, `pilot_worktree_reset.go`.
- 108 `// TODO(Fix #8b): propagate error` markers seeded across 19 non-
  hot-path files for Phase 8b's sweep.
- 4 Phase-A audit tests unskipped and green.
- 8 new coverage tests (4 store + 4 agents).
- Full suite: `go test -tags sqlite_fts5 -count=1 ./...` green.

**What remains for Phase 8b / 8c.**

- **Phase 8b (per-package error propagation).** Each of the 19 files
  carrying `TODO(Fix #8b)` markers gets a focused sweep. Prefer
  propagating the error up the call stack (the caller is usually a
  `run<Agent>Task` function that already returns nothing — switch it to
  return error and have the claim-loop log/escalate on non-nil). When
  propagation isn't possible, wrap in `if err := ...; err != nil {
  logger.Printf(...) }` with a clear recovery hint matching the hot-path
  style. Grep `TODO(Fix #8b)` for the worklist.
- **Phase 8b (audit tests remaining).** These test skips still carry
  `AUDIT-NNN:` markers and are closed by per-package 8b sweeps, not by
  8a: AUDIT-015, -040, -042, -043, -044, -045, -046, -047, -125, -126,
  -127, -129, -130, -131, -132, -137, -151, -155 and the Medium-spot
  siblings that cite specific sites.
- **Phase 8c (long-tail void-return store mutators).** `AUDIT-070` and
  its family list every `_ = db.Exec(...)` and `_, _ = res.RowsAffected()`
  in the store layer that escapes the terminator list. Convert each to
  return error; callers already updated by 8b will propagate naturally.
- **Adjacent work NOT in 8a-c.** AUDIT-027 (`UpdateBountyStatusFrom`
  with source-status guard) rides along with 8b when it's time to
  harden hot-path transitions against the cancel-vs-approve race.

**Watch for.**

- The TODO markers MUST NOT be silently deleted when files are edited
  for other reasons. A CI grep for `TODO(Fix #8b)` combined with a
  countdown commit-by-commit is the cleanest tracking signal.
- CreateEscalation now has two distinct failure modes to keep straight:
  (1) INSERT fails — no row is written, the task stays at its prior
  status, and the caller falls back to FailBounty (single webhook fire,
  task ends `Failed`); (2) INSERT succeeds but the subsequent
  `UpdateBountyStatus(db, taskID, "Escalated")` fails — the row IS on
  disk, the task is NOT Escalated, and the caller currently falls back
  to FailBounty which overwrites to `Failed`. Phase 8b should probably
  treat case (2) as "escalation landed, status update is a separate
  observability concern" rather than flipping to `Failed`, but the
  current behavior is strictly better than the pre-8a silent stuck
  state.
- The hot-path Jedi Council `UpdateBountyStatus(b.ID, "Completed")` after
  a successful merge now escalates via operator mail if the DB write
  fails. This is a genuinely rare (SQLITE_BUSY with `MaxOpenConns=1` is
  almost impossible) but nonzero-probability DB/git-state mismatch;
  documenting it so the operator knows the mail isn't a false positive.
