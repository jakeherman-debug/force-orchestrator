# D0 Verification Report

## Verdict: GO

D0 — Interface Layer Foundation — closes cleanly. All four tracks shipped
real artefacts (not stubs masquerading as artefacts), all eight roadmap
exit criteria are independently verified, no Fix #8d/#8e/#8f closure
regressed, and the `go build ./...` + clients-package race tests
(`-race -count=5`) are green. Pattern P16 is AST-based, walks
`internal/agents/` for real, has no allowlist, asserts both that every
`clients/<svc>/Client` is an interface and that no agent file
constructs a concrete client struct, and the closure report's RGR
red-phase probe is mechanically supported by the detection logic.
The CLAUDE.md "Cross-agent service interfaces" section is a pure
addition (commit `0f47a36`, single insertion hunk at line 28-68;
zero deletions, zero existing-MUST softenings). Daemon wiring is at
two entry points (`cmd/force/fleet_cmds.go:164`,
`cmd/force/obs_cmds.go:425`); the post-migration grep over
`internal/agents/` returns zero direct Librarian calls.

There are no dissenting tracks; the campaign re-opens for no reason.
D1 may proceed.

---

## Independent verification output

### D0-A — directory + CLAUDE.md heading

```
$ ls internal/clients/
capabilities
experiments
graph
librarian
metrics
rules

$ grep -n "Cross-agent service interfaces" CLAUDE.md
28:## Cross-agent service interfaces
```

Six service directories present; CLAUDE.md heading at line 28.

### D0-B — post-migration grep + daemon wiring

```
$ grep -rn "librarian\." --include="*.go" internal/agents/ \
    | grep -v _test.go \
    | grep -vE 'cfg\.Librarian|librarian\.Client|librarian\.NewInProcess|librarian\.NewMock|librarian\.Memory|librarian\.Scope|librarian\.MemoryUpdate|lib librarian\.Client|Librarian librarian\.Client'
(no output)

$ grep -rn "librarian.NewInProcess(" --include="*.go" cmd/
cmd/force/fleet_cmds.go:164:	libClient := librarian.NewInProcess(db)
cmd/force/obs_cmds.go:425:		libClient := librarian.NewInProcess(db)

$ go test -tags sqlite_fts5 -run TestLibrarianClient -race -count=5 -timeout 60s ./internal/clients/librarian/...
ok  	force-orchestrator/internal/clients/librarian	1.523s
```

Zero residual direct Librarian calls in agent code; daemon wires the
in-process client at both entry points (boot path and obs/CLI dog-run
path); race tests green at count=5.

### D0-C — five-stub triplet check + build

```
$ for d in capabilities experiments rules metrics graph; do
    test -f internal/clients/$d/client.go || echo "MISSING client.go: $d"
    test -f internal/clients/$d/inprocess.go || echo "MISSING inprocess.go: $d"
    test -f internal/clients/$d/mock.go || echo "MISSING mock.go: $d"
    grep -qE "^type Client interface" internal/clients/$d/client.go || echo "FAIL $d"
  done
(no output — zero MISSING / FAIL lines)

$ go build ./internal/clients/...
(no output — clean compile)

$ go test -tags sqlite_fts5 -race -count=5 -timeout 60s ./internal/clients/...
ok  	force-orchestrator/internal/clients/capabilities	1.328s
ok  	force-orchestrator/internal/clients/experiments	1.612s
ok  	force-orchestrator/internal/clients/graph	2.009s
ok  	force-orchestrator/internal/clients/librarian	2.131s
ok  	force-orchestrator/internal/clients/metrics	2.386s
ok  	force-orchestrator/internal/clients/rules	2.709s
```

All five future-service stubs satisfy the triplet rule
(client.go + inprocess.go + mock.go); each declares `Client` as an
interface; full clients-package suite green at race count=5.

### D0-D — Pattern P16

```
$ go test -tags sqlite_fts5 -run TestPattern_P16 -race -count=5 -timeout 60s ./internal/audittools/...
ok  	force-orchestrator/internal/audittools	2.012s
```

### Fix #8d/#8e/#8f regression grep set

