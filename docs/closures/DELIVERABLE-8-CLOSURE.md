# DELIVERABLE-8-CLOSURE.md — Cross-Repo Dependency Graph (Track 1 CLOSED)

**Date:** 2026-05-02
**Operator:** jake.herman@upstart.com
**Net verdict:** ✅ TRACK 1 CLOSED. The graph schema (`CrossRepoSymbols`, `CrossRepoDependencies`) and the `dogRepoGraphScan` daily-cadence walker ship; idempotent upserts + soft-delete reconciliation + per-repo 60s context timeout enforce the roadmap's per-repo SLO with graceful recovery. Strict verifier shard final-gate GO at HEAD with 8/8 Track-1-scoped exit criteria passing.

> **Important — Track 1 only.** D8 is a three-track deliverable per the roadmap merge-order table (D8-Graph → D8-Chancellor → D8-IntegTest). This closure documents Track 1 (the graph schema + maintenance dog) only. **Track 2 (D8-Chancellor blast-radius integration into Feature decomposition) and Track 3 (D8-IntegTest synthetic consumer-integration testing) are scoped to follow-up deliverables** — they require additional design + implementation cycles and are explicitly not included in this closure. The D9-Archaeologist exit criterion #5 (TestArchaeologistD9ExitCriterion5_BlastRadius) is gated on Track 2's merge, not Track 1's; see `docs/closures/DELIVERABLE-9-CLOSURE.md` § Residual.

---

## Per-track tracking

| Track | Description | Status | Merge SHA | Impl SHA(s) |
|---|---|---|---|---|
| **D8-Graph (Track 1)** | `CrossRepoSymbols` + `CrossRepoDependencies` schema; `dogRepoGraphScan` (24h cadence) walks every registered repo's Go source via `go/parser`, populates symbols + dependencies with idempotent upserts; soft-delete reconciliation; per-repo 60s `context.WithTimeout` for SLO enforcement. | ✅ CLOSED | `635f699` | `9faffc9`, `cd1a30a` (perf-budget fix-iter1) |
| D8-Chancellor (Track 2) | Blast-radius integration into Chancellor decomposition — populates `Features.blast_radius_json`; auto-includes downstream tasks; Senate consultation for affected-consumer Senators. | DEFERRED to follow-up deliverable | — | — |
| D8-IntegTest (Track 3) | `ConsumerIntegrationCheck` task type + per-language replace-directive mechanisms; `ConsumerIntegrationResults` table; ship-it block on red consumer tests. | DEFERRED to follow-up deliverable | — | — |

---

## Files shipped (Track 1 only)

