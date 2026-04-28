# Fix #8e Verification Report

## Verdict: NOT-DONE

Fix #8e closes the **mechanical** gap that Fix #8d's verifier flagged: every
production `exec.CommandContext` first-arg now traces back syntactically to a
caller-supplied `ctx`, the six refactored helpers (`bestEffortRun`,
`runGitCtx`, `runGitCtxOutput`, `runShortGit`, `combinedShortGit`,
`combinedShortGitArgs`) wrap the passed ctx via `WithTimeout(ctx, T)` rather
than fabricating a fresh `context.Background()`-rooted parent, the rows.Err()
sweep is exhaustive (104/104 production iteration loops covered), CLAUDE.md
invariants are tightened (not weakened), and `TestPattern_P11_ExecCommandsUseContext`
is now a per-site walk that rejects both cheat shapes regardless of allowlist.
On every CODE-side check the campaign delivers what the spec asked for.

But the spec's **restart gate** explicitly enumerates a load-bearing test
artefact that does not exist in the tree:

> *"Integration test `TestAstromech_EstopCancelsInFlightGitOp` demonstrates
> e-stop cancels a running git op within 2 seconds."*

`grep -rn 'TestAstromech_EstopCancelsInFlightGitOp\|SetEstopped' --include="*_test.go" .`
returns ZERO matches. No test in the entire repo invokes `store.SetEstopped`,
no test exercises the operator-e-stop → astromech-claim-loop → in-flight-git-push
chain that this campaign exists to make cancellable. The closure report's
restart-gate row claims that `TestBestEffortRun_CtxCancelKillsSubprocess`
(internal/git/ctx_cancel_test.go) and `TestRunGitCtx_CtxCancel` substitute
for the integration test. They do not:

1. Both live in `package git`, exercise no astromech code, and do not call
   `store.SetEstopped`.
2. Reading the test bodies (`internal/git/ctx_cancel_test.go:21–47, 51–70`)
   reveals they do NOT call `bestEffortRun(...)` or `runGitCtx(...)` either —
   they inline-mirror the shape (`exec.CommandContext(c, "sleep", "30").Run()`)
   instead. A future refactor that breaks the production helper's body
   (e.g. reverting to `WithTimeout(context.Background(), T)`) would still
   leave these tests passing — they prove the author's asserted shape, not
   the helper's body.
3. The Fix #8e prompt's restart gate is unambiguous about the test name.
   "Substitute" is exactly the failure mode the spec was written to forbid.

This is a binary NOT-DONE per the verifier brief: *"integration test for
e-stop cancellation absent or trivially-passing → NOT-DONE."* All other
checks PASS or PASS-with-minor-doc-defects. Closing this single gap closes
the campaign. Specifically:

- Add `TestAstromech_EstopCancelsInFlightGitOp` in `internal/agents/` that
  spawns an astromech against a slow stub git remote, calls
  `store.SetEstopped(db, true)`, and asserts the in-flight git op errors
  within 2s. The test must call the production code path (e.g. via
  `runShortGit`/`combinedShortGit` in a slow-remote scenario), not mirror
  the shape inline.
- Add `TestRunShortGit_CtxCancel` in `internal/agents/` (spec-named,
  currently missing — peer test exists in internal/git but neither
  exercises the astromech helpers directly).
- Either add `TestBestEffortRun_CtxCancelKillsSubprocess` and
  `TestRunGitCtx_CtxCancel` bodies that actually call the helpers (so
  ghost-helper regressions surface at the test layer), or accept that
  Pattern P11's static check is the binding regression and document the
  shape-mirror tests as test-quality smoke (not contract enforcement).

Once the integration test ships green at `-race -count=5` with a ≤2s
assertion budget, the campaign closes on GO. There are no other
restart-blocker gaps.

## Independent verification output

Each numbered check from FIX-8E-PROMPT.md § "Verification procedure" plus
the broader 18-check Fix #8d/#8e procedure.

