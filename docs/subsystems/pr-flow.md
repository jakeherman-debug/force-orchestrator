---
audience: operator
scope: PR-based delivery flow — operator-facing summary; load-bearing invariants live in the auto-rendered pr-flow-invariants.md.
owner: PR-flow
last_reviewed: 2026-05-05
subsystem: pr-flow
type: subsystem-doc
---

# PR flow

Force delivers code through GitHub PRs by default. The operator-facing summary lives here; the load-bearing invariants are auto-rendered to [`../pr-flow-invariants.md`](../pr-flow-invariants.md) from `FleetRules`. **For the binding rules, read that doc.**

## Overview

For each convoy:

1. **Pilot cuts an ask-branch** off main (e.g. `alice-smith/force/ask-7-oauth-support`) and pushes to origin — one per (convoy, repo). The leading `alice-smith/` is the operator's GitHub username, discovered via `gh api user --jq .login` → `gh config get user -h github.com` → `git config user.name`. Bare fallback for local setups.
2. **Astromechs branch off the ask-branch** (not main) to do their work.
3. **Jedi Council approval** opens a sub-PR against the ask-branch and marks for auto-merge once Jenkins CI is green.
4. **Medic handles CI failure** (`CIFailureTriage`): classifies Flaky / RealBug / Environmental / BranchProtection / Unfixable; retriggers or spawns a fix task.
5. **CI circuit breaker**: 5 Environmental failures in 1 hour → sub-PR creation pauses for 30 min.
6. **Pilot rebases ask-branches** as main drifts — `main-drift-watch` (15 min) uses `git ls-remote` first to avoid spending compute when main hasn't moved.
7. **Diplomat opens the draft PR** into main once all sub-PRs merged and ask-branch CI green. PR body is LLM-populated from the repo's `pull_request_template.md` plus convoy summary; pre-post sanity pass for secrets and unfilled placeholders.
8. **ConvoyReview** runs over the full ask-branch diff vs main. Gaps / regressions / incorrect changes spawn CodeEdit fix tasks on the ask-branch. The `convoy-review-watch` dog re-triggers fresh passes until clean.
9. **Operator clicks "Ship it"** in the dashboard (or `force convoy ship <id>`). NEVER auto-merges.
10. **Pilot cleans up** ask-branch after merge; Librarian records a convoy-level memory.

Per-repo opt-out: `pr_flow_enabled=false` falls back to the legacy direct-merge path (`MergeAndCleanup`).

## Components

- **Pilot** (`internal/agents/pilot.go`) — git steward. Tasks: `FindPRTemplate`, `CreateAskBranch`, `CleanupAskBranch`, `RebaseAskBranch`, `RevalidateRepoConfig`.
- **Diplomat** (`internal/agents/diplomat.go`) — draft-PR opener; claims `ConvoyReview` and `PRReviewTriage`.
- **Medic-CI** (`internal/agents/medic_ci.go`) — `CIFailureTriage` task type.
- **`internal/agents/pr_flow.go`** — sub-PR poll, stall detection, retrigger logic.
- **`AskBranchPRs` table** — per-(astromech-branch, ask-branch) sub-PR row.
- **Dogs**: `main-drift-watch`, `draft-pr-watch`, `sub-pr-ci-watch`, `pr-review-poll`, `convoy-review-watch`, `ship-it-nag`.

## Invariants

The full set is auto-rendered to [`../pr-flow-invariants.md`](../pr-flow-invariants.md). Highlights:

1. **Jedi Council is the code-review gate; Jenkins CI is the sanity gate.** Jedi runs first, then sub-PR opens, then CI runs, then auto-merge. Reordering breaks the self-healing contract.
2. **Ask-branch required.** Once a convoy has `ask_branch != ''`, all new tasks branch off the ask-branch. `PrepareAgentBranch` enforces.
3. **Drift-detection invariant.** Whenever an ask-branch is rebased, `Convoys.ask_branch_base_sha` MUST be updated in the same operation.
4. **Human-gate invariant.** The draft PR into main NEVER auto-merges.
5. **Legacy fallback always available.** `pr_flow_enabled=0` → direct-merge path.
6. **PR-review classifier invariants** (also auto-rendered):
   - Bots reply inline; humans never do (the LLM still runs but `replied_at` stays empty).
   - In-scope fixes route through Jedi Council on the ask-branch.
   - Thread loop cap at `pr_review_thread_depth_cap` (default 2) with `conflicted_loop` emit.
   - Resolve only after the fix lands (for `in_scope_fix`); resolve immediately for `not_actionable`; never resolve `out_of_scope` / `conflicted_loop`.

## Configuration

SystemConfig:

- `pr_flow_enabled` — global (default true).
- `pr_review_enabled` — global PR-review-triage kill switch.
- `pr_review_thread_depth_cap` (default 2).
- `convoy_review_max_findings` (default 2).
- `subPRCIStaleLimit` (2h), `subPRCIHardLimit` (6h), `subPRMaxStallRetriggers`.

Per-repo (`Repositories`):

- `pr_flow_enabled` — per-repo override.
- `pr_review_enabled` — per-repo override.
- `pr_template_path` — populated by `FindPRTemplate` (Pilot).
- `mode` — `read_only` / `write` / `quarantined`. New repos default `read_only` (operator promotes via `force repo set-mode <name> write`).
- `remote_url`, `default_branch` — populated by `add-repo` and refreshable via `force repo sync`.

Migration (additive, idempotent):

```bash
force migrate pr-flow --dry-run    # preview
force migrate pr-flow              # apply (auto-snapshots holocron.db)
force migrate pr-flow --rollback --confirm   # DESTRUCTIVE; daemon stopped
```

## Operator surface

```bash
force convoy pr <id>                      # per-repo ask-branches + draft PR + sub-PR rollup
force convoy ship <id> [--merge ...]      # promote draft from draft → ready (or merge directly)
force repo set-pr-flow <name> on|off      # toggle per-repo (future tasks only)
force repo set-mode <name> write          # promote new repo from read_only
```

Dashboard:
- **Convoys tab** — convoy progress with ask-branch / draft-PR state badges.
- **Repos tab** — per-repo PR-flow + mode badges.
- **Mail tab** — `[CONVOY REVIEW PASSED]`, `[SHIP-IT NAG]`, `[CI STALL]` style subjects.

When a draft PR sits unshipped:
- 24h / 72h / 1 week — `ship-it-nag` dog mails the operator.

## See also

- [`../pr-flow-invariants.md`](../pr-flow-invariants.md) — auto-rendered invariants (the binding contract).
- [`convoy-lifecycle.md`](convoy-lifecycle.md) — full convoy state machine.
- [`worktree-isolation.md`](worktree-isolation.md) — astromech branches off ask-branches.
- [`escalation-and-medic.md`](escalation-and-medic.md) — Medic-CI / `CIFailureTriage`.
- [`self-healing.md`](self-healing.md) — CI stall self-heal, retrigger caps.
- `dogs.md` (planned) — convoy-driving dog cadences.
