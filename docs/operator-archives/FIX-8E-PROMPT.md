# Fix #8e — Daemon-ctx threading for `exec.CommandContext`

## Mission

Fix #8d's Track D migrated 20 production call sites from `exec.Command` to `exec.CommandContext`, closing AUDIT-127 / 158 / 165 mechanically. The independent verifier (`FIX-8D-VERIFICATION.md`) flagged a semantic gap: **18 of those sites construct their context via `context.WithTimeout(context.Background(), T)` rather than accepting a caller-supplied ctx.** This bounds subprocess hang risk but breaks the CLAUDE.md invariant the same campaign added:

> *"long-running subprocess invocations … MUST use `exec.CommandContext(ctx, …)` so daemon shutdown / e-stop can cancel them."*

`SpawnAstromech` receives `ctx context.Context` at `internal/agents/astromech.go:318` but has no way to pass that ctx into the `runShortGit` / `combinedShortGit` / `bestEffortRun` helpers — their signatures refuse it. A `git push` that begins 100ms after an operator hits e-stop runs for its full 60-second fabricated deadline regardless of daemon shutdown. That's the exact regression Fix #1 (spend cap + e-stop) exists to prevent, just at a different layer.

Additionally, the Pattern P11 allowlist labels three sites (`pr_flow.go:64,186`, `pilot_worktree_reset.go:115`, `pilot_repo_config.go:152`) as "short" or "rev-parse" — but these are `git push` / `git fetch` / `ls-remote` network operations.

Fix #8e closes this gap. Scope is bounded and NOTHING IS OPTIONAL: thread daemon ctx through five helpers, tighten `TestPattern_P11_ExecCommandsUseContext`, rewrite or eliminate the three inaccurate allowlist reasons, AND close the `rows.Err()` enforcement gap from RESIDUAL-3. Every residual flagged by `FIX-8D-VERIFICATION.md` that this campaign names as in-scope must be closed; the campaign terminates in GO or it does not terminate.

When this ships, the CLAUDE.md invariant the prior campaign added will be enforced by production code AND by a regression test, operator e-stop will actually cancel in-flight git operations, and every `for rows.Next()` loop in production will observably check `rows.Err()` after iteration.

## Required reading

Read these before touching any code.

1. `/Users/jake.herman/code/force-orchestrator/CLAUDE.md` — especially the Fix #8d invariants (state-transition guard, rows.Scan, exec.CommandContext, Chancellor empty-subfield) and Fix #1's three load-bearing e-stop rules.
2. `/Users/jake.herman/code/force-orchestrator/FIX-8D-VERIFICATION.md` — full. This document defines what Fix #8e is closing; read the Track D section and the RESIDUAL-1 through RESIDUAL-5 entries carefully.
3. `/Users/jake.herman/code/force-orchestrator/FIX-8D-CLOSURE.md` — for the state of Track D as delivered.
4. `/Users/jake.herman/code/force-orchestrator/internal/agents/astromech.go` — especially `SpawnAstromech` at line ~318 and the three helpers (`runShortGit`, `combinedShortGit`, `combinedShortGitArgs`) at lines 32–60.
5. `/Users/jake.herman/code/force-orchestrator/internal/git/git.go` — the three helpers (`bestEffortRun`, `runGitCtx`, `runGitCtxOutput`) and every `exec.CommandContext(context.WithTimeout(context.Background(), ...), ...)` call.
6. `/Users/jake.herman/code/force-orchestrator/internal/audittools/` — find `TestPattern_P11_ExecCommandsUseContext` and its allowlist.
7. `/Users/jake.herman/code/force-orchestrator/docs/roadmap.md` — "Worktree discipline (mandatory)" section.

## Prerequisites

Fix #8d is merged to main AND `FIX-8D-CLOSURE.md` is filed AND `FIX-8D-VERIFICATION.md` is filed. Pre-flight verification (do this first, before any code):

1. Confirm the prior campaigns landed: `git log main --oneline | head -20` shows Fix #8d commits on main.
2. Confirm `remainingAuditSkips` in `internal/audittools/audittools_test.go` is empty.
3. Confirm `go test -tags sqlite_fts5 -race -count=5 ./...` is green on main before you start.
4. Confirm the 18 Track D sites from `FIX-8D-VERIFICATION.md` still show the `context.WithTimeout(context.Background(), …)` pattern — if any are already fixed, update scope accordingly.

If any pre-flight check fails: stop. Do not start Fix #8e until the prior state is clean.

## Worktree discipline (mandatory)

Every track runs in its own git worktree. Non-negotiable.

