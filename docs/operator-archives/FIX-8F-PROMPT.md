# Fix #8f — Close the Fix #8e integration-test gap

## Mission

Fix #8e delivered the substantive code work — every helper accepts `ctx`, production no longer fabricates `context.WithTimeout(context.Background(), …)`, Pattern P11 is per-site, allowlist shrunk and truthful, rows.Err() exhaustively swept, CLAUDE.md strengthened. The independent verifier (`FIX-8E-VERIFICATION.md`) returned **NOT-DONE** for a single restart-blocker:

> *"Spec-named integration test `TestAstromech_EstopCancelsInFlightGitOp` is absent. Closure-report substitute (helper-level shape-mirror tests in package git, neither calling `store.SetEstopped` nor calling the production helpers they're named after) does not satisfy the restart-gate contract."*

Plus four non-blocker defects the verifier surfaced for closure-quality reasons.

Fix #8f exists to close the verifier's enumerated gaps and **only** those gaps. Scope is bounded; nothing else changes. When this ships, the FIX-8E verifier re-run must return **GO** with zero residuals on the items below. Anything short of GO means Fix #8f re-opens.

## What this campaign does NOT do

- Modify any production code paths in `internal/git/` or `internal/agents/` beyond test files. (Fix #8e's production code is correct; do not regress it.)
- Modify CLAUDE.md (Fix #8e's wording is correct; do not weaken it).
- Modify the Pattern P11 allowlist (Fix #8e's allowlist is correct; do not relax it).
- Modify Fix #8d closures (`UpdateBountyStatusFrom`, P7 tests, bare-terminator sweep — leave alone).
- Touch any deliverable beyond Fix #8f scope.

If any change in your worktree affects production code outside the listed scope, stop and surface in the closure report.

## Required reading

1. `/Users/jake.herman/code/force-orchestrator/FIX-8E-VERIFICATION.md` — full. Especially "Forensic appendix" Failures 1–2 and Defects 3–5.
2. `/Users/jake.herman/code/force-orchestrator/FIX-8E-CLOSURE.md` — for the substitute-test claims that didn't satisfy the spec.
3. `/Users/jake.herman/code/force-orchestrator/FIX-8E-PROMPT.md` — Track B "Tests" section (the integration-test contract you must satisfy) and the universal anti-cheat directives.
4. `/Users/jake.herman/code/force-orchestrator/internal/agents/astromech.go` — `runShortGit`, `combinedShortGit`, `combinedShortGitArgs` definitions; `SpawnAstromech` ctx threading at line ~326; `RunTaskForeground` at line ~995.
5. `/Users/jake.herman/code/force-orchestrator/internal/git/git.go` — `bestEffortRun`, `runGitCtx`, `runGitCtxOutput` — these are the symbols that Track B's existing shape-mirror tests must be rewritten to actually call.
6. `/Users/jake.herman/code/force-orchestrator/internal/git/ctx_cancel_test.go` — the existing shape-mirror tests you'll be rewriting.
7. `/Users/jake.herman/code/force-orchestrator/internal/audittools/audit_pattern_p1_1_rows_err_test.go` — for the window-tightening change.
8. `/Users/jake.herman/code/force-orchestrator/internal/store/estop.go` — for `SetEstopped` and `IsEstopped` — the integration test will use these.

## Prerequisites

- Fix #8e is merged to main.
- `FIX-8E-CLOSURE.md` and `FIX-8E-VERIFICATION.md` are filed.
- The verifier's NOT-DONE verdict has been read and understood.

Pre-flight verification (do these first, before any code):

1. Confirm Fix #8e on main: `git log main --oneline | head -10` shows recent Fix #8e commits.
2. Confirm `remainingAuditSkips` in `internal/audittools/audittools_test.go` is empty.
3. Confirm `go test -tags sqlite_fts5 -race -count=5 ./...` is green on main before starting (Fix #8e's substantive delivery should not be regressed).
4. Confirm the missing tests are still missing:
   ```
   grep -rn 'TestAstromech_EstopCancelsInFlightGitOp\|TestRunShortGit_CtxCancel' --include="*.go" .
   ```
   Expected: 0 hits. If they have appeared, scope has shifted; report back.
5. Confirm the shape-mirror tests still have their substitution shape:
   ```
   grep -A 20 'func TestBestEffortRun_CtxCancelKillsSubprocess' internal/git/ctx_cancel_test.go
   ```
   Confirm the body inlines `exec.CommandContext(c, "sleep", "30").Run()` rather than calling `bestEffortRun(...)`.
6. Confirm P1.1's window is still 60 lines:
   ```
   grep -n 'closeIdx + ' internal/audittools/audit_pattern_p1_1_rows_err_test.go
   ```
   Expected: a hit showing `closeIdx + 60` (or similar permissive value > 10).

If any pre-flight check fails: stop. Report the unexpected state.

## Worktree discipline (mandatory)

Every track runs in its own git worktree.

- Format: `.build-worktrees/D1-8f-<track-id>/`
- Branch: `deliverable/1/8f-<track-id>`
- Checkout source: main at the moment the agent begins.
- Rebase, never merge. `git rebase main` only.
- One track, one PR. No bundling.
- Conflict resolution in your worktree before merging.
- Never `--no-verify`. Never `git commit --amend` after a pre-commit hook failure.

## Merge order within Fix #8f

| Order | Track | Branch | Depends on | Parallelizable with | Worktree |
|---|---|---|---|---|---|
| 1a | A — Integration test for e-stop cancellation (the load-bearing item) | `deliverable/1/8f-A` | — | B, C | `.build-worktrees/D1-8f-A/` |
| 1b | B — Rewrite shape-mirror tests to call production helpers + add `TestRunShortGit_CtxCancel` | `deliverable/1/8f-B` | — | A, C | `.build-worktrees/D1-8f-B/` |
| 1c | C — P1.1 window tightening + closure-report arithmetic fix | `deliverable/1/8f-C` | — | A, B | `.build-worktrees/D1-8f-C/` |

All three tracks develop concurrently in separate worktrees. Merges serialize (no simultaneous merges to main). No track blocks any other; merge in any order.

## Work tracks

### Track A — Integration test for e-stop cancellation (load-bearing)

This is the restart-blocker. Failure to deliver this correctly is the only thing keeping the campaign from GO.

**File:** `internal/agents/astromech_estop_cancel_test.go` (new file).

**Test name:** `TestAstromech_EstopCancelsInFlightGitOp` (verbatim — spec-named).

**Contract.** The test must:

1. **Live in `package agents`.** Not `package git`. Not `package agents_test`. The verifier checks the package location.
2. **Spawn or simulate an astromech operation that calls a production helper.** The test must invoke `runShortGit`, `combinedShortGit`, `combinedShortGitArgs`, or `RunTaskForeground` with a context that the test controls — it must not inline-mirror the helper's shape.
3. **Use a deliberately-slow operation.** Common shape: `git clone <unreachable-but-non-erroring-source>` or a stub git remote that hangs (use a netcat listener that accepts the connection but never responds, or a local file:// remote with a custom git config that stalls). The op must reliably take longer than 2 seconds in the absence of cancellation, so the post-cancel measurement is meaningful.
4. **Call `store.SetEstopped(db, true)` to trigger cancellation.** This is the property the integration test exists to demonstrate: operator e-stop must reach the in-flight subprocess via ctx cancellation.
5. **Assert the in-flight git op errors within 2 seconds of the SetEstopped call.** Budget is `2 * time.Second`, asserted with `t.Fatal` on miss. NOT 60 seconds; not 30 seconds; 2 seconds. The whole point is to prove the daemon ctx cancellation propagates to the subprocess fast.
6. **Pass at `-race -count=5`.** Deterministic, no flakes.

**Implementation sketch (illustrative — adjust for your choice of slow-op fixture):**

```go
package agents

import (
    "context"
    "os/exec"
    "testing"
    "time"

    "force-orchestrator/internal/store"
)

func TestAstromech_EstopCancelsInFlightGitOp(t *testing.T) {
    if _, err := exec.LookPath("git"); err != nil {
        t.Skip("git not in PATH; integration test requires git")
    }

    db := store.InitHolocronDSN(":memory:")
    defer db.Close()

    // Seed a deliberately-slow remote. Choose one that's reliable in CI:
    // option (a): a local netcat-listening port that accepts but never sends
    //   git protocol bytes; clone hangs at HTTP fetch.
    // option (b): a local file:// remote with a giant pre-receive hook that
    //   sleeps; push hangs.
    // option (c): a synthetic helper that calls runShortGit against a known-
    //   slow operation (e.g. a fetch from a remote with a 30s sleep).
    slowRemote := setupSlowGitRemote(t)  // helper returns a remote URL

    ctx, cancel := context.WithCancel(context.Background())
    defer cancel()

    done := make(chan error, 1)
    started := make(chan struct{})

    go func() {
        close(started)
        // CRITICAL: this must call a production helper, not inline-mirror.
        // Choose runShortGit for the simplest-to-test path.
        err := runShortGit(ctx, "clone", slowRemote, t.TempDir())
        done <- err
    }()

    <-started
    // Wait briefly so the subprocess actually begins.
    time.Sleep(150 * time.Millisecond)

    // The actual e-stop trigger.
    if err := store.SetEstopped(db, true); err != nil {
        t.Fatalf("SetEstopped failed: %v", err)
    }
    cancel()  // simulates the daemon's ctx cancellation in response to e-stop.

    select {
    case err := <-done:
        if err == nil {
            t.Fatal("expected cancellation error from interrupted git op; got nil")
        }
    case <-time.After(2 * time.Second):
        t.Fatal("e-stop did not cancel in-flight git op within 2s — daemon ctx cancellation not propagating to subprocess")
    }
}
```

**Choosing a reliable slow-op fixture.** This is the tricky part of the test. Considerations:

- A `git clone` of an unreachable URL fails fast (DNS, refused connection) — wrong shape.
- A `git clone` of a netcat-listening port that accepts and stalls is reliable but requires opening a port (CI-fragile).
- A local file:// remote with a `pre-receive` hook that `sleep 30`s is reliable cross-platform but requires git-hook plumbing.
- A test-only helper that wraps `exec.CommandContext(ctx, "sleep", "30")` and routes through `runShortGit`-equivalent logic is pragmatic — but read the spec carefully: it MUST exercise the production code path. If you wrap in a way that calls `runShortGit("clone", "--reference", "/bin/sleep", ...)`, that's not production semantics either.

The cleanest robust shape: a local file:// remote with a slow pre-receive hook plus a `runShortGit(ctx, "fetch", ...)` invocation. The hook makes the operation slow; the `runShortGit` call is the production path. Document your fixture choice in the test's leading comment.

**Test-quality requirements (verifier will check):**

- The function name must literally be `TestAstromech_EstopCancelsInFlightGitOp` — no variations.
- The body must contain a literal `store.SetEstopped(` call — verifier will grep for it.
- The body must contain a `time.After(2 * time.Second)` (or `2*time.Second`) — not a longer budget.
- The body must invoke at least one of `runShortGit`, `combinedShortGit`, `combinedShortGitArgs`, or `RunTaskForeground` — not `exec.CommandContext` directly.
- Failure on cancellation miss must use `t.Fatal` or `t.Fatalf`, not `t.Log` or `t.Skipf`.

**No deliberate budget inflation.** A test using `time.After(60 * time.Second)` is a cheat — the assertion would pass whether or not the cancellation works. The 2-second budget is what proves the property.

### Track B — Rewrite shape-mirror tests + add TestRunShortGit_CtxCancel

Two sub-items:

**B1.** Rewrite the existing tests in `internal/git/ctx_cancel_test.go` to actually call the helpers they're named after.

Current state of `TestBestEffortRun_CtxCancelKillsSubprocess`:

```go
func TestBestEffortRun_CtxCancelKillsSubprocess(t *testing.T) {
    ...
    go func() {
        c, c2 := context.WithTimeout(ctx, 30*time.Second)
        defer c2()
        _ = exec.CommandContext(c, "sleep", "30").Run()  // SHAPE MIRROR
        ...
    }()
    ...
}
```

Required state (call the production helper):

```go
func TestBestEffortRun_CtxCancelKillsSubprocess(t *testing.T) {
    if _, err := exec.LookPath("sleep"); err != nil {
        t.Skip("sleep not in PATH")
    }

    repoPath := t.TempDir()  // bestEffortRun expects a working tree; use a temp dir
    // Initialize a minimal git repo so bestEffortRun's git invocations don't trivially fail
    if err := exec.Command("git", "-C", repoPath, "init").Run(); err != nil {
        t.Fatalf("git init: %v", err)
    }

    ctx, cancel := context.WithCancel(context.Background())
    defer cancel()

    done := make(chan struct{})
    go func() {
        // bestEffortRun invokes the production helper, which internally wraps
        // ctx via WithTimeout(ctx, T). If the helper body regresses to
        // fabricating Background, this test surfaces the regression.
        // Use a deliberately-slow git op — e.g. a fetch from a slow remote
        // or a similar long-running git command. Choose a reliable shape.
        _ = bestEffortRun(ctx, "git", "-C", repoPath, "fetch", slowRemoteURL(t))
        close(done)
    }()

    time.Sleep(150 * time.Millisecond)
    cancel()

    select {
    case <-done:
    case <-time.After(2 * time.Second):
        t.Fatal("bestEffortRun did not honor ctx cancellation within 2s — body regressed")
    }
}
```

Same pattern for `TestRunGitCtx_CtxCancel` — call `runGitCtx(ctx, ...)` directly.

The verifier explicitly noted: "A future refactor that breaks the production helper's body (e.g., reverting to `WithTimeout(context.Background(), T)`) would still leave these tests passing — they prove the author's asserted shape, not the helper's body." Track B's rewrite eliminates that gap.

**Slow-op fixture.** Same considerations as Track A. You can share a helper between Track A and Track B if both worktrees rebase cleanly — but since they're parallel-developed, one of them lands first and the other rebases. Simplest approach: each track has its own slow-op helper inline; no cross-track sharing needed for ~30 lines of fixture code.

**B2.** Add `TestRunShortGit_CtxCancel` in a new file `internal/agents/runshortgit_cancel_test.go` (or extend an existing test file in the agents package).

Same shape as B1 but exercising `runShortGit(ctx, ...)` from the agents package. Note: `runShortGit` is a function in `internal/agents/astromech.go`; the test must live in `package agents` so it can call the unexported function directly.

Required:
- Function name: `TestRunShortGit_CtxCancel` (verbatim).
- Body calls `runShortGit(ctx, ...)` — the production helper.
- 2-second budget with `t.Fatal` on miss.
- Pass at `-race -count=5`.

### Track C — P1.1 window tightening + closure-report arithmetic fix

Two sub-items, both small:

**C1.** Tighten the `audit_pattern_p1_1_rows_err_test.go` window from 60 lines to 10 lines, per the spec.

Current at L103: `windowEnd := closeIdx + 60`.

Required: `windowEnd := closeIdx + 10`.

Run the test after the change: `go test -tags sqlite_fts5 -run TestPattern_P1_1 -race -count=5 ./internal/audittools/...`. Expected: still green (the verifier confirmed no production loop has its `rows.Err()` check 50+ lines below the close, so 10 is sufficient). If a real production site fails, fix the production site by moving the `rows.Err()` check closer to the loop close — do NOT widen the window back.

**C2.** Fix the rows.Err() sweep arithmetic in `FIX-8E-CLOSURE.md`.

The closure report claims "Total: 88 patched" but the per-file table sums to 95. This is documentation only; code is correct. Either:

- Update the prose to "95" to match the table, OR
- Update the per-file table to sum to 88 (only if the real number is 88; verify by re-running the count).

Re-run the count by greping `for rows.Next()` (or your equivalent iterator) and tallying per-file. The closure report's table is a snapshot; ensure it matches reality. Whatever the number is, prose and table must agree.

This change is a documentation edit only. No code changes.

## Universal anti-cheat directives

In addition to the universal directives in `docs/roadmap.md`:

1. **No production code changes outside test files and FIX-8E-CLOSURE.md.** Fix #8e's production code is correct. Track A, B, and C produce only test files (Track A new, Track B rewrite + new, Track C tightening) and a single doc edit. Any production diff is out-of-scope.
2. **No 60-second test budgets.** The 2-second budget is what proves the property. A test using `time.After(60 * time.Second)` would pass even if the helper does nothing — useless and a cheat.
3. **No shape-mirror substitution.** Track B's rewrite must invoke the production helpers by name. If a test body contains `exec.CommandContext(c, ...)` instead of `bestEffortRun(c, ...)`, it's still a shape mirror.
4. **No `t.Skip` to skirt the slow-op fixture.** If the slow-op fixture is hard to set up reliably in CI, design it to be reliable — don't skip the test on "fixture too hard." Skipping defeats the purpose.
5. **No `t.Log`-on-miss.** Failure to cancel within 2 seconds must be `t.Fatal` or `t.Fatalf` — fatal, not advisory.
6. **No P1.1 widening.** If a real production loop's `rows.Err()` is more than 10 lines below the close, fix the production site, don't widen the window. The window-tightening is a feature, not a regression target.
7. **No closure-report wallpapering.** If the rows.Err() count is genuinely 95, write 95. If 88, verify and write 88. Don't pick the lower number to make the report look smaller.
8. **No --no-verify on commits.** Pre-commit hooks must pass.

## RGR discipline

For every test added or rewritten:

- **Red.** Before the implementation, the test must fail. For Track A's integration test, "red" means: temporarily revert one of the helpers to fabricate `context.WithTimeout(context.Background(), …)` and confirm the test catches the regression (subprocess does NOT cancel within 2s). Then restore. For Track B's rewrites, "red" means: confirm the rewritten test fails if the helper body fabricates Background. Capture red-phase failure messages in commit bodies.
- **Green.** With current production code, the test passes at `-race -count=5`.
- **Refactor.** Clean up; remove debug logging; tighten if duplication exists.

## Verification procedure

Run these in order. Every check must pass before filing the closure report.

```
# Pre-flight: confirm Fix #8e baseline still holds
go test -tags sqlite_fts5 -race -count=5 ./...   # green
grep -rn 't\.Skip(.AUDIT-' --include="*.go" internal/ cmd/ schema/   # 0 hits
grep -rnE 'context\.WithTimeout\(context\.Background\(\)' --include="*.go" internal/ cmd/ | grep -v '_test.go'   # 0 hits

# Track A
grep -rn 'TestAstromech_EstopCancelsInFlightGitOp' --include="*.go" internal/agents/   # 1 hit (the new test)
go test -tags sqlite_fts5 -run TestAstromech_EstopCancelsInFlightGitOp -race -count=5 ./internal/agents/...   # green
# Verify the test body contains the required artifacts:
grep 'store\.SetEstopped' internal/agents/astromech_estop_cancel_test.go   # 1+ hits
grep '2.*time\.Second' internal/agents/astromech_estop_cancel_test.go   # 1+ hits (the 2-second budget)
grep 'runShortGit\|combinedShortGit\|RunTaskForeground' internal/agents/astromech_estop_cancel_test.go   # 1+ hit (production helper called)
grep 't\.Fatal\|t\.Fatalf' internal/agents/astromech_estop_cancel_test.go   # 1+ hit (fatal on miss)

# Track B
grep -rn 'TestRunShortGit_CtxCancel' --include="*.go" internal/agents/   # 1 hit
go test -tags sqlite_fts5 -run TestRunShortGit_CtxCancel -race -count=5 ./internal/agents/...   # green
# Confirm shape-mirror tests rewritten:
grep -A 30 'func TestBestEffortRun_CtxCancelKillsSubprocess' internal/git/ctx_cancel_test.go | grep 'bestEffortRun('   # 1+ hit
grep -A 30 'func TestRunGitCtx_CtxCancel' internal/git/ctx_cancel_test.go | grep 'runGitCtx('   # 1+ hit
go test -tags sqlite_fts5 -run 'TestBestEffortRun_CtxCancelKillsSubprocess|TestRunGitCtx_CtxCancel' -race -count=5 ./internal/git/...   # green

# Track C
grep -n 'closeIdx + ' internal/audittools/audit_pattern_p1_1_rows_err_test.go   # shows closeIdx + 10
go test -tags sqlite_fts5 -run TestPattern_P1_1 -race -count=5 ./internal/audittools/...   # green

# FIX-8E-CLOSURE.md arithmetic consistent
# (Manual review: prose-table arithmetic must match.)

# Full suite
go test -tags sqlite_fts5 -race -count=5 ./...   # green no flakes
make smoke && make fuzz && make test-audit   # all green

# Re-run the FIX-8E verifier
# Hand the verification prompt back to a fresh agent (or re-run the same one)
# Expected verdict: GO
```

## Deliverables

Produce `FIX-8F-CLOSURE.md` at the repo root with:

1. **Per-track summary** (A, B, C) with commit SHAs.
2. **Track A test details** — slow-op fixture choice, why it's reliable in CI, the red-phase regression demonstration (which helper was temporarily broken to confirm the test catches the regression).
3. **Track B test details** — rewritten test bodies, confirmation each calls the production helper, red-phase regression demonstration.
4. **Track C details** — P1.1 window tightening commit; closure-report arithmetic correction (before/after).
5. **Verification output** pasted verbatim — every command above, every outcome.
6. **Re-run of the FIX-8E verifier** — paste the new verdict and confirm GO. If the verifier returns anything other than GO, the campaign is not done; do not file the closure report.
7. **Anti-cheat self-check** — for each of the 8 numbered directives, affirm not violated.
8. **Residual list** — anything found out of scope.

## Restart gate

The operator restarts the daemon when:

- All three tracks merged to main.
- `FIX-8F-CLOSURE.md` filed.
- Re-run of the FIX-8E verifier returns **GO** (not CONDITIONAL-GO, not NOT-DONE).
- All Fix #8e closures still passing under `-race -count=5`.
- All Fix #8d closures still passing under `-race -count=5`.

If the verifier returns NOT-DONE again on this round, Fix #8f re-opens for the specific items it flags. The campaign terminates in GO.
