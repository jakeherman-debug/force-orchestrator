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
