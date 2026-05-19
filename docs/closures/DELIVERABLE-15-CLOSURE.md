# DELIVERABLE-15 CLOSURE — API-Surface Dependency Graph

## Status: COMPLETE

## Summary

D15 ships a full API-surface dependency graph on top of the D8 symbol graph. Seven provider extractors (Rails, proto, OpenAPI, Spring, Ktor, Express, NestJS) and four consumer extractors (jsclient, rubyclient, javaclient, grpcclient) parse source files and populate two new SQLite tables (`CrossRepoAPIs` / `CrossRepoAPIDependencies`). A daily dog (`repo-api-scan`) drives the scan cycle via the `ExtractorRegistry`; the Chancellor blast-radius post-process and Diplomat consumer integration checks now union API-surface consumers with the existing symbol-level consumers from D8.

## Exit Criteria Verification

| # | Criterion | Status | Notes |
|---|-----------|--------|-------|
| 1 | Schema (3-place parity) | PASS | `CrossRepoAPIs` and `CrossRepoAPIDependencies` present in `createSchema`, `runMigrations`, and `schema/schema.sql` with identical column definitions and indexes |
| 2 | NormalizeAPIPath | PASS | `internal/store/api_path_normalize.go` handles `{id}`, `${id}`, `<id>`, `:id` → `:id`; trailing slash stripped; non-HTTP identifiers left unchanged |
| 3 | Store helpers | PASS | `UpsertCrossRepoAPI`, `UpsertCrossRepoAPIDependency`, `GetAPIBlastRadius`, `ListCrossRepoAPIs`, `SoftDeleteCrossRepoAPIDependency` all exist in `internal/store/cross_repo_apis.go` and return `error` |
| 4 | Provider extractors (7) | PASS | rails, proto, openapi, spring, ktor, express, nestjs — each implements `Kind()`, `ExtractorName()`, `Extract()` |
| 5 | Consumer extractors (4) | PASS | jsclient, rubyclient, javaclient, grpcclient — each implements `SupportedCallKinds()`, `Extract()` |
| 6 | ExtractorRegistry | PASS | `internal/apiextract/registry.go` with `sync.RWMutex`, `RegisterProvider`, `RegisterConsumer`, `AllProviders`, `AllConsumers`; only imports `sync` (zero concrete extractor package imports) |
| 7 | Scanner | PASS | `scanner.go` + `matcher.go` in `internal/apiextract/scanner/`; scanner imports only `apiextract` interface and `store`; `ResolveConsumerDependencies` is idempotent (DB-only resolution pass) |
| 8 | Single import point | PASS | grep confirms only `cmd/force/fleet_cmds.go` imports concrete extractor packages in non-test production code |
| 9 | Dog `repo-api-scan` | PASS | Registered in `dogs.go` with 24h cooldown; `TestListDogs` asserts 41 total dogs (comment explicitly cites D15-P6) |
| 10 | Chancellor integration | PASS | `store.BlastRadiusRecord.APIConsumers []string` in `blast_radius.go`; `unionAPIBlastRadius` helper in `chancellor_blast_radius.go` populates it via `ListCrossRepoAPIs` + `GetAPIBlastRadius` |
| 11 | Diplomat integration | PASS | `diplomat_consumer_integration.go` `DispatchConsumerIntegrationChecks` unions `rec.AffectedConsumerRepos` and `rec.APIConsumers` into a single `consumerSet` before dispatching `ConsumerIntegrationCheck` tasks |
| 12 | Audit pattern tests | PASS | All 4 tests in `internal/audittools/audit_pattern_p_d15_api_graph_test.go` have real assertions: path normalization round-trip, extractor coverage (≥1 result per fixture), resolver completeness (2/3 rows resolved), AST-level diplomat wiring check |
| 13 | Tests green | PASS | `make test` — all 68 packages pass, no failures |

## Phases Shipped

| Phase | Commit | Description |
|-------|--------|-------------|
| P1 | 476afde | Schema (`CrossRepoAPIs` / `CrossRepoAPIDependencies`), `NormalizeAPIPath`, store helpers (`UpsertCrossRepoAPI`, `UpsertCrossRepoAPIDependency`, `GetAPIBlastRadius`, `ListCrossRepoAPIs`, `SoftDeleteCrossRepoAPIDependency`) |
| P2 | b149cb1 | Rails routes extractor (`.rb`), proto RPC extractor (`.proto`), OpenAPI/Swagger YAML/JSON extractor |
| P3 | 7a0145d | Spring annotation extractor (`.java`/`.kt`), Ktor DSL extractor (`.kt`) |
| P4 | 612371c | Express app extractor (`.js`/`.ts`), NestJS decorator extractor (`.js`/`.ts`) |
| P5 | 71175a6 | Consumer-side extractors: jsclient (fetch/axios), rubyclient (HTTParty/Faraday/Net::HTTP/RestClient), javaclient (RestTemplate/OkHttp/Retrofit), grpcclient (Go + Java gRPC stubs) |
| P6 | cd94fe6 | `ExtractorRegistry`, `Scanner` (provider + consumer walk), `matcher.go` (`ResolveConsumerDependencies` / `ResolveConsumerDependenciesWithDeps`), `repo-api-scan` dog, Chancellor `unionAPIBlastRadius`, Diplomat `APIConsumers` union, `fleet_cmds.go` wiring, audit pattern tests |
| P6-fix | ed0ebba | HolonetRotate test fix (pre-existing regression unrelated to D15, repaired opportunistically) |

## Known Limitations / Follow-on

- **Multi-producer convoy fan-out (Track 3 v2):** `DispatchConsumerIntegrationChecks` picks the first `ConvoyAskBranch` as the sole producer; multi-producer cross-wiring is deferred.
- **Non-Go consumer language support:** Track 3 v1 only wires Go consumers via `go.mod replace`. Node/Python/Rust consumers are detected and skipped with an operator alert; full support deferred to a follow-on deliverable.
- **GraphQL extractors:** `graphql-schema` extractor listed in schema comments but not shipped; deferred to D17/D18 as per roadmap.
- **grpcclient Java stubs:** Java gRPC consumer pattern matching uses regex on stub import signatures; protoc-generated stub name variants may miss exotic naming conventions (addressed by testdata coverage but not exhaustive).
- **`ResolveConsumerDependencies` DB-only path:** The DB-only resolver (matcher.go) uses `consumer_file` as a proxy for the API identifier for pre-resolution rows; this is a heuristic that works for the normal dog flow but is documented as a limitation for hand-crafted rows.

## Verified by

D15 P7 strict verifier — 2026-05-19
