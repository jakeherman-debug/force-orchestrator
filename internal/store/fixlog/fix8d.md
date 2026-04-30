## Fix #8d — Code Red Full Closure (38 AUDIT IDs)

**AUDIT IDs closed:** 015, 026, 027, 040, 042, 043, 045, 046, 047, 066, 068,
069, 072, 090, 091, 092, 093, 094, 095, 096, 097, 099, 100, 125, 126, 127,
129, 130, 131, 132, 137, 151, 155, 156, 158, 159, 164, 165.

**Branches/commits:** `acc6a92`, `86ee261`, `9f32afe`, `3869c92`, `a346273`,
`f83be8d`. Merged to main in order; each closure commit drops its
`t.Skip("AUDIT-NNN")` lines and removes the IDs from the allowlist.

**What broke.** The FIX-VERIFICATION.md report on the prior Code Red campaign
found two CONDITIONAL items:

- **29 AUDIT IDs on the allowlist were pattern-covered but not actually
  closed.** The label "Fix #8 pattern-covered (P1)" covered only the three
  terminator signatures; the `rows.Scan` class (AUDIT-090/091/094/095),
  exec.CommandContext class (AUDIT-127/158/165), and others remained open
  at 30+ production sites.
- **Pattern P7 was effectively downgraded.** Both P7 subtests
  (`TestPattern_P7_ConcurrentCancelVsApproveRace`,
  `TestPattern_P7_ResetTaskResurrectsCompleted`) skipped with a message
  referencing a `UpdateBountyStatusFrom(id, from, to)` function that was
  never written anywhere in the tree.

Plus the Fix #8b "complete" claim left **8 bare hot-path terminator calls**
(pilot_worktree_reset.go ×6, medic_ci.go:170, astromech.go:601) and **~28
`_ = store.*` discards** in production without the required deferral
marker. Plus the cmd/force captureOutput race under `-race -count=1`. Plus
the Chancellor SEQUENCE/MERGE empty-subfield fail-open path.

**What shipped.** 9 tracks across 6 commits:

- **Track A (`acc6a92`)** — `UpdateBountyStatusFrom(db, id, from, to) (int64,
  error)` added. `ResetTask`/`ResetTaskFull`/`CancelTask` refuse `Completed`/
  `Cancelled` sources via `AND status NOT IN (...)`. Jedi Council's
  approve paths migrated to source-status CAS. P7 pattern tests unskipped
  and rewritten stronger (assert `approveRowsAffected==0` explicitly,
  not just "no clobber"). Plus AUDIT-156 (internal/git `.Run()` error
  logging via `bestEffortRun` helper) and AUDIT-159 (defer rows.Close in
  dogGitHygiene).
