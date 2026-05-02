# DELIVERABLE-5.5-CLOSURE.md — Staged Convoys (CLOSED)

**Date:** 2026-05-02
**Operator:** jake.herman@upstart.com
**Net verdict:** ✅ CLOSED. All six D5.5 phases (P0, P1, P2, P3, P4, P5) plus P5 fix-iter1 (emergency bypass + P-StagingPromotionConfirm audit) and P5 fix-iter2 (test-precondition repair) merged to `main`; final strict verifier shards all GO at HEAD `4462682` (Static cross-walk: 11/11 exit criteria + 6/6 anti-cheat directives; full `./...` -race -count=5 EXIT 0; targeted -race -count=5 across changed packages EXIT 0; full `make test` 5m10s green).

D5.5 ships staged convoys as the general-purpose primitive for any work that shouldn't land in a single PR — ZDM column dance, feature-flag rollouts, reviewer cognitive-load splits, capacity-aware enablement, soak-between-changes risk reduction, and parallel-work serialization. Single-PR convoys (today's behavior) remain the default; multi-stage is opt-in at planning time. The `staging_strategy` enum (`strict` only in D5.5; `merge_parallel` and `stacked` schema-recognized but planner-rejected with explicit "future deliverable" errors) keeps the schema forward-compatible.

---

## Per-phase tracking

| Phase | Description | Status | Merge SHA |
|---|---|---|---|
| P0 | Schema groundwork — `ConvoyStages` table (11 cols + UNIQUE), `ConvoyAskBranches.stage_id`, `Convoys.staging_mode`/`staging_strategy`, `Repositories.release_label_pattern`; forward-compat migration (existing convoys → `staging_mode='single'`, `staging_strategy='strict'`, single ConvoyStage at stage 1, `gate=null`); store helpers (`CreateStage`, `AdvanceStage`, `ListStages`, `GetStage`, `GetRepositoryReleaseLabelPattern`); baseline tests + forward-compat regression. | ✅ CLOSED | `e8e74a6` |
| P1 | Gate plug interface (`stagegate.Gate.Type()` + `Evaluate(ctx, db, stage StageContext)`); 5 baseline gates (`soak_minutes`, `operator_confirm`, `null`, `all_of`, `any_of`); registry pattern; `MaxNestingDepth=5` + empty-children + single-child validation; `convoy-stage-watch` dog skeleton. | ✅ CLOSED | `3ffb4d1` |
| P2 | `BountyBoard.stage_id` + Commander multi-stage planning prompt + JSON validator (`internal/agents/commander/staging_validator.go`) + ConvoyReview per-stage scoping + per-stage Senate review hook + astromech dispatch gating (SQL `stage_id IS NULL OR EXISTS(... cs.status != 'Pending')`) + Pattern P-StageGate full AST enforcement. | ✅ CLOSED | `50eece8`, `78985e3` |
| P3 | 4 advanced leaf gates (`probe_endpoint`, `release_label_present`, `datadog_metric_threshold`, `databricks_query_threshold`) + per-repo `release_label_pattern` planner-time enforcement (recursive into compound children) + `internal/clients/datadog` + `internal/clients/databricks` Pattern-P16 client interfaces + daemon-side gate registration + gate-timeout escalation. | ✅ CLOSED | `5fe0fd2` |
| P4 | Dashboard view (4 endpoints) + SPA stages modal + stage-transition notify-after pings (debounced) + audit-trail `LogStageAudit`/`ListStageAuditLog`. | ✅ CLOSED | `2016b45` |
| P5 fix-iter1 | Emergency stage bypass (`AUDIT-NNN <reason>` validator on advance handler + `store.BypassStage` helper + `AuditActionStageBypass` action constant) + Pattern P-StagingPromotionConfirm audit-test enforcing no production `store.SetConvoyStaging` callers without operator-confirm. Closes Static-shard NO GO on exit criterion #10 + anti-cheat directive B. | ✅ CLOSED | `c8daf3c` |
| P5 fix-iter2 | Test-precondition repair: 2 of 5 new bypass tests asserted seed stage 1 was `Pending` but `CreateStagedConvoy` lands stage 1 in `Open` immediately. Production code unchanged (bit-for-bit identical to iter1). | ✅ CLOSED | `4462682` |
| P5 walker patch | Aligned 4 audittools walkers (`P1_1`, `P20`, `P21`, `audittools_test×2`) with the canonical `.claude/.fix-worktrees` skip-list so verifier shards no longer surface false-positive offenders from agent isolation worktrees with divergent code. | ✅ CLOSED | `adf8e9d` |

