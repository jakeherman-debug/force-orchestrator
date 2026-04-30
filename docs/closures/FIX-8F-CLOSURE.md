# Fix #8f Closure Report

## Verdict: GO

The FIX-8E verifier re-ran against the post-Fix-#8f state of `main` and
returned **GO** with zero restart-blockers. See "Re-run of FIX-8E
verifier" section below for the full output.

Closes the single restart-blocker the FIX-8E verifier flagged plus the
four non-blocker defects.

The verifier returned NOT-DONE for one specific reason:

> *"Spec-named integration test `TestAstromech_EstopCancelsInFlightGitOp`
> is absent. Closure-report substitute (helper-level shape-mirror tests
> in package git, neither calling `store.SetEstopped` nor calling the
> production helpers they were named after) does not satisfy the
> restart-gate contract."*

Plus four minor defects: `TestRunShortGit_CtxCancel` missing,
shape-mirror tests don't call production helpers, FIX-8E-CLOSURE.md
arithmetic inconsistent, P1.1 window 60 lines vs spec's 10.

Fix #8f closes all five items in three parallel-developed tracks.
Production code is **unchanged** outside of the `bestEffortRun` /
`runGitCtx` / `runShortGit` red-phase reverts (restored before commit;
diffs verified empty by `git diff` post-restore).

## Per-track summary

| Track | Status | Branch | Commit SHA | Worktree |
|---|---|---|---|---|
| A — TestAstromech_EstopCancelsInFlightGitOp | CLOSED | `deliverable/1/8f-A` | `681d741` | `.build-worktrees/D1-8f-A/` |
| B — Rewrite ctx-cancel tests + add TestRunShortGit_CtxCancel | CLOSED | `deliverable/1/8f-B` | `ade7a78` | `.build-worktrees/D1-8f-B/` |
| C — P1.1 window 60→10 + FIX-8E-CLOSURE.md arithmetic | CLOSED | `deliverable/1/8f-C` | `d116b50` | `.build-worktrees/D1-8f-C/` |

All three tracks merged to `main` via `--no-ff` (Track A fast-forwarded;
B and C as merge commits since they branched from main pre-A).

Merge commits: `ec8e2b8` (Track C), `72ecabc` (Track B), `681d741`
(Track A — fast-forwarded).

## Track A — TestAstromech_EstopCancelsInFlightGitOp

**File:** `internal/agents/astromech_estop_cancel_test.go` (new, 170 lines).

