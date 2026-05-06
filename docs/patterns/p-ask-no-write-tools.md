---
audience: agent
scope: ask_handler.go must not call any store mutator; Ask is read-only.
owner: D13
last_reviewed: 2026-05-05
title: Pattern P-AskNoWriteTools — Ask handler read-only invariant
type: pattern-doc
pattern: P-AskNoWriteTools
---

# Pattern P-AskNoWriteTools — Ask handler read-only invariant

## Rationale

The Ask `/` shortcut (D3 P6B.10) is a read-only natural-language
query surface. Allowing it to reach into a store mutator would let
the operator type "delete the bounty" and have it actually fire —
defeating the operator-action discipline. Originates in D3 P6B.10.

Allowed: any read helper (`SearchDrill`, `GetConfig`, `QueryRow`).
Forbidden: writers like `UpdateBountyStatus`, `FailBounty`,
`SendMail`, `InsertEscalation`, `UpsertFleetRule`,
`SetOperatorTrustDial`, `InsertConvoyReviewCycle`, `InsertAnnotation`,
`UpdateAnnotation`, `DeleteAnnotation`.

## What it checks

`TestPattern_AskNoWriteTools` reads `internal/agents/ask_handler.go`
and asserts:

1. None of the names in `pAskForbidden` appear as a function call
   (substring match `<name>(`).
2. The file contains no `UPDATE <table> SET` SQL fragment.
3. The file contains no `DELETE FROM <table>` SQL fragment.
4. The file contains no `INSERT INTO <table>` SQL fragment.

## How it fails

```
Pattern P-AskNoWriteTools: ask_handler.go must not call UpdateBountyStatus — Ask is read-only
```

Or:

```
Pattern P-AskNoWriteTools: ask_handler.go contains UPDATE
```

## How to fix

If Ask needs a new piece of data, add a read-only query to
`ask_handler.go` (or extract one into `internal/store/ask_*.go`).
Operator actions belong on a different surface (a dedicated
dashboard handler with CLI parity per [P25](p25-cli-parity.md)).

## Test reference

- File: `internal/audittools/audit_pattern_ask_no_write_tools_test.go`
- Core assertion: `TestPattern_AskNoWriteTools` (lines 32–56)
- Configuration: `pAskForbidden` (lines 20–30).

## See also

- [P25 — CLI parity](p25-cli-parity.md)
- [P-Replay](p-replay-no-mutation.md)
- `internal/agents/ask_handler.go`
