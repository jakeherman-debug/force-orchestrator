---
audience: operator
scope: D5.5 staged convoys — Commander-drafted phase pipelines with per-stage gates, dispatch fencing, and operator-facing advance/abort/bypass controls.
owner: feature-team
last_reviewed: 2026-05-07
---

# Convoy staging

Staged convoys are the general-purpose primitive for any work that shouldn't land in a single PR — ZDM column-dance, feature-flag rollouts, reviewer cognitive-load splits, capacity-aware enablement, soak-between-changes risk reduction, parallel-work serialization. Single-PR convoys remain the default; multi-stage is opt-in at planning time. The substrate ships in D5.5; this page is the operator reference.

## Overview

A convoy with `staging_mode='staged'` carries an ordered list of `ConvoyStages` rows. Each stage runs independently, opens its own ask-branches and sub-PRs, and is fenced by a per-stage **gate** that blocks advancement until evidence (time-elapsed, operator click, deployed-endpoint healthy, release-label present, Datadog metric, Databricks query result) confirms the previous stage is safe to build on.

Two control loops drive the lifecycle:

- The **`convoy-stage-watch`** dog (5 min cadence — `internal/agents/dogs_convoy_stage_watch.go`) walks active stages and advances state by querying the gate registry.
- The **operator dashboard** surfaces per-stage detail and exposes `advance` / `abort` / emergency-bypass actions backed by `internal/dashboard/handlers_staged_convoys.go`.

Single-stage convoys (the legacy shape) get a forward-compat migration: every existing convoy gets a single `stage_num=1` row with `gate_type=NULL`. The watcher dog and the per-stage scoping logic both treat null-gate single-stage convoys as no-ops, so behavior is bit-for-bit identical pre/post-migration.

## Components

### Stage registry — `internal/stagegate/`

Plug interface (`Gate.Type()` + `Gate.Evaluate(ctx, db, stage StageContext) (passed bool, reason string, err error)`) plus a `Registry` that dispatches uniformly across leaves and compounds. Two compound types nest under `MaxNestingDepth = 5`. Nine gate types ship in D5.5:

| Gate type | File | Config keys | Purpose |
|---|---|---|---|
| `soak_minutes` | `internal/stagegate/soak_minutes.go` | `minutes` | Wait N minutes after `all_prs_merged_at`. |
| `operator_confirm` | `internal/stagegate/operator_confirm.go` | `prompt` | Block on dashboard click; rendezvous via `SystemConfig.stage_advance_<convoy>_<stage>`. |
| `null` | `internal/stagegate/null_gate.go` | none | No-op (terminal stage only; planner-enforced). |
| `all_of` | `internal/stagegate/compound.go` | `gates: [...]` | Boolean AND; short-circuits on first concrete fail. |
| `any_of` | `internal/stagegate/compound.go` | `gates: [...]` | Boolean OR; passes on first concrete pass. |
| `probe_endpoint` | `internal/stagegate/probe_endpoint.go` | `url`, `method`, `expected_status`, `body_match_regex`, `timeout_seconds`, `headers` | HTTP probe of deployed change. |
| `release_label_present` | `internal/stagegate/release_label_present.go` | `polling_interval_minutes` | Every merged PR carries a label matching `Repositories.release_label_pattern`. |
| `datadog_metric_threshold` | `internal/stagegate/datadog_metric_threshold.go` | `metric_query`, `comparator`, `threshold`, `sample_window_minutes` | Datadog metric vs threshold, via `internal/clients/datadog`. |
| `databricks_query_threshold` | `internal/stagegate/databricks_query_threshold.go` | `sql_query`, `comparator`, `threshold`, `warehouse_id`, `timeout_seconds` | SQL query result vs threshold, via `internal/clients/databricks`. |

The evaluator returns one of three outcomes — `passed=true` flips the stage to `GatePassed`; `passed=false, err=nil` flips to `Failed`; `passed=false, err=ErrPending` keeps the stage in `AwaitingGate` for the next dog tick.

### Schema — `schema/schema.sql`

