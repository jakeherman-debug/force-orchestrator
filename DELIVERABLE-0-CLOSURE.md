# DELIVERABLE-0-CLOSURE.md — Interface Layer Foundation

**Status:** GO. All four tracks merged to main; all eight exit criteria
satisfied; full suite green at `-race -count=5`; `make smoke` and
`make test-audit` green; Pattern P16 enforces the invariant going
forward.

**Verdict:** D0 closed. Operator review of this report unlocks D1.

---

## Per-track summary

| Track | Branch | Track SHA | Merge SHA | Files added/modified |
|---|---|---|---|---|
| D0-A | `deliverable/0/A` | `0f47a36` | `c65d12f` | `.gitignore`, `CLAUDE.md`, 6 × `.gitkeep` |
| D0-B | `deliverable/0/B` | `12b67e8` | `c82d8e9` | `internal/clients/librarian/{client,inprocess,mock,client_test}.go` + 19 modified files (call-site migration + test updates) |
| D0-C | `deliverable/0/C` | `6288f8b` | `fdd904c` | 5 services × 4 files (`client.go` + `inprocess.go` + `mock.go` + `client_test.go`); 5 `.gitkeep` deletions |
| D0-D | `deliverable/0/D` | `0c3a486` | `118eb84` | `internal/audittools/audit_pattern_p16_clients_interfaces_test.go` |

Worktree directories used:
- `.build-worktrees/D0-A/`
- `.build-worktrees/D0-B/`
- `.build-worktrees/D0-C/` (parallel-safe with D0-B; no conflicts on rebase)
- `.build-worktrees/D0-D/`

**Merge order observed:**
1. D0-A merged first (foundation: directories + CLAUDE.md invariant) — `c65d12f`
2. D0-B and D0-C merged in either order; both rebased on top of D0-A — `c82d8e9`, `fdd904c`
3. D0-D merged last with both prior tracks present so Pattern P16 had every interface to validate — `118eb84`

---

## Pre-migration Librarian call-site survey (from D0 pre-flight)

```
$ grep -rn "librarian\.\|GetFleetMemory\|WriteMemory" --include="*.go" \
    internal/agents/ | grep -v _test.go
internal/agents/pilot_draft_watch.go:196:	if _, err := store.AddBountyTx(tx, 0, "WriteMemory", memoryPayload); err != nil {
internal/agents/pilot_draft_watch.go:197:		return fmt.Errorf("queue WriteMemory: %w", err)
internal/agents/librarian.go:42:// writeMemoryPayload is the JSON structure placed in WriteMemory bounty payloads by jedi_council.
internal/agents/librarian.go:69:		bounty, claimed := store.ClaimBounty(db, "WriteMemory", name)
internal/agents/librarian.go:80:	logger.Printf("Librarian claimed WriteMemory #%d", bounty.ID)
internal/agents/librarian.go:91:		logger.Printf("WriteMemory #%d: invalid payload JSON: %v — failing task to avoid poisoning memory index", bounty.ID, err)
internal/agents/librarian.go:92:		if failErr := store.FailBounty(db, bounty.ID, fmt.Sprintf("librarian: invalid WriteMemory payload JSON: %v", err)); failErr != nil {
internal/agents/librarian.go:93:			logger.Printf("WriteMemory #%d: FailBounty after bad payload failed: %v — stale-lock detector will recover", bounty.ID, failErr)
internal/agents/librarian.go:98:	// parentID is the original task (CodeEdit) that this WriteMemory was spawned from.
internal/agents/librarian.go:122:		logger.Printf("WriteMemory #%d: Claude failed (%v) — using fallback summary", bounty.ID, err)
internal/agents/librarian.go:138:		logger.Printf("WriteMemory #%d: Completed status update failed: %v — memory was stored; stale-lock detector will recover", bounty.ID, err)
internal/agents/librarian.go:141:	logger.Printf("WriteMemory #%d: memory stored for parent task #%d (repo: %s, tags: %s)",
internal/agents/pr_flow.go:564:	if _, err := store.AddBountyTx(tx, pr.TaskID, "WriteMemory", memoryPayload); err != nil {
internal/agents/pr_flow.go:565:		return fmt.Errorf("queue WriteMemory for task %d: %w", pr.TaskID, err)
internal/agents/jedi_council.go:357:			// the rest. WriteMemory is spawned later when the sub-PR actually merges.
internal/agents/jedi_council.go:506:		store.AddBounty(db, b.ID, "WriteMemory", string(writeMemJSON))
```