**Slow-op fixture:** a file:// remote whose `pre-receive` hook sleeps
30 seconds. `git push` to the remote runs the hook server-side
(file:// is local — the hook is invoked in-process by git-receive-pack),
so the push blocks until either (a) the hook returns or (b) the outer
git push process is killed via ctx cancellation. The fixture seeds a
working tree with one commit and wires `origin` to the bare remote.

**Why this fixture is reliable in CI:**

- Pure-local: no network, no port allocation, no DNS, no firewall.
- POSIX baseline: the test t.Skips if `git`, `sh`, or `sleep` is
  absent from PATH.
- Pre-receive hooks are a stable git feature (since 1.0).
- The 30s pre-receive sleep is far longer than any reasonable CI
  scheduling jitter, so the wall-clock observation is unambiguous.

For runShortGit specifically, the descendant-pipe-inheritance issue
that plagued runGitCtx (CombinedOutput pipe goroutines wait for EOF;
the pre-receive-hook descendant inherits stderr fd 2 = pipe write-end)
does NOT apply — runShortGit uses `.Run()` not `.CombinedOutput()`,
which inherits the test process's stdout/stderr directly without
internal pipes. SIGKILL of the immediate `git push` child returns
`.Run()` immediately regardless of orphan descendants.

**Verifier-readable contract artifacts (per FIX-8F-PROMPT verification):**

```
$ grep -c 'SetEstop' internal/agents/astromech_estop_cancel_test.go
6
$ grep -c '2.*time\.Second\|time\.After(2' internal/agents/astromech_estop_cancel_test.go
2
$ grep -c 'runShortGit\|combinedShortGit\|RunTaskForeground' internal/agents/astromech_estop_cancel_test.go
6
$ grep -c 't\.Fatal\|t\.Fatalf' internal/agents/astromech_estop_cancel_test.go
9
```

**Naming discrepancy disclosed.** The FIX-8F-PROMPT spec referred to
`store.SetEstopped` but the actual production helper is `SetEstop`
(no -ped) in package agents — see `internal/agents/estop.go:45`.
The test calls the real helper directly (no package prefix needed
since the test lives in `package agents`). The grep above shows
6 hits for `SetEstop` — these include comments and the live calls.

**RGR red-phase demonstration.** Temporarily reverted `runShortGit`
in `internal/agents/astromech.go` to fabricate
`context.WithTimeout(context.Background(), shortGitTimeout)` and
re-ran the test:

```
=== RUN   TestAstromech_EstopCancelsInFlightGitOp
    astromech_estop_cancel_test.go:168: e-stop did not cancel in-flight git op within 2s — daemon ctx cancellation not propagating to subprocess (Fix #8e regression)
--- FAIL: TestAstromech_EstopCancelsInFlightGitOp (2.52s)
```

Production code restored before commit; `git diff internal/agents/astromech.go`
post-restore returned empty.

**Green-phase outcome.** With current production code,
`TestAstromech_EstopCancelsInFlightGitOp` passes at -race -count=5 in
0.55–0.58s per run (well under the 2s budget):

```
=== RUN   TestAstromech_EstopCancelsInFlightGitOp
    astromech_estop_cancel_test.go:166: git push terminated after e-stop (err: signal: killed)
--- PASS: TestAstromech_EstopCancelsInFlightGitOp (0.58s)
```

## Track B — Rewrite ctx-cancel tests + add TestRunShortGit_CtxCancel

**Files:**
- `internal/git/ctx_cancel_test.go` — REWRITTEN (was 71 lines, now 167).
- `internal/git/ctx_cancel_helper_init_test.go` — NEW (59 lines).
- `internal/agents/runshortgit_cancel_test.go` — NEW (130 lines).

**B1: Rewritten internal/git tests now call production helpers.**

Pre-Fix-#8f the tests inlined `exec.CommandContext(c, "sleep", "30")`
mirroring what the helpers' bodies looked like, but never called
`bestEffortRun` or `runGitCtx`. A regression in those helpers (e.g.
reverting to fabricated `context.Background`) would have left the
shape-mirror tests passing — the verifier flagged this as Defect #4.

Post-rewrite, the tests call the production symbols by name:

```go
// internal/git/ctx_cancel_test.go (excerpt)
go func() {
    bestEffortRun(ctx, "fix8f-cancel-test", "rev-parse", "HEAD")
    close(done)
}()
```

```go
// internal/git/ctx_cancel_test.go (excerpt)
go func() {
    out, err := runGitCtx(ctx, "rev-parse", "HEAD")
    done <- result{out: out, err: err}
}()
```

Verifier-readable confirmation:

```
$ grep -A 30 'func TestBestEffortRun_CtxCancelKillsSubprocess' internal/git/ctx_cancel_test.go | grep 'bestEffortRun('
		bestEffortRun(ctx, "fix8f-cancel-test", "rev-parse", "HEAD")
$ grep -A 30 'func TestRunGitCtx_CtxCancel' internal/git/ctx_cancel_test.go | grep 'runGitCtx('
		out, err := runGitCtx(ctx, "rev-parse", "HEAD")
```

**Slow-op fixture: slowgit shim.** Critical design choice. The original
attempt used a custom git remote helper (`git-remote-fix8fhang`) plus
`git ls-remote fix8fhang://hang`. That fixture failed at the 2s budget
for `runGitCtx` because:

1. Real `git ls-remote` fork+execs an intermediate `git remote-X`
   process that inherits the runGitCtx CombinedOutput pipe write-end
   via fd 2.
2. exec.Cmd's ctx-cancel only SIGKILLs the **immediate** child (top-level
   `git`); the intermediate `git remote-X` survives and holds the pipe
   write-end until its natural completion (~30s in our fixture).
3. CombinedOutput's read-goroutine then blocks for the full 30s —
   indistinguishable from the fabricated-Background regression case.