---

## Per-gate-type status — 9 gates (5 baseline P1 + 4 advanced P3)

All 9 gate types implement the canonical `stagegate.Gate` interface (`Type() string` + `Evaluate(ctx, db, stage StageContext) (passed bool, reason string, err error)`) and register via the package's `Registry` such that compound gates (`all_of`/`any_of`) treat them uniformly. The gate-evaluator contract:

- `passed=true, err=nil` → stage flips to `GatePassed` next dog tick.
- `passed=false, err=nil` → stage flips to `Failed` (gate unambiguously rejected).
- `passed=false, err=ErrPending` → stage stays `AwaitingGate`; dog re-checks next tick.
- `passed=false, err=<other>` → structural error; dog logs + stays put.

| Gate type | File:line (`Type()` + `Evaluate`) | Default config | Test names | Tests count |
|---|---|---|---|---|
| **soak_minutes** (baseline) | `internal/stagegate/soak_minutes.go:28` + `:31` | `{minutes: int}` | `TestSoakMinutes_{Type,NotYetMerged_Pending,RemainingTime_Pending,Elapsed_Passed,InvalidConfig_Errors}` | 5 |
| **operator_confirm** (baseline) | `internal/stagegate/operator_confirm.go:29` + `:32` | `{prompt: string}` | `TestOperatorConfirm_{Type,NoConfirm_Pending,NoConfirm_NoPrompt_Pending,Confirmed_Passed,KeyScopedPerStage}` | 5 |
| **null** (baseline) | `internal/stagegate/null_gate.go:24` + `:27` | `{}` | `TestNullGate_{Type,AlwaysPasses}` | 2 |
| **all_of** (compound, baseline) | `internal/stagegate/compound.go:30` + `:37` | `{gates: [Gate]}` (max nesting 5; empty rejected at planner) | covered in `compound_test.go` (13 tests across `TestAllOf_*` + nested-depth + edge cases) | (13 shared with `any_of`) |
| **any_of** (compound, baseline) | `internal/stagegate/compound.go:83` + `:87` | `{gates: [Gate]}` (max nesting 5; empty rejected at planner) | covered in `compound_test.go` (`TestAnyOf_*`) | (13 shared with `all_of`) |
| **probe_endpoint** (advanced) | `internal/stagegate/probe_endpoint.go:63` + `:66` | `{url, method:GET\|POST, expected_status, body_match_regex?, timeout_seconds, target_env:prod\|staging, headers?}` | `TestProbeEndpoint_*` (11 tests) | 11 |
| **release_label_present** (advanced) | `internal/stagegate/release_label_present.go:82` + `:85` | `{polling_interval_minutes}` (regex on `Repositories.release_label_pattern`) | `TestReleaseLabelPresent_*` (9 tests) | 9 |
| **datadog_metric_threshold** (advanced) | `internal/stagegate/datadog_metric_threshold.go:63` + `:66` | `{metric_query, comparator:lt\|gt\|eq\|lte\|gte, threshold:float, sample_window_minutes}` | `TestDatadogMetricThreshold_*` (21 tests covering every comparator + ErrNoData/Transient/AuthFailure/OtherError + invalid-config + nil-client) | 21 |
| **databricks_query_threshold** (advanced) | `internal/stagegate/databricks_query_threshold.go:64` + `:67` | `{sql_query, comparator:lt\|gt\|eq\|lte\|gte, threshold:float, warehouse_id, timeout_seconds}` | `TestDatabricksQueryThreshold_*` (16 tests covering every comparator + ErrTransient/Timeout/AuthFailure/ShapeUnexpected + invalid-config + default-timeout backfill) | 16 |

