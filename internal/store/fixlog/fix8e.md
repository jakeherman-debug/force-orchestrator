## Fix #8e — Daemon-ctx threading + rows.Err() exhaustive sweep + P11 tightening

**What broke.** The independent verifier for Fix #8d (FIX-8D-VERIFICATION.md)
returned CONDITIONAL-GO on a single residual: 18 production `exec.Command-
Context` call sites fabricated their context via `context.WithTimeout(
context.Background(), T)` instead of accepting a caller-supplied ctx. The
CLAUDE.md invariant the same campaign wrote said *"so daemon shutdown /
e-stop can cancel them"* — but a `git fetch` kicked off 100ms after e-stop
would run for its full fabricated 5-minute deadline regardless of daemon
cancellation. Additionally, Pattern P11's assertion was ratio-based
(`total <= totalCtx*2`) — half the sites could regress and still pass.
Three allowlist entries labeled network ops (`git push` / `git fetch` /
`ls-remote`) as "short" lookups. A secondary residual: CLAUDE.md promised
`rows.Err()` after every iteration, but Track C only swept `rows.Scan`;
4/5 sampled loops omitted the Err check.

**What shipped.** Fix #8e closes the gap exhaustively across five tracks:

- **Track A — internal/git/ helpers.** `bestEffortRun(ctx, …)`, `runGitCtx(
  ctx, …)`, `runGitCtxOutput(ctx, …)`, `abortOp(ctx, …)`, plus every public
  function (GetDefaultBranch, GetOrCreateAgentWorktree, PrepareAgentBranch,
  RunCmd, MergeAndCleanup, GetDiff, CommitsAhead, FetchMain, RemoteHeadSHA,
  CreateAskBranch, DeleteAskBranch, RebaseBranchOnto, MergeWithUnion-
  Strategy, ForcePushBranch, TriggerCIRerun, AssertNotDefaultBranch,
  PrepareConflictBranch) take ctx as the first parameter. Every internal
  `WithTimeout` wraps the PASSED ctx, not Background.
- **Track B — internal/agents/astromech helpers.** `runShortGit(ctx, …)`,
  `combinedShortGit(ctx, …)`, `combinedShortGitArgs(ctx, …)` accept ctx;
  the inline claude session ctx in `RunTaskForeground` derives from the
  CLI ctx instead of Background. `SpawnAstromech` threads daemon ctx
  through `runAstromechTask`, `processAstromechOutput`, `autoShardIfNo-
  Commits`.
- **Track C — remaining inline sites.** `dogs.go:128` RunDogs per-dog 5m
  ctx now derives from the inquisitor tick ctx. `claude.go:245` — the
  CLIRunner type now takes ctx as its first parameter; AskClaudeCLI-
  Context joins AskClaudeCLI (no-ctx form explicitly commented as legacy).
  `askbranch.go:162/:343/:437` inline sites absorbed into Track A's
  helper migrations.
- **Track D — Pattern P11 tightening.** Per-site check replaces the ratio
  assertion. Two cheat shapes are rejected EVERYWHERE regardless of
  allowlist: `exec.CommandContext(context.WithTimeout(context.Back-
  ground(), …), …)` and `exec.CommandContext(context.Background(), …)`.
  Three allowlist entries with mislabeled network ops (pr_flow.go,
  pilot_worktree_reset.go, pilot_repo_config.go) MIGRATED OUT — not
  relabeled — by ctx-threading the underlying sites. `TestPattern_P11_
  FabricatedContextRejected` (fixture-driven) and `TestPattern_P11_
  AllowlistReasonsTruthful` (descriptor-presence) pin the new contract.
- **Track E — rows.Err() exhaustive sweep.** `TestPattern_P1_1_Rows-
  ErrCheckedAfterIteration` walks every production *.go file, finds
  every `for <iter>.Next()` loop (any iterator name, not just `rows`),
  and asserts `<iter>.Err()` is referenced within 60 lines of the loop
  close. No allowlist. `_ = iter.Err()` silent discard is rejected.
  88 loops patched across 32 files to add logged rows.Err() blocks;
  104/104 production loops now covered. CLAUDE.md wording tightened
  from "is checked" to "MUST also be checked after the iteration" —
  no downgrade.

**AUDIT IDs closed.** 127/158/165 remain closed (Fix #8d) and the
CLAUDE.md invariant the same campaign wrote is now *enforced* in
production. No new AUDIT IDs assigned — this is a pure CONDITIONAL-to-GO
closure of Fix #8d's verifier residual.

**Branches/commits:** `2de29ea` (Tracks A/B/C/D/E main), `7ba5466`
(follow-up: claude.go CLIRunner ctx, Auditor+Investigator ctx, closure
report).

**How it was proved.**
- `grep -rnE 'context\.WithTimeout\(context\.Background\(\)' internal/ cmd/
  | grep -v '_test.go'` → zero hits.
- `TestPattern_P11_ExecCommandsUseContext` — per-site pass.
- `TestPattern_P11_FabricatedContextRejected` — fixture-driven detection
  of both cheat shapes.
- `TestPattern_P11_AllowlistReasonsTruthful` — every surviving allowlist
  entry names (a) what the command does and (b) the cancellation
  mechanism.
- `TestPattern_P1_1_RowsErrCheckedAfterIteration` — 104/104 loops, no
  allowlist.
- `TestBestEffortRun_CtxCancelKillsSubprocess` + `TestRunGitCtx_CtxCancel`
  — parent-ctx cancellation propagates to a subprocess within 2s
  (the contract fabricated-ctx violated).
- Full suite `go test -tags sqlite_fts5 -count=1 -timeout 600s ./...`
  green on main (248s for agents, 24s for git, all other packages < 10s).

**Watch for.**
- `internal/claude/claude.go` `RunCLIStreaming` and `AskClaudeCLI` retain
  no-ctx forms marked "context.Background intentional: legacy convenience
  wrapper." Hot-path agents (Captain, Medic, ConvoyReview, Chancellor,
  Diplomat, PR-review-triage) still call the no-ctx form. Adopting
  `AskClaudeCLIContext` across every LLM-invoking agent is the next
  campaign (call it Fix #8f) — the infrastructure is in place, the
  adoption is not in Fix #8e's scope per the spec.
- The `add_rows_err` one-shot Go helper (`/tmp/add_rows_err`) was used
  for the Track E patcher sweep but is not in the repo — if a future
  campaign re-sweeps rows.Next() loops, rebuild the helper from the
  commit message notes or use `TestPattern_P1_1` to find offenders
  first.
- Pattern P11's allowlist is reason-gated. Adding a new file requires
  a reason that names (a) what the command does (push/fetch/clone/
  ls-remote OR rev-parse/config/symbolic-ref) and (b) the cancellation
  mechanism (dog-level timeout / CLI session / runner layer / sub-
  second). Reviewers should reject unelaborated entries.

**Stats.**
- 3 pre-existing P11 allowlist entries migrated out (pr_flow.go,
  pilot_worktree_reset.go, pilot_repo_config.go).
- 13 → 10 P11 allowlist entries total. Every surviving entry has a
  truthful descriptor.
- 104 production `for <iter>.Next()` loops now have rows.Err() coverage
  (pre-Fix-#8e: 5 of the 83 `rows`-named loops sampled by the Fix #8d
  verifier).
- 2 new cancellation-propagation tests in internal/git/.
- 3 new Pattern tests in internal/audittools/ (P1.1 + 2 P11 sub-tests).
- 91 files changed in the main commit + 12 in the follow-up.
