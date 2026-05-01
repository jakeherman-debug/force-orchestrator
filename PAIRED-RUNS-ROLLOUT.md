# PAIRED-RUNS-ROLLOUT.md

D3 paired-runs rollout — green-tests sign-off log.

This file is the authoritative repo-root sign-off record for the
`docs/paired-runs.md` § "Rollout Plan" phases 1–6 per **D3 exit
criterion 17** (`docs/roadmap.md` line 1278). Each phase has a section
below; each section records the build / test / render-rules gates and
the merge SHAs that close the phase. New rollout iterations append at
the bottom.

The narrative trace for each phase lives in `docs/closures/DELIVERABLE-3-CLOSURE.md`
(per-phase sections + addendums). This document summarises the
gate-status pass/fail evidence so a strict verifier can confirm each
phase actually closed before its successor opened (D3 is strictly
sequential per `docs/roadmap.md` line 984).

---

## Per-phase status

| Phase | Description | Status | First → last commit | Closure addendum |
|---|---|---|---|---|
| 1   | Foundations + Rule Audit                           | CLOSED 2026-04-29 | `908c51d` → `e86a282` (14 commits)                | `DELIVERABLE-3-CLOSURE.md` § "Phase 1 narrative" |
| 2   | Holdout + single-treatment experiments             | CLOSED 2026-04-29 | `20e0329` → `e1cdc83` (5 commits)                 | `DELIVERABLE-3-CLOSURE.md` § "Phase 2 — …" |
| 3   | Engineering Corps + Trust Metrics Infrastructure   | CLOSED 2026-04-30 | `208fafd` → `338b144` (22 commits / 5 merges)     | `DELIVERABLE-3-CLOSURE.md` § "Phase 3 — …" |
| 4   | Factorial + orthogonal-overlap scheduler           | CLOSED 2026-04-30 | `54e4804` (closure addendum)                      | `DELIVERABLE-3-CLOSURE.md` § "Phase 4 — …" |
| 5   | Level-3 paired shadow + Adversarial Pairing + Golden-Set | CLOSED 2026-04-30 | merged via `--no-ff` (Phase 5 commit train) | `DELIVERABLE-3-CLOSURE.md` § "Phase 5 — …" |
| 6A  | Dashboard scaffolding + Pulse + Briefing           | CLOSED 2026-04-30 | tier-0 → tier-4-final (11 merges)                 | `DELIVERABLE-3-CLOSURE.md` § "Phase 6A closure addendum" |
| 6B  | Reflection + Drill + verification spec consumption + shakedown | CLOSED 2026-04-30 | tier-1 → tier-5 (7 merges) + closure | `DELIVERABLE-3-CLOSURE.md` § "Phase 6B closure addendum" |
| polish-iter1 | Polish-pass: silent-error + P31/P32 burn-down | CLOSED 2026-04-30 | `300bd0c`, `d5b8c1a`, `ba737b3`, `3f66abf`        | `DELIVERABLE-3-CLOSURE.md` § "Polish-pass closure addendum" |
| polish-iter2 | Polish-pass iter2: live Haiku, P25 AST, SPA wiring, P27/P32 burn-down | CLOSED 2026-04-30 | `8012202`, `c5e2ab3`, `cb550a4`, `303a114`, `f3c5564`, `c05b0ab` | `DELIVERABLE-3-CLOSURE.md` § "Polish-pass iteration 2 closure" |
| fix-loop-1   | Strict-verifier fix loop — α / β / γ / δ slices  | OPEN              | (in progress, fix-loop-1/alpha first)            | this file (final block) + closure addendum on merge |

`OPEN` rows have no rollout sign-off until merge; `CLOSED` rows have
all three gates green at the recorded SHA(s).

---

## Phase 1 — Foundations + Rule Audit

