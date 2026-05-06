---
audience: agent
scope: SetTrustDial with SetBy=TrustDialOperator may only run from operator-routed handlers/CLI.
owner: D13
last_reviewed: 2026-05-05
title: Pattern P-TrustDialsOperatorWriteDiscipline — operator trust-dial write discipline
type: pattern-doc
pattern: P-TrustDialsOperatorWriteDiscipline
---

# Pattern P-TrustDialsOperatorWriteDiscipline — operator trust-dial write discipline

## Rationale

`OperatorTrustDials` rows track operator-set confidence levels in
each agent's auto-execute paths. A non-operator code path inserting
a row with `set_by='operator'` would synthesize operator approval —
defeating the audit trail. Originates in D3 P6A.6.

Approved sites for `SetBy=TrustDialOperator`:

- `internal/dashboard/handlers_trust_dials.go` — operator API.
- `cmd/force/trust_cmds.go` — operator CLI.
- `internal/store/trust_dials.go` — the helper itself; constants live here.

## What it checks

`TestPattern_TrustDialsOperatorWriteDiscipline` walks
`internal/agents`, `internal/dashboard`, `internal/store`, `cmd/`
for `*.go` (non-test). For each file:

1. Skips files in `trustDialsOperatorWriteAllowlist`.
2. If the file does not contain `SetTrustDial(`, skip.
3. If the file mentions any of `TrustDialOperator`,
   `set_by="operator"`, or `SetBy: "operator"`, record it as an
   offender.

## How it fails

```
Pattern P-trust-dials-discipline: SetBy=TrustDialOperator from non-operator-routed file:
  internal/agents/foo.go

These writes must route through the operator API or CLI, not synthesised by an agent.
```

## How to fix

Move the trust-dial write into the operator API or CLI. If the agent
needs to surface a recommended trust-dial change to the operator,
emit a notification or write a row that flows through the
ratification queue — never invoke `SetTrustDial` with
`TrustDialOperator` from a system code path.

## Test reference

- File: `internal/audittools/audit_pattern_trust_dials_operator_write_test.go`
- Core assertion: `TestPattern_TrustDialsOperatorWriteDiscipline`
  (lines 27–77)
- Configuration: `trustDialsOperatorWriteAllowlist` (lines 20–25).

## See also

- [P25 — CLI parity](p25-cli-parity.md)
- [P-AnnotationsOperatorOnly](p-annotations-operator-only.md)
- `internal/store/trust_dials.go`
