---
audience: both
scope: Strategic deliverable list — D0 through D13+ with merge order, exit criteria, and closure pointers.
owner: D13
last_reviewed: 2026-05-05
---

# Roadmap

Ten deliverables that take Force from post–Code-Red closure through its next-generation architecture. Each deliverable is written to be handed directly to a Claude Code agent as a work brief: required reading, scope, file paths, exit criteria as grep-verifiable checks, anti-cheat directives, verification procedure, and a closure-report artifact.

**Deliverables execute in strict sequential order** (D1 → D10). A deliverable may not begin until its predecessor is merged to main and its closure report is filed. No exceptions.

**Within a deliverable, tracks run in parallel isolated git worktrees** where the dependency graph permits. Every deliverable with more than one track has a **Merge order** subsection specifying which tracks block which, which tracks can run concurrently, and which must land on main first.

The full ordered sequence of track-level merges across the entire roadmap is listed in the [Merge-order appendix](#merge-order-appendix) at the end of this document. That appendix is the authoritative reference for "in what order does work land on main."

## Philosophy

**Risks close before features land.** Known-unknowns that could compound into future incidents take priority over features that would look good in a demo. Code Red cost three hundred dollars in two hours; the premise is that the next incident will cost more, and the only defense is closing risks as they surface.

**Measurement infrastructure is not a feature.** Paired runs, Engineering Corps, and the global holdout (Deliverable 3) are the substrate under every subsequent deliverable. Without them, rule promotions land on intuition and we can't tell whether the last three months of changes made the fleet better or merely different. They ship mid-roadmap, not late.

**Every feature is evidence-gated after D3.** From Deliverable 4 forward, nothing promotes to fleet-wide default without a paired-run demonstrating measurable improvement. This is how the fleet gets better instead of drifting.

**Build cost is autonomous-agent time, not human SWE weeks.** Force is built by Force; agents do the implementation under operator supervision. Build estimates throughout this document are in autonomous-agent-build hours plus operator ratification time, not the SWE-week framing typical of human-engineered projects. Decisions about what to build should weigh: architectural complexity (more moving parts = more failure surface), operator-ratification burden (does this proposal create review queue items?), and per-agent token cost (does this scale prompt size?) — NOT time-to-implement-in-SWE-weeks. The exception is D7 (model-tier experiments) where the cost is experiment runtime — paired-runs need data accumulation, which is wall-clock real even if the implementation is fast.

## How to use this document

Each deliverable section is self-contained. To hand off a deliverable:

1. **Verify the predecessor deliverable is merged to main and its closure report is filed.** This is the gate; do not begin work otherwise.
2. Direct the agent to read this document in full, then re-read their specific deliverable section.
3. Have the agent read every document in the "Required reading" list.
4. The agent executes work tracks per the section's Merge order table. Tracks marked as parallel-eligible may be handed to different agents working in separate worktrees; tracks with a blocking predecessor wait for that predecessor's merge to main.
5. The agent runs the verification procedure before declaring done.
6. The agent files the closure-report artifact (name specified per deliverable) under `docs/closures/`.

The operator ships the deliverable only when the verification procedure passes cleanly AND the closure report is filed AND every track in the deliverable is merged to main.

## Worktree discipline (mandatory)

Every track runs in a git worktree. Non-negotiable.

**Directory convention.** All build worktrees live under `.build-worktrees/` at the repo root:
- Format: `.build-worktrees/D<n>-<track-id>/`
- Examples: `.build-worktrees/D1-T0-1/`, `.build-worktrees/D2-T1-0/`, `.build-worktrees/D8-graph/`
- Worktrees are disposable. When a track merges, its worktree is deleted.

**Branch convention.** One branch per track: `deliverable/<N>/<track-id>`.
- Examples: `deliverable/1/T0-1`, `deliverable/2/T1-0`, `deliverable/8/integtest`.

**Checkout source.** A track's worktree is checked out from main at the moment the agent begins — unless the track has a blocking predecessor, in which case it is checked out from main AFTER the predecessor merges.

**Rebase, never merge.** Never `git merge main` into a track branch. Always `git rebase main` to pick up upstream work. Keeps history linear and the merge-order reasoning intact.

**Conflict resolution is the agent's job.** If a rebase produces conflicts with a track that merged ahead, the agent resolves them in its worktree before the track's own merge. Do not leave conflicts for the operator to resolve.

**One track, one PR.** Each track becomes exactly one PR at merge time. Do not bundle multiple tracks into one PR even when they share a deliverable.

**Merge to main.** Each track's PR merges directly to main. There is no long-lived "deliverable" branch; the deliverable is "complete" when all its constituent tracks have merged to main and the closure report is filed.

**Never use `--no-verify`. Never use `git commit --amend` after a pre-commit hook failure.**

**Parallelism rule.** Two tracks may be claimed concurrently (by different agents in separate worktrees) if and only if neither names the other as a blocking predecessor in its Merge order table. Otherwise, the dependent track waits for the predecessor's merge to main — not the predecessor's "almost done" — before starting.

**RGR discipline** (applies to every deliverable):
- **Red.** Confirm a test fails before the fix. Capture the failure message.
- **Green.** Implement. Confirm the test passes at `-race -count=5` where applicable.
- **Refactor.** If the fix introduces duplication, clean it up. Tests must still pass.

**Universal anti-cheat directives** (apply to every deliverable, in addition to per-deliverable ones):
- No `t.Skip("AUDIT-...")` or `t.Skip("D{N}-...")` without an entry on the `internal/audittools/audittools_test.go` allowlist and a concrete follow-up plan. Fix #8d drove the AUDIT allowlist to empty; do not refill it.
- No ghost functions. A test referencing a function by name in its skip message means the function must exist and be in use by the closing commit.
- No softened assertions. If a red-phase test catches a defect, the green-phase test must assert the post-fix contract at least as tightly.
- No `_ = store.*` discards without a `// deferral-comment(Fix #8b): propagate error — <mechanism>` marker per CLAUDE.md.
- No reduction of `-race -count` below 5 on any new concurrency-sensitive test.
- No fix lands without a test added or unskipped in the same commit.
- No scope-creep fixes unilaterally. If you find something outside scope that needs attention, surface it in the closure report's Residual section for operator decision.

## Deliverable-level dependency graph

```
  D0 ──► D1 ──► D2 ──► D3 ──► D4 ──► D5 ──► D6 ──► D7 ──► D8 ──► D9 ──► D10
  │      │      │      │      │      │      │      │      │      │       │
  │      │      │      │      │      │      │      │      │      │       │
  [gate] [gate] [gate] [gate] [gate] [gate] [gate] [gate] [gate] [gate]   │
                                                                        [gate]
```

Each `[gate]` is a hard cutover: deliverable N+1 does not begin until deliverable N is merged to main AND its `DELIVERABLE-N-CLOSURE.md` is filed.

D0 is the architectural foundation: establishes the interface layer between agents and services so every subsequent deliverable builds against it. Done first because the daemon is currently stopped (post-Fix-#8f); this is the cleanest possible window for an architectural refactor with zero production traffic flowing through the migrated code paths.

D3 is the load-bearing functional node. Every deliverable after it ultimately depends on the paired-runs + Engineering Corps substrate landing.

The topological dependency is strictly linear (D0 → D1 → D2 → ... → D10) by operator decree. Technically D6 depends only on D4 and D7 depends only on D3, so in principle they could parallelize; the operator has chosen strict serialization for attention-budget and rollback-simplicity reasons.

---

## Deliverable 0 — Interface Layer Foundation

### Mission

Establish `internal/clients/<service>/` as the standard pattern for cross-agent service dependencies, BEFORE any subsequent deliverable adds more direct calls. Every cross-agent service interaction goes through a Go interface (the "port") with one or more pluggable implementations (the "adapters"). Today's adapters are in-process; future adapters (gRPC service, shared multi-tenant service, polyglot bridge) become a matter of writing a new implementation file rather than refactoring agent code.

This is the architectural foundation for: D-Lib (future Librarian service carve-out, when triggered); multi-tenant Force operation (future product); polyglot agent implementations (future); and clean unit-testing across the fleet via mock implementations.

### Classification

Architectural foundation. Pre-D1 because every deliverable from D1 forward will introduce or evolve services; without the pattern established first, each would bake in direct calls that have to be migrated later at substantially higher cost.

### Required reading

1. `/Users/jake.herman/code/force-orchestrator/CLAUDE.md` — full.
2. `/Users/jake.herman/code/force-orchestrator/docs/roadmap.md` — this document, especially the D3 section (most future services land there) and the D8 section (graph as a future client).
3. `/Users/jake.herman/code/force-orchestrator/internal/agents/librarian.go` — current Librarian functions; first interface to define.
4. Existing call-site survey: `grep -rn "librarian\." --include="*.go" internal/agents/ | grep -v _test.go` — every site that becomes an interface call.

### Prerequisites

None. The daemon is stopped post-Fix-#8f; this is the cleanest window.

### Merge order within D0

| Order | Track | Branch | Depends on | Parallelizable with | Worktree |
|---|---|---|---|---|---|
| 1 | D0-A — Interface package structure + naming conventions + CLAUDE.md invariant | `deliverable/0/A` | — | — | `.build-worktrees/D0-A/` |
| 2 | D0-B — Librarian interface + in-process implementation + call-site migration | `deliverable/0/B` | D0-A merged | D0-C | `.build-worktrees/D0-B/` |
| 3 | D0-C — Future-service interface stubs (capabilities, experiments, rules, metrics, graph) | `deliverable/0/C` | D0-A merged | D0-B | `.build-worktrees/D0-C/` |
| 4 | D0-D — Pattern P16 enforcement test | `deliverable/0/D` | D0-B + D0-C merged | — | `.build-worktrees/D0-D/` |

**Rationale.** D0-A establishes the directory layout and the CLAUDE.md invariant first so subsequent tracks have a documented standard. D0-B (Librarian — the only existing service with non-trivial migration scope) and D0-C (greenfield interface stubs for future services) are file-disjoint and parallel-safe. D0-D (Pattern P16) merges last so it has every interface to validate against; if D0-D went first, it would fail because there's nothing to enforce yet.

### Work tracks

**Track D0-A — Interface package structure + naming conventions + CLAUDE.md invariant.**

Scope:
- Create directory structure: `internal/clients/`. Subdirectories per service: `internal/clients/librarian/`, `internal/clients/capabilities/`, `internal/clients/experiments/`, `internal/clients/rules/`, `internal/clients/metrics/`, `internal/clients/graph/`. Initially most contain only `client.go` (interface) + a placeholder `inprocess.go` for tracks that own them.
- Each `client.go` follows the convention:
  ```go
  // Package <service> defines the client interface for the <service> service.
  package <service>

  import "context"

  // Client is the contract between agents and the <service> service. All
  // production agent code MUST depend on this interface, never on a
  // concrete implementation type. Implementations live as siblings:
  //   - inprocess.go — in-process backed by holocron.db (D0)
  //   - grpc.go      — gRPC client (future, when service form-factor triggers)
  //   - shared.go    — shared multi-tenant client (future)
  //   - mock.go      — for unit tests
  //
  // Pattern P16 (internal/audittools/audit_pattern_p16_clients_interfaces_test.go)
  // enforces that production agents do not import concrete implementation
  // types — only the Client interface and the corresponding NewX factory.
  type Client interface {
      // method signatures...
  }
  ```
- Add CLAUDE.md invariant section: "Cross-agent service interfaces":
  ```markdown
  ## Cross-agent service interfaces

  Cross-agent service dependencies route through Go interfaces in
  `internal/clients/<service>/`. Direct function-call dependencies between
  agents (e.g., `librarian.GetMemoriesForTask(...)` from Captain) are
  forbidden going forward.

  Pattern:
  - `internal/clients/<service>/client.go` defines the `Client` interface.
  - `internal/clients/<service>/inprocess.go` implements the in-process
    default backed by holocron.db or in-memory state. Constructed via
    `NewInProcess(...)`.
  - Additional implementations (gRPC, shared, mock) live in sibling files
    when their triggers fire. Constructed via `NewGRPC(...)`, `NewShared(...)`,
    `NewMock(...)`.
  - Agents receive `Client` instances via constructor (`Spawn<Agent>(ctx,
    cfg <Agent>Config { ..., Librarian librarian.Client, ... })`), never
    by importing a concrete struct type.

  Why: each interface is the explicit contract between agents and a service.
  When a service form-factor changes (in-process → gRPC → shared multi-tenant),
  agents are unaffected — only one implementation file changes. Tests use
  mock implementations.

  Pattern P16 (`internal/audittools/audit_pattern_p16_clients_interfaces_test.go`)
  walks production code and fails if any agent imports a concrete `*inProcessClient`
  / `*grpcClient` struct type or constructs an implementation by calling
  `&<service>.<Type>{...}` directly. Construction must go through
  `New<Impl>` factory functions; agents only see the interface type.

  This pattern WILL graduate to a BoS rule (BOS-XX) when D4 ships, providing
  commit-time enforcement in addition to the CI-time Pattern test.
  ```

Files added: `internal/clients/<service>/.gitkeep` directories, `CLAUDE.md` updated.

**Track D0-B — Librarian interface + in-process implementation + call-site migration.**

Scope:
- Survey existing Librarian call sites: `grep -rn "librarian\.\|GetFleetMemory\|WriteMemory" --include="*.go" internal/agents/ | grep -v _test.go`. Document each in the closure report.
- Define `internal/clients/librarian/client.go`:
  ```go
  package librarian

  type Client interface {
      // Read path
      GetMemoriesForTask(ctx context.Context, taskID int) ([]Memory, error)
      GetMemoriesByScope(ctx context.Context, scope Scope) ([]Memory, error)

      // Write path
      WriteMemory(ctx context.Context, memory Memory) (int, error)
      UpdateMemory(ctx context.Context, memoryID int, update MemoryUpdate) error
      RemoveMemory(ctx context.Context, memoryID int) error

      // Future: SearchSimilar; defined now in commented stub for forward-compat
  }

  type Memory struct { /* mirrors today's FleetMemory shape */ }
  type Scope struct { /* repo, agent, role, time_window */ }
  type MemoryUpdate struct { /* partial-update fields */ }
  ```
- Implement `internal/clients/librarian/inprocess.go` wrapping today's `internal/agents/librarian.go` functions. The existing functions stay (called from the new in-process client) but every external caller migrates to the interface.
- Add `internal/clients/librarian/mock.go` for unit tests with a fluent fixture API.
- Migrate every call site found in the survey. Each agent's `SpawnX` function gains a `librarian.Client` parameter in its config struct; calls become `cfg.Librarian.GetMemoriesForTask(ctx, taskID)` etc.
- Wire construction at daemon startup: `lib := librarian.NewInProcess(db)`; pass `lib` into every `Spawn*` call's config struct.

Files modified: ~5-10 agent files (call-site migration). Files added: 3 (interface + in-process + mock).

**Track D0-C — Future-service interface stubs.**

Scope:
- Define interfaces NOW for services that future deliverables will fill in:

  `internal/clients/capabilities/client.go` — for D1 T0-1 to fill:
  ```go
  type Client interface {
      LoadProfile(agentName string) (*Profile, error)
      AllowedTools(agentName string) []string
      DisallowedTools(agentName string) []string
      MCPConfigPath(agentName string) (string, error)
  }
  ```

  `internal/clients/experiments/client.go` — for D3 to fill:
  ```go
  type Client interface {
      Apply(ctx context.Context, call CallDescriptor) (CallDescriptor, []Assignment, error)
      Outcome(ctx context.Context, experimentID int) (Outcome, error)
      // additional methods stubbed; D3 fills bodies
  }
  ```

  `internal/clients/rules/client.go` — for D3 to fill (FleetRules):
  ```go
  type Client interface {
      ActiveRules(ctx context.Context, agent string, category string) ([]Rule, error)
      RuleByKey(ctx context.Context, ruleKey string) (Rule, error)
  }
  ```

  `internal/clients/metrics/client.go` — for D3 to fill:
  ```go
  type Client interface {
      RegisterMetric(ctx context.Context, metric MetricVersion) error
      Score(ctx context.Context, runID int, metricName, version string) (float64, error)
  }
  ```

  `internal/clients/graph/client.go` — for D8 to fill:
  ```go
  type Client interface {
      Consumers(ctx context.Context, symbol Symbol) ([]Consumer, error)
      BlastRadius(ctx context.Context, modifiedSymbol Symbol) (BlastRadius, error)
  }
  ```

- Each interface ships with a placeholder `inprocess.go` returning `ErrNotImplemented` for now. The owning deliverable (D1, D3, D8) fills in real implementations.
- Each ships with `mock.go` for unit testing.
- Add doc comment on each interface: which deliverable owns the implementation; expected timeline.

Files added: ~5 interfaces × 3 files (client + inprocess stub + mock) = 15 files. Each is small (~30-50 lines).

**Track D0-D — Pattern P16 enforcement test.**

Scope:
- Add `internal/audittools/audit_pattern_p16_clients_interfaces_test.go`:
  ```go
  // Walks production code (everything under internal/ except _test.go).
  // For every Go file in internal/agents/:
  //   - Parse imports. For each import of internal/clients/<service>:
  //     - The file MUST NOT reference a concrete struct type from that
  //       package (e.g., librarian.inProcessClient, librarian.grpcClient).
  //     - The file MAY reference the interface (librarian.Client),
  //       constructor function names (librarian.NewInProcess), and
  //       data types (librarian.Memory, librarian.Scope).
  //   - The file MUST NOT instantiate a concrete client via &<package>.<StructType>{...}.
  // For every internal/clients/<service>/client.go:
  //   - The exported Client type MUST be an interface, not a struct.
  // Fails with file:line + struct-type name for each offender.
  ```
- Test verifies positive cases (legitimate interface usage) and negative cases (each forbidden pattern).
- Test runs at `-race -count=5` cleanly.

Files added: 1 test file.

### Exit criteria

1. `internal/clients/` exists with subdirectories per service (librarian, capabilities, experiments, rules, metrics, graph).
2. Every `<service>/client.go` defines a `Client` interface; no concrete struct exported as `Client`.
3. `internal/agents/librarian.go` callers (every production site) migrated to use `librarian.Client` interface; daemon startup wires `librarian.NewInProcess(db)` into every `Spawn*` config struct.
4. CLAUDE.md "Cross-agent service interfaces" invariant section present and grep-discoverable.
5. `TestPattern_P16_ClientsInterfaces` green at `-race -count=5`.
6. `go test -tags sqlite_fts5 -race -count=5 ./...` green, no flakes.
7. `make smoke` / `make fuzz` / `make test-audit` all green.
8. No regression of Fix #8d / #8e / #8f closures: `grep -rn 't\.Skip(.AUDIT-' --include="*.go"` returns 0; `remainingAuditSkips` empty; full pattern test suite (P1–P12 minus P5, plus the new P16) green.

### Anti-cheat directives

- **No partial migration.** Every Librarian call site found in the survey must be migrated. A leftover direct `librarian.GetMemoriesForTask(...)` call from inside an agent file is a track failure. Pattern P16 enforces, but the migration must be COMPLETE before P16 ships green — partial green is a partial migration.
- **No re-export of concrete types.** Tempting shortcut: re-export `inProcessClient` as `Client` to avoid touching agent code. Forbidden. The interface is the only exported `Client` type; concrete implementations are unexported struct types accessed only via factory functions.
- **No "use the interface for new code, leave old code alone" exemption.** This is the once-in-a-roadmap clean window. Every existing call site migrates now. Pattern P16 will fail any future direct call regardless of whether it predates D0.
- **No skipping interface stubs.** D0-C ships ALL planned future interfaces (capabilities, experiments, rules, metrics, graph) as stubs. D1/D3/D8 fill in real implementations LATER. Don't defer interface definition to the deliverable that implements; that defeats the purpose of D0 as a foundation.
- **No silent permissive in-process client.** If `inprocess.go` returns `ErrNotImplemented` for unimplemented methods (D0-C stubs), agents calling those methods receive a real error and the calling deliverable's tests catch it. No silent no-ops.
- **CLAUDE.md invariant must be specific.** The invariant text must name the directory pattern, the constructor convention, the Pattern P16 enforcement, AND the BoS rule graduation in D4. Vague language ("agents should use interfaces where appropriate") defeats the rule.

### Verification procedure

```
# Pre-flight
go test -tags sqlite_fts5 -race -count=5 ./...   # green on starting state

# D0-A
ls internal/clients/  # expect: librarian/, capabilities/, experiments/, rules/, metrics/, graph/
grep -n "Cross-agent service interfaces" CLAUDE.md  # expect 1 hit

# D0-B
grep -rn "librarian\." --include="*.go" internal/agents/ | grep -v _test.go | grep -v "cfg\.Librarian\|librarian\.Client\|librarian\.NewInProcess\|librarian\.Memory\|librarian\.Scope"
# Expected: 0 hits — no direct calls to internal/agents/librarian.go functions remain

go test -tags sqlite_fts5 -run TestLibrarianClient -race -count=5 ./internal/clients/librarian/  # expect green

# D0-C
ls internal/clients/capabilities/ internal/clients/experiments/ internal/clients/rules/ internal/clients/metrics/ internal/clients/graph/
# Expected: each contains client.go + inprocess.go + mock.go

# D0-D
go test -tags sqlite_fts5 -run TestPattern_P16_ClientsInterfaces -race -count=5 ./internal/audittools/...  # expect green

# Full suite
go test -tags sqlite_fts5 -race -count=5 ./...  # expect green no flakes
make smoke && make fuzz && make test-audit  # all green
```

### Closure report

Produce `docs/closures/DELIVERABLE-0-CLOSURE.md` with:

- Per-track summary (D0-A, D0-B, D0-C, D0-D) with commit SHAs.
- Pre-migration call-site survey (the grep output of every direct Librarian call before D0-B).
- Post-migration call-site verification (same grep, returning 0 hits).
- Each interface defined with file path + method signatures listed.
- Pattern P16 test output verbatim.
- Verification output of all 8 exit criteria pasted verbatim.
- Anti-cheat self-check: affirm each directive was not violated.
- Forward note: list which deliverables (D1, D3, D8) own which interface implementations going forward, so the operator has a clear mapping.

### Forward integration

Once D0 ships, every subsequent deliverable inherits the interface convention. Specifically:

- **D1 T0-1** (capability profiles) builds the real `capabilities.Client` implementation; the interface is already defined.
- **D3** builds real implementations for `experiments.Client`, `rules.Client`, `metrics.Client`. Engineering Corps's hot path (`treatments.Apply`) goes through `experiments.Client` from day 1.
- **D8** builds the real `graph.Client` implementation.
- **D4** introduces a new BoS rule (BOS-CLIENTS-001 or similar) that enforces Pattern P16 at commit-time, not just CI-time. This is the user-flagged "BoS rule" graduation: until D4, Pattern P16 is the only enforcement; from D4 forward, BoS catches violations one step earlier in the pipeline.
- **D-Lib** (future, gated on triggers) ships `librarian.NewGRPC(...)` as an additional implementation. Daemon startup wires the gRPC client instead of the in-process one. No agent code changes.
- **Multi-tenant Force product** (future, far) ships `librarian.NewShared(...)`, `rules.NewShared(...)`, etc. for tenants that opt into shared services. Same pattern.

The interface layer is the cheapest possible insurance against architectural lock-in. ~3-5 days of work today saves arbitrary rewrites later.

---

## Deliverable 1 — Pre-Restart Security Closure

### Mission

Close three security gaps before the daemon restarts. Two are newly-identified (`--allowedTools` discipline, inbound secret scrubbing); one is the Fix #8d closure campaign. All three must ship before the operator considers the Code Red officially exited.

### Classification

Risk closure — blocking. Daemon stays down until this deliverable ships.

### Required reading

1. `/Users/jake.herman/code/force-orchestrator/CLAUDE.md` — full.
2. `/Users/jake.herman/code/force-orchestrator/FIX-VERIFICATION.md` — full.
3. `/Users/jake.herman/code/force-orchestrator/FIX-8D-PROMPT.md` — full (the T0-3 spec).
4. `/Users/jake.herman/code/force-orchestrator/internal/claude/claude.go` — every `AskClaudeCLI` / `RunCLIStreamingContext` function.
5. `/Users/jake.herman/code/force-orchestrator/internal/store/redact.go` — outbound pattern set.

### Prerequisites

None.

### Merge order within D1

| Order | Track | Branch | Depends on | Parallelizable with | Must merge before |
|---|---|---|---|---|---|
| 1 | T0-3 | `deliverable/1/T0-3` | — (first) | — | T0-1, T0-2 |
| 2 | T0-1 | `deliverable/1/T0-1` | T0-3 merged to main | T0-2 | — |
| 3 | T0-2 | `deliverable/1/T0-2` | T0-3 merged to main | T0-1 | — |

**Rationale.** T0-3 (Fix #8d) touches ~40+ files across `internal/` with invariant-level signature changes. Merging T0-1 or T0-2 first would force Fix #8d into a painful rebase; merging Fix #8d first forces T0-1/T0-2 into a rebase but they are file-scope-narrow and the rebase is mechanical. T0-1 and T0-2 touch mostly disjoint files (T0-1 modifies agent-side tool params; T0-2 adds new files under `internal/claude/` and `internal/repo/`), so they may be developed and merged concurrently after T0-3 lands.

### Work tracks

**Track T0-3 — Fix #8d campaign.**
Execute per `FIX-8D-PROMPT.md` verbatim. That spec is authoritative for T0-3; do not re-derive its contents here.

**Track T0-1 — Per-agent capability profiles (static, YAML-driven).**

Goal: every Claude CLI invocation receives a tool set sourced from a per-agent YAML profile, not a hardcoded literal in the call site. Static least-privilege at the agent boundary — each agent sees only the tools its declared profile permits, plus any fleet-wide blocklist applied. No dynamic capability requests, no operator-approval queue, no hook binary. Static profiles cover the realistic threat model with substantially less infrastructure than a dynamic framework would require.

**Empirical findings driving this design** (verified during planning):
- `--allowedTools` does NOT act as a hard restriction in `--dangerously-skip-permissions` mode (which we use for non-interactive operation). It's an auto-approve indicator only.
- `--disallowedTools` DOES remove tools from Claude's catalog entirely. Claude reports "the tool simply isn't in my toolset" rather than treating it as a denial.
- `--tools` controls built-in catalog visibility (built-ins only, not MCP).
- MCP tool visibility is controlled by `--mcp-config` + `--strict-mcp-config`.
- The right enforcement combination for least-privilege: build the per-agent allowed tool set, compute the complement against the full fleet vocabulary, pass the complement as `--disallowedTools`. The `--allowedTools` flag remains as auto-approve hint for documentation (and forward-compat if Anthropic ever changes the semantics).

**Files to add:**

`agents/capabilities/<agent-name>.yaml` — one per agent:

```yaml
agent: captain
description: |
  Captain reviews diffs for scope and coherence. Read-only across the
  fleet; no shell, no file writes, no destructive ops. Sees domain
  context via Atlassian/Glean/Sonar reads.
builtin_tools:
  - Read
  - Glob
  - Grep
mcp_servers:
  - mcp:atlassian-read       # Jira read; Confluence read
  - mcp:glean-read
  - mcp:sonar-read
notes: |
  Captain's prompt expects to see Jira issues for scope context; Glean
  for cross-repo search; Sonar for existing-quality findings. No
  Confluence write needed (Captain doesn't update wikis). No Bash
  (Captain reasons over textual diffs, doesn't execute).
```

One YAML per agent: `astromech`, `pilot`, `diplomat`, `captain`, `council`, `medic`, `medic-ci`, `chancellor`, `commander`, `boot`, `convoy-review`, `pr-review-triage`, `librarian`, `auditor`, `investigator`, `inquisitor`. ~16 files; each is ~30 lines.

`agents/capabilities/.forceblocklist.yaml` — fleet-wide static blocklist:

```yaml
description: |
  Tools that NO agent gets, fleet-wide, regardless of per-agent profile.
  Even if an agent's YAML accidentally lists one of these, the blocklist
  is the final word. This is the "never reachable" rail per Path 2 of
  the capability design.
blocked:
  # Confluence write — fleet doesn't authorize agents to write to wikis.
  - mcp:confluence-write
  # Slack write tools — agents notify operator via Fleet_Mail, not Slack.
  - mcp:slack-write
  # Datadog write — agents read metrics; only humans modify dashboards.
  - mcp:datadog-write
  # Atlassian destructive Jira ops — issue-create OK; transition/edit operator-only.
  - mcp__plugin_dev-tools_atlassian__transitionJiraIssue
  - mcp__plugin_dev-tools_atlassian__editJiraIssue
  # Sonar destructive ops — read-only authorization.
  - mcp__plugin_sonarqube_sonarqube__change_security_hotspot_status
  - mcp__plugin_sonarqube_sonarqube__change_sonar_issue_status
reasoning: |
  Removing an entry from this list requires operator action with an
  explicit commit + audit trail. Adding to a per-agent profile requires
  the entry to be ABSENT from this blocklist; the loader rejects
  profiles that grant a blocklisted tool.
```

`internal/agents/capabilities/loader.go` — Go loader:

```go
// Profile is the loaded form of agents/capabilities/<agent>.yaml.
type Profile struct {
    Agent        string
    BuiltinTools []string  // Read, Edit, Write, Bash, Glob, Grep, ...
    MCPServers   []string  // namespace tokens, mapped to actual MCP tool names by registry
    Description  string
}

// LoadProfile reads agents/capabilities/<agentName>.yaml + the fleet
// blocklist, validates the profile (every requested tool is real;
// no blocklisted tool is granted; required fields present), and
// returns the resolved Profile. Fails noisily on missing/invalid YAML
// — agents must not silently fall back to "all tools" if their profile
// is missing.
func LoadProfile(agentName string) (*Profile, error)

// AllowedToolsArg renders the profile's permitted tools for the
// --allowedTools CLI flag (auto-approve hint; not the actual
// restriction).
func (p *Profile) AllowedToolsArg() string

// DisallowedToolsArg renders the COMPLEMENT of the profile (every
// fleet-known tool NOT in the profile, plus every blocklist entry)
// for --disallowedTools. This is the actual restriction — tools in
// this list are removed from Claude's catalog entirely.
func (p *Profile) DisallowedToolsArg() string

// MCPConfigArg renders --mcp-config + --strict-mcp-config arguments
// to load only the MCP servers the profile names.
func (p *Profile) MCPConfigArg() (string, error)
```

The complement (`DisallowedToolsArg`) computation requires a fleet-wide tool registry — the source of truth for "every tool that exists in this fleet." Add `agents/capabilities/REGISTRY.yaml`:

```yaml
description: |
  Fleet-wide tool vocabulary. Every tool an agent could theoretically
  invoke must be listed here. The DisallowedToolsArg complement is
  computed against this registry. Adding a new MCP server or builtin
  tool requires updating this file.
builtin_tools:
  - Read
  - Glob
  - Grep
  - Edit
  - Write
  - Bash
  - WebFetch
  - WebSearch
  - LSP
mcp_tools:
  # Atlassian read
  - mcp__plugin_dev-tools_atlassian__atlassianUserInfo
  - mcp__plugin_dev-tools_atlassian__getJiraIssue
  - mcp__plugin_dev-tools_atlassian__searchJiraIssuesUsingJql
  - mcp__plugin_dev-tools_atlassian__getConfluencePage
  - mcp__plugin_dev-tools_atlassian__searchConfluenceUsingCql
  # ... full list of every MCP tool the fleet might use
mcp_namespaces:
  # Convenience namespaces for profile YAMLs to reference. Loader
  # expands these to concrete tool names against mcp_tools above.
  mcp:atlassian-read:
    - mcp__plugin_dev-tools_atlassian__atlassianUserInfo
    - mcp__plugin_dev-tools_atlassian__getJiraIssue
    - mcp__plugin_dev-tools_atlassian__searchJiraIssuesUsingJql
    - mcp__plugin_dev-tools_atlassian__getConfluencePage
    - mcp__plugin_dev-tools_atlassian__searchConfluenceUsingCql
    # ... read-only Atlassian tools
  mcp:atlassian-write:
    - mcp__plugin_dev-tools_atlassian__addCommentToJiraIssue
    - mcp__plugin_dev-tools_atlassian__createJiraIssue
    # ... write Atlassian tools
  mcp:glean-read: [...]
  mcp:sonar-read: [...]
  mcp:datadog-read: [...]
```

**Wiring:**

Every `AskClaudeCLI` / `RunCLIStreamingContext` call site replaces hardcoded tool strings with profile-derived args:

```go
// Before:
rawOut, err := claude.AskClaudeCLI(systemPrompt, userPrompt, claude.CouncilTools, 5)

// After:
profile, err := capabilities.LoadProfile("council")
if err != nil {
    return fmt.Errorf("load council capability profile: %w", err)
}
rawOut, err := claude.AskClaudeCLI(systemPrompt, userPrompt,
    profile.AllowedToolsArg(),
    profile.DisallowedToolsArg(),
    profile.MCPConfigPath(),
    5)
```

`AskClaudeCLI` and `RunCLIStreamingContext` gain new parameters for `disallowedTools` and `mcpConfig`. The existing `tools` parameter (formerly hardcoded strings) becomes the profile's `AllowedToolsArg()` output.

**Pattern test** — `internal/audittools/audit_pattern_p13_capability_profiles_test.go`:

```go
// 1. Every AskClaudeCLI / RunCLIStreamingContext call site sources
//    its tool args from capabilities.LoadProfile(agentName), NOT from
//    a hardcoded string literal. Walks production code; flags any
//    call where the tools arg is a string literal or const reference
//    NOT named *.AllowedToolsArg() or *.DisallowedToolsArg().
//
// 2. Every agent named in the production code (via SpawnX functions)
//    has a corresponding YAML profile in agents/capabilities/.
//
// 3. Every YAML profile validates against the registry (every
//    referenced tool/namespace exists; no blocklisted tool granted).
//
// 4. The blocklist's listed tools are NOT in any per-agent profile.
//
// 5. The registry is consistent (every namespace expansion maps to
//    real mcp_tools entries).
```

Pattern test is the anti-regression — it fails on:
- Hardcoded tool strings reintroduced in agent invocations.
- New agent added without a profile.
- Profile granting a blocklisted tool (operator must remove from blocklist explicitly).
- Profile referencing a tool not in the registry (typo / forgotten registry update).

**Hot-update story:** profiles are static — changes require daemon restart to pick up. This is intentional. If we ever genuinely need hot updates, we add the dynamic capability framework at that point. Today's threat model doesn't justify the added complexity. YAGNI.

**Files added/modified:**

| File | Action |
|---|---|
| `agents/capabilities/<agent>.yaml` (×16) | New — one per agent |
| `agents/capabilities/REGISTRY.yaml` | New — fleet-wide tool vocabulary |
| `agents/capabilities/.forceblocklist.yaml` | New — never-allowed list |
| `internal/agents/capabilities/loader.go` | New — YAML loader + validator |
| `internal/agents/capabilities/loader_test.go` | New — unit tests |
| `internal/audittools/audit_pattern_p13_capability_profiles_test.go` | New — pattern test |
| `internal/claude/claude.go` | Modified — `AskClaudeCLI` + `RunCLIStreamingContext` gain `disallowedTools` + `mcpConfig` args |
| Every `internal/agents/*.go` Spawn function | Modified — replace hardcoded tool strings with `capabilities.LoadProfile(...)` |
| `CLAUDE.md` | Modified — new "Per-agent capability profiles" invariant section |

**What this delivers:**

- Static least-privilege per agent: tools not in profile are removed from catalog.
- Fleet-wide kill list for never-reachable tools (the static "never" rail).
- Explicit single source of truth for which agents have which capabilities (16 YAML files, git-tracked, reviewable).
- Pattern test prevents regression to hardcoded tool strings.
- No dynamic infrastructure: no hook binary, no `AgentCapabilities` table, no `CapabilityRequests` queue, no operator approval flow, no ISB classifier. If/when a real need for runtime capability requests emerges, that's a v2 feature; today it's YAGNI.

**What this explicitly does NOT deliver:**

- Mid-task capability discovery (agents fail or proceed without; operator updates the YAML + restart for next claim).
- Runtime audit log of capability use (rely on git history of YAML changes).
- Per-task scope grants (only per-agent permanent grants).
- Auto-classification of low-risk grants (no ISB classifier loop).

These are deliberate omissions. Cost-benefit doesn't justify them at our current threat model and operator-attention budget. Reopen if/when evidence shows agents are routinely failing for lack of capabilities they could reasonably ask for.

**Track T0-2 — Inbound secret scrubbing + `.forceignore` convention.**

Goal: secrets in files the astromech reads (e.g., `.env`, `credentials.json`, `.netrc`) never reach the Claude prompt or Anthropic's cache.

Two parts:

*Part A — Redact layer at the boundary.*

- Add `internal/claude/inbound_redact.go` defining `ScrubInbound(prompt string) string` which:
  - Applies `store.RedactSecrets` first (reuse existing pattern set).
  - Extends with inbound-specific patterns: common `.env` assignment lines (`[A-Z_][A-Z0-9_]*=<value>` where the LHS matches `(API_KEY|SECRET|TOKEN|PASSWORD|PRIVATE_KEY|CREDENTIAL|AUTH)`), PEM block markers (`-----BEGIN (RSA |EC |DSA |OPENSSH |)PRIVATE KEY-----` through `-----END ... -----`), AWS access key pattern (already in store pattern but ensure coverage), GCP service-account JSON markers (`"private_key": "-----BEGIN`).
  - Returns the scrubbed string + a count of redactions made.
- Wrap `AskClaudeCLI` and `RunCLIStreamingContext` at their entry points: scrub the combined system+user prompt via `ScrubInbound`. If `redactionCount > 0`, log `[INBOUND REDACT]` with the count (not the content) + emit operator mail once per N redactions (deduplicated via DB state) to flag likely repo hygiene issues.
- Add `internal/claude/inbound_redact_test.go` + `internal/claude/inbound_redact_fuzz_test.go` (1M-exec budget). Fuzz corpus seeds: real-shape PEM blocks, `.env` lines, AWS keys, bearer tokens, GH PATs. Red-phase: confirm the test fixtures cause `ScrubInbound` to redact.

*Part B — `.forceignore` convention.*

- Add `internal/repo/forceignore.go` defining:
  - `LoadForceIgnore(repoPath string) (*ForceIgnore, error)` — reads `<repoPath>/.forceignore`, compiles gitignore-style patterns.
  - `(*ForceIgnore) IsIgnored(relPath string) bool`.
- Wire `LoadForceIgnore` into astromech's file-reading paths. Before any file-content is included in a task payload or prompt, check `.forceignore`. Ignored files: skip (do not read), log `[FORCEIGNORE SKIP]`.
- Add pre-commit hook `scripts/pre-commit/forceignore-check.sh` that scans the current commit's diff for any line that looks like a byte sequence of a `.forceignore`'d file from any tracked repo. Rejects the commit with a pointer to the offending file.
- Add `.forceignore.example` at the repo root with a template listing common secret-file patterns.
- Add `TestForceIgnore_AstromechSkipsIgnoredFiles` integration test.

Merge order within T0-2: Part A first (redact layer catches secrets regardless of source), Part B second (`.forceignore` is belt-and-suspenders).

### Exit criteria

1. `grep -rn 't\.Skip(.AUDIT-' --include="*.go"` → 0 hits.
2. `internal/audittools/audittools_test.go` `remainingAuditSkips` is empty.
3. `docs/closures/FIX-8D-CLOSURE.md` filed and reviewed (T0-3).
4. `go test -tags sqlite_fts5 -race -count=5 ./...` green, no flakes.
5. **T0-1 capability profiles:** every Spawn'd agent has a YAML profile in `agents/capabilities/`; `agents/capabilities/REGISTRY.yaml` and `agents/capabilities/.forceblocklist.yaml` exist; `internal/agents/capabilities/loader.go` loads, validates, and serves profiles; every `AskClaudeCLI` / `RunCLIStreamingContext` production call site sources its tool args from `capabilities.LoadProfile(...).AllowedToolsArg()` + `.DisallowedToolsArg()` + `.MCPConfigArg()` — no hardcoded tool literals. Pattern test `TestPattern_P13_CapabilityProfiles` passes (every call-site sourced from loader; every agent has a profile; every profile validates against registry; no profile grants a blocklisted tool).
6. **T0-1 empirical restriction check:** for at least one read-only-profile agent (e.g., Captain), confirm via a runtime test that attempting to invoke a tool not in the agent's profile (e.g., `Bash`) results in the tool being absent from Claude's catalog (Claude reports it isn't available). The test uses a seeded prompt asking the agent to attempt the disallowed tool and asserts the agent reports the tool as absent — not "denied," but not present at all.
7. `grep -rnE 'RedactSecrets|ScrubInbound' /Users/jake.herman/code/force-orchestrator/internal/claude/` — `ScrubInbound` is called at every Claude CLI ingress; test `TestInboundRedactCalledAtEveryCallSite` enforces via AST walk.
8. `.forceignore` pre-commit hook installed at `.git/hooks/pre-commit` (or via `husky` if configured); rejects a seeded `.env`-content commit in the test fixture.
9. New fuzz target `FuzzScrubInbound` runs clean for 1M execs.

### Anti-cheat directives

- **No drop of `--dangerously-skip-permissions`.** That flag is load-bearing for the non-interactive daemon; removing it blocks every agent on interactive permission prompts. The actual restriction comes from `--disallowedTools` (computed from the profile's complement), not from the permission mode.
- **No silent fallback to "all tools" if a profile is missing.** `LoadProfile` MUST fail noisily on missing/invalid YAML. An agent without a profile must NOT be silently granted unrestricted tool access; the call should fail with a clear error.
- **No bypassing the profile by passing custom tool args directly to `AskClaudeCLI`.** The pattern test enforces that production call sites source tool args from `capabilities.LoadProfile(agentName)`. Hardcoded literals are rejected. Test files may use direct args for unit tests of `AskClaudeCLI` itself.
- **No granting blocklisted tools through profile YAML.** The loader rejects any profile that lists a tool in `.forceblocklist.yaml`. Pattern test enforces.
- **No partial redaction.** If `ScrubInbound` redacts anything in a prompt, the call proceeds with the redacted version. Do not "surface a warning and send the original anyway." Redaction is enforcement, not advisory.
- **No silent exclusion of PEM blocks from scrubbing.** PEM blocks spanning multiple lines are the most common shape for accidentally-committed private keys in repos. The scrubber must handle multiline patterns; regression test required.
- **No `.forceignore` bypass via symlinks.** The pre-commit hook resolves symlinks before comparing against `.forceignore` patterns.
- **No re-opening of AUDIT-090/091/094/095/127/158/165.** Fix #8d removed these from the allowlist; they stay closed. Any regression is a campaign failure.

### Verification procedure

```
# T0-3 (Fix #8d)
grep -rn 't\.Skip(.AUDIT-' --include="*.go" internal/ cmd/ schema/  # expect 0
cat internal/audittools/audittools_test.go | grep -A2 remainingAuditSkips  # expect empty map
ls docs/closures/FIX-8D-CLOSURE.md  # expect file present

# T0-1 capability profiles
ls agents/capabilities/*.yaml  # expect ~16+ files (one per agent + REGISTRY + .forceblocklist)
go test -tags sqlite_fts5 -run TestCapabilitiesLoader ./internal/agents/capabilities/...  # expect green
go test -tags sqlite_fts5 -run TestPattern_P13_CapabilityProfiles ./internal/audittools/...  # expect green
grep -rnE 'AskClaudeCLI\(|RunCLIStreamingContext\(' --include="*.go" internal/ | grep -v _test.go | grep -v 'capabilities\.' | grep -v 'AllowedToolsArg\|DisallowedToolsArg'  # expect 0 hits (no hardcoded tool literals)

# T0-1 empirical restriction
go test -tags sqlite_fts5 -run TestCapabilityProfile_EmpiricalRestriction -race -count=3 ./internal/agents/capabilities/...  # expect green

# T0-2
go test -tags sqlite_fts5 -run TestInboundRedact ./internal/claude/...  # expect green
go test -tags sqlite_fts5 -run TestForceIgnore ./internal/repo/...  # expect green
go test -tags sqlite_fts5 -fuzz=FuzzScrubInbound -fuzztime=30s ./internal/claude/...  # expect no crash, new interesting > 0
ls .git/hooks/pre-commit  # expect forceignore-check.sh present or referenced

# Full suite
go test -tags sqlite_fts5 -race -count=5 ./...  # expect green no flakes
make smoke  # expect green
make fuzz  # expect green
make test-audit  # expect green
```

### Closure report

Produce `docs/closures/DELIVERABLE-1-CLOSURE.md` with:

- Per-track summary (T0-1, T0-2, T0-3) with commit SHAs.
- Verification output pasted verbatim for every check above.
- **T0-1 specific:** the per-agent profile inventory (agent name, tools count, MCP namespaces granted, blocklist-affected tools), plus a per-call-site migration table (file:line, before-tools, after-source `LoadProfile(...)`).
- Count of `.forceignore` patterns seeded in `.forceignore.example`.
- Anti-cheat self-check: affirm each directive was not violated.
- Residual list (anything found out-of-scope that the operator should track).

---

## Deliverable 2 — Operational Risk Hardening

### Mission

Close the known-unknowns that manifest only under live load: per-task spend anomalies, per-agent context bloat, astromech Bash blast radius, premature writes to freshly-added repos.

### Classification

Risk closure — operational.

### Required reading

1. `/Users/jake.herman/code/force-orchestrator/CLAUDE.md` — especially Fix #1 (spend cap) and Fix #9 (shell validators).
2. `/Users/jake.herman/code/force-orchestrator/internal/agents/spend_cap.go` + `/Users/jake.herman/code/force-orchestrator/internal/agents/estop.go`.
3. `/Users/jake.herman/code/force-orchestrator/internal/store/schema.go` — `BountyBoard`, `TaskHistory`, `Repositories` tables.
4. `/Users/jake.herman/code/force-orchestrator/internal/agents/astromech.go` lines 481 and 936 (current Bash invocation).
5. `/Users/jake.herman/code/force-orchestrator/docs/closures/DELIVERABLE-1-CLOSURE.md` (for context on what D1 already changed).

### Prerequisites

D1 complete: all three tracks merged to main AND `docs/closures/DELIVERABLE-1-CLOSURE.md` filed.

### Merge order within D2

| Order | Track | Branch | Depends on | Parallelizable with | Must merge before |
|---|---|---|---|---|---|
| 1 | T1-0 | `deliverable/2/T1-0` | — (first) | — | T1-1, T1-2, T1-4 |
| 2a | T1-1 | `deliverable/2/T1-1` | T1-0 merged | T1-2, T1-4 | T1-3.5 |
| 2b | T1-2 | `deliverable/2/T1-2` | T1-0 merged | T1-1, T1-4 | — |
| 2c | T1-4 | `deliverable/2/T1-4` | T1-0 merged | T1-1, T1-2 | — |
| 3 | T1-3.5 | `deliverable/2/T1-3.5` | T1-1 merged (uses TaskHistory hooks) | — | T1-3 |
| 4 | T1-3 | `deliverable/2/T1-3` | T1-3.5 merged | — | — (final) |

**Rationale.** T1-0 (startup reconciliation) lands first to give every subsequent track a clean daemon-start property to build on. T1-1/T1-2/T1-4 are file-disjoint: T1-1 adds TaskHistory columns and a new dog, T1-2 adds context-overflow at `claude.go` ingress, T1-4 adds a column to `Repositories` and gates destructive-op call sites. They may be developed in parallel worktrees and merged in any order relative to each other. T1-3.5 (divergence detector) plugs into TaskHistory; it waits for T1-1 so it can reuse the cost-tracking hooks for commit-hash tracking. T1-3 (bash-guard wrapper) is the largest and last-merging track; it adds a new binary + wraps astromech invocation. Merging it last minimizes rebase churn on the five other tracks.

### Work tracks

**Track T1-0 — Startup reconciliation sweep.**

Goal: on daemon start, every non-terminal `BountyBoard` row is reconciled against observable disk/git state. Divergence (worktree missing, branch missing, branch points to an unexpected SHA, worktree has uncommitted changes not matching the task's expected hash) produces a clean recovery action, never a silent mismatch.

Mechanism:
- New function `agents.ReconcileOnStartup(ctx, db)` runs after `ReleaseInFlightTasks` in `cmd/daemon`.
- Iterates every `BountyBoard` row with status in `{Pending, Locked, AwaitingCaptainReview, AwaitingCouncilReview, AwaitingDraftPR, DraftPROpen, Escalated}`.
- For rows carrying a `branch_name`: verify the branch exists, has not been garbage-collected, and that the worktree at `.force-worktrees/<repo>/<agent>` matches the branch HEAD.
- Divergence handling matrix:
  - Branch missing AND status is pre-Captain: transition to `Pending` with `branch_name=''`; task re-runs cleanly from scratch.
  - Branch missing AND status is post-Captain: escalate with `[RECONCILE] branch disappeared for task #N at status X`; operator decides.
  - Worktree missing but branch exists: worktree is recoverable; queue `WorktreeReset` idempotently.
  - Worktree has uncommitted changes not matching expected state: queue `WorktreeReset` idempotently with the parent task as target.
  - DB says task-owned SHA is on branch but `git log <branch>` does not contain that SHA: the branch was force-pushed externally; escalate `[RECONCILE] branch diverged`.
- Every reconcile action emits a `[RECONCILE]` audit entry; daily summary rolls up into operator mail if any non-zero divergences were handled.
- Reconcile runs in < 60s for a fleet of 100 active tasks; integration test enforces.

Tests:
- `TestReconcile_MissingWorktreeQueuesReset`.
- `TestReconcile_MissingBranchPreCaptain_ReturnsToPending`.
- `TestReconcile_BranchDivergedFromExpectedSHA_Escalates`.
- `TestReconcile_CleanState_NoActions`: the happy path — fleet is clean, zero reconcile actions fired.

**Track T1-1 — Per-task cost tracking + anomaly detection.**

Schema changes (both `createSchema` AND `runMigrations` per the CLAUDE.md invariant):
- `TaskHistory` gets columns: `tokens_in INTEGER DEFAULT 0`, `tokens_out INTEGER DEFAULT 0`, `cost_usd_estimate REAL DEFAULT 0`.
- New table `TaskSpendWatch`: `(task_id INTEGER, window_start TIMESTAMP, cost_usd REAL, notified_at TIMESTAMP NULLABLE)` with an index on `(task_id, window_start)`.

Pipeline:
- Extend `claude.AskClaudeCLI` / `RunCLIStreamingContext` to parse the claude-cli JSON output's `usage` block. Populate `tokens_in`, `tokens_out`. Compute `cost_usd_estimate` from a per-model price table in `internal/claude/pricing.go` (static table keyed by model id; expose update helper).
- Every call stores a `TaskHistory` row with populated cost fields.
- New dog `dogTaskSpendWatch` (5-min cadence, runs in `RunDogs` after `dogSpendBurnWatch`):
  - For every task with activity in the last hour, compute `SUM(cost_usd_estimate)` over the last rolling 10-min window.
  - If `SUM > per_task_spend_alert_usd` (SystemConfig default $5) AND no `TaskSpendWatch` row already marks this (task_id, window_start) as notified: emit `[TASK SPEND ANOMALY]` operator mail with task context; insert `TaskSpendWatch` row.
  - If `SUM > per_task_spend_escalate_usd` (SystemConfig default $15): call `CreateEscalation` severity=Medium + emit `[TASK SPEND ESCALATE]` mail + suspend further claims on that task by setting a `spend_suspended=1` flag on `BountyBoard` until operator clears.

Dashboard:
- Task detail view (`handlers.go::handleTaskDetail`) exposes `tokens_in`, `tokens_out`, `cost_usd_estimate`, `lifetime_cost_usd` (sum across TaskHistory).
- Fleet status view exposes a `task_spend_anomalies_last_hour` counter.

Tests:
- Integration test seeds 12 `TaskHistory` rows for one task_id totaling $6 in 10 min, runs `dogTaskSpendWatch`, asserts mail emitted + TaskSpendWatch row inserted.
- Idempotence test: second invocation of the dog in the same window produces no duplicate mail.
- Schema parity test passes.

**Track T1-2 — Per-agent context-size enforcement + byte-source attribution.**

SystemConfig key: `agent_max_prompt_bytes_<agent>` (per-agent override) and `agent_max_prompt_bytes_default` (200 KB default).

Pipeline:
- At the ingress of `AskClaudeCLI` / `RunCLIStreamingContext`, compute `len(systemPrompt) + len(userPrompt)`.
- Prompt assembly is refactored so each contribution carries a source tag: `claude_md`, `librarian_memory`, `task_payload`, `file_read`, `fleet_rules`, `senate_context`, `scope_guard`, `other`. Source tags + byte counts are recorded per call in a new `PromptByteAttribution` table (task_id, agent_name, call_timestamp, source_tag, bytes).
- If total > cap:
  - Log `[CONTEXT OVERFLOW]` with agent name + byte size + top-3 source breakdown.
  - Emit operator mail, deduplicated per-agent-per-day, including the source breakdown so operator sees "captain context is 60% file_read, 25% claude_md, 10% task_payload".
  - Invoke `librarian.SummarizeForContextOverflow(ctx, prompt, targetBytes)` which returns a shorter variant.
  - If the shorter variant still exceeds the cap, reject the call with `ErrContextOverflow`; caller routes to `handleInfraFailure` (existing pattern).
- Add `Librarian.SummarizeForContextOverflow` implementation: single-turn Claude call with strict byte target, uses Haiku when available to minimize cost of summarization itself.
- Dashboard: per-agent "prompt byte budget" view showing rolling-7-day source breakdown so operator can spot spend-per-source trends ("file_read is 40% of Captain's prompts; maybe we should tune file-selection heuristics").

Tests:
- `TestContextOverflow_TriggersSummarize`: seed a 300 KB prompt, assert summarize is called and returns ≤ 200 KB.
- `TestContextOverflow_HardRejectOnDoubleOverflow`: seed a prompt that summarizes to > 200 KB (mock summarizer), assert `ErrContextOverflow` returned and caller escalates.
- `TestPromptByteAttribution_SourceTagsPopulated`: assemble a multi-source prompt, assert every source tag has its bytes recorded and sum equals total.

**Track T1-3 — Astromech Bash allowlist wrapper.**

Goal: destructive or out-of-scope Bash commands from an astromech session are rejected at the shell boundary, not relied on Claude to avoid.

Mechanism:
- New binary `cmd/force-bash-guard/main.go`:
  - Reads the command from stdin (Claude CLI's Bash tool protocol) or argv.
  - Evaluates against an allowlist and denylist.
  - Allowlist: `git`, `gh`, `go`, `npm`, `yarn`, `pnpm`, `pytest`, `cargo`, `make`, `bun`, `deno`, `rustc`, `rustfmt`, `gofmt`, common test runners, `ls`, `cat`, `grep`, `find` (bounded), `rg`, `head`, `tail`, `wc`, `diff`, `cmp`, `awk`, `sed` (non-in-place only), `jq`, `yq`, `curl` (with host allowlist from SystemConfig), `wget` (same).
  - Denylist: `rm -rf /`, `rm -rf ~`, `rm -rf $HOME`, any `rm` targeting a path outside the current worktree, `sudo`, `su`, `chmod` (non-trivial), `chown`, `dd`, `mkfs`, `kill -9 1`, `shutdown`, `reboot`, `:(){ :|:& };:`, anything writing to `/etc`, `/var`, `/usr`, anything reading `~/.ssh`, `~/.aws`, `~/.config/gh/hosts.yml`.
  - Parses compound commands (`&&`, `||`, `;`, `|`) and evaluates each segment independently; compound rejection short-circuits.
  - Logs every allowed command to `.force-worktrees/<repo>/<agent>/bash.log`; logs every rejected command with rejection reason.
- Astromech invocation is updated to use `force-bash-guard` as the Bash tool's shell (via the Claude CLI's Bash tool shell configuration if possible, or by wrapping the astromech's `PATH` to make `bash` point at our guard).

Tests:
- `TestBashGuard_AllowsGitStatus`, `TestBashGuard_AllowsGoTest`.
- `TestBashGuard_RejectsRmRfHome`: asserts `rm -rf ~/Documents` is rejected, exit code nonzero, rejection logged.
- `TestBashGuard_RejectsCompoundWithDenied`: asserts `git status && rm -rf /tmp/*` is rejected.
- `TestBashGuard_RejectsPathTraversal`: asserts `rm -f /../../etc/hosts` is rejected via path resolution.
- Fuzz target `FuzzBashGuard_ShellInjection` with corpus of command-injection payloads; must not crash and must not allow anything not in the allowlist.

Pattern test: `TestPattern_P15_BashGuardIntegrity` walks `astromech.go` to verify the Bash tool invocation routes through `force-bash-guard`; fails if a bare `bash`-as-shell reintroduction slips in.

**Track T1-3.5 — Divergence detector (circular-commit protection).**

Goal: an astromech that keeps rewriting the same file with minor variations (writes X, then rewrites to not-X, then back to X) is caught before burning another retry.

Mechanism:
- New column `BountyBoard.recent_commit_hashes_json`: stores the last 5 commit tree-hashes produced by this task's worktree.
- After every astromech commit, update `recent_commit_hashes_json`.
- Detection: if the latest commit's tree-hash matches one already in the last 5 (excluding immediate predecessor), the task has circled.
- Action on circle detection: transition to `Escalated` with `[CIRCULAR COMMITS]` subject, call `CreateEscalation` severity=Medium, emit operator mail. Do NOT retry — circling tasks should not be re-claimed by astromech.
- Edge case: the LAST commit repeating the immediate predecessor (an `--amend` equivalent) is not circling; the 5-deep window with exclusion handles this.

Tests:
- `TestDivergenceDetector_ThreeCommitCycle_Escalates`: task produces commits with tree-hashes A, B, A — assert escalation fired.
- `TestDivergenceDetector_LinearProgression_NoAction`: commits A, B, C, D, E — no detection.
- `TestDivergenceDetector_ImmediateAmend_NoAction`: commits A, A — this is a no-op amend, not a cycle.

**Track T1-4 — Repository mode column.**

Schema:
- `Repositories.mode TEXT NOT NULL DEFAULT 'read_only' CHECK (mode IN ('read_only','write','quarantined'))`.
- Migration: existing rows get `mode='write'` (preserve current behavior for existing repos); new rows default to `read_only`.

Enforcement points:
- `store.AddRepo` sets `mode='read_only'` for newly-added repos.
- Astromech claim query filters `WHERE r.mode = 'write'` — astromech can only claim tasks on repos in write mode.
- Every destructive git op (`ForcePushBranch`, `TriggerCIRerun`, `DeleteAskBranch`, `MergeAndCleanup`, `completeAskBranchResolution`) asserts repo mode is `write` as its first check after `AssertNotDefaultBranch`. Returns `ErrRepoNotWritable` otherwise.
- Librarian indexing and Senator consultation work on repos regardless of mode (they read, don't write).
- Captain / Council review works on any mode (review is read-only).
- `quarantined` behaves like `read_only` plus: dashboard surfaces a persistent banner, tasks claiming against quarantined repos emit `[QUARANTINED REPO]` mail.

Dashboard:
- Per-repo view exposes mode + one-click promote-to-write / quarantine / restore button. Promotion requires confirmation dialog and writes an audit entry to `AuditLog`.

Tests:
- `TestNewRepoDefaultsToReadOnly`.
- `TestAstromechClaimSkipsReadOnlyRepo`.
- `TestDestructiveGitOpRejectedOnReadOnlyRepo`.
- `TestQuarantineEmitsMailAndBlocksClaims`.

### Exit criteria

1. `TaskHistory.tokens_in|tokens_out|cost_usd_estimate`, `BountyBoard.recent_commit_hashes_json`, `PromptByteAttribution` table all present in both `createSchema` and `runMigrations`. `TestSchemaParity` green.
2. `dogTaskSpendWatch` registered in `RunDogs` and positioned after `dogSpendBurnWatch`.
3. `ReconcileOnStartup` invoked after `ReleaseInFlightTasks` in daemon start path; integration tests for reconciliation matrix green.
4. Integration test for anomaly flow green; idempotence test green.
5. `TestContextOverflow_*` + `TestPromptByteAttribution_*` tests green.
6. `force-bash-guard` binary builds; `TestBashGuard_*` suite green; fuzz clean for 30s.
7. `TestDivergenceDetector_*` suite green.
8. `Repositories.mode` column present; default behavior correct per migration test.
9. `TestPattern_P15_BashGuardIntegrity` green.
10. Full suite green under `-race -count=5`.
11. Dashboard views expose new data (per-task cost, per-agent prompt-byte-source breakdown, per-repo mode banner, circular-commit escalation alerts).

### Anti-cheat directives

- **No removing Bash from astromech's tool list.** Astromechs need Bash to run tests, build, etc. The fix is a guarded shell, not removal.
- **No allowlist bypass via environment variables.** `force-bash-guard` ignores `BASH_ENV`, `ENV`, `PROMPT_COMMAND`. Explicit tests confirm.
- **No default `write` mode for new repos.** The whole point of T1-4 is safety-first. If a test requires `write` mode, it must call a helper that transitions the repo to `write` — not change the default.
- **No silent context truncation.** If a prompt is over-cap, the summarize path is mandatory; truncating raw bytes is forbidden.
- **No "pattern-covered" reuse for reintroduced silent failures.** Every error path added for the new features must check and propagate per CLAUDE.md.

### Verification procedure

```
# T1-1
go test -tags sqlite_fts5 -run TestTaskSpendWatch ./internal/agents/...  # green
go test -tags sqlite_fts5 -run TestSchemaParity ./internal/store/...  # green
sqlite3 holocron.db "PRAGMA table_info(TaskHistory)" | grep -E "tokens_in|tokens_out|cost_usd_estimate"  # three hits

# T1-2
go test -tags sqlite_fts5 -run TestContextOverflow ./internal/claude/...  # green

# T1-3
go build ./cmd/force-bash-guard  # succeeds
go test -tags sqlite_fts5 -run TestBashGuard ./cmd/force-bash-guard/...  # green
go test -tags sqlite_fts5 -fuzz=FuzzBashGuard -fuzztime=30s ./cmd/force-bash-guard/...  # clean
go test -tags sqlite_fts5 -run TestPattern_P15 ./internal/audittools/...  # green

# T1-4
go test -tags sqlite_fts5 -run TestRepoMode ./internal/store/...  # green
go test -tags sqlite_fts5 -run TestAstromechClaimSkipsReadOnly ./internal/agents/...  # green

# Full suite
go test -tags sqlite_fts5 -race -count=5 ./...  # green no flakes
make smoke && make fuzz && make test-audit  # all green
```

### Closure report

`docs/closures/DELIVERABLE-2-CLOSURE.md` with:

- Per-track summary + commit SHAs.
- Schema diff showing added columns.
- Bash allowlist + denylist pasted (so operator can review and tune).
- SystemConfig key defaults set (`per_task_spend_alert_usd`, `per_task_spend_escalate_usd`, `agent_max_prompt_bytes_default`, per-agent overrides if any).
- Verification output pasted verbatim.
- Anti-cheat self-check.
- Residual list.

---

## Deliverable 3 — Paired Runs + Engineering Corps + Global Holdout

### Mission

Build the measurement substrate that every subsequent deliverable depends on. `treatments.Apply` ingress, full data model for experiments / treatments / metrics / runs / outcomes, Engineering Corps as a new claim-loop agent, global holdout minted on day one.

### Classification

Measurement infrastructure. Load-bearing.

### Required reading

This deliverable's spec is `docs/paired-runs.md` (1040 lines) and `docs/next-gen-agents.md` (528 lines, Engineering Corps section). Agent must read both in full before any code.

### Prerequisites

D1 AND D2 complete: every track of both deliverables merged to main AND `docs/closures/DELIVERABLE-1-CLOSURE.md` + `docs/closures/DELIVERABLE-2-CLOSURE.md` both filed. D1's `ScrubInbound` must be in place (experiments invoke LLMs; same scrubbing applies) and D2's per-task cost tracking must be in place (experiment cost accounting uses the same columns).

### Merge order within D3

D3 is **strictly sequential by phase**. No phase begins until the prior phase's tests are green AND its phase-exit gate is recorded in `PAIRED-RUNS-ROLLOUT.md` at the repo root.

| Order | Phase | Branch | Gate to next phase |
|---|---|---|---|
| 1 | Phase 1 — Foundations + Rule Audit (schema for paired-runs, verification specs, Captain proposals, ProposedFeatures; log-only `treatments.Apply`; FleetRules bootstrap from audited CLAUDE.md with `render_to` categorization; rule-renderer dispatching by `render_to`; per-agent rule injection; metric registry) | `deliverable/3/phase-1` | Schema parity green; log-only pass-through verified; bootstrap idempotent; **rendered CLAUDE.md ≤ 10 KB** (was 20 KB before `render_to` discipline tightened the criteria; 20 KB is the absolute upper bound enforced by the pre-commit hook, 10 KB is the Phase 1 target) |
| 2 | Phase 2 — Holdout + single-treatment experiments (`treatments.Apply` live, `baseline-2026` minted, Bayesian algorithm, basic dashboard views) | `deliverable/3/phase-2` | `treatments.Apply` live on hot path; holdout accumulating runs; first single-treatment experiment terminates |
| 3 | Phase 3 — Engineering Corps + Trust Metrics Infrastructure (`SpawnEngineeringCorps`, six task types, Librarian → EC handoff, ratification flow, cross-layer disagreement tracking, independent ground-truth metrics with prompt_version correlation) | `deliverable/3/phase-3` | EC claim loop running; at least one candidate promotion authored; cross-layer disagreement table populating |
| 4 | Phase 4 — Factorial + orthogonal-overlap scheduler | `deliverable/3/phase-4` | Factorial experiment terminates with main-effects + 2-way interactions computed |
| 5 | Phase 5 — Level-3 paired shadow + adversarial pairing (`gh` recording proxy, shadow worktrees, CI suppression, adversarial pairing for Council/Medic/ConvoyReview auto-execute decisions, golden-set evaluation framework) | `deliverable/3/phase-5` | One tool-using-agent experiment runs in shadow mode to termination; one adversarial pair surfaces a real disagreement; first golden-set evaluation cycle completes |
| 6A | Phase 6A — Dashboard scaffolding + Pulse + Briefing (three-surface IA, dashboard heartbeat, keyboard shortcuts + help overlay, notification budgets + emit-site routing, OperatorSessionState + resume-where-you-left-off, trust dials per agent, NarrativeRenders + live narrative panel, Pulse fleet panel, "while you were away" cinematic on wake, Briefing conversational triage with Haiku-rendered briefings, counter-proposal forcing, prior-similar-decisions context, cooldown banner for high-stakes auto-execute, operator attention tags, CLI parity audit) | `deliverable/3/phase-6a` | All 15 sub-tracks merged; operator can land in Pulse, see narrative panel update every 30s, triage decisions through Briefing focus mode, see cooldown banner on high-stakes auto-execute, mark attention tags. Pattern tests P25–P30 green. |
| 6B | Phase 6B — Reflection + Drill + Verification spec consumption + Trust layers + Shakedown (LLMCallTranscripts capture wrapper at every Claude CLI site, GitOperationLog at internal/git helpers, drill convoy/task/event views, free-text search across transcripts, replay mode for Captain/Council/ConvoyReview/Medic decisions, operator annotations with flag tags, transcript archival housekeeping dog, Ask via `/` shortcut with read-only DB tools, Reflection calibration scoreboard, Reflection fleet learning panel, Reflection 5-min retro generator, verification spec consumption by ConvoyReview with frozen-spec atomic cycles per concern #6, Captain proposal validation + LLM-judge per concern #1, ConvoyReview cross-classification per concern #4 C, ProposedFeatures management with value/complexity scoring per concern #10, spec deprecation flow per concern #9, PromotionProposals revert handling per concern #7, AT-id collision integrity per concern #8, shakedown experiment) | `deliverable/3/phase-6b` | All 13 dashboard sub-tracks + all concerns #1, #4, #6–#10 acceptance criteria green. End-to-end shakedown: Librarian candidate → EC author → operator ratify → run → terminate → promotion → operator ratify → FleetRules insert → rule-renderer commit; convoy verification spec round-trip on shakedown convoy with one operator-ratified amendment; ProposedFeatures dedup verified; replay-mode side-by-side rendered; Pattern tests P31, P32, replay-no-mutation green. |

The Phase 6 split into 6A and 6B is a build-order optimization, not a semantic split — both must merge before D3 closes. **Detailed task briefs** with implementation + validation prompts for every dashboard sub-track live in `docs/dashboard-implementation.md`. That document is the authoritative agent-handoff artifact for Phase 6's dashboard work; this section captures the rollout-plan summary.

**Parallelism within a phase.** Sub-tracks within phases that touch disjoint files may parallel-develop. Where coupled (e.g., schema work in Phase 1, hot-path integration in Phase 2), sub-tracks merge serially.

**No phase skipping.** Phase N's closure tests must be green before Phase N+1 begins.

### Work tracks (expanded)

Execute per the rollout plan in `docs/paired-runs.md` §"Rollout Plan." Phase descriptions below are the consolidated set including all decisions from concerns #1-#5 and the prior architectural discussions.

#### Phase 1 — Foundations + Rule Audit

Schema and bootstrap for everything that follows.

**Original scope:**
- 13 paired-runs tables (Experiments, ExperimentTreatments, ExperimentMetrics, ExperimentRuns, ExperimentOutcomes, TreatmentSpecs, MetricVersions, AnalysisFrameworks, FleetStateSnapshots, FleetRules, PromotionProposals, GlobalHoldouts, ModelAvailability)
- Inheritance columns on Features, Convoys, BountyBoard (`in_holdout`, `experiment_assignments_json`)
- `treatments.Apply` stub in log-only mode (records "would have assigned" without modifying call descriptors)
- Metric registry directory structure + loader + fixture-test runner on daemon start
- `rule-renderer` dog + pre-commit hook rejecting hand-edits to auto-generated files

**NEW scope (additions from prior discussions):**
- **CLAUDE.md audit and refactor.** Categorize every section by `render_to` (controlled enum — see `paired-runs.md` § Rule Registry → Rendered exports): `claude-md-file` (universal-load — tight criteria; rule applies to operator AND Claude Code building Force AND every review agent) / `agent-prompt` (per-agent injection only via `agent_scope` filter; never to a shared file) / `fix-log` (historical narrative, append-only, not auto-loaded) / `pattern-test-docstring` (lives in test file's docstring; CLAUDE.md gets one-line cross-ref) / `per-domain-doc:<file>` (e.g., `docs/dashboard-conventions.md`, `docs/pr-flow-invariants.md`) / `discard`. Bootstrap migration parses categorized content into FleetRules rows with `(category, agent_scope, render_to)` tags. **Default render target during bootstrap is NOT `claude-md-file`** — the auditor must affirmatively justify each rule that stays in the universal-load file. **Target: rendered CLAUDE.md ≤ 10 KB** (Phase 1 goal). Pre-commit hook enforces the 20 KB absolute upper bound; the auditor and renderer aim for the 10 KB target.
- **Per-agent rule injection at agent invocation.** Each agent's `claude -p` call gets `--append-system-prompt` content built from `SELECT content FROM FleetRules WHERE category='claude-md' AND (agent_scope='all' OR agent_scope='<agent>')`. Agents stop paying token cost on rules irrelevant to them.
- **CLAUDE.md size budget pre-commit hook.** Rejects commits that grow the rendered CLAUDE.md beyond 20 KB (the absolute upper bound; 10 KB is the Phase 1 goal). Forces operator-deliberate growth (must subtract or refactor existing content). Also: pattern test `TestPattern_PNN_ClaudeMdSize` fails CI if the file exceeds the 20 KB cap. The renderer itself emits `[RULE-RENDERER OVERFLOW]` operator mail and refuses to write a render exceeding the cap.
- **Pattern-test-as-spec discipline.** Every FleetRule with `category='claude-md'` includes a `enforced_by` field referencing a Pattern test ID OR an explicit "trust-only" tag. Surfaces aspirational rules vs mechanically-enforced ones.
- **Verification spec schema.** `Convoys.verification_spec_json` (acceptance tests, exit criteria, anti-cheat directives, closure artifacts), `Convoys.spec_history_json` (operator-ratified amendment audit trail). Per concern #9 the spec shape is `{ ats: [...active...], deprecated: [{at_id, removed_at, removed_by_email, rationale, removal_kind: 'mistake'|'superseded'|'satisfied'|'out_of_scope', superseded_by: {kind, ref}}] }`. Per concern #6, spec mutation is operator-ratification-only — LLMs propose amendments, the spec doesn't drift mid-cycle.
- **ConvoyReviewCycles table (concern #6).** Atomic snapshot of each ConvoyReview pass: `convoy_id`, `cycle_number` (UNIQUE per convoy, monotonic), `spec_version_at_start` (snapshot of spec version this cycle ran against — frozen once written), `cycle_started_at` / `cycle_completed_at`, `outcomes_json` (per-AT pass/fail/inconclusive), `fix_tasks_spawned_json`, `amendments_proposed_json`, `amendments_ratified_during_cycle_json`. Frozen-spec model: spec amendments mid-cycle take effect at the NEXT cycle, never the in-flight one — prevents the 8d→8e→8f noisy-spec drift the operator flagged.
- **PromotionProposals revert columns (concern #7).** `rejection_action TEXT` ('leave_as_is' | 'clean_revert' | 'cascade_revert' | 'surgical_revert' | 'escalate'), `rejection_rationale TEXT` (mandatory ≥ 20 chars when action != 'leave_as_is'), `revert_task_id INTEGER` (spawned CodeEdit performing the revert), `refiled_feature_id INTEGER` (rejection-as-refile path). Plus `BountyBoard.deferred_revert BOOLEAN` and `BountyBoard.revert_target_task_id INTEGER` for cascade-revert tracking. The operator picks revert semantics explicitly at rejection time — no system-side guessing of intent.
- **Captain proposal schema.** `BountyBoard.proposed_action_json` for spec-amendment proposals when Captain spawns unmapped tasks. Schema includes: `cited_ats[]` (with convoy_id disambiguation per concern #8), `cited_fleet_rules[]`, `spec_link` (tied/glue/unmapped), `classification_confidence` (high/medium/low), `captain_reasoning`, `draft_amendment` (operator-ratifiable text), `alternative` (glue interpretation if ambiguous). LLM proposal schemas accept ADD/MODIFY only — REMOVE intent on AT references is rejected at emit time per concern #9.
- **Cross-convoy AT-id collision invariant (concern #8).** AT IDs are LOCALLY scoped within a single convoy's spec; lookup uses compound key `(convoy_id, at_id)`, never bare `at_id`. UI labeling discipline: dashboard renders AT references with convoy context (e.g., "Convoy #47 / AT-005") — never bare AT-id chips. Future fleet-wide AT namespace via FleetRules references is deferred to v2.
- **Spawning-AT provenance on fix tasks (concern #9).** `BountyBoard.spawning_at_id TEXT DEFAULT ''` — populated when ConvoyReview spawns a fix task, naming the AT that drove the spawn. Cheap query for the in-flight check at AT-removal time: removal endpoint refuses (returns 409) if `spawning_at_id` matches active tasks unless operator passes explicit `inflight_disposition` (Cancel-and-remove / Complete-then-remove / Cancel-removal).
- **ProposedFeatures table** for Investigator's cross-convoy aggregation queue. Schema per concern #4 / Investigator-expansion discussion. Includes queue-management columns: `fingerprint` (canonical-content SHA256 for dedup), `first_seen_at` / `last_seen_at` / `occurrence_count` (aggregation), `evidence_history_json` (per-occurrence evidence trail), `promoted_at` / `promotion_deadline` (active-interest tracking), `archived_at` / `archive_reason` (soft archive). Plus value/complexity scoring: `value_score` and `complexity_score` (low/medium/high CHECK), `value_rationale` / `complexity_rationale`, `scored_by` (`investigator|captain|ec|operator|convoy_review`). Partial UNIQUE on `(fingerprint) WHERE archived_at IS NULL AND fingerprint != ''` enforces dedup.
- **ProposedFeatureSuppressions table** with `fingerprint`, mandatory `rationale TEXT NOT NULL` (≥ 20 chars), `suppressed_until`, `created_by_email`. Operator-only writes; proposers check before insert.
- **ProposedFeatureScoreOverrides table** for audit trail when operator changes an LLM-suggested value/complexity score: `proposed_feature_id`, `prior_value_score` / `prior_complexity_score`, `new_value_score` / `new_complexity_score`, `rationale TEXT NOT NULL`, `overridden_by_email`. No silent score mutations.
- **TaskHistory prompt_version column.** Records which prompt version produced each agent decision. Enables F (independent ground-truth tracking with per-prompt correlation) in Phase 3.
- **Adversarial-pairing tables.** New table `AdversarialPairings(decision_id, primary_outcome, critic_outcome, agreement bool, surfaced_at, ...)` for tracking auto-execute layer adversarial-pair results in Phase 5.
- **Golden-set evaluation tables.** `GoldenSetFixtures(agent, input, expected_output, source, curated_at)` and `GoldenSetEvaluations(agent, prompt_version, fixture_id, actual_output, accuracy_score, evaluated_at)`.
- **Dashboard tables (Phase 6 prerequisites).** Schema lands in Phase 1 alongside the rest of D3 so 6A/6B can build against a stable data layer:
  - `DashboardHealthHeartbeats(ticked_at, process_pid, bind_addr, in_flight_requests)` (6A.2)
  - `OperatorNotificationBudgets(operator_email, source, channel, max_per_period, period_minutes, digest_remainder)` and `OperatorNotificationDigest(operator_email, source, channel, digest_for_date, payload_json, flushed_at)` (6A.4)
  - `OperatorSessionState(operator_email, last_active_at, last_viewed_surface, last_viewed_route, last_focused_decision_id, partial_review_state_json)` (6A.5)
  - `OperatorTrustDials(operator_email, agent, dial_value, set_at, set_by, rationale)` — history-preserving via `UNIQUE(operator_email, agent, set_at)`; bootstrap row per-agent at `dial_value=70` (6A.6)
  - `NarrativeRenders(rendered_at, event_window_start, event_window_end, source_event_count, source_event_refs_json, prose, prompt_version, cost_usd, cache_hit)` (6A.7)
  - `BriefingRenders(decision_id, decision_kind, briefing_text, prior_similar_decisions_json, prompt_version, cost_usd, operator_decision, decision_time_seconds, counter_proposal_kind, counter_proposal_text, counter_proposal_routed_id)` (6A.10 + 6A.11)
  - `CooldownPauses(decision_id, decision_kind, scheduled_action_at, paused_at, paused_by_email, resumed_at, cancelled_at, executed_at)` (6A.13)
  - `OperatorAttentionTags(operator_email, target_kind, target_id, attention_level, set_at, rationale)` (6A.14)
  - `LLMCallTranscripts(task_id, agent, prompt_version, call_started_at, call_completed_at, system_prompt, user_prompt, response_text, tool_calls_json, cost_usd, input_tokens, output_tokens, cache_read_tokens, cache_creation_tokens, archived_at)` — redacted via `RedactSecrets` at write time (6B.1)
  - `GitOperationLog(task_id, convoy_id, repo, operation, args_json, started_at, duration_ms, exit_code, stdout_excerpt, stderr_excerpt, branch, before_sha, after_sha)` (6B.2)
  - `OperatorEventAnnotations(operator_email, event_kind, event_ref, note_text, flag, noted_at)` (6B.8)
  - `ReplayResults(original_event_id, original_event_kind, replay_prompt_version, replay_started_at, replay_response, decision_changed, cost_usd, triggered_by_email)` (6B.7)
  - `FleetLearningPanels(rendered_at, prose, cost_usd, prompt_version, source_event_refs_json)` (6B.12)

**Invocation-layering refinement (D1 T0-1 follow-up).** Discovered while landing the astromech target-CLAUDE.md clause: today only astromechs auto-load CLAUDE.md (because their CWD is `.force-worktrees/<repo>/<agent>/` inside the target repo), and only review agents (Captain, Council, Medic, Chancellor, ConvoyReview, PR-review-triage, Commander) auto-load `force-orchestrator/CLAUDE.md` (because their CWD is the daemon's). The full reference is `docs/architecture/claude-cli-invocation.md`. Phase 1's CLAUDE.md → FleetRules audit was scoped before this asymmetry was articulated; the following clarifications apply:

- **All agents (astromechs included) get fleet rules via `--append-system-prompt`** rendered from FleetRules after Phase 1. The auto-load asymmetry that today distinguishes astromechs from review agents goes away for fleet-coordination invariants — every agent receives the same scoped rule set via the renderer.
- **Astromechs continue to auto-load target-repo CLAUDE.md** as developer guidance (build commands, lint rules, codebase tours). `AstromechTargetCLAUDEMDClause` (added in the D1 T0-1 follow-up) remains live and continues to frame the target's CLAUDE.md as advisory, surface genuine conflicts via the `[TARGET_CLAUDE_MD_OBSERVATION: …]` marker, and close the downstream smuggling channel via the `llmSignalTokens` denylist. Phase 1's audit does NOT touch this clause.
- **The 10 KB target for rendered `force-orchestrator/CLAUDE.md`** (20 KB absolute upper bound) is for content that meets the universal-load bar — applies to operator AND Claude Code building Force AND every review agent simultaneously. The audit must preserve that narrow set; per-agent behavioral rules go to `render_to='agent-prompt'`, fix narratives to `render_to='fix-log'`, pattern test deep-dives to `render_to='pattern-test-docstring'`, dashboard / PR-flow / domain rules to `render_to='per-domain-doc:<file>'`. Treat Force's own CLAUDE.md as a thin universal-load preamble that the operator and review-agent set actually need at every load — not as the source of truth for fleet rules. The realistic post-audit content is roughly: Gas Town pattern, daemon-context-threading, schema parity, build/test conventions, commit style, a list of pattern test names with one-line cross-refs, and the architectural cross-references to per-domain docs.
- **The rule renderer emits a single concatenated `--append-system-prompt` block per agent.** The order Claude sees content in is: default Claude Code system prompt → `~/.claude/CLAUDE.md` → project memory → CWD-walked CLAUDE.md → `--append-system-prompt`. Phase 1 must preserve this ordering; `--append-system-prompt` lands LAST (closest to the user message), giving Force's rules the strongest position for prompt-injection resistance and the most recency-biased weighting.

**Phase 1 exit:** schema parity green; log-only `treatments.Apply` records to its tables without affecting calls; CLAUDE.md rendered from FleetRules with `render_to='claude-md-file'` filter, **≤ 10 KB target hit** (≤ 20 KB hard cap enforced); per-agent rule injection working at agent invocation via `render_to='agent-prompt'` filter; bootstrap migration idempotent; pre-commit hook active; pattern test `TestPattern_PNN_ClaudeMdSize` green; all dashboard tables listed above present in `createSchema` AND `runMigrations` AND `schema/schema.sql`.

#### Phase 2 — Holdout + single-treatment experiments

**Scope (unchanged from original):**
- `treatments.Apply` live on hot path (no longer log-only)
- `baseline-2026` global holdout minted
- Bayesian Beta-Binomial analysis algorithm registered
- Single-treatment experiment authoring + lifecycle (no factorial yet)
- Dashboard views: experiments list, single-experiment view, fleet-progress (holdout vs current)

**Phase 2 exit:** `treatments.Apply` live; holdout enrolling 2% of features; first single-treatment experiment runs to termination with declared outcome.

#### Phase 3 — Engineering Corps + Trust Metrics Infrastructure

**Original scope:**
- `SpawnEngineeringCorps` claim-loop (Diplomat-pattern, separate goroutine, separate queue)
- Six task types: ExperimentAuthor, ExperimentMonitor, PromotionAuthor, DemotionAuthor, MetricAuthor, HoldoutMonitor
- Librarian → EC handoff: Librarian emits PromotionProposals with `origin='librarian'`; EC's ExperimentAuthor consumes
- Ratification endpoint + atomic DB+render+commit on operator approval

**NEW scope (additions from concerns #2 and #5):**
- **Cross-layer disagreement tracking (concern #5 E).** Schema extension on TaskHistory: `prior_review_outcomes_json` listing the chain of agent decisions per task. EC computes per-pair disagreement rates over rolling windows. Surface includes Captain → Council reject rates, Council → CI fail rates, ConvoyReview → astromech "can't fix" rates, Senate concur → Chancellor declines amendment rates, operator approve at DraftPROpen → revert within 30 days rate.
- **Independent ground-truth tracking (concern #5 F).** EC analysis layer joins TaskHistory.prompt_version with downstream-outcome metrics: per-Captain-prompt-version PR merge rate, per-Council-prompt-version CI pass rate, per-Medic-prompt-version convoy-completion rate, per-Chancellor-prompt-version fix-cycle count, per-EC-promotion revert-within-90-days rate.
- **Distribution drift detection (concern #4 E).** EC tracks per-Captain-prompt-version classification rates: tied_rate, glue_rate, unmapped_rate, low_confidence_rate over rolling windows. Alerts on outliers (e.g., unmapped_rate < 1% over 50+ spawns).
- **Amendment-quality measurement infrastructure (concern #2 D).** EC tracks: amendment_revert_rate_30d (fraction of amendments ratified in week N reverted within 30 days); decision_time_correlation_with_revert (do fast-decisions correlate with reverts?). Infrastructure ships now; signal accumulates from day 1.

**Phase 3 exit:** EC claim loop running; at least one candidate promotion authored end-to-end; cross-layer disagreement table populating with data from Phase 2 experiments; per-prompt-version metrics joining cleanly.

#### Phase 4 — Factorial + orthogonal-overlap scheduler

**Scope (unchanged from original):**
- Factorial dimension support (cell-based storage)
- Stratified randomization for cell balancing
- Main-effects + 2-way interactions analysis
- Orthogonal-dimension overlap invariant in scheduler

**Phase 4 exit:** factorial experiment terminates with computed main effects and 2-way interactions; scheduler refuses overlap on shared dimensions.

#### Phase 5 — Level-3 paired shadow + Adversarial Pairing + Golden-Set

**Original scope:**
- `gh` recording-proxy runner
- Shadow worktree lifecycle (creation, cleanup; 24h auto-archive)
- CI suppression for shadow arms
- Pre-CI scoring metrics for shadow-only signals
- Confirm-phase orchestration (winner declaration → confirm phase → ratified PromotionProposal)

**NEW scope (additions from concern #5 revisit):**
- **Adversarial pairing for high-stakes auto-execute layers.** When Council approves a diff, parallel `Council-critic` prompt runs with opposite framing ("find quality issues"). Disagreement surfaces to operator review queue rather than auto-merging. Same pattern for: Medic decision (primary vs critic on requeue/shard/cleanup/escalate); ConvoyReview fix-task drafting (primary vs critic on "would this fix actually close the gap?"). Disagreements written to `AdversarialPairings` table; surface to operator at sufficient volume threshold.
- **Golden-set evaluation framework.** Periodic dog (weekly) runs current Captain/Council/Medic prompts against curated input fixtures with known-correct outputs. Auto-curation: fixtures auto-derived from convoys that shipped cleanly without rework (empirical positives). Operator-curated negative examples optional. Accuracy regression below threshold triggers operator alert.

**Phase 5 exit:** one tool-using-agent experiment runs in shadow mode to termination; one adversarial pair surfaces a real disagreement (operator handles it through the surfaced flow); first golden-set evaluation cycle completes with baseline accuracy recorded per agent prompt version.

#### Phase 6 — Operator UX + Verification Spec + Trust Layers + Shakedown

This is the heaviest phase, consolidating operator-facing UX with verification-spec consumption and the shakedown experiment. Phase 6 splits into 6A and 6B for build-order parallelism, but both must merge before D3 closes.

**Authoritative implementation artifact:** `docs/dashboard-implementation.md` carries per-task implementation prompts and validation prompts for every dashboard sub-track in 6A and 6B. Agents handed Phase 6 work read that document for the work brief; this section captures the rollout-plan summary and cross-references to concerns #1–#10.

**Phase 6A — Dashboard scaffolding + Pulse + Briefing.**
- Three-surface IA (Pulse / Briefing / Reflection / Ask `/`) — caps top-level nav at three forever
- Dashboard heartbeat + health banner + `force dashboard status` CLI
- Keyboard shortcuts + `?` overlay (Pattern P26)
- Notification budgets + emit-site routing (Pattern P27)
- OperatorSessionState + resume-where-you-left-off (mid-review state restore on return)
- Trust dials per agent (operator-set, calibration-suggested, never auto-changed); shifts Briefing friction tier per dial
- NarrativeRenders + Pulse live narrative panel (Haiku-rendered prose every 30s; Pattern P28 keeps the renderer pure)
- Pulse fleet panel (spend rate, active agents, convoys in flight, queue at a glance, trust dials compact)
- "While you were away" cinematic on detected sleep wake (replays NarrativeRenders rows + summary card)
- Briefing — conversational triage with Haiku-rendered briefings, prior-similar-decisions context, cited evidence rendering (Pattern P29: briefings cite real rows, no hallucinated decision IDs)
- Counter-proposal forcing in Briefing (high-stakes rejection requires structured counter)
- Cooldown banner for high-stakes auto-execute decisions (Pattern P30: every high-stakes auto-execute routes through CooldownPauses)
- Operator attention tags (`following` / `normal` / `muted` per convoy/feature/agent/rule_key)
- CLI parity audit + fill (Pattern P25: every mutating dashboard handler has a `force <verb>` CLI equivalent)

**Phase 6B — Reflection + Drill + Verification Spec + Trust Layers + Shakedown.**

*Diagnostic substrate:*
- LLMCallTranscripts capture wrapper at every Claude CLI site (Pattern P31; redaction at write time per Fix #10)
- GitOperationLog at `internal/git` helpers (Pattern P32; redaction at write time)
- Daily transcript-archival housekeeping dog (bounded SQLite; full bodies offload to `~/.force/transcripts/<year>/<month>/<id>.txt.gz`)

*Drill — diagnostic surface:*
- Convoy drill view (timeline + inspectors + filters): unified event stream across TaskHistory, LLMCallTranscripts, GitOperationLog, ConvoyReviewCycles, Escalations, SubPRs, BriefingRenders
- Task drill view (decision chain + attempt history + LLM transcripts + tool calls + git ops + cost rollup)
- Event drill (per-event detail with tool-call expansion)
- Free-text search across transcripts via sqlite_fts5
- Replay mode for Captain rulings, Council rulings, ConvoyReviewCycles, Medic decisions — re-runs with current prompt version side-by-side; Pattern asserts NO mutation of live state on replay path
- Operator annotations with `flag` field (`problem`/`interesting`/`follow_up`); annotations are operator-only writes (no LLM/system path inserts)

*Ask:*
- `/` shortcut → floating input → Haiku call with read-only DB tools; cite-link results clickable to drill
- Pattern asserts no write tools registered on the Ask handler

*Reflection:*
- Calibration scoreboard (decision-time distribution, calibration sample accuracy, trust-dial coaching, replay-driven recalibration)
- Fleet's learning panel (weekly Sunday-night auto-render synthesizing PromotionProposals, spec amendments, ProposedFeatures activity, prompt-version-shift outcomes)
- 5-min retro generator (Friday button, markdown draft to `docs/retros/<date>.md`)

**Original scope (carried into 6B):**
- Manual-override auto-experiment flow
- `model-availability-watch` dog + deprecation operator mail
- Holdout lifecycle dashboard (ramp/plateau/fade indicators)
- Metric registry dashboard
- First operator-authored experiment end-to-end (shakedown)

**Concern-bundled scope (carried into 6B):**

*Verification spec consumption:*
- **ConvoyReview consumes verification specs.** At DraftPROpen, ConvoyReview evaluates each acceptance test, exit criterion, anti-cheat directive in the convoy's `verification_spec_json`. Failures spawn fix tasks scoped to specific spec entries. Spec-was-wrong cases emit `[SPEC AMENDMENT PROPOSED]` instead of fix tasks; out-of-convoy work emits `[PROPOSED_FEATURE]` to Investigator stream.

*ConvoyReviewCycles atomic snapshots (concern #6):*
- **Cycle row written at start of every ConvoyReview pass.** `INSERT INTO ConvoyReviewCycles (convoy_id, cycle_number, spec_version_at_start, cycle_started_at, ...)`. `cycle_number = (SELECT COALESCE(MAX(cycle_number),0) FROM ConvoyReviewCycles WHERE convoy_id = ?) + 1`. UNIQUE (convoy_id, cycle_number) constraint prevents racing inserts.
- **Frozen-spec model at the cycle level.** Once a cycle starts, its `spec_version_at_start` snapshot is what it evaluates against — operator-ratified amendments accepted during the cycle window queue for the NEXT cycle. Prevents mid-cycle spec churn from producing fix tasks that target a spec the convoy isn't actually trying to satisfy.
- **Immutable cycle outcomes.** Once written, a cycle row's `outcomes_json`, `fix_tasks_spawned_json`, `amendments_proposed_json` are append-only via the cycle-completion path. No `UPDATE ConvoyReviewCycles SET outcomes_json = ?` after `cycle_completed_at` is set; Pattern test enforces.
- **8d→8e→8f noisy-spec defense.** The operator explicitly flagged that mid-cycle spec amendments produced 200-500 line verification prompts that grew with each fix-attempt round. Frozen-spec-per-cycle plus operator-ratification-only amendments keeps the spec stable across the convoy's life and surfaces drift attempts to the operator instead of letting them silently bloat the prompt.

*PromotionProposals revert handling (concern #7):*
- **`rejection_action` choice at rejection time.** Modal forces operator to pick one of: `leave_as_is` (rejection logged but no code change), `clean_revert` (no dependents — straight `git revert`), `cascade_revert` (revert this + every task that depended on it; `BountyBoard.deferred_revert=1` tracks the cascade through dependents until all complete), `surgical_revert` (remove only the affected hunks; ConvoyReview re-runs on the surgical revert as semantic safety net), `escalate` (operator can't decide — routes to a fresh proposal).
- **Mandatory rationale ≥ 20 chars.** When `rejection_action != 'leave_as_is'`, `rejection_rationale` is non-null and content-checked. Rejections without rationale fail at the API layer.
- **`revert_task_id` cross-reference.** Spawned CodeEdit performing the revert is tracked back to the originating PromotionProposal — operator can audit "what reverts have I authorized?"
- **`refiled_feature_id` re-file path.** When operator rejects with "this was the wrong implementation; re-file with different approach," the rejection mints a new Feature row and stamps its ID onto the rejected proposal. Closes the loop on rejected-and-refiled proposals.
- **ConvoyReview as semantic safety net for surgical reverts.** A surgical revert leaves the door open to silently breaking sibling work; ConvoyReview pass post-revert validates the verification spec still passes before the revert merges.

*Cross-convoy AT-id collisions (concern #8):*
- **Compound-key lookup invariant.** Every code path resolving an AT MUST use `(convoy_id, at_id)`, never bare `at_id`. Pattern P20 (`TestPattern_P20_ATIdScopeIntegrity`) walks production code and rejects `WHERE at_id = ?` queries without a co-occurring `convoy_id` constraint.
- **UI labeling discipline.** Dashboard renders AT references with convoy context — "Convoy #47 / AT-005" — never bare AT-id chips. Cross-references in Captain proposals, ConvoyReview cycle outcomes, and amendment-history views all carry the convoy prefix.
- **No top-level fleet-wide AT namespace.** Future fleet-wide ATs (if needed) route through FleetRules with `rule_key` prefixed `at:fleet:<name>` — cleanly disambiguates from per-convoy AT IDs. Until that ships in v2, fleet-wide ATs are forbidden (no implicit ID space migration).

*Spec deprecation (concern #9):*
- **Operator removes ATs via UI; LLMs cannot.** Removal endpoint is operator-routed only. Pattern P21 (`TestPattern_P21_ATRemovalIsOperatorOnly`) walks every LLM proposal schema (Captain `proposed_action_json`, ConvoyReview amendment proposals, EC promotion proposals) and asserts none of them carries a "remove" or "deprecate" intent on AT references.
- **Soft deprecation with rationale.** Removal moves the AT from `verification_spec_json.ats[]` → `verification_spec_json.deprecated[]` with: `removed_at`, `removed_by_email`, mandatory `rationale` (≥ 20 chars), `removal_kind` ('mistake' | 'superseded' | 'satisfied' | 'out_of_scope'), optional `superseded_by` (`{kind: 'at'|'fleet_rule', ref}`). Hard delete is forbidden — historical cycle outcomes referencing the AT keep their meaning.
- **In-flight fix-task disposition.** Removal endpoint queries `BountyBoard WHERE spawning_at_id = ? AND status NOT IN ('Completed','Cancelled','Failed')`. If non-empty, modal forces operator to pick: Cancel-and-remove / Complete-then-remove (sets `pending_deprecation` flag; ConvoyReview keeps evaluating until tasks land) / Cancel-removal.
- **Pending Captain proposal re-justification.** Proposals with `cited_ats` referencing the removed AT route through Captain re-justification (cheap LLM-judge: "is this still valid given the AT was deprecated?") before the proposal re-surfaces in the operator's review queue.
- **Spec history append-only.** Every deprecation creates a `spec_history_json` entry with `kind: 'deprecate'`, `at_id`, `rationale`, `proposed_by: 'operator'`, `ratified_at`, `ratified_by_email`. No silent removals — audit trail is mandatory.

*Captain proposal validation (concern #1):*
- **Captain proposal output schema active.** Captain populates `proposed_action_json` for unmapped spawns including `cited_ats`, `cited_fleet_rules`, `spec_link`, `classification_confidence`, `captain_reasoning`, `draft_amendment`, `alternative`.
- **Mechanical validation at proposal-emit time.** Every cited AT resolves to real `(convoy_id, at_id)` in spec history. Every cited FleetRule resolves to real `rule_key`. Prose `AT-NNN` references must appear in `cited_ats`. Reject the ruling on hallucinated references.
- **Source-of-truth display in operator UI.** When operator reviews a Captain proposal, dashboard fetches and renders the actual cited AT text and cited FleetRule text alongside Captain's `WhyCited` claim. Operator can compare claim vs source.
- **Captain reasoning LLM-judge.** Cheap LLM call (Haiku) with Captain's `captain_reasoning` + actual cited AT/FleetRule texts; returns "consistent / inconsistent / ambiguous." Inconsistent → reject ruling, retry with critic note. Ambiguous → proceed with operator-visible "[reasoning may not match cited evidence]" badge.
- **Pattern test P18** (`TestPattern_P18_CaptainProposalIntegrity`) enforces validation runs at proposal-emit; planted hallucinated-AT fixtures rejected; planted prose-without-cited-ats fixtures rejected.

*ConvoyReview cross-classification (concern #4 C):*
- **ConvoyReview cross-classifies Captain's spawn classifications at DraftPROpen.** Reads actual diff and Captain's per-spawn `spec_link`. Emits `[CLASSIFICATION_DISAGREEMENT]` per spawn where ConvoyReview's assessment differs from Captain's. Operator sees disagreements alongside the unmapped-spawn batch.

*Two-track UI with stakes-tiered friction (concern #2 H):*
- **Stakes-tier classification.** Each proposal carries `stakes_tier ∈ {low, medium, high}`. Auto-escalation rules: any proposal touching CLAUDE.md/BoS/ISB/Senate rules → high; AT deprecation → high; cumulative similar > 5 in 30d → high; first-of-kind → high. Operator can manually escalate any proposal to high; demoting from high requires logged reasoning.
- **Tier-based UI.** Low-tier: batch view, multi-select, single approve-all action. Medium-tier: single-proposal view, explicit per-row click, full reasoning visible. High-tier: single-proposal modal with explicit "I have read Captain's reasoning and cited evidence" affirmation checkbox required before Approve enables. **Implementation note:** these tiers map onto Briefing's friction levels in Phase 6A — concrete UI lives in 6A.10 with trust-dial-driven shifts from 6A.6.

*Counter-proposal forcing (concern #2 I):*
- **High-tier rejection requires counter-proposal selection.** "Should not happen at all" (text reason) / "Should happen differently" (operator drafts or prompts EC to draft alternative which becomes new proposal) / "Defer to investigation queue" (kicks to Investigator triage). **Implementation:** Phase 6A.11.

*Decision-distribution dashboard (concern #2 A):*
- **Per-agent decision metrics.** Approve/reject/modify rates over rolling 7/30/90 day windows; decision-time distribution (median, p90); recent-decisions list with spot-check links. **Implementation:** absorbed into Reflection's calibration scoreboard (Phase 6B.11) — daily-driver presence is preserved via Pulse's compact trust-dials panel which links into the full scoreboard.

*Reject-rate baseline warning (concern #2 F):*
- **SystemConfig.expected_reject_rate_min** (default 0.05). Per-agent: actual reject rate over rolling 30d < threshold → dashboard banner ("you've approved X consecutive proposals from <agent>; expected baseline is 5%+ rejection"). Operator can dismiss for 7 days; warning resurfaces.

*Weekly calibration sample (concern #2 B-lightweight):*
- **Weekly audit-sample widget.** Every Monday, dashboard surfaces N=7 (configurable) recently-ratified proposals from prior week. Weighted selection: 30% fast-approved high-stakes; 30% high-approve-rate-agent proposals; 40% random within rest. Operator interactions: "Confirm — still looks right" / "Re-review in detail" / "Should have been rejected" (generates `[RE_EVALUATE]` task; doesn't auto-revert) / "Snooze."
- **CalibrationAuditSamples table.** Records sample selection, operator action, rationale on disagreement.

*Captain classification confidence (concern #4 H):*
- **Captain emits `classification_confidence: high | medium | low`** alongside `spec_link`. Routing rules: high+glue/tied → don't surface for review; medium or unmapped or any low confidence → surface in batched triage. Captain's self-uncertainty drives review surfacing.

*Operator spot-check on classifications (concern #4 F):*
- **Triage view exposes classification spot-check.** Alongside unmapped spawns, shows count of tied/glue spawns and "[Spot-check N]" expandable. Operator can mark any as "should have been unmapped" → moves to amendment review; "should have been a different classification" → records disagreement signal feeding distribution-drift metrics from Phase 3.

*Return UX + sleep handling (concern #3):*
- **Per-convoy `critical` flag.** Set at convoy creation OR by operator before DraftPROpen. Used for triage prioritization in Briefing queue ordering and to gate cooldown banners in Pulse (Phase 6A.13 — only critical-convoy auto-executes get the 60s pause).
- **Return-UX batched triage view.** When operator returns to dashboard after detected idle period, shows: critical convoys with auto-glued amendments → high-stakes amendments awaiting triage → medium-stakes grouped by similarity → ready-to-ship. Similarity-grouping enables batch-approve for consistent patterns; high-tier still requires per-row affirmation. **Implementation:** absorbed into Briefing queue sort + cinematic summary card; "while you were away" cinematic in Phase 6A.9 surfaces critical convoys first in the closing summary card.
- **`force install-sleep-hook` command.** One-time setup that wires the existing graceful shutdown into macOS sleep events via sleepwatcher. Checks sleepwatcher installed (brew install if missing); writes `~/.sleep` and `~/.wakeup` scripts pointing at daemon PID and launch command; verifies sleepwatcher running.
- **Heartbeat-based sleep detection.** Heartbeat goroutine (Phase 6A.2) ticks every 30s, records `last_heartbeat_at`. On each tick, if `now - last_heartbeat_at` > 90s (3× expected), infer sleep happened. On detected wake: log inferred sleep duration, run D2 T1-0 reconciliation sweep, cancel any in-flight subprocess contexts that have been waiting > sleep duration, refresh trailing-hour spend window, surface a sleep event for the cinematic.
- **"While you were away" cinematic.** Replaces the older flat "since you woke up" banner with a 30-second animated narrative replaying NarrativeRenders rows from the sleep window plus a summary card highlighting the highest-stakes pending decision. Implementation: Phase 6A.9.

*Investigator expansion + ProposedFeatures queue management (concern #5 cross-cutting + concern #10):*
- **Investigator subscribes to event streams.** Astromech `follow_up_observations`, Captain `out_of_convoy_observations` (for rejected scope-creep that LOOKS like real work), Council `quality_observations`, ConvoyReview `[PROPOSED_FEATURE]` emissions, Senate plan-time emissions (when D4 ships).
- **Canonical-fingerprint dedup at insert ingress.** Every proposer (Investigator, Captain mid-cycle amendment, EC experiment wrap, ConvoyReview cross-classification, manual operator filing) computes `fingerprint = sha256(canonical(source, topic, sorted_code_paths, sorted_at_refs, sorted_fleetrule_refs))` BEFORE insert. `INSERT ... ON CONFLICT(fingerprint) WHERE archived_at IS NULL DO UPDATE SET occurrence_count = occurrence_count + 1, last_seen_at = datetime('now'), evidence_history_json = json_insert(...)`. Same recurring observation aggregates to one row with occurrence count; recurrence becomes a priority signal, not noise.
- **Suppression check at insert ingress.** Before any insert, proposer queries `ProposedFeatureSuppressions` for active fingerprint match. Suppressed = no-op (logged, not raised). Solves the reject-and-refile cycle; operator sets duration with mandatory rationale.
- **Value/complexity scoring at proposal time.** Proposer's LLM call emits `value_score` (low/medium/high) + `complexity_score` (low/medium/high) + one-line rationales, populated alongside the proposal body. Three-tier scale matches the stakes-tier vocabulary from concern #4 to keep operator cognitive load down.
- **Composite priority sort.** `priority = value_points / complexity_points` (low=1, med=3, high=9). Default ordering: `priority desc, occurrence_count desc, created_at asc`. UI badges: "Quick win" (H/L green), "Big swing" (H/H amber), "Don't bother" (L/H red).
- **Operator override flow with audit.** Operator-edit on a score writes `ProposedFeatureScoreOverrides` row with rationale; original LLM-suggested score remains visible in history. Override accumulation feeds calibration loop (per-source override-rate becomes a meta-metric on Investigator/Captain/EC prompt quality, ties into concern #5 cross-layer disagreement tracking).
- **Promotion-to-active-interest.** Proposal default state = "pending." Operator clicks **Promote** → modal requires either an estimated convoy date OR a self-deadline (suggested deadline scales with complexity: H/L = 1wk, mid = 2wk, H/H = 4wk + explicit "I understand this is a major investment" checkbox). Promoted proposals get prominent placement; un-promoted are subject to auto-archive.
- **Operator triage UX.** Pending ProposedFeatures with batched review, per-row decisions: promote (with deadline) / spawn new convoy / merge into existing convoy as amendment / suppress (with rationale + duration) / archive / merge with similar.
- **Categorized dashboard views.** Tabs: **Active** (created last 7d OR promoted) / **Recurring** (occurrence_count ≥ 3) / **Suppressed** / **Archived**. Filters: source (Investigator/Captain/EC/Operator/ConvoyReview), repo, AT cluster, severity, value floor, complexity ceiling. "Show only quick wins" is one click.
- **Score-aware auto-archive (housekeeping dog).** `proposed-features-housekeeping` (daily): `value=low` AND age > 30d AND occurrence==1 AND not promoted → archive; `value=medium` AND age > 60d AND occurrence==1 AND not promoted → archive; `complexity=high` AND `value=low` (the "don't bother" lane) → archive at 14d; `value=high` → never auto-archive (always operator decision); Captain/operator/ConvoyReview-sourced rows → never auto-archive (active fleet decisions). Archive is soft (sets `archived_at` + `archive_reason`); operator can un-archive at any time. Weekly digest mail summarizes what got archived.
- **Capacity-imbalance dashboard signal.** Widget shows per-source filed-vs-engaged ratio: "Investigator filed 47 in 30d; you've engaged with 3." No hard caps (avoids silent signal loss); imbalance prompts operator to review proposer sensitivity config.

*Trust property documentation (concern #5 G):*
- **CLAUDE.md "Recursive LLM trust" entry** (brief — full architectural detail in `docs/architecture/llm-trust-property.md`). One paragraph + cross-reference. Documents the property; lists auto-execute layers explicitly; names mitigations; honestly disclosed limits.

*Shakedown (original):*
- First operator-authored experiment end-to-end with full round-trip: experiment YAML authored → operator pre-approves → runs → terminates → declares outcome → emits PromotionProposal → operator ratifies → FleetRules insert → rule-renderer commit. PLUS new in this revision: convoy verification-spec round-trip on shakedown convoy (specs evaluated, fix tasks spawned and resolved if any, amendments surfaced and ratified or dismissed appropriately).

**Phase 6 exit:** all UX surfaces functional and operator-tested; shakedown experiment completes full round-trip; convoy verification spec round-trip works on shakedown convoy; ProposedFeatures dedup demonstrated with at least 2 observations across 2 convoys aggregating to 1 entry.

### Exit criteria

Per paired-runs.md, but summarized:

1. All paired-runs tables (Experiments, Treatments, Metrics, Runs, Outcomes, TreatmentSpecs, MetricVersions, AnalysisFrameworks, FleetStateSnapshots, FleetRules, PromotionProposals, GlobalHoldouts, ModelAvailability) plus the additions (ProposedFeatures, ProposedFeatureSuppressions, ProposedFeatureScoreOverrides, AdversarialPairings, GoldenSetFixtures, GoldenSetEvaluations, CalibrationAuditSamples, ConvoyReviewCycles) present in schema parity. Convoys table extended with `verification_spec_json` (with `deprecated[]` subarray), `spec_history_json`, `experiment_assignments_json`, `critical` flag. BountyBoard extended with `proposed_action_json`, `prompt_version`, `prior_review_outcomes_json`, `spawn_spec_link`, `spawn_classification_confidence`, `spawning_at_id`, `deferred_revert`, `revert_target_task_id`. PromotionProposals extended with `rejection_action`, `rejection_rationale`, `revert_task_id`, `refiled_feature_id`. TaskHistory extended with `prompt_version`.
2. `treatments.Apply` is on the hot path for every LLM call; log-only mode retired.
3. `baseline-2026` holdout accumulating runs; 2% of features flagged `in_holdout=true`.
4. `SpawnEngineeringCorps` claim loop running; all six task types present.
5. **CLAUDE.md rendered from FleetRules with `render_to='claude-md-file'` filter, ≤ 10 KB target (≤ 20 KB hard cap).** Pre-commit hook + `TestPattern_PNN_ClaudeMdSize` enforce the cap. Per-agent rule injection at agent invocation working — each agent receives only `render_to='agent-prompt' AND agent_scope IN ('all','<agent>')` rules in its system prompt; the universal-load file does NOT receive per-agent behavioral rules.
6. **Verification spec consumption.** ConvoyReview at DraftPROpen evaluates `verification_spec_json` per convoy, emits fix tasks for failures, emits `[SPEC AMENDMENT PROPOSED]` and `[PROPOSED_FEATURE]` per spec model. Round-trip demonstrated on shakedown convoy.
7. **Captain proposal pipeline.** Captain emits structured `proposed_action_json` for unmapped spawns. Mechanical validation rejects hallucinated cited ATs / FleetRules at emit. Operator UI displays cited evidence text alongside Captain claims. LLM-judge layer flags reasoning-vs-evidence inconsistency. Pattern test P18 green.
8. **Cross-layer disagreement tracking.** Per-pair disagreement rates populating in EC's analysis layer (Captain → Council, Council → CI, ConvoyReview → astromech, Senate concur → Chancellor declines, operator approve → revert-within-30d).
9. **Independent ground-truth tracking.** TaskHistory.prompt_version populated; per-prompt-version downstream metrics joinable.
10. **Adversarial pairing surfacing real disagreements.** At least one Council-primary-vs-critic disagreement detected and surfaced to operator during Phase 5; at least one Medic and one ConvoyReview pairing surfaces equivalent.
11. **Golden-set evaluation cycle.** First weekly evaluation completes with baseline accuracy per agent prompt version. Auto-curation of fixtures from clean-shipping convoys working.
12. **Operator UX surfaces complete.** Stakes-tiered UI with affirmation checkbox at high tier; counter-proposal forcing for high-tier rejections; decision-distribution dashboard panel; reject-rate baseline warning; weekly calibration sample widget; return-UX batched triage view; per-convoy critical flag.
13. **Sleep handling.** `force install-sleep-hook` command available; heartbeat-based sleep detection with post-wake reconciliation; dashboard "since you woke up" widget functional.
14. **Investigator expanded.** Subscribes to all event streams; ProposedFeatures queue populating with deduplication; operator triage UX functional. Value/complexity scoring and operator-override audit trail working; suppression rules enforced at ingress; score-aware auto-archive housekeeping dog running.
14a. **ConvoyReviewCycles atomic snapshots (concern #6).** Cycle rows immutable post-completion; mid-cycle spec amendments queue for next cycle, never mutate the in-flight cycle's `spec_version_at_start`. Pattern test enforces.
14b. **PromotionProposals revert handling (concern #7).** Operator rejection forces explicit `rejection_action` choice; mandatory rationale ≥ 20 chars when action != 'leave_as_is'; cascade-revert tracking via `BountyBoard.deferred_revert` working; surgical revert re-triggers ConvoyReview as semantic safety net. Manual rehearsal of all four revert variants demonstrated on shakedown.
14c. **Cross-convoy AT-id integrity (concern #8).** Pattern P20 green; UI labeling discipline verified (no bare AT-id chips in any operator surface).
14d. **Spec deprecation flow (concern #9).** Pattern P21 green (LLMs cannot propose REMOVE); operator-UI deprecation flow round-tripped end-to-end including in-flight fix-task disposition modal and Captain proposal re-justification trigger; deprecated entries resolve in historical cycle outcome lookups.
15. **At least one end-to-end shakedown experiment.** Authored by EC from a Librarian candidate, pre-approved by operator, ran to termination, declared an outcome, emitted a PromotionProposal, ratified by operator, wrote a FleetRules row, rule-renderer committed the rendered markdown. PLUS the convoy that ran the experiment had a verification spec consumed by ConvoyReview with at least one spec-amendment-proposal round-tripped through operator ratification.
16. Dashboard: experiments list, fleet-progress (holdout vs current), proposals queue, metric registry, rule registry, holdout lifecycle, decision-distribution panel, calibration sample widget, ProposedFeatures queue, "since you woke up" event log.
17. `docs/paired-runs.md` §"Rollout Plan" phases 1–6 each have green-tests sign-off recorded in `PAIRED-RUNS-ROLLOUT.md` at repo root.
18. Full suite green under `-race -count=5` at every phase boundary.

### Anti-cheat directives

- **No log-only mode forever.** Phase 1 ships `treatments.Apply` in log-only mode to shake out data-model bugs. Phase 2 flips it live. Don't stay in log-only because live traffic is scary — the whole point is to start accumulating evidence.
- **No shortcutting the shakedown experiment.** Phase 6 requires an END-TO-END experiment with all real components: Librarian emits candidate, EC authors, operator ratifies, it runs, it terminates, EC emits promotion, operator ratifies, FleetRules writes, markdown renders, commit lands. Every step in that chain is tested. Do not stub any of them.
- **No pre-registered "will win" experiments.** The shakedown experiment must be one where the outcome is genuinely unknown in advance. Otherwise the round-trip is theater.
- **No stale `fleet_state_hash`.** Every experiment run must record its start and end hash. A run without a hash is a bug.
- **No `.env`-file leak via experiment payload.** `treatments.Apply` runs AFTER `ScrubInbound` in the call chain. Verify via integration test.
- **No dropped Engineering Corps throttles.** `engineering_corps_daily_proposal_cap` (default 3) is non-negotiable; a runaway EC is worse than a quiet one.
- **No CLAUDE.md bloat regression.** Pre-commit hook + `TestPattern_PNN_ClaudeMdSize` enforce ≤ 20 KB hard cap on rendered CLAUDE.md (with 10 KB Phase 1 target). Adding a rule with `render_to='claude-md-file'` that pushes the file over budget requires proportional removal/refactor (move existing rules to `agent-prompt` / `fix-log` / `pattern-test-docstring` / `per-domain-doc`). No bypass.
- **No `render_to='claude-md-file'` as default.** Bootstrap audit + any future rule insert must affirmatively justify universal-load placement. The pattern test asserts the count of `render_to='claude-md-file'` rows stays small (initial threshold to be set by the audit; ongoing growth requires operator review). Rules without an explicit `render_to` value reject at insert.
- **No untested `enforced_by` field on FleetRules.** Every rule with category `claude-md` must reference a real Pattern test ID OR carry an explicit `trust-only` tag. Pattern test enforces; rules with neither field reject the FleetRules insert.
- **No silently-stripped Captain reasoning.** When Captain's `captain_reasoning` includes `AT-NNN` references, those references must appear in `cited_ats[]`. Validation at emit-time rejects orphan references.
- **No bypass of operator high-tier affirmation.** Auto-approve flows for high-tier proposals (e.g., scripted approve from a tool) must fail. The affirmation checkbox is part of the data model, not just UI styling.
- **No empty counter-proposal text on high-tier rejection.** "Should not happen at all" path requires a text rationale; "Should happen differently" path requires a draft alternative. Empty text fields reject the rejection action.
- **No silent retreat from sleep detection on wake.** Heartbeat-detected wake events MUST trigger reconciliation; reconciliation that runs but produces no audit log entry is a regression. Pattern test enforces.
- **No adversarial-pair output that's identical to primary.** When primary Council/Medic/ConvoyReview output exactly matches critic output token-for-token, the pair is a sham (likely same-model-same-prompt). Pattern test in Phase 5 detects identical-output pairs and rejects.
- **No golden-set fixtures that always pass.** A golden set whose every fixture's `expected_output` matches every prompt's `actual_output` is calibration theater. Auto-curation must produce fixtures with non-trivial expected behavior; operator-curated negative fixtures (cases where the wrong answer is known) keep the set honest.
- **No skipping the Investigator dedup.** When Investigator inserts a new ProposedFeatures row, it MUST run dedup against existing rows first. Inserting duplicates without merging fails the InvestigatorAggregation pattern test.
- **No non-deterministic ProposedFeatures fingerprints.** Pattern P22 (`TestPattern_P22_FingerprintDeterminism`) feeds canonical input into the fingerprint helper twice and asserts byte-equal hashes. Catches accidental inclusion of timestamps, run IDs, or random salts. The canonical input shape (source + topic + sorted code paths + sorted AT refs + sorted FleetRule refs) is the only legal input; tests assert excluded fields stay excluded.
- **No proposer mutation of archive/suppression state.** Pattern P23 (`TestPattern_P23_ProposerWriteDiscipline`) walks proposer code paths (Investigator, Captain, EC, ConvoyReview) and asserts they only INSERT rows or use the dedup ON CONFLICT path. Direct writes to `archived_at`, `archive_reason`, or any column on `ProposedFeatureSuppressions` from a proposer code path fail the test. Only operator-routed handlers and the housekeeping dog may write archive state.
- **No proposer score-distribution skew.** Pattern P24 (`TestPattern_P24_ScoreDistributionMonitor`) reads recent proposals per source and warns if any source's value-score distribution exceeds 70% in any single bucket. Long-tail flat distributions are healthy; bimodal-toward-high indicates the proposer LLM is treating "value=high" as a default. Warning surfaces on dashboard, not just CI — operator decides whether to tune the proposer prompt.
- **No silent score mutations.** Pattern asserts every `UPDATE ProposedFeatures SET value_score = ?` or `complexity_score = ?` is preceded (in the same transaction) by an `INSERT INTO ProposedFeatureScoreOverrides` write. Direct score updates without an audit row fail.
- **No LLM-driven removal of ATs from a verification spec.** Pattern P21 (`TestPattern_P21_ATRemovalIsOperatorOnly`) walks every LLM proposal schema (Captain `proposed_action_json`, ConvoyReview amendment proposals, EC promotion proposals) and asserts none of them carries a "remove" or "deprecate" intent on AT references. Spec deprecation is operator-UI-only per concern #9.
- **No deletion of historical ATs.** Pattern asserts `DELETE FROM Convoys` paths never strip entries from `verification_spec_json.ats[]` — deprecation moves entries to `verification_spec_json.deprecated[]` with mandatory rationale + removed_at + removed_by_email. Lookups against historical cycle outcomes resolve through the deprecated array.
- **Pattern P25 — CLI-dashboard parity.** Every mutating dashboard handler has a corresponding `force <verb>` CLI command. New mutating endpoint without CLI equivalent fails CI. Allowlist accepted only for non-operator-action handlers (heartbeat writes from the dashboard process itself), with a one-line rationale per CLAUDE.md's allowlist-truthfulness invariant.
- **Pattern P26 — keyboard shortcut consistency.** Every documented shortcut in `?` help overlay binds to a real action; every binding is documented. Static parse of `keymap.js` + `help-overlay.html`; sets must match exactly.
- **Pattern P27 — notification-budget routing at every emit site.** Every operator-mail / push-notification call site routes through `respectNotificationBudget` first. Direct `sendOperatorMail` / banner-set / modal-set calls outside the helper fail the test. High-stakes punches through the budget; low-stakes past budget routes to `OperatorNotificationDigest`.
- **Pattern P28 — narrative is generated, not editorial.** `NarrativeRenders.prose` is produced ONLY by `internal/agents/narrative_renderer.go`. The prompt template is in code (`internal/agents/narrative_prompts/`), not DB-stored, and is version-stamped against `NarrativeRenders.prompt_version`. No human-written copy in the rendering pipeline (would risk drift / favoritism in event description).
- **Pattern P29 — briefing prose cites real evidence.** Every `BriefingRenders.briefing_text` mentioning a prior decision MUST resolve to a real BountyBoard / PromotionProposals / ConvoyReviewCycles row. Fuzz tests feed BriefingRenders with random IDs; hallucinated decision IDs in prose fail the test.
- **Pattern P30 — high-stakes auto-execute uses cooldown.** Every auto-execute decision class with `stakes_tier='high'` MUST insert a CooldownPauses row before its `scheduled_action_at` fires. Direct execution of a high-stakes action without a CooldownPauses row fails the test.
- **Pattern P31 — every LLM call writes a transcript.** Every `claude.AskClaudeCLI` / `claude.RunCLIStreamingContext` call site MUST go through the transcript-capturing wrapper (`claude.CallWithTranscript`). Direct un-wrapped calls fail the test. Redaction at write time via `RedactSecrets` is asserted.
- **Pattern P32 — git ops are logged at the helper layer.** Every `exec.CommandContext(ctx, "git", ...)` / `exec.CommandContext(ctx, "gh", ...)` in production code routes through `internal/git` helpers (`runGitCtx`, `runGitCtxOutput`, `bestEffortRun`, `abortOp`) which write to `GitOperationLog`. New direct exec sites outside the helpers fail the test (composes naturally with existing P11 invariant from CLAUDE.md).
- **Pattern — Replay no mutation.** Replay code paths write only to `ReplayResults` and the replay's OWN `LLMCallTranscripts` row. Direct calls to `store.UpdateBountyStatus`, `store.FailBounty`, `store.SetConvoyStatus`, etc. in the replay path fail the test. Replay is purely diagnostic; live state is sacred.
- **Pattern — Annotations are operator-only writes.** No LLM or system code path INSERTs into `OperatorEventAnnotations`. The CLI command `force annotate` is the only non-dashboard write path; the existing P25 CLI parity covers it.
- **Pattern — Trust dial operator-write discipline.** No system code path INSERTs into `OperatorTrustDials` with `set_by='operator'` from a non-operator-routed handler. Calibration suggestions write rows with `set_by='calibration_suggestion'` which DO NOT change the current dial — they're advisory. Only operator-action paths write `set_by='operator'` rows that change the live dial.
- **Pattern — Ask handler has no write tools.** The `internal/agents/ask_handler.go` agent definition registers ONLY read-only DB-query tools (getConvoy, getTask, searchTranscripts, listFleetRules, listEscalations, etc.). Any write tool registered on the Ask handler fails the test. Composes with P29 — Ask answers must cite real rows, not invented IDs.

### Verification procedure

Per phase. Each phase's exit test suite runs under `-race -count=5`. At phase end:

```
# Example for Phase 3 (Engineering Corps)
go test -tags sqlite_fts5 -run TestEngineeringCorps -race -count=5 ./internal/agents/...  # green
go test -tags sqlite_fts5 -run TestLibrarianToECHandoff -race -count=5 ./internal/agents/...  # green
go test -tags sqlite_fts5 -run TestRatificationEndpoint -race -count=5 ./internal/dashboard/...  # green

# Round-trip shakedown at Phase 6
go test -tags sqlite_fts5 -run TestShakedownExperimentRoundTrip -race -count=1 ./internal/agents/...  # green (long test, -count=1 acceptable)

# All phases complete
make smoke && make fuzz && make test-audit
go test -tags sqlite_fts5 -race -count=5 ./...  # green no flakes
```

### Closure report

`docs/closures/DELIVERABLE-3-CLOSURE.md` with:

- Per-phase sign-off log.
- Shakedown experiment full trace (copy from `PAIRED-RUNS-ROLLOUT.md`): which treatment, which metric, what the outcome was, what rule promoted.
- Schema audit: every new table's columns verified vs spec.
- First monthly fleet-progress snapshot: baseline-2026 holdout vs current cohort, with caveat that data is thin on day 1.
- Anti-cheat self-check.
- Residual list.

---

## Deliverable 4 — Bureau of Standards + Imperial Security Bureau + Senate

### Mission

Ship the three review-layer agents from `docs/next-gen-agents.md`. All three live in the D3 pipeline: their rules are in `FleetRules`, their rendered export files are auto-maintained, their rule changes route through the promotion pipeline with paired-run evidence.

### Classification

Risk mitigation — continuous.

### Required reading

1. `/Users/jake.herman/code/force-orchestrator/docs/next-gen-agents.md` — full. This is the authoritative spec for BoS, ISB, and Senate.
2. `/Users/jake.herman/code/force-orchestrator/docs/paired-runs.md` — §"Rule Registry" and §"Engineering Corps" sections.
3. `/Users/jake.herman/code/force-orchestrator/docs/closures/DELIVERABLE-3-CLOSURE.md` — for the current state of FleetRules, rule-renderer, etc.
4. `/Users/jake.herman/code/force-orchestrator/CLAUDE.md` — every invariant section becomes at least one BoS rule.

### Prerequisites

D3 complete: every phase merged to main AND `docs/closures/DELIVERABLE-3-CLOSURE.md` filed. FleetRules + rule-renderer + promotion pipeline + Engineering Corps + global holdout must all be live.

### Merge order within D4

Strictly sequential. Per `docs/next-gen-agents.md` §"Implementation order."

| Order | Track | Branch | Depends on | Parallelizable with | Must merge before |
|---|---|---|---|---|---|
| 1 | D4-BoS | `deliverable/4/bos` | — (first) | — | D4-ISB |
| 2 | D4-ISB | `deliverable/4/isb` | D4-BoS merged | — | D4-Senate |
| 3 | D4-Senate | `deliverable/4/senate` | D4-ISB merged | — | — (final) |

**Rationale.** BoS first because its rules are the easiest extraction (CLAUDE.md invariants already documented) and every rule lands as a FleetRules row + Go AST check, giving us the simplest validation that the D3 promotion pipeline works end-to-end for rule-shaped work. ISB second because its rules cover the highest-leverage security class and build on BoS's FleetRules + rule-renderer integration (ISB and BoS share the `SecurityFindings` table; co-designing them avoids a rebase later). Senate last because it's the largest architectural change — new claim loop, new state machine integration point between `ProposedConvoys` and `AwaitingChancellorReview` — and benefits from having BoS + ISB already operational for its own rule-promotion tests.

### Work tracks

Per `docs/next-gen-agents.md` §"Implementation order," the sequence is: BoS → ISB → Senate. These are THREE tracks, merged sequentially.

**Track D4-BoS — Bureau of Standards.**

- Create `internal/bos/` package. `type Rule interface { ID() string; CLAUDEMDAnchor() string; Severity() Severity; Check(*ast.File, *types.Info) []Finding }`.
- Implement the 10 rules at launch from `next-gen-agents.md` §"Rules at launch":
  - `BOS-001` through `BOS-010` per the table. Each in its own file `internal/bos/rules/bos_<id>.go` with a companion red/green test.
- **Add `BOS-011 — ClientsInterfaces` rule** that graduates the D0 Pattern P16 enforcement to commit-time. The rule body mirrors P16's AST walk: rejects any production agent file that imports a concrete client struct type (e.g., `librarian.inProcessClient`) or instantiates an implementation directly via `&<package>.<StructType>{...}`. CLAUDE.md anchor: "Cross-agent service interfaces" (the section D0 added). This is the user-promised "BoS rule" graduation — D0's Pattern test was the CI-time enforcement; BOS-011 catches violations at the commit-time gate, one step earlier in the pipeline.
- Rule-check bodies in Go; rule metadata (ID, severity, agent_scope='all', CLAUDE.md anchor) seeded into `FleetRules` at migration time. Rendered `bos/rules/*.yaml` follows via `rule-renderer`.
- New agent claim: `SpawnBoS`, claims `BoSReview` task type inserted by astromech commit hook (post-commit, pre-Captain).
- Bypass mechanism: `// BOS-BYPASS: <AUDIT-NNN> <reason>` inline comment, lands in `SecurityFindings` with `disposition='overridden'`.
- `SecurityFindings` table from `next-gen-agents.md` §"ISB Storage" (shared with ISB).

**Track D4-ISB — Imperial Security Bureau.**

- Create `internal/isb/` package with wrappers for `gosec`, `semgrep`, `gitleaks`.
- Implement the 10 rules at launch per `next-gen-agents.md` §"Rules at launch (minimum set)": `ISB-001` through `ISB-010`.
- Deterministic rules first; LLM layer for context-sensitive rules (`ISB-005`, `ISB-008`, `ISB-010` likely need LLM).
- LLM layer budget: one call per commit; respects `SpendCapExceeded(db)` gate.
- New agent claim: `SpawnISB`, claims `ISBReview` task type inserted at the same pre-Captain point as BoSReview. BoSReview and ISBReview run in parallel; both must approve for the task to forward to Captain.
- Bypass mechanism: `// ISB-BYPASS: <AUDIT-NNN> <reason>`, same audit trail.

**Track D4-Senate — Senate.**

- Create `internal/senate/` package. `SenateChambers`, `SenateMemory`, `SenateReview` tables from `next-gen-agents.md`.
- New agent claim: `SpawnSenate`, claims `SenateReview` task type inserted by Chancellor between `ProposedConvoys` write and the `AwaitingChancellorReview` transition.
- `SenatorOnboarding` task: reads repo, produces candidate `FleetRules` rows (category `senate`, `agent_scope='senate:<repo>'`) + seeds `SenateMemory`; routes through D3's promotion pipeline (operator ratifies). The shakedown Senator is force-orchestrator itself.
- `senate-refresh` dog weekly (or after N commits per repo).
- Per-Senator LLM context assembled from FleetRules + static docs + `SenateMemory` + recent-commits digest from Librarian.

### Exit criteria

1. BoS: all 10 rules active, migrated into FleetRules, BoSReview task claims in the pipeline, CLAUDE.md cross-reference test (`TestPattern_P14_BoSRulesCoverCLAUDEMDInvariants`) green. A seeded violation of each rule triggers rejection in integration test.
2. ISB: all 10 rules active, ISBReview task claims in the pipeline, a seeded secret (test fixture) triggers ISB-001, shell injection fixture triggers ISB-002, etc. Bypass mechanism tested.
3. Senate: force-orchestrator Senator onboarded; at least one test plan run through the Senator; at least one candidate `FleetRules` row promoted via paired-run experiment (D3 round-trip, with BoS/ISB/Senate as the subject agent).
4. Pipeline integration: all three ship without breaking the existing Captain → Council → sub-PR → merge flow. Regression test: a task that would have merged before D4 still merges after D4 when BoS/ISB/Senate all approve.
5. Dashboard: security findings view, per-rule precision metrics, override-audit view, Senate review log per feature.
6. Full suite green under `-race -count=5`.

### Anti-cheat directives

- **No block-default on new rules.** Per `next-gen-agents.md`: every new rule ships at `severity=advise` for 30 clean firings before promoting to `block`. Prevents a noisy rule from locking the fleet out of all work.
- **No shortcutting the FleetRules migration.** Every rule in `internal/bos/rules/` and `internal/isb/rules/` must have its metadata in `FleetRules` (bootstrap migration + manual audit). A rule whose check body exists but has no FleetRules row is NOT active; this is by design and must be tested.
- **No bypass comment proliferation.** Operator-bypasses land in `SecurityFindings` with `disposition='overridden'` and a reason. Tests enforce: a BOS-BYPASS/ISB-BYPASS comment without a reason fails parse.
- **No Senator auto-editing its own rules.** Senator's own rules promote only via the operator-ratified pipeline. Librarian emits candidates, EC runs experiments, operator ratifies.
- **No LLM-layer ISB rule without a deterministic fallback attempt.** Every LLM-powered ISB rule documents the deterministic check it tried first and why it wasn't sufficient. Prevents LLM-layer sprawl.

### Verification procedure

```
# BoS
go test -tags sqlite_fts5 -run TestBoS -race -count=5 ./internal/bos/...
go test -tags sqlite_fts5 -run TestPattern_P14_BoSRulesCoverCLAUDEMDInvariants ./internal/audittools/...

# ISB
go test -tags sqlite_fts5 -run TestISB -race -count=5 ./internal/isb/...
# Seeded security violations trigger rejection:
go test -tags sqlite_fts5 -run TestISB_SeededViolations_AllRulesFire ./internal/agents/...

# Senate
go test -tags sqlite_fts5 -run TestSenate -race -count=5 ./internal/senate/...
go test -tags sqlite_fts5 -run TestSenatorOnboarding_ForceOrchestratorRepo ./internal/agents/...
go test -tags sqlite_fts5 -run TestSenatePromotion_RoundTrip ./internal/agents/...

# Pipeline integration
go test -tags sqlite_fts5 -run TestCommitPipeline_BoS_ISB_Senate_Captain_Council -race -count=3 ./...

# Full
go test -tags sqlite_fts5 -race -count=5 ./...
make smoke && make fuzz && make test-audit
```

### Closure report

`docs/closures/DELIVERABLE-4-CLOSURE.md` with:

- Per-rule status: ID, severity (advise/block), FleetRules row id, first-30-firings precision where applicable.
- Initial Senator onboarded (force-orchestrator) + SENATE.md rendered content.
- First Senate-rule promotion-via-experiment full trace.
- Integration test results.
- Anti-cheat self-check.
- Residual list.

---

## Deliverable 5 — Supply Chain Hygiene

### Mission

Ship five ISB rules that prevent hallucinated / typosquatted / stale / license-incompatible / known-vulnerable dependency introductions across the polyglot fleet (Ruby, Python, JavaScript/TypeScript, Java/Kotlin, plus Go for Force itself).

### Classification

Risk closure, implemented as an ISB rule pack with cross-ecosystem manifest scanning.

### Required reading

1. `/Users/jake.herman/code/force-orchestrator/docs/next-gen-agents.md` — §"Imperial Security Bureau" + §"Rules at launch."
2. `/Users/jake.herman/code/force-orchestrator/docs/closures/DELIVERABLE-4-CLOSURE.md` — for ISB state.
3. `https://github.com/google/osv-scanner` — OSV-scanner as the vuln-check mechanism (vendored as Go lib per D4 pattern).
4. `https://docs.aws.amazon.com/codeartifact/latest/ug/welcome.html` — AWS CodeArtifact registry layer Upstart uses as the canonical proxy for upstream registries.

### Prerequisites

D4 complete: all three tracks merged to main AND `docs/closures/DELIVERABLE-4-CLOSURE.md` filed. ISB live with its initial rule set.

### Registry layer: AWS CodeArtifact, not direct upstream

All registry queries route through Upstart's AWS CodeArtifact domain (`code-artifacts-prod`, account `801997600626`, region `us-east-1`). Per-ecosystem virtual repos:

| Ecosystem | CodeArtifact endpoint |
|---|---|
| PyPI | `https://code-artifacts-prod-801997600626.d.codeartifact.us-east-1.amazonaws.com/pypi/pypi-prod/simple/` |
| npm | `https://code-artifacts-prod-801997600626.d.codeartifact.us-east-1.amazonaws.com/npm/npm-prod/` |
| RubyGems | `https://code-artifacts-prod-801997600626.d.codeartifact.us-east-1.amazonaws.com/ruby/rubygems-prod/` |
| Maven/Gradle | `https://code-artifacts-prod-801997600626.d.codeartifact.us-east-1.amazonaws.com/maven/maven-prod/` |

Force scans against CodeArtifact (not direct upstream) because (a) it's the canonical proxy, (b) internal-network reliability beats internet, and (c) `aws codeartifact list-packages` gives a much better typosquat signal than scraping external popularity feeds — it returns the set of packages the org has actually pulled.

**Excluded:** the `grpc-generic-prod` repository. CodeArtifact's "generic" format is for arbitrary internal binary artifacts (e.g., protoc-gen build outputs); these have no upstream registry, no typosquat surface, and no public CVE feed. Different threat model — out of scope.

### Auth model: AWS SDK Go default credential chain

The Force daemon uses `aws-sdk-go-v2/config.LoadDefaultConfig` to read credentials from the same place the AWS CLI does (`~/.aws/credentials`, `~/.aws/sso/cache/`, env vars, instance profile). When the operator runs `umt artifacts` (Upstart's wrapper around `aws sso login` + `aws codeartifact get-authorization-token`), the resulting SSO credentials are picked up by the SDK on the next API call — no token caching inside Force.

The `umt artifacts` flow requires interactive SSO + an 8-character authorize-code click, so it cannot be automated. Token TTL is 8 hours. Force must therefore be designed to **degrade gracefully when the token expires**, not block all SUPPLY checks. See "Manifest-gating + deferral path" below.

### Manifest-gating + deferral path

ISBReview already runs per-commit on every astromech commit (queued from `astromech.go` post-commit hook). Per-commit network calls would burn through the token TTL and fire too often, so SUPPLY-* rules apply a two-stage filter:

1. **Manifest-gating** — SUPPLY-* rules only fire when the commit's diff actually touches a manifest file (`Gemfile`, `package.json`, `pom.xml`, `requirements.txt`, etc.). Source-file commits skip SUPPLY-* entirely. Most commits make no network calls.

2. **Deferral on token expiration** — when SUPPLY-* needs to query CodeArtifact and the SDK call returns a credentials/auth error:
   - Insert `SecurityFindings` row with `disposition='token_expired'` and a payload capturing `{rule_key, manifest_path, deps_added, branch, commit_sha, deferred_at}`.
   - Advise-mode through; do NOT block the commit.
   - Astromech keeps working at full speed.

Recovery happens via two complementary layers:

**Layer 1 — `supply-token-recheck` dog** (every 30 min):
- Probe CodeArtifact (`DescribeDomain`) with cached creds.
- On 401: emit notify-after Slack ping "umt artifacts token expired — re-auth to enable supply-chain checks." Done.
- On 200: walk all `SecurityFindings` with `disposition='token_expired'` AND `bureau='isb'` AND `rule_key LIKE 'SUPPLY-%'`. Group by branch.
  - For each branch: read the **current tip's** manifest files (latest state, not historical commit — current tip subsumes intermediate churn). If branch deleted/rebased, fall back to the SecurityFinding payload.
  - Re-run SUPPLY-* against the resolved dep set.
  - **Now clean** → flip original row to `disposition='resolved_late'`. Batched Slack ping per branch.
  - **Now flagged** → insert new `disposition='block'` row + flip original to `disposition='superseded'`. Slack ping with rule + dep + branch.

**Layer 2 — ConvoyReview gate**:
- `runConvoyReview` is extended: at DraftPROpen, if any branch in the convoy has `disposition='token_expired'` SUPPLY findings:
  - If CodeArtifact is callable: re-run the recheck loop inline (same logic as the dog).
  - If not: mark convoy `AwaitingSupplyRecheck` and fire Slack "Convoy #N has N deferred supply-chain checks. Run umt artifacts to unblock."
  - Operator's "Ship It" surface refuses to advance until findings are resolved (clean or escalated).

This makes "we could be 10 commits past the manifest change" a non-issue: the dog catches up automatically once the token recovers, and the convoy gate is the last-resort safety net before merge.

### Merge order within D5

| Order | Track | Branch | Depends on |
|---|---|---|---|
| 1 | D5-SupplyRules | `deliverable/5/supply-rules` | — |

### Phases

Single track, six phases; each phase test-green before the next starts.

| Phase | Scope |
|---|---|
| P0 | Foundation — AWS SDK CodeArtifact client (default cred chain, expiry-aware error mapping); manifest parsers per-ecosystem (Ruby, Python, JS/TS, Java/Kotlin, Go); `Repositories.license` column + LICENSE backfill migration; manifest-gating dispatch in ISBReview; `SecurityFindings.disposition='token_expired'` payload schema. |
| P1 | SUPPLY-001 (hallucinated package) + SUPPLY-002 (typosquat) — both registry-hit + deferral-path-aware. |
| P2 | SUPPLY-003 (stale) + SUPPLY-004 (license matrix) — registry-hit + hand-authored compatibility matrix at `internal/isb/rules/license_matrix.yaml`. |
| P3 | SUPPLY-005 (known-CVE blocking) — osv-scanner vendored as Go lib (D4 gosec/gitleaks pattern); independent of CodeArtifact. |
| P4 | `supply-allowlist-refresh` dog (daily `aws codeartifact list-packages` per ecosystem → typosquat allowlist) + `supply-token-recheck` dog (30-min health check + deferral replay) + ConvoyReview `AwaitingSupplyRecheck` gate + e2e fixture sweep across Ruby/Python/JS/Java/Go. |
| P5 | D5 strict verifier — Static + Heavy + Race shards, fresh-context cross-walk against this section. |

### Rule details

**`SUPPLY-001` — Hallucinated package rejection.**
- For each added dep, query CodeArtifact via AWS SDK: `codeartifact.DescribePackageVersion(domain, repository, format, package, version)`.
- 404 (`ResourceNotFoundException`) → reject with `[SUPPLY-001]` + ecosystem + name@version.
- Auth error → deferral path (token_expired).
- Cache valid responses ≤24h with the SDK's response cache; never cache `not-found` responses (a package that 404'd today might exist tomorrow).

**`SUPPLY-002` — Typosquat detection.**
- Per-ecosystem allowlist source: `aws codeartifact list-packages --domain code-artifacts-prod --domain-owner 801997600626 --repository <ecosystem>-prod`. The result is the set of packages the org has ever pulled — better signal than external popularity.
- For each added dep not already in the allowlist, compute Damerau–Levenshtein distance to every allowlist entry.
- If `distance <= 2` AND the added dep is not in `SystemConfig.supply_typosquat_preapproved`, reject with `[SUPPLY-002]` + suspected-original-package suggestion.
- Operator pre-approval lands in the preapproved set via durable audit (force CLI: `force supply preapprove <ecosystem> <name>`).

**`SUPPLY-003` — Stale package detection.**
- For each added dep, `DescribePackageVersion` returns metadata including `publishedTime`. (Where missing, fetch from upstream-mirror metadata via the same SDK call's response payload.)
- If `published < now() - supply_stale_threshold_days` (default 730 ≈ 2 years), reject with `[SUPPLY-003]` + suggestion to check for a maintained alternative.

**`SUPPLY-004` — License compatibility.**
- Repo's declared license lives in `Repositories.license` (new column; backfilled from each repo's `LICENSE` / `LICENSE.md` / `LICENSE.txt` at `AddRepo` time using SPDX-license-id detection).
- For each added dep, license metadata comes from `DescribePackageVersion` (CodeArtifact preserves upstream license fields). If absent, advise-mode + log for operator review (don't auto-block on uncertainty).
- Static compatibility matrix at `internal/isb/rules/license_matrix.yaml` keyed by SPDX IDs (~10 rows covers ~95% of cases). PR-reviewable when matrix changes.
- If incompatible per matrix, reject with `[SUPPLY-004]` + citation of dep license + repo license.
- Pairs not in the matrix → advise-mode + operator review (no LLM-decides-licenses, per anti-cheat).

**`SUPPLY-005` — Known vulnerability blocking.**
- On each manifest change, invoke vendored `osv-scanner` Go lib on the lock file (already supports Ruby, Python, npm, Maven, Go, etc.).
- High or Critical severity → reject with `[SUPPLY-005]` + finding details.
- Medium/Low → advise-mode (operator can promote to block via FleetRules edit).
- Independent of CodeArtifact / AWS auth; works during token-expired windows.

### Rule configuration

All five are FleetRules (category `isb`, agent_scope `all`). Bootstrap migration seeds them at `advise` severity. Promotion to `block` goes through D3's paired-run mechanism.

### Exit criteria

1. `Repositories.license` column present; backfill populated from each repo's LICENSE file (SPDX-detected) via a one-time migration that runs at `runMigrations`.
2. Five rules present in `FleetRules`, active, `rule-renderer` has written `isb/finders/supply_*.yaml` (one per rule) into the appropriate target-repo location during `force render-rules`.
3. AWS SDK CodeArtifact client wired with default credential chain. `SecurityFindings.disposition='token_expired'` path covered: when AWS calls return auth error, ISBReview logs the deferred finding and proceeds in advise-mode without blocking.
4. Manifest-gating: SUPPLY-* rules only fire when commit diff includes one of the recognized manifest files; source-only commits make zero network calls. AST/parser-based detection in `internal/isb/scanners/manifests/` covers Ruby (`Gemfile`, `Gemfile.lock`, `*.gemspec`), Python (`requirements.txt`, `Pipfile`, `Pipfile.lock`, `pyproject.toml`, `poetry.lock`, `setup.py`), JS/TS (`package.json`, `package-lock.json`, `yarn.lock`, `pnpm-lock.yaml`), Java/Kotlin (`pom.xml`, `build.gradle`, `build.gradle.kts`), Go (`go.mod`, `go.sum`).
5. Integration tests per rule per ecosystem (5 rules × 5 ecosystems = 25 fixture-driven test cases). Minimum coverage:
   - `TestSupply001_HallucinatedRubyGem_Rejected`, `..._HallucinatedNpmPackage_Rejected`, `..._HallucinatedPyPI_Rejected`, `..._HallucinatedMaven_Rejected`, `..._HallucinatedGoModule_Rejected`.
   - `TestSupply002_NpmTyposquat_Rejected` (`expres` instead of `express`), `..._RubyTyposquat_Rejected`, etc.
   - `TestSupply003_StalePackage_Rejected` per ecosystem.
   - `TestSupply004_LicenseIncompatible_Rejected` (fixture: repo MIT, adds GPL-3.0 dep) per ecosystem.
   - `TestSupply005_KnownCVE_Rejected` per ecosystem.
6. Bypass mechanism: `// SUPPLY-BYPASS: <AUDIT-NNN> <reason>` (or `# SUPPLY-BYPASS:` for Ruby/Python) → lands in `SecurityFindings` with `disposition='overridden'`. Bypass parser handles per-language comment syntax.
7. `supply-allowlist-refresh` dog (daily) populates `SystemConfig.supply_allowlist_<ecosystem>` from `aws codeartifact list-packages`.
8. `supply-token-recheck` dog (every 30 min) probes CodeArtifact health, replays deferred findings on recovery, fires notify-after Slack on first expiry detection (debounced).
9. ConvoyReview extension: convoys with `disposition='token_expired'` SUPPLY findings on any ask-branch are gated `AwaitingSupplyRecheck` until resolved. Operator Ship-It surface honors the gate.
10. `TestListDogs` count matches new total (+2 from D4).

### Anti-cheat directives

- **No hardcoded allowlists for popular packages.** Allowlist comes from `aws codeartifact list-packages` (org-actual usage). Static fallback is OK only when CodeArtifact is unreachable, and must be timestamped + clearly marked stale.
- **No registry-hit caching of `not-found` responses.** A package that 404'd yesterday might be real today; a "false ham" cache of negative results is the worst failure mode. Positive caches OK ≤24h.
- **No bypass-by-default.** Every bypass requires a reason string + AUDIT-NNN; tests enforce.
- **No license matrix shortcut.** The compatibility matrix is hand-authored YAML, reviewed and committed; do not use an LLM to "decide" license compatibility at check time. Pairs absent from the matrix → advise-mode + operator review, never auto-allow or auto-deny.
- **No silent token-expired passthroughs.** Every auth-error path must emit a `SecurityFindings` row with `disposition='token_expired'`. Pattern P-SupplyDeferral (new audittools test) walks `internal/isb/rules/supply_*.go` and rejects any registry-call site that catches an auth error without logging.
- **No Slack-message-triggers-merge.** Per the D4 retrospective: the operator's `umt artifacts` recovery does not authorize any code-modifying action; it only re-enables the read-only SUPPLY checks.

### Verification procedure

```
# Build + suite
go test -tags sqlite_fts5 -run TestSupply00 -count=3 ./internal/isb/...   # 25 fixtures green
go test -tags sqlite_fts5 -race -count=5 ./...                             # full suite green under -race

# Schema + seeding
sqlite3 holocron.db "SELECT COUNT(*) FROM FleetRules WHERE category='isb' AND rule_key LIKE 'SUPPLY-%'"  # expect 5
sqlite3 holocron.db "SELECT COUNT(*) FROM Repositories WHERE license IS NULL OR license=''"               # expect 0 after backfill

# Renderers — N/A: SUPPLY rules use RenderTo='discard' + DBFleetRulesGate (matching D4 ISB-001..010);
# the rule body is selected at review time from the live FleetRules table, no on-disk YAML render.
# See docs/closures/DELIVERABLE-5-CLOSURE.md residual #3 for the divergence rationale.
sqlite3 holocron.db "SELECT COUNT(*) FROM FleetRules WHERE rule_key LIKE 'SUPPLY-%' AND render_to='discard'"  # expect 5

# Dogs
./force daemon --dry-run | grep -E "supply-(allowlist-refresh|token-recheck)"   # both registered

# Deferral path (manual)
unset AWS_ACCESS_KEY_ID                 # simulate token-expired
go test -tags sqlite_fts5 -run 'TestSUPPLY00._TokenExpired_DeferralLogged' ./internal/isb/...  # expect deferred-finding written (per-rule: SUPPLY-001/003/004)
```

### Closure report

`docs/closures/DELIVERABLE-5-CLOSURE.md` with:

- Per-rule status (BoS-style table): rule-id × ecosystems supported × test names × FleetRules row-id × default severity.
- AWS CodeArtifact endpoints used + which AWS SDK service operations are called per rule.
- `Repositories.license` backfill: row count populated, SPDX-ids detected, ambiguous-license rows surfaced for operator review.
- Allowlist source per ecosystem + refresh dog cadence + last-refresh timestamp at closure time.
- License compatibility matrix: matrix file path, row count, anything-not-in-matrix → advise-mode count.
- Deferral-path coverage: count of `disposition='token_expired'` rows resolved during D5 testing, count escalated to `block` on recheck.
- Integration test results (25 cases × ecosystem matrix).
- Anti-cheat self-check (one line per directive above + evidence file:line).
- Residual list — explicitly NONE blocking.

---

## Deliverable 5.5 — Staged Convoys — Commander-Drafted Phase Pipelines

### Mission

Give the Commander first-class tooling to decompose a single logical convoy into N sequenced stages, each with its own ask-branches and PRs and a configurable gate to advance. Single-stage convoys (today's behavior) remain the default; multi-stage is opt-in at planning time. ZDM is one motivating use case among many — staged convoys are the general primitive for any work that shouldn't land in a single PR.

### Classification

Architectural extension to the convoy execution model. Schema + state machine + Commander integration + new dog + dashboard surface.

### Why this is a tool the Commander needs

Single-PR landings are wrong for many real situations Upstart engineers hit regularly:

| Reason | Example |
|---|---|
| ZDM schema work | column-add → backfill → dual-write → cutover → drop-old |
| Feature flag rollout | scaffold + flag-off → impl behind flag → 1% enable → 100% enable → flag removal |
| Reviewer cognitive load | "this 5000-line PR is unreviewable, split it" — data model + API → client → UI |
| Risk reduction | observability/metrics first → risky change → measure regression in real prod data |
| In-repo dependency ordering | refactor a shared utility → update callers (callers depend on the refactor) |
| Async API contracts | publish new event schema → producers dual-emit → consumers read new → retire old |
| Library breaking-change migration | bump version on one module → migrate next tranche of call sites → repeat |
| Parallel-work serialization | two engineers' independent work would conflict; convoy serializes them |
| Soak between risky changes | "land one phase per day" to limit blast radius even without ZDM |
| Capacity-aware enablement | turn on for low-traffic repos first, then high-traffic ones |

The unifying primitive is the same: **N sequential phases, each independently shippable, with a configurable gate between them.** The Commander is already the planning agent that decides convoy shape; staged decomposition is a natural extension of its role.

### Required reading

1. `/Users/jake.herman/code/force-orchestrator/docs/closures/DELIVERABLE-3-CLOSURE.md` — for ConvoyReview shape (which becomes per-stage post-D5.5).
2. `/Users/jake.herman/code/force-orchestrator/docs/closures/DELIVERABLE-4-CLOSURE.md` — for Senate review patterns (Senate runs against staged convoys at each stage's DraftPROpen).
3. `/Users/jake.herman/code/force-orchestrator/docs/closures/DELIVERABLE-5-CLOSURE.md` — for SUPPLY-* deferral path (the `AwaitingSupplyRecheck` gate runs per-stage post-D5.5).

### Prerequisites

D5 complete: SUPPLY-* rules + deferral path + `AwaitingSupplyRecheck` gate live, AND `docs/closures/DELIVERABLE-5-CLOSURE.md` filed.

### Merge order within D5.5

| Order | Track | Branch | Depends on |
|---|---|---|---|
| 1 | D5.5-StagedConvoys | `deliverable/5.5/staged-convoys` | — |

Single track; six phases (P0..P5) test-green-before-next.

### Architecture

**Schema additions:**

```
ConvoyStages(
  id INTEGER PRIMARY KEY,
  convoy_id INTEGER NOT NULL REFERENCES Convoys(id),
  stage_num INTEGER NOT NULL,           -- 1-indexed; ordered execution
  intent_text TEXT NOT NULL,            -- Commander's reason for this stage
  status TEXT NOT NULL DEFAULT 'Pending',  -- Pending|Open|AllPRsMerged|AwaitingGate|GatePassed|Verified|Failed
  gate_type TEXT,                       -- soak_minutes|operator_confirm|probe_endpoint|release_label_present|metric_threshold|null
  gate_config_json TEXT NOT NULL DEFAULT '{}',  -- per-gate-type config
  gate_timeout_minutes INTEGER NOT NULL DEFAULT 10080,  -- 7 days default; escalation after
  opened_at TEXT,
  all_prs_merged_at TEXT,
  gate_passed_at TEXT,
  completed_at TEXT,
  UNIQUE(convoy_id, stage_num)
);

ConvoyAskBranches.stage_id INTEGER  -- FK to ConvoyStages.id; default migration sets all existing rows to stage 1 of an implicit single-stage convoy
Convoys.staging_mode TEXT NOT NULL DEFAULT 'single'  -- 'single' | 'staged'
Convoys.staging_strategy TEXT NOT NULL DEFAULT 'strict'  -- 'strict' | 'merge_parallel' | 'stacked'; only meaningful when staging_mode='staged'
Repositories.release_label_pattern TEXT NOT NULL DEFAULT ''  -- per-repo regex; empty means repo doesn't use release labels
```

**Staging strategies (forward-compat enum; only `strict` ships in D5.5).**

The `staging_strategy` column captures *when* stage N's astromechs start working and *what* their base branch is. Three modes are recognized at the schema level so future deliverables can opt in without schema migration churn:

| Strategy | When stage N opens | Stage N's base branch | Inter-stage rebase? | Status in D5.5 |
|---|---|---|---|---|
| `strict` | After stage N-1's PRs merge AND its gate passes (`Verified`) | `main` HEAD at stage-open time | No — stage N-1 already merged, no chain | **Implemented (default).** |
| `merge_parallel` | As soon as stage N-1's PRs merge (gate may not have passed yet) | `main` HEAD at merge time | No — still branched off main | Schema-recognized; planner rejects. Add when needed. |
| `stacked` | Concurrent with stage N-1 | Stage N-1's ask-branch tip | Yes — `convoy-stage-rebase` dog needed | Schema-recognized; planner rejects. Add when needed. |

D5.5 implements only `strict`. The Commander's planner validates `staging_strategy` at convoy creation and emits a clear "not yet supported" error for `merge_parallel` or `stacked`. Schema migration in D5.5 sets all existing convoys to `staging_strategy='strict'` (it's a no-op for single-stage convoys).

When future deliverables add `merge_parallel`, the runtime branches on `staging_strategy` to choose stage-open trigger; no schema change needed. When `stacked` lands, the schema may add `ConvoyStages.base_commit_sha` and `ConvoyStages.rebase_attempts` as nullable additions — those are forward-compat-clean ALTER ADD COLUMN ops.

**Why include the strategy enum now even though only `strict` ships:**

1. Avoids painting future deliverables into a corner where they'd need destructive schema migrations.
2. Makes the Commander's planning prompt forward-compatible: it can already emit `"staging_strategy": "strict"` explicitly, and adding new values later is a prompt change + validator change, not a schema change.
3. Serializes operator intent durably: a convoy that was planned-but-not-yet-executed when `merge_parallel` lands can be promoted with one column update, not a re-plan.

**State machine:**
- Convoy-level states stay flat (`DraftPROpen`, `Shipped`, `Abandoned`). Richer state lives in `ConvoyStages.status`.
- Per-stage progression: `Pending → Open → AllPRsMerged → AwaitingGate → GatePassed → Verified`.
- Convoy is `Shipped` only when ALL stages reach `Verified`.
- Astromech work for stage N is gated by `staging_strategy`:
  - `strict` (D5.5 default): stage N's ask-branches are not opened, no worktrees dispatched, no astromechs claim work, until stage N-1 reports `GatePassed` (which transitions both: stage N-1 → `Verified`, stage N → `Open`).
  - `merge_parallel` (future): stage N opens on `AllPRsMerged`, not `GatePassed`. Stage N's astromechs work in parallel with stage N-1's gate evaluation. Stage N's PRs do NOT open for review until stage N-1 hits `GatePassed`.
  - `stacked` (future): stage N opens at convoy-creation time alongside stage N-1. Astromechs work in parallel; stage N's ask-branch is based on stage N-1's tip; an inter-stage rebase dog propagates stage N-1 commits forward.

**Stage N base branch (D5.5 strict mode):** stage N's ask-branches are created off `main` HEAD at the moment stage N transitions to `Open`. Because stage N-1 has already merged to main by definition, stage N's base contains stage N-1's commits — no inter-stage rebase chain is needed. Stage N's ask-branches still rebase to track main as unrelated convoys merge, using the existing `ConvoyAskBranches.failed_rebase_attempts` machinery from D2/D3.

**Stage gate types (pluggable, registry-based):**

The gate plug interface (P1) uses a registry pattern: any registered gate type can appear inside compounds. Eight gate types ship across P1 (baseline) and P3 (advanced), plus two compound gates available from P1.

**Baseline gates (P1):**

| Gate type | Config | Mechanism |
|---|---|---|
| `soak_minutes` | `{minutes: int}` | Wait N minutes after `all_prs_merged_at` before flipping `GatePassed`. |
| `operator_confirm` | `{prompt: string}` | Operator clicks "advance" in the dashboard; stage transitions on operator action. |
| `null` | `{}` | No gate; transitions immediately on `AllPRsMerged → Verified`. Allowed only on the terminal stage of a convoy. |
| `all_of` | `{gates: [Gate]}` | Compound: passes when ALL children pass. Fails when ANY child fails (short-circuit). Stays `AwaitingGate` while any child is pending. |
| `any_of` | `{gates: [Gate]}` | Compound: passes when ANY child passes. Fails only when ALL children fail. |

**Advanced leaf gates (P3):**

| Gate type | Config | Mechanism |
|---|---|---|
| `release_label_present` | `{polling_interval_minutes: int}` (the regex pattern lives on `Repositories.release_label_pattern`, not here) | For each merged PR in the stage, polls `gh pr view --json labels` and checks against the PR's repo's `Repositories.release_label_pattern`. Gate passes when ALL merged PRs carry a matching label per their repo's pattern. **Repo-specific:** if any repo touched by the stage has no `release_label_pattern` configured, the planner rejects the gate at convoy creation with an explicit "configure pattern or pick a different gate" error — no silent fallback. |
| `probe_endpoint` | `{url: string, method: GET\|POST, expected_status: int, body_match_regex: optional string, timeout_seconds: int, target_env: prod\|staging, headers: optional map}` | Calls the configured URL; verifies the change is actually working in the target environment. Use cases: new endpoint exists and returns 200, existing endpoint now returns a new field, admin endpoint lists a newly registered consumer. |
| `datadog_metric_threshold` | `{metric_query: string, comparator: lt\|gt\|eq\|lte\|gte, threshold: float, sample_window_minutes: int}` | Queries Datadog API for time-series metrics. Use cases: "error rate stayed below 0.1% for 30 min," "p95 latency didn't increase by >10% over baseline," "request throughput stayed flat after stage 1 merged." |
| `databricks_query_threshold` | `{sql_query: string, comparator: lt\|gt\|eq\|lte\|gte, threshold: float, warehouse_id: string, timeout_seconds: int}` | Executes a SQL query against the configured Databricks warehouse, comparing scalar result to threshold. Use cases: "backfill complete: count of rows with new column populated == total count," "no data drift: distribution match within tolerance," "all migration shards reported success." |

**Compound gate semantics:**

Compound gates support arbitrary boolean expressions over leaf gates. Children can themselves be compounds, allowing nested logic. Validation rules:

- Maximum nesting depth: 5 levels (planner rejects deeper). Prevents pathological configs.
- Empty children: `all_of: []` and `any_of: []` rejected at planning time.
- Single-child compound: allowed but emits a planner warning ("compound with single child is equivalent to that child").
- Per-child timeouts NOT supported; the convoy stage's `gate_timeout_minutes` applies to the whole compound.
- Compound gates implement the same `Gate` interface as leaf gates; the registry treats them uniformly.

Example — ZDM phase 2→3 gate (after backfill, before reading from new column):

```json
{
  "type": "all_of",
  "gates": [
    {"type": "soak_minutes", "config": {"minutes": 60}},
    {
      "type": "databricks_query_threshold",
      "config": {
        "sql_query": "SELECT COUNT(*) FROM users WHERE user_account_status IS NULL",
        "comparator": "eq",
        "threshold": 0,
        "warehouse_id": "...",
        "timeout_seconds": 60
      }
    },
    {
      "type": "datadog_metric_threshold",
      "config": {
        "metric_query": "avg:trace.http.request.errors{service:user-service}",
        "comparator": "lt",
        "threshold": 0.001,
        "sample_window_minutes": 30
      }
    }
  ]
}
```

Reads as: *"wait at least 60 minutes AND every user row has the new column populated AND error rate stayed below 0.1% over the last 30 minutes."* All three signals must pass; any failure fails the gate.

**Commander integration:**

When the Commander drafts a convoy from a Feature, its planning prompt is extended to ask: *"Should this be staged?"* Output JSON shape becomes (for staged mode):

```json
{
  "staging_mode": "staged",
  "staging_strategy": "strict",
  "stages": [
    {
      "stage_num": 1,
      "intent": "Add nullable user_account_status column + migration",
      "tasks": [...],
      "gate": {
        "type": "release_label_present",
        "config": {"pattern": "release-202\\d+\\.\\d+", "polling_interval_minutes": 15}
      }
    },
    {
      "stage_num": 2,
      "intent": "Dual-write to both old and new column",
      "tasks": [...],
      "gate": {"type": "soak_minutes", "config": {"minutes": 1440}}
    },
    {
      "stage_num": 3,
      "intent": "Read from new column only",
      "tasks": [...],
      "gate": null
    }
  ]
}
```

The Commander reasons about *why* each stage is independently safe, *what* the gate verifies, and *what* the rollback story is per stage. This is captured in `ConvoyStages.intent_text` and surfaced to ConvoyReview at each stage's DraftPROpen.

**Stage advancement dog:** `convoy-stage-watch` (every 5 min) walks active stages, evaluates pending gates, advances stage state. Per stage:
- If `Open` and all stage's ask-branch PRs merged → flip to `AllPRsMerged`, stamp `all_prs_merged_at`.
- If `AllPRsMerged` → flip to `AwaitingGate`.
- If `AwaitingGate` and gate evaluator returns pass → flip to `GatePassed`, stamp `gate_passed_at`. Spawn next-stage opening task.
- If `AwaitingGate` and `now - all_prs_merged_at > gate_timeout_minutes` → emit escalation (operator surface + Slack).

**ConvoyReview per stage:** runs at each stage's DraftPROpen against post-previous-stage main (i.e., what main looks like after stage N-1 has merged). The unified-diff review is now per-stage; the LLM sees only that stage's intent + diff. Cleaner mental model than today's monolithic review for big convoys.

**D5 forward-compat:** the `AwaitingSupplyRecheck` gate from D5 ConvoyReview runs per-stage at each stage's DraftPROpen. SUPPLY-* findings are scoped to the ask-branch they were detected on, which is already stage-scoped post-D5.5 by virtue of `ConvoyAskBranches.stage_id`.

### Phases

| Phase | Scope |
|---|---|
| P0 | Schema (`ConvoyStages` + `ConvoyAskBranches.stage_id` + `Convoys.staging_mode` + `Convoys.staging_strategy` + `Repositories.release_label_pattern`); forward-compat migration (all existing convoys → `staging_mode='single'`, `staging_strategy='strict'`, single ConvoyStage at stage 1, gate=null); store helpers (CreateStage, AdvanceStage, ListStages, GetStage, GetRepositoryReleaseLabelPattern); baseline tests including the forward-compat path. |
| P1 | Gate plug interface (`type Gate interface { Evaluate(ctx, stage) (passed bool, reason string, err error) }`); 5 baseline gates (`soak_minutes`, `operator_confirm`, `null`, plus compounds `all_of` and `any_of`); registry pattern such that future leaf gates plug in without re-architecting; nesting-depth + empty-children + single-child validation; `convoy-stage-watch` dog skeleton with stage advancement transitions. |
| P2 | Commander integration: planning-prompt extension + multi-stage JSON output validator + ConvoyReview per-stage scoping + per-stage Senate review hook. Astromech dispatch gated on stage status. `staging_strategy` validator rejects `merge_parallel` and `stacked` with explicit "not yet supported" errors. |
| P3 | 4 advanced leaf gates (`probe_endpoint`, `release_label_present`, `datadog_metric_threshold`, `databricks_query_threshold`); per-repo `release_label_pattern` enforcement at planning time (planner errors when stage touches a repo without a pattern); gate-timeout escalation surface; clients/datadog and clients/databricks Go interfaces (per CLAUDE.md cross-agent service-interface convention). |
| P4 | Dashboard view (stages list per convoy, gate status, advance/skip/abort buttons), notify-after on stage transitions, stage audit trail surface. |
| P5 | D5.5 strict verifier — Static + Heavy + Race shards, fresh-context cross-walk against this section. Emphasis on anti-cheat directives + forward-compat regression for existing single-stage convoys. |

### Exit criteria

1. Forward-compat: every existing convoy from D3/D4/D5 era continues to function with `staging_mode='single'`, `staging_strategy='strict'`, one ConvoyStage row at stage 1, gate=null. No behavior change for single-stage convoys. Migration tests prove this.
2. Schema parity: `createSchema` + `runMigrations` + `schema/schema.sql` agree on the new tables and columns including `Convoys.staging_strategy`. `TestSchemaParity` green. The `staging_strategy` enum is enforced at the agent layer, not via SQL CHECK constraint, so future values are accepted without migration.
3. `staging_strategy` validator: convoy creation rejects `merge_parallel` and `stacked` with a clear "not yet supported in D5.5" error message that names the deliverable that will add support. Test coverage walks each unsupported value.
3. All 9 gate types implemented (5 baseline in P1 + 4 advanced leaves in P3) with dedicated unit tests + at least one integration test per leaf type that walks a staged convoy through advancement. Compound gates (`all_of`, `any_of`) covered by tests that nest at least 3 levels deep + edge cases (empty children rejected, single-child warning, depth-cap enforcement).
4. `convoy-stage-watch` dog registered; `TestListDogs` count incremented; the dog correctly handles every stage status transition.
5. Commander integration: planning prompt includes the multi-stage option; emitted JSON validated by `internal/agents/commander/staging_validator.go`; multi-stage convoys land in the schema correctly via `runCommanderTask`.
6. ConvoyReview runs per-stage with stage-N main = post-stage-(N-1) main. End-to-end test walks a 3-stage convoy through ConvoyReview at each stage.
7. Astromech dispatch gating: a stage-N task is never claimed by an astromech while stage N is `Pending`. Audit test (Pattern P-StageGate) walks the dispatch path and rejects any code path that would claim a Pending-stage task.
8. Operator dashboard: `/api/convoys/<id>/stages` returns the ConvoyStages list; `POST /api/convoys/<id>/stages/<stage_num>/advance` is the operator-confirm action; SPA renders the staged-convoy view.
9. Stage-timeout escalation fires correctly when a gate hangs past `gate_timeout_minutes`; escalation includes operator surface + notify-after Slack ping.
10. Bypass mechanism for emergency stage advancement: operator-only, requires `AUDIT-NNN <reason>` in the advance request, lands in audit trail. Tested.

### Anti-cheat directives

- **No silent gate skip.** Every `gate_passed_at` flip is durable + audited. Tests assert `gate_passed_at` is non-null only after a real gate evaluation, never auto-set.
- **No Commander single-stage → multi-stage promotion post-hoc** without explicit operator confirmation. Otherwise the Commander could re-plan to hide intent drift. Audit Pattern P-StagingPromotionConfirm enforces.
- **No null-gate without justification.** `gate_type=null` is allowed only for the terminal stage (no successor). Audit test rejects null-gate on non-terminal stages.
- **No skip-stage-N-because-stage-N+1-merged-first.** Out-of-order merges that would bypass a gate must trigger escalation, not silent advance. The `convoy-stage-watch` dog refuses to advance non-current stages.
- **No astromech pre-staging.** Astromechs cannot hold a worktree on a `Pending` stage. Pattern P-StageGate (AST audit) walks the dispatch path and rejects any code path that would claim a Pending-stage task. Verifier executes it.
- **No Slack-message-triggers-stage-advance.** Per the D4 retrospective: only the operator's explicit dashboard action or a real gate evaluation can advance a stage. Slack pings are read-only signal.

### Verification procedure

```
# Build + suite
go test -tags sqlite_fts5 -count=3 ./internal/agents/staged/...        # all gate tests green
go test -tags sqlite_fts5 -race -count=5 ./...                          # full suite green under -race

# Schema + forward-compat
sqlite3 holocron.db "SELECT COUNT(*) FROM ConvoyStages WHERE convoy_id IN (SELECT id FROM Convoys WHERE staging_mode='single')"  # equals count of pre-existing convoys
sqlite3 holocron.db "SELECT COUNT(*) FROM ConvoyAskBranches WHERE stage_id IS NULL"  # expect 0 after forward-compat migration

# Dog registration
./force daemon --dry-run | grep convoy-stage-watch                      # registered

# E2E walk
go test -tags sqlite_fts5 -run TestStagedConvoy_E2E_3Stages ./internal/agents/...  # walks 3-stage convoy through DraftPROpen × 3
```

### Closure report

`docs/closures/DELIVERABLE-5.5-CLOSURE.md` with:

- Per-gate-type status: implementation file:line × gate evaluator test names × integration test name × default config.
- Forward-compat audit: count of existing convoys migrated to `staging_mode='single'` cleanly; count of ConvoyAskBranches assigned `stage_id`.
- Commander prompt extension: file:line of the staging-mode planning prompt; sample multi-stage convoy planned end-to-end.
- ConvoyReview per-stage scoping: evidence that stage-N review sees post-stage-(N-1) main and only stage-N intent.
- Astromech dispatch gating: pattern P-StageGate test file:line.
- Dashboard surface: endpoint list + SPA view file:line.
- Anti-cheat self-check (one line per directive + evidence file:line).
- Residual list — explicitly NONE blocking.

---

## Deliverable 6 — Synthetic Onboarding CLI

### Mission

`force onboard <repo>` reuses D4's Senator bootstrap pipeline to produce a human-readable `ONBOARDING.md` at the target repo root.

### Classification

Feature (operator UX).

### Required reading

1. `/Users/jake.herman/code/force-orchestrator/docs/next-gen-agents.md` — §"Senate" + §"Bootstrap + refresh."
2. `/Users/jake.herman/code/force-orchestrator/cmd/force/` — existing CLI structure.

### Prerequisites

D5 complete: supply-chain ISB rules merged to main AND `docs/closures/DELIVERABLE-5-CLOSURE.md` filed. (Operator-decreed strict sequential ordering; technically only D4 is structurally required.) Senator bootstrap pipeline from D4 present and reusable.

### Merge order within D6

Single track; no internal ordering.

| Order | Track | Branch | Depends on |
|---|---|---|---|
| 1 | D6-OnboardCLI | `deliverable/6/onboard-cli` | — |

### Work tracks

Single track.

**Scope:**

- `cmd/force/onboard.go` — new subcommand.
- `force onboard <repo-spec>` where `<repo-spec>` is either a registered repo name or a path to a repo on disk.
- Invokes the Senator bootstrap pipeline's knowledge-synthesis step without creating a Senator. Reuses:
  - README reader
  - Public API walker
  - Recent-commit digester
  - Task-outcome-pattern reader (if registered repo)
  - Librarian memory query for this repo
- Renders to `ONBOARDING.md` at the repo's local path:
  - Section: "What this repo does" (1-paragraph synthesis of README + top-level packages).
  - Section: "Public API surface" (exported interfaces, HTTP handlers, CLI commands, with 1-line descriptions).
  - Section: "Key modules" (per top-level directory, 2-3 sentences).
  - Section: "Recent activity" (last 90 days summarized).
  - Section: "Known fragility areas" (if task-outcome history exists, aggregated rejection reasons; else "no fleet activity yet").
  - Section: "Common conventions" (pulled from repo's CLAUDE.md / CONTRIBUTING.md / existing SENATE.md if any).
- On re-run, regenerates against current repo state. Operator can configure cadence via `force onboard --refresh <repo>`.

### Exit criteria

1. `force onboard --help` shows the subcommand.
2. `force onboard force-orchestrator` produces `ONBOARDING.md` at this repo's root; output structure per spec.
3. Operator smoke-test on one additional repo; output is judged useful (this is qualitative; closure report cites operator confirmation).
4. Unit test: `TestOnboardingSynthesizesFromSenatorPipeline` asserts the CLI invokes the same internal function the SenatorOnboarding task uses.
5. `ONBOARDING.md` output carries the same `<!-- AUTO-GENERATED -->` header as other rendered files; pre-commit hook rejects hand-edits.

### Anti-cheat directives

- **No duplicating Senator bootstrap code.** The CLI must use the same function the `SenatorOnboarding` task type uses; tests enforce via AST walk.
- **No committing the rendered `ONBOARDING.md` into force-orchestrator's own repo unless the operator explicitly runs it.** This is a CLI, not a dog — it runs on demand.

### Verification procedure

```
force onboard --help  # shows
go test -tags sqlite_fts5 -run TestOnboarding ./cmd/force/...  # green
force onboard force-orchestrator  # produces ONBOARDING.md; operator reviews
```

### Closure report

`docs/closures/DELIVERABLE-6-CLOSURE.md` with CLI help-text pasted, sample `ONBOARDING.md` content, reuse-verification evidence, anti-cheat self-check.

---

## Deliverable 7 — Model-Tier Optimization Experiments

### Mission

Run paired-run experiments that downgrade eight candidate agents from the current default model to Haiku 4.5. Ship the downgrade if quality holds and cost drops materially.

### Classification

Feature (cost reduction).

### Required reading

1. `/Users/jake.herman/code/force-orchestrator/docs/paired-runs.md` — §"Primitive", §"Scoring and Significance", §"Engineering Corps."
2. `/Users/jake.herman/code/force-orchestrator/docs/closures/DELIVERABLE-3-CLOSURE.md` + `docs/closures/DELIVERABLE-4-CLOSURE.md`.

### Prerequisites

D6 complete: onboarding CLI merged to main AND `docs/closures/DELIVERABLE-6-CLOSURE.md` filed. (Operator-decreed strict sequential ordering; technically only D3 is structurally required for the experiment harness.)

### Merge order within D7

Each experiment is its own "track" in the paired-runs sense — own YAML, own operator ratification, own experiment run, own PromotionProposal. But each experiment's SHIPPING commit (when a winner is ratified and a model swap lands in code) is a PR that must merge to main in a particular order.

| Order | Experiment | Subject agent | Branch (ship PR) | Depends on | Parallelizable run with |
|---|---|---|---|---|---|
| 1 | E7-1 | Boot | `deliverable/7/ship-boot-haiku` | — | all others |
| 2 | E7-2 | memory_rerank | `deliverable/7/ship-rerank-haiku` | — | all others |
| 3 | E7-3 | PR-review-triage | `deliverable/7/ship-prtriage-haiku` | — | all others |
| 4 | E7-4 | Librarian (synthesis) | `deliverable/7/ship-librarian-haiku` | — | all others |
| 5 | E7-5 | Diplomat (summarization) | `deliverable/7/ship-diplomat-haiku` | — | all others |
| 6 | E7-6 | Medic (decision classification) | `deliverable/7/ship-medic-haiku` | — | all others |
| 7 | E7-7 | Commander | `deliverable/7/ship-commander-haiku` | — | all others |
| 8 | E7-8 | Chancellor | `deliverable/7/ship-chancellor-haiku` | — | all others |

**Parallelism model.** All eight experiments run concurrently via factorial orthogonal-dimension overlap: each varies on the `model` dimension for a DIFFERENT subject agent, so their dimensions are disjoint (subject-agent is part of dimension identity per paired-runs.md §"Factorial Scoring"). Experiments progress through their Bayesian lifecycles independently. Each ship-PR lands on main when its experiment has declared a winner AND passed confirm phase AND the operator has ratified the PromotionProposal.

**Suggested ship order.** Ship cheapest-to-verify first (Boot, memory_rerank are the simplest decision tasks) and largest-blast-radius last (Chancellor touches every plan). Ordered as above. Operator may reshuffle based on actual experiment-termination order; the numeric column is a suggestion, not a strict gate.

**Hard constraint.** No two ship-PRs may merge simultaneously (to keep `FleetRules` writes linearized). Operator ratifies and merges them one at a time. The 30-day post-ship monitoring window for each PR runs concurrently; all eight windows must clear before D7's closure report is filed.

### Work tracks

Eight experiments, each its own track. May run in parallel via factorial orthogonal-dimension overlap: all eight vary on the `model` dimension on DIFFERENT agents, so overlap is allowed.

**Experiment E7-1 — Chancellor model downgrade.**

- Metric primary: `chancellor_plan_merge_rate@latest` (did the resulting convoy's plan merge to main without Chancellor-plan-origin rework?).
- Metric secondary: `chancellor_cost_per_call`.
- Arms: `{current_default, haiku-4.5}`.
- Stakes tier: `medium`.
- `min_practical_effect`: 0.05 (no more than 5% regression in merge rate acceptable).
- Ship gate: `P(haiku merge rate >= current - 0.05) > 0.95` AND `haiku cost per call < 0.4 × current cost per call`. Confirm phase required.

(Template; replicate for each agent.)

**Experiments:** Chancellor, Commander, Medic (decision classification only — not the code-fix path), Boot, PR-review-triage, Librarian (synthesis), memory_rerank, Diplomat (summarization paths only).

Per experiment: Engineering Corps authors the YAML from this brief; operator ratifies; experiment runs; terminates; if winner declared → confirm phase → if confirm passes → PromotionProposal → operator ratifies → ship.

### Exit criteria

1. Eight experiments terminated (either promoted with evidence or declared null/inconclusive with evidence preserved).
2. For promoted agents: post-ship 30-day monitoring shows no regression. Retention metric visible on dashboard.
3. Aggregate fleet cost per convoy, measured against the `baseline-2026` holdout at T+30 days post-last-ship, shows measurable delta visible on fleet-progress dashboard.
4. `docs/closures/DELIVERABLE-7-CLOSURE.md` contains the evidence trail for each of the 8 experiments (terminated-reason, cell means, posterior, confirm-phase outcome, promoted/not-promoted).

### Anti-cheat directives

- **No promoting-on-cost-alone.** The ship gate requires BOTH quality-hold AND cost-drop. Do not ship a 50% cheaper arm that regresses quality 10%.
- **No cherry-picking the ship gate per experiment.** The gate is uniform across all 8 experiments; if one wants a different gate, it files that as an operator pre-approval request.
- **No running 8 experiments in a single factorial cell.** Each experiment's subject agent differs, so their dimensions are orthogonal (different agent × same dimension name = different dimension in this case because dimension identity includes subject agent). Overlap IS allowed, but via orthogonality, not factorial combination.
- **No shortcutting the confirm phase** for medium-tier experiments that declared winners.

### Verification procedure

```
# Per experiment:
force experiment show E7-N  # shows evidence trail
sqlite3 holocron.db "SELECT termination_reason, winner_treatment_id FROM ExperimentOutcomes WHERE experiment_id = <E7-N>"

# Post-ship monitoring (for promoted ones):
force fleet-progress --metric chancellor_plan_merge_rate --compare holdout --window 30d

# Aggregate:
force fleet-progress --metric fleet_cost_per_convoy --compare holdout --window 30d
```

### Closure report

`docs/closures/DELIVERABLE-7-CLOSURE.md` with per-experiment outcome, promoted-agents list, aggregate cost delta, 30-day retention graph, anti-cheat self-check.

---

## Deliverable 8 — Cross-Repo Dependency Graph

### Mission

Maintain a live graph of exported-symbol → call-site edges across all registered repos. Chancellor decomposition consumes it to auto-include affected-consumer tasks in convoys. Senate consults affected-repo Senators automatically.

### Classification

Feature (proactive blast radius).

### Required reading

1. `/Users/jake.herman/code/force-orchestrator/docs/paired-runs.md` — for the promotion pipeline shape (the graph schema is a fleet asset; its changes land through the same pipeline).
2. `/Users/jake.herman/code/force-orchestrator/docs/next-gen-agents.md` — §"Senate" (Senate consults against this graph).
3. `/Users/jake.herman/code/force-orchestrator/internal/agents/chancellor.go` — decomposition entry point.

### Prerequisites

D7 complete: all eight model-downgrade experiments terminated AND their ship-PRs either merged (winners) or their Outcomes recorded (nulls/inconclusive) AND `docs/closures/DELIVERABLE-7-CLOSURE.md` filed. (Operator-decreed strict sequential ordering; technically only D4 is structurally required — Senate consumes the graph.)

### Merge order within D8

Strictly sequential. Each track consumes the output of its predecessor.

| Order | Track | Branch | Depends on | Must merge before |
|---|---|---|---|---|
| 1 | D8-Graph | `deliverable/8/graph` | — (first) | D8-Chancellor |
| 2 | D8-Chancellor | `deliverable/8/chancellor-integration` | D8-Graph merged (needs `CrossRepoDependencies` table + populated data) | D8-IntegTest |
| 3 | D8-IntegTest | `deliverable/8/integtest` | D8-Chancellor merged (needs `blast_radius_json` populated) | — (final) |

**Rationale.** D8-Graph builds the graph maintenance dog and schema; nothing downstream can query what doesn't exist yet. D8-Chancellor integrates graph consumption into Feature decomposition, populating `Features.blast_radius_json`. D8-IntegTest consumes that field to decide which consumers to run integration tests against. The strict serial order makes each track's scope small and testable in isolation: Graph's tests verify graph correctness; Chancellor's tests verify blast-radius correctness; IntegTest's tests verify integration-check correctness.

### Work tracks

**Track D8-Graph — Graph maintenance dog.**

Schema:
```
CrossRepoSymbols
  id                INTEGER PRIMARY KEY
  repo_id           INTEGER FK
  symbol_path       TEXT          -- 'package.Type.Method' or 'module/api/UserHandler'
  symbol_kind       TEXT          -- 'function' | 'type' | 'http_handler' | 'cli_command' | 'exported_const'
  file_path         TEXT
  line_number       INTEGER
  signature_hash    TEXT          -- stable across purely-renaming changes
  last_scanned_at   TIMESTAMP
  is_public         BOOLEAN
  UNIQUE (repo_id, symbol_path)

CrossRepoDependencies
  id                INTEGER PRIMARY KEY
  consumer_repo_id  INTEGER FK
  consumer_file     TEXT
  consumer_line     INTEGER
  provider_symbol_id INTEGER FK → CrossRepoSymbols.id
  discovered_at     TIMESTAMP
  INDEX (provider_symbol_id)
  INDEX (consumer_repo_id)
```

Dog `dogRepoGraphScan` (daily cadence + triggered on PR merge):
- For each registered repo, walk source via `go/parser` (Go), `tree-sitter` (JS/TS, Python, Rust).
- Extract exported symbols → upsert `CrossRepoSymbols`.
- Extract import / call sites → resolve to `CrossRepoSymbols.id` → upsert `CrossRepoDependencies`.
- Deletion semantics: a consumer-file that no longer exists or no longer references a symbol has its `CrossRepoDependencies` row soft-deleted (new column `deleted_at`). Keep history for debugging.
- Performance budget: full fleet scan in < 30 min on the reference operator machine; incremental update on single-repo PR-merge in < 60s.

**Track D8-Chancellor — Integration into decomposition.**

- When Chancellor's decomposition LLM call produces a plan, post-process: for every task that modifies a `CrossRepoSymbols` row (detected via file-path + symbol-name matching on the diff), query `CrossRepoDependencies` for affected consumers.
- Add `Features.blast_radius_json TEXT` column storing the per-feature computed impact:
  ```json
  {
    "modified_symbols": [...],
    "affected_consumer_repos": [...],
    "auto_included_tasks": [<task_id>, ...]
  }
  ```
- Auto-include downstream tasks in the same convoy. Task payload: `[BLAST_RADIUS_UPDATE] from feature #N modified <symbol>; update your consumer sites at <file>:<line> accordingly.`
- Senate consultation for affected-consumer Senators fires automatically.
- Dashboard: Feature ratification view shows the blast-radius summary.

**Track D8-IntegTest — Synthetic integration testing against downstream consumers.**

Goal: before merging a Feature whose blast-radius flags consumer repos, validate that the proposed change doesn't break those consumers — without waiting for their next CI cycle to find out.

Mechanism:
- New task type `ConsumerIntegrationCheck`, spawned by Diplomat when a Feature's `blast_radius_json.affected_consumer_repos` is non-empty and the convoy is in `DraftPROpen` state.
- For each affected consumer repo:
  - Check out the consumer's `main` branch to a dedicated `.force-worktrees/<consumer>/integ-<feature-id>/` worktree.
  - Apply the proposed change via the appropriate mechanism per language:
    - Go: `go.mod replace` directive pointing at the producer's ask-branch.
    - Node: `package.json` file reference or `npm link` against the producer's local worktree.
    - Python: `pip install -e <producer-path>` in a venv.
    - Rust: `Cargo.toml` `[patch.crates-io]` or path dependency.
  - Run the consumer's test suite (`make test` or the canonical test command from its repo config).
  - Record results in a new `ConsumerIntegrationResults` table: `(feature_id, consumer_repo_id, test_command, exit_code, stdout_tail, stderr_tail, duration_seconds, ran_at)`.
- Aggregate results:
  - All green → convoy proceeds to ship-it readiness as normal; dashboard shows "consumer tests ✓ for N repos".
  - Any red → emit `[CONSUMER BREAKAGE]` operator mail with failures, block ship-it until operator acknowledges. Optionally auto-spawn a `CodeEdit` task on the producer's ask-branch to fix the incompatibility (Captain/Council quality-gated like any other fix).
- Performance bounds: `consumer_integ_timeout_minutes` (default 20) per consumer; exceeding the budget records `timeout` status without blocking the ship gate (operator interprets).
- Cost bound: integration checks run ONCE per Feature in `DraftPROpen`. Re-runs only on subsequent ask-branch force-pushes that materially change the proposed symbols.

Edge cases:
- Consumer repo is in `read_only` or `quarantined` mode (D2 T1-4): skip the integration check; log `[CONSUMER CHECK SKIPPED: repo read_only]`.
- Consumer's test suite itself is broken on main (pre-existing red): record `pre_existing_red` status and proceed without blocking. Don't block a ship on something already broken.
- Consumer repo's language isn't supported by the replace-directive mechanism: skip with `[CONSUMER CHECK SKIPPED: unsupported lang]`; operator mail once per new-language-encountered so we know to build support.

Tests:
- `TestConsumerIntegCheck_GoReplaceDirective`: fixture repos where `repo_b` imports `repo_a`; change to `repo_a`'s exported signature; assert `repo_b`'s tests run against the change; assert red/green aggregation.
- `TestConsumerIntegCheck_PreExistingRed_DoesNotBlock`: consumer's main is already red; change is applied; result is `pre_existing_red`; ship not blocked.
- `TestConsumerIntegCheck_ReadOnlyConsumer_Skips`: repo in read_only mode is skipped cleanly.

Composes with D7: model-tier experiments run on consumer-integration-check agents the same as any other agent; we can A/B-test cheaper models for this classification task.

### Exit criteria

1. `CrossRepoSymbols` + `CrossRepoDependencies` + `ConsumerIntegrationResults` tables in schema parity; `dogRepoGraphScan` registered and running.
2. Integration test: seed two fixture repos; `repo_a` exports `User.ID int`; `repo_b` imports `repo_a.User.ID`. Fleet scans both. A Feature modifying `User.ID int → User.ID string` in `repo_a` produces a convoy that includes an auto-task for `repo_b`.
3. Blast-radius visible on Feature ratification view.
4. `Features.blast_radius_json` populated for every new Feature after D8 cutover.
5. Dashboard: per-repo "who depends on us" view + per-repo "who we depend on" view.
6. Graph freshness: at any time, `MAX(last_scanned_at)` across all repos is within 24h.
7. `ConsumerIntegrationCheck` task type spawned automatically on `DraftPROpen` when blast-radius is non-empty; ship-it blocked on red results; operator mail emitted on failure.
8. `TestConsumerIntegCheck_*` suite green; performance budget honored in tests (20-min per-consumer timeout).

### Anti-cheat directives

- **No string-grep dependency detection.** Use AST, not text search. "Find `User.ID` in consumer repos" via grep catches false positives (comments, strings); AST catches only real references.
- **No skipping non-Go repos.** Tree-sitter bindings for common languages exist; use them. A Go-only graph is incomplete.
- **No blast-radius that isn't actionable.** If a consumer repo's reference is in a test file, the auto-included task should note "test-only consumer — may need import update." Not every downstream is a shipping concern; the plan must distinguish.
- **No treating the graph as permission.** The graph informs decomposition; it doesn't grant autonomous authority to modify consumer repos. Operator still ratifies the Feature that spawned the cascade.

### Verification procedure

```
go test -tags sqlite_fts5 -run TestRepoGraph -race -count=3 ./internal/graph/...
go test -tags sqlite_fts5 -run TestChancellorBlastRadius -race -count=3 ./internal/agents/...

sqlite3 holocron.db "SELECT COUNT(*) FROM CrossRepoSymbols"  # > 0 after first scan
sqlite3 holocron.db "SELECT MAX(last_scanned_at) FROM CrossRepoSymbols"  # within 24h
```

### Closure report

`docs/closures/DELIVERABLE-8-CLOSURE.md` with schema snapshot, integration test results, sample blast-radius JSON, graph freshness SLO evidence, anti-cheat self-check.

---

## Deliverable 9 — Archaeologist + Architecture Health Report

### Mission

Ship two features: (a) proactive debt-detection agent that sweeps for accumulated patterns and emits migration Features; (b) monthly longitudinal report running BoS rules over the full codebase to track invariant-violation trends.

### Classification

Feature (proactive debt + longitudinal visibility).

### Required reading

1. `/Users/jake.herman/code/force-orchestrator/docs/paired-runs.md` — §"Primitive" for how controlled migrations run as experiments.
2. `/Users/jake.herman/code/force-orchestrator/docs/next-gen-agents.md` — §"Bureau of Standards" for the rules the health report consumes.
3. `/Users/jake.herman/code/force-orchestrator/docs/closures/DELIVERABLE-4-CLOSURE.md` + `docs/closures/DELIVERABLE-8-CLOSURE.md`.

### Prerequisites

D8 complete: all three tracks merged to main AND `docs/closures/DELIVERABLE-8-CLOSURE.md` filed. Both D4 (BoS rules — the health report runs them) and D8 (cross-repo graph — Archaeologist uses it for cross-repo pattern sweeps) are structurally required.

### Merge order within D9

Tracks are independent; may develop and merge in either order.

| Order | Track | Branch | Depends on | Parallelizable with |
|---|---|---|---|---|
| 1 (either) | D9-Archaeologist | `deliverable/9/archaeologist` | — | D9-ArchHealth |
| 1 (either) | D9-ArchHealth | `deliverable/9/arch-health` | — | D9-Archaeologist |

**Rationale.** Archaeologist adds a new claim-loop agent + pattern registry; ArchHealth adds a monthly dog that runs existing BoS rules over the full codebase. They touch disjoint files (new package `internal/archaeologist/` vs. new dog `dogArchitectureHealthReport` in existing `internal/agents/dogs.go` + a new `reports/` directory) so parallel development is safe. Merge either first; the other rebases cleanly.

### Work tracks

**Track D9-Archaeologist — Archaeologist agent.**

- New claim-loop agent `SpawnArchaeologist` (Diplomat pattern).
- Claimable task types: `ArchaeologistSweep` (periodic), `ArchaeologistProposeMigration` (triggered by sweep hit).
- Maintained pattern registry at `internal/archaeologist/patterns/`:
  - Each pattern is a Go file implementing `type Pattern interface { ID() string; Scan(*Repo) []Hit; MinHitsForFeature() int }`.
  - Initial patterns: `ARCH-001` deprecated-API (per-language list); `ARCH-002` unused-exports (cross-repo graph detects exports with zero consumers); `ARCH-003` duplicate-abstractions (detected via structural AST hash matching); `ARCH-004` stale-config-files; `ARCH-005` leftover test-only code in production paths.
- Sweep cadence: weekly per repo; results land in `ArchaeologistFindings` table.
- When a pattern's hit count exceeds `MinHitsForFeature()`, Archaeologist's `ArchaeologistProposeMigration` task type fires: produces a candidate Feature (via Librarian hypothesis pipeline to D3's Engineering Corps) with pre-decomposed migration tasks.
- Migration runs as a controlled paired-run experiment: first 5% of call sites are migrated (treatment arm), remaining 95% stays as control. Measure: did migration introduce regressions? If control metric holds AND treatment metric acceptable, ratify operator to migrate the rest. If not, rollback + operator notified.

**Track D9-ArchHealth — Monthly architecture-health report.**

- Dog `dogArchitectureHealthReport` (monthly, runs on the 1st at 00:00 UTC).
- Runs every BoS rule over the full current codebase (not just diffs).
- Aggregates per (rule_id, repo_id, author_type) where author_type ∈ {human, astromech, archaeologist-migration}.
- Produces `reports/architecture-health-YYYY-MM.md`:
  - Per-invariant: current violation count, month-over-month delta, 6-month trend graph.
  - Per-repo: summary card with invariant health score (weighted average, higher is better).
  - Per-author: compliance rate (astromech should be ≥ human; if it's worse, flag for investigation).
- Report committed to repo at `reports/` directory with `AUTO-GENERATED` header.
- Dashboard: architecture-health tab shows the current-month report with trend graphs.

### Exit criteria

1. Archaeologist agent claim loop running. Five initial patterns in `internal/archaeologist/patterns/` with tests.
2. One end-to-end migration trace: Archaeologist detected pattern → proposed Feature → operator ratified → migration experiment ran → confirm phase ran → operator ratified rest-of-migration → migration completed. This is the D9 shakedown.
3. First monthly architecture-health report rendered; trend graph per-invariant per-repo visible; content accurate against manual spot-check.
4. Dashboard health tab live.
5. Integration test: seed 20 sites of a deprecated-API pattern; Archaeologist sweep detects within one cycle; proposes Feature; Feature's blast-radius (via D8) identifies all 20 sites.

### Anti-cheat directives

- **No Archaeologist auto-dispatching migrations.** Archaeologist proposes; operator ratifies. The 5%-then-rest-after-confirm flow is still operator-gated at each step.
- **No pattern that spans every repo equally.** Patterns must be language-aware; a deprecated-Go-API pattern shouldn't scan Rust files.
- **No health-report metric inflation.** The invariant-health score's weighting lives in `docs/arch-health-weights.yaml`; changes to the weights land through the D3 promotion pipeline (weights are a FleetRule-adjacent object). No quietly re-weighting to make the graph look better.
- **No Archaeologist claiming patterns it wasn't registered for.** The pattern registry is the authoritative list; dynamic pattern discovery is disabled in v1.

### Verification procedure

```
go test -tags sqlite_fts5 -run TestArchaeologist -race -count=3 ./internal/archaeologist/...
go test -tags sqlite_fts5 -run TestArchHealthReport ./internal/agents/...

# Shakedown:
force archaeologist sweep force-orchestrator  # runs all 5 patterns
ls reports/architecture-health-*.md  # expect at least one
```

### Closure report

`docs/closures/DELIVERABLE-9-CLOSURE.md` with per-pattern sweep results, shakedown migration trace, first health report pasted, anti-cheat self-check.

---

## Deliverable 10 — Synthetic Handoff Documentation

### Mission

Auto-generated per-PR reviewer narrative + live `ARCHITECTURE.md` update. Deferred until operator confirms demand.

### Classification

Feature (human-review UX).

### Required reading

1. `/Users/jake.herman/code/force-orchestrator/docs/paired-runs.md` — for the experiment harness that validates whether handoff docs actually help.

### Prerequisites

D9 complete: both tracks merged to main AND `docs/closures/DELIVERABLE-9-CLOSURE.md` filed. (Operator-decreed strict sequential ordering; technically only D3 is structurally required — to measure handoff-doc impact via paired-runs.)

### Merge order within D10

Single track; no internal ordering.

| Order | Track | Branch | Depends on |
|---|---|---|---|
| 1 | D10-HandoffDocs | `deliverable/10/handoff-docs` | — |

### Work tracks

Single track.

- New Diplomat task type `PRHandoffSynthesis`: reads the convoy's diff + Council ruling + Captain ruling + ConvoyReview findings + any Senate reviews, produces a reviewer-focused narrative. Posts as a comment on the draft PR.
- Per-repo `handoff_synthesis_enabled` flag on `Repositories`. Default `false` in v1 (opt-in).
- New dog `dogArchitectureDocRender`: on every merge to main of an enabled repo, Librarian synthesizes an updated `ARCHITECTURE.md`. Pre-commit hook rejects hand-edits.
- Paired-run experiment on at least one enabled repo: `{handoff_synthesis_on, handoff_synthesis_off}` × time-to-review-close metric + review-comment-count metric. Ship to more repos only if experiment supports.

### Exit criteria

1. `PRHandoffSynthesis` task type active. Integration test: opens a draft PR on an enabled fixture repo; asserts Diplomat posts a reviewer narrative comment.
2. `ARCHITECTURE.md` auto-update on merge. Pre-commit hook rejects hand-edits.
3. At least one repo enrolled in the handoff-synthesis experiment via D3 mechanism.
4. Operator feedback at T+30 days: either "keep it" (expand enablement) or "drop it" (deprecate).

### Anti-cheat directives

- **No enabling by default.** This is explicitly opt-in until the validating experiment proves out.
- **No long ARCHITECTURE.md that duplicates CLAUDE.md.** The synthesized doc is architecture-level narrative; invariants stay in CLAUDE.md. Tests enforce: `ARCHITECTURE.md` contains no text copied verbatim from CLAUDE.md.
- **No shipping without measuring.** The experiment must produce a verdict; operator ratifies expansion based on evidence.

### Verification procedure

```
go test -tags sqlite_fts5 -run TestPRHandoffSynthesis ./internal/agents/...
go test -tags sqlite_fts5 -run TestArchitectureDocRender ./internal/agents/...

# Experiment progression:
force experiment show D10-handoff-docs
```

### Closure report

`DELIVERABLE-10-CLOSURE.md` with the experiment outcome at T+30 days, operator verdict, and either expansion plan or deprecation plan.

---

## Summary Table

| # | Deliverable | Classification | Items | Prereqs | Build | Closure artifact |
|---|---|---|---|---|---|---|
| **0** | **Interface Layer Foundation** | **Architectural foundation** | **D0-A interfaces + CLAUDE.md, D0-B Librarian migration, D0-C future-service stubs, D0-D Pattern P16** | **—** | **~hours autonomous** | **docs/closures/DELIVERABLE-0-CLOSURE.md** |
| 1 | Pre-Restart Security Closure | Risk closure — blocking | T0-1, T0-2, T0-3 | D0 | ~hours autonomous | docs/closures/DELIVERABLE-1-CLOSURE.md |
| 2 | Operational Risk Hardening | Risk closure — operational | T1-0, T1-1, T1-2, T1-3, T1-3.5, T1-4 | D1 | ~hours autonomous | docs/closures/DELIVERABLE-2-CLOSURE.md |
| 3 | Paired Runs + EC + Holdout + Verification Specs + Trust Layers + CLAUDE.md Refactor | Measurement substrate + bundled additions from concerns #1–#5 | T2-1, T3-1, T3-2 + spec/proposal/trust/UX/sleep additions | D1, D2 | ~1 wk autonomous | docs/closures/DELIVERABLE-3-CLOSURE.md |
| 4 | BoS + ISB + Senate | Risk mitigation — continuous | T2-4 + BOS-011 graduation | D3 | ~hours autonomous | docs/closures/DELIVERABLE-4-CLOSURE.md |
| 5 | Supply Chain Hygiene | Risk closure (ISB rules) | T1-5 | D4 | ~hours autonomous | docs/closures/DELIVERABLE-5-CLOSURE.md |
| 6 | Synthetic Onboarding CLI | Feature — UX | T2-5 | D4 | ~hours autonomous | docs/closures/DELIVERABLE-6-CLOSURE.md |
| 7 | Model-Tier Optimization | Feature — cost | T2-2 | D3 | ~weeks (experiment runtime) | docs/closures/DELIVERABLE-7-CLOSURE.md |
| 8 | Cross-Repo Dependency Graph | Feature — blast radius | T2-3 + D8-IntegTest | D4 | ~hours autonomous | docs/closures/DELIVERABLE-8-CLOSURE.md |
| 9 | Archaeologist + Arch Health | Feature — debt + visibility | T3-3, T3-4 | D4, D8 | ~hours autonomous | docs/closures/DELIVERABLE-9-CLOSURE.md |
| 10 | Synthetic Handoff Docs | Feature — review UX | T3-5 | D3 | ~hours autonomous | DELIVERABLE-10-CLOSURE.md |

## Roadmap-level risks

These are plan-level risks, not product-level risks. Surface here so they're tracked.

1. **D3 slippage cascades into everything after it.** D3 is the single longest deliverable and the one that unblocks the most subsequent work. Any underestimate compounds. Mitigation: `docs/paired-runs.md` §"Rollout Plan" already breaks D3 into six phases with green-tests gates between them; ship phases as they complete rather than saving for a big-bang launch.

2. **D4 invariant extraction from CLAUDE.md is harder than it looks.** BoS requires every CLAUDE.md invariant to become an AST check. Some invariants are semantic ("don't bypass the quality gate"), not syntactic. Mitigation: BoS rules that can't be AST-checked ship as `severity=advise` until an LLM-layer check is built.

3. **D7 Haiku downgrade may declare null more often than expected.** If Haiku isn't good enough at decision-classification, the expected ~25% cost reduction doesn't materialize. Mitigation: evidence decides; null outcomes are the paired-runs framework working as designed. Not a roadmap failure.

4. **Model deprecations outpace holdout refresh cycles.** Haiku-4.5 could deprecate before the 2027 holdout refresh, breaking baseline-2026's model pin. D3's `ModelAvailability` watch + substitution flow is the mitigation; honest caveat logged on every substituted run.

5. **Operator attention is the real bottleneck.** Every ratification needs attention. A roadmap that produces 50 promotion proposals in a week without bandwidth to ratify them is failing the attention budget. Mitigation: `engineering_corps_daily_proposal_cap` (default 3) + dashboard proposal queue with TTL + clear "operator action required" indicators.

## Tracking

Each deliverable produces a closure report under `docs/closures/`. Operator signs off by acknowledging the report. Dashboard "Roadmap" tab ships with D3 and tracks progress from there forward.

---

## Merge-order appendix

Authoritative ordered sequence of every track-level merge to main across the entire roadmap. Each numbered step is a merge event. Brackets around a step contain tracks that may merge in any order relative to each other (parallel-eligible). `⟨GATE⟩` rows are deliverable-closure gates — the next step cannot begin until the gate's closure report is filed.

| # | Step | Branch(es) | Parallel-eligible with | Notes |
|---|---|---|---|---|
| **D0 — Interface Layer Foundation (pre-D1; daemon stays stopped)** |
| 0a | Merge D0-A | `deliverable/0/A` | — | Interface package structure + CLAUDE.md invariant. Lands first; subsequent tracks need the standard documented. |
| 0b | Merge D0-B | `deliverable/0/B` | Step 0c | Librarian interface + in-process implementation + call-site migration. |
| 0c | Merge D0-C | `deliverable/0/C` | Step 0b | Future-service interface stubs (capabilities, experiments, rules, metrics, graph). |
| 0d | Merge D0-D | `deliverable/0/D` | — | Pattern P16 enforcement test (lands last; needs every interface present to validate). |
| 0e | ⟨GATE⟩ | `docs/closures/DELIVERABLE-0-CLOSURE.md` | — | Architectural foundation set; D1 builds against it. |
| **D1 — Pre-Restart Security Closure** |
| 1 | Merge T0-3 | `deliverable/1/T0-3` | — | Fix #8d / Code Red closure (already shipped). |
| 2 | Merge T0-1 | `deliverable/1/T0-1` | Step 3 | Per-agent capability profiles. Builds against the `capabilities.Client` interface defined in D0-C. |
| 3 | Merge T0-2 | `deliverable/1/T0-2` | Step 2 | Inbound secret scrubbing. May develop in parallel with Step 2. |
| 4 | ⟨GATE⟩ | `docs/closures/DELIVERABLE-1-CLOSURE.md` | — | Operator signs off; daemon restart-safe. |
| 5 | Merge T1-0 | `deliverable/2/T1-0` | — | Startup reconciliation sweep. |
| 6 | Merge T1-1 | `deliverable/2/T1-1` | Steps 7, 8 | Per-task cost tracking. |
| 7 | Merge T1-2 | `deliverable/2/T1-2` | Steps 6, 8 | Per-agent context size + byte attribution. |
| 8 | Merge T1-4 | `deliverable/2/T1-4` | Steps 6, 7 | Repo mode column. |
| 9 | Merge T1-3.5 | `deliverable/2/T1-3.5` | — | Divergence detector (depends on T1-1 hooks). |
| 10 | Merge T1-3 | `deliverable/2/T1-3` | — | Bash allowlist wrapper (largest; merges last in D2). |
| 11 | ⟨GATE⟩ | `docs/closures/DELIVERABLE-2-CLOSURE.md` | — | Operational hardening complete. |
| 12 | Merge D3 Phase 1 | `deliverable/3/phase-1` | — | Paired-runs foundations: schema, log-only `treatments.Apply`, FleetRules bootstrap, rule-renderer, metric registry. |
| 13 | Merge D3 Phase 2 | `deliverable/3/phase-2` | — | Holdout + single-treatment experiments; `treatments.Apply` goes live. |
| 14 | Merge D3 Phase 3 | `deliverable/3/phase-3` | — | Engineering Corps claim loop + ratification flow. |
| 15 | Merge D3 Phase 4 | `deliverable/3/phase-4` | — | Factorial + orthogonal-overlap scheduler. |
| 16 | Merge D3 Phase 5 | `deliverable/3/phase-5` | — | Level-3 paired shadow (gh proxy, shadow worktrees, CI suppression). |
| 17 | Merge D3 Phase 6A | `deliverable/3/phase-6a` (15 sub-tracks) | — | Dashboard scaffolding + Pulse + Briefing surfaces; Pattern tests P25–P30 green. See `docs/dashboard-implementation.md` for sub-track briefs. |
| 17b | Merge D3 Phase 6B | `deliverable/3/phase-6b` (13 dashboard sub-tracks + concern bundles) | — | Reflection + Drill + verification spec consumption + concerns #1, #4, #6–#10 acceptance + end-to-end shakedown; Pattern tests P31, P32, replay-no-mutation green. |
| 18 | ⟨GATE⟩ | `docs/closures/DELIVERABLE-3-CLOSURE.md` | — | Measurement substrate live. |
| 19 | Merge D4-BoS | `deliverable/4/bos` | — | Bureau of Standards. |
| 20 | Merge D4-ISB | `deliverable/4/isb` | — | Imperial Security Bureau (shares SecurityFindings with BoS). |
| 21 | Merge D4-Senate | `deliverable/4/senate` | — | Senate + first Senator onboarded + promotion-via-experiment shakedown. |
| 22 | ⟨GATE⟩ | `docs/closures/DELIVERABLE-4-CLOSURE.md` | — | Review-layer agents live. |
| 23 | Merge D5-SupplyRules | `deliverable/5/supply-rules` | — | Five SUPPLY-* ISB rules (hallucinated package, typosquat, stale, license, CVE). |
| 24 | ⟨GATE⟩ | `docs/closures/DELIVERABLE-5-CLOSURE.md` | — | Supply chain hygiene. |
| 25 | Merge D6-OnboardCLI | `deliverable/6/onboard-cli` | — | `force onboard <repo>` CLI. |
| 26 | ⟨GATE⟩ | `docs/closures/DELIVERABLE-6-CLOSURE.md` | — | Synthetic onboarding. |
| 27 | Merge E7-1 ship-PR | `deliverable/7/ship-boot-haiku` | Steps 28–34 (experiments run concurrently; ship-PRs merge one at a time) | Boot → Haiku (if winner). |
| 28 | Merge E7-2 ship-PR | `deliverable/7/ship-rerank-haiku` | Steps 27, 29–34 | memory_rerank → Haiku (if winner). |
| 29 | Merge E7-3 ship-PR | `deliverable/7/ship-prtriage-haiku` | Steps 27–28, 30–34 | PR-review-triage → Haiku (if winner). |
| 30 | Merge E7-4 ship-PR | `deliverable/7/ship-librarian-haiku` | Steps 27–29, 31–34 | Librarian → Haiku (if winner). |
| 31 | Merge E7-5 ship-PR | `deliverable/7/ship-diplomat-haiku` | Steps 27–30, 32–34 | Diplomat summarization → Haiku (if winner). |
| 32 | Merge E7-6 ship-PR | `deliverable/7/ship-medic-haiku` | Steps 27–31, 33–34 | Medic classification → Haiku (if winner). |
| 33 | Merge E7-7 ship-PR | `deliverable/7/ship-commander-haiku` | Steps 27–32, 34 | Commander → Haiku (if winner). |
| 34 | Merge E7-8 ship-PR | `deliverable/7/ship-chancellor-haiku` | Steps 27–33 | Chancellor → Haiku (if winner). |
| 35 | ⟨GATE⟩ | `docs/closures/DELIVERABLE-7-CLOSURE.md` | — | Includes 30-day post-ship monitoring clearance for every shipped PR. |
| 36 | Merge D8-Graph | `deliverable/8/graph` | — | CrossRepoSymbols + dogRepoGraphScan. |
| 37 | Merge D8-Chancellor | `deliverable/8/chancellor-integration` | — | blast_radius_json population in Chancellor decomposition. |
| 38 | Merge D8-IntegTest | `deliverable/8/integtest` | — | ConsumerIntegrationCheck task type + consumer test suite runs. |
| 39 | ⟨GATE⟩ | `docs/closures/DELIVERABLE-8-CLOSURE.md` | — | Cross-repo graph + integration checks live. |
| 40 | Merge D9-Archaeologist | `deliverable/9/archaeologist` | Step 41 | Archaeologist agent + pattern registry. Develops in parallel with Step 41. |
| 41 | Merge D9-ArchHealth | `deliverable/9/arch-health` | Step 40 | Monthly architecture-health report dog. |
| 42 | ⟨GATE⟩ | `docs/closures/DELIVERABLE-9-CLOSURE.md` | — | Proactive debt detection + longitudinal visibility. |
| 43 | Merge D10-HandoffDocs | `deliverable/10/handoff-docs` | — | PRHandoffSynthesis Diplomat task type + ARCHITECTURE.md auto-render. |
| 44 | ⟨GATE⟩ | `DELIVERABLE-10-CLOSURE.md` | — | Handoff documentation experiment outcome recorded. |

**Reading the "Parallel-eligible with" column.** A value of `—` means the step has no parallel peer; it merges alone. A list of step numbers means those tracks may develop concurrently in separate worktrees and merge in any order relative to each other — but they still merge one at a time (no simultaneous merges to main).

**The gate rows are non-skippable.** An agent proposing to begin step N+1 when step N's gate row is not satisfied (closure report not filed, or some merge in the prior deliverable still pending) is rejected. The operator enforces this by refusing to approve the starting PR.

**What changes if an experiment in D7 declares null/inconclusive.** The ship-PR for that experiment doesn't exist (there's nothing to ship). The step number stays in the sequence as a "no-op" — the closure report records the null outcome and moves on. Subsequent gates are unaffected. The operator should NOT hold D7 open waiting for a null experiment to "change its mind"; null is a valid terminal outcome.