- Format: `.build-worktrees/D1-8e-<track-id>/`
- Branch: `deliverable/1/8e-<track-id>`
- Checkout source: main at the moment the agent begins, UNLESS the track has a blocking predecessor (see Merge order table).
- Rebase, never merge. `git rebase main` only; no `git merge main` into track branches.
- One track, one PR. No bundling.
- Conflict resolution is the agent's job; resolve before merging.
- Never `--no-verify`. Never `git commit --amend` after a pre-commit hook failure.

## Merge order within Fix #8e

| Order | Track | Branch | Depends on | Parallelizable with | Worktree |
|---|---|---|---|---|---|
| 1a | A — `internal/git/` helpers + call sites | `deliverable/1/8e-A` | — | B, C, E | `.build-worktrees/D1-8e-A/` |
| 1b | B — `internal/agents/astromech.go` helpers + inline site 1063 | `deliverable/1/8e-B` | — (helpers are parallel wrappers, not nested — verify this in pre-flight by checking whether astromech helpers call git helpers) | A, C, E | `.build-worktrees/D1-8e-B/` |
| 1c | C — non-helper inline sites (`internal/claude/claude.go:245`, `internal/agents/dogs.go:128`, `internal/git/askbranch.go:162, 343, 437`) | `deliverable/1/8e-C` | — | A, B, E | `.build-worktrees/D1-8e-C/` |
| 1d | E — `rows.Err()` enforcement sweep + P1.1 pattern test (MANDATORY) | `deliverable/1/8e-E` | — | A, B, C | `.build-worktrees/D1-8e-E/` |
| 2 | D — Pattern P11 test tightening + allowlist rewrite | `deliverable/1/8e-D` | A + B + C all merged (test will fail on unmigrated code) | — | `.build-worktrees/D1-8e-D/` |

**Pre-flight check required before sharding:** confirm A and B are genuinely independent. Grep whether astromech helpers call git helpers:
```
grep -n "igit\.\|\bgit\." internal/agents/astromech.go | head -20
```
If `astromech.runShortGit` calls `git.runShortGit` (or any internal/git helper), promote B to depend on A. Otherwise they're parallel.

**Parallelism rule.** Four agents may work concurrently in separate worktrees on A, B, C, E. Track D waits for A + B + C (not E) to land on main. Merges serialize — no two tracks merge simultaneously.

## Work tracks

### Track A — `internal/git/` ctx threading

**Scope.**
- Change signatures of `bestEffortRun`, `runGitCtx`, `runGitCtxOutput` in `internal/git/git.go` to accept `ctx context.Context` as the first parameter.
- Body uses the passed ctx directly. If a helper still needs its own timeout, wrap the passed ctx: `ctx, cancel := context.WithTimeout(ctx, T); defer cancel()` — note the parent is the CALLER's ctx, not `context.Background()`.
- Update every call site within `internal/git/` (including `askbranch.go:162, 343, 437` if the helpers are used there).
- Update CLAUDE.md if the function signatures are documented there.

**Caller update contract.** For each caller, trace back to the daemon ctx:
- `SpawnAstromech(ctx, ...)` holds daemon ctx → pass as `ctx`.
- Dogs hold `dogCtx` (set by `RunDogs`) → pass as `ctx`.
- PR-flow claim-loop operations hold the claim-loop ctx → pass as `ctx`.
- If a caller genuinely has no ctx (e.g., a package-init function), that is a red flag — surface in the closure report; do NOT default to `context.Background()` silently.

**Forbidden patterns.** No `context.WithTimeout(context.Background(), …)` in any line this track touches. Period. The campaign exists to eliminate this pattern.

**Tests.**
- Each modified helper gets a test asserting `ctx` cancellation propagates to the subprocess. Example shape:
  ```go
  func TestBestEffortRun_CtxCancelKillsSubprocess(t *testing.T) {
      ctx, cancel := context.WithCancel(context.Background())
      done := make(chan error, 1)
      go func() { done <- bestEffortRun(ctx, "git", "-C", repoPath, "merge", ...) }()
      time.Sleep(100 * time.Millisecond)  // let subprocess start
      cancel()
      select {
      case err := <-done:
          // ctx cancel must surface as an error within timeout budget
          if err == nil { t.Fatal("expected cancellation error") }
      case <-time.After(2 * time.Second):
          t.Fatal("subprocess did not honor ctx cancellation within 2s")
      }
  }
  ```
- Unit test run at `-race -count=5`. Must not flake.

### Track B — `internal/agents/astromech.go` ctx threading

