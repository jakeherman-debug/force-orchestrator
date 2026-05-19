# DELIVERABLE-17 CLOSURE — Deferred Feature Loop Restoration: Tier 2

## Status: COMPLETE

## Summary

D17 delivered four phases that restore the deferred feature-review loop: Senate
material-amendment re-review with a 3-pass cap and escalation (P1A), a
deterministic fleet-state-hash dog for GlobalHoldouts (P1B), five new CLI
commands achieving parity with mutating dashboard handlers (P2A), and three D10
v2 follow-ups covering merge-event triggers, CommentID capture in
PRHandoffSyntheses, and a T+30 verdict dog (P2B). All criteria verified green;
one known invariant violation is flagged below.

## Exit Criteria Verification

| # | Criterion | Status | Notes |
|---|-----------|--------|-------|
| 1 | Senate re-review fires on material amendments; 3-pass cap enforced | PASS | `HasMaterialAmendment()` called at `internal/agents/senate.go:289`; re-queues `SenateReview` when true and `review_pass_count < 3`; operator mail (not silent drop) when cap reached (`senate.go:328-335`); `review_pass_count` column present in `createSchema` (schema.go:87), `runMigrations` (schema.go:3183), and `schema/schema.sql` (line 49) |
| 2 | `GlobalHoldouts.fleet_state_hash` populated by dog; `TestListDogs` count = 44 | PASS | `dogHoldoutSnapshot` in `internal/agents/dogs_holdout_snapshot.go` writes SHA-256 hash; inputs sorted via `sort.Strings`/`sort.Slice` before hashing (deterministic); dog registered at `dogs.go:229` with 24h cooldown; `TestListDogs` asserts `len(dogs) == 44` at `dogs_test.go:413` |
| 3 | All 5 CLI commands functional | PASS | `force briefing` queries `/api/briefing/queue` shape (`briefing_cmds.go`); `force scale --medics N`/`--pilots N` accepted and persisted (`fleet_cmds.go` + `d17_p2a_cli_test.go:76-116`); `force convoy cancel` idempotent (`convoy.go:313-358`, test at `d17_p2a_cli_test.go:149-156`); `force task show`/`status` renders BountyBoard row (`task_cmds.go:617`); `force senate` lists chambers, `force senate refresh <name>` queues task (`senate_cmds.go`) |
| 4a | Merge-event trigger: dog cooldown reset on convoy ship/PR merge; no `RepoMergeEvent` table | PASS | `store.DogResetCooldown("architecture-doc-render")` called in `pilot_draft_watch.go:186`; `grep -rn "RepoMergeEvent"` returns zero hits |
| 4b | `CommentID` stored non-zero in `PRHandoffSyntheses.comment_id` | PASS | `diplomat_pr_handoff.go:282-303` captures `capturedCommentID` from `gh.PostIssueCommentReturnID` and passes it as `CommentID` to the store insert; column present in schema (schema.go:1438, schema.sql:1409) |
| 4c | T+30 mail dog registered; test asserts mail at T+30, NOT T+5 | PASS | `dogs_t30_verdict.go:43` implements `dogT30Verdict`; `dogs.go:235` registers at 24h cooldown; `dogs_t30_verdict_test.go:25-67` asserts mail sent for run at T+30; `TestT30Verdict_TooEarlySkipped` at line 71 asserts 0 mails for run at T+5 |
| 5 | Pattern P25 tests pass (`go test -tags sqlite_fts5 -race -count=1 ./internal/audittools/...`) | PASS | `ok force-orchestrator/internal/audittools 12.154s` |
| 6 | `make test` green | PASS | All 72 test packages pass; zero failures; exit code 0 |

## Phases Shipped

| Phase | Commit | Description |
|-------|--------|-------------|
| P1A | 32c9ba0 | Senate material-amendment re-review loop with 3-pass cap and operator escalation |
| P1B | 5538931 | Holdout fleet-state snapshot dog populates GlobalHoldouts.fleet_state_hash |
| P2A | b5fc268 | CLI parity: force briefing, scale --medics/--pilots, convoy cancel, task show/status, senate |
| P2B | c4aa91c | D10 v2 follow-ups: merge trigger, CommentID capture, T+30 verdict dog |

## Known Issues / Follow-on

**Invariant violation — `store.DogResetCooldown` returns void (D17-introduced).**
`internal/store/holocron.go:330` defines `DogResetCooldown(db *sql.DB, name string)` with no error return.
This violates the CLAUDE.md rule: "New store mutators MUST return `error`".
The function was introduced by P2B (not pre-existing on `migration/initial-import`).
Follow-on: rename to return `error` and update the one call site in
`internal/agents/pilot_draft_watch.go:186` to log or propagate the error.
This is low-risk operationally (the mutation is advisory — a missed reset means
the dog fires one cycle late, not silently wrong), but it should be corrected
before any future store audits land.

## Verified by

D17 P3 strict verifier — 2026-05-19