The actual production-code migration targets (call sites that
*queue* a WriteMemory bounty, vs. the consumer side in
`librarian.go`):

| Site | Pre-D0-B form | Post-D0-B form |
|---|---|---|
| `pilot_draft_watch.go:196` (in tx) | `store.AddBountyTx(tx, 0, "WriteMemory", payloadJSON)` | `lib.WriteMemoryTx(ctx, tx, librarian.Memory{…})` |
| `pr_flow.go:564` (in tx) | `store.AddBountyTx(tx, pr.TaskID, "WriteMemory", payloadJSON)` | `lib.WriteMemoryTx(ctx, tx, librarian.Memory{…})` |
| `jedi_council.go:506` (no tx) | `store.AddBounty(db, b.ID, "WriteMemory", payloadJSON)` | `cfg.Librarian.WriteMemory(ctx, librarian.Memory{…})` |

`internal/agents/librarian.go` is the **consumer side** — the
SpawnLibrarian agent claim loop that processes queued WriteMemory
bounties. It stays in `internal/agents/` because it's an agent, not
a service client; the new `internal/clients/librarian/` package is the
**producer side** that other agents queue work through.

---

## Post-migration call-site verification

```
$ grep -rn "librarian\." --include="*.go" internal/agents/ | grep -v _test.go \
    | grep -v "cfg\.Librarian\|librarian\.Client\|librarian\.NewInProcess\|librarian\.NewMock\|librarian\.Memory\|librarian\.Scope\|lib librarian\.Client\|Librarian librarian\.Client" | wc -l
0
```

Zero hits. Every `librarian.*` reference in production agent code
goes through the interface (`librarian.Client`), the factory
functions (`librarian.NewInProcess`, `librarian.NewMock`), the data
types (`librarian.Memory`, `librarian.Scope`), or appears as a
parameter / field type (`lib librarian.Client`, `Librarian librarian.Client`).

A second sanity check — direct `store.AddBounty(...,"WriteMemory",...)` /
`store.AddBountyTx(...,"WriteMemory",...)` in production code:

```
$ grep -rn "WriteMemory" --include="*.go" internal/agents/ \
    | grep "store.AddBounty\|store.AddBountyTx" | grep -v _test.go
(no matches)
```

Zero hits. The migration is complete.

---

## Each interface defined

### `internal/clients/librarian/client.go` (D0-B — implemented in-process)

Methods (full signatures):

```go
type Client interface {
    GetMemoriesForTask(ctx context.Context, taskID int) ([]Memory, error)
    GetMemoriesByScope(ctx context.Context, scope Scope) ([]Memory, error)
    WriteMemory(ctx context.Context, memory Memory) (int, error)
    WriteMemoryTx(ctx context.Context, tx *sql.Tx, memory Memory) (int, error)
    UpdateMemory(ctx context.Context, memoryID int, update MemoryUpdate) error
    RemoveMemory(ctx context.Context, memoryID int) error
}
```

Supporting types: `Memory`, `Scope`, `MemoryUpdate`. Sentinel errors:
`ErrTxNotSupported`, `ErrEmptyScope`, `ErrNotFound`, `ErrInvalidLimit`.

### `internal/clients/capabilities/client.go` (D0-C — D1 fills bodies)

```go
type Client interface {
    LoadProfile(ctx context.Context, agentName string) (*Profile, error)
    AllowedTools(ctx context.Context, agentName string) ([]string, error)
    DisallowedTools(ctx context.Context, agentName string) ([]string, error)
    MCPConfigPath(ctx context.Context, agentName string) (string, error)
}
```

Supporting type: `Profile`. Sentinel errors: `ErrProfileNotFound`,
`ErrNotImplemented`.

### `internal/clients/experiments/client.go` (D0-C — D3 fills bodies)

```go
type Client interface {
    Apply(ctx context.Context, call CallDescriptor) (CallDescriptor, []Assignment, error)
    Outcome(ctx context.Context, experimentID int) (Outcome, error)
    Register(ctx context.Context, exp ExperimentDecl) (int, error)
    Cancel(ctx context.Context, experimentID int, reason string) error
}
```

Supporting types: `CallDescriptor`, `Assignment`, `Outcome`,
`ExperimentDecl`. Sentinel errors: `ErrExperimentNotFound`,
`ErrBudgetExhausted`, `ErrNotImplemented`.

### `internal/clients/rules/client.go` (D0-C — D3 fills bodies)

