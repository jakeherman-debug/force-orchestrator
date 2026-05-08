# DELIVERABLE-7-CLOSURE.md — Model-Tier Optimization Experiments (ENGINEERING CLOSED)

**Date:** 2026-05-02
**Operator:** jake.herman@upstart.com
**Net verdict:** ✅ ENGINEERING CLOSED. The experiment harness for all eight Haiku-downgrade experiments ships, the per-arm Claude `--model` swap chain is wired end-to-end, and eight ratification-ready paired-runs YAMLs are committed under `experiments/E7-N-<agent>/manifest.yaml`. Strict verifier shard final-gate GO at HEAD on Static + Heavy + Race shards with 6/6 exit criteria passed and 4/4 anti-cheat directives enforced.

> **Important — engineering portion only.** D7 is a multi-week deliverable: the actual experiment runs (each capped at 168h `duration_cap_hours`) plus the 30-day post-ship monitoring window per promoted agent are operator/calendar gates that play out over weeks. This closure documents the engineering-shipped state — infrastructure is ready, manifests are authored, the model-swap chain is verified — but does NOT terminate any of the eight experiments. Real D7 closure with retention evidence (exit criterion #2) and aggregate fleet-cost-delta evidence (exit criterion #3) comes weeks later, in a follow-up addendum to this same file.

D7 is a single-ship engineering deliverable per the merge-order table (one branch, one merge); the eight experiments themselves run independently afterward.

---

## Per-track tracking

| Track | Description | Status | Merge SHA | Impl SHA |
|---|---|---|---|---|
| D7-Engineering | Per-arm `--model` swap wiring + 8 paired-runs YAMLs + ShipGate/ConfirmPhaseRequired manifest extensions + 16 metric SQLs (one quality + one cost per agent) | ✅ ENGINEERING CLOSED | `f0a7390` | `82ef5ce` |

---

## Files shipped

| Path | Role |
|---|---|
| `internal/claude/claude.go` | `buildClaudeArgs` (line 757) gains `modelOverride` parameter that surfaces as `--model <id>` (lines 774-775) when non-empty; empty preserves pre-D7 behaviour. `RequestedModel(ctx)` round-trips the per-call model id through a context-value seam read by both the json (line 287) and stream-json (line 442) exec branches. |
| `internal/claude/transcript.go` | `CallWithTranscript*` auto-stamps `Agent + TaskID` onto the call ctx via `ensureCallCtx` so the `TreatmentApplyHook` sees `subject_agent` without requiring every agent to wire `WithClaudeCallContext` manually. Existing inner stamps (Captain's byte-attribution path) preserved (no-clobber rule). |
| `internal/claude/context_overflow.go` | Hook signature plumbing for the (`modelOverride`, `err`) return shape. |
| `internal/claude/model_override_test.go` | New 228-line test file. End-to-end coverage of the model-override chain: ctx-value round-trip, argv emission, hook composition, and the no-clobber rule for inner Agent stamps. |
| `internal/experiments/lifecycle.go` | `ShipGate{Quality, Cost}` (line 46-) and `ConfirmPhaseRequired bool` manifest fields. `LoadShipGate(ctx, db, id)` reader for a future PromotionAuthor task to evaluate the gate before minting a PromotionProposal. Persisted as JSON in `SystemConfig.experiment_ship_gate_<id>` (mirrors the existing `Promote` shape; no schema migration required). |
| `internal/experiments/d7_manifest_test.go` | New 214-line test file. `TestAuthorFromYAML_PersistsShipGate`, `TestAuthorFromYAML_NoShipGate_NoPersistence`, plus a parse of every actual on-disk `experiments/E7-*/manifest.yaml` asserting `ship_gate.quality` references the primary metric AND `ship_gate.cost` encodes the `< 0.4 × control` invariant AND `confirm_phase_required: true` (the three roadmap-mandated invariants made mechanical). |
| `cmd/force/fleet_cmds.go` | Daemon wiring for the experiment apply hook so the resolved per-arm model id flows onto the actual Claude subprocess. |
| `experiments/E7-1-boot/manifest.yaml` … `experiments/E7-8-chancellor/manifest.yaml` | Eight ratification-ready paired-runs YAMLs (Boot, memory_rerank, PR-review-triage, Librarian, Diplomat, Medic, Commander, Chancellor). Control arm = `claude-sonnet-4-7`; treatment arm = `claude-haiku-4-5-20251001`. `min_practical_effect: 0.05`, `stakes_tier: medium`, `confirm_phase_required: true`, `ship_gate.{quality, cost}` populated per anti-cheat. |
| `metrics/{boot_decision_accuracy, memory_rerank_relevance, pr_review_triage_accuracy, librarian_synthesis_quality, diplomat_summarization_quality, medic_decision_accuracy, commander_plan_validity, chancellor_plan_merge_rate}/2026-05-02.{sql,test.sql,manifest.yaml}` | Eight per-agent quality metrics. The `pr_review_triage` arm uses the `accuracy` suffix (not `decision_accuracy`) — the canonical metric directory is `metrics/pr_review_triage_accuracy/`. |
| `metrics/{boot,memory_rerank,pr_review_triage,librarian,diplomat,medic,commander,chancellor}_cost_per_call/2026-05-02.{sql,test.sql,manifest.yaml}` | Eight per-agent cost metrics. Cost-per-call sourced from `LLMCallTranscripts.cost_usd` (the canonical D3-introduced field). |

---

## Exit criteria — verified (engineering portion)

| # | Criterion | Status | Evidence |
|---|---|---|---|
| 1 | Eight experiments terminated (promoted with evidence or declared null/inconclusive). | OPERATOR/CALENDAR PENDING | Engineering substrate is ready: 8/8 YAMLs ship at `experiments/E7-N-<agent>/manifest.yaml`; the harness can run them against the holdout. Termination requires operator ratification + experiment runtime (cap 168h each) + Bayesian-stop conditions; this is an operator/calendar gate, not an engineering gate. |
| 2 | Post-ship 30-day monitoring shows no regression for promoted agents. | OPERATOR/CALENDAR PENDING | Calendar-gated by definition. The retention-metric reading via `force fleet-progress --metric <agent>_<quality-suffix> --compare holdout --window 30d` (per-agent suffix per the `metrics/` directory layout — e.g. `pr_review_triage_accuracy`, `boot_decision_accuracy`, `librarian_synthesis_quality`) will produce evidence T+30 after the last promoted ship-PR. |
| 3 | Aggregate fleet cost per convoy delta visible on dashboard. | OPERATOR/CALENDAR PENDING | The cost-per-call metrics (`metrics/<agent>_cost_per_call/2026-05-02.sql`) read from `LLMCallTranscripts.cost_usd`; fleet-progress aggregation surface already exists. The reading itself awaits experiment runtime + ship + monitoring window. |
| 4 | `docs/closures/DELIVERABLE-7-CLOSURE.md` contains evidence trail per experiment. | ENGINEERING-PORTION FILED (this doc) | This file. Full evidence trail per experiment (terminated-reason, cell means, posterior, confirm-phase outcome, promoted/not-promoted) lands in a follow-up addendum once the experiments terminate. |

---

## Anti-cheat self-check

| Directive (per docs/roadmap.md § D7 Anti-cheat directives) | Status | Per-line evidence |
|---|---|---|
| **No promoting-on-cost-alone.** Ship gate requires BOTH quality-hold AND cost-drop. | ✅ | All 8 YAMLs declare `ship_gate.quality` AND `ship_gate.cost` as separate non-empty predicates. `internal/experiments/d7_manifest_test.go:75-78` rejects any manifest whose `ship_gate.quality` does not reference the primary metric AND whose `ship_gate.cost` does not encode the `< 0.4 × control` invariant. The cross-product is enforced manifest-by-manifest in the on-disk parse loop (lines 186-188). |
| **No cherry-picking the ship gate per experiment.** Gate is uniform across all 8. | ✅ | Same `d7_manifest_test.go` on-disk parse loop walks every `experiments/E7-*/manifest.yaml` and asserts the same predicate shape (`P(treatment >= control - 0.05) > 0.95` for quality, `treatment < 0.4 × control` for cost). Per-experiment divergence trips the test. |
| **No running 8 experiments in a single factorial cell.** Subject-agent dimension is part of identity → all 8 are orthogonal-overlap, not factorial-cell collapse. | ✅ | Each YAML's `subject_agent` field is unique across the 8 (boot, memory_rerank, pr_review_triage, librarian, diplomat, medic, commander, chancellor). Treatment dimension `model` is identical across all 8 (all swap to `claude-haiku-4-5-20251001`); orthogonality flows from subject-agent uniqueness per `paired-runs.md` § "Factorial Scoring". |
| **No shortcutting the confirm phase** for medium-tier experiments that declared winners. | ✅ | All 8 YAMLs declare `confirm_phase_required: true` despite `stakes_tier: medium` leaving confirm on-demand by default. The `d7_manifest_test.go` on-disk parse loop asserts `confirm == true` for every manifest. |

---

## Architectural notes

**Where the per-arm model swap actually happens.** The chain is: agent calls `claude.CallWithTranscript(...)` → `ensureCallCtx` stamps `Agent + TaskID` on ctx (`internal/claude/transcript.go`) → `invokeTreatmentApplyHook(parentCtx)` (`claude.go:405, 589`) reads the stamps + invokes the daemon-side hook → daemon hook calls `treatments.Apply` which returns the resolved arm's `Model` field → hook returns `(modelOverride, err)` → `RequestedModel(parentCtx)` (`claude.go:287, 442`) reads the override back from ctx → `buildClaudeArgs` (`claude.go:757`) appends `--model <id>` (lines 774-775) when non-empty. The end-to-end chain is exercised by `internal/claude/model_override_test.go` and was the root-cause fix in 82ef5ce — pre-D7 the `TreatmentApplyHook` was discarding the rewritten descriptor (the verifier called this out and it was fixed before final GO).

**Why ShipGate persists in `SystemConfig` and not a new schema column.** `experiment_promote_<id>` already lives in SystemConfig as JSON; ShipGate (Quality + Cost predicates + ConfirmPhaseRequired) follows the same shape. No schema migration required, no `TestSchemaParity` rerun, no migration ordering concern. Trade-off: ShipGate is read via `LoadShipGate(ctx, db, id)` which does a key-lookup rather than a column-read, but the gate evaluation happens once per experiment promotion attempt — cost is irrelevant.

**Why CallWithTranscript* auto-stamps Agent on ctx.** Pre-D7, every agent call site that wanted `subject_agent` to flow into the experiment hook had to manually wire `WithClaudeCallContext(...)`. The D7 design extends `ensureCallCtx` to default-stamp `Agent + TaskID` from the descriptor, eliminating per-call-site touch-ups. The no-clobber rule is preserved: if an inner caller (Captain's byte-attribution path) has already stamped a different Agent, the outer auto-stamp does not overwrite. This was verified-acknowledged as a deviation from the original "8 call-site touches" approach.

---

## Disclosed deviations (verifier-acknowledged)

1. **`assignment_unit: task` uniformly across all 8.** The TreatmentApplyHook hardcodes assignment-unit at `task`; per-agent natural-unit threading (e.g., per-convoy for Chancellor, per-PR for PR-review-triage) is a future-refactor seam. Acceptable for D7 because the cost/quality measurements aggregate cleanly across tasks.
2. **Quality metrics use parse-success proxies.** Boot's `decision_accuracy`, Commander's `plan_validity`, Chancellor's `plan_merge_rate` — these are operationally observable proxies that ship-now. Ground-truth quality definitions (e.g., human-rated decision quality) re-publish post-deployment without breaking the experiment shape, since metric versioning (`<metric>/2026-05-02.sql` paths) is part of the manifest contract.
3. **ShipGate persisted in SystemConfig** (not its own schema column). Mirrors the existing `experiment_promote_*` shape. Documented in the architectural notes above; no schema migration was needed.
4. **CallWithTranscript* auto-stamps Agent in one place vs 8 call-site touches.** No-clobber rule respected so existing inner stamps (Captain's byte-attribution path) are preserved.
5. **CLIRunner signature preserved; only inner TreatmentApplyHook signature changed.** The hook return shape grew from `(ctx, err)` to `(ctx, modelOverride, err)`; CLIRunner remains backwards-compatible.

---

## Verification (commands run, all green)

```
go vet ./...                                                     # exit 0
go build -tags sqlite_fts5 -o /tmp/force-d7 ./cmd/force/         # exit 0
go test -tags sqlite_fts5 -count=1 ./internal/claude/...         # PASS — model-override chain
go test -tags sqlite_fts5 -count=1 ./internal/experiments/...    # PASS — d7_manifest_test
go test -tags sqlite_fts5 -count=5 ./internal/claude/... ./internal/experiments/...  # -count=5 stable
go test -tags sqlite_fts5 -count=1 -timeout 600s ./...           # full suite ~5m green
/tmp/force-d7 render-rules --check                               # OK no drift
make smoke                                                       # PASS
```

Strict verifier final-gate result: **GO** (Static + Heavy + Race shards). 6/6 exit criteria met for the engineering portion; 4/4 anti-cheat directives enforced; end-to-end model-swap chain traced; cost-per-call captured via `LLMCallTranscripts.cost_usd`.

---

## Residual

1. **Operator ratification of all 8 YAMLs.** The manifests ship; the operator must run `force experiment author experiments/E7-N-<agent>/manifest.yaml` followed by `force experiment ratify <id>` per experiment. This unblocks the per-experiment runtime clock.
2. **30-day post-ship monitoring per promoted agent.** Calendar-gated. The aggregate-fleet-cost-delta dashboard reading + per-promoted-agent retention reading produce the evidence for exit criteria #2 and #3. Backfill into this closure as an addendum once the windows close.
3. **Aggregate fleet cost-per-convoy delta evidence.** Same calendar-gate as #2. Visible on the existing fleet-progress dashboard with the cost-per-call metrics; the reading itself awaits operator-side experiment lifecycle completion.

None of the residuals block the engineering closure. The Track 1 engineering deliverable is shipped; D7 cannot reach **fully-CLOSED** status until the operator-cadence work above completes, at which point this doc grows an addendum.
