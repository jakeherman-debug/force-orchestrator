---
audience: agent
scope: Every for-rows.Next loop must observe rows.Err() after the iteration ends.
owner: D13
last_reviewed: 2026-05-05
title: Pattern P1.1 — rows.Err() after iteration
type: pattern-doc
pattern: P1.1
---

# Pattern P1.1 — `rows.Err()` after iteration

## Rationale

`rows.Scan` errors are only one half of the SQL iteration contract; the
other half is `rows.Err()`. The driver surfaces broken-statement errors,
disconnects, and partial-result conditions through `rows.Err()` AFTER the
loop exits — silently dropping that error swallows the precise category
of failure SQL most often produces. Fix #8e closed the gap that Pattern
P1 alone didn't catch.

Anti-cheat: this test carries NO allowlist. Adding one would re-open the
original gap; every production iteration loop is in scope.

## What it checks

For every production `*.go` file:

1. Locates each `for <name>.Next() {` loop at any indent.
2. Traces forward to the matching `}` at the same indent.
3. Within the next 10 lines after that close brace, asserts the file
   references `<name>.Err()`.
4. Rejects the silent-discard form `_ = <name>.Err()`.
5. Reports loops where the close brace cannot be located.

Test files, `vendor/`, build-worktrees, and `testdata/` are skipped.

## How it fails

Failure message:

```
Pattern P1.1 (Fix #8e): N for-rows.Next() loop(s) in production lack a meaningful rows.Err() check:
  internal/foo/bar.go:42 — for rows.Next() { ... } — no rows.Err() reference in the 10 lines after the loop close
...
Fix: after the closing brace, add `if rErr := rows.Err(); rErr != nil { log.Printf(...) }` or equivalent. See pr_comments.go:ComputePRReviewRollup for the canonical pattern.
```

## How to fix

Add an observation after the loop body:

```go
for rows.Next() {
    // ... scan + collect ...
}
if rErr := rows.Err(); rErr != nil {
    log.Printf("iterate: %v", rErr)
    return nil, rErr
}
```

`pr_comments.go::ComputePRReviewRollup` is the canonical pattern.

## Test reference

- File: `internal/audittools/audit_pattern_p1_1_rows_err_test.go`
- Core assertion: `TestPattern_P1_1_RowsErrCheckedAfterIteration` (lines 39–152)
- Helper regex: `forNextRe` (line 42) matches `for <name>.Next() {`.

## See also

- [P1 — `rows.Scan` error checks](p1-rows-scan.md)
- [Fix #8e narrative in FIX-LOG.md](../../FIX-LOG.md)
