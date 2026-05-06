---
audience: both
scope: Per-agent worktrees — isolation model, base-drift discipline, branch naming, cleanup.
owner: architecture
last_reviewed: 2026-05-05
subsystem: worktree-isolation
type: subsystem-doc
---

# Worktree isolation

Astromechs work in isolated git worktrees, one per agent per repo. Two astromechs operating against the same repo never share working-directory state — they each have their own checkout, their own branch, their own `git status`. This is the **worktree isolation invariant**, and it's what lets the fleet parallelize coding work without merge-conflict storms or accidental cross-task contamination.

## Overview

For every (agent, repo) pair the daemon claims work for, Force creates a dedicated git worktree under a stable path. The worktree is registered in the `Agents` table (one row per agent per repo) so:

- The daemon can find the worktree on restart without re-deriving paths.
- The reconciler can verify the worktree still exists and is clean.
- `force cleanup` can remove stale worktrees from disk and DB in one pass.

Worktree naming follows a deterministic shape; branch naming includes the operator's GitHub username so enterprise branch-protection rules (which key on `<user>/…`) work without per-operator config.

## Components

- **`internal/agents/worktree.go`** — `EnsureWorktree`, `WorktreeReset`, branch naming.
- **`internal/agents/branch_naming.go`** — operator GitHub username lookup chain (`gh api user --jq .login` → `gh config get user -h github.com` → `git config user.name` → bare fallback).
- **`internal/agents/reconcile.go`** — startup reconciliation; checks worktree existence + dirtiness against every non-terminal `BountyBoard` row.
- **`Agents` table** — persistent worktree registry (one row per agent per repo).
- **`WorktreeReset` infra task** — Pilot-claimed task type that re-creates a corrupted worktree.
- **`cmd/force/cmd_cleanup.go`** — `force cleanup` removes stale entries.

## Invariants

1. **One worktree per (agent, repo).** Two astromechs cannot share a checkout. Branch creation is idempotent against an existing worktree.
2. **Branch names carry the operator username.** Astromech branch shape: `<username>/agent/<astromech>/task-<id>`. Ask-branches: `<username>/force/ask-<convoyID>-<slug>`. Falls back to a bare name when no username is configured.
3. **Worktree registration goes through `EnsureWorktree`.** Direct `git worktree add` calls are forbidden — the helper writes the `Agents` row, sets up the branch, and primes the worktree state.
4. **Startup reconciliation is mandatory.** Before any agent spawns, `agents.ReconcileOnStartup` walks every non-terminal `BountyBoard` row and verifies the worktree + branch still match. Five divergence cases each have explicit recovery actions:
   - **Clean.** Proceed.
   - **Branch missing pre-Captain.** Auto-recover as a re-pend (`UpdateBountyStatusFromTx` for CAS safety).
   - **Branch missing post-Captain.** Escalate (work was lost after a quality gate).
   - **Worktree missing or dirty.** Queue an idempotent `WorktreeReset` infra task for Pilot.
   - **Branch SHA diverged from recorded.** Escalate (someone moved the branch out from under the fleet).
5. **A failed reconcile is fatal.** The daemon refuses to start with an unreliable view of the fleet.
6. **Once a convoy has an ask-branch, all new tasks branch off the ask-branch, not main.** `PrepareAgentBranch` enforces this. Drift-detection invariant: when an ask-branch is rebased, `Convoys.ask_branch_base_sha` MUST be updated in the same operation.

## Configuration

- **Worktree root**: `~/.force/worktrees/` (configurable via SystemConfig knob if needed; default is fine for one-operator setups).
- **`Agents` table columns**: `agent_name`, `repo_id`, `worktree_path`, `branch_name`, `created_at`, `last_used_at`.
- **`force scale <N>`**: hot-add astromech agents via SIGUSR1; new agents get fresh worktrees on first claim. Scale-down only takes effect on restart and leaves stale worktrees until `force cleanup`.

## Operator surface

```bash
force agents               # list every registered agent worktree
force cleanup              # remove stale worktrees from disk + DB
force scale 4              # hot-scale astromechs (creates worktrees lazily)
sqlite3 holocron.db 'SELECT agent_name, repo_id, worktree_path FROM Agents'
```

Manual worktree maintenance:

```bash
cd ~/.force/worktrees/<agent>/<repo>
git status                 # verify clean
git fetch origin
git reset --hard origin/main   # if you intentionally want to nuke local state
```

If a worktree gets into a state the reconciler can't auto-fix, the operator clears it via `force cleanup` (which removes the disk + DB entries) and the next claim by that agent will re-create it. Pilot's `WorktreeReset` task type does the same thing programmatically when an astromech reports a dirty checkout.

## See also

- [`gas-town.md`](gas-town.md) — `Agents` table is part of the coordination substrate.
- [`pr-flow.md`](pr-flow.md) — ask-branches and the worktree-vs-ask-branch drift invariant.
- [`self-healing.md`](self-healing.md) — `WorktreeReset` as an auto-cleanup path.
- [`../CLAUDE.md`](../../CLAUDE.md) — Cross-agent service interfaces invariant (worktree state is per-agent, not shared).
