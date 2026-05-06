---
audience: both
scope: Failure paths — Medic triage, escalation creation, auto-resolve, the "self-healing default" rule.
owner: D2 + Fix #6 + Fix #8
last_reviewed: 2026-05-05
subsystem: escalation-and-medic
type: subsystem-doc
---

# Escalation and Medic

When a task fails, Force does not silently log and continue. Every failure terminates in either a self-heal action OR an explicit `Escalations` row that the operator must adjudicate. The architecture invariant is **self-healing is the default; escalation is the last step.** The Fleet Medic is the agent that decides which.

## Overview

When a CodeEdit task exhausts its retry budget or hits a permanent infra failure, the daemon's failure handler does not write the operator directly. Instead it spawns a `MedicReview` task. The Medic claims that task, examines:

- The full attempt history (the seance — every prior Claude run, full output).
- All Council / Captain rejection feedback.
- The last git diff (if any commits landed).
- Any `Escalations` rows already linked to this task.

…and renders one of three verdicts:

| Verdict | Action | When |
|---|---|---|
| `requeue` | Reset task to Pending; mail astromechs corrective guidance. | Task valid but needed clearer guidance. |
| `shard` | Cancel the original; insert 2-5 focused sub-tasks. | Task too broad for one agent. |
| `escalate` | Create an `Escalations` row; mail the operator. | Architectural ambiguity / missing dependency / problem a coding agent cannot resolve. |

The Medic is biased toward `requeue` or `shard`. `escalate` is a last resort — its prompt explicitly forbids escalating for worktree hygiene or already-completed work.

## Components

- **`internal/agents/medic.go`** — Medic claim loop, `MedicReview` task type, decision rendering.
- **`internal/agents/medic_ci.go`** — `Medic-CI` (`CIFailureTriage`) — sibling agent that handles CI-failure classification.
- **`internal/store/escalations.go`** — `CreateEscalation`, status taxonomy.
- **`Escalations` table** — the row-level escalation registry.
- **`escalation-sweeper` dog** (10 min) — closes Open escalations whose underlying task has reached `Completed`/`Cancelled` or whose sub-PR has merged.
- **`internal/agents/spawn_reshard.go`** — `queueReshardDecompose` for permanent infra failures.
- **`internal/agents/divergence_detector.go`** — circular-commit escalation path.
- **`autoShardIfNoCommits`** in `pr_flow.go` — fires once `retry_count >= 2`.

## Invariants

1. **No silent failures.** Every error path terminates in `store.FailBounty(...)`, `store.UpdateBountyStatus(...)`, or `CreateEscalation`. Never `log.Printf` an error and continue.
2. **New store mutators return `error`.** Do not add another void-return terminator.
3. **Self-heal classes** (every new `fmt.Errorf(...)` or `FailBounty(...)` falls into one):
   - **Auto-retry**: `ErrClassTransient` / `ErrClassRateLimited` (in `internal/gh/gh.go`). Pilot's retry wrapper handles.
   - **Auto-fix**: Medic `CIFailureTriage` spawns a CodeEdit fix. Cap 3 attempts per PR.
   - **Auto-bypass**: repo `pr_flow_enabled=0` or `quarantined_at` stamped.
   - **Auto-reshard**: permanent infra failure → `Decompose` bounty to Commander via `queueReshardDecompose`. Idempotent per failed task.
   - **Auto-retrigger**: CI stalls per-check-state diagnosis. All-QUEUED → `igit.TriggerCIRerun` push, capped at `subPRMaxStallRetriggers`.
   - **Auto-complete-on-empty-diff**: Medic checks `GetDiff` + `CommitsAhead` BEFORE calling Claude.
   - **Auto-cleanup on contamination**: Medic emits `decision=cleanup` → spawn `WorktreeReset` infra for Pilot.
   - **Auto-resolve stale**: `escalation-sweeper` closes Open escalations whose task transitioned to terminal.
   - **Operator escalation**: `CreateEscalation` + operator mail. **If the remedy can be written as a sequence of shell commands, it is NOT an escalation** — Medic's prompt explicitly forbids escalating for worktree hygiene or already-completed work.
