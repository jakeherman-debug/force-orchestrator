---
audience: agent
scope: Forward-going SendMail call sites must route through RespectNotificationBudget or an emitOperatorMail wrapper.
owner: D13
last_reviewed: 2026-05-05
title: Pattern P27 — Notification budget routing
type: pattern-doc
pattern: P27
---

# Pattern P27 — Notification budget routing

## Rationale

Every operator-facing notification emit must route through
`store.RespectNotificationBudget` so the per-category budgets, digest
flushing, and DND windows all apply uniformly. The helper landed in
D3 P6A.4. Mass-migrating every existing `store.SendMail` site in one
commit was infeasible, so the audit operates in three modes:

1. The helper exists, is exported, and is callable.
2. Files in the forward-going set route every `SendMail` through the
   budget (or an `emitOperatorMail*` wrapper that gates internally).
3. `p27Backlog` records the remaining pre-P27 sites — each entry has
   a one-line rationale.

D3 polish-pass iteration 2 (B2) burned the original 32-entry backlog
down to 4 endpoint / internal-bus entries. New code lands in the
forward set by default — a new `SendMail` call from a non-backlog
file MUST gate.

## What it checks

Three sub-tests:

1. `TestPattern_P27_NotificationBudgetRouting_HelperExists` —
   `internal/store/notification_budgets.go` declares
   `RespectNotificationBudget`, `SetNotificationBudget`,
   `ListNotificationBudgets`, `FlushPendingDigests`.
2. `TestPattern_P27_NotificationBudgetRouting` — walks
   `internal/agents`, `internal/dashboard`, `internal/store` for
   `*.go` (non-test). Skips files in `p27Backlog`. For every file
   that contains `store.SendMail(`, the file must also contain
   `RespectNotificationBudget` OR an `emitOperatorMailGoverned(` /
   `emitOperatorMailHigh(` / `emitOperatorMailMedium(` call (the
   wrappers in `internal/agents/notification_budget_wrapper.go` gate
   at the chokepoint).
3. `TestPattern_P27_BacklogShrinks` — every backlog file must
   either still exist or be deleted; entries must carry a non-empty
   rationale.

## How it fails

```
Pattern P27 violation: forward-going files emit without RespectNotificationBudget gating:
  internal/agents/foo.go
Either route the emit through store.RespectNotificationBudget(...) or add a backlog entry in audit_pattern_p27_notification_budget_routing_test.go with a one-line truthful rationale.
```

## How to fix

Pick the highest-leverage shape:

```go
// Direct gate
allowed, err := store.RespectNotificationBudget(ctx, db, category, label)
if err != nil { ... }
if !allowed { return nil }
return store.SendMail(db, label, body)

// Wrapper (preferred)
return emitOperatorMailHigh(ctx, db, category, label, body)
```

The `emitOperatorMail*` wrappers live in
`internal/agents/notification_budget_wrapper.go` and call
`RespectNotificationBudget` for you.

## Test reference

- File: `internal/audittools/audit_pattern_p27_notification_budget_routing_test.go`
- Core assertions:
  - `TestPattern_P27_NotificationBudgetRouting_HelperExists` (lines 56–73)
  - `TestPattern_P27_NotificationBudgetRouting` (lines 75–144)
  - `TestPattern_P27_BacklogShrinks` (lines 149–162)

## See also

- [P-NotificationDispatch](p-notification-dispatch.md) — D11 layer above this.
- `internal/agents/notification_budget_wrapper.go`
- `internal/store/notification_budgets.go`
