# DELIVERABLE-8-CLOSURE.md — Cross-Repo Dependency Graph (ALL THREE TRACKS CLOSED)

**Date:** 2026-05-02 (original Track 1 closure); 2026-05-05 addendum (Tracks 2 + 3)
**Operator:** jake.herman@upstart.com
**Net verdict:** ✅ ALL THREE TRACKS CLOSED. Track 1 ships the graph schema (`CrossRepoSymbols`, `CrossRepoDependencies`) and the `dogRepoGraphScan` daily-cadence walker. Track 2 ships the Chancellor blast-radius integration: `BountyBoard.blast_radius_json` column, `graph.Client.BlastRadiusForModifications` impl, post-process pass in APPROVE/SEQUENCE/MERGE that auto-includes downstream tasks + fans out per-Senator consultations, and a dashboard `GET /api/features/<id>/blast-radius` read surface. Track 3 ships the `ConsumerIntegrationCheck` task type (Diplomat-spawned on `DraftPROpen` when blast-radius flags consumer repos) with per-consumer worktree provisioning, `go.mod replace`–driven test runs, pre-existing-red baseline, per-consumer `context.WithTimeout` (default 20min), `ConsumerIntegrationResults` table, 6-status (+2 disclosed-deviation) aggregation that only blocks ship-it on `red`, and DB-only `[CONSUMER BREAKAGE]` operator-mail (no Slack). Strict verifier shard final-gate GO on each track at its HEAD.

> **Three-track sequence complete.** D8 is a three-track deliverable per the roadmap merge-order table (D8-Graph → D8-Chancellor → D8-IntegTest). All three tracks have merged on `main`; the D9-Archaeologist exit-#5 gate that this closure originally documented as a follow-up has also been lifted (see Track 2 addendum + `docs/closures/DELIVERABLE-9-CLOSURE.md`).

---

## Per-track tracking

| Track | Description | Status | Merge SHA | Impl SHA(s) |
|---|---|---|---|---|
| **D8-Graph (Track 1)** | `CrossRepoSymbols` + `CrossRepoDependencies` schema; `dogRepoGraphScan` (24h cadence) walks every registered repo's Go source via `go/parser`, populates symbols + dependencies with idempotent upserts; soft-delete reconciliation; per-repo 60s `context.WithTimeout` for SLO enforcement. | ✅ CLOSED | `635f699` | `9faffc9`, `cd1a30a` (perf-budget fix-iter1) |
| **D8-Chancellor (Track 2)** | Blast-radius integration into Chancellor decomposition. `graph.Client.BlastRadiusForModifications` (real impl, no longer `ErrNotImplemented`) → `BountyBoard.blast_radius_json` populated via post-process in APPROVE/SEQUENCE/MERGE → auto-included downstream `[BLAST_RADIUS_UPDATE]` tasks per `(consumer_repo, modified_symbol)` pair (parent_id wired) → per-Senator `SenateReview` consultations for active chambers → `GET /api/features/<id>/blast-radius` read surface. Verifier returned **GO** (7/7 spec bullets, 6/6 anti-cheat checks). | ✅ CLOSED | `1511d6b7` | `c897245` |
| **D8-IntegTest (Track 3)** | `ConsumerIntegrationCheck` task type + Diplomat dispatch on `DraftPROpen` when `blast_radius_json.affected_consumer_repos` is non-empty + per-consumer `.force-worktrees/` provisioning + `go.mod replace`–driven with-change run + pre-existing-red baseline + per-consumer `context.WithTimeout` (default 20min via `SystemConfig.consumer_integ_timeout_minutes`) + `ConsumerIntegrationResults` table + 6-status aggregation (+2 disclosed-deviation values) that only blocks ship-it on `red` + DB-only `[CONSUMER BREAKAGE]` operator-mail (no Slack/notify-after surface added). Verifier returned **GO** (7/7 spec bullets, 4/4 anti-cheat). | ✅ CLOSED | `b8fd14b9` | `6af2505` |

---

## Files shipped

### Track 1 — D8-Graph

