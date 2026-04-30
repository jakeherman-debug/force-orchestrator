# DELIVERABLE-3-CLOSURE.md — Paired Runs + Engineering Corps + Global Holdout (🟡 PARTIAL)

**Date:** 2026-04-29
**Operator:** jake.herman@upstart.com
**Net verdict:** 🟡 PARTIAL — Phase 1 CLOSED; Phases 2–6 OPEN

D3 uses a partial-closure pattern (per D1's precedent). Phase 1 is the
first of six D3 phases; this document is created NEW at Phase 1 closure
and the addendum log section will accept Phase 2–6 closure entries as
they land.

---

## Per-phase tracking

| Phase | Description | Status | Commits |
|---|---|---|---|
| 1 | Foundations + Rule Audit | ✅ CLOSED 2026-04-29 | 908c51d → e86a282 (14 commits) |
| 2 | Holdout + single-treatment experiments | Open | — |
| 3 | Engineering Corps + Trust Metrics Infrastructure | Open | — |
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