**Compound gate semantics** (per `internal/stagegate/compound.go`):

- `MaxNestingDepth = 5` enforced at planner via recursive `validateGateSpec` (`internal/agents/commander/staging_validator.go`).
- Empty children (`all_of: []` or `any_of: []`) rejected at planner. `staging_validator_test.go:263` (`EmptyChildren_Errors`).
- `all_of` short-circuits on first child failure → `passed=false`. Compound returns `ErrPending` while ANY child still pending.
- `any_of` returns `passed=true` on first child pass. Returns `passed=false` only when ALL children explicitly fail.
- Per-child timeouts NOT supported; the convoy stage's `gate_timeout_minutes` applies to the whole compound.

**Pattern P16 compliance** — the two service interfaces (`internal/clients/datadog/` and `internal/clients/databricks/`) follow the cross-agent service-interface convention: exported `Client interface` (NOT a struct), unexported implementation struct, `NewInProcess(...)` factory, sentinel errors (`ErrNoData`, `ErrTransient`, `ErrAuthFailure`, `ErrShapeUnexpected`, `ErrTimeout`, `ErrConfig`). `TestPattern_P16_ClientsInterfaces` PASS.

---

## Forward-compat audit

The forward-compat migration (`internal/store/schema.go` D5.5 block, lines 1670-1727) is exercised by 6 explicit migration tests (all PASS):

- `TestMigration_ExistingConvoy_GetsSingleStage` — pre-D5.5-shaped convoy gets exactly one ConvoyStage row at stage 1 with `gate_type=NULL`, `status='Open'`.
- `TestMigration_StagingMode_DefaultsToSingle` — `staging_mode='single'`, `staging_strategy='strict'` for every existing convoy.
- `TestMigration_Idempotent` — running the migration 3× produces no new rows or column drift.
- `TestMigration_FreshDB_NoOps` — fresh `createSchema` doesn't double-write (P0's create + migration agree).
- `TestMigration_NewConvoyAfterMigration_GetsStageOnReinit` — convoys created post-migration land cleanly.
- `TestMigration_ExistingNonNullStageId_NotOverwritten` — `stage_id` backfill on `ConvoyAskBranches` is idempotent.

`TestSchemaParity` (`internal/store/schema_parity_test.go:26`) PASS — `createSchema` + `runMigrations` + `schema/schema.sql` all agree on the new columns and table. The `staging_strategy` column carries no SQL CHECK constraint; enforcement lives at the agent layer (`internal/agents/commander/staging_validator.go:339-350`) so future enum values land via prompt+validator update, not destructive ALTER.

Single-stage convoys exhibit zero behavior change post-D5.5 — `TestConvoyStageWatch_LegacySingleStageNullGate_NoOp` confirms the watchdog ignores legacy convoys' implicit null-gate stage 1.

---

## Commander integration

Planning-prompt extension at `internal/agents/commander.go:468-498` (staged-mode JSON shape inserted into the planning prompt). The Commander reasons about:

- *Why* each stage is independently safe.
- *What* the gate verifies.
- *What* the rollback story is per stage.

Output validated by `commander.ValidateStagingPlan` (`internal/agents/commander.go:561-571`); multi-stage rows land via `runCommanderTask` (`internal/agents/commander.go:579-581`). 30+ validator tests in `internal/agents/commander/staging_validator_test.go` cover:

- `staging_strategy` rejects `merge_parallel` / `stacked` with explicit "not yet supported in D5.5" error naming the future deliverable (`StagedMergeParallel_RejectsWithExplicitError`, `StagedStacked_RejectsWithExplicitError`).
- Compound nesting depth cap enforced (`CompoundGate_NestingDepthExceeded_Errors`, `DeepButValid_OK`).
- Empty children + single-child handling (`EmptyChildren_Errors`).
- Null-gate allowed only on terminal stage (`NullGateOnNonTerminalStage_Errors`, `NullGateOnTerminalStage_OK`).
- Per-repo `release_label_pattern` enforcement recursive into compound children (`ReleaseLabelGate_AllReposHavePattern_OK`, `_ReposLackPattern_Rejects`, `_NestedInCompound_Rejects`).

