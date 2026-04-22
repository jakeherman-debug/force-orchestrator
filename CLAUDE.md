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

## PR review-comment invariants

After Diplomat opens the draft PR to main, the `pr-review-poll` dog records
bot and human review comments into `PRReviewComments` and Diplomat's
`PRReviewTriage` classifier dispatches them.

1. **Bots reply inline; humans never do.** For `author_kind='bot'`, the
   triage dispatcher posts a reply to GitHub and resolves the thread (after
   the fix lands). For `author_kind='human'`, the LLM still runs and the
   reply is drafted into `reply_body`, but `replied_at` stays empty and
   no gh call fires. The operator posts, edits, or dismisses from the
   dashboard. The dispatcher must hard-normalize `AuthorKind=="human"` →
   `classification="human"` regardless of what the LLM returned.
2. **In-scope fixes route through the Jedi Council.** The dispatcher
   spawns a CodeEdit on the ask-branch (`branch_name=<ask_branch>`), and
   Council's `completeAskBranchResolution` path force-pushes when it
   approves. We never bypass the quality gate for bot suggestions.
3. **Thread loop cap.** When `thread_depth >= pr_review_thread_depth_cap`
   (default 2) AND the classifier detects contradiction, it emits
   `conflicted_loop`, escalates, and stops acting on that thread. The
   classifier must NOT emit `conflicted_loop` at lower depths.
4. **Thread resolution only after the fix lands.** For `in_scope_fix`,
   the review thread is resolved by the `pr-review-resolve` sweep once
   the spawned CodeEdit reaches status=Completed — not when the reply
   was posted. For `not_actionable`, resolve immediately. For
   `out_of_scope` and `conflicted_loop`, never resolve (keep threads
   visible for human follow-up).
5. **Global + per-repo kill switches.** `pr_review_enabled=0` in
   SystemConfig or `Repositories.pr_review_enabled=0` skips the repo
   entirely. Both switches check in `dogPRReviewPoll` and
   `dogPRReviewResolve` before any gh calls.

## ConvoyReview invariants

`ConvoyReview` is the convoy-level completeness gate. It runs one LLM pass over the full
ask-branch diff vs main, finds gaps/regressions/incorrectness, and spawns CodeEdit fix tasks.
A `convoy-review-watch` dog re-triggers it once those fix tasks complete, creating a
self-healing loop that terminates when a pass returns `"clean"`.

1. **Triggered on DraftPROpen (two paths).** Diplomat calls `QueueConvoyReview` immediately
   after `SetConvoyStatus(db, convoyID, "DraftPROpen")`. The `convoy-review-watch` dog (5 min
   cadence) acts as a safety net: it queues a ConvoyReview for any `DraftPROpen` convoy that
   has no pending review and no active fix tasks.
2. **Idempotent queue.** `QueueConvoyReview` returns `0, nil` (no-op) if a ConvoyReview is
   already `Pending` or `Locked` for that convoy. Always call it freely; it will not double-queue.
3. **Loop cap at 5 passes.** If a convoy has already completed ≥ 5 ConvoyReview passes,
   `runConvoyReview` escalates (SeverityHigh) and fails the task instead of spawning more fix
   tasks. The loop cap check runs BEFORE the LLM call.
4. **Fix tasks are pinned to the ask-branch.** Each CodeEdit spawned by a ConvoyReview has its
   `branch_name` set to the convoy's ask-branch via `store.SetBranchName`. This ensures the
   Jedi Council's `completeAskBranchResolution` path applies (force-push to ask-branch, no
   redundant sub-PR).
5. **Max findings cap.** Each pass spawns at most `convoy_review_max_findings` fix tasks
   (SystemConfig, default 5). Remaining findings are picked up in the next pass.
6. **ConvoyReview is an infrastructure task.** It is registered in `InfrastructureTaskTypes`
   and is hidden from the dashboard. It never spawns another ConvoyReview (only CodeEdit fix
   tasks). The dog handles re-triggering.
7. **On LLM parse failure.** One retry with a critic note appended. Second failure → mark
   Completed (not Failed) so the dog retries on the next 5-min tick rather than leaving a
   stuck Locked task.
8. **Dog re-trigger condition.** `dogConvoyReviewWatch` queues a new ConvoyReview only when
   ALL of the following hold: convoy status is `DraftPROpen`, no ConvoyReview is
   `Pending`/`Locked`, and no child CodeEdit task (whose parent is a ConvoyReview for this
   convoy) is in a non-terminal status.

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
