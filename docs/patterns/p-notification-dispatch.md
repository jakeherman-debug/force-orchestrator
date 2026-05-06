---
audience: agent
scope: Every operator-facing notification routes through internal/notify.Dispatch.
owner: D13
last_reviewed: 2026-05-05
title: Pattern P-NotificationDispatch — D11 dispatch-routing invariant
type: pattern-doc
pattern: P-NotificationDispatch
---

# Pattern P-NotificationDispatch — D11 dispatch-routing invariant

## Rationale

D11 Phase 1 moves the notification dispatch surface into
`internal/notify` and makes `notify.Dispatch(ctx, db, category,
convoyID, label, body)` the single entry point. Direct calls to the
legacy notify-after seams (`notifyAfterFn`, `realNotifyAfter`,
`stageTransitionNotifyFn`, `notify.SlackNotify`) outside the
dispatcher itself bypass the per-convoy override + DND + preset chain
that D11 builds on top.

Future deliverables that add a new operator notification MUST:

1. Add the category to `config/notifications.yaml` with
   `tier`, `default`, and `description`.
2. Call `notify.Dispatch(ctx, db, category, convoyID, label, body)`.

Either step missing fails this audit.

## What it checks

Three sub-tests:

1. `TestPattern_P_NotificationDispatch_NoUngatedNotifyCalls` —
   AST-walks `internal/` and `cmd/` (non-test). For every CallExpr
   whose function name (Selector OR bare Ident) is in
   `notificationDispatchBannedFunctions` (`notifyAfterFn`,
   `realNotifyAfter`, `stageTransitionNotifyFn`, `SlackNotify`),
   the file:line is a violation unless the file is in
   `notificationDispatchBypassAllowlist`. The allowlist holds:
   - `internal/notify/dispatcher.go` (IS the dispatcher).
   - `internal/notify/slack.go` (IS the Slack notifier).
   - `internal/agents/dogs_supply_token_recheck.go` (compat shim).
   - `internal/agents/dogs_convoy_stage_watch.go` (test seam).
2. `TestPattern_P_NotificationDispatch_DispatcherSurfacePresent` —
   pins `dispatcher.go` to its package + symbol set
   (`Dispatch`, `ConfigKeyActivePreset`, `ConfigKeyDNDUntil`,
   `ConfigKeyCategoryPrefix`, `dndBypassCategories`).
3. `TestPattern_P_NotificationDispatch_PositiveControl` — confirms
   the five migrated call sites (convoy_review's awaiting-supply,
   the three supply-token-recheck pings, the stage-transition ping)
   contain the literal `notify.Dispatch(ctx, db, "<category>", …)`
   substring.

A sibling test
(`audit_pattern_p_notification_dispatch_synthetic_test.go`) exercises
the AST matcher against a hand-crafted synthetic file (1 banned ident
call, 1 ident-only reference, 1 comment mention, 1 banned selector
call) to pin the matcher's behaviour, plus an allowlist-shape ratchet
(rationales must be ≥20 chars).

## How it fails

```
Pattern P-NotificationDispatch: N ungated notification call site(s):
  internal/agents/foo.go:42 — calls notifyAfterFn; route through notify.Dispatch instead

Fix: replace the call with notify.Dispatch(ctx, db, category, convoyID, label, body) after registering the category in config/notifications.yaml. For seam-owners (internal/notify, the migration-window compat shims), add the file to notificationDispatchBypassAllowlist with a reason naming why the bypass is permanent.
```

## How to fix

Replace the direct call with the dispatcher entry point:

```go
err := notify.Dispatch(ctx, db, "my_new_category", convoyID,
    "Short label",
    "Body text the operator sees.")
```

And add the category to `config/notifications.yaml`:

```yaml
categories:
  my_new_category:
    tier: high          # or low / medium
    default: mail       # or slack / mail+slack
    description: …
```

## Test reference

- Files:
  - `internal/audittools/audit_pattern_p_notification_dispatch_test.go`
  - `internal/audittools/audit_pattern_p_notification_dispatch_synthetic_test.go`
- Core assertions:
  - `TestPattern_P_NotificationDispatch_NoUngatedNotifyCalls`
    (lines 107–204)
  - `TestPattern_P_NotificationDispatch_DispatcherSurfacePresent`
    (lines 211–227)
  - `TestPattern_P_NotificationDispatch_PositiveControl`
    (lines 234–252)
  - `TestPattern_P_NotificationDispatch_SyntheticCounterExample`
    (synthetic file, lines 27–115)
  - `TestPattern_P_NotificationDispatch_AllowlistShape`
    (synthetic file, lines 122–131)

## See also

- [P27 — Notification budget routing](p27-notification-budget-routing.md)
- `internal/notify/dispatcher.go`
- `config/notifications.yaml`