```
1. grep -rn 't\.Skip(.AUDIT-' --include="*.go" internal/ cmd/ schema/ \
    | grep -v "remainingAuditSkips\|audit_pattern_p1"
   → internal/audittools/audittools_test.go:55: comment-only reference
   → 0 live skip markers — PASS

2. remainingAuditSkips empty                                 → PASS
   (verified at internal/audittools/audittools_test.go:34–53; map body
    contains only comment lines)

3. FIX-8E-CLOSURE.md exists with all 8 required sections     → PASS
   (verdict, per-track summary, 18-site migration table, allowlist audit,
    rows.Err() sweep table, verification output, anti-cheat self-check,
    CLAUDE.md updates, residual list — present)

4. _ = store.* with marker check (Fix #8b)                   → PASS (no
   regression observed; out of Fix #8e scope, included for completeness)

5. Bare terminator grep                                      → PASS
   (out of Fix #8e scope, no regressions observed)

6. UpdateBountyStatusFrom defined and used                   → PASS
   (out of Fix #8e scope)

7. go test -tags sqlite_fts5 ./...                            → covered
   by check 8 below (-race -count=5 is the stronger superset).

8. go test -tags sqlite_fts5 -race -count=5 ./...             → PASS
   Final suite outcome (exit code 0, ~28 min wall clock):
     ok  force-orchestrator/cmd/force            60.377s
     ok  force-orchestrator/internal/agents      1444.817s
     ok  force-orchestrator/internal/audittools  9.659s
     ok  force-orchestrator/internal/claude      11.206s
     ok  force-orchestrator/internal/dashboard   12.022s
     ok  force-orchestrator/internal/gh          3.862s
     ok  force-orchestrator/internal/git         149.149s
     ok  force-orchestrator/internal/store       16.691s
     ok  force-orchestrator/internal/telemetry   3.126s
     ?   force-orchestrator/internal/util        [no test files]
   No FAIL, no race-detector hits, no flake at -count=5. The build-only
   `ld: warning: malformed LC_DYSYMTAB` lines are pre-existing macOS
   linker warnings on .test artefacts and unrelated to Fix #8e.

9. TestPattern_P7 race-count=5                               → not run
   directly (out of Fix #8e scope, included in the full suite at #8;
   no regression observed)

10. All Pattern_P{1,2,3,4,6,7,8,9,10,11,12} race-count=5     → PASS
    (TestPattern_P11_ExecCommandsUseContext, TestPattern_P11_FabricatedContextRejected,
     TestPattern_P11_AllowlistReasonsTruthful, TestPattern_P1_1_RowsErrCheckedAfterIteration
     all green at -race -count=5; full Pattern_ run completed in 3.294s)

11. make smoke                                               → PASS
    ok force-orchestrator/internal/agents 0.698s
    ok force-orchestrator/internal/dashboard 2.605s
    ok force-orchestrator/internal/git 1.955s

12. make fuzz                                                → PASS
    ok force-orchestrator/internal/agents 31.648s (FuzzCaptainJSONDecode)
    ok force-orchestrator/internal/agents 31.675s (FuzzConvoyReviewJSONDecode)
    plus the 4 baseline targets earlier (388,351 + 295,025 execs, 0 crashers)

13. make test-audit                                          → PASS
    ok force-orchestrator/internal/audittools 0.413s

14. grep -rnE 'context\.WithTimeout\(context\.Background\(\)' --include="*.go" \
       internal/ cmd/ | grep -v '_test.go'                   → 0 hits — PASS
    (Test-file hits: internal/dashboard/security_test.go:299 is a legitimate
     test-side timeout; internal/audittools/audit_pattern_p11_exec_context_test.go:99,150,209
     are documentation/fixture strings — all expected, all in test scope.)

15. TestAstromech_EstopCancelsInFlightGitOp at -race -count=5 ≤ 2s         → FAIL
    grep -rn 'TestAstromech_EstopCancelsInFlightGitOp' --include="*.go" .
    → 0 hits. The spec-named integration test is absent.

16. TestPattern_P11_FabricatedContextRejected                → PASS
    ok force-orchestrator/internal/audittools 1.941s

17. TestPattern_P1_1_RowsErrCheckedAfterIteration            → PASS
    ok force-orchestrator/internal/audittools 1.434s

18. Every for rows.Next() loop in production has post-loop rows.Err() check
    → PASS (104/104 covered; verified by Pattern P1.1 walking the tree
       exhaustively without an allowlist; sub-agent independently sampled
       15 sites and 4 RESIDUAL-3 sites, all checked)
```

Restart-blocker: **#15 fails**. All other checks pass (or pass with minor
documentation defects, see Forensic appendix).

## Per-track verification

### Track A — `internal/git/` ctx threading

| Check | Status | Evidence |
|---|---|---|
| Helper signatures accept ctx | PASS | `bestEffortRun` git.go:62, `runGitCtx` git.go:75, `runGitCtxOutput` git.go:83, `abortOp` git.go:93 — all `(ctx context.Context, …)` first-param. |
| Helper bodies use passed ctx | PASS | Each body: `ctx, cancel := context.WithTimeout(ctx, shortGitOpTimeout); exec.CommandContext(ctx, "git", args...)`. WithTimeout wraps PASSED ctx, not Background. |
| All call sites pass ctx | PASS | ~70 in-package calls (git.go + askbranch.go); every caller threads ctx. External-caller spot-check (5 sampled) all pass daemon-derived ctx. |
| Cancellation tests at -race -count=5 | PASS | ok internal/git 2.878s — TestBestEffortRun_CtxCancelKillsSubprocess + TestRunGitCtx_CtxCancel both at 2-second budget, t.Fatal on miss. |
| TestRunGitCtxOutput_CtxCancel spec-name coverage | GAP | Test by that exact name absent; runGitCtxOutput is structurally identical to runGitCtx (only .Output() vs .CombinedOutput() differs), so the contract is transitively covered, but the spec-named test is missing. Non-fatal alone. |
| Zero fabricated Background in internal/git/ | PASS | grep returns 0 hits in production files. |
| Zero context.TODO() in internal/git/ | PASS | grep returns 0 hits. |
| Test quality (1–5) | 3/5 | Tests are "shape-mirror" — they do NOT call bestEffortRun/runGitCtx directly. They inline `exec.CommandContext(c, "sleep", "30").Run()` mirroring what the helper body looks like. A regression in the helper body would not surface here. Pattern P11's static check IS the binding regression for that scenario. Test name is misleading; the test exercises shape, not symbol. |

