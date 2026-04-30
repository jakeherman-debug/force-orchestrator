# DELIVERABLE-3-CLOSURE.md — Paired Runs + Engineering Corps + Global Holdout (🟡 PARTIAL)

**Date:** 2026-04-29
**Operator:** jake.herman@upstart.com
**Net verdict:** 🟡 PARTIAL — Phases 1-3 CLOSED; Phases 4–6 OPEN

D3 uses a partial-closure pattern (per D1's precedent). Phase 1 is the
first of six D3 phases; this document was created NEW at Phase 1
closure and the addendum log section accepts Phase 2–6 closure entries
as they land.

---

## Per-phase tracking

| Phase | Description | Status | Commits |
|---|---|---|---|
| 1 | Foundations + Rule Audit | ✅ CLOSED 2026-04-29 | 908c51d → e86a282 (14 commits) |
| 2 | Holdout + single-treatment experiments | ✅ CLOSED 2026-04-29 | 20e0329 → e1cdc83 (5 commits) |
| 3 | Engineering Corps + Trust Metrics Infrastructure | ✅ CLOSED 2026-04-30 | 208fafd → 338b144 (22 commits across 5 merge branches) |
| 4 | Factorial + orthogonal-overlap scheduler | Open | — |
| 5 | Level-3 paired shadow + Adversarial Pairing + Golden-Set | Open | — |
| 6A | Dashboard scaffolding + Pulse + Briefing | Open | — |
| 6B | Reflection + Drill + verification spec consumption + shakedown | Open | — |

---

## Phase 1 narrative

### What landed

**1. Schema substrate (6 commits, ~1855 lines added).** Every D3 table
required by paired-runs.md and dashboard-implementation.md is now present in
both `createSchema` (fresh-DB path) and `runMigrations` (upgrade path) AND
in `schema/schema.sql` (operator-reviewable reference). Schema parity test
green throughout.

| Commit | Tables added |
|---|---|
| `36558f1` | Experiments, ExperimentTreatments, ExperimentMetrics, ExperimentRuns, ExperimentOutcomes, TreatmentSpecs, MetricVersions, AnalysisFrameworks, FleetStateSnapshots, GlobalHoldouts, ModelAvailability, TreatmentApplyLog (Phase 4 prerequisite) |
| `5f02d66` | FleetRules, PromotionProposals (with render_to enum + revert handling) |
| `00d3373` | Convoys/BountyBoard/TaskHistory column extensions (in_holdout, experiment_assignments_json, verification_spec_json, prompt_version, …) |
| `c821400` | ProposedFeatures + suppressions + score overrides + ConvoyReviewCycles |
| `94978c3` | AdversarialPairings + GoldenSetFixtures/Evaluations + CalibrationAuditSamples |
| `decf43b` | 14 dashboard data-layer tables (DashboardHealthHeartbeats, OperatorNotificationBudgets, OperatorNotificationDigest, OperatorSessionState, OperatorTrustDials, NarrativeRenders, BriefingRenders, CooldownPauses, OperatorAttentionTags, LLMCallTranscripts, GitOperationLog, OperatorEventAnnotations, ReplayResults, FleetLearningPanels) |

**Total tables added:** 32 new tables + column extensions on 3 existing
tables (BountyBoard +10 cols, Convoys +6 cols, TaskHistory +1 col).

**2. FleetRules bootstrap audit (commit `cc619a8`).** Every section of the
hand-authored CLAUDE.md was categorized by `render_to` into 33 audit seeds:

| render_to | count | notes |
|---|---|---|
| `claude-md-file` | 11 | universal-load preamble (Gas Town, no-silent-failures, daemon-ctx-threading, capability profiles, cross-agent client interfaces, testing rules, schema conventions, commit style, …) |
| `agent-prompt` | 4 | per-agent injection: worktree-isolation (astromech,pilot), startup-reconciliation (boot), captain-scope-guard (captain), llm-prompt-discipline (council,captain,medic,convoy-review,pr-review-triage,chancellor) |
| `per-domain-doc:*` | 8 | `docs/dashboard-conventions.md`, `docs/pr-flow-invariants.md`, `docs/self-healing.md` |
| `pattern-test-docstring` | 5 | P1 (rows.Scan), P7 (CAS), P10 (shell-boundary), P11 (exec-context), P15 (bash-guard) |
| `fix-log` | 5 | Fix #0 / #1 / #8a / #10 / Campaign 2 |

`BootstrapFleetRules` is idempotent (UNIQUE on `(rule_key, version)` + ON
CONFLICT DO NOTHING). Daemon startup calls it after `ReleaseInFlightTasks`
+ `ReconcileOnStartup` so the runtime DB is always seeded before agents
spawn.

**3. Renderer (commits `43962ed`, `f5a66df`).** Five render targets dispatched
from a single function set:

- `RenderClaudeMdFile` → 20 KB hard cap; refuses to write on overflow.
- `RenderFixLog` → opt-in via `--include-fix-log` (Phase 1 audit covers
  ~5 narratives; rendering would shrink the existing 140 KB historical
  record).
- `RenderPerDomainDocs` → map of relpath → bytes for `per-domain-doc:*`.
- `AssemblePerAgentPrompt` → agent-prompt content filtered by agent_scope
  (3-way: 'all' OR exact OR comma-list match), used by every Spawn function.
- `WriteRendered*` helpers + `CheckRenderDrift` for the `--check` flag.

`force render-rules` and `force render-rules --check` CLI sub-commands.
Makefile targets: `make render-rules`, `make render-rules-check`.
Pre-commit hook (`scripts/pre-commit/claude-md-size-check.sh`) installed
via the new dispatcher (`scripts/install-hooks.sh` updated).

**4. Per-agent injection wiring (commit `a318af6`).** Every Claude call
site that has agent-prompt rules in scope appends the rendered content to
its system prompt:

  - Captain (PromptBuilder + InjectFleetRulesAgentPrompt)
  - Council, Medic, Chancellor, ConvoyReview, PRReviewTriage, Astromech,
    Pilot (string append via AppendFleetRulesToPrompt)

Fail-open semantics: a missing FleetRules table or query error logs but
does not stop agent startup. The legacy const-based system prompt remains
unchanged; this wiring is purely ADDITIVE — Phase 1 adds FleetRules
content alongside existing prompts rather than replacing them. Future
operator-side migration of legacy consts (e.g. `AstromechTargetCLAUDEMDClause`,
`captainSystemPrompt`) into FleetRules is a separate cleanup.

**5. CLAUDE.md regeneration (commit `2d7a135`).**

  CLAUDE.md before: 49,873 bytes (~50 KB)
  CLAUDE.md after:   6,616 bytes
  Reduction:        86.7%

Plus three new per-domain docs:
  docs/dashboard-conventions.md      1,735 bytes
  docs/pr-flow-invariants.md         5,358 bytes
  docs/self-healing.md               4,423 bytes

Total auto-rendered output: 18,132 bytes across 4 files (was 49,873 bytes
in CLAUDE.md alone). Well under the 10 KB Phase 1 target for CLAUDE.md.

`internal/store/testdata/claude_md_pre_p3.md` captures the original
pre-render CLAUDE.md as the audit-time witness for
`TestBootstrapFleetRules_AllSectionsCategorized`.

**6. TestPattern_P17_ClaudeMdSize (commit `2d7a135`).** File-size
invariant: rejects on-disk CLAUDE.md > 20 KB. Bumping the cap requires
moving `claudeMdHardCapBytes` + `agents.ClaudeMdHardCapBytes` +
`scripts/pre-commit/claude-md-size-check.sh` in lockstep with a commit
that justifies the growth.

**7. Log-only treatments.Apply + metric registry (commit `e86a282`).**

  - `internal/treatments/apply.go`: log-only mode wired into
    `claude.AskClaudeCLIContext` and `claude.RunCLIStreamingContext` via
    a daemon-installed hook (`claude.SetTreatmentApplyHook`). Avoids a
    circular import — internal/claude does not depend on internal/treatments.
  - `internal/metrics/registry.go`: YAML manifest loader + RegisterMetric +
    LookupMetric + LoadFromDir. Idempotent on identical SQL body; rejects
    differing body at the same version.
  - `metrics/captain_rejection_rate/2026-04-23.{manifest.yaml,sql,test.sql}`:
    sample metric exercising the round-trip.

### What did NOT land (deferred)

  - **FIX-LOG.md regeneration.** Audit covers ~5 fix narratives; the
    remaining ~10+ narratives would be lost on render. Behind the new
    `--include-fix-log` flag until the audit is comprehensive.
  - **Migration of legacy system-prompt consts** (AstromechTargetCLAUDEMDClause,
    promptInjectionClause, captainSystemPrompt, etc.) into FleetRules.
    The audit does not include them; Phase 1's per-agent injection is
    additive, not replacement. Future cleanup.
  - **Live treatments.Apply pass-through.** Phase 1 ships log-only mode;
    Phase 2 of D3 flips this live.

---

## Heavy validation (closure-time)

```
$ make test
ok  force-orchestrator/cmd/force                       8.623s
ok  force-orchestrator/cmd/force-bash-guard            0.793s
ok  force-orchestrator/internal/agents               259.421s
ok  force-orchestrator/internal/agents/capabilities   (cached)
ok  force-orchestrator/internal/audittools             1.956s
ok  force-orchestrator/internal/claude                 4.615s
ok  force-orchestrator/internal/clients/*             (cached)
ok  force-orchestrator/internal/dashboard              4.369s
ok  force-orchestrator/internal/gh                    (cached)
ok  force-orchestrator/internal/git                   (cached)
ok  force-orchestrator/internal/metrics                1.894s
ok  force-orchestrator/internal/repo                  (cached)
ok  force-orchestrator/internal/store                  6.079s
ok  force-orchestrator/internal/telemetry             (cached)
ok  force-orchestrator/internal/treatments             2.942s
PASS

$ go test -tags sqlite_fts5 -run TestSchemaParity ./internal/store/...
PASS

$ go test -tags sqlite_fts5 -run TestPattern_P17 ./internal/audittools/...
PASS — CLAUDE.md: 6616 bytes (hard cap 20480, Phase 1 target ≤ 10240)

$ ./force render-rules --check
render-rules --check: OK (no drift)
Exit: 0

$ wc -c CLAUDE.md
6616 CLAUDE.md
```

T0/T1/T2 invariant tests intact: P1, P1.1, P3, P7, P8, P10, P11, P12,
P13, P15, P16, P17 all pass; TestInboundRedactCalledAtEveryCallSite,
TestForceIgnore, TestReconcile_*, TestAstromech_TargetCLAUDEMD,
TestNonAstromechAgents_DoNotIncludeTargetCLAUDEMD, TestBashGuard_*,
TestPricing, TestTaskSpendWatch, TestContextOverflow,
TestPromptByteAttribution, TestRepoMode, TestDivergenceDetector all pass.

---

## Anti-cheat self-check

| Directive | Status |
|---|---|
| Each gate is non-negotiable; halt on red | ✅ All four gates green |
| No `--no-verify` / `--force` / `rebase --skip` | ✅ none used |
| No pushes anywhere | ✅ commits stayed local on `main` |
| CLAUDE.md after Phase 3 is GENERATED — never hand-edited | ✅ rendered from FleetRules; pre-commit hook + P17 + drift check enforce |
| Per-agent injection preserves existing system-prompt content semantics | ✅ additive (legacy consts unchanged); FleetRules content appended |
| `render_to='claude-md-file'` count is plausible | ✅ 11 entries (cap 15) |
| No new schema additions outside the documented set | ✅ TreatmentApplyLog (Phase 4 prerequisite, called out in implementation prompt) is the only addition; surfaced as an operator-discretion item |
| Schema parity re-runs after every commit | ✅ green after each commit |
| Bootstrap is idempotent | ✅ verified by TestBootstrapFleetRules_Idempotent |

---

## Operator-discretion items (open)

1. **TreatmentApplyLog table.** Mentioned in D3 Phase 4's implementation
   prompt but not in paired-runs.md's schema block. Added in Phase 1
   commit `36558f1` so log-only writes have a permanent home that does
   not corrupt live `ExperimentRuns` data. Surfaced for operator
   review — if the spec wants log-only to roll into ExperimentRuns
   instead, redirect the writes in Phase 2.

2. **FIX-LOG.md migration is ongoing.** Phase 1's audit covers Fix #0,
   #1, #8a, #10, and Campaign 2 (5 narratives). The remaining ~10+
   narratives in the existing 140 KB FIX-LOG.md are NOT yet in
   FleetRules. The renderer's FIX-LOG.md write is gated behind
   `--include-fix-log` until the audit is comprehensive; on the day
   the audit covers every narrative, drop the gate and the existing
   file becomes auto-generated.

3. **Legacy system-prompt consts.** `AstromechTargetCLAUDEMDClause`,
   `promptInjectionClause`, `captainSystemPrompt`, `medicSystemPrompt`,
   etc. are still hand-authored Go consts. Phase 1's per-agent injection
   wiring is additive (FleetRules content appended alongside the legacy
   const). Future cleanup: migrate the consts to FleetRules rows tagged
   `agent-prompt` and remove the const declarations.

4. **`runChancellorReview` ctx threading.** Chancellor's claim function
   does not currently take a `context.Context` parameter; the FleetRules
   injection at its call site uses `context.Background()`. The
   `AssemblePerAgentPrompt` SELECT is sub-millisecond so the loss of
   ctx-cancellation is acceptable, but threading ctx through is a clean
   follow-up.

5. **Renderer drift in `force render-rules --check` does NOT include
   FIX-LOG.md.** Excluded from the drift detector for the same reason
   it's excluded from the default render — the audit is incomplete.
   Re-enable once item 2 above is resolved.

---

## Forward integration to Phase 2

The schema substrate, FleetRules table, treatments.Apply hook, and
metric registry are all live. Phase 2's flip from log-only to live mode
is a config change in `cmdDaemon`'s SetTreatmentApplyHook closure (write
ExperimentRuns rows, return live CallDescriptor) plus the actual paired-
runs algorithm — the wiring is already where it needs to be.

