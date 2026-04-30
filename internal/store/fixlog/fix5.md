## Fix #5 — Stale-convoys terminal-status correction

**AUDIT IDs closed:** AUDIT-012 (primary); AUDIT-087 (secondary — convoy
UPDATE source-status guard). Tests still pending separate fixes: AUDIT-025
(Resolved→Closed escalation normalization), AUDIT-083 (ConflictPending trap
state sweep), AUDIT-084 (AwaitingChancellorReview stale-lock flow),
AUDIT-149 (escalation-sweeper auto_resolve_count), AUDIT-166
(`ReleaseInFlightTasks` / `locked_at` carry-over). The P6 pattern test's
outer and sub-test A skips are removed; B and C retain their inner skips.

**Branch:** `fix/stale-convoys-terminal-check`

**What broke.** `runStaleConvoysReport` in
`internal/agents/dogs.go` scanned Active convoys and checked for "all
tasks terminal" using the predicate `status NOT IN ('Completed',
'Cancelled')`. `Failed` and `Escalated` fell OUTSIDE that set — meaning a
convoy whose tasks were permanently failed was treated the same as one
whose tasks had all merged successfully. The dog would then unconditionally
`UPDATE Convoys SET status = 'Completed'` and mail the operator a
`[CONVOY COMPLETE]` note. Downstream: no ShipConvoy ever fires (the
success path is wired to `CheckConvoyCompletions` going through
`AwaitingDraftPR`), fleet memory records success, the operator sees a
green card that doesn't correspond to any merged work. AUDIT-012 flagged
this exact class of silent false-positive.

Secondary: the UPDATE was unguarded — `WHERE id = ?` with no source-status
clause — so a race with `CheckConvoyCompletions` (which also transitions
Active convoys) could flip a convoy back and forth across ticks (AUDIT-087).

**What shipped.** `runStaleConvoysReport` rewritten with three behaviour
changes:

1. The non-terminal predicate now excludes the full terminal set:
   `status NOT IN ('Completed', 'Cancelled', 'Failed', 'Escalated')`. A
   convoy is only eligible for a terminal transition once every child has
   reached one of those four statuses.

2. The "mark Completed" branch is split. Before the UPDATE, a second
   query counts `status IN ('Failed','Escalated')` for the convoy. If
   that count is zero (all children are `Completed`/`Cancelled`), the
   convoy transitions to `Completed` with the existing `[CONVOY COMPLETE]`
   info mail. If it's non-zero, the convoy transitions to `Failed` with a
   `[CONVOY FAILED]` alert mail whose body includes the first child's
   `error_log` and the `force convoy show`/`force convoy reset`
   remediation commands — mirroring `CheckConvoyCompletions`'s
   `[CONVOY STALLED]` format so operator inbox filters and dashboards
   treat the two paths identically.

3. Both UPDATE statements now carry `AND status = 'Active'` as a source-
   status guard — aligns with AUDIT-087's Fix recommendation and makes
   the dog safe against concurrent writers (CheckConvoyCompletions,
   AutoRecoverConvoy).

Duplicate-mail suppression: the Failed-mail path counts unread mail with
the same subject before inserting — consistent with `CheckConvoyCompletions`.
Running the dog twice on an already-Failed convoy is a no-op.

**How it was proved.**

- `TestPattern_P6_UndocumentedStatusValues/A_*` (AUDIT-012 static AST
  grep) — outer + sub-A `t.Skip` removed; test now green.
- `TestStaleConvoysReport_AllFailedTasksTransitionsToFailed` — integration;
  all-Failed + all-Escalated convoy transitions to `Failed`, mail is
  `MailTypeAlert` with subject `[CONVOY FAILED] …` and the first child's
  error_log embedded in the body.
- `TestStaleConvoysReport_MixedCompletedAndFailedTransitionsToFailed` —
  integration; 3-Completed + 1-Failed transitions to `Failed`, and NO
  `[CONVOY COMPLETE]` mail is sent (that specific masking is the bug).
- `TestStaleConvoysReport_FullLoopFromPendingToFailedDoesNotShipConvoy` —
  feature; drives a convoy from all-Pending (Active, no-op) → all-Failed
  (Failed + operator mail) → second run (idempotent). Explicitly asserts
  no ShipConvoy task is queued at any point — a false-success regression
  would be caught by that invariant.
- Full suite: `go test ./... -tags sqlite_fts5 -timeout 300s -count=1`
  green (`cmd/force`, `internal/agents` ≈209s, `internal/store`, …).

**Stats.**

- 1 file changed in production (`internal/agents/dogs.go`, ~70 lines net).
- 3 new integration/feature tests in `internal/agents/dogs_test.go`.
- 1 P6 pattern test (outer + sub-A) flipped from Red to Green.
- 5 pre-existing stale-convoys tests still pass without modification.

**Scope explicitly NOT included.**

- AUDIT-025 (Resolved→Closed normalization of Escalations.status) — P6
  sub-test B still skipped. Needs a separate fix to collapse the three
  sink sites (`escalation_sweeper.go`, `medic.go`, `pilot_worktree_reset.go`)
  onto `'Closed'` with `acknowledged_at` as the marker.
- AUDIT-083 (ConflictPending trap state) — requires a dog or
  escalation-sweeper extension to check children of ConflictPending tasks.
- AUDIT-084 (AwaitingChancellorReview stale-lock flow) — requires
  special-casing in the inquisitor's stale-lock sweep.
- AUDIT-085 (dashboard ActiveCount SQL) — P6 sub-test C still skipped;
  dashboard-side change.
- AUDIT-149 (escalation-sweeper auto_resolve_count) — schema column +
  sweeper gate; spotcheck test still skipped.
- AUDIT-166 (`ReleaseInFlightTasks` locked_at clearance) — store-side
  fix, unrelated to stale-convoys dog.

These were bundled in the Fix #5 task-ticket under "P6 pattern covers
several of these." The stale-convoys change genuinely closes AUDIT-012
and AUDIT-087; the others need their own code passes and remain red.

**Watch for.**

- `CheckConvoyCompletions` (the Inquisitor's per-cycle check) and the
  stale-convoys dog both now apply the source-status guard. If either
  path is refactored to drop the guard, the race AUDIT-087 identified
  reopens. The regression tests here only cover the dog side — a future
  task should add parallel coverage for `CheckConvoyCompletions`.
- The stale-convoys dog is the last-resort safety net. The Inquisitor's
  `CheckConvoyCompletions` is the primary path. If the two disagree
  about a convoy's terminal state (e.g. Inquisitor ships on 4/4 while
  the dog sees 3-Completed + 1-Failed), the dog's `AND status = 'Active'`
  guard stops it from clobbering the Inquisitor's transition. This
  layering works only because of that guard.