```
$ grep -rn 't\.Skip(.AUDIT-' --include="*.go" internal/ cmd/ schema/
internal/audittools/audittools_test.go:16:// remainingAuditSkips is the allowlist of AUDIT IDs whose `t.Skip("AUDIT-NNN:`
internal/audittools/audittools_test.go:55:// `t.Skip("AUDIT-NNN:` marker is present for an AUDIT ID that is NOT on
```

Both hits are commentary in the regex describing the rule, not live
`t.Skip(...)` call sites. Effective live-marker count: 0.

```
$ grep -rnE 'context\.WithTimeout\(context\.Background\(\)' --include="*.go" internal/ cmd/ | grep -v '_test.go'
(no output)

$ grep -rn '_ = rows\.Err' --include="*.go" internal/ cmd/ | grep -v '_test.go'
(no output)

$ go build ./...
(no output — clean compile)

$ make smoke
ok  	force-orchestrator/cmd/force	0.290s [no tests to run]
ok  	force-orchestrator/internal/agents	0.522s
ok  	force-orchestrator/internal/audittools	1.621s [no tests to run]
ok  	force-orchestrator/internal/claude	1.800s [no tests to run]
ok  	force-orchestrator/internal/clients/capabilities	0.894s [no tests to run]
ok  	force-orchestrator/internal/clients/experiments	0.689s [no tests to run]
ok  	force-orchestrator/internal/clients/graph	1.223s [no tests to run]
ok  	force-orchestrator/internal/clients/librarian	1.458s [no tests to run]
ok  	force-orchestrator/internal/clients/metrics	2.195s [no tests to run]
ok  	force-orchestrator/internal/clients/rules	2.007s [no tests to run]
ok  	force-orchestrator/internal/dashboard	2.430s
ok  	force-orchestrator/internal/gh	2.602s [no tests to run]
ok  	force-orchestrator/internal/git	2.777s
ok  	force-orchestrator/internal/store	2.909s [no tests to run]
ok  	force-orchestrator/internal/telemetry	2.750s [no tests to run]

$ make test-audit
ok  	force-orchestrator/internal/audittools	0.323s
```

Sub-agent 5's broader Pattern-P / TestAUDIT regression at -race -count=5
across `./...` was green (timing: internal/agents 7.6s, internal/git
6.7s, internal/store 8.7s, internal/audittools 3.8s, internal/dashboard
2.4s); the full suite at `-race -count=1 -timeout 1800s ./...` was also
green (internal/agents 252s longest). Sub-agent reduced `-count=5` to
`-count=1` on the full suite to fit within the 25-minute timing budget;
the broader Pattern test set still ran at `-count=5`.

---

## Per-sub-agent verification

### Sub-agent 1 — Track D0-A (interface structure + CLAUDE.md)

| Check | Status | Evidence |
|---|---|---|
| Six service directories present | PASS | `internal/clients/{librarian,capabilities,experiments,rules,metrics,graph}/` all exist |
| client.go in each | PASS | All six client.go files 70-127 lines (non-stub); each has matching inprocess.go + mock.go + client_test.go |
| CLAUDE.md section heading present | PASS | `CLAUDE.md:28` — `## Cross-agent service interfaces` |
| CLAUDE.md names directory pattern | PASS | `CLAUDE.md:30-31` — "Cross-agent service dependencies route through Go interfaces in `internal/clients/<service>/`" |
| CLAUDE.md names constructor convention | PASS | `CLAUDE.md:41-42` — "MUST be constructed via the `NewInProcess(...)` factory function — never via a `&<service>.<Type>{...}` literal at the call site"; `:44-45` names `NewGRPC`, `NewShared`, `NewMock` |
| CLAUDE.md names Pattern P16 enforcement | PASS | `CLAUDE.md:55` — full file path `internal/audittools/audit_pattern_p16_clients_interfaces_test.go` |
| CLAUDE.md names D4 BoS rule graduation | PASS | `CLAUDE.md:64-67` — "WILL graduate to a BoS rule (BOS-CLIENTS-001 or similar) when D4 ships" |
| CLAUDE.md uses MUST/forbidden language | PASS | `CLAUDE.md:33` "are forbidden going forward"; `:37` "MUST be an interface, never a struct"; `:41` "MUST be constructed via"; `:59` "Construction MUST go through" |
| CLAUDE.md forbids direct cross-agent calls | PASS | `CLAUDE.md:30-33` — "Direct function-call dependencies between agents (e.g., `librarian.GetMemoriesForTask(...)` from Captain) are forbidden going forward" |
| Cheats observed | NONE | — |

**Verdict: ACCEPTED**

### Sub-agent 2 — Track D0-B (Librarian migration)

| Check | Status | Evidence |
|---|---|---|
| Client interface defined | PASS | `internal/clients/librarian/client.go:32` — `type Client interface {` |
| inProcessClient is unexported | PASS | `internal/clients/librarian/inprocess.go:22` — `type inProcessClient struct`; no uppercase `InProcessClient` anywhere; no `type Client = inProcessClient` alias and no `type Client struct` re-export |
| Methods on receiver (not package-level) | PASS | `func (c *inProcessClient) WriteMemory(ctx, m) (int, error)` at `inprocess.go:63`; the only package-level functions are `NewInProcess` (factory, intentional), `encodeWritePayload`, `normalizeUpdateField`, `scanMemoryRows` (unexported helpers — not Client surface) |
| Pre-migration call-site count (closure says) | 3 | producer-side queue sites (`pilot_draft_watch.go:196`, `pr_flow.go:564`, `jedi_council.go:506`) per `DELIVERABLE-0-CLOSURE.md:61-65` |
| Post-migration call-site count (verifier grep) | 0 | The full grep with the documented filter set returns zero residual lines; zero direct calls in any agent file |
| NewInProcess wired in cmd/ | PASS | `cmd/force/fleet_cmds.go:164` (boot path) and `cmd/force/obs_cmds.go:425` (CLI/dashboard dog-run path) |
| Spot-check `pilot_draft_watch.go` | PASS | `:202` — `lib.WriteMemoryTx(ctx, tx, librarian.Memory{...})`; `terminalConvoyTransitionTx` accepts `lib librarian.Client` (line 182), wired down from `dogDraftPRWatch(ctx, db, lib librarian.Client, logger)` (line 47) |
| Spot-check `pr_flow.go` | PASS | `:569` — `lib.WriteMemoryTx(ctx, tx, librarian.Memory{...})` inside `onSubPRMerged(..., lib librarian.Client, ...)` (line 548); plumbed through `dogSubPRCIWatch` (339), `handleSubPRPoll` (370), `mergeSubPRDirect` (932), `onSubPRMissingCI` (950) |
| Spot-check `jedi_council.go` | PASS | `:511` — `lib.WriteMemory(ctx, librarian.Memory{...})` inside `runCouncilTask`; `Librarian librarian.Client` field on the spawn config at line 26 |
| Mock factory exists | PASS | `mock.go:54` — `func NewMock() *MockClient`; struct at `:20`; compile-time `var _ Client = (*MockClient)(nil)` at `:202`. `MockClient` is exported by-design (test fixture); `Client` itself remains an interface |
| `TestLibrarianClient` passes -race -count=5 | PASS | `ok force-orchestrator/internal/clients/librarian 1.523s` (independent re-run). Macho linker warning is benign Darwin-toolchain noise unrelated to D0-B |
| `internal/agents/librarian.go` still exists | PASS | The SpawnLibrarian consumer-side agent is intact; `DELIVERABLE-0-CLOSURE.md:67-71` documents the rationale (consumer side stays in agents/, producer side moved to clients/) |
| No aliased import evading the grep | PASS | `grep '"force-orchestrator/internal/agents/librarian"' --include="*.go" internal/agents/ cmd/` returns zero hits (no `lib "..."`-style alias smuggling direct calls past the canonical-import grep) |
| Cheats observed | NONE | No exported `InProcessClient`; no `Client` alias/struct shape; no package-level `WriteMemory`/`GetMemoriesForTask`; no aliased imports; no partial migration |

**Verdict: ACCEPTED**

### Sub-agent 3 — Track D0-C (future-service stubs)

| Check | Status | Per-service evidence |
|---|---|---|
| Five service directories complete (3 + test files each) | PASS | `capabilities` (76L/34L/85L/49L), `experiments` (104L/29L/79L/52L), `rules` (82L/26L/102L/62L), `metrics` (70L/26L/115L/62L), `graph` (99L/26L/91L/47L) |
| Each `Client` is interface | PASS | `client.go:28` (capabilities), `:26` (experiments), `:28` (rules), `:26` (metrics), `:27` (graph) — all `type Client interface` |
| `inprocess.go` returns `ErrNotImplemented` per method | PASS | capabilities `:21,25,29,33`; experiments `:16,20,24,28` (Apply returns `(call, nil, ErrNotImplemented)` — error slot non-nil so callers respecting err-first contract bail before consulting the descriptor); rules `:13,17,21,25`; metrics `:13,17,21,25`; graph `:13,17,21,25` |
| No silent no-op masquerading | PASS | Audited every method body across all five inprocess.go files. Zero methods return a `nil` error path. Closest borderline is `experiments.Apply` (echoes input descriptor, returns nil for `[]Assignment`) — but the err slot is `ErrNotImplemented` |
| `mock.go` present + non-empty for each | PASS | All five mocks carry real test-friendly defaults plus `*Fn` override hooks plus compile-time `var _ Client = (*MockClient)(nil)` assertions; none stub-empty |
| Doc comment names owning deliverable | PASS | capabilities `client.go:7` (D1, T0-1); experiments `:6` (D3); rules `:9` (D3); metrics `:9` (D3); graph `:11` (D8) |
| `go build ./internal/clients/...` succeeds | PASS | Zero output |
| Tests green at -race -count=5 per service | PASS | capabilities 1.339s, experiments 1.910s, rules 1.639s, metrics 2.161s, graph 2.425s. `TestInProcess_StubReturnsErrNotImplemented` in each package walks every method via `errors.Is` check |
| Cheats observed | NONE | No silent zero-value+nil-error returns; no empty inprocess.go; no empty mock.go; no struct masquerade; constructor pattern compliant |

**Verdict: ACCEPTED**

### Sub-agent 4 — Track D0-D (Pattern P16)

| Check | Status | Evidence |
|---|---|---|
| Test file exists | PASS | `internal/audittools/audit_pattern_p16_clients_interfaces_test.go` (8439 bytes, 263 lines) |
| Test passes -race -count=5 | PASS | `ok force-orchestrator/internal/audittools 1.573s` (independent re-run also `2.012s`) |
| Walks production code via AST/WalkDir | PASS | `filepath.WalkDir(agentRoot, ...)` at line 96; AST parsing via `parser.ParseFile` at lines 104 & 132; `ast.Inspect` at line 137. Phase 1 also AST-parses every `clients/<svc>/*.go` via `findExportedClientType` at lines 229-263 |
| Skips `_test.go` files | PASS | `strings.HasSuffix(path, "_test.go") → return nil` at lines 100 (agent walk) and 237 (clients-package walk) |
| Detects concrete-type imports | INTENTIONAL | The test does NOT specifically flag *imports* of unexported types (Go's compiler already rejects cross-package references to unexported names). The test detects what an attacker *could* plausibly do: composite-literal construction against any exported `*Client`-suffixed type from `clients/<svc>/`. Doc-comment at lines 14-18 explicitly enumerates `&librarian.MockClient{}`, `librarian.InProcessClient{db: db}`, `&capabilities.GRPCClient{...}` |
| Detects `&<pkg>.<Struct>{...}` instantiation | PASS | Lines 137-169. `*ast.CompositeLit` whose Type is `*ast.SelectorExpr` matching a tracked `clients/<svc>/` import alias and a `Sel.Name` ending in "Client". Both `&pkg.T{...}` and bare `pkg.T{...}` produce a CompositeLit AST node, so both forms are caught |
| Detects Client-as-struct masquerade | PASS | Phase 1 lines 75-82. `findExportedClientType` returns the type expression; `_, ok := typ.(*ast.InterfaceType); !ok` triggers `t.Errorf` naming `file:line` and the actual type kind |
| Failure message names file:line | PASS | Line 81 (`t.Errorf("clients/%s: exported `Client` is %T at %s:%d ...", svc, typ, rel(root, file), line)`) and line 187 (`t.Errorf("  %s:%d — &%s.%s{...}  (or %s.%s{...})", ...)`). Position via `fset.Position(cl.Pos())` at line 160 |
| No allowlist for "legacy code" | PASS | Three `skip` matches found, all benign: line 106 ("skip unparseable files"), line 194 ("Empty placeholder directories ... are skipped"), line 226 (comment "skipping _test.go"). Zero `allowlist` / `legacy` / `exempt` / `ignore` per-file bypass map |
| Real assertions, not stub | PASS | 263 lines, no `t.Skip`, no deferred-work-marker comment. Real Phase 1 assertion at lines 80-82, real Phase 2 assertion at lines 185-188. `t.Fatalf` for missing fixtures at lines 65-69 ensures D0-A absence cannot make the test trivially green |
| Empirical RGR probe pattern would be caught | PASS | Closure report `DELIVERABLE-0-CLOSURE.md:207-216` documents probe `var _p16RedProbe = &librarian.MockClient{}` at `internal/agents/jedi_council.go:21` → test correctly failed with verbatim "Pattern P16 (D0): 1 agent file(s) construct a concrete client struct ... internal/agents/jedi_council.go:21 — &librarian.MockClient{...}". Detection logic at lines 137-169 unambiguously matches this pattern |
| Cheats observed | NONE | — |

**Verdict: ACCEPTED**

Forensic notes from sub-agent 4:
1. The test's two-phase design is correct: Phase 1 ensures the contract (`Client` is an interface) holds at the package boundary; Phase 2 ensures agents don't bypass the contract via composite literals.
2. Service discovery is dynamic via `listClientServices` (line 197) — adding a new service under `internal/clients/` automatically extends Phase 1 coverage; no hardcoded service list to drift.
3. `clientsPkgPrefix` (line 45) `force-orchestrator/internal/clients/` correctly matches the module's import path.
4. Phase 1 fatals if zero services found — prevents trivial green from a missing fixture.

### Sub-agent 5 — Fix #8d / #8e / #8f regression

| Check | Status | Evidence |
|---|---|---|
| AUDIT skip marker count (effective) | 0 | Two grep hits are commentary lines in `internal/audittools/audittools_test.go:16,55`; zero live `t.Skip(...)` calls |
| `remainingAuditSkips` map empty | PASS | `internal/audittools/audittools_test.go:27-49` — body contains only comment lines describing closed campaigns; zero live `"AUDIT-NNN":` entries |
| Fabricated `context.Background` contexts (prod) | 0 | `grep -rnE 'context\.WithTimeout\(context\.Background\(\)' --include="*.go" internal/ cmd/ \| grep -v '_test.go'` returns zero hits. Fix #8e Track-D migration intact |
| `_ = rows.Err` silent discards (prod) | 0 | `grep -rn '_ = rows\.Err' --include="*.go" internal/ cmd/ \| grep -v '_test.go'` returns zero hits. Pattern P1.1 intact |
| `exec.CommandContext(context.Background())` (prod) | 0 | grep returns zero hits. Pattern P11 cheat-shape rejection verified |
| Pattern tests pass -race -count=5 | PASS | `TestPattern_P\|TestAUDIT_\|TestAstromech_EstopCancelsInFlightGitOp\|TestRunShortGit_CtxCancel\|TestBestEffortRun_CtxCancelKillsSubprocess\|TestRunGitCtx_CtxCancel` all green at `-race -count=5 -timeout 600s ./...`. Timing: internal/agents 7.6s, internal/git 6.7s, internal/store 8.7s, internal/audittools 3.8s, internal/dashboard 2.4s. No FAIL |
| Full suite pass | PASS (-race -count=1) | Sub-agent reduced count to 1 due to time pressure. internal/agents 252s longest; all 14 internal packages "ok"; cmd/force ok 6.7s. Closure report's count=5 result (`internal/agents 1252.061s`) is the authoritative count=5 datum |
| `go build ./...` | PASS | Zero output |
| CLAUDE.md only-additions diff | PASS | Single touching commit since Fix #8e/#8f is `0f47a36` (D0-A); diff is one insertion hunk at line 27→28-68. Zero deletions. Zero modifications. Verified independently by `git show 0f47a36 -- CLAUDE.md` |
| CLAUDE.md prior invariants intact | PASS | `rows.Scan` + `rows.Err` invariant present (CLAUDE.md:12, hard "MUST" + "rejected by the regression test" language); `exec.CommandContext` invariant present (CLAUDE.md:13, both cheat-shapes named); Pattern P7 invariant present (CLAUDE.md:11). MUST count: pre-D0 21 → post-D0 24 (3 added by new section; zero existing MUSTs softened to "should") |
| `make smoke` | PASS | All packages "ok" (~30s wall) |
| `make test-audit` | PASS | `TestNoAuditSkipMarkersRemain ok 0.301s` |
| Cheats observed | NONE | No new fabricated-Background; no CLAUDE.md weakening; no bare-terminator regression; no AUDIT-skip-marker reintroduction |

**Verdict: ACCEPTED**

---

## Anti-cheat audit (D0-specific)

| # | Cheat category | Status | Evidence |
|---|---|---|---|
| 1 | `Client` exported as struct rather than interface | NOT OBSERVED | All six `client.go` files declare `type Client interface { ... }`; Pattern P16 Phase 1 actively enforces |
| 2 | Concrete in-process type exported (uppercase first letter) | NOT OBSERVED | `internal/clients/librarian/inprocess.go:22` is `type inProcessClient struct` (lowercase); same lowercase pattern across all five future-service stubs |
| 3 | Partial Librarian migration / aliased-import evasion | NOT OBSERVED | Post-migration grep returns 0 hits; aliased-import grep (`grep '"force-orchestrator/internal/agents/librarian"' internal/agents/ cmd/`) returns 0 hits; three documented pre-migration sites all migrated |
| 4 | Future-service `inprocess.go` returns silent no-op | NOT OBSERVED | All 22 method bodies across the five stubs return `ErrNotImplemented` in the error slot; sub-agent 3 audited each method individually |
| 5 | Pattern P16 trivially passing (Skip / empty body / file existence) | NOT OBSERVED | 263-line AST-walking test with real `t.Errorf` assertions at lines 81 and 185-188; `t.Fatalf` guard against missing fixtures at lines 65-69 |
| 6 | CLAUDE.md section title correct but body soft | NOT OBSERVED | Section uses "MUST" (4×), "forbidden", "never" — hard language throughout |
| 7 | CLAUDE.md missing BoS-rule graduation reference | NOT OBSERVED | `CLAUDE.md:64-67` names "BOS-CLIENTS-001 or similar" with explicit D4 graduation timing |
| 8 | Migration moves Librarian functions but keeps them as package-level | NOT OBSERVED | Sub-agent 2 confirmed every Client-interface method is on receiver `(c *inProcessClient)`; only package-level functions in `inprocess.go` are `NewInProcess` (factory, intentional) and three unexported helpers (`encodeWritePayload`, `normalizeUpdateField`, `scanMemoryRows`) |

---

## CLAUDE.md diff review

```
$ git log --oneline -- CLAUDE.md | head -3
0f47a36 feat(D0-A): internal/clients/ package structure + CLAUDE.md interface invariant
2de29ea fix: 8e Tracks A/B/C/D/E — daemon-ctx threading + rows.Err() sweep + P11 tightening
a346273 fix: 8d Tracks F/G/H/I — test-quality, cmd/force race, Chancellor, schema/time

$ git show 0f47a36 -- CLAUDE.md | head -100
```

The single CLAUDE.md commit since Fix #8e/#8f is `0f47a36` (D0-A). The
diff is one `@@ -25,6 +25,47 @@` hunk: 41 lines inserted between the
existing "Shell-boundary validators (Fix #9)" bullet and the existing
"## Dashboard invariants (Fix #2)" section. **Zero deletions. Zero
modifications.** The new section uses MUST / forbidden / never (hard
language). The MUST count rose from 21 → 24 (three new MUSTs added by
the section; zero existing MUSTs softened to "should").

The rows.Scan + rows.Err invariant (CLAUDE.md:12), the exec.CommandContext
invariant (CLAUDE.md:13), and the Pattern P7 state-transition guard
(CLAUDE.md:11) are byte-identical to their pre-D0 form.

---

## Forensic appendix

No failed checks. No suspect findings.

Out-of-scope notes (not verdict-affecting):
- A benign macOS linker warning ("malformed LC_DYSYMTAB, expected 98
  undefined symbols ...") accompanies the `internal/clients/librarian`
  test build. This is a Darwin/Apple-toolchain artefact unrelated to
  D0; the test process itself runs and reports `ok`. Not a verdict
  consideration.
- `experiments.Apply` (`internal/clients/experiments/inprocess.go:16`)
  returns `(call, nil, ErrNotImplemented)` — the input descriptor is
  echoed back and the second return is nil. Sub-agent 3 flagged this
  as borderline but ACCEPTED it because the error slot is non-nil and
  every conventional Go caller checks `err` first; a caller that
  ignores `err` and consumes the assignment slice would get nil-not-
  silent-empty, which is also an immediate panic-on-len-or-range or a
  no-op iteration — i.e. failure mode is loud, not silent. Not a
  verdict consideration.
- `internal/agents/librarian.go` (the SpawnLibrarian consumer-side
  agent) remains in `internal/agents/`. This is the documented design
  per `DELIVERABLE-0-CLOSURE.md:67-71` and the closure-report Residuals
  section: producer-side moved to `internal/clients/librarian/`,
  consumer-side stays as an agent. Not a verdict consideration.

---

## Final verdict (re-stated)

**GO.** D0 — Interface Layer Foundation — closes; D1 may proceed
against `capabilities.Client` (and the rest of the stub interfaces)
on the foundation D0 establishes.
