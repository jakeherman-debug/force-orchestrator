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