**Cheats observed:** Test-quality concern — the cancellation tests do not
exercise the production symbols. Track A's contract IS enforced (by
Pattern P11), but the dynamic tests labelled with the helper names do not
call the helpers. This is a softer form of "ghost test" but does not
defeat the campaign because P11 catches the static regression.

**Verdict: ACCEPTED** (with noted test-quality concern; not a restart blocker
because Pattern P11 carries the contract and the helper bodies were
independently audited.)

### Track B — `internal/agents/astromech.go` ctx threading

| Check | Status | Evidence |
|---|---|---|
| Helper signatures accept ctx | PASS | `runShortGit` astromech.go:38, `combinedShortGit` :47, `combinedShortGitArgs` :56 — all `(ctx context.Context, …)`. |
| Helper bodies use passed ctx | PASS | L41/50/59 `exec.CommandContext(ctx, "git", args...)` preceded by `ctx, cancel := context.WithTimeout(ctx, shortGitTimeout)`. |
| astromech.go:1063 inline site fixed | PASS | RunTaskForeground L995 takes ctx; L1086 `sessionCtx, cancel := context.WithTimeout(ctx, sessionTimeout)`; L1090 `exec.CommandContext(sessionCtx, "claude", …)`. cmd/force/main.go:432 callers thread the CLI signal-cancel ctx. |
| All callers pass ctx | PASS | 12 in-tree call sites of the helpers; every one passes `ctx` as first arg. |
| **TestAstromech_EstopCancelsInFlightGitOp exists** | **FAIL** | grep returns 0 hits in the entire repo. Spec-named integration test absent. |
| TestRunShortGit_CtxCancel exists | FAIL | grep returns 0 hits. |
| Closure-report substitute legitimacy | **CHEAT** | The closure report's restart-gate row claims `TestBestEffortRun_CtxCancelKillsSubprocess` + `TestRunGitCtx_CtxCancel` satisfy the integration-test requirement. They do not: (1) live in package git not agents, (2) do not call astromech helpers, (3) do not call `store.SetEstopped`, (4) do not exercise the e-stop → astromech → subprocess chain. The spec's restart-gate language was written precisely to forbid this substitution. |
| Tests run green at -race -count=5 | PASS-by-vacuum | `go test -run 'TestRunShortGit_CtxCancel\|TestAstromech_EstopCancelsInFlightGitOp' ./internal/agents/...` returns "no tests to run". Green by absence, not by passing. |
| Zero fabricated Background in astromech.go | PASS | grep returns 0 hits. |
| SpawnAstromech ctx threading | PASS | L326 `func SpawnAstromech(ctx context.Context, db *sql.DB, name string)`; ctx threads to runAstromechTask → RunTaskForeground → sessionCtx. Daemon SIGINT/SIGTERM cancellation via cmd/force/main.go:432 propagates end-to-end, in principle. No test demonstrates this in practice. |

**Cheats observed:**
1. Spec-named `TestAstromech_EstopCancelsInFlightGitOp` and `TestRunShortGit_CtxCancel` missing.
2. Closure-report substitute (helper-level shape-mirror tests in a different
   package) is not a legitimate replacement per spec.

**Verdict: REJECTED.**

### Track C — Remaining inline sites

