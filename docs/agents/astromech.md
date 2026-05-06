---
audience: both
scope: Astromechs — the worker agents that claim CodeEdit tasks, run claude -p inside per-agent worktrees, and commit the result for downstream review.
owner: D13
last_reviewed: 2026-05-05
---

# Astromechs — Workers

## Role

Astromechs are the fleet's coding workers. Multiple astromechs run in parallel, each spawning its own claim loop and competing for `CodeEdit` bounties. When one wins a claim it stages a persistent per-agent worktree on the right branch, assembles a rich prompt that fuses task payload + Fleet Memory + Seance + inbox + directives, runs `claude -p` inside the worktree, and commits the result. Astromechs are the only agent class that runs with **target-repo CWD** — every other agent runs from `force-orchestrator/`. Per FleetRules row `astromech-target-claude-md-advisory`, the target's `CLAUDE.md` is treated as advisory (injected via `AppendFleetRulesToPrompt`), not authoritative.

## Responsibilities

- Claim `CodeEdit` bounties.
- Reuse-or-create a persistent worktree at `<repo-root>/.force-worktrees/<astromech-name>` and check out / create `<username>/agent/<name>/task-<id>`.
- Assemble the claude prompt with: payload + goal context, FTS5-ranked Fleet Memory hits, Seance (full prior-attempt output for retries), unread inbox mail, and standing directives loaded from `./directives/astromech.md`.
- Shell `claude -p` with capability args sourced from the loaded profile.
- Commit the result and transition the task to `AwaitingCouncilReview` (or to `BoSReview` + `ISBReview` first under the dual-gate path).
- Honor agent-emitted signals: `[ESCALATED:LOW|MEDIUM|HIGH:reason]`, `[CHECKPOINT: step]`, `[SHARD_NEEDED]`, `[DONE]`.
- Track infra-failure backoff; after `maxInfraFailures` consecutive failures, permanently fail the task, spawn a remediation task, and mail the operator (no silent failures).

## Capability profile

Profile: [`agents/capabilities/astromech.yaml`](../../agents/capabilities/astromech.yaml). Loaded via `capabilities.LoadProfile("astromech")` in `internal/agents/astromech.go`. Astromechs additionally load the librarian profile for write-memory bounties they may emit. Pattern P13 enforces the no-hardcoded-tool-literal rule.

## Key files

- `internal/agents/astromech.go` — `SpawnAstromech(ctx, db, name)` + `SpawnNew(ctx, db, name)`; main claim/worktree/prompt/commit loop.
- `internal/agents/astromech_test.go` — happy-path coverage.
- `internal/agents/astromech_estop_cancel_test.go` — E-stop cancellation.
- `internal/agents/astromech_target_claudemd_test.go` — verifies target-repo `CLAUDE.md` is treated as advisory.
- `internal/agents/branch_prefix_test.go` — branch-name discipline (`<username>/agent/<name>/task-<id>`).
- `agents/capabilities/astromech.yaml` — capability profile.

## Tests

- `internal/agents/astromech_test.go`, `internal/agents/astromech_estop_cancel_test.go`, `internal/agents/astromech_target_claudemd_test.go` — direct unit + integration coverage.
- `internal/audittools/audit_pattern_p13_capability_profiles_test.go` — Pattern P13 capability-profile enforcement.
- `internal/audittools/audit_pattern_p15_bash_guard_test.go` — bash-guard wrapper coverage.
- `internal/audittools/audit_pattern_p17_claude_md_size_test.go` — target-CLAUDE.md size cap.
- `internal/audittools/audit_pattern_p32_git_ops_test.go` — git ops discipline (Astromechs do most fleet-wide git work).
- `internal/audittools/audit_silent_failures_test.go` — confirms astromech infra-failure paths terminate in `FailBounty` / escalation, never silent log+continue.

## See also

- [`docs/agents/captain.md`](captain.md) — receives the astromech's commit and runs the plan-coherence gate.
- [`docs/agents/council.md`](council.md) — code-review gate post-Captain.
- [`docs/agents/medic.md`](medic.md) — handles permanently-failed astromech tasks.
- [`docs/architecture/claude-cli-invocation.md`](../architecture/claude-cli-invocation.md) — the layering reference; Astromechs are the only agent class with target-repo CWD.
