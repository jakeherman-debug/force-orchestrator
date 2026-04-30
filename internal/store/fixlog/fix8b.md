## Fix #8b — Convoy + Commander error-propagation sweep

**AUDIT IDs closed:** none directly (these files carried no AUDIT-marked
TODOs — the 21 markers were seeded by Fix #8a as a worklist for the Phase B
per-package sweep). Contributes to the 108-marker countdown tracked by
`grep -r "TODO(Fix #8b)" internal/`.

**Branch:** `fix/8b-convoy-review-errors`

**Scope.** Fix #8a established the three store-boundary signatures
(`UpdateBountyStatus`, `FailBounty`, `CreateEscalation` — all now return
`error`) and updated five hot-path callers. This campaign sweeps the
convoy/commander pair — 21 of the 108 placeholder markers left for Phase B:

- `internal/agents/convoy_review.go` — 15 markers in `runConvoyReview` and
  its parse-failure retry block. All sit inside a void-returning spawner
  invoked from Diplomat's claim loop, so option (2) (log with recovery
  hint) applies uniformly.
- `internal/agents/commander.go` — 6 markers in `runCommanderTask`. Same
  void-spawner shape; same option (2) pattern.

**What shipped.**

- Every `_ = store.FailBounty(...)`, `_ = store.UpdateBountyStatus(...)`,
  and `_, _ = CreateEscalation(...)` in the two files is now wrapped in
  `if err := ...; err != nil { logger.Printf(..., err); }` with a
  recovery-hint clause drawn from the CLAUDE.md-sanctioned vocabulary:
  - `store.FailBounty` / `CreateEscalation` failures → "stale-lock detector
    will recover" (same idiom as `jedi_council.go` / `medic.go` under Fix
    #8a).
  - `store.UpdateBountyStatus(..., "Completed")` failures in ConvoyReview →
    "convoy-review-watch will retry" (the 5-min dog is the re-trigger path
    for a ConvoyReview row that failed to reach terminal state).
- Each log line names the specific call-site (e.g.
  `"FailBounty(loop cap)"`, `"CreateEscalation(conflicted_loop, convoy
  %d)"`, `"UpdateBountyStatus(Completed, active-tasks defer)"`) so a
  grep of the log file points straight at the code path that failed,
  not just the store function.
- `runCommanderTask`'s final `UpdateBountyStatus(..., "AwaitingChancellorReview")`
  additionally `return`s on error — if the task didn't transition to
  `AwaitingChancellorReview`, we must NOT proceed to `SendMail` or record
  success in `TaskHistory`. The stale-lock detector re-claims the row.
- Two incidental bare `store.FailBounty(db, ...)` calls in
  convoy_review.go (the retry-fail and conflicted-loop escape hatches —
  no `_ =` TODO marker, but still discarding error) are also wrapped.

**How it was proved.**

- `TestFix8B_ConvoyReview_FailBountyErrorSurfacesToLogger` — drops the
  BountyBoard table after seeding a ConvoyReview bounty with `convoy_id=0`,
  runs `runConvoyReview`, asserts the logger output names the FailBounty
  call-site AND the "stale-lock detector will recover" recovery hint.
- `TestFix8B_ConvoyReview_UpdateBountyStatusErrorSurfacesToLogger` — seeds
  a convoy with no ask-branches so runConvoyReview hits the "complete as
  clean" UpdateBountyStatus branch, drops BountyBoard to force the call
  to error, asserts the logger names the call-site AND the
  "convoy-review-watch will retry" hint.
- `TestFix8B_Commander_FailBountyErrorSurfacesToLogger` — seeds a Feature
  bounty but zero repos so loadRepoContext errors out, drops BountyBoard
  to force FailBounty's UPDATE to fail, asserts both the call-site label
  and the "stale-lock detector will recover" hint land in the logger.
- `TestFix8B_ConvoyReview_NoSilentFailuresGrep` — grep-based regression
  guard; fails if any `TODO(Fix #8b)` marker re-appears in the two files.

**Stats.**

- 21 `TODO(Fix #8b)` markers removed (convoy_review.go 15 + commander.go 6).
- 2 incidental bare-`store.FailBounty(...)` sites hardened.
- 4 new tests (`internal/agents/fix8b_convoy_commander_error_propagation_test.go`).
- Zero signature changes outside the two files — nothing to defer.

**Watch for.**

- `runCommanderTask`'s final `UpdateBountyStatus("AwaitingChancellorReview")`
  now short-circuits on error. If an operator reports Features that
  reached `Commander` but never landed the `[PLANNED]` mail, the logger
  will have the specific call-site line — search for "UpdateBountyStatus(
  AwaitingChancellorReview) failed" in the daemon log.
- The recovery-hint vocabulary is load-bearing. "convoy-review-watch will
  retry" is only correct for ConvoyReview rows that failed to reach
  terminal status — the 5-min dog finds them via the
  `status NOT IN ('Completed','Cancelled','Failed')` gate and re-queues.
  If the bug moves to a FailBounty call-site, switch the hint to
  "stale-lock detector will recover" (the 45-min sweep).
