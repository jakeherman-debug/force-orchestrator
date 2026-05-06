---
audience: agent
scope: Every rows.Scan inside a for-rows.Next loop must observe its error.
owner: D13
last_reviewed: 2026-05-05
title: Pattern P1 — rows.Scan error checks
type: pattern-doc
pattern: P1
---

# Pattern P1 — `rows.Scan` error checks

## Rationale

A `rows.Scan` call inside a `for rows.Next() { ... }` loop that ignores its
error silently corrupts the loop's outputs: a half-populated struct gets
appended to the result slice, and the caller has no signal that one row
slipped through. This was the failure mode swept by Fix #8d
(AUDIT-090, AUDIT-091, AUDIT-094, AUDIT-095, AUDIT-100). Pattern P1
prevents the regression class from coming back.

## What it checks

For every production (non-`_test.go`) `*.go` file the test:

1. Locates each `for <name>.Next() { ... }` loop opening line.
2. Looks at the next ~25 lines for the first `<name>.Scan(` call.
3. Asserts the Scan line is one of the accepted error-checked shapes:
   `if err := <name>.Scan(...); err != nil`,
   `if sErr := ...`, `if rErr := ...`, `if scanErr := ...`, `if e := ...`,
   the boolean-gate form `if <name>.Scan(...) != nil`,
   or an `err := ...; if err != nil` pair on consecutive lines.
4. Allows a deferred-check shape only when an explicit
   `deferral-comment(Fix #8b)` annotation precedes the Scan call.

Vendor / build-worktree / `.git` / `node_modules` / `testdata` directories
are skipped. Test files are out of scope.

## How it fails

Failure message:

```
Pattern P1 (Fix #8d): N rows.Scan call(s) inside a for-.Next() loop are not error-checked:
  internal/foo/bar.go:42 — rows.Scan(...) in for rows.Next() { ... } has no `if err := ...; err != nil` guard
...
Fix: capture the error and log (or continue) when it fires. See dogs.go's dogGitHygiene for the canonical pattern.
```

Typical violating snippet:

```go
for rows.Next() {
    var id int
    rows.Scan(&id)        // bare call, no error captured
    out = append(out, id)
}
```

## How to fix

Capture the error and surface it. Minimal patch:

```go
for rows.Next() {
    var id int
    if err := rows.Scan(&id); err != nil {
        log.Printf("scan: %v", err)
        continue
    }
    out = append(out, id)
}
```

`dogs.go`'s `dogGitHygiene` is the canonical reference shape.

## Test reference

- File: `internal/audittools/audit_pattern_p1_rows_scan_test.go`
- Core assertion: `TestPattern_P1_RowsScanErrorsChecked` (lines 29–162)
- Helpers: `forNextRe` regex; `readFile` (line 164).

## See also

- [P1.1 — `rows.Err()` after iteration](p1_1-rows-err.md)
- [Fix #8d narrative in FIX-LOG.md](../../FIX-LOG.md)
- `internal/agents/dogs.go::dogGitHygiene` — canonical scan-loop shape.