The dashboard data-layer tables (commit `decf43b`) sit unused until
Phase 6 of D3, but they're present in `createSchema` so 6A/6B can
build against a stable shape.

---

## Addendum log

(Phase 2–6 closure entries append below, oldest at the top.)

---

### Phase 2 — Holdout + single-treatment experiments — CLOSED 2026-04-29

**Operator:** jake.herman@upstart.com
**Commits (local only, not pushed):**

| Phase | Commit | Subject |
|---|---|---|
| 1 (Bayesian framework) | `20e0329` | feat(D3-P2): Bayesian Beta-Binomial analysis framework + registration |
| 2 (baseline-2026 holdout) | `0cf498e` | feat(D3-P2): mint baseline-2026 global holdout + deterministic assignment |
| 3 (Apply live flip) | `e9356da` | feat(D3-P2): treatments.Apply live mode + SystemConfig kill switch |
| 4 (lifecycle + CLI + sample) | `5fa0ed8` | feat(D3-P2): experiment lifecycle + CLI + shakedown manifest |
| 5 (dashboard endpoints) | `e1cdc83` | feat(D3-P2): dashboard experiments endpoints + minimal tab |
| 6 (closure addendum + docs) | (this commit) | docs(D3-P2): closure addendum + README/Security update |

#### What landed

