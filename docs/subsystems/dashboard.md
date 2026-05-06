---
audience: operator
scope: Fleet Command Center — the SPA dashboard, its tabs, modal flows, JSON API, and security invariants.
owner: D5.5 + D11
last_reviewed: 2026-05-05
subsystem: dashboard
type: subsystem-doc
---

# Dashboard

The web dashboard at `http://localhost:8080` is the **intended primary interface** to the fleet. CLI commands exist for scripting and power-user workflows, but day-to-day operation — submitting work, monitoring progress, handling escalations, reviewing mail — is designed around the browser UI. Pattern P25 (`internal/audittools/audit_pattern_p25_*`) enforces CLI parity: every mutating handler has a `force <verb>` equivalent.

## Overview

The dashboard is a single-page app served by an embedded Go HTTP server in `internal/dashboard/`. It auto-refreshes on a live polling loop — no manual reloads needed. The URL query string stays in sync with your current tab, filter, and sort state so views are bookmarkable. SSE streams power the live log tailers and event feed.

Start it alongside the daemon:

```bash
force daemon          # start the agent fleet
force dashboard       # open Fleet Command Center at http://localhost:8080
force dashboard --port 9090   # custom port
```

## Components

- **`internal/dashboard/`** — handlers, templates, static assets, SSE streams, auth/security middleware.
- **`internal/dashboard/security.go`** — `securityMiddleware`, `loopbackBindAddr`, `setSecurityHeaders`, origin / referer allow-list, body cap.
- **`config/dashboard.yaml`** — D11 personalization layer: tab visibility/order/refresh, theme/density, saved filters.
- **`cmd/force/cmd_dashboard.go`** — CLI entry point.
- **Pulse / Briefing / Reflection surfaces** — D3 Phase 6A/6B information architecture (replaces the legacy three-tab layout). Detail in [`dashboard-implementation.md`](dashboard-implementation.md).

## Invariants

The full dashboard security contract is auto-rendered to [`../dashboard-conventions.md`](../dashboard-conventions.md) from FleetRules; the load-bearing rules:

1. **Bind 127.0.0.1 only** — `loopbackBindAddr(port)` returns `127.0.0.1:PORT`. Never `:PORT`. Remote access goes through SSH tunnel (`ssh -L 8080:localhost:8080`).
2. **Same-origin allow-list on every mutation** — `securityMiddleware` wraps the mux globally; every POST/PUT/PATCH/DELETE is gated by `originAllowed` (with `refererAllowed` fallback).
3. **256 KB body cap** — every mutation wraps `r.Body` in `http.MaxBytesReader`. Translate `*http.MaxBytesError` → 413 via `writeBodyReadError`.
4. **No wildcard CORS** — Pattern P8 greps for `Access-Control-Allow-Origin: *` regressions.
5. **CSP + security headers on every response** — `setSecurityHeaders` writes CSP, X-Content-Type-Options, X-Frame-Options, Referrer-Policy. `index.html` carries a CSP `<meta http-equiv>` belt-and-suspenders.
6. **Attacker-writable strings render as text** — mail bodies, task payloads, PR-review comment bodies use `.textContent`, never `.innerHTML`. `marked.parse` is banned.
7. **High-escalation banner** — `#high-esc-banner` becomes visible at `status.high_escalations >= 3` (AUDIT-064).
8. **CLI parity** — every mutating dashboard handler has a `force <verb>` equivalent (Pattern P25).
9. **Keyboard-shortcut consistency** — every documented shortcut in `?` overlay binds to a real action; every binding is documented (Pattern P26).
10. **Notification budget routing** — operator-mail / banner / modal emit sites route through `RespectNotificationBudget` first (Pattern P27).

Pattern tests `P8_*`, `P25_*`, `P26_*`, `P27_*` in `internal/audittools/` are the regression set.

## Configuration

Personalization lives in `config/dashboard.yaml` (D11 P3-B/C):

- **Tabs** — `tabs.visible` / `tabs.order` / `tabs.refresh_seconds` per-tab.
- **Theme + density** — light/dark/system, compact/comfortable.
- **Saved filters** — per-tab named filters with shareable URLs.
- **Notifications** — per-category enable/disable, tier overrides, presets.
- **Per-convoy override** — pinned-convoy refresh and mute behaviour.

