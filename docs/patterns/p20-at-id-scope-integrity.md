---
audience: agent
scope: Every AT lookup scopes by compound (convoy_id, at_id) — bare at_id predicates are forbidden.
owner: D13
last_reviewed: 2026-05-05
title: Pattern P20 — AT-id scope integrity
type: pattern-doc
pattern: P20
---

# Pattern P20 — AT-id scope integrity

## Rationale

AT (Acceptance Test) IDs are namespaced PER CONVOY: convoy 47's AT-005
is a different row than convoy 48's AT-005. A query with a bare
`WHERE at_id = ?` predicate (no co-occurring `convoy_id` constraint) is
a cross-convoy collision waiting to happen. Roadmap reference: D3
§ "Cross-convoy AT-id collisions (concern #8)" / exit criterion 14c.

The test scaffolds in slice α; once slice β / γ ships the AT resolver,
every new query is held to the compound-key contract. P20 graduates to
a BoS commit-time rule when D4 ships.

## What it checks

Two sub-tests:

1. `TestPattern_P20_ATIdScopeIntegrity` — AST walk over `cmd/` and
   `internal/` for every string literal. If the literal matches
   `p20BareAtIDRe` (an `at_id =` or `at_id IN (...)` predicate) AND
   does NOT match `p20ConvoyScopeRe` (a co-occurring `convoy_id =`
   predicate in the same string), the file:line is an offender.
   Skips: `internal/store/schema.go` (column declaration),
   `internal/audittools/` (self-reference).
2. `TestPattern_P20_AllowlistReasonsTruthful` — every entry in
   `p20Allowlist` must have a rationale ≥20 chars. Allowlist is
   empty at landing.

## How it fails

```
Pattern P20 (D3 14c): N production query site(s) reference at_id without a co-occurring convoy_id constraint. Cross-convoy AT collisions waiting to happen — scope by (convoy_id, at_id):
  internal/agents/foo.go:42 — SELECT * FROM Bounties WHERE at_id = ?
...
Fix: every AT lookup MUST scope by `WHERE convoy_id = ? AND at_id = ?`. Bare `WHERE at_id = ?` is forbidden — AT-005 in convoy 47 is a DIFFERENT row than AT-005 in convoy 48.
```

## How to fix

Always scope by the compound key:

```sql
SELECT * FROM Bounties
WHERE convoy_id = ? AND at_id = ?
```

If a legitimate fleet-wide AT lookup arrives (e.g. v2 fleet-wide
namespace), add the file path to `p20Allowlist` with a reviewer-visible
rationale.

## Test reference

- File: `internal/audittools/audit_pattern_p20_at_id_scope_integrity_test.go`
- Core assertions:
  - `TestPattern_P20_ATIdScopeIntegrity` (lines 82–193)
  - `TestPattern_P20_AllowlistReasonsTruthful` (lines 198–214)
- Regexes: `p20BareAtIDRe`, `p20ConvoyScopeRe` (lines 69, 74).

## See also

- [P21 — AT removal is operator-only](p21-at-removal-operator-only.md)
- BOS-007 / convoy-scoped queries via `convoy_id` not LIKE.