- **Track B (`86ee261`)** — 8 bare hot-path terminators migrated to guarded
  form. 28 `_ = store.*` discards audited: each either migrated or
  tagged with the exact `// deferral-comment(Fix #8b): propagate error —
  <mechanism>` marker. AUDIT-015 (onSubPRMerged returns error +
  escalates idempotently via Fix #3 partial UNIQUE). AUDIT-040
  (escalateCITriage drops redundant manual UPDATE). AUDIT-042/043
  (UpdateAskBranchPRChecks + MarkAskBranchPRClosed guards). AUDIT-045
  (PRAGMA busy_timeout post-Open). AUDIT-046 (mergeMus sync.Map per
  filepath.Clean(repoPath)). AUDIT-047 (per-dog 5m context.WithTimeout +
  Dogs.heartbeat_at column + DogMarkHeartbeat; Inquisitor tick-level
  15m timeout). AUDIT-066 (pruneFleet `?` placeholder). AUDIT-068 (Claim
  helpers log non-ErrNoRows errors via stdlib log). AUDIT-069
  (ResolveFeatureBlockers per-convoy sequence wrapped in db.Begin tx,
  uses AddDependencyTx + ClearConvoyHoldTx). AUDIT-092 (gh Kill+drain
  5s time.After backstop). AUDIT-093 (claude cmd.WaitDelay=5s).
  AUDIT-096 (rateLimitRetries CAS/LoadOrStore + pruneRateLimitRetries
  via inquisitor tick). AUDIT-097 (ResetBranchPrefixCache uses
  usernameCached bool, not sync.Once reassignment). AUDIT-125/126
  (heartbeat+tasklog defers). AUDIT-129 (textBuf.Len() < 409600 cap +
  truncate marker). AUDIT-151 (WorktreeReset captures RowsAffected;
  0-row logs + CreateEscalation + mails operator). AUDIT-155 (union-
  merge acquires lockRepoForMerge). AUDIT-158 (astromech
  runShortGit/combinedShortGit helpers). AUDIT-164 (defer signal.Stop
  already in place; skip removed).
- **Track C (`9f32afe`)** — ~50 rows.Scan sites migrated to error-checked
  form across cmd/force, internal/agents, internal/dashboard,
  internal/store. New TestPattern_P1_RowsScanErrorsChecked guard.
  AUDIT-090 (dogStalledReviews scan errors logged). AUDIT-091
  (dogGitHygiene returns Agents-query error). AUDIT-094 (astromech
  ownership-check distinguishes Exec error from rows=0). AUDIT-095
  (Diplomat classifies Claude error, mails operator on permanent
  fallback). AUDIT-099 (atomic attributes rewrite via tmp + os.Rename,
  SIGINT/SIGTERM handler restoration). AUDIT-100 (worktreeBase 0700 +
  taskLogPath chmod 0600).
- **Track D (`3869c92`)** — internal/git migrated to exec.CommandContext
  with 5-min timeouts via bestEffortRun/runGitCtx/runGitCtxOutput
  helpers. 0 bare exec.Command("git",...) remain in git.go/askbranch.go
  (11 CommandContext instead). New TestPattern_P11_ExecCommandsUseContext
  guard with reason-gated allowlist.
- **Track F (`a346273`)** — AUDIT-137: TestEscalateSubPR_IsAtomic asserts
  escCountAfter==1 + re-run error.
- **Track G (`a346273`, `f83be8d`)** — cmd/force captureOutput race
  resolved. RunCommandCenter refactored into CLI entry that delegates to
  runCommandCenterTo(db io.Writer). Tests use io.Discard. captureOutput
  retains sync.Mutex. TestNewLogger_CreatesLogger softened for count>1.
- **Track H (`a346273`)** — Chancellor SEQUENCE/MERGE with empty required
  subfield now FailBounty + `[CHANCELLOR FAIL-CLOSED]` operator mail.
  New tests pin the contract.
- **Track I (`a346273`)** — AUDIT-130 (astromech checks
  Repositories.quarantined_at post-ClaimBounty). AUDIT-131 (UnmarshalText
  branch dropped). AUDIT-132 (handleSubPRPoll falls back + escalates;
  timeSinceCreatedAt returns 100y on parse fail, not 0).

**How it was proved.**

- Every AUDIT closure follows Red→Green→Refactor: the red-phase fails
  before the fix (captured output in the commit body), post-fix the
  same test passes. Pattern P7 red-phase: 20/20 clobbers, finalCompleted=20.
  Post-fix: 20/20 trials assert approveRowsAffected==0, finalCancelled=20.
- `grep -rn 't\.Skip(.AUDIT-' --include="*.go" internal/ cmd/ schema/`
  returns zero production hits (only comment references in the
  allowlist-enforcer file).
- `remainingAuditSkips` is empty.
- `grep -rn 'pattern-covered' --include="*.go"` returns 0 hits.
- Every `_ = store.*` in production carries the exact deferral-comment
  marker within 1 line (verified by awk).
- Zero bare terminator calls in hot-path files
  (pilot_worktree_reset/medic_ci/astromech) — verified by grep.
- `go test -tags sqlite_fts5 ./...` green.
- `go test -tags sqlite_fts5 -race -count=5 ./...` green after the
  logger-test softening.
- `make smoke` / `make fuzz` / `make test-audit` all green.
- FIX-8D-CLOSURE.md at repo root has the full verification paste.

**What to watch for next.**

- The `mergeMus sync.Map` allocates one mutex per unique repoPath. If the
  operator ever adds hundreds of repos this becomes measurable; in
  practice fleets have 1–10 repos so the overhead is trivial. If repos
  are ever registered/removed dynamically at scale, the map should grow
  a prune.
- The Inquisitor tick's 15-min context.WithTimeout (AUDIT-047) is inherited
  by downstream gh/claude calls only if they explicitly accept ctx. Most
  RunDogs dogs don't take ctx today. This is fine — the dog-level 5m
  timeout inside RunDogs covers the common case. But if a future dog
  explicitly wants Inquisitor's tick budget rather than its own, it has
  to thread the tickCtx through.
- `TestNewLogger_CreatesLogger`'s softened assertion intentionally accepts
  the sync.Once race on count>1. If anyone replaces NewLogger with a
  per-call `os.OpenFile`, the test should be tightened back to its
  original form.
- `escalateSubPR` uses a bare INSERT without ON CONFLICT DO NOTHING —
  relies on the partial UNIQUE to return an error on the second call
  (which AUDIT-137's test asserts). If someone changes that INSERT to
  `INSERT OR IGNORE`, the error disappears and the test breaks; either
  revert or match `CreateEscalation`'s DO UPDATE pattern.

**Restart gate met.** All 13 verification steps green; allowlist empty;
closure report filed at FIX-8D-CLOSURE.md.

