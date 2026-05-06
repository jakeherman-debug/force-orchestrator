---
audience: agent
scope: Post-hoc convoy staging_mode mutations require operator-confirm — no production caller of SetConvoyStaging without an allowlist entry.
owner: D13
last_reviewed: 2026-05-05
title: Pattern P-StagingPromotionConfirm — D5.5 post-hoc promotion gate
type: pattern-doc
pattern: P-StagingPromotionConfirm
---

# Pattern P-StagingPromotionConfirm — D5.5 post-hoc promotion gate

## Rationale

The convoy's `staging_mode` column is mutated by exactly one
production helper: `store.SetConvoyStaging` (in
`internal/store/convoy.go`). The convoy's mode at creation time is
set inside the constructor (`store.CreateConvoy` /
`store.CreateStagedConvoy`) and never flows through this helper —
the constructors INSERT the row with the final value already in place.

That makes `SetConvoyStaging` a strictly post-hoc mutator. Any future
production caller would, by definition, be promoting a single-stage
convoy to staged (or vice versa) AFTER the convoy has started running.
The roadmap forbids that without operator confirmation:

> "No Commander single-stage → multi-stage promotion post-hoc without
> explicit operator confirmation. Otherwise the Commander could re-plan
> to hide intent drift. Audit Pattern P-StagingPromotionConfirm
> enforces."

The cleanest enforcement is "no production caller exists." Future
callers MUST justify via `stagingPromotionConfirmAllowlist` — the
reviewer confirms the named operator-confirm predicate is real.
Originates in D5.5 P5 fix-iter1.

## What it checks

`TestPattern_PStagingPromotionConfirm_NoUngatedSetConvoyStaging`:

1. AST-walks `internal/` and `cmd/` (non-test).
2. For every CallExpr, matches both:
   - qualified `store.SetConvoyStaging(...)`, AND
   - bare `SetConvoyStaging(...)` (defensive — caller would only
     fire from inside `internal/store/...`).
3. If the file is on `stagingPromotionConfirmAllowlist`, log the
   reason and pass; otherwise the file:line is an offender.

The allowlist is empty at landing.

## How it fails

```
Pattern P-StagingPromotionConfirm: N ungated SetConvoyStaging call site(s):
  internal/agents/commander.go:42 — calls store.SetConvoyStaging (post-hoc staging_mode mutator) without operator-confirm gate. Add to stagingPromotionConfirmAllowlist with a reason naming the operator-confirm predicate site.

Fix: precede the SetConvoyStaging call with an operator-confirm predicate (e.g. dashboard endpoint requiring AUDIT-NNN), then add the file to stagingPromotionConfirmAllowlist with a `reason:` line naming the predicate site.
```

## How to fix

Implement an OperatorConfirm predicate (e.g. dashboard endpoint
`POST /api/convoys/<id>/staging-mode` requiring an `AUDIT-NNN`
reference like the stage-bypass path), then:

```go
// stagingPromotionConfirmAllowlist
"internal/dashboard/handlers_staging_mode.go": "operator-confirm endpoint POST /api/convoys/{id}/staging-mode requires AUDIT-NNN",
```

## Test reference

- File: `internal/audittools/audit_pattern_p_staging_promotion_confirm_test.go`
- Core assertion:
  `TestPattern_PStagingPromotionConfirm_NoUngatedSetConvoyStaging`
  (lines 97–204)
- Constant: `stagingPromotionConfirmTargetFunc` (line 85).

## See also

- [P-StageGate](p-stage-gate.md) — sibling D5.5 gate on dispatch-time SQL.
- [P21 — AT removal is operator-only](p21-at-removal-operator-only.md)
- `internal/store/convoy.go::SetConvoyStaging`.
