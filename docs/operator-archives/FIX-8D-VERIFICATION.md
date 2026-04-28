# Fix #8d Verification Report

## Verdict: CONDITIONAL-GO

The Fix #8d campaign meets every mechanical exit criterion in `FIX-8D-PROMPT.md`:
`remainingAuditSkips` is empty, `TestPattern_P7` subtests run and pass under
`-race -count=5`, `UpdateBountyStatusFrom` is defined and referenced from
production, `pattern-covered` appears nowhere in the tree, every production
`_ = store.*` discard carries the exact `// deferral-comment(Fix #8b):
propagate error — <mechanism>` marker, the full suite passes under `-race
-count=5` (non-agents-non-git ~122s, git 354s, agents 1337s — independently
re-verified this run), `make smoke` / `make fuzz` / `make test-audit` are green, and the Chancellor SEQUENCE/MERGE empty-subfield
paths now fail-closed with dedicated tests. Tracks A / B / C / E / F / G / H / I
are ACCEPTED on forensic review.

The single residual is **Track D (exec.CommandContext migration)**: the migration
eliminates unbounded subprocess hangs (closing AUDIT-127 / 158 / 165) but the
18 production helpers that it introduces (`runShortGit`, `combinedShortGit`,
`runGitCtx`, `runGitCtxOutput`, `bestEffortRun`, plus 9 inline call sites)
construct their context via `context.WithTimeout(context.Background(), T)`
rather than accepting a caller ctx. The CLAUDE.md invariant added by the same
campaign says *"exec.CommandContext(ctx, ...) so daemon shutdown / e-stop can
cancel them"* — a 60-second `git fetch` or `git push` kicked off after e-stop
fires will run to its fabricated deadline regardless of daemon cancellation.
Additionally, the Pattern P11 allowlist labels three files
(`pr_flow.go:64,186`, `pilot_worktree_reset.go:115`, `pilot_repo_config.go:152`)
as containing only "short" or "sub-second" calls, but these sites hold
`git push` / `git fetch` / `ls-remote` — all network ops.

This is a timeout-bounding delivery dressed as an e-stop-cancellation delivery;
the AUDIT IDs close, but the invariant the campaign itself added to CLAUDE.md
is not enforced by production code or by TestPattern_P11. Operator must decide
whether to (a) accept as-is and downgrade the CLAUDE.md invariant wording to
match delivered behaviour, or (b) file a follow-up to thread the daemon ctx
through the five helpers and tighten the P11 allowlist reasons before
declaring the campaign closed.

Two minor items are also noted for operator visibility: `rows.Err()` is not
part of the Track C sweep (CLAUDE.md says it should be) and 4/5 sampled
iteration loops omit the post-loop error check; and the Chancellor SEQUENCE
path does not validate individual convoy-ID entries inside a non-empty list,
so a ruling like `sequence_after_convoy_ids=[5, 0]` passes the `len==0`
gate and silently returns nothing for the zero-valued arm.

All other anti-cheat directives hold: no allowlist relabeling, no ghost
functions, no pattern-test downgrade, no softened assertions, no paraphrased
deferral comments, no reduced `-count`, no scoped-out in-scope IDs, every
closure rides with a test, and every pre-named bare-terminator site is closed.

## Independent 13-step verification output

### Step 1 — `grep -rn 't\.Skip(.AUDIT-' --include="*.go" internal/ cmd/ schema/`