```go
type Client interface {
    ActiveRules(ctx context.Context, agent, category string) ([]Rule, error)
    RuleByKey(ctx context.Context, ruleKey string) (Rule, error)
    PromoteFromExperiment(ctx context.Context, experimentID int, p PromotionRequest) (Rule, error)
    Retire(ctx context.Context, ruleKey, reason string) error
}
```

Supporting types: `Rule`, `PromotionRequest`. Sentinel errors:
`ErrRuleNotFound`, `ErrInvalidPromotion`, `ErrNotImplemented`.

### `internal/clients/metrics/client.go` (D0-C — D3 fills bodies)

```go
type Client interface {
    RegisterMetric(ctx context.Context, metric MetricVersion) error
    Score(ctx context.Context, runID int, metricName, version string) (float64, error)
    RecordScore(ctx context.Context, runID int, metricName, version string, score float64) error
    ListMetrics(ctx context.Context) ([]MetricVersion, error)
}
```

Supporting type: `MetricVersion`. Sentinel errors: `ErrNoScore`,
`ErrMetricExists`, `ErrNotImplemented`.

### `internal/clients/graph/client.go` (D0-C — D8 fills bodies)

```go
type Client interface {
    Consumers(ctx context.Context, symbol Symbol) ([]Consumer, error)
    Definers(ctx context.Context, symbol Symbol) ([]Symbol, error)
    BlastRadius(ctx context.Context, modifiedSymbol Symbol) (BlastRadius, error)
    IndexHealth(ctx context.Context) (Health, error)
}
```

Supporting types: `Symbol`, `Consumer`, `BlastRadius`, `Health`.
Sentinel errors: `ErrSymbolNotFound`, `ErrIndexNotReady`,
`ErrNotImplemented`.

---

## Pattern P16 test output

```
$ go test -tags sqlite_fts5 -race -count=5 -run TestPattern_P16 \
    -timeout 60s ./internal/audittools/...
ok  	force-orchestrator/internal/audittools	2.067s
```

RGR red-phase verified during D0-D development by injecting a probe
violation:

```
var _p16RedProbe = &librarian.MockClient{}
```

…into `internal/agents/jedi_council.go`. Test correctly failed:

```
--- FAIL: TestPattern_P16_ClientsInterfaces (0.01s)
    audit_pattern_p16_clients_interfaces_test.go:185: Pattern P16 (D0): 1 agent file(s) construct a concrete client struct from internal/clients/<svc>/. Use the package's NewInProcess / NewGRPC / NewMock factory function instead — agents depend on the interface, never on the implementation type:
    audit_pattern_p16_clients_interfaces_test.go:187:   internal/agents/jedi_council.go:21 — &librarian.MockClient{...}  (or librarian.MockClient{...})
```

Probe reverted before commit; final tree green at `-race -count=5`.

---

## Verification of all 8 exit criteria

### (1) `internal/clients/` exists with subdirectories per service

```
$ ls internal/clients/
capabilities
experiments
graph
librarian
metrics
rules
```

✓ Six service directories present.

### (2) Every `<service>/client.go` defines `Client` as an interface

```
$ for d in librarian capabilities experiments rules metrics graph; do \
    awk '/^type Client / {print FILENAME":"NR": "$0}' \
      internal/clients/$d/client.go; done
internal/clients/librarian/client.go:33: type Client interface {
internal/clients/capabilities/client.go:30: type Client interface {
internal/clients/experiments/client.go:28: type Client interface {
internal/clients/rules/client.go:23: type Client interface {
internal/clients/metrics/client.go:23: type Client interface {
internal/clients/graph/client.go:25: type Client interface {
```

✓ Every `Client` is `interface`. Pattern P16 (Phase 1) verifies this
at CI time — see test output above.

### (3) Librarian call sites migrated; daemon wires `librarian.NewInProcess(db)`

Three production sites migrated (table above). Daemon wiring:

```
$ grep -n "librarian.NewInProcess\|JediCouncilConfig\|InquisitorConfig" \
    cmd/force/fleet_cmds.go
158:	libClient := librarian.NewInProcess(db)
185:	go agents.SpawnJediCouncil(ctx, db, agents.JediCouncilConfig{Name: name, Librarian: libClient})
229:	go agents.SpawnInquisitor(ctx, db, agents.InquisitorConfig{Librarian: libClient})
306:	go agents.SpawnJediCouncil(ctx, db, agents.JediCouncilConfig{Name: name, Librarian: libClient})
```

The daemon constructs one in-process client at startup and threads it
into every Spawn that needs it (JediCouncil for runCouncilTask;
Inquisitor for RunDogs → dogSubPRCIWatch / dogDraftPRWatch). Two CLI /
dashboard one-shot dog runs (`force dogs run <name>` and the dashboard
"Run now" button) construct their own clients at the entry point —
those are not agents, so Pattern P16 doesn't gate them.