---

## ConvoyReview per-stage scoping

`runConvoyReview` in `internal/agents/convoy_review.go:442-547` scopes each pass to the convoy's currently in-flight stage:

1. Resolves the active stage via `store.CurrentInFlightStage(db, convoyID)` (line 442) — lowest-numbered stage in `{Open, AllPRsMerged, AwaitingGate, GatePassed}`.
2. Filters `ListConvoyAskBranches` by `stage_id` (line 452) so the diff only includes the stage's branches.
3. Prefixes the LLM prompt with `ConvoyStages.intent_text` (line 543) so the review reads the stage's stated goal.

End-to-end coverage (`internal/agents/convoy_review_per_stage_test.go`):

- `TestConvoyReview_StagedMode_ScopedToCurrentStage` — 3-stage convoy, run on stage 2; only stage 2's diff reaches the LLM.
- `TestConvoyReview_StageN_BaseIsPriorMerge` — stage N's review base is post-stage-(N-1)-merge.
- `TestConvoyReview_PerStageSenateHook_Fires` + `_FiresOnNeedsWork` — Senate review wires up at each stage's DraftPROpen.

---

## Astromech dispatch gating (Pattern P-StageGate)

Dispatch-time gate at the SQL layer (`internal/store/tasks.go:129-132` in `ClaimBounty`, `:201-204` in `ClaimBountyForWrite`):

```sql
... AND (stage_id IS NULL
         OR EXISTS (SELECT 1 FROM ConvoyStages cs
                    WHERE cs.id = BountyBoard.stage_id
                      AND cs.status != 'Pending'))
```

Pattern P-StageGate (`internal/audittools/audit_pattern_p_stage_gate_test.go`) is a 4-test AST audit:

- `TestPattern_PStageGate_PackageWiringPresent` — `internal/stagegate` package exists.
- `TestPattern_PStageGate_ClaimBountyHasStageFilter` — every `Claim*` function in `internal/store/tasks.go` carries the gating predicate.
- `TestPattern_PStageGate_NoUngatedClaimSQL` — no other production file contains an ungated CLAIM-shaped SQL (Pending+SELECT+LIMIT 1 + sibling Locked UPDATE in same file).
- `TestPattern_PStageGate_ClaimPathSurfaceProbed` — surface-test for gate predicate matching.

All 4 PASS.

`stageGateBypassAllowlist` is empty — the only CLAIM-shaped SQL in the tree (`internal/store/tasks.go`) is itself gated.

---

## Dashboard surface

4 endpoints + SPA wiring (D5.5 P4) + emergency bypass (D5.5 P5 fix-iter1):

| Verb | Path | Handler | Purpose |
|---|---|---|---|
| GET | `/api/convoys/<id>/stages` | `internal/dashboard/handlers_staged_convoys.go:170-191` (`listStages`) | List convoy's ConvoyStages (1 row for single-mode convoys, N rows for staged). |
| GET | `/api/convoys/<id>/stages/<num>` | `:195-264` (`getStageDetail`) | Stage row + ask-branches (filtered by `stage_id`) + per-branch sub-PRs + audit-log history. |
| POST | `/api/convoys/<id>/stages/<num>/advance` | `:272-374` (`advanceStageHandler`) | Two modes: **(a) normal advance** — writes `SystemConfig.stage_advance_<convoy>_<stage>` rendezvous key for the `operator_confirm` gate; **(b) emergency bypass** — when `audit_id` matches `^AUDIT-\d+$`, calls `store.BypassStage` to flip status directly to `GatePassed` regardless of gate type, then `LogStageAudit(action=stage_bypass)` carrying both AUDIT id and reason in detail. Required by D5.5 exit criterion #10. |
| POST | `/api/convoys/<id>/stages/<num>/abort` | `:380-444` (`abortStageHandler`) | Forces stage to `Failed` (terminal); convoy itself stays in `DraftPROpen`. |