Empirically verified via `lsof` during a hung test run: the surviving
pipe holder was `git remote-fix8fhang` (the intermediate process), not
the leaf helper. Brute-force `syscall.Close(0)` through `syscall.Close(65535)`
in the leaf helper did NOT release the pipe — the intermediate process
held its independent fd 2.

The slowgit shim eliminates intermediate processes entirely:

- Symlink at `<tmpBin>/git` pointing back at the test binary.
- `t.Setenv("FIX8F_SLOWGIT_MODE", "true")`.
- `t.Setenv("PATH", tmpBin + ":" + os.Getenv("PATH"))`.
- Helper-mode `init()` in `internal/git/ctx_cancel_helper_init_test.go`
  detects the env var on subprocess startup (before testing's
  `flag.Parse()` would reject git's positional arguments) and
  `time.Sleep(30 * time.Second)`.

The slowgit IS the immediate child of runGitCtx's exec.Cmd. SIGKILL of
slowgit closes its inherited fd 1 / fd 2 (= runGitCtx CombinedOutput
pipe write-ends), the pipe has 0 remaining write-end references,
CombinedOutput's read-goroutine sees EOF, runGitCtx returns. No
grandchildren involved.

**RGR red-phase demonstration.** Temporarily reverted both
`bestEffortRun` and `runGitCtx` in `internal/git/git.go` to fabricate
`WithTimeout(Background, shortGitOpTimeout)`. Re-running:

```
=== RUN   TestBestEffortRun_CtxCancelKillsSubprocess
    ctx_cancel_test.go:127: bestEffortRun did not honor ctx cancellation within 2s — helper body regressed (likely fabricating context.Background)
--- FAIL: TestBestEffortRun_CtxCancelKillsSubprocess (2.25s)
=== RUN   TestRunGitCtx_CtxCancel
    ctx_cancel_test.go:167: runGitCtx did not honor ctx cancellation within 2s — helper body regressed (likely fabricating context.Background)
--- FAIL: TestRunGitCtx_CtxCancel (2.25s)
```

Production code restored before commit; `git diff internal/git/git.go`
post-restore returned empty.

**Green-phase outcome.** Both tests pass at -race -count=5 in 0.26s per
run.

**B2: New TestRunShortGit_CtxCancel in internal/agents.**

Spec-named per FIX-8E-PROMPT § Track B. Calls `runShortGit(ctx, "-C",
workDir, "push", "origin", "main")` against the same file:// remote +
slow pre-receive hook fixture used by Track A. Because runShortGit
uses `.Run()` (not `.CombinedOutput()`), descendant pipe inheritance
does not block return — the pre-receive-hook fixture is sufficient
here.

**RGR red-phase demonstration.** Temporarily reverted `runShortGit` in
`internal/agents/astromech.go` to fabricate Background:

```
=== RUN   TestRunShortGit_CtxCancel
    runshortgit_cancel_test.go:128: runShortGit did not honor ctx cancellation within 2s — helper body regressed (likely fabricating context.Background)
--- FAIL: TestRunShortGit_CtxCancel (2.54s)
```

Production code restored before commit; `git diff
internal/agents/astromech.go` post-restore returned empty.

**Green-phase outcome.** Test passes at -race -count=5 in 0.50–0.53s
per run.

## Track C — P1.1 window 60→10 + FIX-8E-CLOSURE.md arithmetic

**C1: P1.1 window tightened from 60 → 10 lines.**

`internal/audittools/audit_pattern_p1_1_rows_err_test.go:103-105`:

```diff
-			// Window: 60 lines past the close brace. Must reference iter.Err().
-			windowEnd := closeIdx + 60
+			// Window: 10 lines past the close brace. Must reference iter.Err().
+			// Tightened from 60 → 10 by Fix #8f Track C; the spec said "10
+			// lines" and the broader window was masking placement drift.
+			windowEnd := closeIdx + 10
```

The Fix #8e spec said ≤10 lines; 60 was permissive and risked masking
placement drift on future production loops. Tightened to 10. Test still
passes at -race -count=5: every production for-rows.Next() loop has its
rows.Err() check within 10 lines of the close brace, so the tighter
window catches no new offenders.

Per the Fix #8f anti-cheat directive #6: a real production loop with
rows.Err() more than 10 lines below the close would require fixing the
production site, not widening the window. None exist.

**C2: FIX-8E-CLOSURE.md arithmetic corrected (88 → 95, 16-loop delta → 9).**

The closure report's prose claimed "Total: 88 loops patched" but the
per-file table sums to 95. The substantive 100% coverage holds (104/104
production loops verified by the FIX-8E verifier). Only the bookkeeping
was wrong.

**Verification of the actual sum:**

```
1+1+9+2+11+3+2 = 29  (cmd/force/*)
1+1+2+1+1+1+1+1+2+2+2+5+1+2+1+1 = 25  (internal/agents/*)
8                                       (internal/dashboard/handlers.go)
3+4+3+3+2+2+5+5+6 = 33                  (internal/store/*)
Total: 29 + 25 + 8 + 33 = 95
```

Aligned prose to table:

```diff
-**Total: 88 loops patched. 100% coverage post-fix.** (The 16-loop delta
+**Total: 95 loops patched. 100% coverage post-fix.** (The 9-loop delta
 between the per-file totals here and the 104 grep count is loops that
 already had rows.Err() pre-fix — most notably the well-instrumented
-dogs.go:222 and dashboard handlers that already had it from prior fixes.)
+dogs.go:222 and dashboard handlers that already had it from prior fixes,
+plus the 4 RESIDUAL-3 sites the Fix #8d verifier identified as already
+log-with-named-recovery: holocron.go:115 ListRepos, escalation.go:102
+ListEscalations, convoy.go:94 RecoverStaleConvoys, print.go:60 printList.
+**Fix #8f Track C corrected this from "88 / 16-loop delta" to
+"95 / 9-loop delta" to match the per-file table sums.**)
```

Picked the table's number (95) over the prose's number (88) because
per-file granularity is harder to forge than a summary line, and the
table content matches what the Fix #8e Track E commit's diff actually
changed.

## Verification output

Pre-flight: zero fabricated Background in production:

```
$ grep -rnE 'context\.WithTimeout\(context\.Background\(\)' --include="*.go" internal/ cmd/ | grep -v '_test.go'
(no output)
```

Pre-flight: 0 live AUDIT skip markers (only comment-only references):

```
$ grep -rn 't\.Skip(.AUDIT-' --include="*.go" internal/ cmd/ schema/
internal/audittools/audittools_test.go:16:// remainingAuditSkips is the allowlist of AUDIT IDs whose `t.Skip("AUDIT-NNN:`
internal/audittools/audittools_test.go:55:// `t.Skip("AUDIT-NNN:` marker is present for an AUDIT ID that is NOT on
```

Track A artifacts present:

```
$ grep -rn 'TestAstromech_EstopCancelsInFlightGitOp' --include="*.go" internal/agents/
internal/agents/astromech_estop_cancel_test.go:99:// TestAstromech_EstopCancelsInFlightGitOp is the load-bearing integration
internal/agents/astromech_estop_cancel_test.go:109:func TestAstromech_EstopCancelsInFlightGitOp(t *testing.T) {

$ grep -c 'SetEstop' internal/agents/astromech_estop_cancel_test.go
6
$ grep -c '2.*time\.Second\|time\.After(2' internal/agents/astromech_estop_cancel_test.go
2
$ grep -c 'runShortGit\|combinedShortGit\|RunTaskForeground' internal/agents/astromech_estop_cancel_test.go
6
$ grep -c 't\.Fatal\|t\.Fatalf' internal/agents/astromech_estop_cancel_test.go
9
```

Track B artifacts present:

```
$ grep -rn 'TestRunShortGit_CtxCancel' --include="*.go" internal/agents/
internal/agents/runshortgit_cancel_test.go:93:func TestRunShortGit_CtxCancel(t *testing.T) {
(plus 3 comment references)

$ grep -A 30 'func TestBestEffortRun_CtxCancelKillsSubprocess' internal/git/ctx_cancel_test.go | grep 'bestEffortRun('
		bestEffortRun(ctx, "fix8f-cancel-test", "rev-parse", "HEAD")

$ grep -A 30 'func TestRunGitCtx_CtxCancel' internal/git/ctx_cancel_test.go | grep 'runGitCtx('
		out, err := runGitCtx(ctx, "rev-parse", "HEAD")
```

Track C artifacts present:

```
$ grep -n 'closeIdx + ' internal/audittools/audit_pattern_p1_1_rows_err_test.go
105:			windowEnd := closeIdx + 10
```

Targeted Fix #8f tests at -race -count=5:

```
$ go test -tags sqlite_fts5 -run 'TestAstromech_EstopCancelsInFlightGitOp|TestBestEffortRun_CtxCancelKillsSubprocess|TestRunGitCtx_CtxCancel|TestRunShortGit_CtxCancel|TestPattern_P1_1' -race -count=5 ./internal/agents/ ./internal/git/ ./internal/audittools/
ok  	force-orchestrator/internal/agents	8.257s
ok  	force-orchestrator/internal/git	5.021s
ok  	force-orchestrator/internal/audittools	2.282s
```

Full suite at -race -count=5: see "Full suite output" section below.

## Full suite output

```
$ go test -tags sqlite_fts5 -race -count=5 -timeout 1800s ./...
ok  	force-orchestrator/cmd/force	26.475s
ok  	force-orchestrator/internal/agents	1249.772s
ok  	force-orchestrator/internal/audittools	6.911s
ok  	force-orchestrator/internal/claude	9.414s
ok  	force-orchestrator/internal/dashboard	5.953s
ok  	force-orchestrator/internal/gh	2.992s
ok  	force-orchestrator/internal/git	111.976s
ok  	force-orchestrator/internal/store	16.216s
ok  	force-orchestrator/internal/telemetry	3.701s
?   	force-orchestrator/internal/util	[no test files]
```

9/9 packages green at -race -count=5. Zero FAIL lines, no race-detector
hits, no flakes across 5 trials. Total wall clock: ~22 minutes.
(`internal/util` has no tests; expected.)

```
$ make smoke
ok  	force-orchestrator/internal/agents	0.768s
ok  	force-orchestrator/internal/dashboard	1.313s
ok  	force-orchestrator/internal/git	2.557s
(other packages: [no tests to run])
```

```
$ make fuzz
PASS
ok  	force-orchestrator/internal/agents	31.716s
(plus the 4 baseline targets at 295,309 execs, 0 crashers)
```

```
$ make test-audit
go test -tags sqlite_fts5 -timeout 60s -run '^TestNoAuditSkipMarkersRemain$' -count=1 ./internal/audittools
ok  	force-orchestrator/internal/audittools	0.377s
```

## Re-run of FIX-8E verifier

A fresh sub-agent was given the FIX-8E-PROMPT verification brief
(`/Users/jake.herman/code/force-orchestrator/FIX-8E-PROMPT.md` lines
196-264) and asked to verify against the post-Fix-#8f state of `main`.
The agent had no prior context about Fix #8e or Fix #8f.

Verifier output (verbatim):

```
# FIX-8E Re-Verification (post-Fix #8f)

## Verdict: GO

## Per-check results

1. **Zero fabricated `context.WithTimeout(context.Background(),...)` in
   production** — PASS. `grep -rnE
   'context\.WithTimeout\(context\.Background\(\)' --include="*.go"
   internal/ cmd/ | grep -v '_test.go'` returns 0 hits.

2. **Targeted Track A/B/C/D/E tests at -race -count=5** — PASS:
   - `TestBestEffortRun_CtxCancel` ./internal/git/...: ok 2.913s
   - `TestRunGitCtx_CtxCancel` ./internal/git/...: ok 2.621s
   - `TestRunShortGit_CtxCancel` ./internal/agents/...: ok 4.123s
   - `TestAstromech_EstopCancelsInFlightGitOp` ./internal/agents/...: ok 4.641s
   - `TestPattern_P11_*` ./internal/audittools/...: ok 3.078s
   - `TestPattern_P1_1_RowsErrCheckedAfterIteration` ./internal/audittools/...: ok 1.438s

3. **Test body shapes call production helpers by name** — PASS:
   - `TestBestEffortRun_CtxCancelKillsSubprocess` body contains
     `bestEffortRun(ctx, "fix8f-cancel-test", "rev-parse", "HEAD")` —
     production helper, not `exec.CommandContext` shape-mirror.
   - `TestRunGitCtx_CtxCancel` body contains `runGitCtx(ctx, "rev-parse",
     "HEAD")` — production helper.

4. **Integration test contract** — PASS:
   - `TestAstromech_EstopCancelsInFlightGitOp` exists at
     `internal/agents/astromech_estop_cancel_test.go:109`.
   - SetEstop count: 6, 2s budget count: 2, production helper
     (runShortGit) count: 6, t.Fatal count: 9.
   - Body inspected: calls `runShortGit(ctx, "-C", workDir, "push",
     "origin", "main")`, calls `SetEstop(db, true)`, asserts via
     `time.After(2 * time.Second)` with `t.Fatal` on miss.

5. **P1.1 window tightened to 10** — PASS.
   `internal/audittools/audit_pattern_p1_1_rows_err_test.go:105` shows
   `windowEnd := closeIdx + 10` (not 60).

6. **`remainingAuditSkips` empty** — PASS. Map declaration contains only
   comment lines, no entries.

7. **FIX-8E-CLOSURE.md arithmetic** — PASS. Line 165: "Total: 95 loops
   patched. 100% coverage post-fix." Lines 172-173 explicitly note Track
   C corrected from "88 / 16-loop delta" to "95 / 9-loop delta."

8. **CLAUDE.md not downgraded** — PASS. Strengthened wording intact:
   "rows.Err() MUST also be checked... `_ = <iter>.Err()` silent discard
   is rejected"; "Two cheat shapes are rejected at the test layer
   regardless of allowlist: ... (fabricated parent — Fix #8d's gap, Fix
   #8e closure) and ... (direct disconnected ctx)." Regression test
   names `TestPattern_P11_FabricatedContextRejected`,
   `TestPattern_P11_AllowlistReasonsTruthful`,
   `TestPattern_P1_1_RowsErrCheckedAfterIteration` all referenced.

9. **No production code changes since 3aa8852** — PASS.
   `git diff 3aa8852..main -- ':!*_test.go' ':!FIX-8E-CLOSURE.md'
   ':!FIX-8F-CLOSURE.md'` is empty.

## Restart-blocker analysis

Zero restart-blockers. The five gaps the prior Fix #8e verifier flagged
are all closed:
- Restart-blocker (TestAstromech_EstopCancelsInFlightGitOp absent) →
  resolved by Track A.
- Defect #2 (TestRunShortGit_CtxCancel absent) → resolved by Track B.
- Defect #3 (88 vs 95 arithmetic) → resolved by Track C.
- Defect #4 (shape-mirror tests) → resolved by Track B (helpers called
  by name).
- Defect #5 (P1.1 window 60 vs 10) → resolved by Track C.

Track C's Claude/Dogs CtxCancel coverage gap is documented as a
non-blocker (peer coverage via P11 + helper tests is adequate per spec).

## Anti-cheat checks

- **Shape-mirror substitution** — NOT VIOLATED. Both rewritten tests
  call `bestEffortRun`/`runGitCtx`/`runShortGit` by name; integration
  test calls `runShortGit` + `SetEstop` by name.
- **Budget inflation (>2s)** — NOT VIOLATED. All four cancellation
  assertions use `time.After(2 * time.Second)` exactly.
- **t.Skip on slow-op fixture difficulty** — NOT VIOLATED. Skips in
  `TestAstromech_EstopCancelsInFlightGitOp` are limited to PATH lookups
  (git, sh, sleep) — environmental prerequisites, not difficulty
  avoidance.
- **Closure-report wallpapering** — NOT VIOLATED. FIX-8E-CLOSURE.md
  explicitly cross-references the prior incorrect figure ("Fix #8f
  Track C corrected this from '88 / 16-loop delta' to '95 / 9-loop
  delta'").
- **Production code drift** — NOT VIOLATED. Diff against 3aa8852
  excluding test files and the two closure docs is empty.

## Final verdict (re-stated)

**GO.**
```

## Anti-cheat self-check

| # | Directive | Status |
|---|---|---|
| 1 | No production code changes outside test files and FIX-8E-CLOSURE.md | PASS — `git diff main..HEAD -- ':!*_test.go' ':!FIX-8E-CLOSURE.md' ':!FIX-8F-CLOSURE.md'` returns no diff outside test/_test files and the one doc edit. Red-phase reverts to bestEffortRun, runGitCtx, runShortGit were restored before commit; post-restore git diff was empty in each case. |
| 2 | No 60-second test budgets | PASS — every test uses `time.After(2 * time.Second)`. The slowgit/hook fixtures sleep 30s but the test budget remains 2s; pre-fix runGitCtx blocked for the full 30s natural-completion (not the 2s budget), failing the test, while post-fix returned in 0.26s. |
| 3 | No shape-mirror substitution | PASS — `grep -A 30 'func Test...' internal/git/ctx_cancel_test.go` shows `bestEffortRun(ctx, ...)` and `runGitCtx(ctx, ...)` directly invoked. No `exec.CommandContext(c, ...)` substitutions remain. |
| 4 | No `t.Skip` to skirt the slow-op fixture | PASS — t.Skip lines exist only for genuine binary-not-in-PATH cases (git, sh, sleep). The slow-op fixtures (file:// remote with hook, slowgit shim) are reliable in CI; no fixture-difficulty skips. |
| 5 | No `t.Log`-on-miss | PASS — every cancellation-budget assertion uses `t.Fatal` / `t.Fatalf`. Track A test contains 9 `t.Fatal` references. |
| 6 | No P1.1 widening | PASS — Track C tightened from 60 → 10. Did not widen. No production loop required moving its rows.Err() check closer to its close. |
| 7 | No closure-report wallpapering | PASS — the table sums to 95 (verified by hand-computation in this report). Picked 95 (not 88) to match observable reality. The "Fix #8f Track C corrected this from 88/16 to 95/9" line in FIX-8E-CLOSURE.md surfaces the correction transparently. |
| 8 | No `--no-verify` on commits | PASS — all 3 track commits passed pre-commit hooks. No `--no-verify`, no `--amend` after hook failure. |

## Residual list

**Genuinely out-of-scope items only.**

### Residual #1 (out of scope) — `RunCLIStreaming` / `AskClaudeCLI` legacy entry points

Inherited from FIX-8E-CLOSURE.md Residual #1; explicitly out-of-scope
for Fix #8f per the FIX-8F-PROMPT mission statement ("Fix #8e's
production code is correct and must not be modified"). Threading ctx
through every LLM-invoking agent's `AskClaudeCLI` call site remains
a future campaign.

### Residual #2 (out of scope) — Chancellor SEQUENCE list-element validation + MERGE-target-existence

Inherited from FIX-8E-CLOSURE.md Residual #2. Out-of-scope for Fix #8f
per the same mission statement.

### Residual #3 (out of scope) — `_ = ctx` in pilot helpers

Inherited from FIX-8E-CLOSURE.md Residual #3. Out-of-scope.

## Restart gate

The terminal verdict is **GO** once the FIX-8E verifier re-run returns
GO with zero residuals on the items it flagged in the prior round.

Restart gate checks (per FIX-8F-PROMPT.md "Restart gate"):

| Check | Status |
|---|---|
| All three tracks merged to main | YES — see commit log |
| FIX-8F-CLOSURE.md filed | YES — this document |
| Re-run of FIX-8E verifier returns GO (not CONDITIONAL-GO, not NOT-DONE) | YES — see verifier output above |
| All Fix #8e closures still passing under -race -count=5 | YES — full 9-package suite green, 22-min wall clock, no flakes across 5 trials |
| All Fix #8d closures still passing under -race -count=5 | YES — TestPattern_P7_*, TestPattern_P1_*, TestPattern_P10, TestPattern_P11_* all in-suite green |

The campaign terminates in **GO**.
