---
audience: operator
scope: D7 model-tier optimization experiments — paired-runs Haiku-downgrade harness across eight Engineering Corps agents.
owner: feature-team
last_reviewed: 2026-05-07
---

# Model-tier optimization experiments

D7 ships an engineering substrate that lets the Engineering Corps swap a target agent's Claude model on a per-arm basis (control = Sonnet, treatment = Haiku) and measure the cost-vs-quality tradeoff against the baseline-2026 holdout. The deliverable is the **harness + 8 ratification-ready manifests**; the actual experiment runs (each capped at 168h `duration_cap_hours`) plus 30-day post-ship monitoring play out over operator/calendar weeks after engineering closure.

## Overview

D7 piggy-backs on D3's paired-runs primitive. The new pieces:

1. **Per-call model override.** `internal/claude.buildClaudeArgs` accepts a `modelOverride` parameter that surfaces as `--model <id>` on the Claude CLI argv when non-empty. The override is round-tripped through `context.Context` via `WithRequestedModel` / `RequestedModel`, set by the treatments-apply hook at the call boundary.
2. **Manifest ship-gate fields.** `Manifest.ShipGate{Quality, Cost}` and `Manifest.ConfirmPhaseRequired` extend the existing single-treatment manifest. Persisted as JSON in `SystemConfig.experiment_ship_gate_<id>` (mirrors `experiment_promote_<id>`; no schema migration needed).
3. **Eight ratification-ready manifests** at `experiments/E7-1-boot/manifest.yaml` … `experiments/E7-8-chancellor/manifest.yaml`, plus 16 metric SQLs (one quality + one cost per agent) under `metrics/<agent>_<metric>/2026-05-02.sql`.

Anti-cheat-by-default: every D7 manifest declares both quality AND cost ship-gate predicates; promoting on cost alone is forbidden. The Confirm phase is required for all 8 even though `stakes_tier=medium` would otherwise leave it on-demand.

## Components

### Claude CLI override — `internal/claude/claude.go`

- `buildClaudeArgs(prompt, allowedTools, disallowedTools, mcpConfig, maxTurns, outputFormat, modelOverride)` — the lowest level of argv assembly. When `modelOverride != ""` it appends `--model <id>` so the CLI binds the rewritten model.
- `RequestedModel(ctx context.Context) string` — reads back the per-call override stamped onto ctx by the treatments-apply hook.
- `CallWithTranscript*` — auto-stamps `Agent + TaskID` onto the call ctx via `ensureCallCtx` so the treatment-apply hook sees `subject_agent` without per-call-site wiring (no-clobber rule preserves existing inner stamps).

### Experiment lifecycle — `internal/experiments/`

- `lifecycle.go` — single-treatment authoring path. `Manifest{ShipGate, ConfirmPhaseRequired}` rides on the manifest YAML and persists into SystemConfig at `AuthorFromYAML` time. `LoadShipGate(ctx, db, id)` is the reader for a future PromotionAuthor task.
- `factorial_lifecycle.go` / `factorial_manifest.go` — factorial counterparts (D3 Phase 4 ground). D7 uses single-treatment shapes only.
- `ManifestShipGate{Quality string, Cost string}` — both predicates persisted verbatim as human-readable expressions; numerical evaluation lives in PromotionAuthor against live `ExperimentOutcomes` / `LLMCallTranscripts` data.
- Status enum: `authored`, `running`, `confirming`, `terminated` (`StatusAuthored`, `StatusRunning`, `StatusConfirming`, `StatusTerminated`).

### Treatment apply layer — `internal/treatments/`

