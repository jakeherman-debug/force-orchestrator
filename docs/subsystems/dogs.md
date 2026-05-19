---
audience: both
scope: The cron-driven dog cohort — full inventory, cooldowns, dispatch order, and operator overrides.
owner: infrastructure
last_reviewed: 2026-05-07
---

# Dogs — the watchdog cohort

Dogs are the periodic background jobs the Inquisitor dispatches every 5-minute heartbeat. Each dog has a cooldown that prevents stampede; the Inquisitor walks `dogOrder` once per tick, calling `RunDogs` which checks each dog's cooldown against the `Dogs` table and runs the ones that are due. Forty dogs are registered today (`TestListDogs` enforces the count).

## Overview

The dispatcher pattern is uniform: every dog has an entry in `dogCooldowns map[string]time.Duration` and an entry in `dogOrder []string`. `RunDogs` short-circuits if the operator has flipped e-stop (AUDIT-106 / Fix #1) — no dogs run during an emergency halt, not even observational ones. The `spend-burn-watch` dog ALWAYS runs first so a fleet-wide auto-e-stop fires before the rest of the cycle's dogs continue burning tokens; `task-spend-watch` runs second for the same reason scoped per-task.

Cooldowns range from `0` (every Inquisitor tick = every 5 min, used for cheap polling like `sub-pr-ci-watch` and `pr-review-resolve`) to `30 * 24 * time.Hour` (monthly, for `architecture-health-report`). Each tick is bounded by a 15-minute context (`AUDIT-047`); per-dog cancellable contexts derive from it. `DogMarkHeartbeat` writes `heartbeat_at` on each dog start so `/healthz` surfaces wedged dogs.

## Components

- `internal/agents/dogs.go` — the canonical inventory: `dogCooldowns`, `dogOrder`, `RunDogs`, `ListDogs`, `DogStatus`. Per-dog handlers live in sibling files (`dogs_repo_graph_scan.go`, `dogs_supply_token_recheck.go`, `transcript_archive.go`, `learning_panel_renderer.go`, …).
- `internal/agents/inquisitor.go` — `SpawnInquisitor`, `runInquisitorTick`. Dispatches `RunDogs(ctx, db, lib, ca, logger)` once per tick.
- `internal/agents/cooldown_scheduler.go` — `ScheduleCooldown`, `PauseCooldown`, `ResumeCooldown`, `CancelCooldown`, `MarkCooldownExecuted`, `ListPendingCooldowns`, `CooldownDuration`. The high-stakes auto-execute helper that Pattern P30 enforces is wired through.
- `schema/schema.sql` — `Dogs` table (`name PRIMARY KEY`, `last_run_at`, `run_count`, `heartbeat_at`).
- `internal/audittools/audit_pattern_p30_cooldown_test.go` — Pattern P30 regression on the `cooldown_scheduler.go` exports.

## Inventory

The 40 dogs grouped by purpose, with cooldowns. Order within each group reflects `dogOrder`.

**Spend defense (run first; skipped during e-stop):**

- `spend-burn-watch` (5 min) — fleet-wide trailing-hour spend; auto-flips e-stop past `hourly_spend_estop_usd`.
- `task-spend-watch` (5 min) — per-task trailing-10-min spend; soft-alerts the operator and hard-suspends the task on escalate.

**Lifecycle hygiene (DB / git / mail):**

- `git-hygiene` (30 min) — orphan-branch + stale-worktree cleanup.
- `db-vacuum` (6 h) — SQLite maintenance.
- `holonet-rotate` (24 h) — log-file rotation.
- `mail-cleanup` (12 h) — `Fleet_Mail` retention sweep.
- `memory-hygiene` (24 h) — `FleetMemory` retention.

**Stall + aging surfaces:**

- `stalled-reviews` (6 h) — surfaces UnderCaptainReview tasks past threshold.
- `priority-aging` (6 h) — bumps priority on aged tasks.
- `daily-digest` (24 h) — operator daily summary mail.
- `stale-convoys-report` (12 h) — surfaces aged convoys.

**Sub-PR + draft-PR + ask-branch lifecycle:**

- `sub-pr-ci-watch` (every tick) — polls open sub-PRs for CI state + external closure.
- `main-drift-watch` (15 min) — `git ls-remote` per ask-branch; rebases when main moves.
- `draft-pr-watch` (every tick) — polls `gh pr view` per `DraftPROpen` convoy for Merged/Closed transitions.
- `ship-it-nag` (6 h) — operator reminders for aged draft PRs (per-threshold dedup).
- `repo-config-check` (24 h) — revalidates remote URL, default branch, and PR template path per repo.
- `pr-review-poll` (5 min) — fetches bot/human review comments; queues `PRReviewTriage` tasks.
- `pr-review-resolve` (every tick) — sweeps `in_scope_fix` comments whose spawned `CodeEdit` has Completed; calls GraphQL `resolveReviewThread`.
- `convoy-review-watch` (5 min) — `ConvoyReview` cycle dispatcher.

**Escalation + repo-mode:**

- `escalation-sweeper` (10 min) — auto-resolves Open escalations whose underlying task or sub-PR is now terminal.
- `quarantined-repo-watch` (24 h) — surfaces operator mail when claim loops have skipped tasks against quarantined repos (per-repo dedup).

**Disagreement + reflection:**

- `disagreement-tracker` (1 h) — cross-layer disagreement rates (Captain→Council, Council→CI, ConvoyReview→astromech, operator-revert) over rolling 7d/30d/90d windows.
- `learning-panel-render` (7 d) — weekly `FleetLearningPanels` row.
- `transcript-archive` (24 h) — bounded offload of stale `LLMCallTranscripts` to `~/.force/transcripts/`.

**Model + experiment substrate:**

- `model-availability-watch` (30 min) — probes distinct `model_identifier` values from `TreatmentSpecs`; upserts `ModelAvailability`.
- `proposed-features-decay` (12 h) — decays stale `value_score` from high → medium → low.

**Librarian evolution (D4 P0):**

- `librarian-dedup-watch` (12 h) — folds near-identical `FleetMemory` rows; runs first so downstream passes see the post-merge view.
- `librarian-quality-recompute` (24 h) — decays `freshness_score`.
- `librarian-conflict-watch` (24 h) — surfaces contradictory memories as operator tickets.
- `librarian-hypothesis-emit` (24 h) — emits candidate `PromotionProposals` from high-quality memories.
- `claude-md-drift-watch` (7 d) — scans CLAUDE.md invariants vs `FleetRules` and emits drift candidates.

**Senate (D4 P3):**

- `senate-refresh` (7 d) — calls `librarian.RefreshSenatorMemoryDigest`, appends `SenateMemory` entries, bumps `SenateChambers.last_refreshed_at`.

**Supply chain (D5 P4):**

- `supply-allowlist-refresh` (24 h) — populates `SystemConfig.supply_allowlist_<eco>` from `aws codeartifact list-packages` for SUPPLY-002 typosquat detection.
- `supply-token-recheck` (30 min) — probes CodeArtifact health; replays SUPPLY-* deferrals on recovery via `supplydeferral.ReplayPendingDeferrals`.

**Staged convoys (D5.5 P1):**

- `convoy-stage-watch` (5 min) — advances `ConvoyStages` state machine; evaluates gates via `stagegate.Registry`.

**Cross-repo + architecture (D8–D10):**

- `repo-graph-scan` (24 h) — walks registered repos; extracts exported symbols + import call sites into `CrossRepoSymbols` / `CrossRepoDependencies` for blast-radius analysis.
- `repo-api-scan` (24 h) — walks registered repos; dispatches files to the `ExtractorRegistry` (rails/proto/openapi/spring/ktor/express/nestjs providers + jsclient/rubyclient/javaclient/grpcclient consumers); upserts `CrossRepoAPIs` and `CrossRepoAPIDependencies` for API-surface blast-radius (D15).
- `architecture-health-report` (30 d) — monthly longitudinal scan; runs every BoS rule over the full codebase and renders `reports/architecture-health-YYYY-MM.md`.
- `archaeologist-sweep` (7 d) — fans out per-repo `ArchaeologistSweep` tasks for the proactive debt-pattern agent.
- `architecture-doc-render` (1 h) — re-renders `ARCHITECTURE.md` for every repo with `handoff_synthesis_enabled=1`.

**Notification cleanup (D11 P2):**

- `notification-override-cleanup` (24 h) — purges `ConvoyNotificationOverrides` rows >7 d after convoy terminal transition.

## Invariants

1. **Inventory count is asserted.** `TestListDogs` (`internal/agents/dogs_test.go`) requires exactly 41 dogs and names the load-bearing subset; adding a dog requires updating the test in the same commit.
2. **`spend-burn-watch` runs first; `task-spend-watch` runs second.** Both cost defenses must land before any subsequent dog or any subsequent claim cycle continues spending tokens.
3. **E-stop short-circuits all dogs.** AUDIT-106 / Fix #1: no dogs run during emergency halt. The whole point of e-stop is to stop activity that costs money.
4. **Per-tick context is cancellable from the daemon.** `SpawnInquisitor` derives `tickCtx` from the daemon `ctx`; per-dog ctx derives from `tickCtx`. Daemon SIGINT/SIGTERM cancels in-flight dog work.
5. **Cooldown comparisons go through `store.NowSQLite` / `store.ParseSQLiteTime`.** Fix #8c (AUDIT-146) forbids comparing `datetime('now')` against `time.Now()` directly; mismatched locations would silently mis-fire.
6. **High-stakes auto-execute uses the cooldown scheduler.** Pattern P30 (`docs/patterns/p30-cooldown.md`, `internal/audittools/audit_pattern_p30_cooldown_test.go`) requires Council auto-merge / Medic auto-fix / etc. to schedule through `agents.ScheduleCooldown` so the operator can pause / resume / cancel before the action lands.
7. **Bounded per-tick budgets.** Each tick is bounded by a 15-min context; per-dog budgets derive from it. `transcript-archive` caps `maxArchivesPerRun = 1000` per tick so one run cannot dominate a transaction window.

## Configuration

`schema/schema.sql` — `Dogs` table:

- `name TEXT PRIMARY KEY` — dog name (matches `dogOrder` entries).
- `last_run_at TEXT DEFAULT ''` — SQLite-UTC timestamp of last run.
- `run_count INTEGER DEFAULT 0` — cumulative run count.
- `heartbeat_at TEXT DEFAULT ''` — `DogMarkHeartbeat` writes this on dog start; `/healthz` surfaces wedged dogs.

SystemConfig knobs that affect dog behaviour:

- `hourly_spend_cap_usd` (default 25.0) — claim loops skip-and-sleep past this trailing-hour spend.
- `hourly_spend_estop_usd` (default 200.0) — `spend-burn-watch` auto-flips e-stop past this.
- `spend_cap_last_alert_hour` (auto) — dedup key for operator warning mail.
- `bash_guard_curl_hosts` — cross-cutting; consumed by `cmd/force-bash-guard`, not a dog.
- `supply_allowlist_<eco>` — populated by `supply-allowlist-refresh`; read by SUPPLY-002.
- `inbound_redact_alert_threshold` — cross-cutting; not a dog knob.

Constants (in `internal/agents/dogs.go` / `inquisitor.go`):

- `inquisitorInterval = 5 * time.Minute` — heartbeat cadence.
- `staleLockTimeout = 45 * time.Minute` — Inquisitor's stale-lock reset threshold.
- `stallEscTimeout = 30 * time.Minute` — Boot triage trigger.
- `bootCallCooldown = 30 * time.Minute` — per-task Boot triage rate-limit.

## Operator surface

```bash
force dogs                    # list all dogs with cooldown / last_run / next_run / run_count
force dogs status             # alias
force estop on                # halt all dogs (and claim loops) immediately
force estop off               # resume
```

`ListDogs` populates each row's `NextRun` field from `last_run_at + cooldown - now`; "overdue" indicates the cooldown has elapsed and the next Inquisitor tick will run the dog.

Dashboard:

- **Dogs tab** — full roster with cooldown / last-run / next-run; per-dog click-through to recent runs.
- **Health page** (`/healthz`) — surfaces wedged dogs (heartbeat older than per-dog timeout).
- **Estop banner** — visible when e-stop is on; lists the spend-burn-watch hour that tripped it (if any).

Mail subjects:

- `[SPEND-BURN E-STOP]` — `spend-burn-watch` auto-flipped e-stop.
- `[TASK SPEND ESCALATE]` — `task-spend-watch` per-task escalation.
- `[QUARANTINED REPO BACKLOG]` — `quarantined-repo-watch` per-repo daily.
- `[FLEET LEARNING PANEL]` — `learning-panel-render` weekly.

To temporarily disable a dog, the operator can manually update its `Dogs.last_run_at` to a future timestamp — the next-run check will skip it until the timestamp passes. There is no per-dog SystemConfig disable flag today; that is a future addition.

## See also

- [`../agents/inquisitor.md`](../agents/inquisitor.md) — the heartbeat agent that dispatches dogs.
- [`../patterns/p30-cooldown.md`](../patterns/p30-cooldown.md) — Pattern P30 (high-stakes auto-execute cooldown).
- [`escalation-and-medic.md`](escalation-and-medic.md) — `escalation-sweeper` and the no-silent-failures rule.
- [`fleet-memory.md`](fleet-memory.md) — the Librarian-evolution dog cohort.
- [`supply-chain.md`](supply-chain.md) — `supply-allowlist-refresh` + `supply-token-recheck`.
- [`pr-flow.md`](pr-flow.md) — the sub-PR / draft-PR / ask-branch dog cohort.
