---
audience: operator
scope: Self-healing posture — operator-facing summary; load-bearing invariants live in the auto-rendered self-healing.md.
owner: D2 + Fix #6
last_reviewed: 2026-05-05
subsystem: self-healing
type: subsystem-doc
---

# Self-healing

The fleet's failure-handling philosophy is **self-healing is the default; escalation is the last step**. The operator-facing summary lives here; the load-bearing invariants are auto-rendered to [`../self-healing.md`](../self-healing.md) from `FleetRules`. **For the binding rules, read that doc.**

## Overview

Every new `fmt.Errorf(...)` or `FailBounty(...)` added during a PR-flow change must fall into one of nine self-heal classes (auto-retry / auto-fix / auto-bypass / auto-reshard / auto-retrigger / auto-complete-on-empty-diff / auto-cleanup on contamination / auto-resolve stale escalations / operator escalation). The Fleet Medic agent is the gating decision-maker: when a CodeEdit task exhausts retries or hits a permanent infra failure, Medic decides between `requeue` / `shard` / `escalate` based on the seance, rejection feedback, and last diff.

The operator's mental model: most failures self-heal in one or two retry cycles. The dashboard's **Escalations tab** is what surfaces failures Medic could not auto-resolve.

## Components

- **Fleet Medic** (`internal/agents/medic.go`) — `MedicReview` task claimer.
- **Medic-CI** (`internal/agents/medic_ci.go`) — `CIFailureTriage` for sub-PR CI failures.
- **`internal/agents/pr_flow.go`** — `onSubPRStalled` for CI-stall self-heal.
- **`internal/agents/divergence_detector.go`** — circular-commit detection (escalation path).
- **`internal/agents/reconcile.go`** — startup reconciliation; auto-recovers branch-missing-pre-Captain, queues `WorktreeReset` for dirty worktrees.
- **`escalation-sweeper` dog** (10 min) — auto-resolves stale Open escalations.
- **`spend-burn-watch` dog** (5 min) — auto-flips e-stop at the hard cap.
- **`task-spend-watch` dog** (1 min) — per-task spend escalation + `spend_suspended=1`.

## Invariants

The full set is auto-rendered to [`../self-healing.md`](../self-healing.md). Highlights:

### CI stall self-healing

`onSubPRStalled` runs when a sub-PR has been Pending CI longer than `subPRCIStaleLimit` (2h):

1. Past `subPRCIHardLimit` (6h) — escalate unconditionally.
2. Retrigger cap reached — escalate.
3. Any check `IN_PROGRESS` — wait. CI is slow, not stuck.
4. All checks QUEUED/PENDING or zero checks — push empty commit via `igit.TriggerCIRerun`, increment `stall_retrigger_count`.
5. Retrigger push fails — escalate with the git error.

Tests inject a stub via `SetTriggerStalledRerunForTest`.

### Duplicate task prevention

Spawned child tasks MUST be idempotent. Use `store.AddConvoyTaskIdempotent` / `store.AddIdempotentTask` whenever the spawn signal may fire more than once. Three partial UNIQUE indexes back the dedup:

- `idx_bounty_idem ON BountyBoard(idempotency_key) WHERE idempotency_key != '' AND status NOT IN ('Completed','Cancelled','Failed')`
- `idx_escalations_open_task ON Escalations(task_id) WHERE status = 'Open'`
- `idx_feature_blockers_open ON FeatureBlockers(blocked_convoy_id, blocking_feature_id) WHERE resolved_at IS NULL`

Canonical idempotency keys: `rebase-conflict:branch:<agent_branch>`, `rebase-conflict:askbranch:<ask_branch>`, `convoy-review:<convoyID>`, `worktree-reset:<parent_task_id>`, `rebase-agent:<sub_pr_row_id>`, `create-askbranch:<convoyID>`, `rebase-askbranch:<convoyID>:<repo>`, `pr-review-triage:<convoyID>`, `ci-failure-triage:<sub_pr_row_id>`.

### Bounded self-healing (Fix #6)

Every loop that re-invokes the same agent on the same object MUST carry a numeric cap on a stable object:

- **Medic requeue**: `BountyBoard.medic_requeue_count` ≤ 2.
- **Auto-shard on zero commits**: fires once `retry_count >= 2`.
- **Auto-reshard cascade**: `BountyBoard.reshard_generation` ≤ 2.
- **Ask-branch rebase conflict**: `ConvoyAskBranches.failed_rebase_attempts` ≤ 3.

When you add a new self-healing loop, add a cap. **Caps go on a stable object — never on an in-flight process.**

### Operator-escalation rule

If the remedy can be written as a sequence of shell commands, it is **NOT** an escalation. Medic's prompt explicitly forbids escalating for worktree hygiene or already-completed work.

## Configuration

SystemConfig knobs that shape self-heal behaviour:

- `maxMedicRequeues` (2), `maxReshardGeneration` (2), `maxAskBranchConflicts` (3).
- `subPRCIStaleLimit` (2h), `subPRCIHardLimit` (6h), `subPRMaxStallRetriggers`.
- `hourly_spend_cap_usd` (25), `hourly_spend_estop_usd` (200).
- `per_task_spend_alert_usd` (5), `per_task_spend_escalate_usd` (15).
- `agent_max_prompt_bytes_default` (200000) — `[CONTEXT OVERFLOW]` triggers `librarian.SummarizeForContextOverflow`.

Mail subjects (stable for filter rules):

- `[RECONCILE]` — startup divergence + recovery action.
- `[TASK SPEND ANOMALY]` / `[TASK SPEND ESCALATE]`.
- `[INBOUND REDACT]` — count, never content.
- `[CIRCULAR COMMITS]` — divergence detector.
- `[CHANCELLOR FAIL-CLOSED]` — Chancellor parse/Claude fail.
- `[CONTEXT OVERFLOW]` — agent prompt-cap overflow.
- `[CONVOY REVIEW PASSED]`.

## Operator surface

```bash
force escalations                    # Open escalations the fleet couldn't auto-resolve
force escalations requeue <id>       # close escalation and return task to Pending
force dogs                           # cooldown + last-run for every dog
force doctor                         # pre-flight: git/claude/repo paths/DB integrity/e-stop/blocked tasks
```

Dashboard:
- **Escalations tab** — Medic's `escalate` verdicts surface here as cards.
- **High-escalation banner** — `#high-esc-banner` becomes visible at three open HIGH-severity escalations (AUDIT-064 threshold).

When something looks wrong:
1. `force escalations list Open` — what is the fleet asking the operator to adjudicate?
2. `force dogs` — are the safety dogs healthy?
3. `force doctor` — is the environment intact?

## See also

- [`../self-healing.md`](../self-healing.md) — auto-rendered self-healing invariants (the binding contract).
- [`escalation-and-medic.md`](escalation-and-medic.md) — Medic verdicts in detail.
- [`pr-flow.md`](pr-flow.md) — CI-stall self-heal sits in the PR flow.
- `dogs.md` (planned) — every dog and its cooldown.
- [`../FIX-LOG.md`](../../FIX-LOG.md) — Fix #6 (bounded self-healing) and Fix #8 (no silent failures) narratives.