**internal/analysis** — Bayesian Beta-Binomial framework (commit `20e0329`).
`BetaBinomialPosterior` with closed-form conjugate update (Beta(a,b) +
k/n → Beta(a+k, b+n-k)). `CredibleInterval` via bisection on the
regularised incomplete beta function `I_x(a, b)` implemented with
Lentz's continued-fraction expansion. `ComparePosteriors` estimates
P(treatment > control) by Monte Carlo over Marsaglia-Tsang Gamma
samples (deterministic seed for replay). `DecideOutcome` returns
{treatment | control | inconclusive} with a default
`MinSamplesPerArm=30` gate so small-n experiments are forced
inconclusive regardless of effect size. `RegisterBayesianBetaBinomial`
inserts the framework into `AnalysisFrameworks` with `version='2026-04-29'`,
idempotent on the version PK and rejecting re-registrations whose
config_hash drifts. Wired into daemon startup AFTER
`BootstrapFleetRules` and BEFORE the holdout mint.

**internal/holdout** — baseline-2026 global holdout (commit `0cf498e`).
`MintBaseline2026` inserts the row into `GlobalHoldouts` (idempotent
on the UNIQUE name index), defaults match paired-runs.md § Lifecycle
(7-day ramp, 2% indefinite plateau, 90-day fade once retired).
`Holdout.CurrentFraction(t)` is a pure function of `now` over the
ramp/plateau/fade math. `IsInHoldout` / `IsInHoldoutAt` /
`IsInHoldoutWithSnapshot` decide membership: SHA-256 over
`fmt.Sprintf("%d:%s:%d", holdoutID, kind, id)` → first 8 bytes as
big-endian uint64 / 2^64 ∈ [0, 1), member iff < CurrentFraction(now).
The hash domain is part of the contract.

**internal/treatments live flip** (commit `e9356da`). `Apply` now
defaults to live behaviour: the holdout check, experiment enrollment,
descriptor rewrite, and journal sequence runs by default. The
`SystemConfig` key `treatments_apply_mode` is the single-write
rollback (default `'live'`, set to `'log-only'` to stop enrollment
without a re-deploy). Holdout members short-circuit the experiment
loop. Multiple-experiment composition is in id order (factorial
orthogonality lands in Phase 4). Stickiness across Medic re-queues is
preserved by the existing-row check before INSERT.

**internal/experiments lifecycle + CLI** (commit `5fa0ed8`).
`AuthorFromYAML` / `Ratify` / `EnrollUnit` / `Terminate` /
`MaybePromoteRule` / `GetStatus` cover the single-treatment lifecycle.
Operator-routed gate via `Ratify`: empty operator email rejects, a
re-ratify against a running experiment errors via the CAS update,
each successful Ratify writes an `AuditLog` row. Termination computes
the outcome via the Bayesian framework over per-arm Bernoulli rollups
of `ExperimentRuns.score`. `MaybePromoteRule` mints a
`PromotionProposal` with an evidence trail iff the manifest's
`promote` block is set AND the outcome declared a winner.

`force experiment author <yaml> | ratify <id> | terminate <id> |
status <id> | list [--status]` ships the operator surface.

`experiments/2026-04-29-test-captain-prompt-v18/manifest.yaml` is the
shakedown experiment — a benign single-treatment experiment that
varies one Captain prompt-template version against control. Real-
shape, low-stakes (a no-op rename) so the lifecycle is exercised
without divergent behaviour.

