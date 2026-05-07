---
audience: operator
scope: D11 mail/notification delivery channel — Slack webhook + mail rows + per-category Tier 1/2/3 mail-vs-slack-vs-off mapping. Sibling to notification-routing.md (which covers higher-level routing/preset/DND semantics).
owner: feature-team
last_reviewed: 2026-05-07
---

# Mail system

The mail system is the **delivery-channel layer** that sits beneath D11's notification dispatcher. Two side effects are visible to the operator: a row inserted into `Fleet_Mail` (the dashboard inbox) and a Slack webhook ping via the `notify-after` shell-out. Every operator-facing notification routes through `notify.Dispatch`, which resolves a routing decision and then fires zero, one, or both side effects based on the resolved per-category `Setting`.

This page complements [`notification-routing.md`](notification-routing.md): that page covers the higher-level routing chain (presets, per-convoy overrides, DND windows). This one focuses on the *channel* layer — what `mail`, `slack`, `mail+slack`, and `off` actually mean at the side-effect boundary.

## Overview

D11 declares 19 operator-facing notification categories in `config/notifications.yaml`, organised into three tiers. (D12 added `system_event` as a 20th — Tier-2.) Each tier has a default routing:

- **Tier 1** — operator must act to unblock the fleet → default `mail+slack`.
- **Tier 2** — informational, useful but skippable → default `mail`.
- **Tier 3** — debug-trace, opt-in → default `off`.

The four legal `Setting` values across every layer of the resolution chain:

- `off` — no mail, no Slack.
- `mail` — `Fleet_Mail` row only.
- `slack` — `notify-after` Slack ping only.
- `mail+slack` — both side effects.

Three named presets ship: `default` (per-tier defaults), `focus` (quiet — most categories `off`, only the most urgent fire), and `verbose` (everything to `mail+slack`). Two categories — `spend_cap_e_stop` and `consumer_breakage` — bypass DND windows and dispatch even when the operator has set DND active.

## Components

### Notify package — `internal/notify/`

- `dispatcher.go` — `Dispatch(ctx, db, category, convoyID, label, body)` is the single entry point. Resolves the routing chain (per-convoy override → DND → active preset → per-category override → YAML default), then fires side effects per the resolved `Setting`. Owns SystemConfig keys: `notification_active_preset`, `notification_dnd_until`, `notification_dnd_reason`, `notification_dnd_set_by`, `notification_category_<name>`.
- `config.go` — YAML model + parser. `Setting` (`off | mail | slack | mail+slack`), `CategorySpec`, `Preset`, `Config`. Setting values are strings (not int constants) so they round-trip cleanly through SystemConfig + YAML + JSON.
- `slack.go` — `SlackNotifier` indirection seam. The default `realSlackNotify` shells out to the `notify-after` script (the same surface `internal/agents/dogs_supply_token_recheck.go` previously called directly; D11 P1 migrated all callers to `notify.Dispatch`). Tests install a counting stub via `SetSlackNotifierForTest`.
- `seed.go` — `SeedRegistryFromYAML(db, cfg)` upserts every YAML category into `NotificationCategoryRegistry` at daemon startup. Idempotent — re-runs update tier / yaml_default / description / yaml_version but preserve `registered_at` and never auto-delete categories present in DB but absent from YAML.

### Mail rows — `internal/store/fleet_mail.go`

