---
audience: operator
scope: D11 notification substrate — categories, tiers, presets, per-convoy override, DND, budget routing.
owner: D11
last_reviewed: 2026-05-05
subsystem: notification-routing
type: subsystem-doc
---

# Notification routing

D11's notification substrate is the single ingress for operator-facing signals — operator mail, dashboard banners, modals, and external Slack pings via `notify-after`. Every emit site routes through `notify.Dispatch` so the operator can shape the firehose with categories, tiers, presets, per-convoy overrides, and DND windows without touching code.

## Overview

Pre-D11, every signal site (escalations, spend, CI-stall, convoy review, dog failures, …) decided independently whether and how to bother the operator. The result was either too quiet (real-incident-buried-in-noise) or too loud (banner saturation). D11 unifies the surface:

1. Every emit site calls `notify.Dispatch(ctx, evt)` with a typed `notify.Event`.
2. The dispatcher resolves the operator's effective rule for `(category, tier, convoy)` against `config/notifications.yaml`.
3. The rule decides which channels fire: operator mail, dashboard banner, modal, Slack ping, or silent budget-only.
4. Pattern P27 (`audit_pattern_p27_notification_budget_routing_test.go`) walks every emit site to ensure no signal escapes the substrate.

## Components

- **`internal/notify/`** — `Dispatch`, `Event`, `Category`, `Tier`, channel implementations, budget tracking.
- **`config/notifications.yaml`** — operator-authored ruleset. Reloaded on `force notifications reload` or daemon SIGHUP.
- **`internal/notify/presets.yaml`** — bundled presets (Quiet / Normal / Loud / Incident) the operator can `extend:` from.
- **`internal/store/notifications.go`** — `NotificationBudgets`, `NotificationDigest` tables backing the budget + digest path.
- **Pattern P27** — every operator-mail / banner / modal emit site routes through `RespectNotificationBudget` first.

## Invariants

1. **Single ingress.** No emit site bypasses `notify.Dispatch`. Pattern P27 fails CI on direct `mail.SendOperator` / `banner.Show` calls outside the substrate.
2. **Budget-routed.** Every signal consumes a per-category budget; over-budget signals roll into the digest queue. The budget never silently drops a signal — it either dispatches now, rolls into the next digest, or escalates if the digest is also full.
3. **Per-convoy override is additive.** A convoy-specific override never makes a signal *louder* than the global rule unless the operator explicitly opted in (Watch chip on a convoy). Pinned-convoy noise is opt-in.
4. **DND is a first-class window.** During DND, only `incident` tier fires immediately; everything else digests. There is no "DND but for spend caps" carve-out — operator chooses the tier when authoring the cap, not the substrate.
5. **Presets are starter kits, not policy.** A preset is an `extend:` base; operator overrides take precedence. Editing a preset in code does NOT auto-apply to operators who pinned it (the YAML carries a content-hash anchor).

## Configuration

`config/notifications.yaml` shape:

```yaml
extend: presets/normal           # optional: inherit preset
dnd:
  start: "22:00"
  end:   "07:30"
  exempt_tiers: [incident]
defaults:
  tier: info
  channels: [mail]
categories:
  spend:
    tier: alert                  # info | alert | incident
    channels: [mail, banner]
    budget:
      per_hour: 6
      digest_after: 3
  escalation:
    tier: incident
    channels: [mail, banner, modal, slack]
  convoy_review:
    tier: info
    channels: [mail]
overrides:
  convoy:
    "watching:7":                # convoy ID 7 pinned via Watch chip
      tier: alert
      channels: [mail, banner]
```

Presets bundled in `internal/notify/presets.yaml`:
- **Quiet** — incidents only; everything else digests.
- **Normal** — alerts to banner+mail; info to mail; incidents everywhere.
- **Loud** — alerts modal too; info banner+mail.
- **Incident** — short-lived high-noise mode (`force notifications incident-mode --duration 1h`).

SystemConfig knobs:
- `notifications_enabled` — global kill switch.
- `notifications_digest_cron` — when the rolled-up digest fires (default `0 9 * * *`).
- `notifications_yaml_path` — alternate YAML location.

## Operator surface

CLI:

```bash
force notifications budgets        # current per-category counter state
force notifications digest         # show queued digest items
force notifications reload         # re-read config/notifications.yaml
force notifications preset use Quiet
force notifications incident-mode --duration 1h
```

Dashboard:
- **Notifications tab** (D11 P3) — visualise current per-category budgets, queued digest, last 24h volume.
- **Watch chip** on a convoy card — opt-in louder signal for a specific convoy.
- **Per-tab refresh + theme** — D11 P3-B (in `config/dashboard.yaml`, separate substrate but UI-adjacent).

Mail:
- Subjects are stable so filters work: `[TASK SPEND ANOMALY]`, `[TASK SPEND ESCALATE]`, `[INBOUND REDACT]`, `[CIRCULAR COMMITS]`, `[CHANCELLOR FAIL-CLOSED]`, `[CONTEXT OVERFLOW]`, `[CONVOY REVIEW PASSED]`, `[RECONCILE]`, …

## See also

- [`dashboard.md`](dashboard.md) — Notifications tab + Watch chip surface.
- `dogs.md` (planned) — many dogs are notification emitters.
- `mail-system.md` (planned) — fleet mail substrate (notification's sibling for agent-to-agent traffic).
- `security.md` (planned) — `[INBOUND REDACT]` / `[CHANCELLOR FAIL-CLOSED]` event taxonomy.
- [`../closures/DELIVERABLE-11-CLOSURE.md`](../closures/DELIVERABLE-11-CLOSURE.md) — D11 closure report.
