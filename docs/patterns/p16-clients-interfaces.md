---
audience: agent
scope: clients/<svc>/ exports `Client` as an interface; agents never construct concrete clients via composite literal.
owner: D13
last_reviewed: 2026-05-05
title: Pattern P16 — Cross-agent service interfaces
type: pattern-doc
pattern: P16
---

# Pattern P16 — Cross-agent service interfaces

## Rationale

Cross-agent dependencies route through Go interfaces under
`internal/clients/<service>/`. Direct function-call dependencies between
agents are forbidden (Domain 0). Each service package shape is fixed:

- `client.go` defines the exported `Client` interface — never a struct.
- `inprocess.go` implements it via `NewInProcess(...)`.
- Additional implementations (gRPC, shared, mock) live in sibling files.
- Agents receive `Client` instances by constructor injection; they
  never import a concrete struct type.

Pattern P16 graduates to a BoS commit-time rule when D4 ships.

## What it checks

Two phases (AST-based, not grep, so doc-comments are fine):

1. Every `internal/clients/<svc>/` package must export a type named
   exactly `Client` and that type must be an `*ast.InterfaceType`.
2. Every production `internal/agents/*.go` (non-test) is parsed; any
   composite literal whose type is `<clients-pkg-alias>.<TypeName>`
   where `TypeName` ends in `Client` (e.g. `librarian.MockClient`,
   `librarian.InProcessClient`, `capabilities.GRPCClient`) is an
   offence. Construction must go through the exported factories
   (`NewInProcess` / `NewGRPC` / `NewShared` / `NewMock`).

`listClientServices` walks `internal/clients/` and treats each
subdirectory with at least one non-test `.go` file as a service.

## How it fails

```
clients/foo: exported `Client` is *ast.StructType at internal/clients/foo/client.go:18 — Pattern P16 requires it to be an interface
```

Or for the agent-side check:

```
Pattern P16 (D0): N agent file(s) construct a concrete client struct from internal/clients/<svc>/. Use the package's NewInProcess / NewGRPC / NewMock factory function instead — agents depend on the interface, never on the implementation type:
  internal/agents/captain.go:42 — &librarian.MockClient{...}  (or librarian.MockClient{...})
```

## How to fix

For service authors: declare `Client` as an interface in `client.go`.
For agent authors: replace the composite literal with a factory call:

```go
// Bad
client := &librarian.InProcessClient{DB: db}

// Good
client := librarian.NewInProcess(db)
```

## Test reference

- File: `internal/audittools/audit_pattern_p16_clients_interfaces_test.go`
- Core assertion: `TestPattern_P16_ClientsInterfaces` (lines 58–189)
- Helpers: `listClientServices` (lines 197–222),
  `findExportedClientType` (lines 229–263).
- Constants: `clientsPkgPrefix`, `p16AgentDir`.

## See also

- [P13 — Capability profiles](p13-capability-profiles.md)
- [P33 — Agent memory via Librarian Client](p33-agent-memory-via-librarian-client.md)
- CLAUDE.md "Cross-agent service interfaces" section.