- `ConvoyStages` (`id, convoy_id, stage_num, intent_text, status, gate_type, gate_config_json, gate_timeout_minutes, opened_at, all_prs_merged_at, gate_passed_at, completed_at`). Status linear ordering: `Pending → Open → AllPRsMerged → AwaitingGate → GatePassed → Verified` plus terminal `Failed`. UNIQUE(convoy_id, stage_num).
- `Convoys.staging_mode` (`'single'` default, `'staged'` opt-in) and `Convoys.staging_strategy` (`'strict'` is the only D5.5 implementation; `merge_parallel` and `stacked` are validator-rejected with explicit "future deliverable" errors).
- `BountyBoard.stage_id` — non-NULL for multi-stage convoy tasks; partial index `idx_bounty_stage_id` covers the populated rows.
- `ConvoyAskBranches.stage_id` — FK to `ConvoyStages.id`; `idx_convoy_ask_branches_stage_id` backs the per-stage scope filter.
- `Repositories.release_label_pattern` — per-repo regex consumed by `release_label_present`.

### Per-stage CRUD — `internal/store/convoy_stages.go`

`CreateStage`, `GetStage`, `GetStageByNum`, `ListStages`, `AdvanceStage` (linear-progression validator), `BypassStage` (operator-only emergency cut-through; stamps `gate_passed_at` and `all_prs_merged_at` if missing), `CurrentInFlightStage` (lowest-numbered stage in `{Open, AllPRsMerged, AwaitingGate, GatePassed}` — the per-stage scoping anchor), `LogStageAudit` / `ListStageAuditLog` (per-stage AuditLog actions: `stage_advance`, `stage_abort`, `stage_auto_advance`, `stage_bypass`).

### Review scoping — `internal/agents/convoy_review.go`

`runConvoyReview` calls `store.CurrentInFlightStage(db, convoyID)` and filters `ListConvoyAskBranches` by `stage_id` so each review pass only ingests the active stage's diff. The Senate hook fires once per stage at DraftPROpen. Per-stage scope coverage lives in `internal/agents/convoy_review_per_stage_test.go`.

### Dashboard surface — `internal/dashboard/handlers_staged_convoys.go`

Four endpoints under `/api/convoys/<id>/`:

- `GET stages` — returns ordered stage rows.
- `GET stages/<num>` — stage detail + scoped ask-branches + per-stage AuditLog.
- `POST stages/<num>/advance` — body `{operator, reason, audit_id?}`. Without `audit_id` writes the `operator_confirm` rendezvous key. With `audit_id` matching `^AUDIT-\d+$`, calls `store.BypassStage` (emergency cut-through, audited via `AuditActionStageBypass`).
- `POST stages/<num>/abort` — forces stage to `Failed` (terminal).

## Invariants

- **Astromechs cannot claim Pending-stage tasks.** Pattern **P-StageGate** (`internal/audittools/audit_pattern_p_stage_gate_test.go`) AST-walks every `Claim*` SQL site and rejects Pending-status SELECTs against `BountyBoard` that lack `stage_id IS NULL OR EXISTS (SELECT 1 FROM ConvoyStages cs WHERE cs.id = BountyBoard.stage_id AND cs.status != 'Pending')`. The dispatch gate lives at the SQL layer, not in agent code.
- **No silent post-hoc staging promotion.** Pattern **P-StagingPromotionConfirm** (`docs/patterns/p-staging-promotion-confirm.md`) rejects any production caller of `store.SetConvoyStaging` not on the `stagingPromotionConfirmAllowlist`. The allowlist is empty at HEAD; the only legal `staging_mode` writes happen inside `CreateConvoy` / `CreateStagedConvoy` constructors.
- **Linear stage progression.** `validateStageTransition` rejects any transition that skips a state. `Verified` and `Failed` are terminal — no further transitions. Any non-terminal state may move to `Failed`.
- **Compound gate depth ≤ 5.** Enforced both at planner time (`internal/agents/commander/staging_validator.go`) and defensively at runtime (`Registry.EvaluateGateConfig` rejects depth ≥ `MaxNestingDepth`).
- **Null gate only on terminal stage.** Validator-enforced at convoy creation; runtime accepts any stage but the planner rejects it for non-terminal stages.
- **Emergency bypass requires `AUDIT-NNN` + reason.** The dashboard handler's strict regex `^AUDIT-\d+$` is the only path that lets a stage skip gate evaluation; the audit row carries the AUDIT id and operator reason in the detail blob.
- **`SetConvoyStaging` has zero ungated production callers.** No code in `internal/` or `cmd/` outside the constructor chain calls it. Future callers must go through an operator-confirm predicate and add a `stagingPromotionConfirmAllowlist` entry naming the predicate site.

