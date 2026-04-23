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

## Fix #7 — Tighten ConvoyReview

**AUDIT IDs closed:** AUDIT-006, AUDIT-007, AUDIT-029, AUDIT-031, AUDIT-032,
AUDIT-111, AUDIT-113, AUDIT-117, AUDIT-120, AUDIT-135, AUDIT-136,
AUDIT-138, AUDIT-161, AUDIT-162

**Branch:** `fix/convoy-review-tightening`

**What broke.** ConvoyReview was the headline cost vector observed during
the $300 burn. A single convoy could legitimately burn $50-$100 because
of four independent loops:

1. `convoy_review_max_findings` defaulted to 5. Combined with the
   5-pass loop cap, that's 25 Astromech sessions per convoy (each a
   full 45-min Claude run).
2. Second LLM parse failure marked the task Completed — the 5-min
   `convoy-review-watch` dog immediately requeued with no memory that
   the last run was a parse failure. Up to 5 × ~$5 = ~$25 burned on a
   convoy whose LLM simply couldn't emit valid JSON.
3. The fleet had no dedup between passes. Pass 1 could flag 3
   findings, spawn 3 fix tasks, those fix tasks didn't resolve the
   issues, pass 2 flagged the same 3 findings and spawned 3 more —
   stacking non-resolving fix tasks on top of non-resolving fix tasks.
4. A clean pass gave zero protection against a later pass surfacing
   "new" findings — either from diff drift or an inconsistent LLM
   re-read. Fix tasks would spawn against findings the convoy had
   already been signed off on as delivered.

The same pattern-family showed up in sibling code paths: Council JSON
parse failures rode the shared MaxInfraFailures=5 budget (AUDIT-029);
PRReviewTriage's per-thread depth cap was bypassable by a bot that
opened a new thread per iteration (AUDIT-117); PRReviewComments had no
`classify_attempts` counter so transient classifier errors re-ran
forever (AUDIT-032); and Medic's Flaky→RealBug promotion path allowed
concurrent fix-task spawns on the same sub-PR when two CI failures
arrived in quick succession (AUDIT-120).

**What shipped.**

1. **Max findings default 5 → 2.** `convoyReviewDefaultMaxFindings` in
   `internal/agents/convoy_review.go`. Operator override via
   `SystemConfig.convoy_review_max_findings` still honoured.
2. **Parse-failure memory with escalation.** New column
   `BountyBoard.parse_failure_count` (createSchema + additive migration
   in `internal/store/schema.go`; reflected in `schema/schema.sql`). On
   LLM parse error, `incrementParseFailureCount` bumps the counter;
   after `convoyReviewParseFailureCap` (=2) attempts, `FailBounty` +
   `CreateEscalation` fire and the operator is mailed. Dog no longer
   requeues a dead-parse convoy.
