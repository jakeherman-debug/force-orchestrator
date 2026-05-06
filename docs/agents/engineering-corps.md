---
audience: both
scope: Engineering Corps — paired-runs experimentation orchestrator that authors experiments, monitors them, drafts promotion / demotion proposals, authors metric SQL, and watches holdouts.
owner: D13
last_reviewed: 2026-05-05
---

# Engineering Corps — Paired-Runs Experimentation Orchestrator

## Role

The Engineering Corps is the experimentation orchestrator. Its task handlers (`ExperimentAuthor`, `ExperimentMonitor`, `PromotionAuthor`, `DemotionAuthor`, `MetricAuthor`, `HoldoutMonitor`) generate experiment YAML, metric SQL, and promotion-proposal evidence summaries. All inputs are inlined into the prompt and the output is text destined for direct DB writes — **no MCP tool surface needed** (the EC profile ships with `builtin_tools: []` and `mcp_servers: []`, mirroring Diplomat).

Engineering Corps does not edit files in a worktree — its deliverables are DB rows + ratifiable proposals. The operator reviews and applies anything destined for `claude-md-file` / similar render targets via the rule-renderer, so EC itself does not need Edit / Write / Bash. The dispatcher fails closed on missing dependencies (`SpawnEngineeringCorps` with a zero-value config short-circuits cleanly).

## Responsibilities

The dispatcher claims six task types (one handler per type — `TestAllTaskTypesIsSix` is the regression test that the count does not drift):

| Task type | What it does |
|---|---|
| `ExperimentAuthor` | Generates an experiment YAML manifest from an emitted candidate. Rejects manifests missing the primary metric or carrying injection-token contamination. |
| `ExperimentMonitor` | Watches a running experiment; declares a winner when min-runs and significance thresholds clear. Honors `LIVE_HAIKU_DISABLED` for tests, and the global emergency-stop. |
| `PromotionAuthor` | Drafts a `PromotionProposals` row from a winning experiment. Operator-routed: the proposal materializes the rule, EC does not insert into `FleetRules` directly. |
| `DemotionAuthor` | Drafts a demotion / rollback proposal for a stale ratified promotion. Skips fresh proposals; idempotent. |
| `MetricAuthor` | Generates the metric SQL for a candidate metric. Output is a row in the metrics table; operator approves before live use. |
| `HoldoutMonitor` | Watches the holdout cohort to ensure the experiment populations stay disjoint. |

## Capability profile

Profile: [`agents/capabilities/engineering-corps.yaml`](../../agents/capabilities/engineering-corps.yaml). The profile is `builtin_tools: []` / `mcp_servers: []` — every input is inlined into the prompt and the output is text bound for direct DB writes. Loaded via `capabilities.LoadProfile("engineering-corps")` in the experimentation handlers' Claude call sites.

## Key files

- `internal/agents/engineering_corps/engineering_corps.go` — `SpawnEngineeringCorps(ctx, cfg)` and the dispatcher (`claimAndDispatch`, `dispatch`).
- `internal/agents/engineering_corps/types.go` — `EngineeringCorpsConfig` and the task-type registry.
- `internal/agents/engineering_corps/experiment_author.go` — `ExperimentAuthor` handler.
- `internal/agents/engineering_corps/experiment_monitor.go` — `ExperimentMonitor` handler.
- `internal/agents/engineering_corps/promotion_author.go` — `PromotionAuthor` handler.
- `internal/agents/engineering_corps/demotion_author.go` — `DemotionAuthor` handler.
- `internal/agents/engineering_corps/metric_author.go` — `MetricAuthor` handler.
- `internal/agents/engineering_corps/holdout_monitor.go` — `HoldoutMonitor` handler.
- `agents/capabilities/engineering-corps.yaml` — capability profile (empty tool surface).

## Tests

- `internal/agents/engineering_corps/dispatcher_test.go` — `TestEngineeringCorpsDispatcher_UnknownTypeFailsCleanly`, `TestEngineeringCorpsConfig_FailsClosedOnMissingDeps`, `TestAllTaskTypesIsSix` (regression on the six-type count).
- `internal/agents/engineering_corps/experiment_author_test.go` — happy path + LLM parse error + injection-token rejection + missing-primary-metric rejection + prior-outcomes logging.
- `internal/agents/engineering_corps/experiment_monitor_test.go` — winner declaration, below-min-runs, emergency-stop, heartbeat scans-all.
- `internal/agents/engineering_corps/promotion_author_test.go`, `demotion_author_test.go`, `metric_author_test.go`, `holdout_monitor_test.go` — per-handler coverage.
- `internal/agents/engineering_corps/integration_test.go` — `TestDispatcher_AllSixTypes_HandlersWired` end-to-end.
- `internal/audittools/audit_pattern_p13_capability_profiles_test.go` — capability profile invariant.
- `internal/audittools/audit_pattern_p23_proposer_write_discipline_test.go` — EC writes `PromotionProposals`, never `FleetRules` directly.
- `internal/audittools/audit_pattern_p31_llm_transcripts_test.go` — every EC handler that calls Claude writes a transcript.

## See also

- [`docs/agents/librarian.md`](librarian.md) — emits the candidates EC turns into experiments.
- [`docs/agents/senate.md`](senate.md) — Senate's no-self-promote pipeline routes through EC paired-runs before any FleetRules row materializes.
- [`docs/architecture/claude-cli-invocation.md`](../architecture/claude-cli-invocation.md) — invocation layering (EC runs with `force-orchestrator/` CWD).
