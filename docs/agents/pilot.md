---
audience: both
scope: Pilot — the PR-flow git-ops steward that handles deterministic branch / rebase / cleanup work without involving the LLM on the happy path.
owner: D13
last_reviewed: 2026-05-05
---

# Pilot — PR-Flow Steward

## Role

The Pilot is the git-ops steward for the PR-based delivery flow. It handles infra tasks that do not require code synthesis — cutting per-ask integration branches, rebasing them against main, auto-merging sub-PRs, and cleaning up branches after a convoy ships. Pilot's happy path is pure shell-out (git + gh) with **no LLM call** — fast and auditable. When a rebase conflicts, Pilot never tries to resolve it itself; it spawns a `RebaseConflict` `CodeEdit` and lets an astromech do the code work. The single LLM call Pilot does make is for `FindPRTemplate` (only when the deterministic filesystem search comes up empty for a repo with an unusual template location).

Roster: Poe-Dameron, Wedge-Antilles, Hera-Pilot.

## Responsibilities

| Task type | What it does |
|---|---|
| `FindPRTemplate` | Locate the PR template for a repo. Deterministic filesystem search first (`.github/`, root, `docs/`, common variants); falls back to a single Claude call only for unusual layouts. Writes the result to `Repositories.pr_template_path`. |
| `CreateAskBranch` | Cut and push the convoy's integration branch (`force/ask-<id>-<slug>` or `<username>/force/ask-<id>-<slug>`) off main. For multi-repo convoys, fans out per-repo branch creation — each touched repo gets its own row in `ConvoyAskBranches`. Idempotent. |
| `CleanupAskBranch` | Delete branches after the convoy ships or is abandoned. |
| `RebaseAskBranch` | Rebase an ask-branch onto main and force-push; spawn a `RebaseConflict` `CodeEdit` when the rebase doesn't apply cleanly. |
| `RevalidateRepoConfig` | Revalidate remote URL, default branch, and template path to catch repo renames or moved templates. |

The branch name's leading `<username>/` prefix is discovered via a fallback chain: `gh api user --jq .login` → `gh config get user -h github.com` → `git config user.name`. Astromech work branches follow the same convention: `<username>/agent/<name>/task-<id>`. Local setups with no username configured fall back to the bare name.

## Capability profile

Profile: [`agents/capabilities/pilot.yaml`](../../agents/capabilities/pilot.yaml). Loaded via `capabilities.LoadProfile("pilot")` in `internal/agents/pilot.go`. Tools focus on Bash (git + gh) plus Read; no Edit/Write outside the ask-branch surface.

## Key files

- `internal/agents/pilot.go` — `SpawnPilot(ctx, db, name)` claim loop.
- `agents/capabilities/pilot.yaml` — capability profile.

## Tests

- `internal/audittools/audit_pattern_p13_capability_profiles_test.go` — capability profile invariant.
- `internal/audittools/audit_pattern_p32_git_ops_test.go` — git-ops discipline; Pilot is the largest surface this pattern covers.
- `internal/audittools/audit_pattern_p25_cli_parity_test.go` — Pilot-relevant CLI parity (force CLI subcommands that mirror Pilot tasks must stay parity with the daemon path).
- `internal/audittools/audit_silent_failures_test.go` — Pilot rebase / branch failures terminate cleanly.

## See also

- [`docs/agents/diplomat.md`](diplomat.md) — runs the final ship step (draft PR open) after Pilot finishes branch / rebase work.
- [`docs/agents/medic.md`](medic.md) — handles the `RebaseConflict` `CodeEdit` task Pilot spawns when a rebase doesn't apply cleanly.
- [`docs/agents/inquisitor.md`](inquisitor.md) — dispatches the `main-drift-watch` dog that triggers Pilot rebases every 15 minutes.