`SendMail(db, from, to, subject, body, taskID, msgType) int64` is the canonical insert helper. It scrubs secrets via `RedactSecrets` before writing — the dashboard renders `Fleet_Mail.body` directly, so redaction at the store boundary closes the AUDIT-055/056 exfil path (Fix #10).

Subject convention for D11 dispatcher rows: `[D11/<category>] <label>`. The dashboard `/api/notifications/catalog` endpoint counts `Fleet_Mail` rows whose `subject LIKE '[D11/<cat>]%' AND created_at >= now() - 7d` to surface a per-category fire rate without a dedicated audit table.

### Schema — `schema/schema.sql`

- `Fleet_Mail` (`id, from_agent, to_agent, subject, body, task_id, message_type, read_at, consumed_at, created_at`). `to_agent` may be an agent name, role, or `'all'`. `task_id=0` means "standing order"; `>0` ties the mail to a specific task. `consumed_at` is set when an agent has read its inbox; `read_at` is set when the operator opens the dashboard row. Hot-path indexes: `idx_mail_to_consumed`, `idx_mail_task_id`, `idx_mail_created_at`.
- `NotificationCategoryRegistry` (`id, category UNIQUE, tier, yaml_default, description, registered_at, yaml_version`) — seeded from YAML at daemon startup.
- `ConvoyNotificationOverrides` (`convoy_id PRIMARY KEY, mode, custom_json, set_at, set_by, reason, convoy_closed_at`). `mode ∈ {verbose, quiet, custom_json}`. `convoy_closed_at` is populated by terminal-transition hooks; the cleanup dog deletes rows 7d after closure.

### Override cleanup dog — `internal/agents/dogs_notification_override_cleanup.go`

`dogNotificationOverrideCleanup(db, logger)` runs daily (24h cadence) and deletes `ConvoyNotificationOverrides` rows whose `convoy_closed_at` is older than 7 days. The retention preserves enough history for post-incident debugging without letting the table accumulate indefinitely. Cleanup is silent — no operator-mail, no Slack ping. Coupled change: terminal convoy transitions (`Shipped`, `Abandoned`, `Failed`) call `MarkConvoyOverrideClosed` to stamp `convoy_closed_at` (callsites in `pilot_draft_watch.go` and `convoy.go`).

### Dashboard handlers — `internal/dashboard/`

- `handlers_d11_notifications.go` — `/api/notifications/{catalog, state, preset, dnd, dnd/clear, category/<name>, category/<name>/clear, preset/save}`. Read/write surface for the SPA's notifications tab. Tier-1 per-category toggles require `confirm: true` in the body (modal-gated). State mutations write through `internal/notify` SystemConfig keys; the file does NOT call `notify.Dispatch` directly.
- `handlers_convoy_watch.go` — `/api/convoys/<id>/watch`, `/api/convoys/<id>/watch/clear`. The 👁 Watch popover on a convoy card. Modes: `verbose`, `quiet`, `custom_json`. Routes through `store.UpsertConvoyNotificationOverride`; reason + operator land in the audit trail.

## Invariants

- **Single ingress.** Pattern **P-NotificationDispatch** (`docs/patterns/p-notification-dispatch.md`) AST-walks `internal/` and `cmd/` and rejects any callsite that invokes `notifyAfterFn`, `realNotifyAfter`, `stageTransitionNotifyFn`, or `notify.SlackNotify` outside the dispatcher itself plus the four allowlist files (the dispatcher, the Slack notifier, the supply-token-recheck compat shim, the stage-transition test seam).
- **Budget-routed.** Pattern **P27** (`docs/patterns/p27-notification-budget-routing.md`) ensures every forward-going `store.SendMail` callsite routes through `store.RespectNotificationBudget` or one of the `emitOperatorMail{Governed,High,Medium}` wrappers. The backlog of pre-P27 sites is tracked in `p27Backlog`; the forward set must gate.
- **DND bypass is code-declared, not YAML.** `dndBypassCategories = {spend_cap_e_stop, consumer_breakage}` lives in `internal/notify/dispatcher.go`. The set cannot be silently disabled by editing `config/notifications.yaml`. Operators who want to silence one of these must explicitly turn off the category via per-category override. `IsDNDBypass(category)` is exported so the dashboard can render the "fires during DND" label.
- **DND window cap.** `MaxDNDDays = 14`. Server-side validators reject DND values past `now + MaxDNDDays` so a fat-fingered "DND for 14 MONTHS" doesn't silently suppress the fleet for a year.
- **Setting is one of four exact strings.** `off | mail | slack | mail+slack`. Parser rejects unknown values; the resolver fails closed if any layer returns an empty string.
- **Secrets scrubbed at store boundary.** `SendMail` calls `RedactSecrets` on subject and body before insert. No callsite bypasses this — the dashboard renders the column verbatim.
- **Mail subject convention.** `[D11/<category>] <label>` for every dispatcher-emitted row. The catalog endpoint's 7-day fire-count read depends on this prefix.

## Configuration

- **YAML.** `config/notifications.yaml` declares 19 D11 categories (plus `system_event` from D12 — 20 total) and 3 presets. Adding a category here registers it on the next daemon start via `SeedRegistryFromYAML`. Each row carries `tier`, `default`, and `description`. Rules can be a literal map (`{"*": "off", "spend_cap_e_stop": "mail+slack", ...}`) or the sentinel string `tier_defaults` (used by the `default` preset, mapping Tier-1 → `mail+slack`, Tier-2 → `mail`, Tier-3 → `off`).
- **SystemConfig keys.**
  - `notification_active_preset` — preset name; default `"default"`.
  - `notification_dnd_until` — ISO8601 UTC; empty = no DND.
  - `notification_dnd_reason`, `notification_dnd_set_by` — audit-only.
  - `notification_category_<name>` — per-category override; absent means "use preset/yaml-default."
- **Slack channel.** The `notify-after` script reads its webhook URL from operator-managed local config; the `realSlackNotify` shell-out lives in `internal/notify/slack.go`. Tests stub via `SetSlackNotifierForTest`.

## Operator surface

### Dashboard

- **Notifications tab** — surfaces the per-category catalog, current resolved setting, 7d fire count, and per-category override controls. Tier-1 toggles are modal-gated.
- **Watch chip on a convoy card** — opt-in louder signal for a specific convoy. Three modes: `verbose` (all categories `mail+slack`), `quiet` (all categories `off` for that convoy), `custom_json` (per-category map).
- **DND control** — set a DND window with a reason; the `MaxDNDDays = 14` cap is server-enforced.

### CLI

- `force notifications budgets` — current per-category counter state.
- `force notifications digest` — show queued digest items.
- `force notifications reload` — re-read `config/notifications.yaml`.

### Mail / Slack — what the operator actually sees

Per-category Tier 1/2/3 default mapping (excerpt; full list in `config/notifications.yaml`):

| Category | Tier | Default | Notes |
|---|---|---|---|
| `supply_token_expired` | 1 | mail+slack | umt artifacts token died; supply-chain deferred |
| `spend_cap_e_stop` | 1 | mail+slack | Auto-stop fired on $/h cap breach (DND-bypass) |
| `gate_timeout_failed` | 1 | mail+slack | D5.5 stage gate hung past timeout |
| `operator_confirm_required` | 1 | mail+slack | D5.5 operator-confirm gate awaits action |
| `consumer_breakage` | 1 | mail+slack | D8 consumer-repo test breakage (DND-bypass) |
| `senate_dissent_block` | 1 | mail+slack | D4 Senate severity=block dissent |
| `promotion_proposal_pending` | 1 | mail+slack | D7/D9 PromotionProposal awaits ratification |
| `task_escalated` | 1 | mail+slack | Captain or Council escalated |
| `convoy_review_needs_work` | 1 | mail+slack | ConvoyReview spawned fix tasks |
| `stage_transition` | 2 | mail | D5.5 convoy stage flipped state |
| `convoy_review_clean` | 2 | mail | ConvoyReview pass found no issues |
| `pr_handoff_posted` | 2 | mail | D10 reviewer narrative posted |
| `daily_digest` | 2 | mail | Daily fleet activity rollup |
| `awaiting_supply_recheck` | 2 | mail | Token re-auth pending |
| `system_event` | 2 | mail | D12 daemon lifecycle events |
| `supply_token_recovered` | 3 | off | Trace ping when token recovered |
| `supply_per_branch_summary` | 3 | off | Per-branch supply summary |

## See also

- [`subsystems/notification-routing.md`](notification-routing.md) — higher-level routing chain (presets, DND, per-convoy overrides, budget routing).
- [`patterns/p-notification-dispatch.md`](../patterns/p-notification-dispatch.md) — single-ingress invariant.
- [`patterns/p27-notification-budget-routing.md`](../patterns/p27-notification-budget-routing.md) — budget routing for forward-going SendMail callsites.
- [`closures/DELIVERABLE-11-CLOSURE.md`](../closures/DELIVERABLE-11-CLOSURE.md) — D11 closure trail.