**Dashboard endpoints + minimal tab** (commit `e1cdc83`).
`/api/experiments[?status=...]` lists; `/api/experiments/:id` returns
the full record with arms / metrics / outcome / per-arm enrollment +
observed rate; `/api/fleet-progress` returns the holdout's lifecycle
phase, current fraction, member count, and three rolling windows
(24h / 7d / 30d) of holdout-vs-current `ExperimentRuns.score`. The
new "Experiments" tab is intentionally thin — list + detail panel +
holdout strip — because Phase 6 absorbs and rebuilds it. POST/PATCH
on the singleton endpoint returns 405; operator mutations stay on the
CLI.

#### What did NOT land (deferred)

- **Cross-experiment treatment-spec dedup.** The current
  `TreatmentSpecs.spec_hash` is salted with `expID`, so two manifests
  authoring the same `(arm_label, prompt_template_ref, model)` triple
  produce distinct spec rows. paired-runs.md § Data Model contemplates
  cross-experiment sharing; the lookup-or-insert dance lands in a
  later phase that handles `ON CONFLICT(spec_hash) DO NOTHING
  RETURNING id` properly.
- **Operator-side experiment ratification UI.** The dashboard tab is
  read-only; operator mutations flow through the CLI until Phase 6
  ships UI-side ratification.
- **Live `ExperimentRuns.score` writeback.** The Bayesian outcome is
  computed when Terminate runs, but the score field on each
  `ExperimentRuns` row is whatever the score-source agent (a metric
  evaluator, ConvoyReview rollup, etc.) wrote. Phase 2 ships the
  framework; the score-source pipeline lands in Phase 3 (Engineering
  Corps + Trust Metrics).
- **`fleet_state_hash` on the holdout row.** `FleetStateSnapshots`
  isn't yet populated by any other agent, so the mint leaves
  `fleet_state_hash` blank. Backfill is an operator-discretion item.

#### Heavy validation (closure-time)

```
$ make test
ok  	force-orchestrator/cmd/force                   6.430s
ok  	force-orchestrator/cmd/force-bash-guard        (cached)
ok  	force-orchestrator/internal/agents             259.341s
ok  	force-orchestrator/internal/analysis           (cached)
ok  	force-orchestrator/internal/audittools         0.745s
ok  	force-orchestrator/internal/dashboard          2.266s
ok  	force-orchestrator/internal/experiments        1.128s
ok  	force-orchestrator/internal/holdout            (cached)
ok  	force-orchestrator/internal/treatments         (cached)
... (all packages green)

$ go test -tags sqlite_fts5 -run TestSchemaParity ./internal/store/...
ok

$ go test -tags sqlite_fts5 -run TestPattern_P8 ./internal/dashboard/...
ok — same-origin allow-list intact, no wildcard CORS reintroduced

$ go test -tags sqlite_fts5 -run "TestBetaBinomial|TestComparePosteriors|TestDecideOutcome|TestRegisterBayesianBetaBinomial" ./internal/analysis/...
ok — 15 tests, all pass

$ go test -tags sqlite_fts5 -run "TestMintBaseline2026|TestIsInHoldout" ./internal/holdout/...
ok — 9 tests, all pass

$ go test -tags sqlite_fts5 -run "TestApply_" ./internal/treatments/...
ok — 9 tests covering: pass-through, journal mode, rollback, holdout
membership, single-treatment enrollment, deterministic + spread, nil
db, sticky retries

$ go test -tags sqlite_fts5 -run "TestAuthorFromYAML|TestRatify|TestEnrollUnit|TestTerminate|TestMaybePromoteRule|TestLifecycle_EndToEnd" ./internal/experiments/...
ok — 11 tests, end-to-end shakedown round-trip green

$ go test -tags sqlite_fts5 -run "TestCaptain|TestCouncil|TestMedic|TestChancellor|TestConvoyReview|TestAstromech|TestPilot|TestDiplomat" ./internal/agents/...
ok — agent regression matrix is byte-identical for non-experiment, non-holdout units (Phase 3 invariant held)
```

#### Anti-cheat self-check

| Directive | Status |
|---|---|
| Each gate is non-negotiable; halt on red | ✅ Gates 1-5 + final all green |
| No `--no-verify` / `--force` / `rebase --skip` | ✅ none used |
| No pushes anywhere | ✅ commits stayed local on `main` |
| Live flip is byte-identical for non-experiment + non-holdout units | ✅ TestApply_NotInHoldout_NoActiveExperiments_PassesThrough + agent regression matrix |
| Live flip applies HOLDOUT-IDENTICAL behaviour to holdout members | ✅ TestApply_HoldoutMember_SkipsExperimentEnrollment |
| Live flip applies TREATMENT-MODIFIED behaviour to enrolled units | ✅ TestApply_SingleActiveExperiment_AppliesAssignedTreatment |
| Holdout assignment is deterministic | ✅ TestIsInHoldout_DeterministicAssignment over 5× repeat per unit |
| Experiment ratification is operator-routed + audit-logged | ✅ TestRatify_RequiresOperatorRoute_AuditLogged |
| All new mutators return error | ✅ MintBaseline2026, RegisterBayesianBetaBinomial, AuthorFrom*, Ratify, EnrollUnit, Terminate, MaybePromoteRule |
| Schema parity re-runs after every commit | ✅ green |
| No edits to D1/D2/D3 P1 closures | ✅ this addendum is the only D3 closure write |
| Phase 5 dashboard endpoints inherit securityMiddleware | ✅ no new http.Server, no wildcard CORS, P8 green |

#### Operator-discretion items surfaced

1. **Bootstrap upsert observation (P1 cleanup carryover).** Not
   re-encountered in this chunk — Phase 2 inserts go through fresh
   tables (`AnalysisFrameworks`, `GlobalHoldouts`) whose primary keys
   are explicit version / name strings, so a long-running operator
   dev-DB sees idempotent no-ops on re-mint. The previously-surfaced
   bootstrap upsert semantics issue is unchanged from the P1 cleanup
   note.

   **2026-04-30 update:** investigated as part of a follow-up drift-fix
   chunk and reclassified as a **false-positive carryover**. The
   apparent CLAUDE.md / FIX-LOG.md drift surfaced by `force render-rules
   --check` reproduces only against a stale persistent dev-DB —
   `BootstrapFleetRules` uses `ON CONFLICT(rule_key, version) DO
   NOTHING`, so existing rows are not updated when the audit slice
   changes. Against a fresh DB the audit slice and on-disk rendered
   files actually agree (verified: exit 0). The drift-fix worktree
   (`deliverable/3/phase-2-drift-fix`) closed as a no-op; the systemic
   fix is queued as a separate D3 chunk: convergent
   `BootstrapFleetRules` + `force render-rules` operating against an
   in-memory DB by default so the dev-DB-staleness signal stops being
   confused with real drift.