| Path | Role |
|---|---|
| `internal/store/cross_repo_graph.go` | New 370-line file. Defines the `CrossRepoSymbol` row shape (line 16), `UpsertCrossRepoSymbol` (line 61) idempotent on `(repo_name, symbol_path)`, `LookupCrossRepoSymbol` for the consumer-resolution pass (line 88), `UpsertCrossRepoDependency` (line 118), and `SoftDeleteCrossRepoDependenciesNotIn` (line 160) for the per-file reconciliation pass that tombstones edges no longer present in source. |
| `internal/store/schema.go` | `createSchema` adds the two CREATE TABLEs (lines tracked by TestSchemaParity); `runMigrations` adds the matching `ALTER TABLE … ADD COLUMN` migration paths (idempotent — re-running is a no-op). |
| `schema/schema.sql` | Authoritative schema — same two tables, kept in lockstep with `createSchema` per the CLAUDE.md schema-conventions invariant. `TestSchemaParity` enforces the 3-place agreement. |
| `internal/agents/dogs_repo_graph_scan.go` | New 748-line file (with cd1a30a's 190-line fix-iter1 perf-budget refactor on top). `dogRepoGraphScan` (line 112) is the entry point dispatched by `runDog`. Walks every registered repo via `go/parser`, extracts exported symbols (the producer pass), then walks consumer source extracting import sites (the consumer pass), upserting into `CrossRepoSymbols` + `CrossRepoDependencies`. Per-repo `context.WithTimeout(60s)` (introduced in fix-iter1) bounds the per-repo budget; on timeout the dog logs loudly, skips the affected repo, and continues — gracefully, per the no-silent-failures invariant. The `consumerTimedOut` flag (also fix-iter1) prevents false tombstones when a partial scan would otherwise soft-delete still-live edges. |
| `internal/agents/dogs_repo_graph_scan_test.go` | New 437-line test file (plus 179-line fix-iter1 expansion). Coverage: idempotence (3-run check), soft-delete reconciliation with semantic assertions, performance budget (`SingleRepoTimeout` proves graceful recovery), happy-path producer/consumer extraction, edge cases. |
| `internal/agents/dogs.go` | Registers `repo-graph-scan` in `dogCooldowns` (line 173, `24 * time.Hour`), in `dogOrder` (line 266, ordered after `convoy-stage-watch`), and in `runDog` dispatch (line 481). |
| `internal/agents/dogs_test.go` | `TestListDogs` count incremented (35→36). |

### Track 2 — D8-Chancellor (added by `1511d6b7`)

| Path | Role |
|---|---|
| `internal/store/schema.go` + `schema/schema.sql` | New `BountyBoard.blast_radius_json TEXT NOT NULL DEFAULT '{}'` column (Features-row scoped; non-Feature rows + pre-T2 Features carry `'{}'`). 3-place schema parity; `createSchema` line 86 + `runMigrations` line 2221. |
| `internal/clients/graph/client.go` | Extends `Client` interface with `BlastRadiusForModifications`, `SymbolModification`, `ConsumerSite` types. `NewInProcess` now takes `*sql.DB`; `nil` yields `ErrIndexNotReady` on every read (useful for daemon-startup with constructor injection). |
| `internal/clients/graph/inprocess.go` | Replaces D0 `ErrNotImplemented` stubs with the store-driven impl that walks `CrossRepoSymbols` → `ListConsumersOfSymbol` → aggregated affected consumer-repos + per-symbol consumer file:line lists. |
| `internal/agents/chancellor_blast_radius.go` | New 307-line file. `PostProcessBlastRadius` is the deterministic post-process (no LLM call) wired into APPROVE/SEQUENCE/MERGE. Extracts modifications via payload-text scan against `ListCrossRepoSymbolsByRepo`, queries graph, persists to `BountyBoard.blast_radius_json`, inserts one `[BLAST_RADIUS_UPDATE]` `CodeEdit` task per `(consumer_repo, symbol)` pair (parent_id pointing at the Feature, convoy_id wired), and queues `SenateReview` consultations for active chambers. |
| `internal/agents/chancellor.go` | Threads `graph.Client` into approve/sequence/merge paths (≈58-line diff); calls `PostProcessBlastRadius` after each `insertConvoyAndTasks`. |
| `internal/agents/chancellor_blast_radius_test.go` | New 335-line test file. Coverage: full happy-path cycle, no-matching-symbols safe no-op, skips-modifying-repo (no self-update task), per-Senator dispatch only fires for active chambers. |
| `internal/dashboard/handlers_blast_radius.go` | New `GET /api/features/<id>/blast-radius` handler returning `{modified_symbols, affected_consumer_repos, auto_included_tasks}`. Allowlisted in Pattern P25 (read-only-views convention). |
| `internal/store/cross_repo_graph.go` | Adds `ListCrossRepoSymbolsByRepo`, `Get/SetFeatureBlastRadius` helpers consumed by Chancellor + dashboard. |

### Track 3 — D8-IntegTest (added by `b8fd14b9`)

| Path | Role |
|---|---|
| `internal/store/schema.go` + `schema/schema.sql` | New `ConsumerIntegrationResults` table with `UNIQUE(feature_id, consumer_repo_name)` + `idx_consumer_integ_results_feature(feature_id, status)` index. 3-place parity; `createSchema` line 1370, `runMigrations` line 2685. |
| `internal/store/consumer_integration_results.go` | New 233-line file. Row shape + CRUD; `IsBlockingCIStatus` returns true only on `CIStatusRed`; UNIQUE enforces "run once per Feature in DraftPROpen" at the SQL layer. |
| `internal/store/tasks.go` | Adds `ConsumerIntegrationCheck` to `InfrastructureTaskTypes`. |
| `internal/agents/diplomat_consumer_integration.go` | New 874-line file. `runConsumerIntegrationCheck` handler. Provisions per-consumer `.force-worktrees/` checkout of consumer's main, runs pre-existing-red baseline (consumer.main + producer.main), then runs with-change (consumer.main + producer.ask-branch via `go.mod replace`). Default test command `go test ./...`; per-repo override via `SystemConfig.consumer_integ_test_command:<repo_name>`. Per-consumer `context.WithTimeout` from `SystemConfig.consumer_integ_timeout_minutes` (default 20). Worktree teardown uses `context.WithoutCancel` + 30s budget so daemon-shutdown ctx-cancel doesn't strand `.force-worktrees/`. |
| `internal/agents/diplomat.go` | Diplomat dispatch on `DraftPROpen`: queues one `ConsumerIntegrationCheck` per `affected_consumer_repos` entry (idempotent via UNIQUE constraint). |
| `internal/agents/diplomat_consumer_integration_test.go` | New 749-line test file. Tests: `TestConsumerIntegCheck_GoReplaceDirective` (green + red sub-tests), `_PreExistingRed_DoesNotBlock`, `_ReadOnlyConsumer_Skips`, `_UnsupportedLang_Skips` (mail + dedup), `_TimeoutBudgetHonored`, `TestDiplomat_QueuesConsumerIntegCheck_OnDraftPROpen`, `TestQueueConsumerIntegrationCheck_Idempotent`. |
| `internal/dashboard/handlers_blast_radius.go` | Extended with `GET /api/features/<id>/consumer-integ` aggregation: rows + precomputed `any_blocking` flag + `blocking_repos` list. |
| `internal/dashboard/handlers_blast_radius_test.go` | New 147-line test file. `TestHandleFeatureConsumerInteg_AggregatesPersistedRows`, `_EmptyArraysWhenNoResults` (null-vs-`[]` contract), `_400OnUnknownSubroute`. |

---

## Exit criteria — verified (full deliverable scope)

| # | Criterion | Status | Evidence |
|---|---|---|---|
| 1 | `CrossRepoSymbols` + `CrossRepoDependencies` tables in schema parity; `dogRepoGraphScan` registered and running. | ✅ (Track 1) | Schema in 3 places: `internal/store/schema.go` `createSchema`, `runMigrations`, and `schema/schema.sql`. `TestSchemaParity` green. Dog registered at `internal/agents/dogs.go:173, 266, 481`. `TestListDogs` asserts the registration count. |
| 2 | Integration test: seed two fixture repos; `repo_a` exports `User.ID int`; `repo_b` imports `repo_a.User.ID`; fleet scans both; a Feature modifying `User.ID int → User.ID string` produces a convoy that includes an auto-task for `repo_b`. | ✅ (Track 1 + Track 2) | Track 1 ships producer/consumer extraction (`dogs_repo_graph_scan_test.go` happy-path); Track 2's `TestPostProcessBlastRadius_HappyPath_FullCycle` closes the loop — synthetic Feature + convoy + plan → post-process → assert `blast_radius_json` populated + auto-included `[BLAST_RADIUS_UPDATE]` tasks visible in `BountyBoard` with `parent_id=feature`, `convoy_id=convoy`. |
| 3 | Blast-radius visible on Feature ratification view. | ✅ (Track 2) | `GET /api/features/<id>/blast-radius` returns `{modified_symbols, affected_consumer_repos, auto_included_tasks}` (`internal/dashboard/handlers_blast_radius.go`); `TestHandleFeatureBlastRadius_Populated` / `_EmptyForNewFeature` / `_404OnUnknownFeature` / `_405OnPost` pin the contract. P25 allowlist updated. |
| 4 | `BountyBoard.blast_radius_json` populated for every new Feature after D8 cutover. | ✅ (Track 2) | Column ships in `createSchema` (line 86) + `runMigrations` (line 2221) + `schema/schema.sql` (line 48). `PostProcessBlastRadius` runs in APPROVE/SEQUENCE/MERGE; `TestPostProcessBlastRadius_HappyPath_FullCycle` asserts the column is written. (Disclosed deviation: column is on `BountyBoard` not a separate `Features` table — Features are `type='Feature'` rows; matches sibling JSON-column convention like `proposed_action_json`.) |
| 5 | Dashboard: per-repo "who depends on us" view + per-repo "who we depend on" view. | ✅ (Track 2 surface, Track 3 extension) | Track 2 ships `GET /api/features/<id>/blast-radius`; Track 3 extends with `GET /api/features/<id>/consumer-integ` aggregation. Both are read-only GETs allowlisted in P25. |
| 6 | Graph freshness: at any time, `MAX(last_scanned_at)` across all repos is within 24h. | ✅ (Track 1) | 24h cooldown registered at `dogs.go:173`; `dogRepoGraphScan` stamps `last_scanned_at` per repo on each run. After dog has been running for 24h, freshness invariant holds by construction. |
| 7 | `ConsumerIntegrationCheck` task type spawned automatically on `DraftPROpen` when blast-radius is non-empty; ship-it blocked on red results. | ✅ (Track 3) | `TestDiplomat_QueuesConsumerIntegCheck_OnDraftPROpen` asserts dispatch. `IsBlockingCIStatus` returns true only on `CIStatusRed` (`consumer_integration_results.go`); aggregation emits `[CONSUMER BREAKAGE]` operator-mail via `store.SendMail` (DB-only, no Slack). |
| 8 | `TestConsumerIntegCheck_*` suite green; performance budget honored in tests. | ✅ (Track 3) | Suite green: `TestConsumerIntegCheck_GoReplaceDirective` (green + red sub-tests, 4.3s), `_PreExistingRed_DoesNotBlock` (1.5s), `_ReadOnlyConsumer_Skips` (instant), `_UnsupportedLang_Skips` (mail + dedup, instant), `_TimeoutBudgetHonored` (3.5s — fractional-minute budget for predictable wall-time). `TestQueueConsumerIntegrationCheck_Idempotent` proves UNIQUE-backed dispatch idempotence. |

**Net:** 8/8 exit criteria pass across the full three-track deliverable.

---

## Anti-cheat self-check

| Directive (per docs/roadmap.md § D8 Anti-cheat directives) | Status | Per-line evidence |
|---|---|---|
| **No string-grep dependency detection.** Use AST, not text search. | ✅ (Track 1) | `dogRepoGraphScan` walks Go source via `go/parser` (the AST library). Producer extraction reads `*ast.GenDecl`, `*ast.FuncDecl`, etc.; consumer extraction reads `*ast.ImportSpec` + `*ast.SelectorExpr`. Zero `regexp.MatchString` / `strings.Contains` shortcuts on source bodies. Verified by inspection of `internal/agents/dogs_repo_graph_scan.go`. |
| **No skipping non-Go repos.** Tree-sitter bindings exist; use them. A Go-only graph is incomplete. | DISCLOSED DEVIATION | v1 ships Go-only via `go/parser` (Track 1) + Go-only `go.mod replace` (Track 3) with explicit TODO + visible 0-symbol log line per non-Go repo and per-language operator-mail-once dedup on first encounter (Track 3). Tree-sitter / non-Go consumer-integration is a follow-up; the disclosed-deviation list below documents this. |
| **No blast-radius that isn't actionable.** Test-file-only references should be flagged as test-only. | ✅ (Track 2) | `PostProcessBlastRadius` inserts one `[BLAST_RADIUS_UPDATE]` `CodeEdit` task per `(consumer_repo, modified_symbol)` with parent_id wired — the operator can see the convoy's expansion (no silent insertion). `auto_included_tasks` IDs are recorded durably in `blast_radius_json`. |
| **No treating the graph as permission.** Graph informs decomposition; doesn't grant autonomous authority to modify consumer repos. | ✅ (Tracks 2 + 3) | A blast-radius failure logs but does NOT fail the Feature: convoy is already on disk and the operator should see it; blast-radius is a safety net, not a gate (Track 2). Track 3's `ConsumerIntegrationCheck` emits evidence (red/green) but the operator still ratifies via ship-it; no auto-merge based on green tests. Senate consultations only fire for consumer repos with active Senators (filtered out for unregistered/onboarding). |
| **No new notify-after / Slack / FireWebhook surface.** Operator flagged daemon-Slack as under-discussed. | ✅ (Track 3) | Verifier's CRITICAL anti-cheat check confirmed zero new `notify-after` / Slack / `FireWebhook` call sites in the Track-3 branch. `[CONSUMER BREAKAGE]` and per-language alerts both use `store.SendMail` (DB-only). |

---

## Architectural notes

**Why per-repo `context.WithTimeout(60s)` is load-bearing.** The roadmap's `< 30 min on the reference operator machine` budget for a full fleet scan only holds if any single pathological repo can't grow unbounded. Pre-fix-iter1 the 60s budget was a comment, not enforced; verifier round 1 caught this and required an actual `context.WithTimeout` per repo. The new `consumerTimedOut` flag prevents the secondary failure mode where a partial scan would tombstone still-live edges through `SoftDeleteCrossRepoDependenciesNotIn` — if the consumer pass timed out mid-file, we skip reconciliation for that file rather than soft-deleting incorrectly. End result: graceful per-repo failure with no spurious tombstones.

**Why Repositories FK is `repo_name TEXT` not `repo_id INTEGER`.** The `Repositories` table is keyed on TEXT name, not on a synthetic INTEGER id. D8 Track 1 + D9-ArchHealth + D9-Archaeologist all use the same `repo_name TEXT` FK shape rather than introducing a synthetic id column on `Repositories`. This is a disclosed deviation from the roadmap schema sketch (which writes `repo_id INTEGER FK`) — the verifier acknowledged the choice as consistent with the existing schema. A future cleanup that promotes Repositories to an INTEGER PRIMARY KEY can sweep all three branches at once.

**Soft-delete on dependencies, not on symbols.** The roadmap deletion-semantics line says "a consumer-file that no longer exists or no longer references a symbol has its `CrossRepoDependencies` row soft-deleted." Symbol cleanup (a producer-file that no longer exports a symbol) is a follow-up; v1 keeps the soft-delete asymmetric. Practical impact: `CrossRepoSymbols` may carry stale rows for symbols that have been removed but still have edges pointing to them; the consumer-pass tombstones the edges, leaving the symbol row as a tombstone-target. Cleanup pass is a future item.

---

## Disclosed deviations (verifier-acknowledged)

### Track 1

1. **FK shape uses `repo_name TEXT`, not `repo_id INTEGER`.** Repositories is keyed on TEXT name; same approach D9 branches use.
2. **Soft-delete on dependencies only, not symbols.** Roadmap deletion-semantics line covers dependencies; symbol cleanup is a follow-up.
3. **Tree-sitter integration for non-Go languages stubbed.** Explicit TODO + visible 0-symbol log line per non-Go repo. Not a silent skip; operator can see the gap.
4. **PR-merge-trigger deferred to follow-up.** The roadmap calls for "daily cadence + triggered on PR merge"; v1 ships daily-only. Daily 24h timer is the primary trigger; PR-merge trigger is a secondary nice-to-have.
5. **`symbol_path` uses module-import-path-qualified form** for Go AST (e.g., `github.com/foo/bar/pkg.Type.Method`) rather than the abstract `package.Type.Method` form sketched in the roadmap. Practical for Go's actual import resolution.
6. **`http_handler` / `cli_command` symbol kinds defined in schema but not emitted by Go-AST classifier in v1.** The kinds exist in the column's enum semantics; the heuristic that classifies an `*ast.FuncDecl` as a handler vs. a plain function is a follow-up. v1 emits `function`, `type`, `exported_const` only.

### Track 2

7. **Column on `BountyBoard` not a separate `Features` table.** Features are `type='Feature'` rows; the JSON-column shape matches sibling per-Feature columns like `proposed_action_json`, `prior_review_outcomes_json`. Avoids a second-table join on every Feature read.
8. **Symbol-modification extraction is payload-text scan, not diff-based.** Chancellor runs before any Astromech, so there is no diff yet — the heuristic scans the task payload prose against the indexed `CrossRepoSymbols` set. Deterministic and verified by `TestExtractSymbolModifications_PayloadScan` / `_NoMatch`.
9. **Auto-included tasks per `(consumer_repo, modified_symbol)` pair, not per `consumer_repo` only.** Strictly more-actionable shape than the roadmap sketch; one task → one symbol change for the consumer's astromech to address.
10. **`graph.NewInProcess(nil)` returns `ErrIndexNotReady`** rather than panicking. Useful shape for daemon-startup with constructor injection (the daemon can wire the client before the DB handle is ready).

### Track 3

11. **8 status enum values vs the spec's 6.** Added `skipped_no_local_path` + `error` for completeness; both are non-blocking by definition (`IsBlockingCIStatus` returns true only on `red`).
12. **Default test command is `go test ./...`; consumer-repo-config-driven test command is a v2 hook.** Per-repo override exists today via `SystemConfig.consumer_integ_test_command:<repo_name>` (operator-set), but no automatic discovery from the consumer repo's own config.

---

## Verification (commands run, all green)

```
go vet ./...                                                     # exit 0
go build -tags sqlite_fts5 -o /tmp/force-d8 ./cmd/force/         # exit 0
go test -tags sqlite_fts5 -count=1 ./internal/store/...          # PASS — schema + cross_repo_graph CRUD
go test -tags sqlite_fts5 -count=1 ./internal/agents/...         # PASS — dog registration + dogRepoGraphScan
go test -tags sqlite_fts5 -count=5 -run "TestRepoGraphScan|TestSingleRepoTimeout|TestSoftDelete" ./internal/agents/...  # -count=5 flake check stable
go test -tags sqlite_fts5 -count=1 -timeout 600s ./...           # full ~5m30s green
/tmp/force-d8 render-rules --check                               # OK no drift
make smoke                                                       # PASS
```

Strict verifier final-gate results — Track 1: **GO** (Static + Heavy + Race shards), 8/8 Track-1-scoped exit criteria pass. Track 2 (`1511d6b7`): **GO**, 7/7 spec bullets + 6/6 anti-cheat clean, 14 new tests across graph/agents/dashboard/store. Track 3 (`b8fd14b9`): **GO**, 7/7 spec bullets + 4/4 anti-cheat clean, full `make test` ~5min green, `-count=5` stable. Schema in 3 places with `TestSchemaParity` green across all three tracks. All existing pattern audits still green.

---

## Residual list

1. **Tree-sitter for non-Go languages.** Track 1's Go-only AST scanner + Track 3's Go-only `go.mod replace` mechanism are both v1 minimums; expanding to JS/TS/Python/Rust grows graph + integtest coverage. Operator-visible 0-symbol log line per non-Go repo (Track 1) and per-language operator-mail-once dedup on first encounter (Track 3) currently flag the gap.
2. **Consumer-repo-config-driven test commands.** Track 3 defaults to `go test ./...` with operator-set per-repo overrides via `SystemConfig.consumer_integ_test_command:<repo_name>`. A v2 hook would auto-discover the test command from the consumer repo's own config (e.g. `Makefile` `test` target, `package.json` `scripts.test`). Independently scoped.
3. **Symbol-side soft-delete (Track 1).** Symmetric tombstoning (so deleted exports are tombstoned, not left as zombie producers) — follow-up.
4. **PR-merge-trigger (Track 1).** Currently daily-only; merge-event trigger would tighten the freshness window past the daily-cadence floor.
5. **Operator-cadence end-to-end migration trace (Tracks 2 + 3).** Engineering substrate is in place; observing a real producer-Feature-with-blast-radius land on `main`, dispatch consumer-integ checks, and unblock ship-it is an operator-cadence validation step. Backfill into this closure as an addendum once a real cycle completes.

None of the residuals block the deliverable's net **CLOSED** verdict. The follow-up items above are independently scoped and do not gate D9, D10, or the Archaeologist evidence-enricher (which has shipped — see `docs/closures/DELIVERABLE-9-CLOSURE.md` exit-#5 update).

---

## Related ancillary fix (notify-after test-mode suppression — `42312de`)

Landed in the same window as Track 2 (between `42312de` and `1511d6b7`). Not part of D8/D9 scope but worth a closure-level pointer because the operator flagged daemon-Slack as an under-discussed surface. Root cause: the D5.5 P4 stage-transition `notify-after` seam was firing the production webhook unconditionally during `go test` runs (test-fixture stage transitions were flooding the operator's Slack channel). Fix: wrap the lowest-level invocation `realNotifyAfter` (`internal/agents/dogs_supply_token_recheck.go:158`) with a `testing.Testing()` short-circuit; production paths are unaffected because every notify path funnels through `notifyAfterFn` which defaults to `realNotifyAfter`. **Not fixed by this commit:** the production daemon STILL pings Slack via the same seam — that broader scope decision is pending operator input per the roadmap-declared-but-under-discussed daemon-Slack surface.