**Scope.**
- Change signatures of `runShortGit`, `combinedShortGit`, `combinedShortGitArgs` to accept `ctx context.Context` as first parameter.
- If these helpers call into `internal/git/` helpers, Track B rebases on Track A and uses A's updated signatures.
- Update the inline `exec.CommandContext(context.WithTimeout(context.Background(), …), …)` at `astromech.go:1063` (the claude CLI invocation) to use `SpawnAstromech`'s ctx.
- Update every caller of these helpers within `internal/agents/`. Each caller must pass the ctx it already holds.

**Caller update contract.** Same as Track A: trace back to the daemon ctx; no silent `context.Background()` fallback.

**Tests.**
- Each modified helper gets a cancellation-propagation test as in Track A.
- Integration test: `TestAstromech_EstopCancelsInFlightGitOp` — spawn an astromech in a test repo with a deliberately slow network-op (mocked via a stub that hangs for 10s), issue e-stop via `store.SetEstopped(true)`, assert the astromech's git op errors within 2s (not 10s).

### Track C — Remaining inline sites

**Scope.**
- `internal/claude/claude.go:245` — inline `context.WithTimeout(context.Background(), …)` for claude CLI invocations. Update `AskClaudeCLI` / `RunCLIStreamingContext` as needed so the caller's ctx threads through. (`RunCLIStreamingContext` already accepts ctx; `AskClaudeCLI` may need a Context variant.)
- `internal/agents/dogs.go:128` — inline site. Thread `dogCtx` (the ctx passed to `RunDogs`).
- `internal/git/askbranch.go:162, 343, 437` — inline sites. These are `git worktree remove` (defer), `merge --no-ff`, and similar. Thread the caller's ctx.

**Tests.**
- For each site, a test that the caller's ctx cancellation surfaces as an error before the fabricated deadline fires.

### Track D — Pattern P11 test tightening + allowlist rewrite

**Prerequisites:** A + B + C all merged to main. The tightened test will fail if any unmigrated site remains.

**Scope.**

*Test tightening.* Modify `TestPattern_P11_ExecCommandsUseContext` in `internal/audittools/` to enforce:

1. Replace the current ratio-based assertion (`totalCtx > 0 && total <= totalCtx*2`) with a per-site check: **every `exec.Command` OR `exec.CommandContext` call outside the explicit allowlist must either (a) use `exec.CommandContext` with a ctx argument that is NOT `context.Background()` and NOT a fresh `context.WithTimeout(context.Background(), …)`, or (b) be a `testing.T`-scoped call in a `_test.go` file**.
2. The detection mechanism: walk the AST of every `exec.CommandContext(...)` call; the first argument must resolve to a parameter, field, or variable — not to a function call that returns a fresh disconnected context.
3. Explicit denial check: scan for `exec.CommandContext(context.WithTimeout(context.Background(), …), …)` as a literal shape; any match outside the allowlist fails the test.
4. Allowlist shape: `map[string]string` keyed by `<file>:<line>` with a mandatory reason. Reason must truthfully describe (a) what the command is (e.g., "ls-remote against remote origin — network op"), and (b) why daemon ctx is not threaded (e.g., "dog-level 5-minute parent timeout; fleet cancellation propagates through dog shutdown channel").

*Allowlist rewrite.* Audit each existing entry against its actual call:
- `pr_flow.go:64` and `pr_flow.go:186` — currently labeled "short (rev-parse)" but actual is `git push`. Either migrate these sites to ctx-threaded form (drop from allowlist) OR rewrite the reason to accurately describe the network op + the compensating timeout mechanism.
- `pilot_worktree_reset.go:115` — currently labeled "reset/clean cleanup" but actual is `git fetch origin`. Same decision.
- `pilot_repo_config.go:152` — currently labeled "ls-remote under dog-level 5m timeout." This is accurate in mechanism but mislabeled as "short." Rewrite to describe the network op explicitly.

