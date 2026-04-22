# CLAUDE.md — directives for agents working on this codebase

This file captures invariants that are easy to violate without noticing. Read it before making changes.

## Core architecture

- **Gas Town pattern.** All coordination happens through the SQLite `holocron.db`. Never use Go channels or in-memory maps for cross-agent state. If two agents need to talk, one writes a row, the other reads it.
- **No silent failures.** Every error path must terminate in `store.FailBounty(...)`, `store.UpdateBountyStatus(...)`, or an explicit escalation. Never `log.Printf` an error and continue as if nothing happened.
- **CLI shelling for LLM calls.** Agents invoke Claude via `claude -p` (through `internal/claude`), not the Anthropic HTTP API. This preserves the MCP toolchain available to Claude Code.
- **Worktree isolation.** Astromechs work in persistent per-agent git worktrees (`.force-worktrees/<repo>/<agent>`). They branch off HEAD of the repo (or the convoy's ask-branch under the PR flow). Never hardcode `main` or `master` — use `GetDefaultBranch(repoPath)`.

## PR flow invariants

The fleet delivers via GitHub PRs by default (`pr_flow_enabled = true`). Code touching the approval, merge, or branch-creation paths must respect the following:

1. **Jedi Council is the code-review gate, Jenkins CI is the sanity gate.** Jedi runs first (agent LLM review), then the sub-PR opens, then CI runs, then auto-merge. Reordering breaks the self-healing contract.
   - *Special case*: when Jedi approves a task whose `branch_name == ConvoyAskBranch.ask_branch` (rebase-conflict resolution), the sub-PR path is skipped: `completeAskBranchResolution` force-pushes the ask-branch and updates the stored base SHA. Opening a PR with head==base would be nonsense.
2. **Ask-branch required invariant.** Once a convoy has `ask_branch != ''`, all new tasks in that convoy MUST branch off the ask-branch, not main. `PrepareAgentBranch` is the enforcement point.
3. **Drift-detection invariant.** Whenever an ask-branch is rebased, `Convoys.ask_branch_base_sha` must be updated in the same operation. A stale base_sha means `main-drift-watch` either misfires or never fires.
4. **Human-gate invariant.** The draft PR into main NEVER auto-merges. The ship-it button (`gh pr ready` + optional `gh pr merge`) is the one and only path.
5. **Legacy fallback is always available.** `pr_flow_enabled=0` on a repo sends it through the pre-PR-flow direct-merge path (`MergeAndCleanup` in `internal/git/git.go`). This is the escape hatch for repos with broken remotes or branch protection rules we can't satisfy.

## Self-healing is the default; escalation is the last step

Every new `fmt.Errorf(...)` or `FailBounty(...)` added during a PR-flow change must fall into one of these buckets:

- **Auto-retry:** the error is `ErrClassTransient` or `ErrClassRateLimited` (see `internal/gh/gh.go`). Pilot's retry wrapper handles these automatically.
- **Auto-fix:** Medic `CIFailureTriage` spawns a CodeEdit task on the astromech branch. Fix loops cap at 3 attempts per PR.
- **Auto-bypass:** repo marked `pr_flow_enabled=0` or `quarantined_at` stamped, so future tasks take the legacy path.
- **Operator escalation:** `CreateEscalation(...)` + operator mail. Reserved for cases where self-healing is genuinely not possible (auth expired, branch protection, unfixable bug).

If a new error path does not fit any of the above, stop and design the self-healing path before writing the code.

## Testing rules

- **Always run `make test` (with `-tags sqlite_fts5`) before considering a phase done.** Tests run in ~2-3 minutes.
- **Tests exercise real flows, not just happy paths.** When you add a code path, add tests for: (a) the happy path, (b) each distinct failure mode, (c) idempotence (run twice, same result).
- **Never mock the database.** `store.InitHolocronDSN(":memory:")` gives you a real SQLite — use it.
- **Mock `gh` and `git` only at the package boundary.** `gh` ops use `gh.NewClientWithRunner(stubRunner)`; git ops use real `git init`/`git commit` on a temp dir (see `makeGitRepo` in `pilot_preflight_test.go`).
- **Docs and tests are part of each phase's exit criteria.** A phase is not done until `go test ./...` is green AND the relevant README / schema.sql / CLAUDE.md is updated.

## Store / schema conventions

- `createSchema` creates tables with IF NOT EXISTS — used for fresh DBs.
- `runMigrations` runs the ALTERs for existing DBs — always additive, never destructive. Both run automatically from `InitHolocronDSN`.
- When adding a column, add it to BOTH `createSchema` (for fresh installs) and `runMigrations` (for upgrades).
- `IFNULL(col, '')` in SELECTs when reading columns that might be NULL on rows written before the column existed.
- SQLite migrations are idempotent — re-running the same migration twice must be a no-op. Use `IF NOT EXISTS` for tables, and rely on ALTER TABLE ADD COLUMN's silent failure on duplicates.

## Commit style

- Conventional commits (`feat:`, `fix:`, `docs:`, etc.). Body explains WHY, not WHAT.
- No `--no-verify`. Pre-commit hooks run for a reason.
- When a pre-commit hook fails, fix the root cause and re-stage; do not `--amend` (the commit didn't happen).