2. **Dashboard tab is intentionally minimal.** D3 Phase 6 rebuilds
   around Pulse / Briefing / Reflection and absorbs this tab.
   Operator-side experiment ratification UI lands in 6A.

3. **Operator-pre-approval is CLI-only.** `force experiment ratify
   <id> --operator <email>` is the only ratification path. Dashboard
   `POST /api/experiments/:id/ratify` is intentionally absent (returns
   405 today) and ships in Phase 6 with the broader operator-action
   audit-trail surface.

4. **`fleet_state_hash` on the holdout row is blank.**
   `FleetStateSnapshots` is not yet populated by any other agent, so
   the mint leaves the column empty. Backfill via an explicit operator
   command is a follow-up.

5. **Cross-experiment TreatmentSpec dedup is deferred.** spec_hash is
   salted with expID for now to avoid the UNIQUE collision when the
   same manifest is authored twice. The cross-experiment sharing
   property mentioned in paired-runs.md § Data Model lands in a later
   phase that handles `ON CONFLICT DO NOTHING RETURNING id` properly
   for SQLite.

6. **Live ExperimentRuns score writeback is not yet wired.**
   `ExperimentRuns.score` is whatever the score-source pipeline writes;
   Phase 2 ships the framework that consumes the column, Phase 3 ships
   the producer (per-prompt-version metrics correlation in EC).

#### Forward integration to Phase 3

The lifecycle's `MaybePromoteRule` writes `PromotionProposals` rows
with `authored_by='engineering-corps'`. Phase 3 formalises the EC
claim loop that consumes those rows + reverse-feeds the candidates
from Librarian. The score-source pipeline (writing
`ExperimentRuns.score`) is also a Phase 3 deliverable; the framework
already consumes the column.

The dashboard-tab + endpoints are explicitly thin so the Phase 6
rebuild can absorb them without legacy carry-over.

---

### 2026-04-30 — operational hygiene addendum (db-protection branch)