## Configuration

- **YAML.** No standalone YAML — staging plans are emitted by the Commander directly into `ConvoyStages` rows at convoy creation time. Per-repo release-label regex lives on `Repositories.release_label_pattern` (set via `store.SetRepositoryReleaseLabelPattern`).
- **SystemConfig keys.**
  - `stage_advance_<convoy_id>_<stage_num>` — written by the dashboard `advance` handler when an `operator_confirm` gate is in play; reads back as `<operator>:<rfc3339-timestamp>`. The gate's evaluator checks for non-empty.
- **Datadog / Databricks credentials.** `internal/clients/datadog/inprocess.go` and `internal/clients/databricks/inprocess.go` read from environment / standard credential chains; nil clients at daemon startup mean the corresponding gate types refuse to evaluate (structural error → operator surfaces it).
- **Stage timeout.** `ConvoyStages.gate_timeout_minutes` defaults to 7 days (10080 min). The dog escalates past timeout via `notify.Dispatch(category=gate_timeout_failed)` (Tier-1, DND-bypassable through the budget).

## Operator surface

### Dashboard

The convoy detail panel exposes a per-stage view: each stage row carries status, gate type, gate evaluation summary, and (for stages in `AwaitingGate`) an **Advance** button (operator_confirm gates) plus an **Abort** button. The "view history" panel reads `ListStageAuditLog`. For ungated emergency advance, the modal accepts an `AUDIT-NNN` reference and a free-text reason; both land in the audit row.

### CLI

There is no top-level `force convoy stage` CLI in D5.5 — staged-convoy operations route through the dashboard endpoints. The Commander emits the staging plan as part of its normal feature planning; operator review happens at convoy creation, not via CLI.

### Mail / Slack

Three D11 categories cover the substrate:

- `stage_transition` (Tier-2, default `mail`) — fires on every state change, debounced inside `dogs_convoy_stage_watch.go`.
- `gate_timeout_failed` (Tier-1, default `mail+slack`) — fires when the watcher trips the gate timeout.
- `operator_confirm_required` (Tier-1, default `mail+slack`) — fires when an `operator_confirm` gate becomes the in-flight stage's blocker.

### Audit trail

Every stage transition appends an `AuditLog` row scoped to the convoy id and stage_num via `LogStageAudit`. Action prefix is `stage_*`:

- `stage_advance` — operator-driven advance via the dashboard.
- `stage_abort` — operator-forced abort to `Failed`.
- `stage_auto_advance` — automatic advance by `convoy-stage-watch` after a gate evaluation outcome.
- `stage_bypass` — emergency cut-through with `AUDIT-NNN` + operator reason in the detail blob.

The detail JSON is uniform: `{"stage_num":N,"old_status":"...","new_status":"...","reason":"...","gate_evaluation_summary":"..."}`. The dashboard's per-stage history panel reads `ListStageAuditLog(convoyID, stageNum)` and decodes the JSON for rendering.

## See also

- [`closures/DELIVERABLE-5.5-CLOSURE.md`](../closures/DELIVERABLE-5.5-CLOSURE.md) — full design + per-gate test inventory + forward-compat migration audit.
- [`patterns/p-stage-gate.md`](../patterns/p-stage-gate.md) — astromech dispatch fence (P-StageGate).
- [`patterns/p-staging-promotion-confirm.md`](../patterns/p-staging-promotion-confirm.md) — post-hoc promotion gate (P-StagingPromotionConfirm).
- [`subsystems/convoy-lifecycle.md`](convoy-lifecycle.md) — broader Feature → Convoy → ask-branch lifecycle this slots into.
- [`subsystems/notification-routing.md`](notification-routing.md) — D11 routing applied to the three stage-related categories.