3. **Pass-to-pass fingerprint dedup.** Stable `findingFingerprint`
   (SHA256 of repo+file+line+type+normalised-description). Set
   fingerprint is persisted to `BountyBoard.last_findings_fingerprint`
   on the Completed ConvoyReview row. On the next pass,
   `lastCompletedFindingsFingerprint` retrieves it — if equal to the
   current pass's fingerprint, we escalate as `conflicted_loop`
   (findings unchanged after fix tasks ran = they didn't resolve).
4. **Clean-pass gate.** `convoyReviewCleanMarker` sentinel stamps rows
   whose LLM returned `status="clean"`. `hasPriorCleanPass` checks the
   sentinel; once any prior pass is clean, subsequent passes that
   surface new findings escalate (severity=Medium) instead of spawning
   more fix tasks.
5. **Council dedicated parse budget** (AUDIT-029). New
   `councilParseFailureCap` (=3) in `jedi_council.go` using the same
   `parse_failure_count` column. Parse failures past the cap rewrap as
   "Council unable to parse LLM output" and route to operator via
   CreateEscalation instead of eating another Council pass
   (`~$0.25-$0.50` per retry).
6. **PRReviewTriage hard thread-depth + convoy cap** (AUDIT-031 /
   AUDIT-117). `dispatchPRReviewDecision` hard-forces
   `classification=conflicted_loop` when either
   `c.ThreadDepth >= depth_cap` OR
   `store.CountInScopeFixesForConvoy(convoyID) >= pr_review_convoy_fix_cap`
   (default 10). The per-thread cap was advisory (prompt text only);
   a thread-hopping bot would reset the counter each iteration.
7. **PRReviewComments classify_attempts** (AUDIT-032). New column
   `PRReviewComments.classify_attempts`. On classifier transient
   failure, `IncrementClassifyAttempts` bumps the counter; past
   `classifyAttemptsCap` (=3) the row is marked
   `classification='human'` with a "classifier gave up" reason for
   operator triage.
8. **Medic RealBug concurrency + lifetime gate** (AUDIT-120). New
   column `AskBranchPRs.spawned_fix_count`. `applyCITriageRealBug`
   now checks `store.HasOpenFixTaskForPR` (1 in-flight fix task per
   branch) and `pr.SpawnedFixCount >= medicRetriggerCap` (3 lifetime
   spawns per PR). Second failure while prior fix still running no
   longer races a second Astromech on the branch.
9. **Medic breaker-open short-circuit** (AUDIT-161). `runMedicCITriage`
   now checks `IsCIBreakerOpen` BEFORE invoking Claude. When the
   breaker is open, the triage routes straight to the Environmental
   path (no LLM call) and waits for cooldown. Previously an open
   breaker still burned a Medic Claude call every triage.
10. **Test infrastructure upgrades** (AUDIT-111 / AUDIT-135).
    `withStubCLIRunner` now returns a `*stubCLIRunner` with an atomic
    `CallCount()` method and a `Prompts()` / `LastPrompt()` capture
    window. `stubConvoyReviewLLM` returns the runner so callers can
    assert both bounded Claude invocations and structural prompt shape
    (via `assertConvoyReviewPromptShape`). New helper
    `withStubCLIRunnerFn` for adversarial tests that need a per-call
    response dispatcher.

**How it was proved.**

- **`convoy_review_fix7_test.go`** — 10 new tests:
  - `TestRunConvoyReview_MaxFindingsDefaultIsTwo` — 8 findings in →
    2 spawned, 1 Claude call.
  - `TestRunConvoyReview_OperatorOverrideStillHonoured` — SystemConfig
    override to 4 respected.
  - `TestRunConvoyReview_ParseFailure_EscalatesAfterCap` — 2 Claude
    calls (original + critic-note retry), Failed status,
    `parse_failure_count=2`, 1 Escalation row.
  - `TestRunConvoyReview_FingerprintDedup_SpawnIsSuppressed` —
    identical pass-1/pass-2 finding set; pass 2 spawns 0 fix tasks,
    escalates.
  - `TestRunConvoyReview_DifferentFindings_NoDedup` — distinct
    findings across passes do NOT over-broaden the dedup check.
  - `TestConvoyReview_TotalClaudeCallsBounded` (AUDIT-113 closer) —
    50 dog iterations with adversarial alternating LLM responses;
    asserts `stub.CallCount() <= 12`.
  - `TestFullConvoyLifecycle_AdversarialLLM` (AUDIT-138 closer) —
    all-malformed LLM; asserts terminal state reached AND
    `CallCount() <= 4`.
  - `TestFindingFingerprint_IsStable` — fingerprint determinism,
    order-insensitivity of the set, normalisation of
    whitespace/case, line-number sensitivity.
  - `TestRunConvoyReview_AfterCleanPass_NewFindingsEscalate` — clean
    pass 1, new findings in pass 2 → escalate (not spawn).
  - `TestStubConvoyReviewLLM_CapturesPrompt` (AUDIT-135 closer) —
    prompt capture + structural-marker assertion.
- **`TestRunMedicCITriage_EnvironmentalTripsBreaker`** updated
  (AUDIT-161 closer) — post-trip assertion: after breaker opens, 3
  extra triages must NOT grow `CallCount` past the trip point.
- **`TestRunAstromechTask_RateLimit`** updated (AUDIT-162 closer) —
  asserts `CallCount() == 1` on the rate-limit path.
- **`audit_cost_loops_test.go`, `audit_cost_advisory_test.go`,
  `audit_test_quality_test.go`, `audit_medium_spotcheck_d_test.go`**
  — all 14 assigned AUDIT tests had their `t.Skip` lines removed and
  their assertions inverted so they now pass when the fix is in
  place and fail if the defect re-appears.
- **Full suite.** `go test -tags sqlite_fts5 -timeout 300s -count=1
  ./...` — green across every package, 217 s total.

**Stats.**

- 3 new schema columns (`BountyBoard.parse_failure_count`,
  `BountyBoard.last_findings_fingerprint`,
  `AskBranchPRs.spawned_fix_count`,
  `PRReviewComments.classify_attempts`). Each added to both
  `createSchema` and `runMigrations` (idempotent ALTER) plus
  `schema/schema.sql` for the reference copy.
- 1 new source file: `internal/agents/convoy_review_fix7_test.go`
  (10 tests, ~520 lines).
- 3 new store helpers: `CountInScopeFixesForConvoy`,
  `IncrementClassifyAttempts`, `MarkClassifyUnrecoverable`,
  `IncrementSpawnedFixCount`, `HasOpenFixTaskForPR`.
- 4 new constants: `convoyReviewParseFailureCap=2`,
  `convoyReviewDefaultMaxFindings=2`, `convoyReviewCleanMarker=CLEAN`,
  `councilParseFailureCap=3`, `classifyAttemptsCap=3`.
- 14 `t.Skip("AUDIT-NNN: ...")` lines removed.

**Worst-case per-convoy cost before Fix #7:** 25 Astromech sessions
(5 passes × 5 findings) + 5 parse-fail passes × 2 retries + unbounded
Council retries = ~$150. **Worst case after Fix #7:** 2 passes ×
2 findings = 4 fix-task Astromech sessions + bounded parse-fail
escalation at 2 tries + conflicted-loop short-circuit on pass-to-pass
dedup = ~$20-30.

**Watch for.**

- **Operator override of `convoy_review_max_findings`.** The guard is
  the default; if an operator raises it to 5 for a specific fleet
  deployment, the fingerprint dedup still protects against runaway
  passes, but the single-pass cost goes back up. Document the
  trade-off in ops notes.
- **Fingerprint precision.** The current normalisation
  (whitespace/case) handles the common LLM drift but a pathological
  description rewrite ("bug A" vs "defect A") defeats dedup. If we
  see that pattern in practice, consider adding a semantic
  fingerprint that hashes on file+line only (accepting some
  false-positive matches).
- **Clean-pass sentinel collision.** If a future pass-type also
  writes an empty/default fingerprint without calling the full LLM
  pipeline, it could accidentally pass the `hasPriorCleanPass` gate
  if someone changes the query to match empty rather than the
  explicit `CLEAN` marker. The sentinel pattern is documented in
  CLAUDE.md invariant #10.
- **PRReview convoy-level cap interacting with human-flagged
  follow-ups.** The cap counts ALL `in_scope_fix` classifications.
  If an operator manually re-classifies a human comment as
  in_scope_fix later, it counts toward the cap. That's the intended
  behaviour (operator is opting-in to a fix task) but means a
  bot-heavy convoy could push a later human-originated fix into
  conflicted_loop. Acceptable trade-off — the operator always has
  the dashboard override path.