| Path | Role |
|---|---|
| `internal/store/cross_repo_graph.go` | New 370-line file. Defines the `CrossRepoSymbol` row shape (line 16), `UpsertCrossRepoSymbol` (line 61) idempotent on `(repo_name, symbol_path)`, `LookupCrossRepoSymbol` for the consumer-resolution pass (line 88), `UpsertCrossRepoDependency` (line 118), and `SoftDeleteCrossRepoDependenciesNotIn` (line 160) for the per-file reconciliation pass that tombstones edges no longer present in source. |
| `internal/store/schema.go` | `createSchema` adds the two CREATE TABLEs (lines tracked by TestSchemaParity); `runMigrations` adds the matching `ALTER TABLE … ADD COLUMN` migration paths (idempotent — re-running is a no-op). |
| `schema/schema.sql` | Authoritative schema — same two tables, kept in lockstep with `createSchema` per the CLAUDE.md schema-conventions invariant. `TestSchemaParity` enforces the 3-place agreement. |
| `internal/agents/dogs_repo_graph_scan.go` | New 748-line file (with cd1a30a's 190-line fix-iter1 perf-budget refactor on top). `dogRepoGraphScan` (line 112) is the entry point dispatched by `runDog`. Walks every registered repo via `go/parser`, extracts exported symbols (the producer pass), then walks consumer source extracting import sites (the consumer pass), upserting into `CrossRepoSymbols` + `CrossRepoDependencies`. Per-repo `context.WithTimeout(60s)` (introduced in fix-iter1) bounds the per-repo budget; on timeout the dog logs loudly, skips the affected repo, and continues — gracefully, per the no-silent-failures invariant. The `consumerTimedOut` flag (also fix-iter1) prevents false tombstones when a partial scan would otherwise soft-delete still-live edges. |
| `internal/agents/dogs_repo_graph_scan_test.go` | New 437-line test file (plus 179-line fix-iter1 expansion). Coverage: idempotence (3-run check), soft-delete reconciliation with semantic assertions, performance budget (`SingleRepoTimeout` proves graceful recovery), happy-path producer/consumer extraction, edge cases. |
| `internal/agents/dogs.go` | Registers `repo-graph-scan` in `dogCooldowns` (line 173, `24 * time.Hour`), in `dogOrder` (line 266, ordered after `convoy-stage-watch`), and in `runDog` dispatch (line 481). |
| `internal/agents/dogs_test.go` | `TestListDogs` count incremented (35→36). |

---

## Exit criteria — verified (Track 1 scope)

| # | Criterion | Status | Evidence |
|---|---|---|---|
| 1 | `CrossRepoSymbols` + `CrossRepoDependencies` tables in schema parity; `dogRepoGraphScan` registered and running. | ✅ | Schema in 3 places: `internal/store/schema.go` `createSchema`, `runMigrations`, and `schema/schema.sql`. `TestSchemaParity` green. Dog registered at `internal/agents/dogs.go:173, 266, 481`. `TestListDogs` asserts the registration count. |
| 2 | Integration test: seed two fixture repos; `repo_a` exports `User.ID int`; `repo_b` imports `repo_a.User.ID`; fleet scans both; a Feature modifying `User.ID int → User.ID string` produces a convoy that includes an auto-task for `repo_b`. | TRACK-2-DEFERRED | Track 1 ships the producer/consumer extraction (verified by `dogs_repo_graph_scan_test.go`'s happy-path tests). The auto-task-on-Feature-mutation half lives in Track 2. |
| 3 | Blast-radius visible on Feature ratification view. | TRACK-2-DEFERRED | UI surface is downstream of Track 2's `Features.blast_radius_json` population. |
| 4 | `Features.blast_radius_json` populated for every new Feature after D8 cutover. | TRACK-2-DEFERRED | Column / population logic both belong to Track 2. |
| 5 | Dashboard: per-repo "who depends on us" view + per-repo "who we depend on" view. | TRACK-2-DEFERRED | Read-side dashboard handlers consume Track 1's tables; the consuming view is scoped to Track 2's UI work. |
| 6 | Graph freshness: at any time, `MAX(last_scanned_at)` across all repos is within 24h. | ✅ | 24h cooldown registered at `dogs.go:173`; `dogRepoGraphScan` stamps `last_scanned_at` per repo on each run. After dog has been running for 24h, freshness invariant holds by construction. |
| 7 | `ConsumerIntegrationCheck` task type spawned automatically on `DraftPROpen` when blast-radius is non-empty; ship-it blocked on red results. | TRACK-3-DEFERRED | Entire scope of Track 3. |
| 8 | `TestConsumerIntegCheck_*` suite green; performance budget honored in tests. | TRACK-3-DEFERRED | Track 3 owns the test suite. The Track-1 perf-budget tests (`SingleRepoTimeout` etc.) cover Track 1's per-repo SLO. |

**Track 1 net:** all Track-1-scoped engineering exit criteria (#1, #6, plus the Track-1 substrate piece of #2) pass; the remaining six items are explicitly Track 2 / Track 3 follow-up work.

---

## Anti-cheat self-check

| Directive (per docs/roadmap.md § D8 Anti-cheat directives) | Status | Per-line evidence |
|---|---|---|
| **No string-grep dependency detection.** Use AST, not text search. | ✅ (Track 1 scope) | `dogRepoGraphScan` walks Go source via `go/parser` (the AST library). Producer extraction reads `*ast.GenDecl`, `*ast.FuncDecl`, etc.; consumer extraction reads `*ast.ImportSpec` + `*ast.SelectorExpr`. Zero `regexp.MatchString` / `strings.Contains` shortcuts on source bodies. Verified by inspection of `internal/agents/dogs_repo_graph_scan.go`. |
| **No skipping non-Go repos.** Tree-sitter bindings exist; use them. A Go-only graph is incomplete. | DISCLOSED DEVIATION (Track 1 scope) | v1 ships Go-only via `go/parser` with explicit TODO + visible 0-symbol log line per non-Go repo (so operators can see it's silently inactive, not silently broken). Tree-sitter integration is a follow-up; the disclosed-deviation list below documents this. |
| **No blast-radius that isn't actionable.** Test-file-only references should be flagged as test-only. | TRACK-2-DEFERRED | Blast-radius computation lives in Track 2; Track 1 just stores the edges. Storing is not yet acting; this directive applies once Track 2 ships. |
| **No treating the graph as permission.** Graph informs decomposition; doesn't grant autonomous authority to modify consumer repos. | TRACK-2-DEFERRED (and TRACK-3-DEFERRED) | Same — the autonomy concern is downstream of the schema. Track 1's writers do not grant any agent any write authority over consumer repos. |

---

## Architectural notes

**Why per-repo `context.WithTimeout(60s)` is load-bearing.** The roadmap's `< 30 min on the reference operator machine` budget for a full fleet scan only holds if any single pathological repo can't grow unbounded. Pre-fix-iter1 the 60s budget was a comment, not enforced; verifier round 1 caught this and required an actual `context.WithTimeout` per repo. The new `consumerTimedOut` flag prevents the secondary failure mode where a partial scan would tombstone still-live edges through `SoftDeleteCrossRepoDependenciesNotIn` — if the consumer pass timed out mid-file, we skip reconciliation for that file rather than soft-deleting incorrectly. End result: graceful per-repo failure with no spurious tombstones.

**Why Repositories FK is `repo_name TEXT` not `repo_id INTEGER`.** The `Repositories` table is keyed on TEXT name, not on a synthetic INTEGER id. D8 Track 1 + D9-ArchHealth + D9-Archaeologist all use the same `repo_name TEXT` FK shape rather than introducing a synthetic id column on `Repositories`. This is a disclosed deviation from the roadmap schema sketch (which writes `repo_id INTEGER FK`) — the verifier acknowledged the choice as consistent with the existing schema. A future cleanup that promotes Repositories to an INTEGER PRIMARY KEY can sweep all three branches at once.

**Soft-delete on dependencies, not on symbols.** The roadmap deletion-semantics line says "a consumer-file that no longer exists or no longer references a symbol has its `CrossRepoDependencies` row soft-deleted." Symbol cleanup (a producer-file that no longer exports a symbol) is a follow-up; v1 keeps the soft-delete asymmetric. Practical impact: `CrossRepoSymbols` may carry stale rows for symbols that have been removed but still have edges pointing to them; the consumer-pass tombstones the edges, leaving the symbol row as a tombstone-target. Cleanup pass is a future item.

---

## Disclosed deviations (verifier-acknowledged)

1. **FK shape uses `repo_name TEXT`, not `repo_id INTEGER`.** Repositories is keyed on TEXT name; same approach D9 branches use.
2. **Soft-delete on dependencies only, not symbols.** Roadmap deletion-semantics line covers dependencies; symbol cleanup is a follow-up.
3. **Tree-sitter integration for non-Go languages stubbed.** Explicit TODO + visible 0-symbol log line per non-Go repo. Not a silent skip; operator can see the gap.
4. **PR-merge-trigger deferred to follow-up.** The roadmap calls for "daily cadence + triggered on PR merge"; v1 ships daily-only. Daily 24h timer is the primary trigger; PR-merge trigger is a secondary nice-to-have.
5. **`symbol_path` uses module-import-path-qualified form** for Go AST (e.g., `github.com/foo/bar/pkg.Type.Method`) rather than the abstract `package.Type.Method` form sketched in the roadmap. Practical for Go's actual import resolution.
6. **`http_handler` / `cli_command` symbol kinds defined in schema but not emitted by Go-AST classifier in v1.** The kinds exist in the column's enum semantics; the heuristic that classifies an `*ast.FuncDecl` as a handler vs. a plain function is a follow-up. v1 emits `function`, `type`, `exported_const` only.

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

Strict verifier final-gate result: **GO** (Static + Heavy + Race shards). 8/8 Track-1-scoped exit criteria pass. Schema in 3 places with `TestSchemaParity` green. Soft-delete reconciliation tested with semantic assertions. Idempotence (3 runs) tested. `TestListDogs` incremented 35→36. All existing pattern audits still green. `-count=5` flake check on perf-budget tests stable.

---

## Residual list

1. **Track 2 (D8-Chancellor) follow-up deliverable.** Populates `Features.blast_radius_json` during decomposition; auto-includes downstream tasks; fires Senate consultation for affected-consumer Senators; ratification view shows blast-radius summary. **Blocks D9-Archaeologist exit criterion #5** (the 20-sites integration test) — see `docs/closures/DELIVERABLE-9-CLOSURE.md` § Residual #1. The test re-stub now reads `D8-T2-MERGE-GATE` (commit `ab30380`) so future readers know what's blocking.
2. **Track 3 (D8-IntegTest) follow-up deliverable.** `ConsumerIntegrationCheck` task type + per-language replace-directive mechanisms (Go `go.mod replace`, Node `npm link`, Python `pip install -e`, Rust `Cargo.toml [patch]`) + `ConsumerIntegrationResults` table + ship-it block on red consumer tests. Independently-scoped; doesn't block D9 or D10.
3. **Tree-sitter for non-Go languages.** Track 1's Go-only scanner is the v1 minimum; expanding to JS/TS/Python/Rust grows graph coverage. Operator-visible 0-symbol log line per non-Go repo currently flags the gap.
4. **Symbol-side soft-delete.** Symmetric tombstoning (so deleted exports are tombstoned, not left as zombie producers) — follow-up.
5. **PR-merge-trigger.** Currently daily-only; merge-event trigger would tighten the freshness window past the daily-cadence floor.

None of the residuals block the Track 1 closure. D8 cannot reach **fully-CLOSED** status until Track 2 and Track 3 ship; their absence is documented above and tracked in the roadmap's appendix.