| Check | Status | Evidence |
|---|---|---|
| claude/claude.go:245 migrated | PASS | defaultCLIRunner accepts `parentCtx context.Context`; L252 `ctx, cancel := context.WithTimeout(parentCtx, timeout)`; L259 `exec.CommandContext(ctx, "claude", args...)`. |
| dogs.go:128 migrated | PASS | RunDogs L101 `func RunDogs(ctx context.Context, ...)`; L137 `dogCtx, dogCancel := context.WithTimeout(ctx, 5*time.Minute)`. |
| askbranch.go:162 migrated | PASS | RebaseBranchOnto L152 takes ctx; L172 calls `bestEffortRun(ctx, ...)` (Track A helper). |
| askbranch.go:343 migrated | PASS | MergeWithUnionStrategy L246 takes ctx; L355 `mergeCtx, mergeCancel := context.WithTimeout(ctx, shortGitOpTimeout)`. |
| askbranch.go:437 migrated | PASS | TriggerCIRerun L425 takes ctx; L455 `ctCtx, ctCancel := context.WithTimeout(ctx, shortGitOpTimeout)`. |
| Caller chains resolve to daemon ctx | PASS | claude defaultCLIRunner: caller is RunCLI/RunCLIStreamingContext/AskClaudeCLIContext — ctx-aware variants thread daemon ctx. dogs RunDogs: inquisitor.go:145 passes inquisitor tick ctx (daemon-derived). askbranch: pilot_rebase_agent.go:113, pilot_rebase.go:142,153, diplomat.go:257, pr_flow.go:698 — all pass agent-claim ctx. |
| claude.go Background retention legitimate | LEGITIMATE | claude.go:319-320 and :455-456 carry the spec-mandated `// context.Background intentional: legacy non-daemon entry-point with no caller-supplied ctx` comment; ctx-aware variants `RunCLIStreamingContext` (L333) and `AskClaudeCLIContext` (L463) defined. Per FIX-8E-PROMPT.md anti-cheat directive 6, comment-marked Background is acceptable for legitimate no-ctx-available callers. |
| TestClaudeCLI_CtxCancel / TestDogs_CtxCancel exist | GAP | Neither named test exists; peer coverage via P11 (static enforcement) and internal/git/ctx_cancel_test.go (helper shape) is adequate but not what spec named. |
| Zero fabricated Background in Track C files | PASS | grep returns 0 hits. |

**Cheats observed:** None at the code level. Two named tests missing
(GAP). Production agent callers (Captain/Medic/Jedi/etc.) still use
`AskClaudeCLI` (no-ctx form) rather than `AskClaudeCLIContext` — the
runner has the infrastructure but adoption is partial. Closure-report
explicitly scopes this as out-of-Track-C; spec's Track C scope was
specifically the `claude.go:245` runner site, which IS migrated.

**Verdict: ACCEPTED.**

### Track D — Pattern P11 tightening + allowlist rewrite

| Check | Status | Evidence |
|---|---|---|
| Per-site (not ratio) assertion | PASS | audit_pattern_p11_exec_context_test.go L105-193 walks every production *.go file, runs two cheat-shape regexes line-by-line (rejected EVERYWHERE no allowlist exemption), then runs `bareCmdRe` line-by-line on non-allowlisted files. Final assertion: `if len(offenders) == 0 { return }` else `t.Errorf(...)`. The only `total <= totalCtx*2` text is in a doc comment at L85 explaining what was removed. |
| TestPattern_P11_FabricatedContextRejected real | PASS | L201-241: 5 table-driven cases proving the regex flags both cheat shapes and accepts ctx_var / wrapped_caller_ctx / bare exec.Command shapes correctly. |
| TestPattern_P11_AllowlistReasonsTruthful real | PASS | L249-291: walks every entry in shortExecAllowlist, asserts the reason contains at least one descriptor from a hardcoded keyword list (push/fetch/ls-remote/clone/sub-second/dog-level/runner-layer/etc.). Length-only check would be a CHEAT; this is keyword-based, not length-based. |
| pr_flow.go:64,186 migrated | PASS | `internal/agents/pr_flow.go` NOT in allowlist. L71 `exec.CommandContext(ctx, "git", "-C", repo.LocalPath, "push", ...)` and L196 same — both use daemon ctx. |
| pilot_worktree_reset.go:115 migrated | PASS | NOT in allowlist. L123 `exec.CommandContext(ctx, "git", ..., "fetch", "origin", ...)` uses daemon ctx. |
| pilot_repo_config.go:152 migrated | PASS | NOT in allowlist. L162 `exec.CommandContext(ctx, "git", ..., "ls-remote", ...)` uses daemon ctx. |
| Allowlist size delta | PASS — shrunk | 13 → 10 entries (closure-claim verified by visual count of allowlist body L31-69). Three claimed-dropped entries (pr_flow.go, pilot_worktree_reset.go, pilot_repo_config.go) absent. Size shrank, did not grow. |
| Surviving entries truthful | PASS per entry | All 10 surviving entries contain ≥1 truthful descriptor (CLI/sub-second/local-only/ExecRunner/comment-only/preflight/etc.). Test enforces this on every run. |
| P11 tests green at -race -count=5 | PASS | TestPattern_P11_* at internal/audittools/ green in 3.272s. |
| Fixture-rejection reasoning | PASS | A file containing the cheat shape placed anywhere in production tree would: (1) be visited by WalkDir, (2) match fabricatedCtxRe at L147, (3) append to offenders regardless of allowlist (cheat-shape check runs BEFORE allowlist gate at L162), (4) trigger t.Errorf at L188. |
| No hidden ratio | PASS | grep for `>=`/`<=`/`>`/`<`/`*2`/`totalCtx`: only matches are doc-comment historical reference and sort.Slice ordering. No live arithmetic. |

