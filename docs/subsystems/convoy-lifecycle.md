---
audience: both
scope: Convoy lifecycle — Feature → ProposedConvoy → Convoy → ask-branches → ConvoyReview → DraftPROpen → Ship.
owner: PR-flow
last_reviewed: 2026-05-05
subsystem: convoy-lifecycle
type: subsystem-doc
---

# Convoy lifecycle

A **convoy** is the named group of tasks spawned from a single feature request. The convoy lifecycle is the spine of how Force delivers work end-to-end — from a `force add "…"` call to a merged PR. Every fleet primitive (Commander planning, Chancellor approval, ask-branches, sub-PRs, ConvoyReview, Diplomat ship, Pilot cleanup) hangs off the convoy state machine.

## Overview

The state graph (forward only — terminal states do not transition):

```
Feature task                       Convoy
─────────────                      ──────
Pending                            (none)
  │ Commander decomposes
  ▼
AwaitingChancellorReview           ProposedConvoys row written
  │ Chancellor approves
  ▼
Completed                          Active   ◄── CodeEdit tasks queued
                                     │
                                     │ Pilot creates ask-branch(es)
                                     ▼
                                   AskBranchOpen
                                     │ astromechs work, sub-PRs merge
                                     ▼
                                   AllSubPRsMerged
                                     │ Diplomat opens draft PR + queues ConvoyReview
                                     ▼
                                   DraftPROpen
                                     │ ConvoyReview passes clean
                                     ▼
                                   ReadyToShip
                                     │ operator clicks "Ship it"
                                     ▼
                                   Shipped     ◄── Pilot cleans up branches
```

Every transition is a row mutation in `Convoys.status` (sometimes via `SetConvoyStatus`, sometimes via the per-stage handler). The Gas Town pattern means every state change is observable in `holocron.db` without instrumentation.

## Components

- **`Convoys` table** — one row per convoy with `status`, `ask_branch_base_sha`, `feature_id`, …
- **`ConvoyAskBranches` table** — per-(convoy, repo) integration branches (a convoy may touch multiple repos).
- **`ProposedConvoys` table** — Commander-drafted plans awaiting Chancellor review.
- **`ConvoyReviewCycles` table** — one row per ConvoyReview pass (loop cap, fingerprint, decision).
- **Commander** (`internal/agents/commander.go`) — decomposes Features into CodeEdit subtasks.
- **Chancellor** (`internal/agents/chancellor.go`) — single-instance approval gate; rules APPROVE / SEQUENCE / REJECT / MERGE.
- **Pilot** (`internal/agents/pilot.go`) — git steward: `CreateAskBranch`, `RebaseAskBranch`, `CleanupAskBranch`, `FindPRTemplate`.
- **Diplomat** (`internal/agents/diplomat.go`) — opens draft PR, queues ConvoyReview, claims `PRReviewTriage`.
- **ConvoyReview** (`runConvoyReview` in `internal/agents/convoy_review.go`) — completeness gate over the full ask-branch diff vs main.
- **Dogs** that drive convoy events:
  - `convoy-review-watch` (5 min) — re-triggers ConvoyReview for `DraftPROpen` convoys when fix tasks complete.
  - `main-drift-watch` (15 min) — rebases ask-branches when main moves.
  - `draft-pr-watch` (5 min) — polls draft PRs into main for state.
  - `sub-pr-ci-watch` (5 min) — Jenkins CI on sub-PRs against the ask-branch.
  - `pr-review-poll` (5 min) — bot + human review comments on the draft PR.
  - `ship-it-nag` (24h) — reminds operator if a draft PR sits unshipped 24h / 72h / 1w.

## Invariants