SystemConfig knobs that surface in the UI:
- `hourly_spend_cap_usd`, `hourly_spend_estop_usd`, `per_task_spend_alert_usd`, `per_task_spend_escalate_usd` — spend cap UI on Status header.
- `pr_flow_enabled` (per-repo) — surfaces "PR flow" badge on the Repos tab.

## Operator surface

**Header** (always visible): Daemon badge, E-STOP badge, E-Stop / Resume buttons, `+ Queue Task` button.

**Stats Bar** (refreshed every 5s): Running, Pending, Review, Done, Failed, Escalations, Convoys, Unread Mail, Total Spend.

**Tabs**:

| Tab | What it surfaces |
|---|---|
| Tasks (default) | Sortable, filterable, paginated table (50/page). Click any row → slide-in detail panel with meta, broader goal, payload, error log, attempt history, fleet memories, task mail, action buttons. |
| Escalations | Cards per escalation. Filter Open / Closed / All. Actions: Acknowledge, Close, Close & Requeue. |
| Convoys | Progress cards with bars and counts. Click drills into the Tasks tab filtered to the convoy. Activate / cancel actions. |
| Agents | Registered agent worktrees with current task. |
| Mail | Inbox of last 200 fleet mail messages. Modal renders Markdown body. Mark-all-read. |
| Knowledge | Fleet Memory browser. Search + filter (success/failure × repo). Detail modal; per-row delete. |
| Experiments | Paired-runs lifecycle + global holdout strip with rolling 24h/7d/30d snapshots. |
| Logs | Live-tailing log viewer (SSE) — `fleet.log` or `holonet.jsonl` events. |
| Pulse / Briefing / Reflection | D3 Phase 6 information architecture. See [`dashboard-implementation.md`](dashboard-implementation.md) for the full task-brief artifact. |

**`+ Queue Task` modal**: Auto / Feature / Investigate / Audit. Repo + priority optional. Submission idempotent within 60s.

**JSON API** — every operator surface has a `/api/...` endpoint. Full table:

| Method | Endpoint | Purpose |
|---|---|---|
| GET | `/api/status` | Fleet health (daemon PID, e-stop, counts, escalations, spend) |
| GET | `/api/stats` | Pending / active / completed today / active convoys / active agents |
| GET | `/api/tasks` | Task list — `status`, `convoy_id`, `sort_by`, `sort_dir`, `limit`, `offset` |
| GET | `/api/tasks/{id}` | Full task detail (history, memories, mail) |
| POST | `/api/tasks/{id}/{retry,reset,cancel,approve,reject}` | Mutations |
| GET | `/api/escalations` | List; `?status=Open\|Closed` |
| POST | `/api/escalations/{id}/{ack,close,requeue}` | Mutations |
| GET | `/api/convoys` | List with progress |
| POST | `/api/convoys/{id}/{approve,cancel}` | Activate / cancel |
| GET | `/api/agents` | Agent registry with current task |
| GET / POST | `/api/mail*` | Inbox, mark read, mark-all-read |
| GET / DELETE | `/api/memories*` | Fleet memory browse + prune |
| POST | `/api/add` | Queue a task |
| GET | `/api/events`, `/api/fleet-log` | SSE streams |
| POST | `/api/control/{estop,resume}` | E-stop toggle |
| GET | `/healthz` | Health check |

CLI parity: every POST has a `force <verb>` equivalent (Pattern P25).

## See also

- [`dashboard-conventions.md`](../dashboard-conventions.md) — auto-rendered security invariants (the load-bearing rules).
- [`dashboard-implementation.md`](dashboard-implementation.md) — D3 Phase 6 task briefs (agent-handoff artifact).
- [`notification-routing.md`](notification-routing.md) — operator-mail + banner + modal substrate that the dashboard surfaces.
- [`paired-runs.md`](paired-runs.md) — Experiments tab data model and lifecycle.
- `security.md` (planned) — dashboard isolation in the broader security posture.
- [`../onboarding.md`](../onboarding.md) — first dashboard run.
- [`../operator-runbook.md`](../operator-runbook.md) — when the dashboard reports trouble.