All 4 method-gated (`http.StatusMethodNotAllowed` for wrong verbs); all require non-empty `operator` + `reason` payload; bypass additionally validates `^AUDIT-\d+$` BEFORE any state mutation so malformed IDs never reach the audit trail. Routing wired at `internal/dashboard/handlers.go:929-937` → `dashboard.go:45`.

SPA wiring at `internal/dashboard/static/index.html:778-820` (stages-modal + stage-action-modal) and `internal/dashboard/static/app.js:1294, 1574-1737` (`showConvoyStages`, `renderStagesPanel`, `toggleStageHistory`, `openStageActionModal`, `confirmStageAction`).

Stage-transition notify-after at `internal/agents/dogs_convoy_stage_watch.go:535-579` (`onStageTransition`), called from all 5 dog-driven transitions (Open→AllPRsMerged, AllPRsMerged→AwaitingGate, AwaitingGate→GatePassed, AwaitingGate→Failed timeout, AwaitingGate→Failed gate-fail). Debounce key `stage_transition_notified_<convoy>_<stage>_<new_status>` prevents re-pings.

Audit trail via `LogStageAudit` + `ListStageAuditLog` (`internal/store/convoy_stages.go:364-428`) — re-uses `AuditLog` table, action prefix `stage_*`, 4 actions: `stage_advance`, `stage_abort`, `stage_auto_advance`, `stage_bypass`.

---

## Anti-cheat self-check