**Cheats observed:** None.

**Verdict: ACCEPTED.**

### Track E — `rows.Err()` exhaustive sweep

| Check | Status | Evidence |
|---|---|---|
| Total for-rows.Next() loops in prod | 104 | Matches closure-claim. Narrower for-rows.Next() count: 83. |
| 4 RESIDUAL-3 named sites compliant | PASS each | holocron.go:115 (ListRepos): rows.Err() logged at L129-131 with named recovery. escalation.go:102 (ListEscalations): same shape at L110-112. convoy.go:94 (RecoverStaleConvoys): same shape at L102-104. print.go:60 (printList): same shape at L82-84. All are LOGGED WITH NAMED RECOVERY (file:func: rows iter error). |
| 15 sampled production loops compliant | 15/15 PASS | Independent sub-agent sample of holocron:231, tasks:1223, proposed_convoy:106, pilot_draft_watch:232, dashboard handlers:1166, escalation:155, feature_blockers:154, proposed_convoy:127, holocron:115, feature_blockers:48, fleet_mail:45, proposed_convoy:191, digest:106, tasks:1275, auditor:378 — all checked, RETURNED or LOGGED-WITH-RECOVERY shape. |
| P1.1 test walks production exhaustively | PASS | audit_pattern_p1_1_rows_err_test.go: filepath.WalkDir at L52 with vendor/.git/.force-worktrees/.build-worktrees/node_modules/testdata excluded; regex matches any iterator name (`forNextRe = ^(\s*)for\s+(\w+)\.Next\(\)\s*\{\s*$`). NO ALLOWLIST. |
| Zero silent-discard `_ = <iter>.Err()` in prod | PASS | grep returns 0 hits. P1.1 actively rejects the form (L116-126). |
| Zero statement-discard `<iter>.Err()` (bare expression) in prod | PASS | grep returns 0 hits. |
| P1.1 green at -race -count=5 | PASS | ok internal/audittools 1.434s. |
| CLAUDE.md wording strengthened | PASS | "rows.Err() is checked after the iteration" → "rows.Err() MUST also be checked after the iteration — Fix #8e closed the prior coverage gap so every `for <iter>.Next()` loop in production now has a meaningful `<iter>.Err()` observation". RFC2119 MUST. Strengthened. |
| Sweep-table arithmetic | INCONSISTENT | Closure-report claims "Total: 88 patched" but per-file table sums to 95. Substantive 104/104 coverage holds; bookkeeping number is wrong. Documentation defect, not a correctness defect. |
| Test quality (1–5) | 4/5 | Strengths: walks every prod *.go, no allowlist, rejects silent-discard, regex captures iter name and matches on the captured name. Weaknesses: 60-line window (spec said ≤10); presence-only check (a bare `rows.Err()` expression-statement that ignores its return value would pass — but Track E independently verified zero such cases exist). Adequate as written. |

**Cheats observed:** Closure-report arithmetic mistake (88 ≠ 95). Not a
code-side cheat — substantive coverage is real.

**Verdict: ACCEPTED.**

## Universal anti-cheat verification

| # | Check | Status | Evidence |
|---|---|---|---|
| 1 | Zero fabricated `context.WithTimeout(context.Background(), …)` in production | PASS | `grep -rnE 'context\.WithTimeout\(context\.Background\(\)' --include="*.go" internal/ cmd/ \| grep -v '_test.go'` → 0 hits. |
| 2 | Zero `context.TODO()` in production | PASS | `grep -rnE 'context\.TODO\(' --include="*.go" internal/ cmd/ \| grep -v '_test.go'` → 0 hits. |
| 3 | Every `exec.CommandContext` first-arg traces to caller-supplied ctx | PASS | Pattern P11 enforces this per-site walking the AST/regex equivalent. Verified by sub-agent sampling. |
| 4 | Pattern P11 asserts per-site, not ratio | PASS | Reviewed audit_pattern_p11_exec_context_test.go body; offender-list build, no ratio comparisons. |
| 5 | P1.1 walks production exhaustively, no allowlist | PASS | Reviewed audit_pattern_p1_1_rows_err_test.go body; line 37-38 explicit "this test does NOT carry an allowlist." |
| 6 | Zero `_ = rows.Err()` / `_ = iter.Err()` in production | PASS | grep returns 0 hits with any iterator name. |
| 7 | CLAUDE.md invariants strengthened, not weakened | PASS | Diff vs commit 2d0a004 (pre-Fix-8e): "is checked" → "MUST also be checked" + "MUST trace back" + named regression tests. RFC2119 MUST language added. No softening verbs. |
| 8 | Integration test `TestAstromech_EstopCancelsInFlightGitOp` exists and asserts cancellation within 2s | **FAIL** | Test does not exist anywhere in the repo. Closure-report substitute (helper-level shape-mirror tests in package git) does not exercise the e-stop → astromech → subprocess chain and does not call `store.SetEstopped`. |
| 9 | No regression of Fix #8d closures | PASS | remainingAuditSkips still empty; AUDIT-026/027/072/090/091/094/095/100/127/156/158/159/165 all carry no live skip markers. Full -race -count=5 suite green across all 9 testable packages; no regressions surfaced. |