1. **Single Chancellor instance.** Deliberate serialization point. `num_chancellor` is fixed at 1. Two Chancellors approving conflicting plans simultaneously is the failure mode Chancellor exists to prevent.
2. **Ask-branch required.** Once `Convoys.ask_branch != ''`, all new tasks in that convoy MUST branch off the ask-branch. `PrepareAgentBranch` enforces.
3. **Drift-detection invariant.** Whenever an ask-branch is rebased, `Convoys.ask_branch_base_sha` MUST be updated in the same operation.
4. **Human-gate invariant.** The draft PR into main NEVER auto-merges. The "Ship it" button is the one and only path.
5. **ConvoyReview loop cap.** Past 5 completed passes, `runConvoyReview` escalates (SeverityHigh) instead of spawning more fix tasks.
6. **Fingerprint dedup across passes.** SHA-256 over sorted per-finding hashes; same fingerprint as a prior Completed pass → escalate (`conflicted_loop`). Fix #7.
7. **Clean-pass gate.** Once any prior pass returns "clean", subsequent passes may only verify regressions. New findings after a clean pass → escalate Medium.
8. **Ask-branch conflict gating.** When a convoy's ask-branch has an unresolved `REBASE_CONFLICT` CodeEdit, `runConvoyReview` and `dogConvoyReviewWatch` defer via `store.HasActiveAskBranchConflict(db, convoyID)`.
9. **Convoy-scoped queries use `convoy_id` column** (Pattern P3). Never `payload LIKE '%"convoy_id":N%'`.
10. **Legacy fallback always available.** `pr_flow_enabled=0` on a repo sends it through the pre-PR-flow direct-merge path (`MergeAndCleanup`).

## Configuration

SystemConfig knobs:

- `pr_flow_enabled` — global (default true). Per-repo override on `Repositories.pr_flow_enabled`.
- `convoy_review_max_findings` (default 2) — cap on fix tasks spawned per ConvoyReview pass.
- `convoy_review_max_passes` (default 5) — escalate threshold.
- `pr_review_thread_depth_cap` (default 2) — emit `conflicted_loop` only at this depth or beyond.
- `pr_review_enabled` — global PR-review-triage kill switch.

Per-repo:

- `Repositories.pr_flow_enabled` — opt out of PR flow per repo.
- `Repositories.pr_review_enabled` — opt out of PR review triage per repo.
- `Repositories.pr_template_path` — populated by `FindPRTemplate`; used by Diplomat.

## Operator surface

```bash
force convoy list                              # all convoys with progress
force convoy show <id>                         # progress + dependency tree
force convoy approve <id>                      # activate Planned convoy
force convoy reset <id>                        # reset failed/escalated tasks in convoy
force convoy reject <id> <feedback>            # reject Commander's plan, requeue Feature
force convoy pr <id>                           # per-repo ask-branches + draft PR + sub-PR rollup
force convoy ship <id> [--merge squash|merge|rebase]   # promote draft from draft → ready
force convoy create <name>                     # manual convoy
```

Dashboard:
- **Convoys tab** — progress cards, filter All / Active / Completed × time window. Click drills into Tasks tab filtered by convoy.
- **Ship-it button** in convoy detail panel when `status='ReadyToShip'`.

When a convoy is stuck:

1. Check `force convoy pr <id>` for sub-PR or CI state.
2. Check `force escalations list Open` for blockers.
3. Check the `convoy-review-watch` and `draft-pr-watch` dog cooldowns: `force dogs`.
4. ConvoyReview parse failures escalate after 2 attempts (Fix #7); look for `[CHANCELLOR FAIL-CLOSED]`-style mail.

## See also

- [`pr-flow.md`](pr-flow.md) — operator-facing summary that links to the auto-rendered `pr-flow-invariants.md`.
- [`worktree-isolation.md`](worktree-isolation.md) — astromech branches off ask-branches.
- [`escalation-and-medic.md`](escalation-and-medic.md) — ConvoyReview escalation paths.
- `dogs.md` (planned) — convoy-driving dog cadences.
- [`../pr-flow-invariants.md`](../pr-flow-invariants.md) — auto-rendered ask-branch + ConvoyReview + PR-review invariants.
