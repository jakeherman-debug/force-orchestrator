---
audience: agent
scope: Every mutating dashboard handler has a matching `force <verb>` CLI command.
owner: D13
last_reviewed: 2026-05-05
title: Pattern P25 — CLI parity
type: pattern-doc
pattern: P25
---

# Pattern P25 — CLI parity

## Rationale

Every operator-action dashboard handler (non-GET route) must have a
matching `force <verb>` command in `cmd/force/`. The dashboard is the
visual surface; the CLI is the scriptable surface; both must invoke
the same reusable core. Originates in D3 P6A.15.

D3 polish-pass iteration 2 (B1) moved the implementation from regex
matching to AST walking. The regex form was brittle to formatting
(multi-line `HandleFunc`, comment-prefixed strings, string-concat in
the route literal) and silently missed routes whose source line did
not match. The AST walk is robust to all three.

## What it checks

Three sub-tests:

1. `TestPattern_P25_CLIParity` — AST-walks
   `internal/dashboard/dashboard.go` for every `*.HandleFunc(<lit>, …)`
   call where the literal starts with `/api/`. For each route, it:
   - skips read-only-by-convention prefixes (`/api/status`,
     `/api/stats`, `/api/agents`, `/api/repos`, `/api/mail`, etc.),
   - skips entries on `p25Allowlist` (each entry must have a
     non-empty rationale),
   - maps `/api/<noun>` to verb `<noun>` (with carve-outs for
     `briefing/reject` → `briefing-reject`, `trust-dials` → `trust`,
     etc.),
   - asserts the verb appears either as a `case "<verb>":` arm in
     `cmd/force/main.go` or in `p25KnownVerbs`.
2. `TestPattern_P25_AST_BasedImplementation` — reads this test file's
   own source and rejects regex-based scanning (the iteration-1
   form). The `regexp` import must NOT appear; `go/ast` and
   `go/parser` MUST.

## How it fails

```
Pattern P25 violation: mutating dashboard handlers without CLI parity:
  /api/foo (expected verb: foo)
Add a corresponding `force <verb>` command in cmd/force/, or add a one-line rationale to p25Allowlist for non-operator-action handlers.
```

Or for the AST-implementation regression:

```
Pattern P25: regex-based scanning reintroduced — remove regexp import
```

## How to fix

For each unmapped route either:

- Author a `force <verb>` command in `cmd/force/<verb>_cmds.go` that
  shares its core with the dashboard handler (typically a function in
  `internal/agents/`), wire it into the `case "<verb>":` arm in
  `cmd/force/main.go`, OR
- Add an entry to `p25Allowlist` with a one-line rationale naming
  why the route has no operator-action semantic (read-only,
  alternative existing verb, deferred to a later phase).

## Test reference

- File: `internal/audittools/audit_pattern_p25_cli_parity_test.go`
- Core assertions:
  - `TestPattern_P25_CLIParity` (lines 148–227)
  - `TestPattern_P25_AST_BasedImplementation` (lines 348–366)
- Helpers: `extractDashboardRoutesAST` (lines 241–284),
  `loadCLIVerbs` (lines 286–321).

## See also

- [P26 — Keyboard shortcut consistency](p26-keyboard-shortcuts.md)
- [P-AskNoWriteTools](p-ask-no-write-tools.md) — the Ask handler is read-only.
- [P-Replay](p-replay-no-mutation.md) — the replay handler is read-only on live state.