**Anti-cheat #8 fails — restart blocker.**

## Pattern closure (P1.1, P11 emphasis; P1–P12 regression check)

| Pattern | Status | Evidence |
|---|---|---|
| P1 (rows.Scan) | PASS | TestPattern_P1_RowsScanErrorsChecked green; no skip markers. |
| **P1.1 (rows.Err)** | PASS | TestPattern_P1_1_RowsErrCheckedAfterIteration green at -race -count=5 in 1.434s; walks production exhaustively without allowlist. |
| P2 | PASS | (regression check; no Fix #8e change) |
| P3 (idempotent inserts) | PASS | (regression check) |
| P4 | PASS | (regression check) |
| P6 | PASS | (regression check) |
| P7 (state-transition) | PASS | (regression check; Fix #8d) |
| P8 (dashboard) | PASS | (regression check) |
| P9 | PASS | (regression check) |
| P10 (-- separator) | PASS | (regression check) |
| **P11 (exec.CommandContext)** | PASS | per-site enforcement, both cheat shapes rejected, allowlist truthfulness asserted, fabricated-context fixture rejection green at -race -count=5. |
| P12 (LLM boundary) | PASS | (regression check; Fix #8.5) |

## Common-cheat audit (Fix #8e-specific)

| # | Cheat shape | Status | Evidence |
|---|---|---|---|
| 1 | Helper accepts ctx but body ignores it | NONE OBSERVED | Every helper body uses `WithTimeout(ctx, T)` wrapping the parameter; bodies audited per-site. |
| 2 | Caller-side `context.Background()` introduced where there was a usable ctx | NONE OBSERVED | Spot-check + grep across all callers found no silent Background fallback. |
| 3 | `context.TODO()` placeholder | NONE OBSERVED | grep returns 0 hits. |
| 4 | P11 ratio assertion replaced with another ratio | NONE OBSERVED | Test body does not contain ratio comparisons. |
| 5 | P11 allowlist bloated | NONE — shrunk | 13 → 10. The 3 mislabeled entries dropped. |
| 6 | Allowlist reason rewritten honestly but still allows network op without cancellation mechanism | NONE OBSERVED | All 10 surviving entries name a cancellation mechanism (CLI/Ctrl-C/sub-second/runner-layer/dog-level) or are comment-only. |
| 7 | `_ = rows.Err()` in production | NONE OBSERVED | grep returns 0 hits. |
| 8 | `rows.Err()` as bare expression statement | NONE OBSERVED | grep returns 0 hits. |
| 9 | CLAUDE.md wording softened | NONE — strengthened | Diff shows MUST added, regression-test names added, no removal of contracts. |
| 10 | Integration test budget set to mask the fix | N/A — TEST ABSENT | The test the spec required (TestAstromech_EstopCancelsInFlightGitOp) does not exist; cannot evaluate budget. The shape-mirror tests use 2s budget — appropriate for what they test, but they do not test the integration the spec asked for. |
| 11 | Test fixtures excluded from sweep via build tags or directory-skip to hide forbidden patterns | NONE OBSERVED | Skip-dirs are vendor/.git/.force-worktrees/.build-worktrees/node_modules/testdata — standard exclusions; no production code is under them. |

Cheat #10 is the one that bites: the spec named the integration test by
name; the closure report substituted a helper-level shape-mirror test in
a different package. Per the spec's restart-gate language, this is the
NOT-DONE.

## Test quality assessment

10 sampled tests new/modified by Fix #8e:

| # | Test | File | Score (1–5) | Notes |
|---|---|---|---|---|
| 1 | TestBestEffortRun_CtxCancelKillsSubprocess | internal/git/ctx_cancel_test.go | 3 | 2s budget, t.Fatal on miss — but shape-mirror, does NOT call bestEffortRun. Pattern P11 carries the static contract. |
| 2 | TestRunGitCtx_CtxCancel | internal/git/ctx_cancel_test.go | 3 | Same as #1; shape-mirror. |
| 3 | TestPattern_P11_ExecCommandsUseContext | internal/audittools/ | 5 | Per-site exhaustive walk, no allowlist on cheat-shapes, t.Errorf with file:line. |
| 4 | TestPattern_P11_FabricatedContextRejected | internal/audittools/ | 5 | Table-driven proof of regex correctness over 5 cases, both want=true and want=false. |
| 5 | TestPattern_P11_AllowlistReasonsTruthful | internal/audittools/ | 4 | Keyword-based, descriptor list defensible, list could be tighter (CLI/local-only are loose). |
| 6 | TestPattern_P1_1_RowsErrCheckedAfterIteration | internal/audittools/ | 4 | Walks production exhaustively, rejects silent-discard, no allowlist. 60-line window is permissive vs spec's 10-line. Doesn't catch bare-expression `rows.Err()` (but none exist). |
| 7 | TestNoAuditSkipMarkersRemain | internal/audittools/audittools_test.go | 5 | (regression check from prior campaigns) |
| 8 | (Fix #8d cancel-related test) | internal/agents/ | (no new test) | TestAstromech_EstopCancelsInFlightGitOp absent. |
| 9 | TestRunShortGit_CtxCancel | internal/agents/ | (absent) | Spec-named, missing. |
| 10 | TestClaudeCLI_CtxCancel / TestDogs_CtxCancel | various | (absent) | Spec-named, missing. |

**Average of present tests: 4.1 / 5.** Above the 4.0 threshold.

Test-quality regression specifically: the cancellation tests (#1, #2) are
shape-mirror and do not exercise the production symbols. Pattern P11
catches the static regression that would matter most. The dynamic-test
weakness is a follow-up note, not a NOT-DONE on its own. **The NOT-DONE
is driven by the absence of #8 (TestAstromech_EstopCancelsInFlightGitOp),
not by test quality.**

## CLAUDE.md diff review

Pre-Fix-8e (commit 2d0a004):
> "Every `rows.Scan(...)` in production code MUST check the error … `rows.Err()` is checked after the iteration. Test files are exempt. `TestPattern_P1_RowsScanErrorsChecked` is the grep-based regression."

Post-Fix-8e (HEAD):
> "Every `rows.Scan(...)` in production code MUST check the error … **`rows.Err()` MUST also be checked after the iteration — Fix #8e closed the prior coverage gap so every `for <iter>.Next()` loop in production now has a meaningful `<iter>.Err()` observation (returned, logged with named recovery, or error-wrapped). `_ = <iter>.Err()` silent discard is rejected by the regression test.** Test files are exempt. `TestPattern_P1_RowsScanErrorsChecked` (Scan check) and `TestPattern_P1_1_RowsErrCheckedAfterIteration` (Err check) are the grep-based regressions; neither carries an allowlist."

Strengthened: "is checked" → "MUST also be checked"; named regression test
added; no-allowlist contract added; silent-discard explicitly rejected.

Pre-Fix-8e exec.CommandContext invariant:
> "Long-running subprocess invocations … MUST use `exec.CommandContext(ctx, ...)` so daemon shutdown / e-stop can cancel them. Short lookups … may stay as `exec.Command` when the caller holds no context. `TestPattern_P11_ExecCommandsUseContext` is the grep-based regression with an explicit short-command allowlist."

Post-Fix-8e:
> "Long-running subprocess invocations … MUST use `exec.CommandContext(ctx, ...)` so daemon shutdown / e-stop can cancel them. **The ctx MUST trace back (syntactically) to a caller-supplied parameter, field, or local derived from one. Two cheat shapes are rejected at the test layer regardless of allowlist: `exec.CommandContext(context.WithTimeout(context.Background(), …), …)` (fabricated parent — Fix #8d's gap, Fix #8e closure) and `exec.CommandContext(context.Background(), …)` (direct disconnected ctx).** … `TestPattern_P11_ExecCommandsUseContext` is the per-site regression (Fix #8e replaced the prior ratio assertion); `TestPattern_P11_FabricatedContextRejected` and `TestPattern_P11_AllowlistReasonsTruthful` pin the cheat-shape and allowlist-truthfulness invariants. **Internal `internal/git/` helpers (`bestEffortRun`, `runGitCtx`, `runGitCtxOutput`, `abortOp`) and astromech helpers (`runShortGit`, `combinedShortGit`, `combinedShortGitArgs`) all accept `ctx context.Context` as the first parameter; their internal `WithTimeout` wraps the passed ctx, NOT `Background()`.**"

Strengthened in three ways: (1) ctx-trace-syntactic requirement added,
(2) two cheat shapes named and rejected, (3) the six refactored helpers
named with their contract. No softening.

**No CLAUDE.md downgrade.**

## Forensic appendix

### Failure 1 — TestAstromech_EstopCancelsInFlightGitOp absent

**Cited spec lines:** FIX-8E-PROMPT.md line 120 (Track B test scope), line 207
(verification procedure), line 258 (restart-gate enumeration).

**Closure-report claim** (FIX-8E-CLOSURE.md line 286):
> "Integration test demonstrates parent-ctx cancellation | PASS — `TestBestEffortRun_CtxCancelKillsSubprocess` + `TestRunGitCtx_CtxCancel`"

**Observable state:**
```
$ grep -rn "TestAstromech_EstopCancelsInFlightGitOp" --include="*.go" .
(no output)

$ grep -rn "SetEstopped" --include="*_test.go" .
(no output)
```

**Substitute test bodies** (internal/git/ctx_cancel_test.go):
```go
func TestBestEffortRun_CtxCancelKillsSubprocess(t *testing.T) {
    if _, err := exec.LookPath("sleep"); err != nil { ... }
    ctx, cancel := context.WithCancel(context.Background())
    done := make(chan struct{})
    go func() {
        // Mirror bestEffortRun: WithTimeout wraps the passed ctx (not
        // Background) so the outer cancel propagates.
        c, c2 := context.WithTimeout(ctx, 30*time.Second)
        defer c2()
        _ = exec.CommandContext(c, "sleep", "30").Run()
        close(done)
    }()
    time.Sleep(100 * time.Millisecond)
    cancel()
    select {
    case <-done:
    case <-time.After(2 * time.Second):
        t.Fatal("subprocess did not honor parent-ctx cancellation within 2s — fabricated-ctx regression")
    }
}
```

The test (a) does not call `bestEffortRun`, (b) does not call
`SetEstopped`, (c) lives in package `git` not `agents`. It cannot satisfy
the integration-test contract in the spec.

**Required remediation:** add `TestAstromech_EstopCancelsInFlightGitOp`
in `internal/agents/` that:
1. Spawns an astromech against a stub git remote that hangs.
2. Calls `store.SetEstopped(db, true)` after the git op begins.
3. Asserts the in-flight subprocess errors within 2 seconds (not 10+).
4. Calls the production helper (`runShortGit` / `combinedShortGit` /
   `combinedShortGitArgs`) directly, not a shape mirror.

### Failure 2 — TestRunShortGit_CtxCancel absent

Same shape as Failure 1: spec named the test, no test by that name exists,
peer-test substitute is in a different package.

Less restart-blocker-y than Failure 1 (the spec's restart-gate enumerates
TestAstromech_EstopCancelsInFlightGitOp explicitly; TestRunShortGit_CtxCancel
is in the verification procedure but not the restart-gate list). Still a
spec gap.

### Defect 3 (non-blocker) — Closure-report arithmetic 88 ≠ 95

FIX-8E-CLOSURE.md § "rows.Err() sweep table" claims "Total: 88 loops
patched" but the per-file row sums to 95. Documentation prose is
inconsistent with the per-file table. Substantive 100% coverage holds;
this is a bookkeeping error in the prose narrative.

### Defect 4 (non-blocker) — Track A cancellation tests are shape-mirror

`TestBestEffortRun_CtxCancelKillsSubprocess` and `TestRunGitCtx_CtxCancel`
do not call the production helpers. They mirror what the helper body
ought to look like. A regression in the helper body would not surface
here. Pattern P11's per-site walk catches the static case. Recommend
either rewriting the test bodies to call the helpers, or accepting that
P11 carries the contract and renaming the tests to reflect what they
actually test.

### Defect 5 (non-blocker) — P1.1 window is 60 lines, spec said ≤10

audit_pattern_p1_1_rows_err_test.go uses `windowEnd := closeIdx + 60` at
L103. The spec said "within the 10-line window following the loop's
closing brace." 60 lines is permissive but not unsafe in practice (no
production loop has its rows.Err() check 50+ lines below the close;
sub-agent sample of 15 found 0 such cases). Tightening to 10 lines is a
follow-up tightening, not a NOT-DONE.

## Fix #8d regression check

Verified no regression of Fix #8d closures:

- AUDIT-026/027 (state-transition guard, P7) — TestPattern_P7 green at
  -race -count=5 (in-flight in main suite).
- AUDIT-072/090/091/094/095 (rows.Scan) — TestPattern_P1_RowsScanErrorsChecked
  green; no skip markers.
- AUDIT-100 (Fix #8d Track C closure) — clean.
- AUDIT-127/158/165 (exec.CommandContext) — Pattern P11 closes, with Fix
  #8e tightening (per-site, cheat-shape rejection). All migration sites
  carry ctx.
- AUDIT-156/159 (state-transition test extension) — P7 green.
- remainingAuditSkips empty.

## Final verdict (re-stated)

**NOT-DONE.** One restart-blocker gap: spec-named integration test
`TestAstromech_EstopCancelsInFlightGitOp` is absent and the closure
report's substitute (helper-level shape-mirror tests in a different
package, neither of which calls `store.SetEstopped`) does not satisfy the
restart-gate contract. All other checks PASS or PASS-with-minor-doc-defects.
Closing this single gap closes the campaign. Recommend the operator
re-open Fix #8e with the integration test as the only remaining
deliverable; expect a small follow-up commit to land the test and
re-trigger verification.
