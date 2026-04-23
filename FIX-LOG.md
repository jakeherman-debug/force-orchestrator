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