| # | Directive | Enforcement (file:line) | Test (file:line) | Status |
|---|---|---|---|---|
| A | **No silent gate skip** — `gate_passed_at` flips ONLY after a real gate evaluation. | Sole `gate_passed_at` writer in production: `internal/store/convoy_stages.go:175` (`AdvanceStage`, only on `StageStatusGatePassed`). Sole production caller for the dog-driven path: `internal/agents/dogs_convoy_stage_watch.go:297`, gated by `if passed { ... }` from `evaluateGate(...)` at line 285. The new bypass path (`store.BypassStage` at `convoy_stages.go:215`) ALSO writes `gate_passed_at` BUT only via the explicit `^AUDIT-\d+$`-gated handler branch — every bypass leaves a `stage_bypass` audit row carrying the AUDIT id, so the durable trail distinguishes "real gate pass" from "operator bypass." | `dogs_convoy_stage_watch_test.go:196,242`; `convoy_stages_test.go:TestBypassStage_*` (4 tests) | ✅ |
| B | **No post-hoc single→multi promotion without operator confirm** — `staging_mode` mutator audit. | `store.SetConvoyStaging` is the sole production mutator of `staging_mode`. Pattern P-StagingPromotionConfirm AST-walks production `.go`, rejects any unallowlisted call site. | `internal/audittools/audit_pattern_p_staging_promotion_confirm_test.go` — `TestPattern_PStagingPromotionConfirm_NoUngatedSetConvoyStaging`. Allowlist empty — zero production callers today. | ✅ |
| C | **No null-gate non-terminal** — `gate_type=null` allowed only on terminal stage. | `internal/agents/commander/staging_validator.go:212-217` rejects `Gate==nil` on non-terminal stages with explicit error mentioning `gate=null` + `terminal` + anti-cheat. | `staging_validator_test.go:200,220` (`NullGateOnNonTerminalStage_Errors`, `NullGateOnTerminalStage_OK`) | ✅ |
| D | **No skip-stage out-of-order** — out-of-order merges trigger escalation, NOT silent advance. | Dog query (`dogs_convoy_stage_watch.go:204`) walks only `status IN ('Open','AllPRsMerged','AwaitingGate')` — `Pending` stages never advance. Astromech claim SQL (`internal/store/tasks.go:129-132`) blocks dispatch on Pending stages. | `dogs_convoy_stage_watch_test.go:294` (`LegacySingleStageNullGate_NoOp`) | ✅ |
| E | **No astromech pre-staging** — astromechs cannot hold a worktree on `Pending` stage. | Pattern P-StageGate AST audit (4 tests, see "Astromech dispatch gating" section above). | `audit_pattern_p_stage_gate_test.go:95,180,295,434` | ✅ |
| F | **No Slack-message-triggers-stage-advance** — only operator dashboard action or real gate evaluation can advance. | The two Slack-touching code paths in `dogs_convoy_stage_watch.go` (`emitGateTimeoutEscalation` at line 454, `onStageTransition` at line 535) are **read-side** — both fire AFTER `store.AdvanceStage` succeeds. There's no inbound webhook handler that mutates ConvoyStages. | Verified by grep audit: no `mux.HandleFunc.*stages.*POST` outside the dashboard `/advance|/abort` endpoints. | ✅ |
| **+1** (new) | **Bypass mechanism (D5.5 exit criterion #10)** — emergency stage advancement is operator-only and durable. | `^AUDIT-\d+$` regex enforced at `internal/dashboard/handlers_staged_convoys.go:121,320-324` BEFORE any stage lookup. Reason field still required. Bypass writes `stage_bypass` audit row carrying both AUDIT id and reason. Works regardless of gate type. | `handlers_staged_convoys_test.go:TestAdvanceStageHandler_Bypass_*` (5 tests covering happy path, from-Pending, malformed AUDIT ids × 6 cases, requires-reason, terminal-rejected) + `convoy_stages_test.go:TestBypassStage_*` (4 store-level tests) | ✅ |

---

## Verification evidence

**Strict verifier shards (final, against HEAD `4462682`):**

| Shard | Result |
|---|---|
| Static (cross-walk) | GO — 11/11 exit criteria + 6/6 anti-cheat + bypass mechanism + P-StagingPromotionConfirm; build/vet/render-rules clean |
| Original race (`go test -race -count=5 ./...`, full tree) | EXIT 0 (clean against `adf8e9d`; production unchanged thereafter) |
| Targeted race (`-race -count=5` across changed packages) | EXIT 0 (against `4462682`: store, dashboard, audittools, stagegate) |
| Full `make test` | PASS, 5m10s wall |
| `make smoke` | PASS, 14s |
| `go vet ./...` | exit 0 |
| `./force render-rules --check` | OK no drift |

**Walked-but-not-failing systemic noise (out of scope; logged for backlog):** during full-suite run the dashboard package emitted two `calibration_queries.go:LoadCalibrationScoreboard` `Scan error: converting NULL to int is unsupported` log lines from a pre-existing query that doesn't COALESCE empty calibration tables. Tests pass; the log noise is a latent issue independent of D5.5 and should be addressed in a separate fix.

---

## Residual

**NONE blocking.** Every roadmap-mandated D5.5 P5 exit criterion + every anti-cheat directive is enforced at the source level with corresponding test coverage. Three forward-compat hooks remain explicitly enumerated in the schema and validators:

1. `staging_strategy='merge_parallel'` — schema-recognized; planner emits `"forward-compat-recognized but not yet implemented in D5.5; will land in <future deliverable>"`. Promotion when needed = prompt update + validator switch + dog branch on `staging_strategy` for stage-open trigger; no schema migration.
2. `staging_strategy='stacked'` — schema-recognized; planner emits the same shape error. Promotion adds an inter-stage rebase dog (`convoy-stage-rebase`) and may add `ConvoyStages.base_commit_sha` + `ConvoyStages.rebase_attempts` (forward-compat-clean ALTER ADD COLUMN).
3. `StageStatusVerified` — defined and reachable via `AdvanceStage`, but no production code transitions `GatePassed → Verified` today. Per-stage ConvoyReview is the natural caller (a future deliverable will mark a stage `Verified` once ConvoyReview passes for that stage). Single-stage convoys remain unaffected because they never enter `GatePassed` via the dog path (their implicit stage 1 stays in `Open`).

The bypass mechanism's `AuditActionStageBypass` audit rows are durable and searchable — incident retros can `SELECT * FROM AuditLog WHERE action='stage_bypass'` to enumerate every emergency cut-through.
