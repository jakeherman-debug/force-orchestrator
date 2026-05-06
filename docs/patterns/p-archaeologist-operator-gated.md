---
audience: agent
scope: Archaeologist's only proposal-emission seam is librarian.Client.EmitCandidate.
owner: D13
last_reviewed: 2026-05-05
title: Pattern P-ArchaeologistOperatorGated — D9 Archaeologist proposal discipline
type: pattern-doc
pattern: P-ArchaeologistOperatorGated
---

# Pattern P-ArchaeologistOperatorGated — D9 Archaeologist proposal discipline

## Rationale

The Archaeologist proposes; the operator ratifies. The ONLY proposal-
emission seam reachable from `internal/archaeologist/*` or
`internal/agents/archaeologist*.go` is `librarian.Client.EmitCandidate`.
Direct INSERTs into PromotionProposals, calls to
`store.RatifyPromotionProposal`, and direct invocations of
EngineeringCorps experiment-author methods are forbidden.

Originates in D9 Track A.

## What it checks

Two sub-tests:

1. `TestPattern_PArchaeologistOperatorGated_OnlyEmitCandidate` —
   AST-walks `internal/archaeologist/` (recursive) and the
   archaeologist agent files in `internal/agents/`
   (`pArchaeologistExtraFiles`). For every CallExpr with a
   selector:
   - if the selector name is `EmitCandidate`, mark the positive
     control as seen,
   - if the selector name is in `pArchaeologistForbiddenSelectors`
     (`RatifyPromotionProposal`, `InsertPromotionProposal`,
     `AuthorExperiment`, `DispatchMigration`, `AutoApplyCandidate`),
     record the offence.
   At least ONE `EmitCandidate` call site MUST exist (positive
   control — protects against a future refactor silently deleting the
   seam).
2. `TestPattern_PArchaeologistOperatorGated_NoRawPromotionProposalInsert` —
   for every basic-string literal in the archaeologist tree,
   rejects any case-insensitive `INSERT INTO PROMOTIONPROPOSALS`.

Test files (`*_test.go`) are excluded — tests legitimately mock the
seam.

## How it fails

```
Pattern P-ArchaeologistOperatorGated: N forbidden proposal-dispatch selector(s) reached from the archaeologist tree. The ONLY permitted proposal-emission seam is librarian.Client.EmitCandidate (anti-cheat #1: archaeologist proposes; operator ratifies):
  internal/archaeologist/foo.go:42 — .RatifyPromotionProposal(...)
```

Or for the raw-INSERT prong:

```
Pattern P-ArchaeologistOperatorGated: N raw `INSERT INTO PromotionProposals` literal(s) inside the archaeologist tree. Use librarian.Client.EmitCandidate instead (the librarian package owns the INSERT):
  internal/archaeologist/foo.go:42
```

## How to fix

Replace any direct dispatch with a candidate emission:

```go
err := lib.EmitCandidate(ctx, librarian.Candidate{
    Source: "archaeologist",
    Topic:  "migration:legacy-rails-import",
    // ... canonical input fields ...
})
```

The librarian package owns the underlying `INSERT INTO
PromotionProposals`; the archaeologist must not bypass it.

## Test reference

- File: `internal/audittools/audit_pattern_p_archaeologist_operator_gated_test.go`
- Core assertions:
  - `TestPattern_PArchaeologistOperatorGated_OnlyEmitCandidate`
    (lines 69–152)
  - `TestPattern_PArchaeologistOperatorGated_NoRawPromotionProposalInsert`
    (lines 159–217)
- Configuration: `pArchaeologistForbiddenSelectors`,
  `pArchaeologistFileRoots`, `pArchaeologistExtraFiles`.

## See also

- [P34 — Senate no self-promote](p34-senate-no-self-promote.md) — same shape for the Senate.
- [P33 — Agent memory via Librarian Client](p33-agent-memory-via-librarian-client.md)
- `internal/clients/librarian/::Client.EmitCandidate`.