### (4) CLAUDE.md "Cross-agent service interfaces" invariant section

```
$ grep -n "^## Cross-agent service interfaces" CLAUDE.md
28:## Cross-agent service interfaces
```

✓ Section present (D0-A). It names the directory pattern, the
constructor convention, the Pattern P16 enforcement, and the BoS rule
graduation in D4 — satisfying the anti-cheat "must be specific"
directive.

### (5) `TestPattern_P16_ClientsInterfaces` green at `-race -count=5`

```
$ go test -tags sqlite_fts5 -race -count=5 -run TestPattern_P16 \
    -timeout 60s ./internal/audittools/...
ok  	force-orchestrator/internal/audittools	2.067s
```

✓ Green.

### (6) `go test -tags sqlite_fts5 -race -count=5 ./...` green, no flakes

```
$ go test -tags sqlite_fts5 -race -count=5 ./...
ok  	force-orchestrator/cmd/force	27.085s
ok  	force-orchestrator/internal/agents	1252.061s
ok  	force-orchestrator/internal/audittools	7.884s
ok  	force-orchestrator/internal/claude	12.201s
ok  	force-orchestrator/internal/clients/capabilities	4.198s
ok  	force-orchestrator/internal/clients/experiments	4.608s
ok  	force-orchestrator/internal/clients/graph	2.622s
ok  	force-orchestrator/internal/clients/librarian	2.230s
ok  	force-orchestrator/internal/clients/metrics	2.904s
ok  	force-orchestrator/internal/clients/rules	3.199s
ok  	force-orchestrator/internal/dashboard	6.236s
ok  	force-orchestrator/internal/gh	1.974s
ok  	force-orchestrator/internal/git	113.564s
ok  	force-orchestrator/internal/store	15.681s
ok  	force-orchestrator/internal/telemetry	3.203s
```

✓ Every package green at `-race -count=5`. No flakes.

### (7) `make smoke` and `make test-audit` green

```
$ make smoke
ok  	force-orchestrator/internal/clients/graph	3.217s [no tests to run]
ok  	force-orchestrator/internal/clients/librarian	2.541s [no tests to run]
ok  	force-orchestrator/internal/clients/metrics	2.321s [no tests to run]
ok  	force-orchestrator/internal/clients/rules	1.727s [no tests to run]
ok  	force-orchestrator/internal/dashboard	3.848s
ok  	force-orchestrator/internal/gh	3.856s [no tests to run]
ok  	force-orchestrator/internal/git	2.626s
ok  	force-orchestrator/internal/store	3.284s [no tests to run]
ok  	force-orchestrator/internal/telemetry	3.682s [no tests to run]

$ make test-audit
go test -tags sqlite_fts5 -timeout 60s -run '^TestNoAuditSkipMarkersRemain$' -count=1 ./internal/audittools
ok  	force-orchestrator/internal/audittools	0.373s
```

✓ Both green. (`make fuzz` is not on D0's exit-criteria list, but
the fuzz targets cover validators that D0-B did not touch — re-running
them was not gated on this deliverable.)

### (8) No regression of Fix #8d / #8e / #8f closures

```
$ grep -rn 't\.Skip(.AUDIT-' --include="*.go" .
(no matches)

$ grep -A 2 "remainingAuditSkips = map" internal/audittools/audittools_test.go | head -5
var remainingAuditSkips = map[string]string{
	// AUDIT-011, AUDIT-025, AUDIT-085, AUDIT-149: closed by Campaign 2
…
```

The map body still contains only commentary — no live entries. The
full pattern test suite (P1, P1.1, P3, P7, P11, P12, plus the new
P16) is green; coverage for the Fix #8 invariants (rows.Scan check,
rows.Err check, exec.CommandContext threading, LLM-prompt discipline)
all stayed valid through the call-site migrations.

---

## Anti-cheat self-check