- `apply.go` — single hot-path entry. Every Claude / git op routes through `Apply(ctx, db, call) (CallDescriptor, []RunAssignment)`. Live mode resolves holdout membership, queries active experiments matching `(subject_agent, assignment_unit)`, deterministically assigns one arm per experiment via `SelectOrthogonalEnrollments`, and rewrites the descriptor's `Model` field.
- `live.go` — live-mode pipeline. Holdout members short-circuit (no enrollment); non-holdout units enroll in the maximal non-conflicting subset of candidate experiments (paired-runs.md § Orthogonal dimension invariant).
- `scheduler.go` — `SelectOrthogonalEnrollments` chooses the largest subset of candidates that don't share a factor / prompt slot / metric. Lowest experiment id wins ties.
- `treatments_apply_mode` SystemConfig key — `'live'` (default) vs `'log_only'` (emergency rollback; pass-through + journal only, no descriptor rewrite, no ExperimentRuns mutation).

### Holdout discipline — `internal/holdout/`

- `baseline.go` — `MintBaseline2026(ctx, db)` inserts the canonical `baseline-2026` row into `GlobalHoldouts`. Idempotent on UNIQUE name. Defaults: `ramp_up_days=7`, `plateau_fraction=0.02`, `fade_days=90`, `fade_start_at=NULL`.
- `assignment.go` — `IsInHoldoutAt(ctx, db, holdoutID, kind, id, when)` is the deterministic membership decision. Holdout members never enroll in experiments.

### Bayesian analysis — `internal/analysis/`