Not a new D3 phase. While investigating the false-positive drift
(operator-discretion item #1 above), the persistent `holocron.db` was
inadvertently removed during a backup/restore compound command. The
DB was recreated from a fresh schema + bootstrap (operational state
loss limited to the operator's local dev-DB; no shared state was
affected), and three layers of protection were added so the same
mistake cannot recur.

**Branch:** `db-protection` (merged + cleaned, not pushed).

**Layers added:**

1. **Filesystem ACL (load-bearing).** `make protect-db` applies a
   macOS ACL (`everyone deny delete,delete_child`) to `holocron.db`,
   `holocron.db-wal`, `holocron.db-shm`. SQLite read/write operations
   are unaffected; only `unlink` / `rename` syscalls are blocked.
   Idempotent on re-run. Reverse: `make unprotect-db`.
2. **Claude Code deny rules.** `.claude/settings.json` rejects any
   Bash invocation matching `rm` / `mv` / `unlink` / `cp` / `dd` on
   `holocron*` paths before the syscall reaches the kernel. Belt-and-
   suspenders alongside the ACL, and the only layer that catches
   intent (vs effect).
3. **Hourly snapshot cron.** `make install-snapshots` schedules an
   hourly `sqlite3 .backup` (WAL-consistent, unlike `cp`) into
   `~/.force/backups/`, with a daily 04:00 cleanup that prunes
   snapshots older than 30 days. Idempotent installer.

**Files added:**

| Path | Purpose |
|---|---|
| `.claude/settings.json` | deny rules for rm/mv/unlink/cp/dd on holocron* |
| `scripts/setup-snapshots.sh` | idempotent crontab installer |
| `scripts/uninstall-snapshots.sh` | mirror — remove crontab entries |

**Files modified:**

| Path | Change |
|---|---|
| `Makefile` | `protect-db` / `unprotect-db` / `install-snapshots` / `uninstall-snapshots` / `db-status` targets |
| `README.md` | operator-protection subsection (under Security & Safety) |
| `DELIVERABLE-3-CLOSURE.md` | this addendum + reclassification of operator-discretion item #1 |

**Operator-state actions (per-machine, not committed):**

- `./force render-rules --check` recreated `holocron.db` (exit 0 →
  drift theory confirmed false-positive).
- A brief `force daemon` start triggered the daemon-startup-only
  registrations (Bayesian framework into `AnalysisFrameworks`,
  baseline-2026 into `GlobalHoldouts`) so the dev-DB matches what
  Phase 2 ships.
- `make protect-db` applied the ACL.
- `make install-snapshots` installed the crontab entries.

**Self-healing fix queued as a separate chunk.** The convergent-
bootstrap fix (so `BootstrapFleetRules` upserts content-changed rows,
plus `force render-rules` operating against a fresh in-memory DB by
default) is the systemic fix that makes the dev-DB-staleness signal
stop being mistaken for source-level drift. It's small and lands on
its own branch ahead of D3 Phase 3.

---

### 2026-04-30 — repo-layout reorg

Closure documents moved from repo root to `docs/closures/`;
audit/historical artifacts moved from repo root to
`docs/operator-archives/`. Repo root post-move contains only
`README.md`, `CLAUDE.md`, `FIX-LOG.md`. Cross-references in
`docs/*.md`, `README.md`, and the embedded fixlog content
(`internal/store/fixlog/fix8d.md`) were all updated; CLAUDE.md
was byte-identical post-rerender (no audit-slice references
touched); FIX-LOG.md regenerated cleanly. `./force render-rules
--check` exits 0 post-merge.

**Operator-discretion:** future closure documents (D4–D10) ship
into `docs/closures/`, not the root. README.md gained a "Repo
layout" section memorialising this so the convention survives the
next contributor.

**Files moved (11):**

| From | To |
|---|---|
| `DELIVERABLE-{0,1,2,3}-CLOSURE.md` | `docs/closures/` |
| `FIX-{8D,8E,8F}-CLOSURE.md` | `docs/closures/` |
| `AUDIT.md`, `AUDIT-VERIFICATION.md`, `AUDIT-TEST-MANIFEST.md`, `FINAL-STATUS.md` | `docs/operator-archives/` |

**Files modified for cross-references (6):** `docs/roadmap.md`
(47 hits + 4 prose contradictions), `docs/dashboard-implementation.md`
(1), `docs/next-gen-agents.md` (1), `README.md` (1 reference + new
Repo layout section), `internal/store/fixlog/fix8d.md` (2),
`FIX-LOG.md` (regenerated).

No deliverable-status change; D3 remains 🟡 PARTIAL (Phases 1–2
closed). This addendum records operational hygiene only.

---

### 2026-04-30 — convergent bootstrap + P18 + pre-commit hook

The systemic fix for the recurring "false-drift signal" class.
Three coupled changes plus a hook upgrade landed on branch
`convergent-bootstrap` and merged back to `main` via `--no-ff`:

1. **`BootstrapFleetRules` is now convergent on `content_hash`
   mismatch** (only for bootstrap-managed rows; operator-direct-write
   rules are untouched). When the audit slice's content for an
   existing `(rule_key, version)` row diverges from the persisted DB
   content, bootstrap refreshes the DB row to match. Eliminates the
   "stale persistent DB" failure mode that produced false-drift
   signals on every audit-slice content edit.

2. **`force render-rules` uses a fresh in-memory DB by default.**
   `--use-runtime-db` is the explicit opt-in for inspecting renders
   that include operator-direct-write rules. The CLI's render output
   no longer depends on operator-side persistent DB state.

3. **`TestPattern_P18_RenderCoherence` lands as an in-suite drift
   gate.** Asserts on-disk `CLAUDE.md` / `FIX-LOG.md` / per-domain
   docs byte-equal what the audit slice renders against a fresh
   in-memory DB. A synthetic-regression sanity test
   (`TestPattern_P18_DetectsInjectedDrift`) proves the comparison
   helper actually surfaces drift — P18 is not toothless.

4. **Pre-commit hook upgraded from size-only to coherence-check.**
   `scripts/pre-commit/render-coherence-check.sh` runs `force
   render-rules --check` when audit-relevant files
   (`fleet_rules_audit.go`, `fixlog/`, `fleet_rules_bootstrap.go`,
   `rule_renderer.go`) are staged. Cheap fast-path skips otherwise.
   Auto-discovered by the existing `dispatcher.sh`; activates on
   the next operator `make hooks-install`.

The drift class is sealed in three layers:

| Layer | Mechanism | Catches |
|---|---|---|
| 1 | Convergent `BootstrapFleetRules` | Stale DB at the source — re-bootstrap refreshes drifted rows. |
| 2 | `TestPattern_P18_RenderCoherence` | Drift in `make test` / CI before merge. |
| 3 | `render-coherence-check.sh` pre-commit hook | Drift one step earlier at commit time. |

Auto-resolution of the doc-reorg drift wrinkle: the persistent
dev-DB drift on `main` (the stale `fix8d-code-red-full-closure`
row) refreshes automatically on the first daemon-side
`BootstrapFleetRules` run after this merge. No operator action
required.

No deliverable-status change; D3 remains 🟡 PARTIAL (Phases 1–2
closed + drift class sealed). Phase 3 (Engineering Corps + Trust
Metrics Infrastructure) starts on a clean baseline.

---

### Phase 3 — Engineering Corps + Trust Metrics Infrastructure — CLOSED 2026-04-30

**Operator:** jake.herman@upstart.com
**Topology:** five merge-back branches via `--no-ff` (skeleton →
task-types → handoff-ratify → disagreement-metrics → shakedown).
22 commits total. Worktrees: `.build-worktrees/D3-P3-{skeleton,A,B,C,
shakedown,closure}` — all removed at closure.

#### What landed

**1. EC skeleton (Phase 1 of P3 — orchestrator's own worktree, branch `deliverable/3/phase-3-skeleton`).**
SpawnEngineeringCorps claim loop, six-task-type dispatcher, shared
types, capability profile (`engineering-corps.yaml` — empty tools,
mirroring Diplomat's read-only baseline), daemon spawn wiring after
the existing review-agent roster + treatments.Apply hook.

| Commit | Subject |
|---|---|
| `208fafd` | feat(D3-P3): EngineeringCorps skeleton — Spawn loop + dispatcher + types |
| `5ab4be5` | feat(D3-P3): wire SpawnEngineeringCorps into daemon startup |
| `32f38be` | Merge branch 'deliverable/3/phase-3-skeleton' (--no-ff) |

The const block in `internal/agents/engineering_corps/types.go` is
the authoritative task-type inventory. Sub-agents A/B/C discovered
the inventory from this file. Dispatcher's default branch fails
the bounty cleanly via `store.FailBounty` (the captain pattern P12
fail-closed-on-unknown-decision shape). Pattern P13 honored:
`capabilities.LoadProfile("engineering-corps")` sources tool args.
Pattern P16 honored: `EngineeringCorpsConfig` holds
`librarian.Client` + `metrics.Client` interfaces, never concrete
struct types.

**2. Six task type handlers (sub-agent A — branch `deliverable/3/phase-3-task-types`, 6 commits + merge).**
Each handler in its own file under `internal/agents/engineering_corps/`:

| Handler | Shape | Operator-routing |
|---|---|---|
| `HoldoutMonitor` | SQL-only — counts active GlobalHoldouts, emits debug heartbeat. Full availability watch deferred to P5/P6. | No mutation of FleetRules or PromotionProposals. |
| `ExperimentMonitor` | SQL-only — Bayesian framework over (treatment, control); declares winners (≥0.95 posterior, ≥30 trials per arm via `analysis.DecisionRule.MinSamplesPerArm`); emergency-stops (P(treatment worse) > 0.9, ≥20 trials); queues `ECPromotionAuthor` follow-up. | Never sets `ratified_at`. Promotion is downstream. |
| `PromotionAuthor` | SQL-only — assembles `PromotionProposals` row from terminated declared-winner experiment with full evidence trail; idempotent on existing open proposals. | Writes `ratified_at=''` + 14-day TTL. Operator gate preserved. |
| `DemotionAuthor` | SQL-only — finds promote-proposals ratified > N days ago (default 30); writes placeholder `kind='demote'` rows. Full retention scoring deferred to P4/P5. | Demote proposal unratified; idempotent. |
| `MetricAuthor` | LLM — generates metric SQL via Claude; validates read-only via word-boundary deny-list (INSERT/UPDATE/DELETE/ALTER/DROP/CREATE/REPLACE/TRUNCATE/ATTACH/DETACH/VACUUM/PRAGMA/BEGIN/COMMIT/ROLLBACK); writes MetricVersions row. | Metric never auto-attaches to ExperimentMetrics; no FleetRules edit. |
| `ExperimentAuthor` | LLM — generates experiment manifest; sentinel-tag wraps untrusted hypothesis + librarian evidence (Pattern P12); strict-decodes (Fix #8.5); writes Experiments rows in `authored` state via `experiments.AuthorFromManifest`; stages manifest YAML to `experiments/<stamp>-<id>/manifest.yaml`. | Lands in `authored`; operator must call `Ratify` before `running`. |

Dispatcher fully wired — `ErrNotImplemented` removed, every task
type routes to a real handler. Pattern P12 sentinel-wrap + Pattern
P13 capability-profile sourcing honored at every LLM call site.

**3. Librarian → EC handoff + dashboard ratification surface (sub-agent B — branch `deliverable/3/phase-3-handoff-ratify`, 3 commits + merge).**

- `librarian.Client` extended with `EmitCandidate(ctx, Candidate) (int, error)` and `ListPendingCandidates(ctx) ([]Candidate, error)`. Schema convention: `kind='candidate' AND authored_by='librarian'` doubles `authored_by` as the origin column — no schema migration (`origin` column not added; the P2 closure note's pattern stands).
- Dashboard handlers in `internal/dashboard/handlers_ec.go`: GET `/api/ec/proposals` (list pending), GET `/api/ec/proposals/:id` (single), POST `/api/ec/proposals/:id/ratify` (operator-routed; CAS UPDATE + AuditLog), POST `/api/ec/proposals/:id/reject` (rejection_rationale ≥ 20 chars when `rejection_action != 'leave_as_is'` per concern #7).
- CLI: `force ec list / ratify / reject / status` (`cmd/force/ec.go`).
- Frontend: minimal "EC" tab in `internal/dashboard/static/{index.html,app.js}`. Phase 6 absorbs into Pulse / Briefing / Reflection.

Ratify rejects empty operator email (header fallback `X-Operator-Email` allowed); CAS conflict on terminal rows returns 409. Pattern P8 stays green: no wildcard CORS, no new `http.Server`, body cap inherited from `securityMiddleware`.

**4. Cross-layer disagreement tracking + per-prompt-version metrics (sub-agent C — branch `deliverable/3/phase-3-disagreement-metrics`, 5 commits + merge).**

Schema addition: `DisagreementPairs` table (rolling-window aggregate, distinct from `AdversarialPairings` which is per-decision primary-vs-critic). Added to `createSchema` + `runMigrations` + `schema/schema.sql` in commit `919daef` — `TestSchemaParity` green.

Disagreement pairs computed:

| Pair | Shape | Status |
|---|---|---|
| `captain-council-reject` | Captain "Completed" → later Council "Rejected" on same task_id | live |
| `council-ci-fail` | Council "Completed" / "AwaitingSubPRCI" → later Failed outcome | live |
| `convoy-review-cant-fix` | CodeEdit fix-task whose parent has a ConvoyReview TaskHistory entry → fix-task ended Failed | live |
| `senate-chancellor-decline` | Senate concur → Chancellor declines | **deferred until D4** (Senate ships in D4; pair returns `Deferred=true` with `DeferredReason="Senate agent ships in D4; pair will populate then"`) |
| `operator-revert-30d` | BountyBoard `Completed` → revert task with `revert_target_task_id` pointing back, within 30 days | live |

`internal/analytics/disagreement.go` exposes `ComputeDisagreementRates(ctx, db, window)`; `internal/agents/dog_disagreement_tracker.go` runs hourly (post `dogTaskSpendWatch`), honors `IsEstopped` + `SpendCapExceeded`, persists via `PersistDisagreementRates`. `TestListDogs` count bumped 21 → 22.

`internal/metrics/per_prompt_version.go` exposes `MetricByPromptVersion(ctx, db, metricName, since)` returning a `map[promptVersion]value`. Reads `TaskHistory.prompt_version` (added in P1 schema). `RegisterGroupedMetric` is the registry sibling.

Dashboard: GET `/api/disagreement-rates` returns latest row per pair × window combination (`internal/dashboard/handlers_disagreement.go`).

**5. End-to-end shakedown (orchestrator's own worktree — branch `deliverable/3/phase-3-shakedown`, 1 commit + merge).**

`internal/agents/engineering_corps/shakedown_test.go` —
`TestShakedown_LibrarianToFleetRulesRoundTrip` — exercises the
full chain in a hermetic test:

```
Librarian.EmitCandidate
  → experiments.AuthorFromManifest (synthetic; bypasses LLM)
  → operator-routed experiments.Ratify (with AuditLog)
  → seeded ExperimentRuns (35 trials per arm; treatment 100% / control 28%)
  → handleExperimentMonitor (declares winner via Bayesian framework)
  → handlePromotionAuthor (assembles ratifiable proposal)
  → operator-routed dashboard ratify (CAS UPDATE + AuditLog)
  → simulated FleetRules INSERT (P6 atomic-DB+render+commit deferred)
  → fresh-DB bootstrap convergence smoke check
```

Determinism: posterior > 0.95 every run. Two AuditLog rows for
the operator across the round-trip. `experiments.Ratify` with
empty operator email is rejected (operator-routed gate).

#### Heavy validation (closure-time)

```
$ go build -tags sqlite_fts5 -o force ./cmd/force/
(silent — clean build)

$ make test
ok  force-orchestrator/agents/capabilities          [no test files]
ok  force-orchestrator/cmd/force                       8.190s
ok  force-orchestrator/cmd/force-bash-guard          (cached)
ok  force-orchestrator/internal/agents               256.473s
ok  force-orchestrator/internal/agents/capabilities  (cached)
ok  force-orchestrator/internal/agents/engineering_corps   1.770s
ok  force-orchestrator/internal/analysis             1.359s
ok  force-orchestrator/internal/analytics            1.852s
ok  force-orchestrator/internal/audittools           2.523s
ok  force-orchestrator/internal/claude               6.278s
ok  force-orchestrator/internal/clients/{capabilities,experiments,graph,librarian,metrics,rules}  (cached)
ok  force-orchestrator/internal/dashboard            4.392s
ok  force-orchestrator/internal/experiments          3.090s
ok  force-orchestrator/internal/git                 24.323s
ok  force-orchestrator/internal/holdout              3.111s
ok  force-orchestrator/internal/metrics              2.911s
ok  force-orchestrator/internal/repo                 (cached)
ok  force-orchestrator/internal/store                7.692s
ok  force-orchestrator/internal/telemetry            (cached)
ok  force-orchestrator/internal/treatments           3.332s

$ go test -tags sqlite_fts5 -run "TestShakedown_LibrarianToFleetRulesRoundTrip" ./internal/agents/engineering_corps/...
PASS — round-trip: candidate=1 → exp=1 → promotion=2 → ratified — winner posterior > 0.95

$ go test -tags sqlite_fts5 -run "TestSchemaParity" ./internal/store/...
PASS

$ ./force render-rules --check
render-rules --check: OK (no drift)
Exit: 0
```

Pattern test inventory (P1, P1.1, P3, P7, P8, P10, P11, P12, P13,
P15, P16, P17, P18) + Phase 3-specific tests
(`TestEngineeringCorps`, `TestHandle*`, `TestEmitCandidate`,
`TestECHandler`, `TestComputeDisagreementRates`,
`TestMetricByPromptVersion`,
`TestShakedown_LibrarianToFleetRulesRoundTrip`) all green.

#### Anti-cheat self-check

| Directive | Status |
|---|---|
| Each gate is non-negotiable; halt on red | ✅ all gates passed; one rebase conflict resolved (dashboard.go — both B and C added handlers in same block; both kept) |
| No `--no-verify` / `--force` / `rebase --skip` | ✅ none used |
| No pushes anywhere | ✅ commits stayed local on `main` |
| Capability profile sourced from YAML at every Claude CLI call site | ✅ `engineering-corps.yaml` + `LoadProfile` at every handler |
| Cross-agent dependencies route through `Client` interfaces | ✅ Pattern P16 green; `EngineeringCorpsConfig` holds `librarian.Client` + `metrics.Client` |
| All new mutators return `error` | ✅ `EmitCandidate`, `PersistDisagreementRates`, `RegisterGroupedMetric`, all P3 handlers |
| Dispatcher fail-closed on unknown task type | ✅ `TestEngineeringCorpsDispatcher_UnknownTypeFailsCleanly` |
| Sentinel-tag wraps attacker-controllable LLM input | ✅ `ExperimentAuthor` wraps hypothesis + evidence (Pattern P12) |
| Strict-JSON-decode on LLM responses | ✅ `strictJSONDecode` / `strictDecode` at every call site (Fix #8.5) |
| Schema parity re-runs after every merge | ✅ green; `DisagreementPairs` added to all three sources |
| Render coherence re-checked after every merge | ✅ `force render-rules --check` exit 0; P18 green |
| Topology preserved (`--no-ff` + visible merge commits) | ✅ five merges (skeleton, task-types, handoff-ratify, disagreement-metrics, shakedown) all visible in `git log --graph` |

#### Operator-discretion items surfaced

1. **FleetRules INSERT from ratified PromotionProposal is Phase 6 work.**
   `internal/dashboard/handlers_ec.go` header explicitly notes that
   "the FleetRules write itself is Phase 6's atomic DB+render+commit
   dance." Phase 3's ratify handler flips `ratified_at` and writes
   AuditLog; the actual FleetRules INSERT lives in Phase 6. The
   shakedown simulates the future row with a direct INSERT to
   demonstrate the renderer's robustness against operator-direct-write
   rules; the production path lands in Phase 6.

2. **`ExperimentAuthor`'s LLM-using path is unit-tested separately.**
   The shakedown bypasses it via `experiments.AuthorFromManifest` to
   keep the round-trip hermetic (no Claude CLI dependency in the test
   binary). `experiment_author_test.go` covers the LLM path
   (happy / parse-error / operator-routing-preserved).

3. **`MetricAuthor` write does NOT auto-attach metrics to
   `ExperimentMetrics`.** The handler writes a `MetricVersions` row
   only — wiring a new metric into an experiment requires explicit
   operator action (the manifest references it). This is deliberate
   per paired-runs.md § Metric registry: "Metric does not go live
   until operator ratifies. Metrics are higher stakes than
   experiments."

4. **`DemotionAuthor` is placeholder-shape.** The handler enumerates
   stale ratified `kind='promote'` proposals and writes a `kind='demote'`
   placeholder. Full retention scoring (Tier 2 metrics, regression
   detection) lands in P4/P5 once the metric registry has accumulated
   downstream-outcome data.

5. **`HoldoutMonitor` is heartbeat-only.** Phase 3 ships the SQL
   plumbing; the model-availability watch (probing model identifiers
   for deprecation headers / 404s) lands in P5/P6.

6. **`Senate → Chancellor` disagreement pair is `Deferred=true`.**
   Senate ships in D4. The pair's docstring + `DeferredReason` field
   explicitly name the deferral; tests assert the deferred state
   rather than fabricating a value.

7. **One rebase conflict during merge-back.** Both sub-agent B and
   sub-agent C added a `mux.HandleFunc` block to
   `internal/dashboard/dashboard.go`. The conflict was a clean
   "both add" — kept both blocks (B's `/api/ec/proposals` + C's
   `/api/disagreement-rates`). No semantic conflict, no test churn.
   Documented here so the cross-track adjacency surface is
   explicit if a future Phase 6A track touches the same region.

#### Forward integration to Phase 4

EC's ExperimentAuthor today writes single-treatment manifests
(treatment + control). Phase 4 introduces factorial dimensions
(cell-based storage, stratified randomization, main-effects + 2-way
interactions). The handler signature stays stable; the manifest
schema extends with a `factors` block.

The disagreement-tracker dog populates `DisagreementPairs` hourly
starting now; by Phase 4's close the rolling 7d / 30d / 90d windows
will have data the factorial-scheduler can use to refuse overlap on
shared dimensions.

The per-prompt-version metric correlation lands the substrate that
Phase 5's golden-set evaluation framework consumes — accuracy
regression alerts join `TaskHistory.prompt_version` with the
fixture-evaluation outcomes via the same `MetricByPromptVersion`
shape.
