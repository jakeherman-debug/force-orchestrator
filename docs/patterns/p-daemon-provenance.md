---
audience: operator
scope: Build-time provenance vars (GitSHA / BuildTime / GitBranch) are wired through ldflags and surfaced via provenance.Set in main.init.
owner: infrastructure
last_reviewed: 2026-05-07
title: Pattern P_DaemonProvenance — D12 build provenance wiring
type: pattern-doc
pattern: P_DaemonProvenance
---

# Pattern P_DaemonProvenance — D12 build provenance wiring

## Rationale

The trust-file gate (`P_DaemonTrustFile`) and `force daemon status`
both surface "which binary is this?" to the operator. That answer
is built from three package-level vars in `cmd/force/main.go` —
`GitSHA`, `BuildTime`, `GitBranch` — populated at link time via
`-ldflags "-X main.GitSHA=… -X main.BuildTime=… -X main.GitBranch=…"`.
A binary built outside the Makefile keeps the default `"unknown"`
markers, which `force version` and `force daemon status` surface as
a hint that the binary's history is unverified.

Pattern P_DaemonProvenance catches three orthogonal regressions:

1. Someone deletes one of the three package-level vars (the linker
   would happily ignore `-X` flags pointing at a missing symbol).
2. Someone refactors the Makefile and drops a `-X` flag (the binary
   builds; the var keeps the `"unknown"` default).
3. Someone removes `provenance.Set(...)` from `main.init()` (non-main
   code can no longer read the values via `provenance.Get()` because
   nothing primes the singleton).

Closure narrative:
[`docs/closures/DELIVERABLE-12-CLOSURE.md`](../closures/DELIVERABLE-12-CLOSURE.md)
and [`docs/subsystems/daemon-lifecycle.md`](../subsystems/daemon-lifecycle.md)
§ "Provenance".

## What it checks

The single test `TestPattern_P_DaemonProvenance` runs three checks:

1. **`cmd/force/main.go` declares the three vars.** AST walks every
   top-level `GenDecl` whose token is `VAR` and confirms the
   `ValueSpec` names contain `GitSHA`, `BuildTime`, and `GitBranch`.
   Each missing name emits a distinct `t.Errorf`.
2. **The Makefile's build target carries the three `-X` flags.**
   Reads `Makefile` as text and asserts each of `-X main.GitSHA`,
   `-X main.BuildTime`, `-X main.GitBranch` is present, plus the
   build target references `$(LDFLAGS)` (so adding a fourth flag is
   a one-line edit, not a multi-target sweep).
3. **`provenance.Set(...)` is called from `main.init()`.** Walks
   every top-level `FuncDecl` named `init` with no receiver and
   inspects its body for a `CallExpr` of shape `provenance.Set(...)`.
   Without this hook, non-main packages cannot read GitSHA /
   BuildTime / GitBranch via `provenance.Get()`.

## How it fails

```
Pattern P_DaemonProvenance: cmd/force/main.go missing package-level var GitSHA — `force version` won't surface build provenance
Pattern P_DaemonProvenance: Makefile missing "-X main.BuildTime" in -ldflags — `make build` will produce a binary with default 'unknown' provenance
Pattern P_DaemonProvenance: Makefile defines LDFLAGS but the build target doesn't reference $(LDFLAGS)
Pattern P_DaemonProvenance: cmd/force/main.go does not call provenance.Set(...) from init() — non-main packages can't read GitSHA/BuildTime/GitBranch
```

Typical violating snippet (`init()` removed):

```go
// cmd/force/main.go
package main

var (
    GitSHA    = "unknown"
    BuildTime = "unknown"
    GitBranch = "unknown"
)

func main() { ... }
// MISSING: func init() { provenance.Set(GitSHA, BuildTime, GitBranch) }
```

## How to fix

Restore the three package-level vars and wire `provenance.Set` from
`init()`:

```go
package main

import "force-orchestrator/internal/daemon/provenance"

var (
    GitSHA    = "unknown"
    BuildTime = "unknown"
    GitBranch = "unknown"
)

func init() {
    provenance.Set(GitSHA, BuildTime, GitBranch)
}
```

And confirm the Makefile carries the matching ldflags:

```make
LDFLAGS := -ldflags "-X main.GitSHA=$(GIT_SHA) -X main.BuildTime=$(BUILD_TIME) -X main.GitBranch=$(GIT_BRANCH)"

build:
    go build $(LDFLAGS) -o force .
```

## Test reference

- File: `internal/audittools/audit_pattern_p_daemon_provenance_test.go`
- Core assertion: `TestPattern_P_DaemonProvenance`
- Helpers: standard `go/parser` + `go/ast` walk for vars and
  `init()`; `os.ReadFile` against `Makefile` for the linker flag
  grep; `moduleRoot(t)` to resolve the repository root.

## See also

- [P_DaemonTrustFile](p-daemon-trust-file.md) — the consumer of
  provenance values during a binary swap.
- [P_DaemonUpdateHistory](p-daemon-update-history.md) — records
  `provenance.Get().GitSHA` snapshots into the audit row.
- `internal/daemon/provenance/provenance.go` — the runtime accessor.
- [`docs/subsystems/daemon-lifecycle.md`](../subsystems/daemon-lifecycle.md)
  § "Provenance".