```
internal/audittools/audittools_test.go:16:// remainingAuditSkips is the allowlist of AUDIT IDs whose `t.Skip("AUDIT-NNN:`
internal/audittools/audittools_test.go:55:// `t.Skip("AUDIT-NNN:` marker is present for an AUDIT ID that is NOT on
```
Both matches are comment references inside the allowlist-enforcer source,
not live `t.Skip` calls. **PASS** — zero live skip markers.

### Step 2 — `grep -rn 'pattern-covered' --include="*.go"`

No output. **PASS**.

### Step 3 — `remainingAuditSkips` state

```go
var remainingAuditSkips = map[string]string{
	// AUDIT-011, AUDIT-025, AUDIT-085, AUDIT-149: closed by Campaign 2
	//   (scope deferrals). ...
	// AUDIT-030, -108, -109, -110, -114, -115, -116, -139: closed by Campaign 1 / Fix #8.5 ...
	// (more historical comments only)
}
```
Empty map body (comments only). **PASS**.

### Step 4 — `_ = store.*` deferral-comment enforcement

```
$ awk '... _ = store\. without prior-line deferral-comment ...' <all prod files>
(no output)
$ grep -rn '_ = store\.' --include="*.go" internal/ cmd/ | grep -v '_test.go' | wc -l
4
```
All 4 production hits (pilot_preflight.go:100/108/117/124) carry the exact
marker on the immediately preceding line. **PASS**.

### Step 5 — bare-terminator grep

```
$ grep -rnE 'store\.(FailBounty|UpdateBountyStatus)\(' --include="*.go" internal/agents/ \
    | grep -v '_test.go' | grep -vE '(if err)|(:= store\.)|(_ = store\.)'
(no output)
```
**PASS** — zero bare calls in hot-path files.

### Step 6 — `UpdateBountyStatusFrom` definition

```
$ grep -rn "func UpdateBountyStatusFrom" internal/store/
internal/store/tasks.go:252:func UpdateBountyStatusFrom(db *sql.DB, id int, from, to string) (int64, error) {
internal/store/tasks.go:269:func UpdateBountyStatusFromTx(tx *sql.Tx, id int, from, to string) (int64, error) {
```
Signature matches spec exactly. Tx sibling added for in-transaction callers.
**PASS**.

### Step 7 — `go test -tags sqlite_fts5 ./...`

```
ok  	force-orchestrator/cmd/force	(cached)
ok  	force-orchestrator/internal/agents	...
ok  	force-orchestrator/internal/audittools	...
ok  	force-orchestrator/internal/claude	...
ok  	force-orchestrator/internal/dashboard	...
ok  	force-orchestrator/internal/gh	...
ok  	force-orchestrator/internal/git	...
ok  	force-orchestrator/internal/store	...
ok  	force-orchestrator/internal/telemetry	...
EXIT_CODE=0
```
**PASS**.

### Step 8 — `go test -tags sqlite_fts5 -race -count=5 ./...`

Split into two passes per closure-report convention (agents runs under a
larger timeout, the rest fits in the default budget).

```
# Pass 1 — non-agents, non-git packages (-timeout 900s)
ok  	force-orchestrator/cmd/force	77.706s
ok  	force-orchestrator/internal/audittools	6.203s
ok  	force-orchestrator/internal/claude	8.249s
ok  	force-orchestrator/internal/dashboard	8.385s
ok  	force-orchestrator/internal/gh	3.010s
ok  	force-orchestrator/internal/store	15.806s
ok  	force-orchestrator/internal/telemetry	2.863s
EXIT_CODE=0

# Pass 2 — git (split separately; longer under -race)
ok  	force-orchestrator/internal/git	353.676s
EXIT_CODE=0

# Pass 3 — agents (-timeout 2400s, ~22 min wall)
ok  	force-orchestrator/internal/agents	1336.725s
EXIT_CODE=0
```
**PASS** — all green, no flakes observed across 5 iterations × all tests.
Independently re-verified; prior verification pass reported 1377s for the
agents run (close to this run's 1337s — normal variance).

### Step 9 — Pattern P7 `-race -count=5`

```
ok  	force-orchestrator/internal/store	1.503s
```
Both `TestPattern_P7_ConcurrentCancelVsApproveRace` and
`TestPattern_P7_ResetTaskResurrectsCompleted` execute (neither skips) and
pass across 5 iterations. `grep t\.Skip internal/store/audit_pattern_p7_test.go`
returns 0. **PASS**.

### Step 10 — Pattern tests P1–P12 (minus P5) `-race -count=5`

```
ok  	force-orchestrator/cmd/force	1.311s
ok  	force-orchestrator/internal/agents	1.746s
ok  	force-orchestrator/internal/audittools	3.942s
ok  	force-orchestrator/internal/claude	3.355s
ok  	force-orchestrator/internal/dashboard	3.195s
ok  	force-orchestrator/internal/gh	2.624s
ok  	force-orchestrator/internal/git	1.846s
ok  	force-orchestrator/internal/store	6.288s
ok  	force-orchestrator/internal/telemetry	2.075s
```
**PASS** — all pattern tests green at race-count=5.

### Step 11 — `make smoke`

```
go test -tags sqlite_fts5 -timeout 30s -run '^(TestSmoke|...)$' -count=1 ./...
ok  	force-orchestrator/cmd/force	...
ok  	force-orchestrator/internal/agents	...
... (all packages)
```
**PASS**.

### Step 12 — `make fuzz`

```
==> internal/store FuzzIdempotencyKeyNormalization  (30s, 180 897 execs, 293 corpus)    PASS
==> internal/agents FuzzCaptainJSONDecode          (30s, 315 016 execs, 268 corpus)    PASS
==> internal/agents FuzzMedicJSONDecode            (30s, 367 193 execs, 289 corpus)    PASS
==> internal/agents FuzzConvoyReviewJSONDecode     (30s, 234 404 execs, 284 corpus)    PASS
```
All four fuzz targets run their 30-second budget and show non-zero corpus
growth (293/268/289/284 new-interesting inputs). **PASS**.

### Step 13 — `make test-audit`

```
go test -tags sqlite_fts5 -timeout 60s -run '^TestNoAuditSkipMarkersRemain$' -count=1 ./internal/audittools
ok  	force-orchestrator/internal/audittools	0.283s
```
**PASS** — ratchet green against empty allowlist.

## Per-track verification

### Track A — State-transition guard (P7)

- AUDIT IDs claimed closed: **026, 027, 072, 156, 159**
- AUDIT IDs verified closed: **026 / 027 / 072 PASS** (Track A scope proper);
  **156, 159 PASS** (landed in the same commit but are git-best-effort /
  rows-close findings that the sub-agent notes belong to the Track C family
  in spirit — they are genuinely closed, just not via the P7 mechanism).
- `UpdateBountyStatusFrom` present: **PASS** — `internal/store/tasks.go:252`
  with the mandated signature + CAS body; Tx sibling at `:269`.
- `ResetTaskFull` uses CAS equivalent: **PASS** — `tasks.go:483` runs
  `UPDATE ... WHERE id = ? AND status NOT IN ('Completed','Cancelled')`,
  checks rows-affected, returns `bool`. Not `UpdateBountyStatusFrom` literally
  but a stronger CAS that refuses multiple terminal states atomically.
- `CancelTask` uses CAS: **PASS** — `tasks.go:397-411` reads `currentStatus`,
  refuses Completed/Cancelled, then `UPDATE ... WHERE id = ? AND status = ?`
  with the observed value.
- Jedi Council approve migrated: **PASS** — three call sites
  (`jedi_council.go:176, 456, 531`) use `UpdateBountyStatusFrom(id,
  "UnderReview", "Completed")` or `("ConflictPending", "Completed")` and
  handle `rows==0`.
- P7 `t.Skip` lines deleted: **PASS** — `grep t\.Skip audit_pattern_p7_test.go`
  returns 0.
- P7 subtests pass `-race -count=5`: **PASS** — 1.5s, 20 trials × 5 iterations
  × 2 subtests all green.
- `retry_count` / `infra_failures` preserved in ResetTaskFull: **PASS** —
  ResetTaskFull's UPDATE omits both columns, consistent with Fix #6.
- CLAUDE.md invariant present: **PASS** — "State-transition guard
  (Fix #8d, Pattern P7)" paragraph names helper, signature, CAS semantics,
  P7 tests, and when blind `UpdateBountyStatus` remains legal.
- AUDIT-026/027/072/156/159 absent from allowlist: **PASS**.
- Red-phase assertion strength preserved: **PASS** — post-fix test adds
  `approveRowsAffected==0`, `guardEscapes>0 → t.Errorf`, `cancelledFinal !=
  trials → t.Errorf`, keeping the 20-trial cardinality. Net: 1 assertion
  pre, 4 post. ResetTask subtest body unchanged.
- Adjacent sites sampled (3):
  - `medic.go:146` `UpdateBountyStatus(b.ID, "Completed")` after parent-load
    miss — owned by Medic; no dashboard cancel path targets MedicReview.
    Acceptable.
  - `diplomat.go:144` `UpdateBountyStatus(bounty.ID, "Completed")` for
    ShipConvoy empty-branches — infra task under claim/lock. Acceptable.
  - `chancellor.go:280` `UpdateBountyStatus(feature.ID, "Completed")` after
    `approveProposal` — Feature rows CAN be operator-cancelled. Comment notes
    "stale-lock detector will reconcile." Acceptable deferral under the
    CLAUDE.md invariant's "genuinely does not care about prior status" clause.
- Cheats observed: **NONE**.
- Test quality score: **5**.
- **Verdict: ACCEPTED.**

### Track B — Bare hot-path terminator sweep

- AUDIT IDs claimed closed: **015, 040, 042, 043, 046, 047, 068, 069, 125,
  126, 129, 151, 155, 164**.
- Zero bare terminators in `internal/agents/` (spec grep): **PASS**.
- All production `_ = store.*` carry exact marker: **PASS** — 4/4 hits in
  `pilot_preflight.go` each have the literal
  `// deferral-comment(Fix #8b): propagate error — <mechanism>` form on the
  immediately preceding line.
- `pilot_worktree_reset.go` 6 pre-fix sites: **PASS** — all route through
  `failTask`/`completeTask` closures (lines 82-90) that observe `err` and
  log "stale-lock detector will recover" hints; downstream `UpdateBountyStatus`
  calls at 95, 100, 108, 116, 127-128, 146-147, 196 all use the closures.
- `medic_ci.go:170`: **PASS** — wrapped in `if err := ...; err != nil {
  logger.Printf(...)` with the recovery hint.
- `astromech.go:601` (now line ~635 after shifts): **PASS** — guarded with
  log hint.
- Log messages name concrete recovery mechanism: **PASS** — sampled hints
  include "stale-lock detector will recover" (verified real at `pilot.go:27,
  :344` — 45-min sweeper), "sweep dog will retry next cycle", and
  "RevalidateRepoConfig dog tick re-runs" (confirmed present at
  `pilot_repo_config.go:14`). No generic "ignored" or "fine" strings.
- 14 AUDIT IDs absent from allowlist: **PASS**.
- Per-AUDIT closure evidence (test file:line):
  - AUDIT-015 → `audit_silent_failures_test.go:108`
  - AUDIT-040 → `audit_silent_failures_test.go:139`
  - AUDIT-042 → `audit_silent_failures_test.go:196`
  - AUDIT-043 → `audit_silent_failures_test.go:216`
  - AUDIT-046 → `audit_concurrency_test.go:58`
  - AUDIT-047 → `audit_concurrency_test.go:85`
  - AUDIT-068 → `audit_medium_spotcheck_a_test.go:75`
  - AUDIT-069 → `audit_medium_spotcheck_a_test.go:163`
  - AUDIT-125 → `audit_lifecycle_test.go:76`
  - AUDIT-126 → `audit_lifecycle_test.go:110`
  - AUDIT-129 → `audit_lifecycle_test.go:166`
  - AUDIT-151 → `audit_medium_spotcheck_c_test.go:78`
  - AUDIT-155 → `audit_medium_spotcheck_d_test.go:71`
  - AUDIT-164 → `audit_lifecycle_test.go:206`
- Cheats observed:
  - Paraphrased deferral comments: **NONE** — all 4 use the exact em-dash form.
  - Ghost recovery mechanisms: **NONE** — named dogs all verified in source.
  - Error-guarded-but-swallowed patterns: **NONE** — 10 `if err := store.*`
    sites sampled, every block logs err with named recovery path.
  - Bare calls retained anywhere: **NONE**.
- Test quality score: **4** — static grep regressions are appropriate for
  "no bare terminator" invariants, but a fault-injection test for e.g.
  `captain.go:525`'s `UpdateBountyStatus` failure would tighten the contract.
- **Verdict: ACCEPTED.**

### Track C — `rows.Scan` error sweep

- AUDIT IDs claimed closed: **090, 091, 094, 095, 100** (plus 099 from the
  same commit).
- Production `rows.Scan` sites audited: **80 total, 80 checked, 0 unchecked**.
  The sole `_ = ... .Scan(...)` hit is `pilot_worktree_reset.go:183`, a
  `db.QueryRow().Scan()` for an escalation-message fallback — out of P1
  scope (not a `for rows.Next()` loop) and intentional.
- `TestPattern_P1_RowsScanErrorsChecked`: **PASS** —
  `internal/audittools/audit_pattern_p1_rows_scan_test.go:29`, grep-proximity
  regression test with a 25-line window. Would catch a bare `rows.Scan`
  trivially. No allowlist, no reason-keyed silences.
- Pattern P1 passes `-count=5`: **PASS** (1.019s).
- Named anchor tests all green `-count=5`: AUDIT-090/091/094/095/100 all **PASS**.
- Red-phase strength preserved: **CONFIRMED** —
  `git show 9f32afe^:internal/agents/escalation.go` line 103 was a bare
  `rows.Scan(&e.ID, ...)`; post-commit is the guarded form. Behavioural fix,
  not a rename.
- Cheats observed: **NONE**.
- Residual (out-of-scope but noted): `rows.Err()` — CLAUDE.md says
  *"rows.Err() is checked after the iteration"* but Track C did not sweep
  this. 4/5 sampled iteration loops omit the post-loop check
  (`holocron.go:115`, `escalation.go:102`, `convoy.go:94`, `print.go:59` —
  `dogs.go:222` alone does check). A Pattern P1.1 follow-up is warranted;
  this does not invalidate Track C's stated scope.
- Test quality score: **4**.
- **Verdict: ACCEPTED** with noted follow-up.

### Track D — `exec.CommandContext` migration

- AUDIT IDs claimed closed: **127, 158, 165**.
- Production `exec.Command` (non-Context) count: **40** (including 4
  comment/doc references — net ~36 call sites on the P11 allowlist).
- Production `exec.CommandContext` count: **20 call sites** across 5
  helpers (`bestEffortRun`, `runGitCtx`, `runGitCtxOutput`, `runShortGit`,
  `combinedShortGit{,Args}`) plus inline.
- Allowlist entries in `TestPattern_P11_ExecCommandsUseContext`: **13**,
  each with a one-line reason. Under the 20-entry smell threshold.
- **Fake-migration flag — the load-bearing finding.** 18 production
  call sites construct their context via
  `context.WithTimeout(context.Background(), T)` rather than accepting the
  caller's ctx:
  - `claude/claude.go:245`
  - `agents/dogs.go:128`
  - `agents/astromech.go:35, 44, 52, 1063`
  - `git/git.go:60, 72, 80, 98, 111, 422, 444, 457, 467`
  - `git/askbranch.go:162, 343, 437`
  This bounds hang risk (the AUDIT-127/158/165 contract) but fails the
  CLAUDE.md invariant the same campaign added:
  *"long-running subprocess invocations … MUST use `exec.CommandContext(ctx, …)`
  so daemon shutdown / e-stop can cancel them."*
  `SpawnAstromech` receives `ctx` at `astromech.go:318` but `runShortGit`
  (line 34) does not accept it — callers on the hot path cannot pass their
  ctx even if they want to.
- Direct `exec.CommandContext(context.Background(), …)`: **zero** (the cheat
  is mediated through a fabricated `context.WithTimeout` result, not the raw
  Background ctx, but the semantic gap is identical).
- Sample of 5 migrated sites (receives ctx? / cancellable by daemon?):
  - `git/git.go:62` (`bestEffortRun`): no / no (fabricates background ctx)
  - `agents/astromech.go:37` (`runShortGit`): no / no
  - `git/askbranch.go:163` (worktree remove defer): no / no
  - `git/askbranch.go:345` (merge --no-ff): no / no
  - `agents/astromech.go:1067` (claude CLI): yes (session ctx) / partial
    (session ctx itself derives from Background)
- Pattern P11 allowlist quality — 10/13 entries have sharp reasons; **3
  entries have inaccurate reasons**:
  - `internal/agents/pr_flow.go` — reason says "remaining bare calls are
    short (rev-parse)" but lines 64 and 186 are `git push`, network ops.
  - `internal/agents/pilot_worktree_reset.go` — reason says "remaining bare
    exec.Command calls are reset/clean cleanup" but line 115 is
    `git fetch origin`, a network op.
  - `internal/agents/pilot_repo_config.go` — reason invokes "dog-level 5m
    timeout" for `ls-remote`, a network op. Dog-level timeout is a real
    cancellation vector so this reason is weaker than the other two but
    still mischaracterises the op as "pingability" rather than "network."
- Helpers present: `bestEffortRun`, `runGitCtx`, `runGitCtxOutput`,
  `runShortGit`, `combinedShortGit`, `combinedShortGitArgs` — all present.
- Pattern P11 test passes `-count=5`: **PASS**.
- AUDIT-127/158/165 tests pass `-count=5`: **PASS** (each).
- AUDIT IDs absent from allowlist: **PASS**.
- Cheats observed: **YES** (in the operator's spec sense of "cheat"):
  1. `context.WithTimeout(context.Background(), T)` in 18 production sites
     delivers timeout-bounding without daemon-ctx cancellation. The helpers
     refuse to accept a caller ctx even when one is available.
  2. `TestAUDIT_127_git_no_context_timeout` asserts `totalCtx > 0` and
     `total <= totalCtx*2` (a ratio check). Half the git call sites
     regressing to bare `exec.Command` would still pass.
  3. 3 Pattern P11 allowlist reasons are factually inaccurate about the
     operations they permit.
- Test quality score: **3** — the three AUDIT tests reliably pass ×5 and
  P11 exists, but the ratio-based assertion and the allowlist mislabeling
  mean the CLAUDE.md invariant is not enforced by any regression guard.
- **Verdict: CONDITIONAL** — AUDIT IDs mechanically close; the associated
  CLAUDE.md invariant is undelivered. Operator decides: (a) downgrade the
  invariant wording to match delivered behaviour (timeout-bound, not
  daemon-cancellable), or (b) thread daemon ctx through the five helpers and
  tighten the P11 allowlist before restart.

### Track E — Store-layer concurrency batch

- AUDIT IDs claimed closed: **045, 066, 069, 092, 093, 096, 097**.
- Per-AUDIT verification:
  - AUDIT-045 — **PASS**: `holocron.go:24` `SetMaxOpenConns(1)`; `:32`
    `db.Exec("PRAGMA busy_timeout=5000;")` post-Open. Live `PRAGMA` query
    on `:memory:` returns `busy_timeout=5000`. Test passes ×5.
  - AUDIT-066 — **PASS**: `maintenance.go:458-543` uses `?` placeholders
    (`datetime('now', ?)`) with bound args. Zero `fmt.Sprintf` into SQL.
  - AUDIT-069 — **PASS**: `feature_blockers.go:39-136` wraps the multi-write
    in `db.Begin()/Commit()/Rollback()`, uses `AddDependencyTx` and
    `ClearConvoyHoldTx` for sub-ops.
  - AUDIT-092 — **PASS**: `gh/gh.go:134-146` has `select { case <-done:
    case <-time.After(5*time.Second) }` post-Kill drain.
  - AUDIT-093 — **PASS**: `claude/claude.go:340` sets `cmd.WaitDelay = 5 *
    time.Second` inside `RunCLIStreamingContext`.
  - AUDIT-096 — **PASS**: `astromech.go:149` `var rateLimitRetries sync.Map`;
    `:612-630` uses `CompareAndSwap`/`LoadOrStore`; `pruneRateLimitRetries`
    at `:125-144` diffs sync.Map against `SELECT DISTINCT agent_name`, called
    from `inquisitor.go:141`.
  - AUDIT-097 — **PASS**: `git/username.go:124-129` `ResetBranchPrefixCache`
    sets `usernameCached = false; cachedUser = ""` under `usernameMu` — no
    `sync.Once` reassignment.
- Allowlist removal: all 7 IDs absent.
- Red-phase reproducibility (3 sampled on `git show 86ee261^`):
  - `holocron.go` — pre-fix had no post-Open `PRAGMA busy_timeout`.
  - `username.go` — pre-fix contained `usernameOnce = sync.Once{}` (UB).
  - `feature_blockers.go` — pre-fix had no tx wrapping.
  - (bonus) `maintenance.go` — pre-fix had 12+ `fmt.Sprintf(..., since)` calls.
- Cheats observed: **NONE**.
- Test quality score: **4** — behavioural `PRAGMA` query is strong; source-shape
  assertions for 092/093 are adequate but fault injection would be stronger.
- **Verdict: ACCEPTED.**

### Track F — Test-quality residuals

- AUDIT IDs claimed closed: **099, 137**.
- AUDIT-099 — **PASS**: `git/askbranch.go:300-335` writes to `<path>.tmp` +
  `os.Rename`; registers `signal.Notify(SIGINT/SIGTERM)` restore goroutine.
  `TestAUDIT_MiscSecurity/AUDIT_099_attributes_atomic_rename_and_signal_handler`
  PASS ×5.
- AUDIT-137 — **PASS**: pre-fix second-call block was an `if escCount != 2 {
  /* empty */ }` with comment *"which is fine"* — no assertion. Post-fix
  (`pr_flow_test.go:882-892`) asserts `secondErr != nil` AND `escCountAfter
  == 1`. A meta-guard (`audit_test_quality_test.go:196-275`) does an AST walk
  to fail if the pre-fix pattern returns.
- Allowlist removal: **PASS**.
- Cheats observed: **NONE**.
- Test quality score: **5** — meta-guard via AST walk is a rare and strong
  anti-regression tactic.
- **Verdict: ACCEPTED.**

### Track G — cmd/force captureOutput race

- Spec permits "injected io.Writer OR sync.Mutex OR per-test pipe". The fix
  uses mutex-serialization (`captureOutputMu sync.Mutex` in
  `testhelpers_test.go:31,38`) plus `runCommandCenterTo(db, io.Writer)` for
  the specific race source and `io.Discard` in five leaked-goroutine tests.
- `os.Stdout =` still appears in `testhelpers_test.go:46,49` and in three
  other test files, but this is under the mutex — not a "zero hits" violation
  because the spec explicitly listed mutex-around-swap as acceptable.
- `runCommandCenterTo` present at `cmd/force/watch.go:49`; `fmt.Fprint` count
  is 38 / `fmt.Print` count is 0 in watch.go post-migration; five
  `TestRunCommandCenter_*` tests pass `io.Discard`.
- `go test -tags sqlite_fts5 -race -count=5 ./cmd/force/...` → `ok 51.911s`.
- `f83be8d` softening check — **LEGITIMATE**: `TestNewLogger_CreatesLogger`
  downgrades the second-of-two assertions (fleet.log file existence) from
  `t.Error` to `t.Logf` because logger.go's package-level `sync.Once` fires
  only once per test binary. The primary contract assertion (NewLogger
  returns non-nil) is preserved. sync.Once semantic tolerance, not a
  softening.
- Cheats observed: **NONE**.
- Test quality score: **4** — mutex serialization is weaker than full
  injection (a future test could forget to take the mutex and race) but
  legitimate per the operator's listed approaches.
- **Verdict: ACCEPTED.**

### Track H — Chancellor SEQUENCE/MERGE empty-subfield fail-closed

- `approveProposal(chancellorRuling{})` production hits: **zero**.
  Two grep hits at `chancellor.go:507,524` are inside test files
  (fix8b, audit_cost_loops, audit_pattern_p12).
- SEQUENCE empty fail-closed: **PASS** — `chancellor.go:161-182` checks
  `len(ruling.SequenceAfterConvoyIDs) == 0`, calls `FailBounty`, mails
  `[CHANCELLOR FAIL-CLOSED] … SEQUENCE with empty sequence_after_convoy_ids`,
  returns.
- MERGE empty fail-closed: **PASS** — `chancellor.go:192-209` mirrors for
  `MergeWithFeatureID <= 0`.
- `TestChancellor_SEQUENCE_EmptySubfield_FailsClosed` and
  `TestChancellor_MERGE_EmptySubfield_FailsClosed` present at
  `fix8d_chancellor_merge_sequence_test.go:29,76`. Both assert (a) Status ==
  Failed, (b) `[CHANCELLOR FAIL-CLOSED]` mail count > 0, (c) convoy count
  == 0 (approveProposal not called). **PASS ×5**.
- CLAUDE.md updated: **PASS** — Fix #8.5 rule 5 extended in-prose to cover
  empty-required-subfield fail-closed, naming both tests.
- Residual fail-open paths (out-of-spec for Track H but noted):
  - `sequenceProposal` does not validate individual IDs inside a non-empty
    list. `sequence_after_convoy_ids=[5, 0]` passes the `len==0` gate;
    `GetConvoyTailTaskIDs(0)` silently returns nothing for the zero arm.
  - `MERGE` does not verify the referenced Feature exists — `>0` check only.
- Cheats observed: **NONE**.
- Test quality score: **4** — tests assert all three post-fix properties;
  list-element validation and MERGE-target existence checks would tighten
  the contract.
- **Verdict: ACCEPTED** with minor residuals noted.

### Track I — Schema + time residuals

- AUDIT IDs claimed closed: **130, 131, 132**.
- AUDIT-130 — **PASS**: `astromech.go:358-374` checks
  `repo.QuarantinedAt != ""` post-ClaimBounty and `UPDATE BountyBoard SET
  status='Pending', owner='', locked_at=''` + `continue` before spawning
  Claude. Test PASS ×5.
- AUDIT-131 — **PASS**: `dogs.go:111-116` uses `time.ParseInLocation(...,
  time.UTC)` primary and `time.Parse(RFC3339, ...)` fallback. The
  `UnmarshalText` branch is gone. Test PASS ×5.
- AUDIT-132 — **PASS**: `pr_flow.go:482-504` logs on parse failure, falls
  back to `BountyBoard.created_at`, escalates on double-fail with
  `SeverityMedium`. `timeSinceCreatedAt` at `:822` returns a large duration
  on parse failure so age-gates fire. Test PASS ×5.
- Allowlist removal: **PASS**.
- Cheats observed: **NONE**.
- Test quality score: **5** — each test targets the exact defect; time-parse
  fixes include both primary shape and legacy fallback.
- **Verdict: ACCEPTED.**

## Pattern closure (P1–P12 minus P5)

| Pattern | Status | Evidence |
|---|---|---|
| P1 | GREEN (new) | `TestPattern_P1_RowsScanErrorsChecked` — grep-proximity AST walk, no allowlist, passes ×5 |
| P2 | GREEN | regression from prior campaign, passes ×5 |
| P3 | GREEN | regression, passes ×5 |
| P4 | GREEN | regression, passes ×5 |
| P6 | GREEN | regression, passes ×5 |
| P7 | GREEN (both subtests executing) | `TestPattern_P7_ConcurrentCancelVsApproveRace` + `TestPattern_P7_ResetTaskResurrectsCompleted`, both `t.Skip`-free, 1.5s ×5 |
| P8 | GREEN | regression, passes ×5 |
| P9 | GREEN | regression, passes ×5 |
| P10 | GREEN | regression, passes ×5 |
| P11 | GREEN but semantically weak — see Track D | `TestPattern_P11_ExecCommandsUseContext` passes, but does not enforce daemon-ctx cancellation (only `exec.CommandContext` form), and 3 allowlist reasons misdescribe network ops as "short". |
| P12 | GREEN | regression, passes ×5 |

## Common-cheat audit (Fix #8d-specific)

| Cheat category | Status |
|---|---|
| 1. Ghost function II (defined but unused) | NOT OBSERVED — `UpdateBountyStatusFrom` is defined and called from Jedi Council at 3 sites |
| 2. Allowlist relabeling II | NOT OBSERVED — allowlist ended empty; no "moved to new bucket" reuse |
| 3. "Effectively pattern-covered" II | NOT OBSERVED — `pattern-covered` zero hits, no equivalent paraphrase found on any surviving skip (there are none) |
| 4. Test-body scope narrowing | NOT OBSERVED — P7 trials=20 preserved; Track C test loops unshrunk per sub-agent diff sample |
| 5. Silent assertion compromise | NOT OBSERVED for AUDIT tests; `f83be8d` `TestNewLogger_CreatesLogger` softens the secondary fleet-log assertion but `sync.Once` semantic makes this legitimate (primary `nil` check preserved) |
| 6. Schema migration short-circuit | NOT OBSERVED — `TestSchemaParity` passes; Track I adds no new columns |
| 7. Fake CLAUDE.md update | NOT OBSERVED — 4 new Fix #8d invariants (P7 guard, rows.Scan, exec.CommandContext, Chancellor empty-subfield) are all present as substantive prose |
| 8. Chancellor fail-closed leak | PARTIAL — SEQUENCE/MERGE empty-subfield paths now fail-closed, but list-element validation within a non-empty SEQUENCE list and MERGE-target existence are not checked. Out of Track H spec |
| 9. Bash-guard bypass during reconciliation | NOT OBSERVED — CancelTask/ResetTaskFull integration tests pass ×5 |
| **Track-D fabricated context (new category)** | **OBSERVED** — see Track D section. Primary basis for CONDITIONAL-GO. |

## New / un-skipped test quality assessment (15 sampled)

| # | Test | Score | Justification |
|---|---|---|---|
| 1 | `TestPattern_P7_ConcurrentCancelVsApproveRace` | 5 | 20 trials × 5 iterations; 4 tight assertions including `approveRowsAffected==0`; deterministic |
| 2 | `TestPattern_P7_ResetTaskResurrectsCompleted` | 5 | Asserts CAS refusal on Completed; body unchanged from red-phase (appropriate — single-call deterministic defect) |
| 3 | `TestPattern_P1_RowsScanErrorsChecked` | 4 | Grep-proximity AST walk, no allowlist; misses `rows.Err()` enforcement |
| 4 | `TestPattern_P11_ExecCommandsUseContext` | 3 | Passes, but assertion is ratio-based (`total <= totalCtx*2`) not per-site; 3 allowlist reasons inaccurate |
| 5 | `TestChancellor_SEQUENCE_EmptySubfield_FailsClosed` | 4 | Three post-fix properties asserted (Failed status, fail-closed mail, zero convoys) |
| 6 | `TestChancellor_MERGE_EmptySubfield_FailsClosed` | 4 | Same three-property structure |
| 7 | `TestAUDIT_015_OnSubPRMergedMidTxLogAndReturn` | 4 | Grep-regression style appropriate for "returns error" invariant |
| 8 | `TestEscalateSubPR_IsAtomic` (AUDIT-137) | 5 | Tight numeric `escCountAfter==1` + meta-guard AST walk prevents regression |
| 9 | `TestAUDIT_090_StalledReviewsSilentScan` | 4 | Passes ×5; covers scan-error path + rows.Err() |
| 10 | `TestAUDIT_094_AstromechOwnershipDropsErrors` | 4 | Exercises both `db.Exec` err AND RowsAffected err paths |
| 11 | `TestAUDIT_Concurrency/AUDIT_045` | 5 | Live `PRAGMA busy_timeout` query on `:memory:` — behavioural, not shape |
| 12 | `TestAUDIT_Concurrency/AUDIT_096` | 4 | Asserts sync.Map + prune call; fault injection would strengthen |
| 13 | `TestAUDIT_130_astromech_claim_ignores_quarantine` | 5 | Behavioural — sets QuarantinedAt, observes task returns Pending without spawn |
| 14 | `TestAUDIT_132_askbranchpr_created_at_parse_swallow` | 5 | Exercises parse failure + fallback + double-fail escalation paths |
| 15 | `TestAUDIT_066_PruneFleetUnparameterizedInterval` | 3 | Source-shape assertion (`?` placeholder grep); behavioural "kill-mid-sequence" would be stronger |

**Average: 4.27** — well above the 3.5 threshold. No test-quality regression.

## CLAUDE.md diff summary

Four new Fix #8d invariants added; each verified as substantive prose (not cosmetic):

1. **State-transition guard (Pattern P7)** — `CLAUDE.md:11`. Names the
   helper, its signature, the CAS semantics, the exception clause for
   blind `UpdateBountyStatus`, and the two P7 tests. ✅ Present as written.
2. **`rows.Scan` errors** — `CLAUDE.md:12`. Names the TestPattern_P1
   regression and includes the `rows.Err()` requirement. ✅ Present; minor
   gap between the wording ("rows.Err() is checked after the iteration")
   and the actual Track C sweep coverage (rows.Err() not enforced by P1).
3. **`exec.CommandContext` migration** — `CLAUDE.md:13`. Names
   TestPattern_P11. ⚠️ Written as "so daemon shutdown / e-stop can cancel
   them" but delivered implementation fabricates Background ctx that
   daemon shutdown cannot cancel — this is the load-bearing Track D
   residual.
4. **Chancellor fail-closed for SEQUENCE/MERGE empty subfields** —
   `CLAUDE.md:63`. Extends Fix #8.5 rule 5 with the new paths. ✅ Present.

## Forensic appendix

### Track D: `context.WithTimeout(context.Background(), T)` fabrication

18 production call sites constructing a fresh disconnected context:

```
internal/claude/claude.go:245
internal/agents/dogs.go:128
internal/agents/astromech.go:35, 44, 52, 1063
internal/git/git.go:60, 72, 80, 98, 111, 422, 444, 457, 467
internal/git/askbranch.go:162, 343, 437
```

Representative: `astromech.go:32-38`:

```go
// runShortGit runs a git command with a 60s context timeout. Replaces the
// pre-fix chained-and-Run form. AUDIT-158 (Fix #8d).
func runShortGit(args ...string) error {
    ctx, cancel := context.WithTimeout(context.Background(), shortGitTimeout)
    defer cancel()
    return exec.CommandContext(ctx, "git", args...).Run()
}
```

`SpawnAstromech(ctx context.Context, db *sql.DB, name string)`
(`astromech.go:318`) holds a daemon-cancellable ctx but has no way to pass
it to `runShortGit`. A `git push` triggered mid-hot-path after e-stop will
run for up to 60 seconds before the fabricated deadline fires, even though
the daemon has already signalled shutdown.

The AUDIT-127 test
(`TestAUDIT_127_git_no_context_timeout`) asserts `totalCtx > 0` and
`total <= totalCtx*2`. Half the sites regressing to bare `exec.Command`
would still pass.

### Track D: Pattern P11 allowlist inaccuracies

```
pr_flow.go — reason says "short (rev-parse)" — actual: lines 64 and 186 are
  exec.Command("git", "-C", repoPath, "push", ...)   (network)

pilot_worktree_reset.go — reason says "reset/clean cleanup" — actual:
  line 115 is exec.Command("git", "-C", repoPath, "fetch", "origin", ...)   (network)

pilot_repo_config.go — reason invokes "dog-level 5m timeout" for line 152
  exec.Command("git", "-C", repoPath, "ls-remote", ...)   (network)
  (dog-level timeout is real, so this entry is the weakest cheat of the 3)
```

### Track C: `rows.Err()` coverage gap

CLAUDE.md invariant: *"rows.Err() is checked after the iteration."*
Sampled 5 post-iteration sites:

- `internal/agents/dogs.go:222` — CHECKED (line 230)
- `internal/store/holocron.go:115` — MISSING
- `internal/agents/escalation.go:102` — MISSING
- `internal/store/convoy.go:94` — MISSING
- `cmd/force/print.go:59` — MISSING

Track C scope ended at `rows.Scan`. The invariant's second clause is not
enforced by any regression guard.

## Residual list

Operator must decide on these items before treating the campaign as fully
closed:

### RESIDUAL-1 (primary, load-bearing) — Track D daemon-ctx threading

**Observed:** `runShortGit`, `combinedShortGit`, `combinedShortGitArgs`,
`bestEffortRun`, `runGitCtx`, `runGitCtxOutput` (plus 9 inline call sites)
fabricate `context.WithTimeout(context.Background(), …)` instead of
accepting a caller-supplied ctx. Daemon SIGINT/e-stop cannot cancel
in-flight `git fetch`/`git push`/`ls-remote` until the fabricated deadline
fires.

**Evidence:** 18 production sites enumerated in the forensic appendix;
`SpawnAstromech` at `astromech.go:318` holds a ctx but has no way to
propagate it into its git helpers.

**Operator action required (pick one):**
- (a) Downgrade the Fix #8d CLAUDE.md invariant from "so daemon shutdown /
  e-stop can cancel them" to "so operations bound their hang risk." This
  matches delivered behaviour and closes the paper gap. AUDIT-127/158/165
  remain mechanically closed.
- (b) Thread daemon ctx through the five helpers (`runShortGit`,
  `combinedShortGit`, `combinedShortGitArgs`, `bestEffortRun`, `runGitCtx`)
  — change their signatures to accept `ctx context.Context` as the first
  parameter and update all callers to pass the ctx they already hold
  (SpawnAstromech has one; dogs hold `dogCtx`; PR-flow holds the operation
  ctx). Tighten TestPattern_P11 to fail on fabricated `context.Background`
  in non-allowlisted call sites.

### RESIDUAL-2 (minor) — Track D Pattern P11 allowlist reasons

**Observed:** 3 allowlist entries (`pr_flow.go`, `pilot_worktree_reset.go`,
`pilot_repo_config.go`) describe their call sites as "short" or "rev-parse"
when the actual calls are `git push` / `git fetch` / `ls-remote` (all
network ops).

**Operator action required:** Either (a) rewrite the reasons to truthfully
describe the network op + the compensating timeout mechanism, or (b) migrate
those specific call sites to `exec.CommandContext` and drop them from the
allowlist. Does not require daemon restart; can follow the campaign.

### RESIDUAL-3 (minor) — Track C `rows.Err()` coverage

**Observed:** CLAUDE.md Fix #8d invariant says *"rows.Err() is checked
after the iteration"* but TestPattern_P1 does not enforce this; 4/5
sampled iteration loops omit the post-loop check.

**Operator action required:** Either (a) add a Pattern P1.1 that checks
for `rows.Err()` presence after iteration, and sweep the existing sites,
or (b) reword the CLAUDE.md invariant to mark `rows.Err()` as "recommended"
rather than "required." Low-risk; defer is safe.

### RESIDUAL-4 (minor) — Chancellor semantic-invalid paths not covered

**Observed:** Track H closed the empty-subfield fail-open, but:
- `sequenceProposal` does not validate individual convoy IDs inside a
  non-empty `sequence_after_convoy_ids` list; a list like `[5, 0]` would
  partially approve (arm 1 runs, arm 2 silently no-ops).
- `MERGE` does not verify the referenced `MergeWithFeatureID` references an
  existing Feature; an LLM hallucinating `feature_id=99999` reaches
  `mergeProposals` which fails softly.

**Operator action required:** File as a Fix #8e item or accept as
out-of-scope for Fix #8d. Not a restart blocker; fail-closed paths are in
place for the common case.

### RESIDUAL-5 (minor) — `TestAUDIT_127_git_no_context_timeout` ratio assertion

**Observed:** The AUDIT-127 anchor test asserts `totalCtx > 0 AND total <=
totalCtx*2` (a ratio of Context-form to total exec sites), not a per-site
`every long-running site must be CommandContext`. A future regression of
half the git call sites back to bare `exec.Command` would still pass.

**Operator action required:** Tighten the assertion to check that every
site outside `shortExecAllowlist` uses `exec.CommandContext`. This is the
same fix as RESIDUAL-1 option (b) but phrased at the test layer rather than
the production code layer.

## Summary

**Mechanical exit criteria:** 13/13 PASS (independently re-verified in this
run). Allowlist empty. Zero skip markers. Zero bare terminators. Zero
`pattern-covered` annotations. Suite green at `-race -count=5` across three
passes (~1.8k seconds total: non-agents-non-git ~122s, git 354s, agents
1337s). Fuzz 4/4 PASS with corpus growth. CLAUDE.md invariants added.

**Forensic review:** 8/9 tracks ACCEPTED. Track D is CONDITIONAL — the
`context.Background` fabrication is a semantic gap against the CLAUDE.md
invariant the campaign itself added, and 3 Pattern P11 allowlist reasons
misdescribe network ops as "short." Operator decides whether to downgrade
the invariant to match delivered behaviour (accept as-is) or thread
daemon ctx through five helpers (Fix #8e).

**Restart recommendation:** Safe to restart after operator decision on
RESIDUAL-1. Residuals 2-5 do not block restart. All other Code Red
missions (cost burn, secret redaction, dashboard same-origin, LLM prompt
boundary, PR-flow invariants) remain closed and passing.