4. **Bounded self-healing** (Fix #6). Every loop that re-invokes the same agent on the same object MUST carry a numeric cap on a stable object (never on an in-flight process):
   - **Medic requeue**: `BountyBoard.medic_requeue_count` ≤ `maxMedicRequeues` (2). `ResetTaskFull` PRESERVES `retry_count` and `infra_failures` (zeroing them was AUDIT-005).
   - **Auto-shard on zero commits**: fires once `retry_count >= 2`.
   - **Auto-reshard cascade**: `BountyBoard.reshard_generation` ≤ `maxReshardGeneration` (2).
   - **Ask-branch rebase conflict**: `ConvoyAskBranches.failed_rebase_attempts` ≤ `maxAskBranchConflicts` (3).
5. **`Escalations.status`** only takes documented values (Pattern P6). No `'Resolved'` regression.
6. **CAS-safe transitions** (Pattern P7). `ResetTask` / `ResetTaskFull` / `CancelTask` refuse to resurrect terminal rows. Council's approve path uses CAS so a concurrent operator cancel is never silently clobbered.
7. **Idempotent escalation creation.** `CreateEscalation` merges on conflict; the partial UNIQUE index `idx_escalations_open_task ON Escalations(task_id) WHERE status = 'Open'` enforces one open escalation per task.

## Configuration

SystemConfig knobs:

- `maxMedicRequeues` (default 2) — Medic requeue cap.
- `maxReshardGeneration` (default 2) — auto-reshard cascade cap.
- `maxAskBranchConflicts` (default 3) — ask-branch conflict cap.
- `subPRMaxStallRetriggers` — CI-stall retrigger cap.
- `subPRCIStaleLimit` (2h) — when to trigger stall diagnosis.
- `subPRCIHardLimit` (6h) — escalate unconditionally past this.

Mail subjects (stable for filter rules):
- `[ESCALATION]` — new escalation created.
- `[TASK SPEND ESCALATE]` — per-task spend escalation.
- `[CIRCULAR COMMITS]` — divergence detector tripped.
- `[CHANCELLOR FAIL-CLOSED]` — Chancellor parse/Claude failure path.
- `[CONTEXT OVERFLOW]` — agent prompt-cap overflow.

## Operator surface

```bash
force escalations                              # list Open escalations
force escalations list [Open|Acknowledged|Closed]
force escalations ack <id>                     # mark seen, do not re-queue
force escalations close <id>                   # close without re-queue
force escalations requeue <id>                 # close and return task to Pending
```

Dashboard:
- **Escalations tab** — cards filtered Open / Closed / All. Per-card actions: Acknowledge / Close / Close & Requeue.
- **High-escalation banner** — `#high-esc-banner` becomes visible when `status.high_escalations >= 3` (AUDIT-064 threshold). A self-healing breakdown is visible without scrolling.

When you see a HIGH escalation:

1. Read the agent message: `force escalations list Open`.
2. Open the originating task: `force logs <task_id>` and `force history <task_id>`.
3. If it's a worktree-hygiene problem, the Medic should have caught it; spawn a `WorktreeReset` manually if needed.
4. If it's a missing dependency / architectural ambiguity, decide: amend the task payload and `force escalations requeue <id>`, or `force escalations close <id>` if no longer actionable.

## See also

- [`gas-town.md`](gas-town.md) — `Escalations` is part of the coordination substrate.
- [`pr-flow.md`](pr-flow.md) — many escalations originate in the PR flow path.
- [`convoy-lifecycle.md`](convoy-lifecycle.md) — ConvoyReview parse-failure escalation path.
- `dogs.md` (planned) — `escalation-sweeper` and other sweeper dogs.
- [`../self-healing.md`](../self-healing.md) — auto-rendered self-healing invariants.
- [`../FIX-LOG.md`](../../FIX-LOG.md) — Fix #6 (bounded self-healing) and Fix #8 (no silent failures) narratives.