- `bayesian_beta_binomial.go` — `BetaBinomialPosterior`, `NewPosterior`, `ComparePosteriors` (Monte Carlo `P(treatment > control)`), library-free numerical methods.
- `factorial_analysis.go` — `ComputeMainEffects`, `Compute2WayInteractions`, `DecideFactorialOutcome`. Determinism: every Monte Carlo seed pins off `DecisionRule.RandomSeed` (offset per-(factor, level) so different estimates don't collapse onto identical sample paths).
- Registered at daemon startup as `AnalysisFrameworks.version = '2026-04-29'`.

### Metric SQLs — `metrics/<agent>_<metric>/2026-05-02.sql`

16 files, one quality + one cost per agent. Quality SQLs read parse-success / decision-validity proxies from `LLMCallTranscripts`; cost SQLs aggregate `LLMCallTranscripts.cost_usd` across the run's natural unit. Each carries a sibling `2026-05-02.test.sql` (golden-result test) and `2026-05-02.manifest.yaml` (metric metadata).

The 8 covered agents: **boot, memory_rerank, pr_review_triage, librarian, diplomat, medic, commander, chancellor**.

### Experiment manifests — `experiments/E7-N-<agent>/manifest.yaml`

Eight ratification-ready YAMLs. Each:

- `subject_agent: <agent>` (unique across the 8 — orthogonality flows from this).
- `treatments`: control arm `model: claude-sonnet-4-7`; treatment arm `model: claude-haiku-4-5-20251001`.
- `metrics`: primary quality metric (`<agent>_decision_accuracy` / `_relevance` / `_synthesis_quality` / `_summarization_quality` / `_plan_validity` / `_plan_merge_rate`) + secondary cost metric (`<agent>_cost_per_call`).
- `ship_gate.quality`: e.g. `"P(haiku decision_accuracy >= control - 0.05) > 0.95"`.
- `ship_gate.cost`: `"haiku cost_per_call < 0.4 * control cost_per_call"`.
- `confirm_phase_required: true`.
- `stakes_tier: medium`, `min_practical_effect: 0.05`, `duration_cap_hours: 168`, `budget_usd: 10`, `hard_cap_usd: 25`, `assignment_unit: task`, `analysis_framework_version: "2026-04-29"`.

## Invariants

- **No promoting on cost alone.** `internal/experiments/d7_manifest_test.go` parses every `experiments/E7-*/manifest.yaml` and asserts `ship_gate.quality` references the primary metric AND `ship_gate.cost` encodes the `< 0.4 × control` invariant. Both must clear before PromotionAuthor mints a PromotionProposal.
- **No cherry-picking the ship gate per experiment.** Same on-disk parse loop walks all 8 manifests and asserts uniform predicate shapes.
- **No factorial-cell collapse.** Each manifest's `subject_agent` field is unique across the 8; the treatment dimension (`model`) is identical. Orthogonality flows from subject-agent uniqueness per `paired-runs.md` § Factorial Scoring.
- **No shortcut on Confirm.** All 8 manifests declare `confirm_phase_required: true` despite `stakes_tier: medium` defaulting to on-demand confirm. The on-disk parse loop asserts `confirm == true` for every manifest.
- **Holdout never enrolls.** `treatments.applyLive` short-circuits for holdout members; the global holdout never participates in any experiment, model-tier or otherwise. Membership is decided once at natural-unit creation and inherited downstream.
- **Determinism.** `EnrollFactorialUnit` and `SelectOrthogonalEnrollments` are pure functions of `(experiment_id, unit_kind, unit_id)`. The same unit always lands in the same cell of the same experiment.
- **No subprocess shell-out for LLM calls.** Pattern P16 (`internal/audittools/audit_pattern_p16_clients_interfaces_test.go`) ensures cross-agent service deps route through `internal/clients/<svc>/` interfaces; the Claude CLI invocation goes through `internal/claude` only.

## Configuration

- **YAML.** `experiments/E7-N-<agent>/manifest.yaml` (8 manifests). Authored once, ratified by the operator via `force experiment ratify <id>`.
- **Metric SQL.** `metrics/<agent>_<metric>/2026-05-02.sql` + sibling `*.test.sql` + `*.manifest.yaml`. Bound to manifest via `analysis_framework_version` + `metric_name` + `metric_version`.
- **SystemConfig keys.**
  - `treatments_apply_mode` — `'live'` (default) | `'log_only'` (emergency rollback).
  - `experiment_ship_gate_<id>` — JSON-marshalled `ManifestShipGate + ConfirmPhaseRequired`.
  - `experiment_promote_<id>` — JSON `Promote{RuleKey, ProposedContent}` (existing D3 shape).
- **Schema.** `Experiments`, `ExperimentTreatments`, `TreatmentSpecs`, `ExperimentMetrics`, `ExperimentRuns`, `ExperimentOutcomes`, `LLMCallTranscripts.cost_usd`, `GlobalHoldouts`, `AnalysisFrameworks`, `TreatmentApplyLog`. Single source of cost-per-call truth: `LLMCallTranscripts.cost_usd` (introduced in D3).

## Operator surface

### CLI

```bash
force experiment author experiments/E7-N-<agent>/manifest.yaml   # parse + persist (status='authored')
force experiment ratify <id>                                     # status='running'
force experiment status <id>                                     # current outcome / cell means
force experiment terminate <id> --reason=window_elapsed          # close out
force fleet-progress --metric <agent>_decision_accuracy --compare holdout --window 30d
```

The 8 D7 manifests are ratification-ready at engineering closure. The operator runs `force experiment author` + `force experiment ratify` per manifest to start each experiment's clock. `treatments_apply_mode` flips to `log_only` via a single SystemConfig write for emergency rollback (no re-deploy).

### Dashboard

The fleet-progress dashboard surfaces per-agent quality and cost-per-call deltas against the baseline-2026 holdout. Aggregate fleet cost-per-convoy delta lives on the same surface. Both readings populate post-experiment-runtime; engineering closure does not depend on them.

### Mail / Slack

D7 inherits the standard D11 categories — `promotion_proposal_pending` (Tier-1, `mail+slack`) fires when a PromotionAuthor mints a proposal that needs operator ratification.

## See also

- [`closures/DELIVERABLE-7-CLOSURE.md`](../closures/DELIVERABLE-7-CLOSURE.md) — full D7 engineering closure with verification commands and disclosed deviations.
- [`subsystems/paired-runs.md`](paired-runs.md) — D3 paired-runs primitive (the substrate D7 builds on).
- [`subsystems/notification-routing.md`](notification-routing.md) — D11 routing for `promotion_proposal_pending`.
- [`closures/DELIVERABLE-3-CLOSURE.md`](../closures/DELIVERABLE-3-CLOSURE.md) — paired-runs framework closure.
