---
audience: both
scope: D3 paired-runs design — experimentation as a fleet primitive with global holdout and evidence-gated rule promotion.
owner: D3
last_reviewed: 2026-05-05
---

# Paired Runs — Fleet Experimentation as a Primitive

Technical design document for adding experimentation as a first-class fleet primitive. Every decision the fleet makes — prompts, memories, rules, models, context sizes, max_turns, tool availability, agent routing thresholds — becomes testable through the same mechanism. Rule promotions into the fleet's permanent configuration are gated by experimental evidence, not operator intuition.

This is the infrastructure layer beneath the promotion pipeline in [next-gen-agents.md](../next-gen-agents.md). Senate, ISB, BoS, and the evolved Librarian all emit signals that become hypotheses; those hypotheses become experiments; winning experiments promote into the fleet's rule registry. This document specifies how that middle stage works.

---

## Goals

1. **Generalize experimentation.** A single `treatments.Apply` ingress lets any LLM-invoking call be subject to swaps of prompt / memory / rule-set / model / max_turns / context / tools.
2. **Factorial by default.** Experiments can vary multiple dimensions simultaneously; scoring recovers main effects and 2-way interactions without separate experiments per dimension.
3. **Full historical reproducibility.** Any experiment from any time period can be re-scored against its original or current metrics; frozen snapshots of every treatment, metric, and analysis framework are retained indefinitely.
4. **Evidence-gated promotion.** Rules enter the fleet's permanent configuration only via the promotion pipeline, which requires terminated experimental evidence plus operator ratification.
5. **Long-term honesty.** A permanent global holdout cohort measures aggregate fleet evolution against a frozen reference state; prevents local-minimum optimization from masquerading as progress.
6. **Self-healing experimentation.** Engineering Corps authors, monitors, and terminates experiments autonomously within operator-set constraints. The operator's attention is reserved for ratification, not authorship.

## Non-goals

- Replacing Captain, Council, ConvoyReview, or any existing review gate. Experiments measure *which configurations of those gates perform best*; they do not adjudicate specific code changes.
- Multi-tenant experiments across multiple fleets. Scoped to a single fleet instance.
- Real-time feature flags. This is an experimentation system, not a feature-flag service; changes that need runtime toggling for operational reasons use the existing SystemConfig + e-stop mechanisms.
- Bandit-style adaptive allocation in v1. Fixed cell weights with Bayesian termination; bandit allocation is a v2 candidate.

---

## The Primitive

Four concepts compose everything else:

### Treatment

A **treatment** is a specific configuration across one or more dimensions. Dimensions are the axes of variation supported by `treatments.Apply`:

| Dimension | Description |
|---|---|
| `prompt` | System + user prompt templates for a specific agent |
| `memory` | Memory bundle injected into the call (per-repo / per-agent) |
| `rules` | Set of active `FleetRules` for the call |
| `model` | Anthropic model identifier |
| `max_turns` | Claude CLI max-turn budget |
| `context_size` | Context-window byte cap |
| `tools` | Set of tools available to the call |
| `routing_thresholds` | Agent-internal decision parameters (Flavor A; e.g., Medic's decision-threshold values) |

Each treatment pins a specific value on one or more dimensions. A treatment that varies `prompt` and `rules` can be used to test "does new prompt + new rules, together, win?"

### Experiment

An **experiment** declares a hypothesis and the treatment arms that test it. Factorial experiments declare multiple dimensions; the arms span the cross product (or a chosen subset of cells).

### Cell

A **cell** is a specific combination of dimension values in a factorial design. For a 2×2 experiment with dimensions `{prompt, rules}` and values `{prompt ∈ {A, B}, rules ∈ {on, off}}`, the four cells are `(A,on), (A,off), (B,on), (B,off)`. Every run belongs to exactly one cell.

### PairedRun

Two execution modes produce comparable data:

- **Holdout mode (default).** A single run executes. Its treatment is determined by deterministic bucketing. Scoring is relative to other runs in the same experiment, differing in cell membership. Cheap; no extra spend beyond the baseline call.
- **Paired shadow mode (for tool-using agents and high-stakes confirms).** Two runs execute in parallel: the real arm (commits to real branches, hits real gh/Jenkins) and a shadow arm (commits to throwaway shadow branch, gh calls intercepted and replayed from real arm, CI suppressed). The shadow arm's artifact is scored alongside the real arm's. Costs 2× tokens for the experimented-on slice of traffic.

Holdout is what runs continuously. Paired shadow is what runs during **confirm phases** after holdout declares a likely winner, and for experiments on tool-using agents (Astromech, Pilot) where uncontrolled confounding is too costly.

---

## Three-Layer Versioning

Every experimental decision is immutable after termination and reproducible years later. This requires versioning three independent things.

### Layer 1 — Metric versions (Stripe-style, per-metric, date-stamped)

Metrics are reviewed SQL files. Each edit creates a new version identified by publication date. Versions are immutable; old versions remain callable indefinitely.

```
metrics/
  captain_rejection_rate/
    2026-04-23.sql
    2026-04-23_test.sql
    2026-04-23.manifest.yaml
    2026-06-11.sql
    2026-06-11_test.sql
    2026-06-11.manifest.yaml
    2026-06-11.changelog.md
```

An experiment YAML references metrics as `captain_rejection_rate` (resolves to latest at experiment-start) or `captain_rejection_rate@2026-04-23` (explicit pin). Resolution is frozen on experiment start; every `ExperimentRuns` row records the exact `(metric_name, version)` used to score it.

### Layer 2 — Analysis framework versions

The analysis framework is the code that turns scores into decisions: Bayesian posterior calculation, declare-winner thresholds, detected-degradation thresholds, factorial decomposition policy, confirm-phase triggers, min-runs-for-kill. Today these live as constants in Go; versioning means extracting them into YAML config.

```
analysis/
  2026-04-23.yaml
  2026-06-01.yaml
```

Sample framework config:

```yaml
version: 2026-04-23
description: Initial Bayesian framework, factorial main-effects + 2-way interactions

posterior:
  algorithm: bayesian_beta_binomial
  prior:
    type: uniform
    params: {alpha: 1.0, beta: 1.0}

declare_winner:
  required: [p_winner_gt, p_effect_gt_practical]
  p_winner_gt: 0.95               # default for medium tier
  p_effect_gt_practical: 0.95

declare_null:
  p_no_practical_effect_gt: 0.95

detect_degradation:
  p_worse_by_practical_gt: 0.9
  min_runs_for_kill: 20

confirm_phase:
  trigger: on_winner_declared
  n_runs: 30
  must_also_clear_degradation: true

factorial:
  decomposition: main_effects_plus_2way
  max_interaction_order: 2
  warn_on_imbalance_ratio: 3.0
```

An `algorithm` identifier (`bayesian_beta_binomial`) maps to registered Go code. New algorithms get new identifiers; old identifiers are never un-registered, so historical experiments remain reproducible.

### Layer 3 — Treatment spec versions

At experiment start, every treatment's artifact references are resolved to their concrete state and frozen. Prompts and memories become content snapshots (byte-identical reproduction); rule sets become lists of `FleetRules.id` FKs (since `FleetRules` rows are themselves immutable); models become identifier strings with a health-watch (see [Model deprecation](#model-deprecation)).

```
TreatmentSpecs
  id                       INTEGER PRIMARY KEY
  spec_hash                TEXT UNIQUE        -- SHA256 of normalized spec
  prompt_template_ref      TEXT               -- 'captain/default@<git-sha>'
  prompt_template_content  TEXT               -- frozen snapshot
  rule_set_refs_json       TEXT               -- JSON array of FleetRules.id
  memory_bundle_ref        TEXT
  memory_bundle_content    TEXT
  model_identifier         TEXT
  max_turns                INTEGER
  context_size_bytes       INTEGER
  tool_availability_json   TEXT
  routing_thresholds_json  TEXT
  created_at               TIMESTAMP
```

The `spec_hash` unique constraint lets identical treatments across different experiments share one row — enabling cross-experiment questions like "has this exact treatment ever won?"

### Reproducibility property

Given any `experiment_id`:

1. Load `Experiments.analysis_framework_version` → load config → instantiate algorithm from binary.
2. For each arm, load `TreatmentSpecs.*_content` → reconstruct exact treatment.
3. For each run, load `(metric_name, version)` → load frozen SQL → re-score if desired.
4. Numbers are identical to what the operator saw at ratification.

---

## Data Model

```
Experiments
  id                           INTEGER PRIMARY KEY
  name                         TEXT
  hypothesis_text              TEXT               -- required
  min_practical_effect         REAL               -- required
  stakes_tier                  TEXT               -- low | medium | high | safety_critical
  declare_threshold_override   REAL               -- nullable; requires operator approval
  factorial_dimensions_json    TEXT               -- JSON array, e.g. ["prompt","rules"]
  subject_agent                TEXT               -- 'captain' | 'chancellor' | etc.
  assignment_unit              TEXT               -- 'feature' | 'convoy' | 'task'
  analysis_framework_version   TEXT               -- FK → AnalysisFrameworks.version
  status                       TEXT               -- authored | ratified | running | confirming | terminated
  termination_reason           TEXT               -- nullable
  budget_usd                   REAL
  hard_cap_usd                 REAL
  duration_cap_hours           INTEGER
  confirm_phase_id             INTEGER            -- nullable; FK to the confirm experiment
  created_by                   TEXT               -- 'engineering-corps' | 'operator:<name>' | 'manual-override'
  created_at                   TIMESTAMP
  ratified_at                  TIMESTAMP          -- nullable
  ratified_by                  TEXT               -- nullable
  started_at                   TIMESTAMP
  terminated_at                TIMESTAMP

ExperimentTreatments
  id                           INTEGER PRIMARY KEY
  experiment_id                INTEGER FK
  arm_label                    TEXT               -- 'control', 'tight_rules', ...
  cell_json                    TEXT               -- {"prompt":"B","rules":"on"}
  treatment_spec_id            INTEGER FK → TreatmentSpecs
  target_cell_weight           REAL               -- 0.25 for balanced 2×2

ExperimentMetrics
  id                           INTEGER PRIMARY KEY
  experiment_id                INTEGER FK
  metric_name                  TEXT
  metric_version               TEXT               -- resolved at experiment start
  direction                    TEXT               -- 'higher_is_better' | 'lower_is_better'
  params_json                  TEXT               -- metric-instantiation parameters
  is_primary                   BOOLEAN            -- one per experiment; drives declare-winner

ExperimentRuns
  id                           INTEGER PRIMARY KEY
  experiment_id                INTEGER FK
  treatment_id                 INTEGER FK → ExperimentTreatments
  cell_json                    TEXT               -- redundant with treatment but indexed for scoring
  natural_unit_kind            TEXT               -- 'feature' | 'convoy' | 'task'
  natural_unit_id              INTEGER
  mode                         TEXT               -- 'holdout' | 'paired_real' | 'paired_shadow'
  paired_with_run_id           INTEGER            -- nullable; self-FK for paired mode
  agent_name                   TEXT
  assigned_at                  TIMESTAMP
  completed_at                 TIMESTAMP
  score                        REAL               -- frozen at scoring time
  score_source                 TEXT               -- 'downstream_verdict' | 'llm_judge' | 'operator_ratification'
  metric_version               TEXT
  model_substituted_from       TEXT               -- nullable; for holdout model substitutions
  model_substituted_to         TEXT               -- nullable
  is_provisional               BOOLEAN            -- true for llm_judge pending downstream

TreatmentApplyLog
  -- log-only mode of treatments.Apply records the call descriptor and
  -- intended assignment without mutating the call; Phase 2 of D3 flips it
  -- to live; this table is the audit trail that lets the live flip be a
  -- config change rather than a code change.
  id                           INTEGER PRIMARY KEY
  applied_at                   TIMESTAMP
  agent_name                   TEXT NOT NULL
  natural_unit_kind            TEXT               -- 'feature' | 'convoy' | 'task'
  natural_unit_id              INTEGER
  prompt_template              TEXT
  model                        TEXT
  in_holdout                   BOOLEAN
  assignments_json             TEXT               -- JSON array of intended assignment records
  mode                         TEXT NOT NULL      -- 'log_only' (Phase 1) | 'live' (Phase 2+)

  INDEX (applied_at)

ExperimentOutcomes
  id                           INTEGER PRIMARY KEY
  experiment_id                INTEGER FK UNIQUE
  terminated_at                TIMESTAMP
  termination_reason           TEXT               -- declared_winner | declared_null | inconclusive | budget_exhausted | emergency_stop | operator_closed
  winner_treatment_id          INTEGER            -- nullable
  winner_posterior             REAL               -- frozen at termination
  winner_effect_estimate       REAL
  cell_means_json              TEXT               -- frozen snapshot
  fleet_state_hash_at_start    TEXT               -- FK → FleetStateSnapshots
  fleet_state_hash_at_end      TEXT
  confirm_phase_outcome        TEXT               -- nullable
  promotion_proposal_id        INTEGER            -- nullable; if emitted

FleetStateSnapshots
  state_hash                   TEXT PRIMARY KEY
  computed_at                  TIMESTAMP
  active_rules_manifest_json   TEXT               -- hash per rule_key
  active_memories_manifest_json TEXT              -- hash per repo memory
  active_models_manifest_json  TEXT               -- model per agent
  active_prompts_manifest_json TEXT               -- prompt version per agent
  agent_binary_git_sha         TEXT

AnalysisFrameworks
  version                      TEXT PRIMARY KEY   -- '2026-04-23'
  config_content               TEXT
  config_hash                  TEXT
  algorithm_git_sha            TEXT
  published_at                 TIMESTAMP
  published_by                 TEXT
  description                  TEXT
  deprecated_at                TIMESTAMP          -- nullable

MetricVersions
  metric_name                  TEXT
  version                      TEXT
  sql_content                  TEXT
  test_content                 TEXT
  manifest_json                TEXT
  published_at                 TIMESTAMP
  published_by                 TEXT
  description                  TEXT
  deprecated_at                TIMESTAMP
  PRIMARY KEY (metric_name, version)

FleetRules
  id                           INTEGER PRIMARY KEY
  rule_key                     TEXT
  category                     TEXT               -- semantic kind: 'architecture' | 'schema' |
                                                  -- 'security' | 'pr-flow' | 'dashboard' |
                                                  -- 'llm-prompt-discipline' | 'self-healing' |
                                                  -- 'senate' | 'bos' | 'isb' | 'fix-narrative'
  agent_scope                  TEXT               -- audience: 'all' | 'operator' | 'claude-code-build'
                                                  -- | 'captain' | 'council' | 'medic' | 'astromech'
                                                  -- | 'diplomat' | 'pilot' | 'convoy-review'
                                                  -- | 'chancellor' | 'commander' | 'investigator'
                                                  -- | 'librarian' | 'auditor' | 'inquisitor'
                                                  -- | 'pr-review-triage' | 'medic-ci' | 'boot'
                                                  -- | 'senate:<repo>' | <comma-separated combinations>
  render_to                    TEXT NOT NULL      -- physical render target (controlled enum):
                                                  --   'claude-md-file'      → CLAUDE.md the file
                                                  --                          (auto-loaded by Claude Code
                                                  --                          + review agents in daemon CWD).
                                                  --                          HARD-CAPPED. Tight criteria —
                                                  --                          rule applies to operator AND
                                                  --                          Claude Code building Force AND
                                                  --                          every review agent.
                                                  --   'agent-prompt'        → rendered ONLY into per-agent
                                                  --                          --append-system-prompt content
                                                  --                          via agent_scope filter; NEVER
                                                  --                          to a shared file.
                                                  --   'fix-log'             → appended to FIX-LOG.md
                                                  --                          (historical narrative; not
                                                  --                          auto-loaded into prompts).
                                                  --   'pattern-test-docstring' → lives in the Pattern test
                                                  --                          file's docstring; CLAUDE.md
                                                  --                          gets a one-line cross-ref.
                                                  --   'per-domain-doc:<file>' → renders to a domain-specific
                                                  --                          markdown file (e.g.,
                                                  --                          'docs/dashboard-conventions.md');
                                                  --                          loaded by relevant agents only.
                                                  --   'discard'             → removed entirely (audit-time
                                                  --                          decision; row kept for history
                                                  --                          but renders nowhere).
  content                      TEXT
  content_hash                 TEXT
  version                      INTEGER            -- increments per rule_key
  active_from                  TIMESTAMP
  active_until                 TIMESTAMP          -- nullable
  promoted_by_experiment_id    INTEGER            -- nullable
  created_by                   TEXT               -- 'engineering-corps' | 'operator:<name>' | 'bootstrap'
  created_at                   TIMESTAMP

  UNIQUE INDEX (rule_key, version)
  PARTIAL INDEX (rule_key) WHERE active_until IS NULL   -- one active per key
  INDEX (render_to, agent_scope) WHERE active_until IS NULL  -- renderer query path

PromotionProposals
  id                           INTEGER PRIMARY KEY
  experiment_id                INTEGER FK
  kind                         TEXT               -- 'promote' | 'demote'
  rule_key                     TEXT               -- nullable for new rules
  proposed_content             TEXT               -- for new or replacement rules
  evidence_summary_json        TEXT               -- cell means, posterior, confirm results
  authored_by                  TEXT               -- 'engineering-corps'
  authored_at                  TIMESTAMP
  ratified_at                  TIMESTAMP          -- nullable
  ratified_by                  TEXT
  rejected_at                  TIMESTAMP          -- nullable
  rejected_reason              TEXT
  ttl_expires_at               TIMESTAMP          -- 14 days from authored_at
  -- concern #7 revert handling:
  rejection_action             TEXT               -- 'leave_as_is' | 'clean_revert' |
                                                  -- 'cascade_revert' | 'surgical_revert' | 'escalate'
  rejection_rationale          TEXT               -- mandatory when rejection_action != 'leave_as_is'
  revert_task_id               INTEGER            -- nullable; spawned CodeEdit that performs the revert
  refiled_feature_id           INTEGER            -- nullable; if rejection re-files as a new feature

GlobalHoldouts
  id                           INTEGER PRIMARY KEY
  name                         TEXT UNIQUE        -- 'baseline-2026'
  reference_date               TIMESTAMP
  fleet_state_hash             TEXT FK
  ramp_up_days                 INTEGER DEFAULT 7
  plateau_fraction             REAL DEFAULT 0.02
  fade_start_at                TIMESTAMP          -- nullable
  fade_days                    INTEGER DEFAULT 90
  retired_at                   TIMESTAMP          -- nullable
  retired_reason               TEXT
  created_by                   TEXT
  notes                        TEXT

ModelAvailability
  model_id                     TEXT PRIMARY KEY
  last_checked_at              TIMESTAMP
  last_success_at              TIMESTAMP
  deprecation_detected_at      TIMESTAMP          -- nullable
  announced_kill_at            TIMESTAMP
  successor_suggested          TEXT
```

Inheritance columns on existing tables:

```
Features       + in_holdout BOOLEAN DEFAULT 0
               + experiment_assignments_json TEXT    -- JSON: experiment_id → treatment_id

Convoys        + in_holdout BOOLEAN DEFAULT 0
               + experiment_assignments_json TEXT
               + parent_feature_id INTEGER          -- if not already present
               + verification_spec_json TEXT         -- acceptance tests, exit criteria,
                                                     -- anti-cheat directives, closure artifacts.
                                                     -- Shape: { ats: [...active...], deprecated: [
                                                     --   {at_id, removed_at, removed_by_email,
                                                     --    rationale, removal_kind:
                                                     --    'mistake'|'superseded'|'satisfied'|'out_of_scope',
                                                     --    superseded_by: {kind, ref}}
                                                     -- ] }  (concern #9)
               + spec_history_json TEXT              -- operator-ratified amendment audit trail.
                                                     -- Append-only; entries: {version, kind:
                                                     -- 'add'|'modify'|'deprecate', at_id, rationale,
                                                     -- proposed_by, ratified_at, ratified_by_email}
                                                     -- (concerns #6 + #9)
               + critical BOOLEAN DEFAULT 0          -- triage-priority flag

BountyBoard    + in_holdout BOOLEAN DEFAULT 0
               + experiment_assignments_json TEXT
               + proposed_action_json TEXT           -- Captain's spec-amendment proposal for
                                                     -- unmapped spawns (cited_ats, cited_fleet_rules,
                                                     -- spec_link, classification_confidence,
                                                     -- captain_reasoning, draft_amendment, alternative)
               + prompt_version TEXT                 -- which prompt produced this decision
               + prior_review_outcomes_json TEXT     -- chain of agent decisions on this task
               + spawn_spec_link TEXT                -- 'tied_to_AT-NNN' | 'glue' | 'unmapped' (when
                                                     -- this row was spawned by Captain)
               + spawn_classification_confidence TEXT  -- 'high' | 'medium' | 'low'
               + spawning_at_id TEXT DEFAULT ''      -- concern #9: which AT a ConvoyReview-spawned fix
                                                     -- task targets (for in-flight check on AT removal)
               + deferred_revert BOOLEAN DEFAULT 0   -- concern #7: row scheduled to revert when its
                                                     -- dependents complete (cascade-revert flow)
               + revert_target_task_id INTEGER       -- concern #7: nullable; the task this row reverts

TaskHistory    + prompt_version TEXT                 -- enables per-prompt-version metric correlation
```

Tables introduced by the bundled additions (concerns #1–#5 + cross-cutting):

```
ProposedFeatures              -- Investigator's cross-convoy aggregation queue
  id                  INTEGER PRIMARY KEY
  observation_summary TEXT NOT NULL          -- one-line operator-facing description
  category            TEXT NOT NULL          -- 'category_b_new_work' | 'category_c_spec_amendment' | etc.
  source              TEXT NOT NULL          -- 'investigator' | 'captain' | 'ec' | 'operator' | 'convoy_review'
  source_observations TEXT                   -- JSON array of {convoy_id, agent, evidence}
  fingerprint         TEXT NOT NULL DEFAULT ''  -- canonical-content SHA256 (concern #10 dedup)
  occurrence_count    INTEGER DEFAULT 1      -- bundled across multiple convoys
  first_seen_at       TIMESTAMP              -- alias of first_observed_at; new canonical name
  last_seen_at        TIMESTAMP              -- alias of last_observed_at; new canonical name
  evidence_history_json TEXT DEFAULT '[]'    -- per-occurrence evidence trail (append-only)
  value_score         TEXT NOT NULL DEFAULT 'medium' CHECK(value_score IN ('low','medium','high'))
  complexity_score    TEXT NOT NULL DEFAULT 'medium' CHECK(complexity_score IN ('low','medium','high'))
  value_rationale     TEXT DEFAULT ''        -- proposer's one-line justification
  complexity_rationale TEXT DEFAULT ''       -- proposer's one-line justification
  scored_by           TEXT NOT NULL DEFAULT '' -- matches `source` at insert; updated to 'operator' on override
  promoted_at         TIMESTAMP              -- operator-marked active interest (concern #10)
  promotion_deadline  TIMESTAMP              -- self-imposed deadline at promotion time
  status              TEXT DEFAULT 'pending' -- 'pending' | 'spawned_convoy' | 'merged' | 'discarded'
  decided_at          TIMESTAMP              -- nullable
  decided_by          TEXT                   -- 'operator:<name>'
  decision_action     TEXT                   -- 'new_convoy:<id>' | 'amendment:<convoy_id>' | 'discard:<reason>'
  archived_at         TIMESTAMP              -- nullable; soft-archive (housekeeping dog or operator)
  archive_reason      TEXT                   -- nullable; why this row was archived

  -- Partial UNIQUE: enforces dedup on active rows; archived dups allowed for history
  -- CREATE UNIQUE INDEX idx_pf_active_fingerprint ON ProposedFeatures(fingerprint)
  --   WHERE archived_at IS NULL AND fingerprint != '';

ProposedFeatureSuppressions   -- operator-installed mute rules; concern #10
  id                  INTEGER PRIMARY KEY
  fingerprint         TEXT NOT NULL          -- matches ProposedFeatures.fingerprint
  rationale           TEXT NOT NULL          -- ≥ 20 chars; CHECK(length(rationale) >= 20)
  suppressed_until    TIMESTAMP NOT NULL     -- max 1 year out; no infinite mutes
  created_at          TIMESTAMP DEFAULT (datetime('now'))
  created_by_email    TEXT NOT NULL
  -- INDEX idx_pfs_fp ON ProposedFeatureSuppressions(fingerprint, suppressed_until);

ProposedFeatureScoreOverrides -- audit trail for operator score changes; concern #10
  id                     INTEGER PRIMARY KEY
  proposed_feature_id    INTEGER NOT NULL
  prior_value_score      TEXT
  prior_complexity_score TEXT
  new_value_score        TEXT
  new_complexity_score   TEXT
  rationale              TEXT NOT NULL       -- mandatory; why operator overrode the LLM-suggested score
  overridden_at          TIMESTAMP DEFAULT (datetime('now'))
  overridden_by_email    TEXT NOT NULL

AdversarialPairings           -- Council/Medic/ConvoyReview adversarial-pair results
  id                  INTEGER PRIMARY KEY
  decision_id         INTEGER NOT NULL       -- references the original task/decision
  agent               TEXT NOT NULL          -- 'council' | 'medic' | 'convoy_review'
  primary_outcome     TEXT NOT NULL          -- structured decision from primary prompt
  critic_outcome      TEXT NOT NULL          -- structured decision from critic prompt
  agreement           BOOLEAN                -- true if outcomes match
  surfaced_at         TIMESTAMP              -- nullable; set when surfaced to operator
  operator_resolution TEXT                   -- nullable; what operator decided when surfaced
  created_at          TIMESTAMP

GoldenSetFixtures             -- Curated input fixtures with known-correct outputs
  id                  INTEGER PRIMARY KEY
  agent               TEXT NOT NULL          -- 'captain' | 'council' | 'medic' | etc.
  input               TEXT NOT NULL          -- the input the prompt receives
  expected_output     TEXT NOT NULL          -- known-correct structured output
  source              TEXT NOT NULL          -- 'auto-clean-shipping' | 'operator-curated' | 'archaeologist'
  curated_at          TIMESTAMP
  curated_by          TEXT                   -- 'system' | 'operator:<name>' | 'archaeologist'
  retired_at          TIMESTAMP              -- nullable; for fixtures no longer relevant

GoldenSetEvaluations          -- Periodic prompt-vs-fixture evaluation results
  id                  INTEGER PRIMARY KEY
  agent               TEXT NOT NULL
  prompt_version      TEXT NOT NULL
  fixture_id          INTEGER NOT NULL FK → GoldenSetFixtures
  actual_output       TEXT NOT NULL          -- what the current prompt produced
  accuracy_score      REAL                   -- 0.0-1.0; how closely actual matches expected
  evaluated_at        TIMESTAMP
  -- Aggregated per (agent, prompt_version, week) for accuracy-trend tracking

CalibrationAuditSamples       -- Weekly calibration sample widget records
  id                  INTEGER PRIMARY KEY
  sample_week         TEXT NOT NULL          -- ISO week identifier
  proposal_id         INTEGER NOT NULL FK → PromotionProposals
  selection_bucket    TEXT NOT NULL          -- 'fast_high_stakes' | 'high_approve_rate' | 'random'
  surfaced_at         TIMESTAMP
  operator_action     TEXT                   -- 'confirm' | 'still_approve_after_review' | 'should_have_been_rejected' | 'snoozed' | NULL
  operator_acted_at   TIMESTAMP
  operator_rationale  TEXT                   -- when 'should_have_been_rejected'

ConvoyReviewCycles            -- concern #6: atomic snapshot evaluations against a frozen spec
  id                           INTEGER PRIMARY KEY
  convoy_id                    INTEGER NOT NULL FK → Convoys
  cycle_number                 INTEGER NOT NULL              -- monotonic per convoy
  spec_version_at_start        TEXT NOT NULL                 -- snapshot of spec version this cycle ran against
  cycle_started_at             TIMESTAMP NOT NULL
  cycle_completed_at           TIMESTAMP                     -- nullable while in flight
  outcomes_json                TEXT                          -- {AT-NNN: 'passed'|'failed'|'inconclusive', ...}
  fix_tasks_spawned_json       TEXT                          -- [{task_id, target_at_id, ...}]
  amendments_proposed_json     TEXT                          -- LLM-suggested spec amendments (operator-ratifies)
  amendments_ratified_during_cycle_json TEXT                 -- audit: which amendments operator approved this cycle
  UNIQUE (convoy_id, cycle_number)
```

Adjacent FleetRules schema additions (Phase 1 of D3):

```
FleetRules     + enforced_by TEXT             -- references a Pattern test ID
                                              -- (e.g., 'TestPattern_P12') OR 'trust-only'
                                              -- if no mechanical enforcement exists.
                                              -- Insert rejects rules with neither.
FleetRules     + render_to TEXT NOT NULL      -- physical render target. Controlled enum:
                                              --   'claude-md-file' | 'agent-prompt'
                                              --   | 'fix-log' | 'pattern-test-docstring'
                                              --   | 'per-domain-doc:<file-path>' | 'discard'.
                                              -- See § Rule Registry → Rendered exports.
                                              -- Bootstrap migration audit categorizes every
                                              -- absorbed CLAUDE.md section by render_to;
                                              -- default is NOT 'claude-md-file' — auditor
                                              -- must justify each universal-load rule.
```

Dashboard tables introduced for D3 Phase 6 (concern #11 + Drill); landed in Phase 1's schema migration so 6A/6B can build against a stable data layer:

```
DashboardHealthHeartbeats     -- 6A.2
  id INTEGER PRIMARY KEY
  ticked_at TIMESTAMP NOT NULL
  process_pid INTEGER
  bind_addr TEXT
  in_flight_requests INTEGER
  -- INDEX idx_dh_heartbeats_recent ON DashboardHealthHeartbeats(ticked_at DESC);

OperatorNotificationBudgets   -- 6A.4
  id INTEGER PRIMARY KEY
  operator_email TEXT NOT NULL
  source TEXT NOT NULL                   -- 'investigator'|'captain'|'ec'|'fleet'|'convoy_review'|'medic'|...
  channel TEXT NOT NULL                  -- 'email'|'modal'|'banner'
  max_per_period INTEGER NOT NULL
  period_minutes INTEGER NOT NULL
  digest_remainder BOOLEAN NOT NULL DEFAULT 1
  UNIQUE(operator_email, source, channel)

OperatorNotificationDigest    -- 6A.4
  id INTEGER PRIMARY KEY
  operator_email TEXT NOT NULL
  source TEXT NOT NULL
  channel TEXT NOT NULL
  digest_for_date TEXT NOT NULL          -- 'YYYY-MM-DD'
  payload_json TEXT NOT NULL
  flushed_at TIMESTAMP
  UNIQUE(operator_email, source, channel, digest_for_date)

OperatorSessionState          -- 6A.5
  id INTEGER PRIMARY KEY
  operator_email TEXT NOT NULL UNIQUE
  last_active_at TIMESTAMP DEFAULT (datetime('now'))
  last_viewed_surface TEXT               -- 'pulse'|'briefing'|'reflection'|'drill'
  last_viewed_route TEXT                 -- full URL fragment
  last_focused_decision_id INTEGER
  partial_review_state_json TEXT         -- bounded at 32 KB at write time

OperatorTrustDials            -- 6A.6
  id INTEGER PRIMARY KEY
  operator_email TEXT NOT NULL
  agent TEXT NOT NULL
  dial_value INTEGER NOT NULL CHECK(dial_value BETWEEN 0 AND 100)
  set_at TIMESTAMP DEFAULT (datetime('now'))
  set_by TEXT NOT NULL                   -- 'operator'|'calibration_suggestion'|'system_default'
  rationale TEXT
  UNIQUE(operator_email, agent, set_at)  -- history-preserving; latest = current

NarrativeRenders              -- 6A.7
  id INTEGER PRIMARY KEY
  rendered_at TIMESTAMP DEFAULT (datetime('now'))
  event_window_start TIMESTAMP NOT NULL
  event_window_end TIMESTAMP NOT NULL
  source_event_count INTEGER NOT NULL
  source_event_refs_json TEXT NOT NULL   -- [{kind, ref}, ...] click-through to drill
  prose TEXT NOT NULL
  prompt_version TEXT NOT NULL
  cost_usd REAL
  cache_hit BOOLEAN
  -- INDEX idx_nr_window ON NarrativeRenders(event_window_end DESC);

BriefingRenders               -- 6A.10 + 6A.11
  id INTEGER PRIMARY KEY
  decision_id INTEGER NOT NULL
  decision_kind TEXT NOT NULL            -- 'captain_proposal'|'spec_amendment'|'promotion_proposal'|'proposed_feature'|...
  rendered_at TIMESTAMP DEFAULT (datetime('now'))
  briefing_text TEXT NOT NULL
  prior_similar_decisions_json TEXT      -- [{decision_id, outcome, when, context}, ...]
  prompt_version TEXT NOT NULL
  cost_usd REAL
  operator_decision TEXT                 -- 'approved'|'rejected'|'deferred'
  decision_time_seconds INTEGER
  counter_proposal_kind TEXT             -- 'whole_thing'|'different_approach'|'defer'
  counter_proposal_text TEXT
  counter_proposal_routed_id INTEGER     -- spawned proposal/task on rejection-with-counter
  -- INDEX idx_br_decision ON BriefingRenders(decision_kind, decision_id, rendered_at DESC);

CooldownPauses                -- 6A.13
  id INTEGER PRIMARY KEY
  decision_id INTEGER NOT NULL
  decision_kind TEXT NOT NULL
  scheduled_action_at TIMESTAMP NOT NULL
  paused_at TIMESTAMP
  paused_by_email TEXT
  resumed_at TIMESTAMP
  cancelled_at TIMESTAMP
  executed_at TIMESTAMP
  -- INDEX idx_cp_pending ON CooldownPauses(scheduled_action_at) WHERE executed_at IS NULL AND cancelled_at IS NULL;

OperatorAttentionTags         -- 6A.14
  id INTEGER PRIMARY KEY
  operator_email TEXT NOT NULL
  target_kind TEXT NOT NULL              -- 'convoy'|'feature'|'agent'|'rule_key'
  target_id TEXT NOT NULL
  attention_level TEXT NOT NULL CHECK(attention_level IN ('following','normal','muted'))
  set_at TIMESTAMP DEFAULT (datetime('now'))
  rationale TEXT                         -- required when attention_level='muted'
  UNIQUE(operator_email, target_kind, target_id)

LLMCallTranscripts            -- 6B.1; redaction at write time per Fix #10
  id INTEGER PRIMARY KEY
  task_id INTEGER
  agent TEXT NOT NULL
  prompt_version TEXT NOT NULL
  call_started_at TIMESTAMP NOT NULL
  call_completed_at TIMESTAMP
  system_prompt TEXT NOT NULL            -- pre-redacted
  user_prompt TEXT NOT NULL              -- pre-redacted
  response_text TEXT                     -- pre-redacted
  tool_calls_json TEXT                   -- [{tool, args, result, duration_ms}, ...]
  cost_usd REAL
  input_tokens INTEGER
  output_tokens INTEGER
  cache_read_tokens INTEGER
  cache_creation_tokens INTEGER
  archived_at TIMESTAMP                  -- when body offloaded to disk
  -- INDEX idx_llmct_task ON LLMCallTranscripts(task_id, call_started_at);
  -- INDEX idx_llmct_agent ON LLMCallTranscripts(agent, call_started_at);

GitOperationLog               -- 6B.2
  id INTEGER PRIMARY KEY
  task_id INTEGER
  convoy_id INTEGER
  repo TEXT NOT NULL
  operation TEXT NOT NULL                -- 'fetch'|'push'|'rebase'|'force-push'|'merge'|'reset'|'worktree-add'|'gh-pr'|'gh-checks'|...
  args_json TEXT                         -- pre-redacted
  started_at TIMESTAMP NOT NULL
  duration_ms INTEGER
  exit_code INTEGER
  stdout_excerpt TEXT                    -- truncated to 4 KB
  stderr_excerpt TEXT                    -- truncated to 4 KB
  branch TEXT
  before_sha TEXT
  after_sha TEXT
  -- INDEX idx_gol_convoy ON GitOperationLog(convoy_id, started_at);
  -- INDEX idx_gol_task ON GitOperationLog(task_id, started_at);

OperatorEventAnnotations      -- 6B.8
  id INTEGER PRIMARY KEY
  operator_email TEXT NOT NULL
  event_kind TEXT NOT NULL               -- 'llm_call'|'task_transition'|'git_op'|'narrative'|'cycle'|'ruling_council'|...
  event_ref TEXT NOT NULL
  note_text TEXT NOT NULL
  flag TEXT                              -- 'problem'|'interesting'|'follow_up'|NULL
  noted_at TIMESTAMP DEFAULT (datetime('now'))
  -- INDEX idx_oea_event ON OperatorEventAnnotations(event_kind, event_ref);
  -- INDEX idx_oea_flag ON OperatorEventAnnotations(flag, noted_at) WHERE flag IS NOT NULL;

ReplayResults                 -- 6B.7
  id INTEGER PRIMARY KEY
  original_event_id INTEGER NOT NULL
  original_event_kind TEXT NOT NULL      -- 'captain_ruling'|'council_ruling'|'convoy_review_cycle'|'medic_decision'
  replay_prompt_version TEXT NOT NULL
  replay_started_at TIMESTAMP DEFAULT (datetime('now'))
  replay_response TEXT
  decision_changed BOOLEAN
  cost_usd REAL
  triggered_by_email TEXT NOT NULL

FleetLearningPanels           -- 6B.12
  id INTEGER PRIMARY KEY
  rendered_at TIMESTAMP DEFAULT (datetime('now'))
  prose TEXT NOT NULL
  cost_usd REAL
  prompt_version TEXT NOT NULL
  source_event_refs_json TEXT
```

Per-task implementation prompts and validation prompts for every dashboard sub-track live in `docs/subsystems/dashboard-implementation.md`. The schema additions above are the data-layer prerequisites that must land in Phase 1.

---

## Integration Surface — `treatments.Apply`

A single function is the ingress for the entire system. Every LLM call in the fleet routes through it.

```go
// Apply returns the call descriptor the agent should actually use, after
// applying global holdout membership, active experiment enrollments, and
// cell assignment. Also returns assignment records for ExperimentRuns.
func Apply(ctx context.Context, db *sql.DB, call CallDescriptor) (CallDescriptor, []RunAssignment, error)

type CallDescriptor struct {
    AgentName         string
    NaturalUnitKind   string     // 'feature' | 'convoy' | 'task'
    NaturalUnitID     int
    PromptTemplate    string     // ref, e.g. 'captain/default@HEAD'
    Memory            string
    RuleSetRefs       []int      // FleetRules.id list
    Model             string
    MaxTurns          int
    ContextSize       int
    Tools             []string
    RoutingThresholds map[string]any
    InHoldout         bool       // inherited from the natural unit
}

type RunAssignment struct {
    ExperimentID int
    TreatmentID  int
    Cell         map[string]string
    Mode         string           // 'holdout' | 'paired_real' | 'paired_shadow'
}
```

**Execution order inside `Apply`:**

1. If `call.InHoldout == true`: resolve the active holdout's frozen state, apply to `CallDescriptor`, return with empty assignment list. Skip all experiment logic.
2. Query active experiments whose `subject_agent == call.AgentName` AND `assignment_unit` matches the natural-unit kind AND are in `running` or `confirming` status.
3. For each candidate experiment, compute deterministic cell assignment: `hash(natural_unit_id, experiment_id) % cell_weight_total`.
4. Reject overlap with already-assigned experiments on shared dimensions (factorial orthogonality invariant).
5. Apply each assigned treatment's dimensions to `CallDescriptor` in a deterministic order. Conflicts on orthogonal dimensions are impossible by invariant (4); conflicts on non-dimensional fields are impossible because experiments don't modify non-dimensional fields.
6. Record `RunAssignment` list.
7. If any assigned experiment is in `confirming` status or the agent is in `tool_using_agent_set`, additionally enroll in paired-shadow: spawn the shadow arm (see [Shadow worktrees](#shadow-worktrees)).
8. Return.

**Determinism.** Given the same fleet state, the same call always yields the same assignment. This is the basis for inheritance — a convoy spawned from a feature inherits that feature's assignment without re-hashing.

**Budget check.** Before returning, `Apply` checks `estimated_cost(call) + sum(current_spend for assigned_experiments) <= hard_cap`. If a specific experiment's arm would exceed its hard cap, its treatment is silently dropped from the descriptor (the arm degrades to control for this call). Budget-exhausted experiments don't break calls; they stop participating.

---

## Assignment and Inheritance

### Natural unit per agent

Assignment unit is the topmost work unit the agent operates on. Inheritance is one-way and set-once: when a child unit is created (convoy from feature, task from convoy), it copies its parent's `in_holdout` flag and `experiment_assignments_json`. Never re-hashed, never reconsidered.

| Agent | Assignment unit |
|---|---|
| Chancellor | Feature |
| Commander | Feature (if dispatching new work) or Convoy (if coordinating in-flight) — inherits from claimed work |
| Senate | Plan/Convoy |
| Captain / Council / Medic / ConvoyReview | Convoy |
| PR-review-triage | Convoy (1:1 with PRs) |
| Diplomat | Convoy (ShipConvoy) or Task (PRReviewTriage) — inherits from claimed work |
| Pilot / Astromech | Task |
| Engineering Corps | Never subject to experiments (meta-agent) |

### Holdout inheritance

Global holdout membership is a single boolean (`in_holdout`) decided **once, at the topmost natural unit**, then inherited down the hierarchy:

- `Features.in_holdout` decided at feature creation. 2% land in holdout (subject to ramp/fade schedule — see [Global Holdout](#global-holdout-long-term)).
- `Convoys.in_holdout` inherits from parent `Features.in_holdout` if a parent exists; otherwise hashes itself at creation.
- `BountyBoard.in_holdout` inherits from parent `Convoys.in_holdout` if a parent exists; otherwise hashes itself at creation.

Work units in holdout are **excluded from all experiment assignment**. They run at the active holdout's frozen reference state across every agent that touches them.

### Sticky task retries

When a task is Medic-requeued and re-hits its subject agent, it keeps its original cell assignment (hash is on `natural_unit_id`, which doesn't change on retry). One task's journey through retries is one observation per experiment. When a task is Medic-sharded into children, each child gets fresh assignment (new `natural_unit_id`).

---

## Factorial Scoring

### Cell-based storage

Every `ExperimentRuns` row records its cell as `{"dim_prompt":"B","dim_rules":"on"}`. The analysis layer groups by cell, computes cell means and variances, then derives effects from linear combinations:

- **Main effect of dimension D:** average over all cells, partitioned by D's value.
- **2-way interaction between D1 and D2:** `[mean(D1=a,D2=b) - mean(D1=a',D2=b)] - [mean(D1=a,D2=b') - mean(D1=a',D2=b')]`, non-zero indicates "the best value of D1 depends on D2."

### Orthogonal dimension invariant

Two experiments can run simultaneously on the same call **if and only if** their `factorial_dimensions` sets are disjoint. The scheduler enforces this at assignment time. This makes factorial analysis within each experiment straightforward and keeps cross-experiment contamination impossible by construction.

### Cell balance

Experiment YAML declares `target_cell_weights` per arm. The scheduler uses stratified randomization (hash buckets mapped to target weights) so cells stay balanced. The analysis layer warns if any cell's observed share deviates from target by more than `warn_on_imbalance_ratio` (default 3×).

### Sample-size cost

Factorial experiments require N per *cell*, not per arm. A 2×2 needs 4× the total samples a single-arm experiment needs for equivalent main-effect power. Interactions need ~4× more samples than main effects for equivalent power. This is the tradeoff for simultaneously measuring multiple dimensions. Default experiment budgets scale with cell count.

### Higher-order interactions

3-way and higher interactions are supported but off by default. Experiment YAML opts in with `factorial.max_interaction_order: 3`; dashboard warns that sample size may be inadequate. Default is main effects plus 2-way.

---

## Scoring and Significance

### Declare-winner thresholds by stakes tier

| Tier | `P(winner) >` | `P(effect > practical) >` | Confirm phase required |
|---|---|---|---|
| `low` | 0.80 | 0.80 | No |
| `medium` (default) | 0.95 | 0.95 | On-demand |
| `high` | 0.97 | 0.95 | Yes |
| `safety_critical` | 0.99 | 0.95 | Yes, with `confirm_n_runs × 2` |

Per-experiment override of thresholds is allowed but requires operator approval on the YAML (flagged in the authoring UI).

### The Bayesian caveats (operator-facing)

Four properties the operator needs to understand and the dashboard surfaces:

1. **Priors are loadable.** Default prior is uniform over the plausible range. Informed priors are opt-in per experiment and require operator confirmation on the YAML.
2. **Posterior language avoids "confidence."** Dashboard says "arm B appears better in 95% of simulated futures," not "95% confident."
3. **Minimum practical effect is mandatory.** Experiments cannot declare a winner on `P(winner) > 0.95` alone; `P(effect > min_practical_effect) > 0.95` is also required, preventing "statistically significant, practically meaningless" promotions.
4. **Multiple-comparisons hedge.** Declared winners graduate to a confirm phase (fresh, smaller experiment) before emitting a promotion proposal. Confirm-phase failure auto-closes the experiment as `inconclusive`; the hypothesis enters the historical record.

### Ground-truth scoring — layered

Every experiment declares a primary metric. The scoring pipeline produces a score for each run via:

| Source | Timing | Use |
|---|---|---|
| `downstream_verdict` | Hours to days after run | Primary; what drives winner declaration |
| `llm_judge` | Immediate (at run completion) | Provisional; feeds the dashboard's live posterior; flagged `is_provisional=1` |
| `operator_ratification` | On-demand | Used when `needs_human_scoring=1` is set on the experiment |

Provisional scores are overwritten by downstream-verdict scores when they arrive; the overwrite is recorded so re-analysis can distinguish "scored live" from "scored after verdict." Winner declarations use only non-provisional scores.

### Senate's two-tier scoring

Senate advice is hard to score. Two metrics run in parallel:

- **Tier 1 (immediate, high-fidelity):** advice-citation rate from Chancellor's decision trace. "Did Chancellor incorporate this advice?" Scored per call.
- **Tier 2 (delayed, noisy, matched-pairs):** aggregate convoy-outcome metrics (ship-success rate, rework-cycle count, escalation density, spend-per-shipped-convoy) bucketed by convoy complexity. Compared across advice versions on same-complexity buckets.

Tier 1 drives per-experiment Senate declarations. Tier 2 runs as a background analysis that periodically produces "retention reports" on already-shipped Senate rules, feeding into auto-demotion proposals.

---

## Metric Registry

### Structure

Metrics are reviewed code, not runtime SQL.

```
metrics/
  <metric_name>/
    <date>.sql             # produces (run_id, score) given experiment_id
    <date>_test.sql        # validates against fixture DB
    <date>.manifest.yaml
    <date>.changelog.md    # when >1 version
```

Manifest:

```yaml
name: captain_rejection_rate
version: 2026-04-23
direction: lower_is_better
unit: rate
parameters:
  - name: time_window_hours
    type: integer
    default: 48
  - name: repo_id
    type: integer
    default: null
description: |
  Fraction of Captain reviews that rejected. Scope: specified repo and window.
```

### Proposal flow (LLM-written)

Engineering Corps can propose new metrics (or new versions of existing metrics) when a Librarian hypothesis needs a measurement the registry doesn't have:

1. EC generates `metrics/<name>/<date>.{sql,test.sql,manifest.yaml,changelog.md}`.
2. Commits with `[ENG-CORPS-PROPOSED-METRIC]` marker.
3. **Metric does not go live until operator ratifies.** Metrics are higher stakes than experiments — every future experiment depends on their correctness. Ratification is separate from experiment ratification and typically requires the operator to review the SQL itself, not just the hypothesis.
4. Once ratified, the metric is callable by name forever.
5. `_test.sql` runs against an ephemeral fixture DB on daemon start; metrics whose test fails are disabled until fixed.

### Exploratory metrics

An `exploratory: true` flag on an experiment YAML allows raw SQL-as-metric without registry ratification. Exploratory results are logged but **cannot** declare winners or feed promotion proposals. This is the notebook mode — useful for learning, not for shipping.

### Schema compatibility

The existing CLAUDE.md invariant that schema changes are additive-only is load-bearing for metric longevity. Every published metric version is re-tested against the current schema on every PR; breakage fails the PR. Column removals are prohibited; deprecated-in-practice columns stay until every metric that references them is deprecated.

---

## Rule Registry — `FleetRules`

### DB as source of truth

All fleet rules — what today lives in CLAUDE.md, SENATE.md, BoS rule files, ISB finder configs — live in `FleetRules` as versioned rows. The DB is the source of truth. Markdown files are auto-rendered exports maintained by a `rule-renderer` dog.

### Assembly at call time

Each LLM call's rule preamble is assembled by SQL at `treatments.Apply` time. The query filters by `render_to`, NOT by `category` — `category` is the semantic-kind tag (architecture / security / pr-flow / etc.); `render_to` is the physical render target. Per-agent injection pulls only `agent-prompt` rows scoped to the agent:

```sql
SELECT content FROM FleetRules
WHERE render_to = 'agent-prompt'
  AND active_from <= datetime('now')
  AND (active_until IS NULL OR active_until > datetime('now'))
  AND (
      agent_scope = 'all'
      OR agent_scope = ?                     -- agent name
      OR ',' || agent_scope || ',' LIKE '%,' || ? || ',%'  -- comma-separated multi-scope
  )
ORDER BY category, rule_key
```

The retrieved contents are concatenated with section headings and injected into the system prompt. This is structurally identical to today's "read CLAUDE.md before the call" except the read is now queryable AND filtered to JUST the rules the agent needs (not the universal-load 50 KB).

The CLAUDE.md FILE (auto-loaded by Claude Code + review agents in daemon CWD) is rendered separately via a tighter filter — see [Rendered exports](#rendered-exports) below.

### Experiments on rules

A treatment arm can:

- Activate a not-yet-promoted rule: inject the proposed rule row into the assembly for this arm.
- Deactivate an active rule: filter out rule-ID X from the assembly for this arm.
- Replace a rule's content: substitute rule-ID X's content for this arm with the challenger's content.

Layer 3 Treatment Specs store rule changes as `rule_set_refs_json` — the list of `FleetRules.id` active for the arm, including any proposed-but-not-yet-live IDs. Reproduction is a DB fetch, not a content blob.

### Rendered exports

A `rule-renderer` dog fires on every rule promotion/demotion. The renderer is dispatched by `render_to`, not `category`:

- **`CLAUDE.md`** ← `WHERE render_to='claude-md-file'`, section-ordered. **Hard-capped at 10 KB.** Tight criteria — rule applies to operator AND Claude Code building Force AND every review agent. Almost nothing meets the bar; bulk of today's CLAUDE.md content does NOT belong here.
- **Per-agent system prompts** ← `WHERE render_to='agent-prompt'` filtered by `agent_scope`. Rendered at `treatments.Apply` time into `--append-system-prompt`; never written to a shared file. Each agent only sees rules tagged for them; aggregate doesn't compound.
- **`FIX-LOG.md`** ← `WHERE render_to='fix-log'`, append-only narrative history. Not auto-loaded into prompts; lives at repo root for operator browsing.
- **Pattern test docstrings** ← `WHERE render_to='pattern-test-docstring'`. Generated alongside the pattern test files; CLAUDE.md gets a one-line cross-reference (e.g., "Pattern P11 enforces exec.CommandContext propagation; see internal/audittools/audit_pattern_p11_exec_context_test.go") rather than the full narrative.
- **`docs/<domain>.md` files** ← `WHERE render_to LIKE 'per-domain-doc:%'`, target file extracted from the suffix. Examples: `per-domain-doc:docs/dashboard-conventions.md`, `per-domain-doc:docs/pr-flow-invariants.md`. Loaded by relevant agents only (those whose `agent_scope` mentions the domain) AND by developers reading the doc.
- **`SENATE.md` per-repo** ← `WHERE render_to='per-domain-doc:SENATE-<repo>.md'` (or equivalent — Senate's exact filing convention is D4 territory).
- **`bos/rules/*.yaml`, `isb/finders/*.yaml`** — domain-specific structured renderers; same pattern.

The CLAUDE.md hard cap (10 KB target post-Phase-1; 20 KB absolute upper bound) is enforced by:
1. The renderer's size-assertion (refuses to write a render exceeding the cap; emits `[RULE-RENDERER OVERFLOW]` operator mail).
2. The CLAUDE.md size-budget pre-commit hook (rejects commits where rendered CLAUDE.md exceeds the cap).
3. A pattern test (`TestPattern_PNN_ClaudeMdSize`) that fails CI if the file is over budget.

The categorization audit during Phase 1 bootstrap aggressively pushes content out of `render_to='claude-md-file'` and into `agent-prompt` / `fix-log` / `pattern-test-docstring` / `per-domain-doc:*`. Default render target during bootstrap is **NOT** `claude-md-file` — the auditor must affirmatively justify each rule that stays in the universal-load file.

Each rendered file carries a header:

```markdown
<!-- AUTO-GENERATED from FleetRules table. Do not edit by hand. -->
<!-- Last rendered: 2026-04-23 14:22:07 UTC -->
<!-- Source: engineering-corps promotion of experiment #42 -->
```

Auto-commit:

```
chore(rules): render FleetRules → CLAUDE.md (promotion via exp #42)
```

Manual edits to rendered files are rejected by a pre-commit hook. Operators who want to change a rule use the dashboard promotion flow (or direct-write escape hatch; see below).

### Operator direct-write escape hatch

For cases where operator judgment alone is sufficient (incident response, novel infrastructure decisions), a dashboard "direct-write rule" button inserts into `FleetRules` with `promoted_by_experiment_id=NULL` and `created_by='operator:<name>'`. Still goes through DB + render + commit. Skips only the experimental-evidence requirement.

### Bootstrap

One-time migration parses the existing `CLAUDE.md` into `FleetRules` rows, one per section heading. Each bootstrap row: `version=1`, `active_from=now()`, `promoted_by_experiment_id=NULL`, `created_by='bootstrap'`. From the migration forward, the DB is authoritative.

---

## Engineering Corps

Engineering Corps is its own multi-task claim-loop agent (same scheduling pattern as Diplomat — separate goroutine, separate queue, claims multiple task types).

### Claimable task types

| Type | Purpose |
|---|---|
| `ExperimentAuthor` | Turn a Librarian `PromotionProposal` hypothesis into a complete experiment YAML |
| `ExperimentMonitor` | Watch a running experiment's posterior, budget, duration; declare winner/null/inconclusive; emergency-stop on degradation |
| `PromotionAuthor` | On winner + confirm, assemble `PromotionProposals` row with full evidence trail |
| `DemotionAuthor` | On retention-report signal, assemble demotion `PromotionProposals` |
| `MetricAuthor` | When a hypothesis needs an unregistered metric, generate the metric SQL + test + manifest |
| `HoldoutMonitor` | Run holdout refresh lifecycle; detect model-deprecation threats; emit operator mail |

### Throttles

- `engineering_corps_daily_proposal_cap` — default 3. Exceeding queues proposals for next day. Librarian hypotheses never drop.
- `engineering_corps_author_retry_cap` — default 3. YAML that fails validation bounces back with a critic note, up to 3 times, then escalates.
- `engineering_corps_emergency_stop_min_runs` — default 20. Small samples cannot trigger emergency-stop.

### Decision logic using DB history

When Librarian surfaces a hypothesis, EC queries `ExperimentOutcomes` for prior attempts. Decision table:

| Prior state | Action |
|---|---|
| No prior experiment | Propose. |
| Prior winner, rule shipped, currently active | Skip — already live. |
| Prior winner, rule shipped, since demoted | Propose re-test with `baseline_state=demoted` + referenced prior outcome. |
| Prior null, fleet-state hash matches current | Skip — already answered under current conditions. |
| Prior null, fleet-state hash differs meaningfully | Propose re-test with `revisits: [prior_experiment_id]`. |
| Prior inconclusive (budget exhausted) | Propose with increased budget + `revisits:`. |
| Prior emergency-stop | Require operator confirmation before re-proposing. |

### Hand-off to promotion

EC never writes to `FleetRules` directly. Its terminal deliverable is a `PromotionProposals` row with the full evidence trail:

- `experiment_id` + `winner_treatment_id`
- Cell means JSON at termination
- Posterior probability at termination
- Confirm-phase outcome
- `fleet_state_hash_at_start` and `fleet_state_hash_at_end`
- Analysis framework version used
- Metric version used

Operator ratifies on the dashboard; ratification triggers the DB+render+commit atomic action. TTL on unratified proposals is 14 days; expiry archives the proposal and EC may re-propose if the signal persists.

### Emergency stop

If an experiment's data shows `P(treatment worse than control by at least min_practical_effect) > 0.9` after `min_runs_for_kill` (20) runs, EC stops the experiment immediately:

- New assignments refuse.
- In-flight runs complete and are scored.
- `ExperimentOutcomes.termination_reason='emergency_stop'`.
- Operator mail with `[EXPERIMENT EMERGENCY STOP]` subject + full evidence.

---

## Global Holdout (long-term)

### Lifecycle

Each holdout has four phases governed by `current_fraction(holdout, now)`:

```
current_fraction(h, t):
  if t < h.reference_date:                               return 0
  if t < h.reference_date + h.ramp_up_days:              return h.plateau_fraction * elapsed / ramp_up_days
  if h.fade_start_at is null or t < h.fade_start_at:     return h.plateau_fraction
  if t < h.fade_start_at + h.fade_days:                  return h.plateau_fraction * (1 - elapsed_since_fade / fade_days)
  return 0
```

Defaults: `ramp_up_days=7`, `plateau_fraction=0.02`, `fade_days=90`. Annual refresh: `fade_start_at` set 12 months after `reference_date`, retirement 3 months later.

### Overlapping successors

When a holdout enters fade, its successor is minted at full 2% from day 1:

```
baseline-2026  |  0 → 2%  |     2%     |  fade 2% → 0%  |     0% retired
                ramp       plateau       3-month fade
baseline-2027                               0 → 2%  |     2%     |  fade ...
                                            ramp     plateau
```

Peak holdout overhead is ~3% during the 3-month overlap; steady-state is 2%. Overlap enables calibration: both cohorts run against the same current fleet, letting analysts stitch year-over-year trends together.

### Assignment

Features hash themselves at creation against the combined holdout pool. First-match (oldest active holdout wins tiebreaker) assigns the feature to exactly one holdout. Holdout-assigned features bypass all experiment logic.

### Model deprecation

Models are the uniquely fragile treatment dimension — we don't control their availability. All other dimensions are content-snapshotted into `TreatmentSpecs` or referenced via immutable `FleetRules` rows.

`model-availability-watch` dog (daily):

1. For every model identifier in any active experiment or holdout, issue a minimal-cost availability probe.
2. Record `ModelAvailability`. Deprecation detection fires on deprecation-header responses or 404/permanent-failure.
3. On detection: emit `[HOLDOUT AT RISK]` or `[EXPERIMENT AT RISK]` operator mail with substitution options, named successor (if Anthropic announced one), and announced kill date.

**Operator response options:**

- **Substitute.** Pick successor model. Every substituted run records `model_substituted_from` / `model_substituted_to`. Holdout semantics become "reference date config except for this substitution."
- **Early retire.** Freeze enrollment (0-day fade). Existing data retained.
- **Freeze.** Same as early retire, different semantics label.

**Automatic fallback:** if operator doesn't act before announced kill date, auto-freeze (never auto-substitute). Auto-substitution would silently corrupt holdout semantics; freezing preserves all data and stops the bleed.

### Honest caveats

- Holdout freezes **config**, not code. Agent binary evolves underneath frozen config. `FleetStateSnapshots.agent_binary_git_sha` captures this; dashboard flags "N significant agent-code changes landed since baseline" so operators know they're comparing new-code-with-old-config vs. new-code-with-new-config, not old-code-with-old-config vs. new-code-with-new-config.
- Holdout only measures evolution **from its reference date forward**. Changes promoted before holdout creation are present in both cohorts. Create the first holdout as early as possible.
- Annual refresh loses granular year-over-year continuity. The 3-month overlap provides calibration but not a single continuous record. For multi-year trends, analysts join across retired holdouts using the calibration offsets.

---

## Dashboard and Operator Workflow

### Views

- **Experiments.** List of running, confirming, terminated experiments. Filter by status, subject agent, stakes tier, authoring agent. Per-experiment view shows live posterior (with `provisional` flag when driven by llm_judge scores), cell means, sample counts per cell, budget consumed, duration remaining.
- **Proposals queue.** `PromotionProposals` awaiting ratification. Shows evidence trail, proposed content diff, related experiment, TTL remaining.
- **Fleet progress.** Holdout vs current rolling metrics. Per-metric time series with confidence bands. Annotations for each promotion landing.
- **Metric registry.** List of metrics with versions, deprecation status, tests passing/failing.
- **Rule registry.** Current `FleetRules` active set, filterable by category and agent scope. History view per rule_key showing version progression.
- **Holdout lifecycle.** Active and retired holdouts with lifecycle phase indicators, current traffic_fraction, model-availability status for pinned models.

### Operator actions

- **Ratify promotion proposal.** One-click. Writes to `FleetRules` + triggers rule-renderer + auto-commits rendered markdown.
- **Reject promotion proposal.** One-click with required reason. Archives proposal.
- **Pre-approve experiment YAML.** Required for EC-authored experiments before they enter `ratified` → `running`.
- **Operator direct-write rule.** Escape hatch; writes to `FleetRules` without experimental evidence.
- **Manual override.** Disable/enable a rule. Default: auto-creates a micro-experiment (`alternating on/off assignment`) so the intervention becomes a learning opportunity. `--skip-experiment` flag opts out.
- **Declare winner early / close experiment.** Override EC's automatic decisions.
- **Substitute holdout model / retire holdout.** On deprecation mail.

### Security

All operator actions inherit the existing dashboard invariants: 127.0.0.1 bind, same-origin allow-list on mutations, 256 KB body cap, no wildcard CORS, CSP headers, `.textContent` rendering for any attacker-writable string. No changes to the dashboard security posture; new endpoints follow the existing `securityMiddleware` wrapping.

---

## Composition with Existing and Future Agents

### Librarian (evolved)

Librarian curates FleetMemory into per-repo / per-agent memory bundles and surfaces patterns that look like hypotheses. Its output to Engineering Corps is a `PromotionProposal` with `kind='promote'` and `origin='librarian'` — a *candidate* hypothesis, not a ratified proposal. EC's `ExperimentAuthor` task consumes it, writes the experiment YAML, and the experiment runs. Only post-confirm does it become a ratified `PromotionProposal` to the operator.

### Senate

Senate experiments follow the standard flow with Senate-specific scoring (Tier 1 advice-citation rate as primary, Tier 2 matched-pairs downstream outcomes as retention check). When a Senator memory rule is promoted via experiment, the rendered SENATE.md for that repo is updated.

### ISB / BoS

Both are deterministic-first gates. Their rules live in `FleetRules` under `category='isb'` and `category='bos'`. Experiments on their rules test "does enabling this rule reduce the failure class it targets without increasing false-positive blocks." Standard flow; scoring metrics are per-rule-class.

### Existing agents (Captain, Council, Medic, etc.)

No structural changes to their internals. They receive treatment-applied call descriptors from `treatments.Apply`; the rest of their logic is unchanged. Their Spawn loops remain the same; the one new requirement is that each LLM call must route its descriptor through `Apply` before invoking `claude.AskClaudeCLI`.

---

## Governance

### Budget enforcement

Three caps, evaluated at commit time (not assign time):

1. **Fleet hourly spend cap** (existing `SpendCapExceeded`). Trumps everything. E-stop halts experiments along with all other work.
2. **Experiment `budget_usd`** — soft. At `current_spend >= budget_usd`, the experiment enters `over_budget` status: no new assignments, in-flight runs finish. Operator can extend or let it settle.
3. **Experiment `hard_cap_usd`** — terminal. Default 1.5× budget. At `current_spend >= hard_cap_usd`, no further runs of any kind; operator must extend to continue.

Cost estimation uses P95 historical cost per agent × call (self-calibrating; no hardcoded pricing table).

### Pre-registration

Every experiment YAML must declare:

- `hypothesis_text` (required, human-readable)
- `min_practical_effect` (required, numeric)
- `stakes_tier` (required)
- `primary_metric` (exactly one; drives winner declaration)

Exploratory experiments are allowed with `exploratory: true`, but cannot declare winners or feed promotion proposals.

### Winner declaration authority

Only Engineering Corps (via `ExperimentMonitor`) or explicit operator override declares winners. Never automatic on raw threshold-cross alone: the confirm phase is the gate for `high` and `safety_critical` experiments, and emergency-stop is the only automatic terminal-decision path.

### Demotion authority

Demotion proposals emit from EC's `DemotionAuthor` task, triggered by:

- **Retention reports** (Tier 2 scoring) showing a shipped rule's downstream benefit has decayed.
- **Direct degradation signal** from a post-ship monitor experiment showing `P(regression > practical) > 0.8`.
- **Operator-flagged suspicion** with a required re-experiment.

Like promotions, demotions require operator ratification — no auto-demote.

### Proposal TTLs

Unratified `PromotionProposals` expire after 14 days and are archived. EC may re-propose if the underlying signal persists. This prevents proposal-queue rot.

---

## Failure Modes and Mitigations

### Mode 1 — Runaway proposal volume

**Mitigation.** `engineering_corps_daily_proposal_cap` (default 3). Queuing, not dropping.

### Mode 2 — Detected-degradation false positive

**Mitigation.** `min_runs_for_kill` (default 20) + threshold on `P(worse by practical)` not `P(worse by anything)`.

### Mode 3 — LLM-authored YAML that semantically measures the wrong thing

**Mitigation.** Two layers. (a) Structural validation on YAML commit — references must resolve. (b) Operator pre-approval gate — no EC-authored experiment runs until operator clicks approve.

### Mode 4 — Stale experiment result blocks legitimate re-test

**Mitigation.** `fleet_state_hash` comparison: if prior null experiment ran under meaningfully different fleet state, EC may re-propose. No fixed cool-down.

### Mode 5 — Experiment on an already-shipped rule

**Mitigation.** EC's DB-history decision logic recognizes this and either skips (rule active) or treats it as a revert experiment (rule demoted). Revert experiments require explicit operator confirmation and Level-3 paired-shadow execution.

### Mode 6 — Metric drift from schema changes

**Mitigation.** Every metric version's `_test.sql` runs against current schema on every PR. Breakage fails the PR. Schema invariant: additive-only (no column removals).

### Mode 7 — Model deprecation kills a holdout

**Mitigation.** Daily `model-availability-watch` + operator mail + never-auto-substitute fallback to freeze.

### Mode 8 — Two overlapping experiments on the same call

**Mitigation.** Factorial orthogonality invariant enforced at assignment time. Experiments sharing any dimension cannot overlap.

### Mode 9 — Shadow worktree orphans

**Mitigation.** Three cleanup layers: run-termination handler (fast path), `shadow-worktree-gc` dog (15-min sweep), daemon startup sweep (deletes >24h orphans).

### Mode 10 — Operator hand-edits a rendered markdown file

**Mitigation.** Pre-commit hook rejects manual edits to files with `AUTO-GENERATED` header. Operator must use dashboard flow.

### Mode 11 — Pre-PR-flow legacy repos

**Mitigation.** Experiments on agents that support legacy direct-merge flow continue to apply normally; the repo's `pr_flow_enabled` flag is orthogonal to treatment assignment. Ship-it semantics (direct merge vs. draft PR) are not experiment subjects in v1.

### Mode 12 — Confirm phase indefinitely deferred

**Mitigation.** Confirm phase wall-clock deadline (`confirm_phase_timeout_hours`, default 336 = 14 days). Timeout terminates the experiment as `inconclusive` and archives the candidate promotion.

---

## Shadow Worktrees (Level-3 paired mode)

### Directory layout

```
.force-worktrees/
  <repo>/
    <agent>-shadow-<exp_id>-<run_id>/
```

Distinct from production worktrees by the `-shadow-` component. Never collides.

### Shadow-mode execution

When `treatments.Apply` enrolls a call in paired-shadow:

1. Spawn a parallel goroutine that duplicates the call context.
2. Create shadow worktree (checkout at same commit as real worktree).
3. Apply shadow arm's treatment to the shadow descriptor.
4. Real arm runs normally.
5. Shadow arm's `git push` operations are rewritten to `shadow-exp-<exp_id>-run-<run_id>` branches.
6. Shadow arm's `gh` calls are classified per-call: reads (`gh pr view`, `gh pr checks`) are replayed from real arm's response cache; writes (`gh pr create`, `gh pr merge`) are suppressed (recorded but not dispatched).
7. Jenkins/CI triggers from shadow arm go to `/dev/null`.
8. Shadow arm's artifact (diff + commit message + tool-call trace) is scored.
9. On termination, shadow worktree + shadow branch + any queued shadow bookkeeping are cleaned up.

### gh stubbing

`gh.NewClientWithRunner` already provides an extension point for per-call classification. Shadow-aware runner adds a `shadow: bool` context flag; on shadow calls, it consults a per-run response cache populated by the real arm (keyed by normalized arguments).

### Scoring limitations

Shadow runs cannot score on downstream signals that require real CI or merge (because Jenkins doesn't run for them, and branches don't merge). Metrics scoring shadow runs must be pre-CI: diff quality, Council-parseable artifact, scope-violation count, LLM-judge scores on the artifact itself. Post-CI metrics are scored only on the real arm.

---

## Rollout Plan

The phasing reflects the consolidated decisions from concerns #1–#5 in the post-Code-Red roadmap discussions. Each phase has expanded scope vs the original v1 design; total wall-clock build time at autonomous-agent pace is ~1 week. Phases are sequential (no skipping); within a phase, sub-tracks that touch disjoint files may parallel-develop.

### Phase 1 — Foundations + Rule Audit

**Original scope:**
- Schema: Experiments, ExperimentTreatments, ExperimentMetrics, ExperimentRuns, ExperimentOutcomes, TreatmentSpecs, MetricVersions, AnalysisFrameworks, FleetStateSnapshots, FleetRules, PromotionProposals, GlobalHoldouts, ModelAvailability
- Inheritance columns on Features, Convoys, BountyBoard (`in_holdout`, `experiment_assignments_json`)
- `treatments.Apply` stub in log-only mode
- Metric registry directory + loader + fixture-test runner on daemon start
- `rule-renderer` dog + pre-commit hook rejecting hand-edits to auto-generated files
- Tests for every schema invariant + assembly query correctness + hash-bucket determinism

**Additions from concerns #1–#5:**

- **CLAUDE.md audit and refactor.** Categorize every section (stay-in-CLAUDE-md / move-to-FIX-LOG.md / move-to-per-agent-doc / collapse-to-Pattern-test-reference / delete). Bootstrap migration parses the audited content into FleetRules with appropriate `category` and `agent_scope`. Target: rendered CLAUDE.md ≤ 20 KB.
- **Per-agent rule injection at agent invocation.** Each agent's `claude -p` call gets `--append-system-prompt` content built from `SELECT content FROM FleetRules WHERE category='claude-md' AND (agent_scope='all' OR agent_scope='<agent>')`. Agents stop paying token cost on irrelevant rules.
- **CLAUDE.md size budget pre-commit hook.** Rejects commits growing rendered CLAUDE.md beyond 20 KB.
- **Pattern-test-as-spec discipline.** Every `category='claude-md'` FleetRule has an `enforced_by` field referencing a Pattern test ID OR an explicit `trust-only` tag. Insert reject if neither.
- **Verification spec schema additions** to `Convoys`: `verification_spec_json`, `experiment_assignments_json`, `critical` flag, `spec_history`.
- **Captain proposal schema additions** to `BountyBoard`: `proposed_action_json` (cited_ats[], cited_fleet_rules[], spec_link, classification_confidence, captain_reasoning, draft_amendment, alternative), `prompt_version`, `prior_review_outcomes_json`, `spawn_spec_link`, `spawn_classification_confidence`.
- **`ProposedFeatures` table** for Investigator's cross-convoy aggregation queue.
- **`AdversarialPairings` table** for tracking auto-execute layer adversarial-pair results.
- **`GoldenSetFixtures` and `GoldenSetEvaluations` tables** for golden-set evaluation framework.
- **`CalibrationAuditSamples` table** for weekly calibration-sample widget.
- **TaskHistory.prompt_version** column for cross-prompt-version analysis.

### Phase 2 — Holdout mode + single-treatment experiments

**Scope (unchanged):**
- Turn `treatments.Apply` on (non-log-only). Honors `in_holdout` flag and single-treatment experiments (no factorial yet)
- Mint `baseline-2026` holdout
- Analysis-framework YAML loader + Bayesian Beta-Binomial algorithm registration
- Dashboard views: Experiments list, single-experiment view, fleet-progress (holdout vs current)
- Integration tests running whole agent calls through the pipeline

### Phase 3 — Engineering Corps + Trust Metrics Infrastructure

**Original scope:**
- `SpawnEngineeringCorps` claim loop
- Six task types: ExperimentAuthor, ExperimentMonitor, PromotionAuthor, DemotionAuthor, MetricAuthor, HoldoutMonitor
- Librarian → EC handoff: Librarian emits `PromotionProposals` with `origin='librarian'` and `kind='candidate'`; EC's ExperimentAuthor picks them up
- Promotion-proposal ratification endpoint + dashboard one-click
- Automatic DB+render+commit on ratification

**Additions from concerns #2 (D), #4 (E), #5 (E + F):**

- **Cross-layer disagreement tracking.** EC computes per-pair disagreement rates: Captain → Council reject, Council → CI fail, ConvoyReview → astromech "can't fix," Senate concur → Chancellor declines, operator approve → revert-within-30d. Surface as dashboard panel.
- **Independent ground-truth tracking.** EC analysis layer joins `TaskHistory.prompt_version` with downstream-outcome metrics: per-Captain-prompt-version PR merge rate, per-Council-prompt-version CI pass rate, per-Medic-prompt-version convoy-completion rate, per-EC-promotion revert-within-90-days rate.
- **Distribution drift detection.** EC tracks per-Captain-prompt-version classification rates (tied/glue/unmapped/low_confidence) over rolling windows. Alerts on outliers (e.g., unmapped_rate < 1% over 50+ spawns).
- **Amendment-quality measurement infrastructure.** EC tracks `amendment_revert_rate_30d` and `decision_time_correlation_with_revert`. Infrastructure ships now; signal accumulates from day 1.

### Phase 4 — Factorial + orthogonal-overlap scheduler

**Scope (unchanged):**
- Factorial dimensions support
- Cell-weight stratified assignment
- Main-effects + 2-way interaction analysis
- Scheduler's orthogonal-dimension overlap invariant

### Phase 5 — Level-3 paired shadow + Adversarial Pairing + Golden-Set

**Original scope:**
- Shadow-aware `gh` runner with response cache
- Shadow worktree spawn/cleanup lifecycle
- Jenkins/CI suppression
- Shadow-worktree-gc dog + startup sweep
- Pre-CI scoring metrics for shadow-only signals
- Extend EC's `ExperimentMonitor` to orchestrate confirm phases

**Additions from concern #5 (revisited):**

- **Adversarial pairing for high-stakes auto-execute layers.**
  - **Council adversarial pair**: when Council approves a diff, parallel `Council-critic` prompt with opposite framing ("find quality issues") runs. Disagreement → operator review queue rather than auto-merge.
  - **Medic decision adversarial pair**: when Medic decides requeue/shard/cleanup/escalate, second prompt critiques the decision. Disagreement → operator surface.
  - **ConvoyReview fix-task drafting adversarial pair**: when ConvoyReview spawns a fix task, second prompt evaluates "would this fix actually close the gap?" Inconsistency → reject the spawn.
  - All disagreements written to `AdversarialPairings` table; surface to operator at sufficient volume threshold.

- **Golden-set evaluation framework.** Periodic dog (weekly) runs current Captain/Council/Medic prompts against curated input fixtures with known-correct outputs. Auto-curation: fixtures auto-derived from convoys that shipped cleanly without rework. Operator-curated negative examples optional. Accuracy regression below threshold triggers operator alert.

### Phase 6 — Operator UX + Verification + Trust Layers + Shakedown

**Original scope:**
- Manual-override auto-experiment flow
- Model-availability-watch dog + deprecation mail flow
- Holdout lifecycle dashboard (ramp / plateau / fade indicators)
- Metric registry dashboard
- First operator-authored experiment end-to-end as shakedown

**Additions from concerns #1, #2, #3, #4, #5:**

*Verification spec consumption (verification-spec discussion):*
- ConvoyReview at DraftPROpen evaluates `verification_spec_json` per convoy. Failures spawn fix tasks scoped to specific spec entries. Spec-was-wrong cases emit `[SPEC AMENDMENT PROPOSED]`; out-of-convoy work emits `[PROPOSED_FEATURE]` to Investigator.

*ConvoyReviewCycles atomic snapshots (concern #6):*
- Each ConvoyReview pass writes a `ConvoyReviewCycles` row at start with `spec_version_at_start` snapshot; row is immutable once written
- Spec amendments mid-cycle do NOT mutate the in-flight cycle's spec — operator-ratified amendments take effect at the NEXT cycle start
- `cycle_number` monotonic per convoy; UNIQUE(convoy_id, cycle_number) prevents racing inserts
- Cycle outcomes_json records pass/fail/inconclusive per AT-id; fix_tasks_spawned_json records what was spawned; amendments_proposed_json records LLM suggestions awaiting operator decision; amendments_ratified_during_cycle_json captures audit of amendments approved within this cycle's window
- Pattern asserts no `UPDATE ConvoyReviewCycles SET spec_version_at_start = ?` paths; once written, it's frozen
- Frozen-spec model: spec is written once at convoy plan time; subsequent cycles evaluate against the most-recently-ratified version; amendment ratification is the only path to spec mutation (LLMs propose, operator ratifies)

*Cross-convoy AT-id collisions (concern #8):*
- AT IDs are LOCALLY scoped within `Convoys.verification_spec_json` — `AT-005` in convoy 12 has zero relationship to `AT-005` in convoy 47
- Lookup invariant: every code path resolving an AT MUST use compound key `(convoy_id, at_id)`, never bare `at_id`
- UI labeling discipline: dashboard always renders AT references with convoy context (e.g., "Convoy #47 / AT-005") — never bare AT-id chips
- Pattern P20 (`TestPattern_P20_ATIdScopeIntegrity`) walks production code; rejects any `WHERE at_id = ?` query without a co-occurring `convoy_id` constraint
- Deferred to v2: a future fleet-wide AT namespace via FleetRules references; until then, top-level fleet-wide AT namespace is forbidden (prevents accidental ID collision via amendment)

*Spec deprecation (concern #9):*
- Operator wants to REMOVE an AT, not just add. Removal moves the entry from `verification_spec_json.ats[]` → `verification_spec_json.deprecated[]` with mandatory rationale + `removal_kind` ('mistake' | 'superseded' | 'satisfied' | 'out_of_scope') + optional `superseded_by` reference
- Spec_history_json append-only entry records the deprecation (`kind: 'deprecate'`, rationale, ratified_at, ratified_by_email)
- ConvoyReview skips deprecated ATs in active evaluation; lookups still resolve through `deprecated[]` so historical cycle outcomes referencing the AT keep their meaning
- In-flight fix-task check: removal endpoint queries `BountyBoard WHERE spawning_at_id = ? AND status NOT IN ('Completed','Cancelled','Failed')`; if non-empty, operator chooses Cancel-and-remove / Complete-then-remove / Cancel-removal
- Pending Captain proposal re-justification: proposals with `cited_ats` referencing the removed AT route through Captain re-justification flow before ratification UI re-surfaces them
- LLM-only ADD/MODIFY proposals; REMOVE is operator-UI-only (Pattern P21)

*PromotionProposals revert handling (concern #7):*
- Rejection is more than just "no" — operator picks `rejection_action`: `leave_as_is` / `clean_revert` (no dependents) / `cascade_revert` (revert this + cascade dependents) / `surgical_revert` (remove only the affected hunks; ConvoyReview validates safety) / `escalate` (operator can't decide; routes to a fresh proposal)
- `rejection_rationale` mandatory (≥ 20 chars) for every action other than `leave_as_is`
- `revert_task_id` references the spawned CodeEdit performing the revert; `BountyBoard.deferred_revert` flags rows scheduled to revert when their dependents complete (cascade-revert flow)
- ConvoyReview acts as semantic safety net: a surgical-revert spawn re-runs ConvoyReview to verify the revert didn't break sibling work; cycle outcome surfaces to operator before merge
- Re-filing path: `refiled_feature_id` records when a rejection re-files as a new feature (e.g., "this was the wrong implementation; re-file with different approach")

*Captain proposal validation (concern #1 + revisit):*
- Captain populates `proposed_action_json` for unmapped spawns
- Mechanical validation at emit-time: every cited AT resolves, every cited FleetRule resolves, prose `AT-NNN` references must appear in `cited_ats[]`. Rejected rulings retry with critic note.
- Source-of-truth display in operator UI: cited AT/FleetRule actual text rendered alongside Captain's `WhyCited` claim
- Captain reasoning LLM-judge step (Haiku): validates `captain_reasoning` consistency with cited evidence; "inconsistent" rejects ruling, "ambiguous" proceeds with operator-visible badge
- Pattern test P18 (`TestPattern_P18_CaptainProposalIntegrity`) enforces

*ConvoyReview cross-classification (concern #4 C, revisited):*
- ConvoyReview at DraftPROpen reads actual diff and Captain's per-spawn `spec_link`. Emits `[CLASSIFICATION_DISAGREEMENT]` per spawn where ConvoyReview's assessment differs from Captain's. Operator sees disagreements alongside unmapped-spawn batch.

*Two-track UI with stakes-tiered friction (concern #2 H):*
- Stakes-tier classification: low / medium / high
- Auto-escalation rules: any proposal touching CLAUDE.md/BoS/ISB/Senate rules → high; AT deprecation → high; cumulative similar > 5 in 30d → high; first-of-kind → high
- Tier-based UI: low (batch view, multi-select); medium (single-proposal view, explicit per-row click); high (modal with explicit "I have read the reasoning and cited evidence" affirmation checkbox before Approve enables)

*Counter-proposal forcing (concern #2 I):*
- High-tier rejection requires selection: "Should not happen at all" (text reason) / "Should happen differently" (operator drafts or prompts EC to draft alternative which becomes new proposal) / "Defer to investigation queue"

*Decision-distribution dashboard (concern #2 A):*
- Per-agent decision metrics: approve/reject/modify rates over rolling 7/30/90 day windows; decision-time distribution (median, p90); recent-decisions list with spot-check links

*Reject-rate baseline warning (concern #2 F):*
- SystemConfig.expected_reject_rate_min (default 0.05). Per-agent: actual reject rate over rolling 30d < threshold → dashboard banner. Operator can dismiss for 7 days; warning resurfaces.

*Weekly calibration sample (concern #2 B-lightweight):*
- Every Monday, dashboard surfaces N=7 (configurable) recently-ratified proposals from prior week
- Weighted selection: 30% fast-approved high-stakes / 30% high-approve-rate-agent proposals / 40% random within rest
- Operator interactions: Confirm / Re-review in detail / Should have been rejected (generates `[RE_EVALUATE]` task; doesn't auto-revert) / Snooze
- CalibrationAuditSamples table records sample selection, operator action, rationale on disagreement

*Captain classification confidence (concern #4 H):*
- Captain emits `classification_confidence: high | medium | low` alongside `spec_link`
- Routing: high+glue/tied → don't surface for review; medium or unmapped or any low confidence → surface in batched triage

*Operator spot-check on classifications (concern #4 F):*
- Triage view exposes "[Spot-check N]" expandable for tied/glue spawns; operator can mark any as "should have been unmapped" → moves to amendment review; "should have been a different classification" → records disagreement signal feeding distribution-drift metrics

*Return UX + sleep handling (concern #3, simplified for MacBook reality):*
- Per-convoy `critical` flag for triage prioritization
- Return-UX batched triage view: critical convoys → high-stakes amendments → medium-stakes amendments grouped by similarity → ready-to-ship convoys; similarity-grouping enables batch-approve
- `force install-sleep-hook` command: one-time setup wiring graceful shutdown to macOS sleep events via sleepwatcher
- Heartbeat-based sleep detection: 30s ticks; gap > 90s infers sleep; on detected wake, run reconciliation sweep (D2 T1-0), refresh trailing-window calculations, surface dashboard event
- Dashboard "since you woke up" widget: banner with sleep duration + tasks failed/retried/escalated/clean-verified counts

*Investigator expansion + ProposedFeatures queue management (concern #4 cross-cutting + concern #10):*
- Investigator subscribes to event streams: astromech `follow_up_observations`, Captain `out_of_convoy_observations`, Council `quality_observations`, ConvoyReview `[PROPOSED_FEATURE]` emissions, Senate plan-time emissions (post-D4)
- Canonical-fingerprint dedup at every proposer's insert ingress (sha256 over source + topic + sorted code paths + sorted AT refs + sorted FleetRule refs); same fingerprint = update existing row's occurrence_count + last_seen_at + evidence_history_json append
- Suppression check at insert ingress against `ProposedFeatureSuppressions`; suppressed fingerprints = no-op insert (logged); operator sets duration with mandatory rationale (≥ 20 chars), max 1 year
- Value/complexity scoring at proposal time: proposer LLM emits `value_score` + `complexity_score` (low/medium/high) + one-line rationales; operator can override with audit trail in `ProposedFeatureScoreOverrides`
- Composite priority: `value_points / complexity_points` (low=1, med=3, high=9); UI badges for Quick win (H/L), Big swing (H/H), Don't bother (L/H); default sort: priority desc, occurrence_count desc, created_at asc
- Promotion-to-active-interest: operator clicks Promote → modal requires either an estimated convoy date OR self-deadline; deadline scales with complexity (H/L = 1wk, mid = 2wk, H/H = 4wk + explicit major-investment checkbox)
- Operator triage UX: per-row decisions (promote / spawn new convoy / merge into existing / suppress / archive / merge-with-similar)
- Categorized dashboard tabs: Active (created last 7d OR promoted) / Recurring (occurrence_count ≥ 3) / Suppressed / Archived; filters by source, repo, AT cluster, severity, value floor, complexity ceiling
- Score-aware auto-archive (`proposed-features-housekeeping` daily dog): low-value > 30d unactioned → archive; medium-value > 60d unactioned → archive; H-complexity + L-value at 14d (the don't-bother fast track); high-value never auto-archives; Captain/operator/ConvoyReview-sourced rows never auto-archive (active fleet decisions); soft-archive (sets `archived_at`); operator can un-archive
- Capacity-imbalance dashboard widget: per-source filed-vs-engaged ratio surfaces signal-to-noise mismatch without dropping anything

*Trust property documentation (concern #5 G):*
- CLAUDE.md "Recursive LLM trust" entry (brief; full architectural detail in `docs/architecture/llm-trust-property.md`)
- Documents the property; lists auto-execute layers explicitly; names mitigations; honestly disclosed limits

*Shakedown (original + extended):*
- First operator-authored experiment end-to-end with full round-trip
- PLUS: convoy verification-spec round-trip on shakedown convoy (specs evaluated, fix tasks spawned and resolved if any, amendments surfaced and ratified or dismissed appropriately)
- PLUS: ProposedFeatures dedup demonstrated with at least 2 observations across 2 convoys aggregating to 1 entry

### Phase 0 (prerequisite)

- Librarian evolution (per [next-gen-agents.md](../next-gen-agents.md)). EC cannot function without curated memory as a hypothesis source.

---

## Composition with Promotion Pipeline

The end-to-end loop:

```
Fleet signals
  ├─ agent rejections (Captain, Council, ConvoyReview)
  ├─ escalations (incident traces)
  ├─ operator corrections (direct-write rules, manual overrides)
  └─ Senate dissents / Librarian patterns
        │
        ▼
Librarian curates → emits candidate PromotionProposal (kind='candidate', origin='librarian')
        │
        ▼
Engineering Corps / ExperimentAuthor
  ├─ Queries ExperimentOutcomes for prior state
  ├─ (if novel metric needed) MetricAuthor proposes metric; operator ratifies
  ├─ Generates experiment YAML
  └─ Status: authored
        │
        ▼
Operator pre-approves YAML → Status: ratified → Status: running
        │
        ▼
Runs accumulate (via treatments.Apply) → ExperimentRuns rows
        │
        ▼
Engineering Corps / ExperimentMonitor
  ├─ Watches posterior, budget, duration
  ├─ On threshold-cross: → Status: confirming (confirm phase spawned)
  └─ On confirm-phase success: → Status: terminated + ExperimentOutcomes
        │
        ▼
Engineering Corps / PromotionAuthor
  └─ Emits ratified PromotionProposal (kind='promote' | 'demote')
        │
        ▼
Operator ratifies on dashboard
  └─ Atomic: FleetRules insert + rule-renderer commit
        │
        ▼
Fleet evolves. Baseline holdout measures aggregate improvement.
```

Every arrow in this graph is a DB row transition. Every decision point is queryable. Every promotion carries the full experiment evidence in git history. Every historical moment is reproducible.

---

## Open Questions

These are intentionally deferred to v2 or later, listed so they don't drift.

1. **Bandit-style adaptive allocation.** Fixed cell weights are easier to analyze but less sample-efficient. Thompson sampling or UCB allocation could dramatically speed winner declaration for low-signal experiments.
2. **Informed priors from prior outcomes.** Currently experiments default to uniform priors. A controlled mechanism for "inherit prior from related prior experiment" would speed convergence when genuine prior evidence exists, at the cost of confirmation-bias risk.
3. **Cross-fleet experiments.** If multiple fleet instances ever coexist, experiments that federate results would amortize sample cost.
4. **Meta-experiments on analysis frameworks.** Treated in this doc as "Layer 2 is versioned, so framework changes can themselves be tested by long-term holdout comparisons." A more rigorous mechanism for "experiment on the declare-winner threshold" is deferred.
5. **Agent-routing (Flavor B) treatment dimension.** Experimenting on which agent handles the work. Deferred because confounding is severe and scoring requires new machinery.
6. **3-way+ interactions as default.** Currently opt-in; sample requirements too large for routine use.
7. **Tool-using agents in holdout mode (Level 1).** Tool-using agents run paired-shadow in v1 for experiments. Extending holdout-mode randomization to them without shadow overhead is a v2 optimization.
8. **Shadow-mode expansion to non-LLM side effects.** Jenkins suppression is the v1 boundary; future shadow-mode could also intercept Slack / PagerDuty / webhook calls for agents that emit those.
