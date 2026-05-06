---
audience: both
scope: Diplomat — the draft-PR opener that ships a convoy by populating the repo's PR template, running a sanity pass, and queueing ConvoyReview / PRReviewTriage.
owner: D13
last_reviewed: 2026-05-05
---

# Diplomat — Draft-PR Opener

## Role

Diplomat handles the final ship step: once a convoy's sub-PRs are all merged, Diplomat rebases the ask-branch onto main, populates the repo's PR template via Claude, runs a sanity pass (secret scan + placeholder check + length limit), and opens a **draft PR** against main. Immediately after the draft PR is created, Diplomat queues a `ConvoyReview` task. Sanity-pass failures trigger one LLM retry with critic feedback; a second failure escalates. Diplomat also claims `ConvoyReview` and `PRReviewTriage` tasks in its work loop — these are convoy-level quality gates, not new delivery steps. Human operators review the draft PR on GitHub and click *Ship it* in the dashboard (or merge directly) after `ConvoyReview` passes.

Roster: Leia-Organa, Padme-Amidala, Bail-Organa.

## Responsibilities

- Claim `Diplomat` (open-draft-PR) bounties.
- Claim `ConvoyReview` bounties — runs one LLM pass over the full ask-branch diff vs main, checking it against every commissioned task. Gaps / regressions / incorrect changes each spawn a `CodeEdit` fix task on the ask-branch. The `convoy-review-watch` dog re-triggers a fresh pass once those fixes land. The loop terminates when a pass returns clean.
- Claim `PRReviewTriage` bounties — handles inbound PR-review comments and routes them to fix tasks or replies.
- Run the sanity-pass critic loop (one retry, then escalate).
- Persist the opened draft PR row so the human ship-it / dashboard surfaces have what they need.

## Capability profile

Diplomat loads three profiles at spawn time:

- [`agents/capabilities/diplomat.yaml`](../../agents/capabilities/diplomat.yaml) — primary draft-PR open flow.
- [`agents/capabilities/pr-review-triage.yaml`](../../agents/capabilities/pr-review-triage.yaml) — `PRReviewTriage` claim path.
- [`agents/capabilities/convoy-review.yaml`](../../agents/capabilities/convoy-review.yaml) — `ConvoyReview` claim path; the adversarial sibling is [`agents/capabilities/convoy-review-critic.yaml`](../../agents/capabilities/convoy-review-critic.yaml).

All three are loaded via `capabilities.LoadProfile(...)` in `internal/agents/diplomat.go`.

## Key files

- `internal/agents/diplomat.go` — `SpawnDiplomat(ctx, db, name)` and the multi-profile claim loop.
- `internal/agents/diplomat_pr_handoff.go` — sanity-pass + draft-PR open + handoff bookkeeping.
- `internal/agents/diplomat_consumer_integration.go` — integration of the `ConvoyReview` / `PRReviewTriage` consumers.
- `internal/agents/convoy_review.go` — `ConvoyReview` runner used from inside Diplomat's claim loop.
- `internal/agents/convoy_verification_spec.go` — verification-spec assembly used by `ConvoyReview`.
- `internal/agents/pr_review_triage.go`, `pr_review_poll.go`, `pr_review_resolve.go` — PR-review-triage subsystem.
- `agents/capabilities/diplomat.yaml`, `pr-review-triage.yaml`, `convoy-review.yaml`, `convoy-review-critic.yaml` — capability profiles.

## Tests

- `internal/agents/diplomat_test.go`, `diplomat_pr_handoff_test.go`, `diplomat_consumer_integration_test.go` — Diplomat unit + integration coverage.
- `internal/agents/convoy_review_test.go`, `convoy_review_cycle_test.go`, `convoy_review_per_stage_test.go`, `convoy_review_supply_gate_test.go`, `convoy_review_fix7_test.go` — `ConvoyReview` behavior.
- `internal/agents/pr_review_triage_test.go`, `pr_review_poll_test.go`, `pr_review_resolve_test.go` — PR-review-triage behavior.
- `internal/audittools/audit_pattern_p13_capability_profiles_test.go` — capability profile invariant (Diplomat loads three profiles; all enforced).
- `internal/audittools/audit_pattern_p31_llm_transcripts_test.go` — Diplomat / ConvoyReview LLM calls write transcripts.
- `internal/audittools/audit_pattern_p32_git_ops_test.go` — Diplomat's git ops discipline (rebase, push, draft-PR open).

## See also

- [`docs/agents/pilot.md`](pilot.md) — upstream gate; Pilot finishes branch / rebase work before Diplomat opens the draft PR.
- [`docs/agents/council.md`](council.md) — Council clears the sub-PRs whose merge satisfies Diplomat's "all sub-PRs merged" precondition.
- [`docs/agents/medic.md`](medic.md) — handles `CIFailureTriage` / `CodeEdit` tasks Diplomat spawns from a `ConvoyReview` clean-pass failure.
- [`docs/architecture/claude-cli-invocation.md`](../architecture/claude-cli-invocation.md) — Diplomat is one of the review agents that runs with daemon CWD = `force-orchestrator/`.