**Scope** — schema substrate (D3 tables: Experiments, Treatments,
Metrics, Runs, Outcomes, TreatmentSpecs, MetricVersions,
AnalysisFrameworks, FleetStateSnapshots, GlobalHoldouts, FleetRules,
PromotionProposals, ProposedFeatures, AdversarialPairings,
GoldenSetFixtures, GoldenSetEvaluations, CalibrationAuditSamples,
ConvoyReviewCycles, ModelAvailability, TreatmentApplyLog),
inheritance columns on Features / Convoys / BountyBoard,
`treatments.Apply` log-only stub, FleetRules audit slice +
`render_to='claude-md-file'` filter rendering CLAUDE.md from DB,
per-agent rule injection skeleton.

**Gates at close** (from `DELIVERABLE-3-CLOSURE.md` § "Heavy
validation (closure-time)"):

- `make build` (with `-tags sqlite_fts5`): PASS
- `make test`: PASS — 26 packages green; D3 P1 specific tests
  (TestSchemaParity, TestBootstrapFleetRules_Idempotent,
  TestRenderClaudeMdFile, TestPattern_P17_ClaudeMdSize) all green
- `./force render-rules --check`: PASS — exit 0; rendered CLAUDE.md
  6616 bytes (well under 10 KB Phase-1 target / 20 KB hard cap)

**Anti-cheat self-check** (excerpt — full table in closure):
- Schema parity re-runs after every commit: PASS
- Bootstrap is idempotent: PASS — verified by
  `TestBootstrapFleetRules_Idempotent`
- `render_to='claude-md-file'` count is plausible: PASS — 11 entries
  (cap 15)

**Merge SHAs** — `908c51d` (initial) → `e86a282` (close); 14 commits
on the Phase-1 commit train. No external pushes. Forward integration
to Phase 2 documented in closure § "Forward integration to Phase 2".

---

## Phase 2 — Holdout + single-treatment experiments

**Scope** — `treatments.Apply` flipped from log-only to live mode;
`baseline-2026` GlobalHoldouts row minted; 2% holdout assignment via
deterministic SHA-256 hash bucketing on `(unit_id, holdout_id)`;
single-treatment experiment lifecycle (Author → Pre-approve → Run →
Terminate → Outcome → PromotionProposal); Bayesian Beta-Binomial
analysis-framework registration; D3 P2 dashboard tab (read-only,
intentionally minimal — Phase 6 absorbs).

**Gates at close**:

- `make build`: PASS
- `make test`: PASS — agent regression matrix byte-identical for
  non-experiment + non-holdout units (Phase 2 invariant);
  TestApply_NotInHoldout_NoActiveExperiments_PassesThrough,
  TestApply_HoldoutMember_SkipsExperimentEnrollment,
  TestApply_SingleActiveExperiment_AppliesAssignedTreatment,
  TestIsInHoldout_DeterministicAssignment (5× repeat per unit),
  TestRatify_RequiresOperatorRoute_AuditLogged all green
- `./force render-rules --check`: PASS — Phase 2 added no
  `claude-md-file`-class FleetRules

**Anti-cheat self-check**:
- Live flip is byte-identical for non-experiment + non-holdout
  units: PASS
- Holdout assignment is deterministic: PASS
- Experiment ratification is operator-routed + audit-logged: PASS
- All new mutators return `error`: PASS

**Merge SHAs** — `20e0329` (initial Phase-2 work) → `e1cdc83`
(close). 5 commits.

---

## Phase 3 — Engineering Corps + Trust Metrics Infrastructure

**Scope** — `SpawnEngineeringCorps` claim loop; six EC task types
(ExperimentAuthor, ExperimentMonitor, PromotionAuthor, DemotionAuthor,
MetricAuthor, HoldoutMonitor) each implementing the `taskHandler`
contract; Librarian → EC handoff via `PromotionProposals`
(`origin='librarian'`, `kind='candidate'`); promotion-proposal
ratification endpoint with operator-routing; cross-layer
disagreement tracking + DisagreementPairs persistence;
TaskHistory.prompt_version live; distribution-drift detection
infrastructure.

**Gates at close**:

- `make build`: PASS
- `make test`: PASS — 26 packages; D3 P3 specific tests
  (TestEngineeringCorps_*, TestLibrarianToECHandoff,
  TestRatificationEndpoint, TestEngineeringCorpsDispatcher_*) all
  green; pattern test inventory (P1, P1.1, P3, P7, P8, P10, P11,
  P12, P13, P15, P16, P17, P18) all green
- `./force render-rules --check`: PASS — exit 0; P18 green

**Anti-cheat self-check**:
- Capability profile sourced from YAML at every Claude CLI call site:
  PASS — `engineering-corps.yaml` + `LoadProfile` at every handler
- Cross-agent dependencies route through `Client` interfaces: PASS —
  Pattern P16 green; `EngineeringCorpsConfig` holds
  `librarian.Client` + `metrics.Client`
- All new mutators return `error`: PASS — `EmitCandidate`,
  `PersistDisagreementRates`, `RegisterGroupedMetric`, all P3
  handlers
- Dispatcher fail-closed on unknown task type: PASS
- Topology preserved (`--no-ff` + visible merge commits): PASS — five
  visible merges (skeleton, task-types, handoff-ratify,
  disagreement-metrics, shakedown)

**Merge SHAs** — `208fafd` → `338b144`; 22 commits across 5
`--no-ff` merge branches (`deliverable/3/phase-3-skeleton`,
`-task-types`, `-handoff-ratify`, `-disagreement-metrics`,
`-shakedown`).

---

## Phase 4 — Factorial + orthogonal-overlap scheduler

**Scope** — multi-arm × multi-factor experimentation surface;
factorial dimensions support; cell-weight stratified assignment;
orthogonal-dimension overlap invariant on the scheduler; main-effects
+ 2-way interaction analysis; deterministic factorial enrollment.

**Gates at close** (per `DELIVERABLE-3-CLOSURE.md` § "Phase 4 —
Factorial + orthogonal-overlap scheduler — CLOSED 2026-04-30"):

- `make build`: PASS — `go build -tags sqlite_fts5 -o force ./cmd/force/`
- `make test`: PASS — full suite green after each merge across 26
  packages; pattern test inventory all green; new factorial /
  orthogonal-scheduler shakedowns
  (`TestEnrollFactorialUnit_Deterministic`,
  `TestSelectOrthogonal_DeterministicAcrossRuns`) green
- `./force render-rules --check`: PASS — exit 0, no drift

**Anti-cheat self-check**:
- Single-treatment path UNCHANGED — TestLifecycle_EndToEnd_ShakedownExperiment
  still PASS
- Math fixtures non-tautological — hand-computed posterior means
  match implementation: PASS
- All new mutators return error: PASS
- Determinism: same unit + same factorial experiment → same cell:
  PASS
- Naive "enroll in every match" code path removed (no parallel old/
  new behavior): PASS — `loadActiveExperiments` deleted
- Six `--no-ff` merges preserve topology: PASS

**Merge SHAs** — orchestrator + 3 parallel sub-agents pattern; six
`--no-ff` merges. Closure SHA: `54e4804` (closure addendum); merge
verification at `6ef1aa5`.

---

## Phase 5 — Level-3 paired shadow + Adversarial Pairing + Golden-Set

**Scope** — shadow-aware `gh` runner with response cache; shadow
worktree spawn/cleanup lifecycle (`.force-shadow-worktrees/` distinct
from production `.force-worktrees/`); Jenkins/CI suppression on
shadow runs; shadow-worktree-gc dog; pre-CI scoring metrics for
shadow-only signals; ExperimentMonitor confirm-phase orchestration;
adversarial pairing for high-stakes auto-execute layers (Council,
Medic, ConvoyReview) with critic profiles + DisagreementPairings
write-time enforcement of distinct prompt versions; golden-set
evaluation framework with auto-curation + operator-curated negatives.

**Gates at close**:

- `make build`: PASS — `go build -tags sqlite_fts5 -o force ./cmd/force/`
- `make test`: PASS — full suite green after each merge;
  TestSchemaParity, P1, P1.1, P3, P7, P8, P10, P11, P12, P13, P15,
  P16, P17, P18 all green; new shadow / adversarial / golden_set
  tests all green; shakedown tests (3 P5-specific + 1 negative-space)
  all PASS
- `./force render-rules --check`: PASS — no drift

**Anti-cheat self-check**:
- Shadow worktrees use a distinct `.force-shadow-worktrees/` prefix
  from production: PASS
- Shadow-mode gh writes are recorded but NOT dispatched to real gh
  binary: PASS — `TestShakedown_ShadowExperimentToTermination`
  confirmed delegate stub never saw `pr create`
- Shadow-mode pushes rewrite to local-only refspec
  (`shadow-exp-<exp>-run-<run>`): PASS
- Critic and primary prompt-version tags MUST differ: PASS —
  `ErrIdenticalPromptVersions` enforced on three failure modes
- Critic uses Pattern-P13-compliant capability profile (separate
  `*-critic.yaml`): PASS
- Auto-curated fixtures pass tautology guard: PASS
- Auto-curation idempotent on `(agent, input)`: PASS
- Operator-curated negatives kept as separate provenance class:
  PASS

**Merge SHAs** — Phase 5 commit train (multi-merge sequence
documented in closure addendum). All `--no-ff`; topology preserved.

---

## Phase 6A — Dashboard scaffolding + Pulse + Briefing

**Scope** — three-surface IA (Pulse / Briefing / Reflection
placeholder); 14 dashboard surfaces (heartbeat, keyboard shortcuts +
`?` overlay, notification budgets + helper, OperatorSessionState
resume, trust dials per agent, live narrative renderer, Pulse fleet
panel snapshot, "while you were away" cinematic on detected sleep
wake, conversational Briefing with Haiku-rendered prose synthesis,
counter-proposal forcing on high-stakes rejection, prior-similar-
decisions context, cooldown scheduler for high-stakes auto-execute,
operator attention tags, CLI parity audit + fill).

**Gates at close**:

- `make build`: PASS
- `make test`: PASS — `TestShakedown_P6A` exercises 12 sub-cases
  against in-memory holocron in <1s using deterministic synthesis
- `./force render-rules --check`: PASS

**Anti-cheat self-check**:
- Pattern tests added: P25 (CLI parity), P26 (keyboard shortcut
  consistency), P27 (notification budget routing — with backlog),
  P28 (NarrativeRenders single-writer + prompt-in-code), P29
  (briefing prose cites real evidence), P30 (cooldown scheduler API
  contract) — all green at close
- 5-minute heartbeat gap detected as sleep + cinematic builds: PASS
- CLI-parity decide sets the same DB state as the dashboard click:
  PASS — TestShakedown_P6A's parity subcase

**Merge SHAs** — 11 `--no-ff` merges to main: tier-0 + tier-1 (5) +
tier-2 (5) + tier-3 (combined) + tier-4-final. Final closure +
shakedown commits ending at `48411d0` (Merge `phase-6a-final`).

---

## Phase 6B — Reflection + Drill + verification spec consumption + shakedown

**Scope** — diagnostic substrate (LLMCallTranscripts capture wrapper
+ Pattern P31; GitOperationLog at internal/git helpers + Pattern
P32; transcript archival housekeeping dog); Drill diagnostic surface
(convoy / task / event views with filtering + free-text FTS5
search; replay mode purely-diagnostic; operator annotations with
flag taxonomy); Ask `/` shortcut with read-only DB-query tools;
Reflection (calibration scoreboard + fleet learning panel + 5-min
retro generator).

**Gates at close**:

- `make build`: PASS
- `make test`: PASS — all packages green; `TestShakedown_P6B`
  exercises 10 sub-cases against in-memory holocron in <2s; pattern
  test inventory (P1..P32) all green
- `./force render-rules --check`: PASS

**Anti-cheat self-check**:
- Pattern P31 (every Claude CLI call site routes through the
  transcript wrapper, with backlog allowlist): PASS
- Pattern P32 (every direct git/gh exec routes through the
  internal/git wrapper): PASS — backlog allowlist documents the
  pre-6B sites for migration
- Pattern P-Replay (`replay.go` contains no UPDATE/DELETE; only
  ReplayResults + replay's own LLMCallTranscripts INSERT): PASS
- Pattern P-AnnotationsOperatorOnly (non-operator paths can't write
  to OperatorEventAnnotations): PASS
- Pattern P-AskNoWriteTools (`ask_handler.go` contains no
  INSERT/UPDATE/DELETE, no reach into store mutators): PASS
- CLI parity for new endpoints: PASS — `force learning {refresh,show}`,
  `force annotate <kind> <ref> <flag> <text>`,
  `force replay <kind> <id>`, `force ask <question>`,
  `force retro {generate,save}`

**Merge SHAs** — 7 `--no-ff` merges at minimum (transcripts, git-log,
reflection-learning, tier2 drill, tier3 fts5 search, tier4 replay+
annotations, tier5 ask+calibration+retro) plus shakedown + closure
addendum. End: `aa91eaf` (Merge `phase-6b-closure`).

---

## polish-iter1 — Polish-pass: silent-error + P31/P32 burn-down

**Scope** — A2 + A3 silent-error propagation fixes; per-bucket
calibration accuracy; B3 P31 LLM-transcripts backlog 21→2; B4 P32
git-ops backlog 17→11 (6 of 9 files migrated); D1 closure addendum.

**Gates at close**:

- `make build`: PASS (exit 0)
- `make test`: PASS — 0 failures across all 28 packages
- `./force render-rules --check`: not re-run in this addendum (the
  polish pass touched no FleetRules rows; CLAUDE.md unchanged)
- Pattern test inventory: P1..P32 all green

**Honest deferrals (visible to strict verifier, recorded at iter1
close)**: A1 (live Haiku in 7 renderers), B1 (P25 regex→AST), B2 (P27
emit-site backlog 32 entries), B4 (P32 remaining 7 entries), C1 (SPA
wiring), C2 (replay structured-output diff). Iter2 closes all six.

**Merge SHAs** — `300bd0c` (`polish/tier-a-errors`),
`d5b8c1a` (`polish/tier-b-p31`), `ba737b3` (`polish/tier-b-p32`),
`3f66abf` (`polish/tier-d-closure`). All `--no-ff`. Wall-clock
~2h25m.

---

## polish-iter2 — Polish-pass iter2: live Haiku + SPA wiring + remaining burn-down

**Scope** — closes all 6 iter1-deferred items: A1 live Haiku in 7
renderers (env-flag gated; deterministic synth fallback on error);
B1 Pattern P25 regex→AST upgrade; B2 P27 emit-site backlog 32→4 (4
remaining are legitimate exemptions, not backlog); B4r P32 backlog
11→5 (with regression-protected astromech.go deferral pending
LogAndRun WaitDelay); C1 SPA wiring for P6B endpoints in Reflection
surface; C2 replay structured-output diff.

**Gates at close**:

- `make build`: PASS (exit 0)
- `make test`: PASS — all packages green
- `./force render-rules --check`: clean
- Pattern test inventory: P1..P32 all green; P25 now AST-based,
  P27 backlog 4 entries (legitimate exemptions only), P32 backlog
  6 entries (5 wrapper-self + astromech.go pending)

**Honest deferrals at iter2 close**: EMPTY at the item level. ONE
sub-item documented in the closure rather than hidden — astromech.go
P32 migration deferred with a regression-protected rationale
(LogAndRun's CombinedOutput-based shape blocks subprocess stdio
pipe closure, breaks ctx-cancel propagation in fix #8e/#8f e-stop
integration tests).

**Merge SHAs** — `8012202` (`tier-alpha-haiku`),
`c5e2ab3` (`tier-beta-p25`), `cb550a4` (`tier-beta-p27`),
`303a114` (`tier-gamma-spa`), `f3c5564` (`tier-beta-p32`),
`c05b0ab` (`tier-delta-closure`). Plus post-merge fix-ups
`693b741`, `d9773e1`. Wall-clock ~2h45m.

---

## fix-loop-1 — Strict-verifier fix loop (in progress)

**Why this iteration exists** — strict verifier returned NO GO with
12 unmet roadmap exit criteria after polish-iter2. fix-loop-1 closes
them via four parallel slices (α / β / γ / δ) on disjoint files.

### Slice α — pattern tests, rollout doc, install-sleep-hook CLI

**Scope** — Pattern tests P20 / P21 / P22 / P23 / P24 (each scaffold-
pattern modeled after P13 / P16 / P25 / P31 / P32); P5 + P19
intentionally unallocated (no roadmap reference); this `PAIRED-RUNS-
ROLLOUT.md` (D3 exit 17); `force install-sleep-hook` CLI (D3 exit
13).

**Pattern tests added** (D3 fix-loop-1 α1):
- `internal/audittools/audit_pattern_p20_at_id_scope_integrity_test.go`
  — `TestPattern_P20_ATIdScopeIntegrity` (D3 exit 14c, line 1182).
  Walks production SQL string literals; rejects bare `at_id =`
  predicates without a co-occurring `convoy_id` constraint. Empty
  allowlist; today zero offenders.
- `internal/audittools/audit_pattern_p21_at_removal_operator_only_test.go`
  — `TestPattern_P21_ATRemovalIsOperatorOnly` (D3 exit 14d, line
  1187, 1303). Walks proposer prompts + `verification_spec_json.deprecated[]`
  write paths; rejects LLM-driven removal intents on AT references.
  Empty allowlist.
- `internal/audittools/audit_pattern_p22_fingerprint_determinism_test.go`
  — `TestPattern_P22_FingerprintDeterminism` (D3 anti-cheat line
  1299). Behavioral test; passes with "scaffold pending" log until
  slice β wires the production fingerprint helper, then enforces
  byte-equal output for identical canonical inputs (incl. order-
  insensitivity for code_paths / at_refs / fleetrule_refs).
- `internal/audittools/audit_pattern_p23_proposer_write_discipline_test.go`
  — `TestPattern_P23_ProposerWriteDiscipline` (D3 anti-cheat line
  1300). Walks the canonical proposer files (Investigator, Captain,
  ConvoyReview, EC experiment_author / metric_author / promotion_author);
  rejects archive-state and suppression-table writes from proposer
  code paths.
- `internal/audittools/audit_pattern_p24_score_distribution_monitor_test.go`
  — `TestPattern_P24_ScoreDistributionMonitor` (D3 anti-cheat line
  1301). Behavioral test over a fake fixture; asserts the >70%
  single-bucket skew threshold logic with N≥5 floor and strict-
  greater boundary.

**P5 / P19 — intentionally unallocated.** Neither is referenced
anywhere in `docs/roadmap.md` (no exit criterion, no anti-cheat
directive, no architectural-invariant text). The slice α closure
records them as numbering gaps, NOT as authored-but-passing tests.
Future deliverables may claim those slot numbers; doing so requires
a roadmap entry first.

**install-sleep-hook CLI** (D3 fix-loop-1 α3):
- `cmd/force/sleep_hook_cmd.go` — `cmdInstallSleepHook(ctx, db, args)`
  writes `~/.sleep` + `~/.wakeup` scripts on darwin via sleepwatcher
  integration; idempotent (force-owned marker line); preserves
  operator-authored scripts unless `--force`. Linux / unsupported OS
  branches print informational message and exit 0 (the daemon's
  heartbeat-based detection in `internal/agents/cinematic.go:124`
  `DetectSleepStartedAt` works on any platform; the OS hook is the
  latency-closing integration point).
- `cmd/force/sleep_hook_cmd_test.go` — 11 test cases covering
  happy-path, idempotence, operator-authored protection, --force
  overwrite, --check, --uninstall, --uninstall preserves operator-
  authored, missing-sleepwatcher (with and without --force), linux
  branch, unsupported OS, --help, unknown flag.
- `cmd/force/main.go` + `cmd/force/print.go` — wire `install-sleep-hook`
  case into `main` switch + add to `printUsage()` under "System
  integration".

**Gates at slice α merge**:

- `make build`: (recorded at merge time)
- `make test`: (recorded at merge time)
- `./force render-rules --check`: (recorded at merge time)

**Merge SHA**: (recorded post-merge in this section.)

### Slice β / γ / δ

Tracked separately; each slice's closure records its own gates +
merge SHA on completion. fix-loop-1 status flips to CLOSED only after
all four slices land green.

---