| Directive | Status |
|---|---|
| **No partial migration.** Every Librarian call site migrated. | ✓ Three sites — `pilot_draft_watch.go:196`, `pr_flow.go:564`, `jedi_council.go:506` — all routed through `librarian.Client`. Post-migration grep returns 0 hits for the old `store.AddBounty(...,"WriteMemory",...)` shape in production code. |
| **No re-export of concrete types as `Client`.** | ✓ Every `clients/<svc>/client.go` declares `Client` as an interface. Concrete backings are unexported (`inProcessClient`); `MockClient` is exported by-design (test fixture) but not named `Client`. Pattern P16 Phase 1 enforces. |
| **No "use the interface for new code, leave old code alone" exemption.** | ✓ Every existing call site migrated in the same commit that introduced the interface. Pattern P16 will fail any future direct call regardless of whether it predates D0. |
| **No skipping interface stubs in D0-C.** | ✓ All five planned future interfaces (capabilities, experiments, rules, metrics, graph) shipped as stubs in this deliverable. None deferred to the implementing deliverable. |
| **No silent permissive in-process client.** | ✓ Every D0-C `inprocess.go` returns `ErrNotImplemented` from every method. A caller reaching one before D1/D3/D8 lands gets a real error. Verified by `TestInProcess_StubReturnsErrNotImplemented` in each service package. |
| **CLAUDE.md invariant must be specific.** | ✓ The "Cross-agent service interfaces" section names: the directory pattern (`internal/clients/<service>/`), the factory-function constructor convention (`NewInProcess` / `NewGRPC` / `NewShared` / `NewMock`), the Pattern P16 enforcement test (with full path), AND the BoS rule graduation in D4 (BOS-CLIENTS-001 or similar). |

---

## Forward note: which deliverable owns which implementation

| Service | Owning deliverable | Status after D0 | What lands later |
|---|---|---|---|
| `librarian` | D0-B (this deliverable) | **Implemented.** Production daemon and CLI/dashboard one-shots wire `librarian.NewInProcess(db)` today. Existing FleetMemory queue semantics preserved. | D-Lib (future, gated on triggers) ships `librarian.NewGRPC(...)` as a sibling implementation. Daemon startup wires the gRPC client; no agent code changes. |
| `capabilities` | D1 (capability profiles, T0-1) | Stub — `NewInProcess()` returns `ErrNotImplemented`. | D1 fills `inprocess.go` with `agent_profiles/<agent>.toml` reads. Astromech / Captain / Medic claim-time prompts pull tool allowlists / MCP configs / system-prompt fragments through the interface. |
| `experiments` | D3 (paired-runs + Engineering Corps) | Stub. | D3 fills `inprocess.go` with the treatment-application machinery. Engineering Corps's hot path (`treatments.Apply`) routes through `experiments.Client` from day 1 of D3. |
| `rules` | D3 (paired-runs + Engineering Corps) | Stub. | D3 fills `inprocess.go` with the FleetRules table reads. Captain / Council / Medic consume `ActiveRules(ctx, agent, category)` at claim time — promoted-from-experiment behaviour rules graduate from "winning paired run" to "fleet default." |
| `metrics` | D3 (paired-runs + Engineering Corps) | Stub. | D3 fills `inprocess.go` with versioned-metric storage and per-run scoring. `experiments.Outcome` and the operator dashboard score charts both read from this. |
| `graph` | D8 (cross-repo graph) | Stub. | D8 fills `inprocess.go` with the LSP-driven extractor + SQLite-backed graph store. Agents query `Consumers / Definers / BlastRadius` for change-impact analysis at claim time. |

When D4 ships, Pattern P16 graduates to a BoS rule (BOS-CLIENTS-001 or
similar); until then, the CI-time AST test in
`internal/audittools/audit_pattern_p16_clients_interfaces_test.go` is
the only enforcement. The directive in CLAUDE.md notes the
graduation explicitly so no one forgets.

---

## Residuals (none scope-shifting)

- `internal/agents/librarian.go` (the SpawnLibrarian consumer loop)
  stays exactly where it was. It's an agent, not a service client; it
  consumes WriteMemory bounties produced by `internal/clients/librarian/`.
  Future cleanup: the consumer side could move to
  `internal/clients/librarian/server.go` if Force ever needs an
  out-of-process Librarian, but that's D-Lib's call, not D0's.
- The `_ = status` and similar dead lines were not introduced by D0;
  the existing tree's style stayed unchanged.
- The `librarian.Client` interface includes `WriteMemoryTx(ctx, *sql.Tx,
  Memory)` for the in-process atomic-finalisation paths
  (terminalConvoyTransitionTx, onSubPRMerged). This is a leaky abstraction
  toward the in-process backing — a remote (gRPC) backing must return
  `ErrTxNotSupported`. D-Lib will need to either restructure those callers
  to commit-then-queue, or ship a different transport that supports
  cross-process transactions. Documented in client.go's interface comments.

---

**Operator action:** review this report, then explicitly authorise D1
in a separate session. D1 reads the `capabilities.Client` interface
defined here and builds the real in-process implementation against it.
