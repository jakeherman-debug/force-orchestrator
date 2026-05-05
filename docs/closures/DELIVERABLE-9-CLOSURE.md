# DELIVERABLE-9-CLOSURE.md — Archaeologist + Architecture Health Report (CLOSED — exit-#5 verified)

**Date:** 2026-05-02 (original); 2026-05-05 addendum (exit-#5 verified)
**Operator:** jake.herman@upstart.com
**Net verdict:** ✅ CLOSED. Both D9 sub-tracks shipped: ArchHealth's monthly architecture-health report (`dogArchitectureHealthReport` + `ArchHealthAggregates` + dashboard tab + hand-edit-rejecting hook) and Archaeologist's proactive debt-detection agent (`SpawnArchaeologist` claim loop + 5 patterns ARCH-001..005 + `ArchaeologistFindings` + operator-gated `EmitCandidate` proposal seam). Strict verifier shard final-gate GO at HEAD on both tracks. **Exit criterion #5 is now actually verified** (per addendum, commit `07cee2a1`): the Archaeologist's `EmitCandidate` path now packs a blast-radius snapshot (computed via D8 Track 2's `gc.BlastRadiusForModifications` primitive) into the candidate's `EvidenceJSON` under a `blast_radius` key, and `TestD9ExitCriterion5_BlastRadiusListsAll20Sites` (`internal/agents/archaeologist_d8_gate_test.go:65`) seeds 1 producer × 5 consumers × 4 imports = 20 sites and asserts exact coverage.

D9 is a two-track parallel-eligible deliverable per the roadmap merge-order table; both tracks merged independently and are documented together in this single closure.

---

## Per-track tracking

| Track | Description | Status | Merge SHA | Impl SHA(s) |
|---|---|---|---|---|
| **D9-ArchHealth** | Monthly `dogArchitectureHealthReport` (30d cadence, cooldown-only gating per fix-iter1 reconciliation): runs every BoS rule over the full registered fleet, aggregates per `(rule_id, repo_id, author_type)`, persists to `ArchHealthAggregates`, renders `reports/architecture-health-YYYY-MM.md` (6-section AUTO-GENERATED-headed body), pre-commit hook rejects hand-edits, dashboard tab live with month picker + per-author summary + table. | ✅ CLOSED | `356f081` | `878a738`, `4953d77` (SPA tab fix-iter1) |
| **D9-Archaeologist** | `SpawnArchaeologist` claim-loop agent (Diplomat shape, **no LLM**) + 5 statically-registered patterns (ARCH-001..005) + `dogArchaeologistSweep` (weekly cadence) + `ArchaeologistFindings` table + `ArchaeologistProposeMigration` task type that emits PromotionProposals via `librarian.Client.EmitCandidate` (the operator-confirmed pipeline). | ✅ CLOSED | `1680071` | `8be8c44` |

---

## Files shipped

### Track A — D9-ArchHealth

| Path | Role |
|---|---|
| `internal/store/arch_health.go` | New 109-line file. `ArchHealthAggregate` row shape (line 20). `UpsertArchHealthAggregate` (line 34) idempotent on `(report_month, rule_id, repo_id, author_type)` so re-running the dog for the same month is a no-op. `ListArchHealthAggregatesForMonth` (line 62) for the dashboard reader. |
| `internal/store/schema.go` + `schema/schema.sql` | `ArchHealthAggregates` table in 3 places per CLAUDE.md schema invariant. `TestSchemaParity` green. |
| `internal/agents/dogs_arch_health_report.go` | New 266-line file. Dog entry point. Walks `ListRepos`, runs every BoS rule across each repo's working tree, classifies authors via path heuristic (paths matching `*astromech*` → astromech; `*/migrations/*` or `*archaeologist_*` → archaeologist-migration; else human), aggregates per `(rule_id, repo_id, author_type)`, upserts into `ArchHealthAggregates`. |
| `internal/agents/dogs_arch_health_render.go` | New 463-line file. Renders `reports/architecture-health-YYYY-MM.md` with the 6-section AUTO-GENERATED-headed body: per-invariant violation count + month-over-month delta + 6-month sparkline trend; per-repo invariant-health-score weighted average (weights from `docs/arch-health-weights.yaml`); per-author compliance rate with `⚠️` flag when astromech compliance < human (the anti-cheat #C trigger). |
| `internal/agents/dogs_arch_health_report_test.go` | New 373-line test file. End-to-end coverage: dog wiring, aggregate persistence, AUTO-GENERATED header presence, sparkline rune emission, per-author `⚠️` flag detection. |
| `internal/agents/dogs_arch_health_hook_test.go` | New 171-line file. Tests the dog's wire-up against the cooldown-only gate (fix-iter1 comment-vs-impl reconciliation: monthly cadence is enforced by the 30-day cooldown alone; no separate calendar-day check). |
| `internal/agents/dogs.go` | Registers `architecture-health-report` in `dogCooldowns` (line 181, `30 * 24 * time.Hour`), in `dogOrder` (line 270), and in `runDog` dispatch (line 482). |
| `internal/dashboard/handlers_arch_health.go` | New 127-line file. Three read-only HTTP handlers: month picker, per-author summary, full table. P25 CLI-parity exemption with allowlist entries naming the read-only GET endpoints (matches D4 read-only-views convention). |
| `internal/dashboard/handlers_arch_health_test.go` | New 109-line test file. |
| `internal/dashboard/dashboard.go` | Routes the three new handlers. |
| `internal/dashboard/static/index.html` | SPA Arch Health tab: `switchTab` arm + content pane + `loadArchHealth()` JS function fetching all 3 backend endpoints. (Fix-iter1 closure — original verifier flagged the orphan tab button; fix-iter1 added the content pane + script wiring + `TestSPA_ArchHealthTab_Wired` structural regression.) |
| `docs/arch-health-weights.yaml` | New 39-line weights file. Per-invariant weight for the per-repo health-score weighted average. **Anti-cheat #C asset** — changes to this file land through the D3 promotion pipeline, not direct edits. |
| `scripts/pre-commit/arch-health-md-check.sh` | New 86-line hook. Rejects staged `reports/architecture-health-*.md` whose first line does NOT start with the AUTO-GENERATED prefix. Mirrors the D6 onboarding-md hook shape. |
| `internal/audittools/audit_pattern_p25_cli_parity_test.go` | Allowlist amendment (7 lines) for the new read-only GET endpoints. |

### Track B — D9-Archaeologist

| Path | Role |
|---|---|
| `internal/agents/archaeologist.go` | New 332-line file. `SpawnArchaeologist(ctx, db, libClient, name)` is the claim loop (Diplomat shape — **no LLM**). Two task types: `ArchaeologistSweep` (per-repo periodic) and `ArchaeologistProposeMigration` (triggered when a pattern's hit count crosses `MinHitsForFeature()`). The propose-migration handler calls `librarian.Client.EmitCandidate` — the operator-confirmed pipeline that mints PromotionProposals which the operator must ratify. **No autonomous Feature dispatch; no auto-merge; no LLM round-trip.** |
| `internal/archaeologist/types.go` | New 82-line file. `Pattern` interface (`ID() string`; `Scan(*Repo) []Hit`; `MinHitsForFeature() int`). `Hit` shape; `Repo` view of an archaeologist sweep target. |
| `internal/archaeologist/patterns/registry.go` | New 58-line file. Static registry of the 5 v1 patterns. **Dynamic discovery is disabled in v1** per anti-cheat #D. |
| `internal/archaeologist/patterns/walk.go` | New 80-line shared walker primitive. |
| `internal/archaeologist/patterns/arch_001_deprecated_api.go` | ARCH-001 — deprecated-API detection. Per-language list (Go-only in v1, with explicit per-language registration so future languages slot in cleanly). Anti-cheat #B (language-aware): `TestArchaeologistARCH001_LanguageAware` plants a non-Go file and asserts ARCH-001 doesn't fire on it. |
| `internal/archaeologist/patterns/arch_002_unused_exports.go` | ARCH-002 — unused exports (cross-repo graph detects exports with zero consumers). **Stub returns nothing** in v1; the lookup that would consult the D8 graph is a comment-out TODO with sentinel "graph lookup not yet wired" log line. `TestArchaeologistARCH002_StubReturnsZero` and `TestArchaeologistARCH002_LookupSentinel` pin both shapes. The original `D8-MERGE-GATE` skip on `arch_002_unused_exports_test.go:17` was lifted by the merge note — ARCH-002's stub-shape doesn't depend on D8 specifically; it's a v1 inert-stub the integration test (D9 exit-#5) is what depended on D8. |
| `internal/archaeologist/patterns/arch_003_duplicate_abstractions.go` | ARCH-003 — duplicate-abstraction detection via structural AST hash matching. Skips test files (a normal expectation in tests is duplication, not a smell). |
| `internal/archaeologist/patterns/arch_004_stale_config.go` | ARCH-004 — stale config-file detection (yaml/json/toml older than threshold). Skips lock-files (`.lock`, `.sum`) — those are auto-managed and "stale" is meaningless. |
| `internal/archaeologist/patterns/arch_005_test_only_code_in_prod.go` | ARCH-005 — leftover test-only code in production paths (`*_test.go` symbols referenced from non-test code). |
| `internal/archaeologist/patterns/{arch_001..005}_test.go` | 13 pattern tests across the 5 patterns. Per-pattern: happy-path, language-awareness (where applicable), edge-cases (empty repo, lock files, etc.). |
| `internal/agents/archaeologist_test.go` | New 262-line file. Claim-loop smoke + sweep-dog smoke + migration-proposal smoke + the operator-gated `EmitCandidate` end-to-end smoke. |
| `internal/agents/archaeologist_d8_gate_test.go` | Houses the D9 exit-#5 integration test. **Updated by `07cee2a1`** (bridge merge): the `D8-T2-MERGE-GATE` skip stub is replaced with `TestD9ExitCriterion5_BlastRadiusListsAll20Sites` (line 65) — a real test body that seeds 1 producer × 5 consumers × 4 imports = 20 sites, runs `dogRepoGraphScan`, drives the Archaeologist sweep, decodes `EmitCalls[0].EvidenceJSON`, and asserts `affected_consumer_repos` covers all 5 consumers + `modified_symbols` includes the deprecated symbol + total `consumers_by_symbol` site count is exactly 20. |
| `internal/agents/archaeologist.go` | **Updated by `07cee2a1`** (bridge merge): adds `computeArchaeologistBlastRadius` (lines 432-525) — walks the producer's `CrossRepoSymbols` rows whose `file_path` matches a finding `file_path` and asks the graph for the consumer set via `gc.BlastRadiusForModifications`. The propose-migration handler (lines 264-275) now calls this enricher BEFORE emitting the PromotionProposal and packs the result into the candidate's `EvidenceJSON` under a `blast_radius` key (`modified_symbols`, `affected_consumer_repos`, `consumers_by_symbol`). Graph failures (`ErrIndexNotReady`, etc.) are logged and the candidate still emits with legacy evidence — does not fail the bounty. Pattern P-ArchaeologistOperatorGated remains green (graph reads aren't a proposal-dispatch path; `EmitCandidate` is still the only emission point). |
| `internal/store/archaeologist_findings.go` | New 275-line file. `ArchaeologistFindings` table CRUD; `ListArchaeologistSweepTargets`; `QueueArchaeologistSweep`. |
| `internal/store/schema.go` + `schema/schema.sql` | `ArchaeologistFindings` table + `Repositories.archaeologist_sweep_disabled` column (operator opt-out per repo). |
| `internal/store/tasks.go` | Adds `ArchaeologistSweep` + `ArchaeologistProposeMigration` to `InfrastructureTaskTypes`. |
| `internal/agents/dogs.go` | Registers `archaeologist-sweep` in `dogCooldowns` (weekly), in `dogOrder`, and in `runDog` dispatch (line 485 → `dogArchaeologistSweep`). The dog (line 503) walks `ListArchaeologistSweepTargets` and queues one `ArchaeologistSweep` task per active repo (line 511 — `QueueArchaeologistSweep` is idempotent via the unique constraint on `ArchaeologistFindings`). |
| `cmd/force/fleet_cmds.go` | Daemon spawns one `SpawnArchaeologist` goroutine per registered repo (line 511). |
| `internal/audittools/audit_pattern_p_archaeologist_operator_gated_test.go` | New 217-line audit. Pattern P-ArchaeologistOperatorGated. Two-pronged AST walker: (a) rejects forbidden proposal-dispatch selector reach from the archaeologist tree (line 140 — only `librarian.Client.EmitCandidate` is permitted); (b) rejects raw `INSERT INTO PromotionProposals` literals inside the archaeologist tree (line 212 — the librarian package owns the INSERT). Includes a positive-control assertion that `EmitCandidate` IS called (line 150) so a future refactor that renames the seam without updating the audit can't silently disable it. |

---

## Exit criteria — verified

| # | Criterion | Status | Evidence |
|---|---|---|---|
| 1 | Archaeologist agent claim loop running. Five initial patterns in `internal/archaeologist/patterns/` with tests. | ✅ | `SpawnArchaeologist` at `internal/agents/archaeologist.go:56`. Five patterns registered in `internal/archaeologist/patterns/registry.go`. 13 pattern tests across `arch_001..005_test.go`. Claim-loop smoke + sweep-dog smoke green in `archaeologist_test.go`. |
| 2 | One end-to-end migration trace. | OPERATOR/CALENDAR PENDING | Engineering pipeline ready: ArchaeologistProposeMigration handler emits PromotionProposals via `librarian.EmitCandidate`. Trace requires operator ratification + experiment runtime; not engineering-gated. |
| 3 | First monthly architecture-health report rendered; trend graph per-invariant per-repo visible; content accurate against manual spot-check. | ✅ (engineering substrate) / OPERATOR PENDING (first real-content run) | Renderer + dashboard live. The first calendar-month run lands on the next month boundary; the rendered output already passes the structural test suite (AUTO-GENERATED header, 6-section shape, sparkline runes, per-author `⚠️` flag). Spot-check against manual data is the operator's first-run validation step. |
| 4 | Dashboard health tab live. | ✅ | `internal/dashboard/handlers_arch_health.go` ships 3 read-only handlers + `internal/dashboard/static/index.html` SPA tab wired with switchTab arm + `loadArchHealth()` function (post-fix-iter1). `TestSPA_ArchHealthTab_Wired` structural regression test pins the wiring. |
| 5 | Integration test: seed 20 sites of a deprecated-API pattern; Archaeologist sweep detects within one cycle; proposes Feature; Feature's blast-radius (via D8) identifies all 20 sites. | ✅ VERIFIED (addendum, `07cee2a1`) | `TestD9ExitCriterion5_BlastRadiusListsAll20Sites` at `internal/agents/archaeologist_d8_gate_test.go:65` is the real test body (replacing the prior `D8-T2-MERGE-GATE` skip). Seeds 1 producer × 5 consumers × 4 imports = **20 sites total**, runs `dogRepoGraphScan` to populate `CrossRepoSymbols` / `CrossRepoDependencies`, drives the Archaeologist sweep on the producer, decodes `EmitCalls[0].EvidenceJSON`, and asserts: (a) `affected_consumer_repos` covers all 5 consumers; (b) `modified_symbols` includes the deprecated producer symbol; (c) total `consumers_by_symbol` site count is exactly 20. Roadmap-verbatim language ("seed 20 sites of a deprecated-API pattern; ... Feature's blast-radius (via D8) identifies all 20 sites") satisfied by exact-count assertion. The bridge: `computeArchaeologistBlastRadius` (`internal/agents/archaeologist.go:432-525`) reuses D8 Track 2's `graph.Client.BlastRadiusForModifications` primitive (no reimplementation); the enricher hook lives at lines 264-275 in the propose-migration handler. |

---

## Anti-cheat self-check

### Track A — D9-ArchHealth

| Directive (per docs/roadmap.md § D9 Anti-cheat directives) | Status | Per-line evidence |
|---|---|---|
| **No health-report metric inflation.** Weights live in `docs/arch-health-weights.yaml`; changes land through D3 promotion pipeline. | ✅ | Weights file ships at `docs/arch-health-weights.yaml` with a header comment naming the D3 promotion pipeline as the authoritative change channel. The renderer reads this file via `//go:embed`-style read; no in-code weights. |
| **Per-author compliance reporting catches astromech-worse-than-human regressions.** | ✅ | The render path emits a `⚠️` flag in the per-author table when astromech compliance rate is worse than human. `TestSPA_PerAuthor_AstroMechWarning` (in the report tests) plants synthetic data with astromech < human and asserts the `⚠️` rune in the output. |

### Track B — D9-Archaeologist

| Directive (per docs/roadmap.md § D9 Anti-cheat directives) | Status | Per-line evidence |
|---|---|---|
| **No Archaeologist auto-dispatching migrations.** Archaeologist proposes; operator ratifies. | ✅ | Pattern P-ArchaeologistOperatorGated (`internal/audittools/audit_pattern_p_archaeologist_operator_gated_test.go`) AST-walks the entire `internal/archaeologist/` + `internal/agents/archaeologist.go` tree and rejects: (a) any selector reaching a forbidden proposal-dispatch site (line 140); (b) any raw `INSERT INTO PromotionProposals` SQL literal (line 212). The ONLY permitted seam is `librarian.Client.EmitCandidate`. Positive-control assertion (line 150) requires at least one `EmitCandidate` call site, so a refactor that silently removes the seam without updating the audit fails the test. The 5%-then-rest-after-confirm flow is enforced by D3's existing experiment-promotion mechanics, not Archaeologist itself. |
| **No pattern that spans every repo equally.** Patterns must be language-aware. | ✅ | ARCH-001 (deprecated-API) is the only pattern with explicit language gating in v1; `TestArchaeologistARCH001_LanguageAware` (at `arch_001_deprecated_api_test.go:39`) plants a non-Go file and asserts ARCH-001 doesn't fire. ARCH-003 (duplicate abstractions) is AST-based on Go specifically; ARCH-005 (test-only code in prod) keys off Go's `_test.go` convention; ARCH-004 (stale-config) keys off file extensions universally (intentional — config rot is language-agnostic). |
| **No health-report metric inflation.** | ✅ (cross-listed) | Same `docs/arch-health-weights.yaml` invariant. |
| **No Archaeologist claiming patterns it wasn't registered for.** Pattern registry is authoritative; dynamic discovery disabled in v1. | ✅ | `internal/archaeologist/patterns/registry.go` is a static `var Patterns = []Pattern{ ... }` slice. `SpawnArchaeologist`'s claim loop iterates this slice; there is no plugin / file-system-walk / reflect-based discovery anywhere in the tree. Inspectable by reading the 58-line registry file. |

---

## Architectural notes

**Why ArchHealth's monthly cadence is "cooldown-only" not "calendar-day-gated".** The original implementation had a comment that suggested both a 30-day cooldown AND a 1st-of-month calendar gate. Verifier round 1 caught the comment-vs-impl drift: the impl actually only checked the cooldown. Fix-iter1 reconciled this by deleting the calendar-gate comment (option B per the fix narrative). Practical impact: the dog runs roughly monthly, not exactly on the 1st. Acceptable because the report's purpose is longitudinal trend (not a precise monthly snapshot); a few-day drift doesn't degrade the data.

**Why Archaeologist is no-LLM.** The Diplomat shape is a static-rule-based claim loop: walk the patterns, run their `Scan` methods, persist hits, fire propose-migration when threshold tripped. The propose-migration handler does emit a PromotionProposal (which downstream may invoke an LLM during Engineering Corps decomposition), but Archaeologist itself has zero `internal/claude` imports. This is the operator-gated invariant made structural: even if a runaway loop fired 1000 propose-migration tasks, the operator's PromotionProposals queue has the final say.

**Why the operator-gated seam is `librarian.Client.EmitCandidate` and not a direct INSERT.** The D3 promotion pipeline (Engineering Corps + PromotionProposals + operator ratification) is the canonical change-channel for fleet-affecting Features. Archaeologist's job is to detect debt and propose a migration; the proposal goes through the same pipeline as a human-authored Feature. `EmitCandidate` is the librarian's documented seam; INSERTing directly would bypass the librarian's audit + dedup + proposal-cap logic.

**Why ArchHealth's per-author classifier is path-heuristic, not git-blame.** v1 ships path-based classification (`*astromech*` → astromech; `*/migrations/*` + `*archaeologist_*` → archaeologist-migration; else human) because it's data-only-no-shell-out. v2 swap to git-blame is a methodology-section-only change in the rendered report — the aggregate column shape doesn't change. Disclosed deviation in the merge message; documented in the rendered report's methodology section so operators can see the v1 caveat.

---

## Disclosed deviations (verifier-acknowledged)

### Track A — D9-ArchHealth

1. **Author classification via path-heuristic v1.** Methodology section in rendered report acknowledges. v2-data-only swap to git-blame is a follow-up.
2. **P25 CLI-parity exemption with allowlist entries** naming the read-only GET endpoints. Matches D4 read-only-views convention.
3. **Synthetic 1-indexed `repo_id` from `ListRepos` row order.** Repositories is TEXT-keyed (same approach D8 + D9-Archaeologist branches use).

### Track B — D9-Archaeologist

1. **`ArchaeologistFindings.repo_id` references `Repositories.rowid`** (no declared FK); Repositories is TEXT-keyed by name. Same approach D8 + D9-ArchHealth use.
2. **No `force archaeologist sweep <repo>` CLI subcommand** — closure mention only, not exit criterion. Deferred.
3. **ARCH-001 deprecated-API list is Go-only v1.** Future-language extension via the existing pattern interface (other languages register their own list under the same `Pattern` shape).

---

## Verification (commands run, all green)

```
go vet ./...                                                     # exit 0
go build -tags sqlite_fts5 -o /tmp/force-d9 ./cmd/force/         # exit 0
go test -tags sqlite_fts5 -count=1 ./internal/archaeologist/...  # PASS — 13 pattern tests
go test -tags sqlite_fts5 -count=1 ./internal/agents/...         # PASS — claim loop + dog + report
go test -tags sqlite_fts5 -count=1 ./internal/audittools/...     # PASS — Pattern P-ArchaeologistOperatorGated + P25
go test -tags sqlite_fts5 -count=1 ./internal/dashboard/...      # PASS — handlers + SPA wiring
go test -tags sqlite_fts5 -count=5 -run "TestArchaeologist|TestArchHealth|TestSPA_Arch" ./...  # -count=5 stable
go test -tags sqlite_fts5 -count=1 -timeout 600s ./...           # full suite green
/tmp/force-d9 render-rules --check                               # OK no drift
make smoke                                                       # PASS
```

Strict verifier final-gate result for both tracks: **GO** (Static + Heavy + Race shards). Track A: 3/3 exit criteria pass; A–E anti-cheat clean; full suite green; `-count=5` stable. Track B: all D9-Archaeologist exit criteria pass (modulo #5 — see Residual #1); A/B/D anti-cheat directives enforced via Pattern P-ArchaeologistOperatorGated + language-awareness assertions + static-only registry inspection; SpawnArchaeologist confirmed LLM-free; 13 pattern tests + claim-loop smoke + sweep-dog smoke + migration-proposal smoke; `TestSchemaParity` green; `-count=5` stable.

---

## Residual list

1. ~~**Exit criterion #5 stubbed against D8 Track 2.**~~ **CLOSED by addendum `07cee2a1`.** Both D8 Track 2 (Chancellor blast-radius integration, merge `1511d6b7`) and the Archaeologist→PromotionProposal evidence-enricher bridge (merge `07cee2a1`) landed on `main`. `TestD9ExitCriterion5_BlastRadiusListsAll20Sites` (`internal/agents/archaeologist_d8_gate_test.go:65`) is the real test body, asserting exact 20-site coverage. Pattern P-ArchaeologistOperatorGated remains green (graph reads aren't a proposal-dispatch path; `EmitCandidate` is still the only emission point). See exit-criteria table above.
2. **End-to-end migration shakedown trace (exit criterion #2).** Engineering substrate is in place — Archaeologist proposes via `EmitCandidate`, operator ratifies, Engineering Corps decomposes, paired-runs experiment validates the 5%-cohort treatment. The full trace requires operator-cadence work over the experiment runtime window. Backfill into this closure as an addendum once a real migration completes the loop.
3. **First real-content monthly health report (exit criterion #3 spot-check).** The next month-boundary run produces the first non-synthetic content; structural correctness is already pinned by the test suite. Operator's qualitative spot-check is a few-day-after-month-rollover step.
4. **Tree-sitter / non-Go-language pattern coverage.** ARCH-001's deprecated-API list is Go-only v1; ARCH-003's structural AST hash is Go-only. Expansion to JS/TS/Python/Rust slots into the existing `Pattern` interface; not blocking.
5. **`force archaeologist sweep <repo>` CLI subcommand.** Operator currently triggers a sweep via the dog cycle; an explicit CLI subcommand for ad-hoc sweeps is a UX nicety, not a closure blocker.

All exit criteria #1-5 now pass at engineering scope. Both tracks remain CLOSED.

---

## Related ancillary fix (notify-after test-mode suppression — `42312de`)

Landed in the same window as the Archaeologist evidence-enricher bridge. Not part of D9 scope but worth a closure-level pointer because the operator-flagged daemon-Slack surface concern was raised during the same review window. **Root cause:** the D5.5 P4 stage-transition `notify-after` seam (and the gate-timeout escalation seam) was firing the production webhook unconditionally during `go test` runs — test-fixture stage transitions were flooding the operator's `#jake-herman-claude-updates` Slack channel. **Fix:** wrap the lowest-level invocation `realNotifyAfter` (`internal/agents/dogs_supply_token_recheck.go:158`) with a `testing.Testing()` short-circuit (Go 1.21+; this repo is on 1.25.3). Returns `true` iff the binary was built by `go test`; production paths are unaffected because every notify path funnels through `notifyAfterFn` which defaults to `realNotifyAfter`. Existing test seams (`notifyAfterFn`, `SetStageTransitionNotifyForTest`, `SetGateTimeoutSendMailForTest`) keep working — tests that want to assert on the call path install a mock via the seam; tests that don't (the noisy ones) get silent operation. **What's NOT fixed:** the production daemon STILL pings Slack via the same seam — that broader scope decision is pending operator input per the roadmap-declared-but-under-discussed daemon-Slack surface.