**My recommendation:** migrate the `pr_flow.go` and `pilot_worktree_reset.go` sites to ctx-threaded form (they have callers holding usable ctx) and drop them from the allowlist. Keep `pilot_repo_config.go:152` in the allowlist with a corrected reason (the dog-level timeout is a legitimate cancellation path when daemon ctx isn't available at the call site).

**Tests.**
- `TestPattern_P11_FabricatedContextRejected` — seed a test fixture Go file containing `exec.CommandContext(context.WithTimeout(context.Background(), time.Minute), "git", "status")` and assert the pattern test rejects it.
- `TestPattern_P11_AllowlistReasonsTruthful` — for every allowlist entry, verify the reason string references either a network-op descriptor ("push", "fetch", "ls-remote", "clone") OR a cancellation-mechanism descriptor ("dog-level timeout", "sweep-cycle fallback"). Missing both → test fails.

### Track E — `rows.Err()` enforcement (MANDATORY)

**Scope.**
- New pattern test `TestPattern_P1_1_RowsErrCheckedAfterIteration` in `internal/audittools/`.
- Walk production code for EVERY `for rows.Next() { ... }` loop (no allowlist, no sample — the full set); assert that within the 10-line window following the loop's closing brace, `rows.Err()` is called AND its result is either returned, logged with a named recovery path, or error-wrapped. A loop whose function returns cleanly without `rows.Err()` is a failure; "no further use of rows" is NOT sufficient.
- Fix the 4 sites flagged by `FIX-8D-VERIFICATION.md` RESIDUAL-3:
  - `internal/store/holocron.go:115`
  - `internal/agents/escalation.go:102`
  - `internal/store/convoy.go:94`
  - `cmd/force/print.go:59`
- Enumerate and fix EVERY other iteration loop in production. Do not sample; exhaustive sweep. Run the grep `grep -rnE 'for rows\.Next\(\)' --include="*.go" internal/ cmd/ | grep -v '_test.go'`, enumerate every hit, audit each for a post-loop `rows.Err()` check, fix any that lack it.
- Update CLAUDE.md's rows.Scan invariant to confirm "rows.Err() is checked after iteration" is now enforced by Pattern P1.1. DO NOT downgrade the wording — the campaign closes the gap rather than weakening the contract.

This track is fully independent of A/B/C/D. It MUST complete before `FIX-8E-CLOSURE.md` can be filed. The operator will NOT accept a CONDITIONAL-GO verdict that defers Track E.

## Universal anti-cheat directives

The Fix #8d verifier's Track D finding gives a specific template for what NOT to repeat:

1. **No `context.WithTimeout(context.Background(), …)` in any line this campaign touches.** If a helper genuinely needs its own timeout budget, wrap the PASSED ctx: `ctx, cancel := context.WithTimeout(ctx, T)`. The parent MUST be the caller's ctx. This is the load-bearing rule.
2. **No helper that silently creates `context.Background()` when the caller doesn't pass one.** If a caller doesn't have a ctx, surface it. Add it to the caller's signature, propagate up to the claim loop. The claim loop has a ctx; nothing is more than two hops away from one.
3. **No "short-op exception" by vocabulary.** A `git push` against a remote is not "short." An `ls-remote` is not "pingability." The allowlist reason must describe what the command actually does.
4. **No ratio-based Pattern P11 assertion.** Per-site enforcement; every non-allowlisted site must be individually compliant.
5. **No ghost helper.** A helper whose new signature is `ctx context.Context, ...` but whose body doesn't use `ctx` for the subprocess ctx is a ghost migration.
6. **No `context.TODO()` as a placeholder for "I'll fix this later."** If a caller legitimately has no ctx, use `context.Background()` with an explicit comment: `// context.Background intentional: <specific reason>`. The Pattern P11 test grants a narrow exception for comments-with-reason that reviewers have accepted; operator approves the allowlist addition.
7. **No softened test assertions.** If a test asserted a behavior at `-race -count=5` with 20 trials, the post-fix version does the same.
8. **No `_ = rows.Err()` as the "fix" for Track E.** The Err check must be meaningful (`if err := rows.Err(); err != nil { return err }` or equivalent logged-and-recovered path), not silently discarded.

The universal anti-cheat directives from `docs/roadmap.md` top section also apply.

## RGR discipline

For every code change:

- **Red.** Before the fix, confirm the cancellation-propagation test fails (subprocess runs to its fabricated deadline rather than cancelling on ctx cancel). Capture the failure message.
- **Green.** Apply the fix. Confirm the test passes at `-race -count=5`.
- **Refactor.** If the fix introduces duplication, clean up. Tests must still pass.

Red-phase evidence goes in the commit body or in a running log for the closure report.

## Verification procedure

Run these exactly. Each must pass before moving on.

```
# Track A
go test -tags sqlite_fts5 -run TestBestEffortRun_CtxCancel -race -count=5 ./internal/git/...  # green
go test -tags sqlite_fts5 -run TestRunGitCtx_CtxCancel -race -count=5 ./internal/git/...      # green

# Track B
go test -tags sqlite_fts5 -run TestRunShortGit_CtxCancel -race -count=5 ./internal/agents/...  # green
go test -tags sqlite_fts5 -run TestAstromech_EstopCancelsInFlightGitOp -race -count=5 ./internal/agents/...  # green

# Track C
go test -tags sqlite_fts5 -run TestClaudeCLI_CtxCancel -race -count=5 ./internal/claude/...    # green
go test -tags sqlite_fts5 -run TestDogs_CtxCancel -race -count=5 ./internal/agents/...         # green

# Track D — pattern test tightening
go test -tags sqlite_fts5 -run TestPattern_P11_ -race -count=5 ./internal/audittools/...       # green
go test -tags sqlite_fts5 -run TestPattern_P11_FabricatedContextRejected -race -count=5 ./internal/audittools/...  # green
go test -tags sqlite_fts5 -run TestPattern_P11_AllowlistReasonsTruthful -race -count=5 ./internal/audittools/...   # green

# Track E (mandatory)
go test -tags sqlite_fts5 -run TestPattern_P1_1_RowsErrCheckedAfterIteration -race -count=5 ./internal/audittools/...  # green
grep -rnE 'for rows\.Next\(\)' --include="*.go" internal/ cmd/ | grep -v '_test.go' | wc -l  # enumerate; every hit must be audit-checked for post-loop rows.Err()

# Grep check — no fabricated contexts in production
grep -rnE 'context\.WithTimeout\(context\.Background\(\)' --include="*.go" internal/ cmd/ | grep -v '_test.go'
# Expected: 0 hits (or only allowlisted hits with approved comment markers).

# Grep check — Pattern P11 test body enforces fabricated-context denial
grep -n 'context.WithTimeout(context.Background' internal/audittools/*.go
# Expected: at least one hit in the pattern test body as a rejection-fixture or denial pattern.

# Full suite
go test -tags sqlite_fts5 -race -count=5 ./...  # green no flakes
make smoke && make fuzz && make test-audit      # all green
```

If ANY of the above fails, the campaign is not done and the closure report must not be filed.

## Deliverables

Produce `FIX-8E-CLOSURE.md` at the repo root with:

1. **Per-track summary** (A, B, C, D, E — all five, mandatory) with commit SHAs.
2. **Migration table.** For each of the 18 original Track D sites (enumerated in `FIX-8D-VERIFICATION.md`): the file:line, the post-fix form, and the ctx it now threads.
3. **Allowlist audit.** For each Pattern P11 allowlist entry: the entry's reason before Fix #8e, the reason after, and whether the site was migrated out of the allowlist (preferred) or kept with a corrected reason.
4. **rows.Err() sweep table.** For every `for rows.Next()` loop in production (not sampled — every one): file:line, pre-fix state (checked / unchecked), post-fix state, and the test that enforces the check.
5. **Verification output** pasted verbatim — every command above, every outcome.
6. **Anti-cheat self-check.** For each of the 8 numbered anti-cheat directives, affirm it was not violated.
7. **CLAUDE.md updates.** Confirm the Fix #8d `exec.CommandContext` invariant is now enforced by production code + regression test. Confirm the `rows.Err()` clause is now enforced by `TestPattern_P1_1_RowsErrCheckedAfterIteration`. No wording downgrades.
8. **Residual list.** Genuinely out-of-scope items only. Any in-scope residual means the campaign is not done and the closure report is not filed. If the operator's verifier returns CONDITIONAL-GO, Fix #8e re-opens until it returns GO.

## Restart gate

The operator will re-run the Fix #8d verifier (or a Fix #8e-specific variant) against this campaign. The required verdict is **GO**. **CONDITIONAL-GO is not acceptable.** If the verifier returns CONDITIONAL or NO-GO, the campaign is not done; Fix #8e re-opens and continues until the verifier returns GO.

The verifier will check every one of the following; any miss is a restart blocker:

- Zero `context.WithTimeout(context.Background(), …)` in production (non-test) code outside a documented allowlist.
- Every `exec.CommandContext` first-arg traces back (syntactically) to a caller-supplied ctx.
- Integration test `TestAstromech_EstopCancelsInFlightGitOp` demonstrates e-stop cancels a running git op within 2 seconds.
- Pattern P11 test has a per-site check, not a ratio check.
- Allowlist reasons describe truthfully what the command is.
- `TestPattern_P1_1_RowsErrCheckedAfterIteration` is present and green, enforcing `rows.Err()` on every `for rows.Next()` loop in production.
- Every `for rows.Next()` loop in production has a post-iteration `rows.Err()` check whose result is propagated.
- Full suite green at `-race -count=5`.
- CLAUDE.md invariants (both the Fix #8d `exec.CommandContext` one and the `rows.Err()` one) are enforced by production + regression tests, not weakened in wording.

This campaign terminates in a full GO. There is no path from here to "ship with caveats." Either the gap closes or the work continues.
