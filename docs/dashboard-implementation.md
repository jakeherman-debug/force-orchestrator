---
audience: agent
scope: D3 Phase 6 dashboard redesign task briefs (Pulse, Briefing, Reflection IA + per-task implementation/validation prompts).
owner: D3
last_reviewed: 2026-05-05
---

# Dashboard Implementation — Task Briefs

This document is the agent-handoff artifact for D3 Phase 6's dashboard redesign. Each task below carries:

- **Implementation prompt** — a self-contained brief an autonomous agent can execute against
- **Validation prompt** — a self-contained brief the operator (or a verification agent) runs to confirm the task landed correctly

Tasks are grouped into Phase 6A (UX scaffolding + Pulse + Briefing) and Phase 6B (Reflection + Drill + verification spec consumption + shakedown). Within a phase, tasks list their dependencies; tasks with no shared dependencies parallelize across separate worktrees.

## Required reading for every dashboard task

Before starting any task, the agent reads:

- `CLAUDE.md` (project root)
- `docs/roadmap.md` § Deliverable 3 in full, with focused re-read of Phase 6
- `docs/paired-runs.md` § Schema additions (Convoys, BountyBoard, ProposedFeatures, ConvoyReviewCycles, dashboard tables)
- `internal/dashboard/` directory structure (existing handlers, templates, static assets)
- `internal/clients/` if the task touches a cross-agent service
- `cmd/force/` for existing CLI command patterns (when adding CLI parity)
- This file's preamble + the specific task brief

The agent must NOT begin implementation before reading the listed material. State the read in the closure note: "Read CLAUDE.md, roadmap.md §D3, paired-runs.md §Schema, internal/dashboard/."

## Universal anti-cheat invariants

Every dashboard task obeys these on top of CLAUDE.md's directives:

- **Bind 127.0.0.1 only** (Fix #2). Every new HTTP handler attaches to the existing mux; no new `http.Server` instances.
- **Same-origin allow-list on every mutation.** Every POST/PUT/PATCH/DELETE goes through `securityMiddleware`.
- **256 KB body cap on mutations.** Translate `*http.MaxBytesError` to 413.
- **No wildcard CORS.** No `Access-Control-Allow-Origin: *` ever.
- **Attacker-writable strings render as `.textContent`, never `.innerHTML`.**
- **Every operator-mail / push-notification site routes through `respectNotificationBudget`** (Pattern P27).
- **CLI equivalent for every mutating dashboard handler** (Pattern P25).
- **Documented keyboard shortcut for every binding; binding for every documented shortcut** (Pattern P26).
- **Schema parity:** every column added to a table is added to BOTH `createSchema` AND `runMigrations` AND `schema/schema.sql`.
- **Tests under `-race -count=5`** for any concurrency-sensitive new code.

---

# Phase 6A — UX Scaffolding + Pulse + Briefing

Phase 6A delivers the operator's day-one experience. After Phase 6A merges, the operator can land in Pulse, see the fleet narrate itself, and triage decisions through Briefing. Reflection and Drill come in 6B.

## Task 6A.1 — Three-surface IA + nav rebuild

**Depends on:** none (foundation task)
**Estimated:** 2 hr autonomous
**Track:** `deliverable/3/phase-6a-nav`

### Implementation prompt

You are rebuilding the dashboard's navigation around three surfaces: **Pulse** (default landing, ambient fleet view), **Briefing** (decision queue and conversational triage), and **Reflection** (calibration and learning). Plus a global **Ask** shortcut accessible via `/` from anywhere.

Existing dashboard tabs (experiments list, proposals queue, metric registry, etc.) get folded INTO these three — they don't get their own top-level slots. The goal is to cap top-level navigation at three options forever.

**Files:**
- `internal/dashboard/static/index.html` — replace existing nav with three-pane layout; `<header>` carries Pulse / Briefing / Reflection links + always-visible search input
- `internal/dashboard/static/app.js` — route handler maps `#/pulse`, `#/briefing`, `#/reflection` to the corresponding render functions; default route is `#/pulse`
- `internal/dashboard/static/style.css` — three-tab nav styling; consistent colour treatment per tab (Pulse=blue, Briefing=amber, Reflection=teal)
- `internal/dashboard/handlers.go` — add `GET /pulse`, `GET /briefing`, `GET /reflection` page handlers (initially serving placeholder content; subsequent tasks fill in)
- `internal/dashboard/handlers_test.go` — assertion that all three pages return 200 with expected page-title strings

**Behaviour:**
- Browser back/forward respects route changes (use `popstate`)
- `Cmd-1`, `Cmd-2`, `Cmd-3` switch tabs (set up keymap stub; full shortcuts arrive in Task 6A.3)
- Search input stays mounted across all three surfaces
- Dashboard load lands on `#/pulse` by default
- Existing widgets that haven't been migrated to a new surface yet remain accessible via `#/legacy/<name>` routes — no breakage of operator's current workflow during the rebuild

**Anti-cheat:**
- All three new handlers wrapped by the existing `securityMiddleware`
- No new `http.Server` instance
- Existing CSP headers preserved
- HTML never assigns attacker-controllable strings to `.innerHTML`

### Validation prompt

Confirm the three-surface IA is live:

```bash
make build && ./force dashboard --port 8080 &
sleep 2
curl -sS http://127.0.0.1:8080/pulse | grep -q "<title>Pulse"
curl -sS http://127.0.0.1:8080/briefing | grep -q "<title>Briefing"
curl -sS http://127.0.0.1:8080/reflection | grep -q "<title>Reflection"
curl -sS http://127.0.0.1:8080/ | grep -q "Pulse" # default landing
```

Open the dashboard in a browser:
- Default landing is Pulse (URL fragment `#/pulse`)
- Three top-level tabs visible
- Search input visible on all three
- Browser back/forward navigates between tabs
- Existing legacy widgets still accessible via `#/legacy/*`
- Clicking a legacy link does NOT 404
- View source: no `innerHTML` assigned a value pulled from server-rendered task or proposal text
- View source: no second `http.Server` constructed in the handler chain

Confirm pattern tests:

```bash
go test -tags sqlite_fts5 -race -count=5 -run TestDashboardThreeSurfaceNav ./internal/dashboard/...
```

---

## Task 6A.2 — Dashboard heartbeat + health banner + status CLI

**Depends on:** 6A.1
**Estimated:** 45 min autonomous
**Track:** `deliverable/3/phase-6a-heartbeat`

### Implementation prompt

Build a heartbeat mechanism so the dashboard can detect — and surface — when it has been silently down. A separate goroutine in the dashboard process inserts into `DashboardHealthHeartbeats(ticked_at, process_pid, bind_addr, in_flight_requests)` every 30s. On every page render, the page template fetches the most recent row; if it is older than 60s, a yellow banner displays at the top of the page: "⚠ Dashboard last successfully ticked X ago — the process may have just restarted."

Add a CLI command `force dashboard status` that prints the most recent heartbeat row from the database and exits 0 if the heartbeat is fresh (< 60s) or 1 if stale.

**Files:**
- `internal/store/schema.go` (createSchema) + `runMigrations` — add `DashboardHealthHeartbeats` table and an index on `ticked_at DESC`
- `schema/schema.sql` — same DDL
- `internal/dashboard/heartbeat.go` (new) — `StartHeartbeat(ctx, db)` goroutine, ticks every 30s
- `internal/dashboard/server.go` (or wherever `RunDashboard` lives) — call `StartHeartbeat` on startup
- `internal/dashboard/middleware.go` — middleware increments an in-flight counter to populate the column; ensure decrement on response completion
- `internal/dashboard/static/index.html` — banner placeholder element with id `#dashboard-health-banner`; logic in app.js fills it from `/api/dashboard/health` (new endpoint)
- `cmd/force/dashboard_cmds.go` — add `force dashboard status` command, calls a store helper that reads the most recent row
- `internal/dashboard/heartbeat_test.go` — heartbeat ticks on a fast clock (injected); banner shows when last tick is forced > 60s old

**Anti-cheat:**
- Heartbeat insert is fire-and-forget but errors are logged with `[DASHBOARD-HEARTBEAT]` prefix
- The CLI command does NOT touch the live dashboard process; it only reads the DB row
- Goroutine respects ctx cancellation (CLAUDE.md daemon-context-threading invariant)

### Validation prompt

```bash
make build
./force dashboard --port 8080 &
sleep 35  # wait for at least one heartbeat tick

./force dashboard status
# expect: exit 0, prints "ticked Xs ago"

# Simulate a stale heartbeat:
sqlite3 ~/.force/holocron.db "UPDATE DashboardHealthHeartbeats SET ticked_at = datetime('now', '-5 minutes') WHERE id = (SELECT MAX(id) FROM DashboardHealthHeartbeats);"
./force dashboard status
# expect: exit 1, prints "STALE — last tick X ago"

# Reload the dashboard in browser:
# expect: yellow banner at top of every page
```

```bash
# Schema parity check
go test -tags sqlite_fts5 -run TestSchemaParity ./internal/store/...

# Heartbeat unit tests
go test -tags sqlite_fts5 -race -count=5 -run TestDashboardHeartbeat ./internal/dashboard/...
```

Confirm `DashboardHealthHeartbeats` table is in BOTH `createSchema` and `runMigrations` and `schema/schema.sql` (CLAUDE.md schema parity invariant).

---

## Task 6A.3 — Keyboard shortcuts + `?` help overlay

**Depends on:** 6A.1
**Estimated:** 1 hr autonomous
**Track:** `deliverable/3/phase-6a-shortcuts`

### Implementation prompt

Add a comprehensive keymap to the dashboard with a `?` overlay documenting every shortcut. The keymap MUST satisfy Pattern P26: every documented shortcut binds to a real action, and every binding is documented.

**Required shortcuts (initial set):**

| Key | Action |
|---|---|
| `Cmd-1` / `Cmd-2` / `Cmd-3` | Switch to Pulse / Briefing / Reflection |
| `/` | Focus the search/Ask input |
| `?` | Toggle help overlay |
| `Esc` | Close help overlay or focus mode |
| `j` / `k` | Next / previous row (in Briefing queue and Reflection lists) |
| `Enter` | Open focused row (in Briefing) |
| `a` | Approve focused decision (Briefing focus mode only) |
| `r` | Reject focused decision (Briefing focus mode only) |
| `D` | Drill — open drill view of focused row (placeholder route in 6A; functional in 6B) |
| `g p` / `g b` / `g r` | "Goto" prefix bindings — same as Cmd-1/2/3 (vim-style) |

**Files:**
- `internal/dashboard/static/keymap.js` (new) — central key dispatch; maps key → action callback; supports prefix bindings (`g p`)
- `internal/dashboard/static/app.js` — register keymap; bind to `keydown` at document level
- `internal/dashboard/static/help-overlay.html` — table-of-shortcuts modal; absolute positioning, dismissed by `Esc` or click-outside
- `internal/dashboard/static/style.css` — overlay styling
- `internal/audittools/audit_pattern_p26_test.go` — parses `keymap.js` (regex-based extraction of `bind('key', action)` calls) AND `help-overlay.html` table; asserts the key sets match exactly

**Behaviour:**
- All shortcuts have a one-line description in the help overlay
- The overlay is also reachable via a `?` button in the page header
- Shortcuts that depend on focus (e.g., `a`/`r` for approve/reject) are disabled outside their relevant context — pressing `a` in Pulse does nothing
- `Esc` always returns to the most recent stable view (Briefing focus mode → Briefing list; Briefing list → Pulse if explicitly initiated from Pulse)

### Validation prompt

```bash
go test -tags sqlite_fts5 -race -count=5 -run TestPattern_P26_KeyboardShortcutConsistency ./internal/audittools/...
```

Open the dashboard in a browser:
- Press `?` — overlay appears with full shortcut list
- Press `Esc` — overlay dismisses
- Press `Cmd-1`, `Cmd-2`, `Cmd-3` — switches between Pulse / Briefing / Reflection
- Press `/` — search input gets focus
- In Briefing, `j`/`k` navigate rows
- Pattern P26 test fails if you remove a shortcut from the help overlay without removing the binding (or vice versa)

Sanity grep:

```bash
# Every binding referenced in keymap.js MUST appear in help-overlay.html, and vice versa.
go test -tags sqlite_fts5 -run TestPattern_P26 -v ./internal/audittools/...
```

---

## Task 6A.4 — Notification budgets schema + emit-site routing

**Depends on:** 6A.1
**Estimated:** 1.5 hr autonomous
**Track:** `deliverable/3/phase-6a-notifications`

### Implementation prompt

Add operator-configurable rate limits for outbound notifications (operator mail, modal alerts, banners) and route every existing emit site through a budget-checking helper. Past the budget, notifications go to a daily digest mail; high-stakes punch through.

**Schema:**

```sql
CREATE TABLE OperatorNotificationBudgets (
  id INTEGER PRIMARY KEY,
  operator_email TEXT NOT NULL,
  source TEXT NOT NULL,                  -- 'investigator'|'captain'|'ec'|'fleet'|'convoy_review'|'medic'|...
  channel TEXT NOT NULL,                 -- 'email'|'modal'|'banner'
  max_per_period INTEGER NOT NULL,
  period_minutes INTEGER NOT NULL,
  digest_remainder BOOLEAN NOT NULL DEFAULT 1,
  UNIQUE(operator_email, source, channel)
);

CREATE TABLE OperatorNotificationDigest (
  id INTEGER PRIMARY KEY,
  operator_email TEXT NOT NULL,
  source TEXT NOT NULL,
  channel TEXT NOT NULL,
  digest_for_date TEXT NOT NULL,         -- 'YYYY-MM-DD'
  payload_json TEXT NOT NULL,            -- accumulated suppressed notifications
  flushed_at TIMESTAMP,
  UNIQUE(operator_email, source, channel, digest_for_date)
);
```

**Code:**
- `internal/store/notification_budgets.go` (new) — `respectNotificationBudget(ctx, db, source, channel, payload, stakesTier) (allowed bool, err error)`. Allowed when:
  - `stakesTier == "high"` → always allowed (punches through)
  - No budget row for this `(operator, source, channel)` → allowed
  - Within the budget window → allowed
  - Past the budget AND `digest_remainder=true` → write to `OperatorNotificationDigest`, return `allowed=false`
  - Past the budget AND `digest_remainder=false` → drop, return `allowed=false`

- Every existing call site that sends operator-facing notifications routes through this helper. Audit:
  - `internal/agents/util.go` — `sendOperatorMail` (or equivalent helper)
  - `internal/agents/*.go` — every `[CONVOY REVIEW PASSED]`, `[CHANCELLOR FAIL-CLOSED]`, `[ESCALATION]` etc. emission
  - `internal/dashboard/handlers.go` — modal/banner emission paths
  - `internal/agents/dogs.go` — periodic notifications (e.g., `convoy-review-watch` summary)

- `internal/dashboard/handlers.go` — `GET /api/notifications/budgets` and `PUT /api/notifications/budgets/:source/:channel` for operator config UI

- `internal/dashboard/static/reflection-budgets.js` — minimal UI in Reflection tab to set per-source budgets (initially in Reflection placeholder; Reflection itself is built in 6B)

- `internal/agents/dogs.go` — new `notification-digest-flush` dog (daily at 09:00 local) — flushes `OperatorNotificationDigest` rows with `flushed_at IS NULL` as a single combined email per source/channel

**Pattern P27 (`TestPattern_P27_NotificationBudgetRouting`):**
- Greps production code for `sendOperatorMail`, `pushNotification`, banner-set, modal-set call sites
- Asserts each one is preceded (in the same code path) by `respectNotificationBudget(...)` returning `allowed=true`
- Direct calls outside the helper fail the test; allowlist accepted only for the helper itself and digest-flush

### Validation prompt

```bash
# Schema parity
go test -tags sqlite_fts5 -run TestSchemaParity ./internal/store/...

# Pattern P27 — every emit site routes through the budget helper
go test -tags sqlite_fts5 -race -count=5 -run TestPattern_P27_NotificationBudgetRouting ./internal/audittools/...

# Helper unit tests
go test -tags sqlite_fts5 -race -count=5 -run TestRespectNotificationBudget ./internal/store/...
```

Manual verification:

```bash
# Set a low budget for Investigator email
sqlite3 ~/.force/holocron.db "INSERT INTO OperatorNotificationBudgets (operator_email, source, channel, max_per_period, period_minutes, digest_remainder) VALUES ('jake.herman@upstart.com', 'investigator', 'email', 1, 60, 1);"

# Trigger an Investigator notification (programmatic — use a test fixture or manual sql)
# Confirm: only one email sent; subsequent ones land in OperatorNotificationDigest
# After 09:00 next day, the digest dog runs; combined email arrives

# High-stakes punches through
# Trigger a high-stakes notification (any [ESCALATION] or [CHANCELLOR FAIL-CLOSED])
# Confirm: arrives even when budget exhausted
```

Confirm Pattern P27 catches a regression: temporarily insert a direct `sendOperatorMail` call in a non-helper site, run the pattern test, expect failure. Revert.

---

## Task 6A.5 — OperatorSessionState + resume-where-you-left-off

**Depends on:** 6A.1
**Estimated:** 1 hr autonomous
**Track:** `deliverable/3/phase-6a-session-state`

### Implementation prompt

When an operator returns to the dashboard after an idle period, mid-review state restores: the same surface, the same focused row, the same scroll position, and any partial form input.

**Schema:**

```sql
CREATE TABLE OperatorSessionState (
  id INTEGER PRIMARY KEY,
  operator_email TEXT NOT NULL,
  last_active_at TIMESTAMP DEFAULT (datetime('now')),
  last_viewed_surface TEXT,             -- 'pulse'|'briefing'|'reflection'|'drill'
  last_viewed_route TEXT,               -- full URL fragment, e.g., '#/briefing/decision/1234'
  last_focused_decision_id INTEGER,     -- nullable
  partial_review_state_json TEXT,       -- nullable; form fields, scroll position, draft text
  UNIQUE(operator_email)
);
```

**Code:**
- Each surface's render hook persists `(surface, route, focused_decision_id)` to OperatorSessionState every 5s and on tab-blur
- Briefing's focus mode also persists `partial_review_state_json` (any draft text, checkbox states, scroll pos) every 5s
- On dashboard load, an early-render hook reads OperatorSessionState; if `last_active_at > 30 min ago`, a banner appears: "You were last reviewing 〈decision title〉. **[ Resume ]** **[ Start fresh ]**"
- "Resume" navigates to `last_viewed_route`, restores scroll, repopulates form fields
- "Start fresh" clears the partial state and lands on Pulse

**Files:**
- `internal/store/schema.go` + migrations + `schema/schema.sql`
- `internal/store/operator_session.go` (new) — getter/setter
- `internal/dashboard/handlers.go` — `GET /api/session/state`, `PUT /api/session/state`
- `internal/dashboard/static/session.js` (new) — periodic save + resume UI
- `internal/dashboard/session_test.go` — round-trip + 30-min-stale banner trigger

**Anti-cheat:**
- `partial_review_state_json` is bounded at 32 KB at write time (reject larger payloads with 413)
- Single operator per system; UNIQUE constraint enforces

### Validation prompt

```bash
go test -tags sqlite_fts5 -run TestSchemaParity ./internal/store/...
go test -tags sqlite_fts5 -race -count=5 -run TestOperatorSessionState ./internal/store/... ./internal/dashboard/...
```

Manual:
- Open dashboard, navigate to Briefing, open a decision in focus mode, type some draft text in a counter-proposal field
- Wait 5s for save
- Force-restart the dashboard (`pkill -SIGTERM force; ./force dashboard --port 8080 &`)
- Dashboard banner shows "You were last reviewing 〈decision title〉. Resume?"
- Click Resume — same focus mode, same draft text, same scroll position

```bash
# Forge a stale session row
sqlite3 ~/.force/holocron.db "UPDATE OperatorSessionState SET last_active_at = datetime('now', '-1 hour');"
# Reload — banner should appear
# Click "Start fresh" — verify partial_review_state_json cleared in DB
```

---

## Task 6A.6 — Trust dials per agent

**Depends on:** 6A.1
**Estimated:** 1.5 hr autonomous
**Track:** `deliverable/3/phase-6a-trust-dials`

### Implementation prompt

Every agent (Captain, Council, Medic, Investigator, EC, ConvoyReview, Pilot, Diplomat, Chancellor) gets a per-operator trust dial (0-100). The dial is operator-set, suggested by calibration coaching, never auto-changed. Dial value influences friction tier in Briefing (high trust → lighter UI; low trust → heavier).

**Schema:**

```sql
CREATE TABLE OperatorTrustDials (
  id INTEGER PRIMARY KEY,
  operator_email TEXT NOT NULL,
  agent TEXT NOT NULL,
  dial_value INTEGER NOT NULL CHECK(dial_value BETWEEN 0 AND 100),
  set_at TIMESTAMP DEFAULT (datetime('now')),
  set_by TEXT NOT NULL,                  -- 'operator' | 'calibration_suggestion'
  rationale TEXT,                        -- nullable; required when 'operator' overrides a suggestion
  UNIQUE(operator_email, agent, set_at)
);
```

The `UNIQUE` on `(operator_email, agent, set_at)` is history-preserving — each row is one trust-dial-event. Latest by `set_at` per `(operator, agent)` is the current value.

**Code:**
- `internal/store/trust_dials.go` (new) — `GetCurrentTrustDial(db, operator, agent) (int, error)`, `SetTrustDial(...)` writes a new row
- Bootstrap migration: insert a row per (operator, agent) at `dial_value=70`, `set_by='system_default'`
- `internal/agents/briefing.go` (new — placeholder; real Briefing logic in 6A.10) — function `frictionTierFor(dial int, baseTier string) string` shifts tier up/down based on dial

**Friction shift rules:**
- Base medium-stakes proposal + dial < 40 → bump to high-stakes
- Base medium-stakes proposal + dial > 85 → drop to low-stakes
- High-stakes (CLAUDE.md/BoS/Senate amendments, AT deprecations) NEVER shift down — they stay high regardless of dial

**UI:**
- Pulse fleet panel shows compact horizontal trust dials (one row per agent)
- Reflection has a full trust dials panel with history graph (sparkline of dial value over time)
- Click a dial in Pulse → opens Reflection trust panel scrolled to that agent
- Operator change requires a rationale field if the change applies a different value than a recent calibration suggestion

**Pattern:**
- New pattern test asserts no system code path inserts into OperatorTrustDials with `set_by='operator'` from a non-operator-routed handler

### Validation prompt

```bash
go test -tags sqlite_fts5 -run TestSchemaParity ./internal/store/...
go test -tags sqlite_fts5 -race -count=5 -run TestTrustDials ./internal/store/...
go test -tags sqlite_fts5 -race -count=5 -run TestPattern_TrustDialsOperatorWriteDiscipline ./internal/audittools/...
```

Manual:
- Open Pulse — trust dials visible for all 9 agents at dial_value=70
- Open Reflection — trust dial panel with history graph (initially flat at 70 since just bootstrapped)
- Set Captain dial to 30 — change reflected in Pulse
- Open Briefing (after 6A.10 lands), open a Captain medium-stakes proposal — should render in high-stakes UI (affirmation checkbox required)
- Set Captain dial to 90 — same proposal renders in low-stakes UI

```bash
# DB sanity
sqlite3 ~/.force/holocron.db "SELECT agent, dial_value, set_by, set_at FROM OperatorTrustDials ORDER BY set_at DESC LIMIT 20;"
# Expect: 9 bootstrap rows at 70, then operator-set rows for Captain
```

---

## Task 6A.7 — Live narrative panel + LLM batching

**Depends on:** 6A.2 (heartbeat for stale-detection), 6A.4 (budget for emit), 6A.6 (trust dials display)
**Estimated:** 2 hr autonomous
**Track:** `deliverable/3/phase-6a-narrative`

### Implementation prompt

The Pulse left half is a continuously-updating narrative panel. Every 30s, a goroutine collects events from the prior 30s window (TaskHistory transitions, sub-PR events, Council/Medic/ConvoyReview rulings, dog actions, escalations, ratifications), calls Haiku with a fixed-template prompt, stores the rendered prose in `NarrativeRenders`, and broadcasts it to connected clients via SSE.

**Schema:**

```sql
CREATE TABLE NarrativeRenders (
  id INTEGER PRIMARY KEY,
  rendered_at TIMESTAMP DEFAULT (datetime('now')),
  event_window_start TIMESTAMP NOT NULL,
  event_window_end TIMESTAMP NOT NULL,
  source_event_count INTEGER NOT NULL,
  source_event_refs_json TEXT NOT NULL,  -- [{kind, ref}, ...] for click-through to raw event
  prose TEXT NOT NULL,
  prompt_version TEXT NOT NULL,
  cost_usd REAL,
  cache_hit BOOLEAN
);
CREATE INDEX idx_nr_window ON NarrativeRenders(event_window_end DESC);
```

**Code:**
- `internal/agents/narrative_renderer.go` (new) — `SpawnNarrativeRenderer(ctx, db)` goroutine; ticks every 30s; respects `IsEstopped(db)` (when e-stop, writes a static "🛑 E-STOP active" template and skips the LLM call); honours notification budgets (uses `respectNotificationBudget` internally for the "narrative panel render" channel — operators can configure how often narrative refreshes if they want)
- Prompt template lives in `internal/agents/narrative_prompts/v1.go` as a versioned const (Pattern P28 enforces no editorial copy in the rendering pipeline)
- `internal/dashboard/handlers.go` — `GET /api/pulse/narrative/stream` (SSE; emits new NarrativeRenders rows as they arrive)
- `internal/dashboard/static/pulse.js` — connects to SSE; renders narrative cards; click on a narrative line opens a popover with the source event refs (click-through to drill in 6B)

**Cost cap:**
- `SystemConfig.narrative_render_daily_cap_usd` (default 1.50). Daily rolling sum of `NarrativeRenders.cost_usd` past the cap → renderer falls back to a static template ("X events in the last 30s — narrative budget exhausted; raise the cap in SystemConfig if needed") until next UTC midnight.

**Pattern P28:**
- Asserts `NarrativeRenders.prose` is produced ONLY by `internal/agents/narrative_renderer.go` (no other code path writes to the table)
- Asserts the prompt template is in code (`internal/agents/narrative_prompts/`), not a DB-stored config (drift risk)
- Asserts the prompt template is version-stamped and `NarrativeRenders.prompt_version` matches the const

### Validation prompt

```bash
go test -tags sqlite_fts5 -run TestSchemaParity ./internal/store/...
go test -tags sqlite_fts5 -race -count=5 -run TestNarrativeRenderer ./internal/agents/...
go test -tags sqlite_fts5 -race -count=5 -run TestPattern_P28_NarrativeIsGenerated ./internal/audittools/...
```

Manual:
- Start the daemon AND the dashboard
- Trigger a few events: queue a task, complete a task, trigger a Council ruling
- Wait 30s
- Open Pulse — narrative cards appear with timestamps and prose
- Click a card — popover shows source event refs

```bash
# Check cost is bounded
sqlite3 ~/.force/holocron.db "SELECT SUM(cost_usd) FROM NarrativeRenders WHERE rendered_at > datetime('now', '-1 day');"
# Expect: < 1.50 (or fallback active)

# Check e-stop respected
./force estop --on
# Wait 35s
# Pulse narrative shows "🛑 E-STOP active" template
sqlite3 ~/.force/holocron.db "SELECT prose FROM NarrativeRenders ORDER BY rendered_at DESC LIMIT 1;"
# Expect: starts with "🛑 E-STOP active"
./force estop --off
```

---

## Task 6A.8 — Pulse fleet panel

**Depends on:** 6A.1, 6A.6
**Estimated:** 2 hr autonomous
**Track:** `deliverable/3/phase-6a-pulse-panel`

### Implementation prompt

The Pulse right half is the fleet vital-signs panel: spend rate, active agents, convoys in flight, queue at a glance, trust dials compact.

**Components (right column, top to bottom):**

1. **Spend** — current $/hr (trailing 1-hour window from spend events); today's projected total; last 7d avg (sparkline); top burner this hour with task ID link
2. **Active agents** — list of agents currently locked on tasks; for each: agent name, task ID + truncated payload, claimed_at relative time, animated progress dot
3. **Convoys in flight** — list of convoys with status != 'Completed' / 'Cancelled'; each row: convoy name, status pill (Planning / Coding / Review / DraftPROpen / Shipping), cycle count, click → drill convoy view (placeholder until 6B)
4. **Queue at a glance** — N decisions awaiting operator, broken down by stakes tier (low/medium/high counts as compact badges); click → Briefing
5. **Trust dials compact** — horizontal sparkline-style row per agent (use same widget as 6A.6 in compact mode)

**Code:**
- `internal/dashboard/handlers.go` — `GET /api/pulse/snapshot` returns a single JSON payload combining all components above; `GET /api/pulse/stream` SSE for live updates (every 5s)
- `internal/dashboard/static/pulse.js` — render all panels from the snapshot; SSE updates re-render in place
- `internal/store/pulse_queries.go` (new) — pre-computed query helpers (avoid N+1; use joins against BountyBoard, TaskHistory, Convoys, OperatorTrustDials)

**Performance:**
- Snapshot query targets < 100 ms total; if exceeded, log a `[PULSE-SLOW]` warning to operator mail (subject to budget)
- SSE updates throttled at 5s minimum interval per connection

**Anti-cheat:**
- All payload fields render via `.textContent` (task payload truncation done server-side, no `.innerHTML`)

### Validation prompt

```bash
go test -tags sqlite_fts5 -race -count=5 -run TestPulseSnapshot ./internal/dashboard/...
```

Manual:
- Open Pulse with the daemon running
- All five panels populated
- Trigger a task claim — active-agents panel updates within 5s via SSE
- Trigger a task completion — convoys-in-flight panel updates
- Trigger an LLM call (any agent action) — spend panel reflects the cost within 5s
- Click a convoy row — opens drill view (placeholder until 6B)

```bash
# Performance check
time curl -sS http://127.0.0.1:8080/api/pulse/snapshot >/dev/null
# Expect: < 100ms

# XSS check — inject a payload with HTML in a task description
sqlite3 ~/.force/holocron.db "UPDATE BountyBoard SET payload = '<script>alert(1)</script>' WHERE id = (SELECT MIN(id) FROM BountyBoard WHERE status = 'Locked');"
# Reload Pulse — verify the script tag renders as text, not executes
```

---

## Task 6A.9 — "While you were away" cinematic on wake

**Depends on:** 6A.7, 6A.8, plus heartbeat-based sleep detection from D3 Phase 6 sleep-handling track
**Estimated:** 2 hr autonomous
**Track:** `deliverable/3/phase-6a-cinematic`

### Implementation prompt

When the dashboard loads after a detected sleep event (heartbeat gap > 90s + reconciliation sweep ran), a 30-second cinematic plays in Pulse before the normal narrative panel resumes. The cinematic is a synthesized animated narrative of what happened during sleep: ConvoyReview cycles passing as quiet pulses, escalations as flagged pauses, total tasks shipped, total spent, the top thing that needs you.

**Components:**
- `internal/dashboard/static/cinematic.js` (new) — animated SVG-or-canvas timeline; reads from `/api/pulse/cinematic?since=<sleep_started_at>`; renders ~30s of animation
- Backend handler aggregates events from the sleep window: TaskHistory transitions, NarrativeRenders rendered during sleep (replayed quickly), escalations opened/closed, sub-PRs merged, total spend
- Cinematic ends with a "summary card" naming the single most important thing the operator needs to attend to (use the queue's highest-stakes-pending item)
- Skip-cinematic button in the corner; preference saved to OperatorSessionState (`partial_review_state_json.cinematic_skipped: true` becomes default for future wakes)

**Edge cases:**
- Sleep event detected but no events occurred during sleep → static "Welcome back. Nothing happened while you were away." card; no animation
- Operator was offline > 7 days → cinematic compressed to 30s regardless; show a "long sleep" framing ("a lot has happened — this is highlights only")
- Operator is currently in Briefing focus mode when wake detected (laptop wakes mid-review somehow) → no cinematic, just refresh state and resume

**Cost:**
- Cinematic uses NO new LLM calls — it replays existing NarrativeRenders rows. The summary card uses one Haiku call (~$0.001) to synthesize the "most important thing." Same notification budget as narrative renderer.

### Validation prompt

```bash
go test -tags sqlite_fts5 -race -count=5 -run TestCinematicHandler ./internal/dashboard/...
```

Manual:
- With the daemon running, simulate a sleep gap: stop the daemon for 10 min while events accumulate (or fake the heartbeat gap by SQL)
- Restart the daemon — heartbeat detects the gap
- Reload the dashboard — cinematic plays in Pulse
- Cinematic shows: the events from the window in animated form, ends with a summary card highlighting the highest-stakes pending decision
- Skip button works; verify preference persists across reloads

```bash
# Force a sleep event
sqlite3 ~/.force/holocron.db "INSERT INTO DashboardHealthHeartbeats (ticked_at) VALUES (datetime('now', '-15 minutes'));"
# Trigger any state mutation so reconciliation logs a sleep
# Reload dashboard — cinematic should play
```

---

## Task 6A.10 — Briefing conversational triage

**Depends on:** 6A.1, 6A.2, 6A.4, 6A.5, 6A.6
**Estimated:** 3 hr autonomous
**Track:** `deliverable/3/phase-6a-briefing`

### Implementation prompt

Briefing is the decision queue surface. Each pending decision (Captain proposal, spec amendment ratification, ProposedFeature triage, PromotionProposal ratification, etc.) gets a conversational presentation: a Haiku-generated briefing paragraph sourced from the decision's structured data and prior similar decisions. Operator approves/rejects/asks-for-more inline.

**Schema:**

```sql
CREATE TABLE BriefingRenders (
  id INTEGER PRIMARY KEY,
  decision_id INTEGER NOT NULL,
  decision_kind TEXT NOT NULL,           -- 'captain_proposal'|'spec_amendment'|'promotion_proposal'|'proposed_feature'|...
  rendered_at TIMESTAMP DEFAULT (datetime('now')),
  briefing_text TEXT NOT NULL,
  prior_similar_decisions_json TEXT,     -- [{decision_id, outcome, when, context}, ...]
  prompt_version TEXT NOT NULL,
  cost_usd REAL,
  operator_decision TEXT,                -- 'approved'|'rejected'|'deferred' — set when operator decides
  decision_time_seconds INTEGER          -- set when operator decides
);
CREATE INDEX idx_br_decision ON BriefingRenders(decision_kind, decision_id, rendered_at DESC);
```

**Code:**
- `internal/agents/briefing_renderer.go` (new) — `RenderBriefing(ctx, db, decisionKind, decisionID, trustDial int) (BriefingRender, error)`. Steps:
  1. Load the decision's structured data
  2. Load up to 5 prior similar decisions (same kind, same agent, similar payload — use simple similarity heuristic + LLM judgment for fuzzy match)
  3. Call Haiku with the briefing prompt template (versioned in `internal/agents/briefing_prompts/v1.go`)
  4. Insert into BriefingRenders
- `internal/dashboard/handlers.go`:
  - `GET /api/briefing/queue` — returns pending decisions sorted by stakes tier (high first), then by created_at
  - `GET /api/briefing/decision/:kind/:id` — returns BriefingRender (creates one if none exists)
  - `POST /api/briefing/decide` — operator decision; updates BriefingRender + dispatches the actual approve/reject to the relevant downstream handler (PromotionProposalRatify, AmendmentRatify, etc.)
- `internal/dashboard/static/briefing.js`:
  - List view: queue rows with stakes-tier visual differentiation (concern #11 G — left border colour, badge)
  - Click row → focus mode (full-screen briefing card)
  - Buttons: Approve / Reject / Tell me more
  - "Tell me more" expands inline: cited evidence, raw decision data, prior similar decisions panel, "Ask Force" inline input
  - Friction tier (concern #4): low → batch view multi-select; medium → single-row click; high → modal with affirmation checkbox required

**Trust-dial integration:**
- The `frictionTierFor` helper from 6A.6 shifts the rendered tier based on the agent's trust dial
- Briefing UI reads the (effective tier) from the API response and renders accordingly

**Pattern P29 (briefing prose cites real evidence):**
- Fuzz tests feed BriefingRenders with random decision IDs
- Asserts every decision ID mentioned in `briefing_text` resolves to a real row
- Hallucinated IDs (LLM made up a decision number) fail the test

**Cost cap:**
- `SystemConfig.briefing_render_daily_cap_usd` (default 5.00); past the cap, fallback to structured-table presentation (no prose) until UTC midnight

### Validation prompt

```bash
go test -tags sqlite_fts5 -run TestSchemaParity ./internal/store/...
go test -tags sqlite_fts5 -race -count=5 -run TestBriefingRenderer ./internal/agents/...
go test -tags sqlite_fts5 -race -count=5 -run TestPattern_P29_BriefingCitesRealEvidence ./internal/audittools/...
```

Manual flow:
- Queue several pending decisions (Captain proposal, spec amendment, ProposedFeature, PromotionProposal)
- Open Briefing — list view shows all pending decisions sorted by stakes tier
- Click highest-stakes row — focus mode opens
- Briefing text mentions specific cited evidence (AT-NNN, FleetRules.rule_key)
- Click "Tell me more" — cited evidence renders alongside Captain's claim text
- Approve via `a` keyboard shortcut — decision dispatched, row slides out
- Reject — counter-proposal forcing modal opens (per 6A.11)
- Verify trust dial shift: lower Captain dial to 30, refresh a Captain medium-stakes decision, observe high-stakes UI (affirmation checkbox)

```bash
# Hallucination check
sqlite3 ~/.force/holocron.db "INSERT INTO BriefingRenders (decision_id, decision_kind, briefing_text, prompt_version) VALUES (99999, 'captain_proposal', 'Three weeks ago you approved a similar task on convoy #99999.', 'v1');"
go test -tags sqlite_fts5 -run TestPattern_P29 ./internal/audittools/...
# Expect: failure naming the bogus decision ID
```

---

## Task 6A.11 — Counter-proposal forcing in Briefing

**Depends on:** 6A.10
**Estimated:** 1 hr autonomous
**Track:** `deliverable/3/phase-6a-counter-proposal`

### Implementation prompt

When the operator rejects a high-stakes decision in Briefing, a modal opens forcing them to choose ONE of:

- **The whole thing — shouldn't happen at all** → text reason mandatory ≥ 20 chars
- **Different approach — let me draft an alternative** → inline draft area; on submit, draft routes back through the relevant agent (Captain for proposals, EC for promotion proposals) to refine into a structured proposal that re-enters the queue
- **Defer — I want Investigator to look at this more** → kicks the rejected proposal to Investigator's intake stream as a `[REJECTED_NEEDS_INVESTIGATION]` event

**Schema:**

```sql
ALTER TABLE BriefingRenders ADD COLUMN counter_proposal_kind TEXT;        -- 'whole_thing'|'different_approach'|'defer'
ALTER TABLE BriefingRenders ADD COLUMN counter_proposal_text TEXT;
ALTER TABLE BriefingRenders ADD COLUMN counter_proposal_routed_id INTEGER;  -- nullable; ID of the new proposal/task spawned
```

**Code:**
- `internal/dashboard/handlers.go` — `POST /api/briefing/reject` accepts `{decision_id, decision_kind, counter_proposal_kind, text}`; validates per kind; routes appropriately
- `internal/agents/counter_proposal_router.go` (new) — `RouteCounterProposal(ctx, db, briefingID, kind, text) (newProposalID int, err error)`. For `different_approach`, calls Captain/EC with the operator's draft + critic note; for `defer`, inserts an `InvestigatorAttention` row
- Modal UI in `briefing.js` with the three radio options + conditional fields

**Pattern:**
- Existing high-tier rejection requires `counter_proposal_kind` non-null; API returns 400 if missing
- `whole_thing` requires `text` ≥ 20 chars; `different_approach` requires text ≥ 50 chars; `defer` allows empty text
- Pattern test asserts API rejects empty `counter_proposal_kind` on high-stakes decisions

### Validation prompt

```bash
go test -tags sqlite_fts5 -race -count=5 -run TestCounterProposalRouter ./internal/agents/...
go test -tags sqlite_fts5 -race -count=5 -run TestPattern_HighStakesRequiresCounterProposal ./internal/audittools/...
```

Manual:
- Reject a high-stakes decision in Briefing → modal opens
- Try to submit empty → blocked at UI AND server-side
- Submit "different approach" with draft text → new proposal appears in queue, routed through Captain
- Submit "defer" → InvestigatorAttention row created

```bash
sqlite3 ~/.force/holocron.db "SELECT counter_proposal_kind, counter_proposal_text, counter_proposal_routed_id FROM BriefingRenders WHERE operator_decision = 'rejected' ORDER BY rendered_at DESC LIMIT 5;"
```

---

## Task 6A.12 — Prior-similar-decisions context

**Depends on:** 6A.10
**Estimated:** 1 hr autonomous
**Track:** `deliverable/3/phase-6a-similar-decisions`

### Implementation prompt

When Briefing renders a decision, include up to 5 prior similar decisions in the briefing text and the "Tell me more" panel. Similarity is computed via:

1. Same `decision_kind`
2. Same `agent` (when applicable)
3. For Captain proposals: same `cited_ats[].at_id` clusters OR same target file paths in payload
4. For ProposedFeatures: same `fingerprint` (which is already canonical)
5. For PromotionProposals: same `rule_key`
6. Fallback: text-similarity (cosine over TF-IDF on the payload+title) over the last 200 decisions of the same kind

**Code:**
- `internal/store/decision_similarity.go` (new) — `FindPriorSimilar(db, kind, decisionID, limit int) ([]PriorSimilar, error)`. PriorSimilar struct includes `decision_id`, `decided_at`, `outcome` ('approved'|'rejected'|'deferred'), `subsequent_outcome` (e.g., 'shipped_clean'|'reverted'|'flagged_in_review'|'pending'), `summary` (one-line)
- BriefingRenderer (6A.10) calls `FindPriorSimilar` and includes the result in the prompt context AND stores in `BriefingRenders.prior_similar_decisions_json`

**Subsequent outcome computation:**
- For each prior similar decision, look up downstream signals: was the resulting convoy merged clean, did ConvoyReview flag a regression on a later cycle, did the operator revert via a PromotionProposal rejection_action?
- Cache results in the BriefingRenders.prior_similar_decisions_json so re-rendering doesn't re-query

**Anti-cheat:**
- The "subsequent_outcome" field MUST be derivable from real DB rows; cannot be invented by the LLM. Pattern asserts each value appears in a small enum.
- Fuzz test: feed a synthetic prior decision into the helper, mutate the downstream state, verify subsequent_outcome reflects.

### Validation prompt

```bash
go test -tags sqlite_fts5 -race -count=5 -run TestFindPriorSimilar ./internal/store/...
```

Manual:
- Approve a Captain proposal that rate-limits an endpoint
- Wait for it to ship clean
- Approve another similar proposal a week later
- Open Briefing on the second one — "Tell me more" panel shows the first decision with outcome="shipped clean"
- Check `prior_similar_decisions_json` in DB: verifiable refs to real rows

```bash
# Reject one and verify outcome=rejected appears in subsequent similar
sqlite3 ~/.force/holocron.db "SELECT prior_similar_decisions_json FROM BriefingRenders ORDER BY rendered_at DESC LIMIT 1;" | jq .
```

---

## Task 6A.13 — Cooldown banner for high-stakes auto-execute

**Depends on:** 6A.4, 6A.10
**Estimated:** 1.5 hr autonomous
**Track:** `deliverable/3/phase-6a-cooldown`

### Implementation prompt

When an auto-execute decision (Council approve → auto-merge, Medic auto-fix on a high-stakes path, ConvoyReview spawn on a critical convoy) has `stakes_tier='high'`, schedule the action with a 60-second cooldown. During the cooldown, Pulse displays a banner: "⏱ Council just approved Aria-3's PR on convoy #47. Auto-merging in 47s. **[ Pause ]** **[ Skip cooldown ]**". Operator can pause (action holds until manually resumed), skip cooldown (executes immediately), or ignore (default executes after 60s).

**Schema:**

```sql
CREATE TABLE CooldownPauses (
  id INTEGER PRIMARY KEY,
  decision_id INTEGER NOT NULL,
  decision_kind TEXT NOT NULL,
  scheduled_action_at TIMESTAMP NOT NULL,
  paused_at TIMESTAMP,                   -- nullable
  paused_by_email TEXT,                  -- nullable
  resumed_at TIMESTAMP,                  -- nullable; if paused, when resumed
  cancelled_at TIMESTAMP,                -- nullable; if operator cancelled outright
  executed_at TIMESTAMP                  -- nullable; when the action actually fired
);
CREATE INDEX idx_cp_pending ON CooldownPauses(scheduled_action_at) WHERE executed_at IS NULL AND cancelled_at IS NULL;
```

**Code:**
- `internal/agents/cooldown_scheduler.go` (new) — wraps any auto-execute call; for high-stakes, inserts a CooldownPauses row and schedules a deferred execution via a dog or goroutine ticker; for non-high-stakes, executes immediately as today
- Audit every auto-execute site in `internal/agents/`:
  - Council approve → sub-PR merge / auto-merge to main
  - Medic auto-fix dispatch (when target convoy is `critical=true`)
  - ConvoyReview fix-task spawn (when target convoy is `critical=true`)
  - PromotionProposal auto-revert (when `rejection_action='clean_revert'`)
- Pulse banner subscribes to a SSE stream of pending CooldownPauses
- Pause endpoint: `POST /api/cooldown/:id/pause`
- Resume endpoint: `POST /api/cooldown/:id/resume` (operator must provide rationale; logged in CooldownPauses)
- Cancel endpoint: `POST /api/cooldown/:id/cancel`
- `cooldown-execute` dog (1s tick) processes due CooldownPauses

**Pattern P30:**
- Asserts every high-stakes auto-execute call site routes through `cooldown_scheduler.Schedule(...)`. Direct execution of a high-stakes action without a CooldownPauses row fails the test.

### Validation prompt

```bash
go test -tags sqlite_fts5 -run TestSchemaParity ./internal/store/...
go test -tags sqlite_fts5 -race -count=5 -run TestCooldownScheduler ./internal/agents/...
go test -tags sqlite_fts5 -race -count=5 -run TestPattern_P30_HighStakesCooldown ./internal/audittools/...
```

Manual:
- Mark a convoy as `critical=true`
- Trigger a Council approval on that convoy → cooldown banner appears in Pulse
- Click Pause within 60s — action holds; banner shows "Paused by you"
- Click Resume with rationale — action executes
- Repeat with a non-critical convoy — no cooldown, action executes immediately

```bash
sqlite3 ~/.force/holocron.db "SELECT id, decision_kind, scheduled_action_at, paused_at, executed_at FROM CooldownPauses ORDER BY id DESC LIMIT 5;"
```

---

## Task 6A.14 — Operator attention tags

**Depends on:** 6A.1, 6A.4
**Estimated:** 1 hr autonomous
**Track:** `deliverable/3/phase-6a-attention-tags`

### Implementation prompt

Operator can mark any convoy / feature / agent / FleetRule as `following` (high attention — events ping with banner notifications) or `muted` (events route to digest only). Default is `normal`.

**Schema:**

```sql
CREATE TABLE OperatorAttentionTags (
  id INTEGER PRIMARY KEY,
  operator_email TEXT NOT NULL,
  target_kind TEXT NOT NULL,             -- 'convoy'|'feature'|'agent'|'rule_key'
  target_id TEXT NOT NULL,
  attention_level TEXT NOT NULL CHECK(attention_level IN ('following','normal','muted')),
  set_at TIMESTAMP DEFAULT (datetime('now')),
  rationale TEXT,                        -- nullable; required when 'muted'
  UNIQUE(operator_email, target_kind, target_id)
);
```

**Code:**
- `internal/store/attention_tags.go` — getter + setter
- `internal/dashboard/handlers.go` — `PUT /api/attention/:kind/:id` accepts level + optional rationale
- UI: in Pulse and Briefing, every convoy/feature/agent reference renders with a small attention icon (eye for following, mute for muted, nothing for normal); click toggles
- `respectNotificationBudget` from 6A.4 reads attention tags: `following` → unconditional emit; `muted` → always digest; `normal` → existing budget rules

### Validation prompt

```bash
go test -tags sqlite_fts5 -run TestSchemaParity ./internal/store/...
go test -tags sqlite_fts5 -race -count=5 -run TestAttentionTags ./internal/store/...
```

Manual:
- Mark convoy #47 as `following` → events from convoy #47 appear as banner notifications regardless of source budget
- Mark Investigator agent as `muted` → Investigator emails go to digest until next 09:00 flush
- Verify `rationale` required for muted — UI rejects empty submission

---

## Task 6A.15 — CLI parity audit + fill (Pattern P25)

**Depends on:** 6A.2, 6A.4, 6A.5, 6A.6, 6A.10, 6A.11, 6A.13, 6A.14
**Estimated:** 2 hr autonomous
**Track:** `deliverable/3/phase-6a-cli-parity`

### Implementation prompt

Every mutating dashboard handler MUST have a CLI equivalent. Walk `internal/dashboard/handlers.go` and audit every non-GET route; for each, ensure a `force <verb>` command exists in `cmd/force/`.

**Audit checklist (existing + new from 6A):**

| Handler | CLI command (existing or to-add) |
|---|---|
| POST /api/briefing/decide | `force decide <kind> <id> [--approve|--reject] --rationale <text>` |
| POST /api/briefing/reject | `force reject <kind> <id> --counter-kind <…> --text <…>` |
| PUT /api/notifications/budgets | `force notifications budget <source> <channel> <max> <period_min>` |
| PUT /api/session/state | `force session save <route>` (mostly internal — but keep for symmetry) |
| PUT /api/trust-dials/:agent | `force trust <agent> <value> [--rationale <text>]` |
| PUT /api/attention/:kind/:id | `force attention <kind> <id> <level> [--rationale <text>]` |
| POST /api/cooldown/:id/pause | `force cooldown pause <id>` |
| POST /api/cooldown/:id/resume | `force cooldown resume <id> --rationale <text>` |
| POST /api/cooldown/:id/cancel | `force cooldown cancel <id>` |
| (existing) POST /api/escalations/:id/ack | `force escalation ack <id>` (verify exists) |
| (existing) POST /api/proposals/:id/ratify | `force ratify <id>` (verify exists) |

**Code:**
- For each missing command, add `cmd/force/<verb>_cmds.go`
- Each CLI command calls the same store-layer function the handler uses (or makes an HTTP call to the local dashboard if the operation is dashboard-stateful)
- `internal/audittools/audit_pattern_p25_test.go` — walks `handlers.go` for non-GET routes via regex/AST; for each, asserts a corresponding command exists in `cmd/force/` (matched by command name registry)

**Pattern P25:**
- Allowlist accepted ONLY for routes that genuinely have no operator-action semantic (e.g., heartbeat-write endpoints from the dashboard process itself); allowlist entries carry a one-line rationale per CLAUDE.md's allowlist-truthfulness invariant

### Validation prompt

```bash
go test -tags sqlite_fts5 -race -count=5 -run TestPattern_P25_CLIParity ./internal/audittools/...
```

Manual:

```bash
# Test each CLI parallels its handler
./force trust captain 85 --rationale "calibration suggests"
./force attention convoy 47 following
./force cooldown pause 12
./force cooldown resume 12 --rationale "verified diff is clean"
./force decide captain_proposal 1234 --approve --rationale "ship it"
./force reject promotion_proposal 56 --counter-kind whole_thing --text "this rule conflicts with AT-008 in convoy #47"

# Verify the same DB rows result as if the operator had clicked through the dashboard
```

```bash
# Pattern P25 catches a regression
# Add a new handler without a CLI equivalent → P25 fails
```

---

# Phase 6B — Reflection + Drill + Verification Spec + Trust Layers + Shakedown

Phase 6B builds on 6A's foundations. Reflection consumes the calibration data captured by Briefing; Drill consumes the transcripts captured at every LLM call site; verification spec consumption uses the spec amendment UX from Briefing.

## Task 6B.1 — LLMCallTranscripts capture wrapper

**Depends on:** none in 6B (foundation for the rest of 6B's diagnostic surfaces)
**Estimated:** 1.5 hr autonomous
**Track:** `deliverable/3/phase-6b-transcripts`

### Implementation prompt

Every Claude CLI invocation in the fleet records its full prompt + response + tool calls + cost into `LLMCallTranscripts`. This is the substrate for Drill, Replay, and the cost analytics in Reflection.

**Schema:**

```sql
CREATE TABLE LLMCallTranscripts (
  id INTEGER PRIMARY KEY,
  task_id INTEGER,                       -- nullable
  agent TEXT NOT NULL,
  prompt_version TEXT NOT NULL,
  call_started_at TIMESTAMP NOT NULL,
  call_completed_at TIMESTAMP,
  system_prompt TEXT NOT NULL,
  user_prompt TEXT NOT NULL,
  response_text TEXT,
  tool_calls_json TEXT,                  -- [{tool, args, result, duration_ms}, ...]
  cost_usd REAL,
  input_tokens INTEGER,
  output_tokens INTEGER,
  cache_read_tokens INTEGER,
  cache_creation_tokens INTEGER,
  archived_at TIMESTAMP                  -- when body offloaded to disk
);
CREATE INDEX idx_llmct_task ON LLMCallTranscripts(task_id, call_started_at);
CREATE INDEX idx_llmct_agent ON LLMCallTranscripts(agent, call_started_at);
```

**Code:**
- `internal/claude/transcript.go` (new) — wraps existing `AskClaudeCLI` and `RunCLIStreamingContext` to record into LLMCallTranscripts
- The wrapper:
  1. Inserts a row with `call_started_at` set, body fields populated
  2. Routes ALL fields through `store.RedactSecrets` BEFORE insert (Fix #10 invariant)
  3. After the underlying call returns, UPDATE the row with response, tool calls, cost, tokens, completed_at
  4. Cancellation (ctx.Done) records partial state (response = whatever was streamed; completed_at NULL)
- Refactor `claude.AskClaudeCLI` and `claude.RunCLIStreamingContext` so all production call sites flow through the wrapper. The signature changes minimally — add a `(agent string, taskID int, promptVersion string)` triplet to the call descriptor.
- Audit every call site:
  - `internal/agents/captain.go`, `jedi_council.go`, `medic.go`, `convoy_review.go`, `pr_review_triage.go`, `chancellor.go`
  - `internal/agents/narrative_renderer.go` (from 6A.7)
  - `internal/agents/briefing_renderer.go` (from 6A.10)
  - Any other LLM-invoking file in `internal/agents/`

**Pattern P31 (`TestPattern_P31_AllLLMCallsCaptured`):**
- Greps production code for `claude.AskClaudeCLI` / `claude.RunCLIStreamingContext` direct calls
- Asserts each call site goes through the new wrapper (`claude.CallWithTranscript(...)`)
- Direct un-wrapped calls fail the test

**Anti-cheat:**
- Redaction at write time is non-negotiable (Fix #10); pattern test asserts `RedactSecrets` is called before insert
- Wrapper handles ctx cancellation gracefully — partial transcripts persist for forensic value

### Validation prompt

```bash
go test -tags sqlite_fts5 -run TestSchemaParity ./internal/store/...
go test -tags sqlite_fts5 -race -count=5 -run TestLLMCallTranscriptWrapper ./internal/claude/...
go test -tags sqlite_fts5 -race -count=5 -run TestPattern_P31_AllLLMCallsCaptured ./internal/audittools/...
```

Manual:
- Trigger a Captain ruling on a real task
- After the ruling completes, query DB:

```bash
sqlite3 ~/.force/holocron.db "SELECT id, task_id, agent, length(system_prompt), length(response_text), cost_usd FROM LLMCallTranscripts ORDER BY id DESC LIMIT 5;"
# Expect: row with non-empty prompts, response, cost_usd > 0
```

```bash
# Redaction check — inject a fake secret pattern, trigger an LLM call, verify redacted in DB
# (use the existing redaction test fixtures)
```

---

## Task 6B.2 — GitOperationLog at internal/git helpers

**Depends on:** none
**Estimated:** 1.5 hr autonomous
**Track:** `deliverable/3/phase-6b-git-log`

### Implementation prompt

Every shell git/gh invocation in production code logs to `GitOperationLog` for forensic visibility in Drill.

**Schema:**

```sql
CREATE TABLE GitOperationLog (
  id INTEGER PRIMARY KEY,
  task_id INTEGER,
  convoy_id INTEGER,
  repo TEXT NOT NULL,
  operation TEXT NOT NULL,               -- 'fetch'|'push'|'rebase'|'force-push'|'merge'|'reset'|'worktree-add'|'gh-pr'|'gh-checks'|...
  args_json TEXT,                        -- redacted via RedactSecrets
  started_at TIMESTAMP NOT NULL,
  duration_ms INTEGER,
  exit_code INTEGER,
  stdout_excerpt TEXT,                   -- truncated to 4 KB
  stderr_excerpt TEXT,                   -- truncated to 4 KB
  branch TEXT,
  before_sha TEXT,
  after_sha TEXT
);
CREATE INDEX idx_gol_convoy ON GitOperationLog(convoy_id, started_at);
CREATE INDEX idx_gol_task ON GitOperationLog(task_id, started_at);
```

**Code:**
- `internal/git/oplog.go` (new) — `LogAndRun(ctx, db, operation, args, repo, taskID, convoyID, ...) (out, err)`. Wraps `exec.CommandContext(ctx, "git", args...)` (or `gh`), captures duration + exit + truncated stdout/stderr, redacts args, writes a GitOperationLog row.
- Refactor existing helpers: `runGitCtx`, `runGitCtxOutput`, `bestEffortRun`, `abortOp` in `internal/git/git.go` and `runShortGit`, `combinedShortGit`, `combinedShortGitArgs` in astromech helpers all flow through `LogAndRun`
- Pattern P32 asserts no direct `exec.CommandContext(ctx, "git", ...)` or `exec.CommandContext(ctx, "gh", ...)` outside the helper layer (composes naturally with existing P11 invariant)

**Anti-cheat:**
- Redaction at write time
- Stdout/stderr truncation at 4 KB each (overflow recorded in `metadata`)
- Operation parameter is a controlled enum — agent code can't fabricate operation labels

### Validation prompt

```bash
go test -tags sqlite_fts5 -run TestSchemaParity ./internal/store/...
go test -tags sqlite_fts5 -race -count=5 -run TestGitOpLog ./internal/git/...
go test -tags sqlite_fts5 -race -count=5 -run TestPattern_P32_GitOpsLogged ./internal/audittools/...
```

Manual:
- Trigger a task that produces git ops (any astromech task)
- Query:

```bash
sqlite3 ~/.force/holocron.db "SELECT id, repo, operation, exit_code, duration_ms, branch FROM GitOperationLog ORDER BY id DESC LIMIT 20;"
# Expect: rows for fetch, push, rebase, etc.
```

---

## Task 6B.3 — Drill: convoy view (timeline + inspectors + filters)

**Depends on:** 6A.8 (convoy click in Pulse), 6B.1, 6B.2
**Estimated:** 3 hr autonomous
**Track:** `deliverable/3/phase-6b-drill-convoy`

### Implementation prompt

The convoy drill page is the diagnostic backbone. Click any convoy from Pulse / Briefing / search → opens drill at `#/drill/convoy/:id`.

**Layout:**
- **Header** (full width): convoy name, status pill, ask-branch link, sub-PR link (if applicable), total spend (sum of LLMCallTranscripts.cost_usd for tasks in this convoy + any cinematic/narrative costs charged), current cycle count, attention tag toggle
- **Left 60% — chronological timeline.** Every event from:
  - TaskHistory (status transitions per task in the convoy)
  - LLMCallTranscripts (every LLM call linked to a task in the convoy)
  - GitOperationLog (every git op for the convoy)
  - ConvoyReviewCycles (cycle starts and completions)
  - Escalations (open and close events)
  - SubPRs + sub-PR CI state changes
  - Operator decisions (BriefingRenders.operator_decision)
  - Each event is a card with timestamp, actor, action, click → event drill (Task 6B.5)
- **Right 40% — inspectors:**
  - Tasks panel (every BountyBoard row for the convoy with status pills, retry counts, branch names; click → task drill)
  - Spend breakdown (per-task / per-agent / per-LLM-call grouping; pie chart)
  - Cycles panel (ConvoyReviewCycles rows with outcomes_json fully expandable)
  - Escalations (open + closed)
  - Sub-PR + CI state (live link, last check status, stall retriggers fired)

**Filters:**
- Time range slider (default: full convoy lifetime)
- Actor multiselect (Aria-3 / Captain / Council / ... — populated from events)
- Event type multiselect (LLM calls / git ops / state transitions / decisions / escalations)
- Free-text search across all transcripts + payloads in the convoy
- Filters DIM non-matching events rather than hiding them — preserves timeline shape

**Code:**
- `internal/dashboard/handlers.go`:
  - `GET /api/drill/convoy/:id` — returns the unified event stream for a convoy (joining TaskHistory, LLMCallTranscripts, GitOperationLog, ConvoyReviewCycles, Escalations, SubPRs, BriefingRenders)
  - `GET /api/drill/convoy/:id/spend` — per-task/agent/call breakdown
  - `GET /api/drill/convoy/:id/search?q=...` — server-side full-text search
- `internal/dashboard/static/drill-convoy.js` — render the layout, fetch + display events, filter handling
- `internal/store/drill_queries.go` — efficient unified-event-stream query (UNION ALL across the source tables, ordered by timestamp; pagination for very long convoys)

**Performance:**
- Convoy with 1000 events should render < 500 ms (with pagination of 200 events at a time)
- Search uses sqlite_fts5 (already enabled per CLAUDE.md tags) — index `LLMCallTranscripts(user_prompt, response_text)` and `BountyBoard(payload)` into fts5 virtual tables

### Validation prompt

```bash
go test -tags sqlite_fts5 -race -count=5 -run TestDrillConvoyHandler ./internal/dashboard/...
go test -tags sqlite_fts5 -race -count=5 -run TestDrillUnifiedEventStream ./internal/store/...
```

Manual:
- Pick an active convoy with several tasks
- Open `#/drill/convoy/<id>` in browser
- Verify all event types appear in the timeline in chronological order
- Apply actor filter (e.g., only Captain) — non-matching events dim, matching highlighted
- Apply free-text search — events matching the search term highlight
- Click into a task in the right inspector — navigates to task drill

```bash
# Performance check
time curl -sS http://127.0.0.1:8080/api/drill/convoy/47 >/dev/null
# Expect: < 500ms even for active convoys
```

---

## Task 6B.4 — Drill: task view

**Depends on:** 6B.3
**Estimated:** 2 hr autonomous
**Track:** `deliverable/3/phase-6b-drill-task`

### Implementation prompt

Click any task ID anywhere → opens `#/drill/task/:id`.

**Layout:**
- **Header.** Task ID, type, status, full payload (rendered via `.textContent`, never HTML), branch, parent task, dependencies, retry count, infra failures, reshard generation
- **Decision chain.** Captain ruling (if any) → astromech work → Council ruling → CI (if PR flow) → ConvoyReview cycle outcomes (if convoy-scoped). Each row expandable to show the full structured ruling/outcome.
- **Attempt history.** Every retry shown side-by-side. For each attempt, show:
  - What changed in the payload (scope guard added, critic notes, etc.) — visualized as a payload diff
  - What changed in the prompt version (if any)
  - What changed in the outcome (succeeded, escalated, requeued)
- **LLM transcripts.** Every Claude CLI call linked to this task, collapsed by default. Each transcript card shows: agent, prompt_version, started_at, duration, cost, expand → full system prompt + user prompt + response + tool calls.
- **Tool calls.** Inside each transcript, tool calls (Read/Edit/Bash/etc.) expandable to show args + results.
- **Git ops.** Every GitOperationLog row for this task, in order.
- **Cost rollup.** Sum + per-call breakdown.

**Code:**
- `internal/dashboard/handlers.go` — `GET /api/drill/task/:id`
- `internal/dashboard/static/drill-task.js` — render layout
- Reuse the unified event stream query from 6B.3 with task_id filter

### Validation prompt

```bash
go test -tags sqlite_fts5 -race -count=5 -run TestDrillTaskHandler ./internal/dashboard/...
```

Manual:
- Pick a task that has been through Captain → astromech → Council → CI
- Open `#/drill/task/<id>`
- Verify each section: decision chain shows all 4 stages, attempt history shows the retries, LLM transcripts expand to full prompts, tool calls expand to args + results, git ops list complete
- Cost rollup matches sum of LLMCallTranscripts.cost_usd for this task

```bash
# Sanity check
sqlite3 ~/.force/holocron.db "SELECT SUM(cost_usd) FROM LLMCallTranscripts WHERE task_id = 1234;"
# Should match the cost rollup shown in the UI
```

---

## Task 6B.5 — Drill: event view

**Depends on:** 6B.3
**Estimated:** 1.5 hr autonomous
**Track:** `deliverable/3/phase-6b-drill-event`

### Implementation prompt

Click any single event in the convoy/task timeline → opens `#/drill/event/:kind/:id`. Single-event focus view.

**Layouts (per event kind):**
- **LLM call event:** full system prompt, user prompt, response, every tool call with args + result + duration. Replay button (Task 6B.7).
- **Task transition event:** before/after task state diff, who triggered (agent/dog/operator), why (reason field if any).
- **Git op event:** command + redacted args + stdout + stderr + exit code + before/after SHAs.
- **Escalation event:** full escalation message + linked task chain + close-action audit.
- **Council/Captain ruling event:** structured ruling + linked LLMCallTranscripts row → click to drill into the call that produced it.
- **ConvoyReviewCycle event:** outcomes_json fully rendered, fix_tasks_spawned_json with click-through to each spawned task, amendments_proposed_json with operator-action history.

**Code:**
- `internal/dashboard/handlers.go` — `GET /api/drill/event/:kind/:id` (kind ∈ {llm_call, task_transition, git_op, escalation, ruling_council, ruling_captain, cycle, …})
- `internal/dashboard/static/drill-event.js` — switch on kind, render appropriate template

### Validation prompt

```bash
go test -tags sqlite_fts5 -race -count=5 -run TestDrillEventHandler ./internal/dashboard/...
```

Manual:
- From a convoy drill, click an LLM call event → opens event view with full prompt/response
- Click a git op event → shows command + output + SHAs
- Click a ruling event → shows structured ruling + link to the LLM call that produced it
- Verify XSS: payload contents render as text (`<script>` tags visible as text, not executed)

---

## Task 6B.6 — Drill: free-text search

**Depends on:** 6B.1, 6B.2, 6B.3
**Estimated:** 1 hr autonomous
**Track:** `deliverable/3/phase-6b-drill-search`

### Implementation prompt

Search input in the convoy drill view (and at top-level via the Ask shortcut from Task 6B.10) queries:
- `LLMCallTranscripts.system_prompt`, `user_prompt`, `response_text`
- `BountyBoard.payload`
- `GitOperationLog.stdout_excerpt`, `stderr_excerpt`
- `ConvoyReviewCycles.outcomes_json`, `amendments_proposed_json`
- `BriefingRenders.briefing_text`
- `OperatorEventAnnotations.note_text` (Task 6B.8)

**Implementation:**
- Build sqlite_fts5 virtual tables shadowing these columns; refresh via triggers
- Search endpoint: `GET /api/drill/search?q=<query>&kind=convoy&id=<convoy_id>` (scoped to a convoy) OR `&scope=global`
- Result format: list of `{event_kind, event_ref, snippet, score}`
- UI: result list with snippets, click → navigate to drill view of that event

**Code:**
- `internal/store/drill_search.go` (new) — fts5 setup + query helper
- Migration: create fts5 virtual tables + triggers to keep them in sync
- `internal/dashboard/handlers.go` — search endpoint
- Reuse Pulse search input as the global entry point; the dashboard Ask handler routes search-like queries here

### Validation prompt

```bash
go test -tags sqlite_fts5 -race -count=5 -run TestDrillSearch ./internal/store/...
```

Manual:
- Search "rate limit" — pulls in LLMCallTranscripts mentioning rate limits, BountyBoard payloads, etc.
- Search "AT-005" — pulls in BriefingRenders, ConvoyReviewCycles outcomes
- Verify scoped search (convoy-specific) returns only events from that convoy

```bash
# Performance
time curl -sS "http://127.0.0.1:8080/api/drill/search?q=rate+limit&scope=global" >/dev/null
# Expect: < 200ms
```

---

## Task 6B.7 — Drill: replay mode

**Depends on:** 6B.1, 6B.5
**Estimated:** 2.5 hr autonomous
**Track:** `deliverable/3/phase-6b-drill-replay`

### Implementation prompt

Replay re-runs a historical Captain ruling / Council ruling / Medic decision / ConvoyReviewCycle with the **current** prompt version, side-by-side with the original. Pure read — never mutates live state.

**Schema:**

```sql
CREATE TABLE ReplayResults (
  id INTEGER PRIMARY KEY,
  original_event_id INTEGER NOT NULL,
  original_event_kind TEXT NOT NULL,     -- 'captain_ruling'|'council_ruling'|'convoy_review_cycle'|'medic_decision'
  replay_prompt_version TEXT NOT NULL,
  replay_started_at TIMESTAMP DEFAULT (datetime('now')),
  replay_response TEXT,
  decision_changed BOOLEAN,
  cost_usd REAL,
  triggered_by_email TEXT NOT NULL
);
```

**Code:**
- `internal/agents/replay.go` (new) — `ReplayDecision(ctx, db, eventKind, eventID, currentPromptVersion) (ReplayResult, error)`. Steps:
  1. Load the original LLMCallTranscripts row (or equivalent for convoy review cycle which spans multiple calls)
  2. Re-run the LLM call with the SAME inputs but the current prompt version (loaded from `internal/agents/<agent>_prompts/`)
  3. Parse the response with the same parser
  4. Compare to the original outcome (decision changed: yes/no)
  5. Insert into ReplayResults
- Critically: replay does NOT call any code path that writes to BountyBoard, Convoys, Escalations, FleetRules, ConvoyReviewCycles, or any other live-state table. The replay path is a pure-read-and-record fork.
- `internal/dashboard/handlers.go` — `POST /api/drill/replay/:kind/:id`
- `internal/dashboard/static/drill-event.js` — Replay button on supported event kinds; on click, posts to API, polls for completion, renders side-by-side diff
- Side-by-side UI: original | replayed, with highlighted diff (use a JSON diff visualizer for structured outputs, plain text diff for prose)

**Pattern P-Replay (`TestPattern_ReplayNoMutation`):**
- Walks the replay code path
- Asserts no calls into store mutators except for `ReplayResults` insert and `LLMCallTranscripts` insert (the replay's OWN transcript)
- Direct call to `store.UpdateBountyStatus`, `store.FailBounty`, etc. in the replay path fails the test

### Validation prompt

```bash
go test -tags sqlite_fts5 -run TestSchemaParity ./internal/store/...
go test -tags sqlite_fts5 -race -count=5 -run TestReplayDecision ./internal/agents/...
go test -tags sqlite_fts5 -race -count=5 -run TestPattern_ReplayNoMutation ./internal/audittools/...
```

Manual:
- Pick a historical Captain ruling that rejected a task
- Open the event drill, click Replay
- Wait ~10s for replay to complete
- Side-by-side renders: original (rejected) | replayed (with current prompt version, possibly different outcome)
- Verify NO state in any other table changed (BountyBoard.status of the original task is unchanged)

```bash
# Verify no live state mutated
sqlite3 ~/.force/holocron.db "SELECT id, status FROM BountyBoard WHERE id = <original_task_id>;"
# Status before and after replay should be identical
```

---

## Task 6B.8 — Drill: operator annotations

**Depends on:** 6B.3, 6B.4, 6B.5
**Estimated:** 1 hr autonomous
**Track:** `deliverable/3/phase-6b-drill-annotations`

### Implementation prompt

Right-click any event in the drill timeline → "Add note" modal. Annotations carry a `flag` field: `problem` / `interesting` / `follow_up`. Surface in Reflection ("events you flagged as `problem` this month") and feed Investigator's pattern detection.

**Schema:**

```sql
CREATE TABLE OperatorEventAnnotations (
  id INTEGER PRIMARY KEY,
  operator_email TEXT NOT NULL,
  event_kind TEXT NOT NULL,              -- 'llm_call'|'task_transition'|'git_op'|'narrative'|'cycle'|'ruling_council'|...
  event_ref TEXT NOT NULL,
  note_text TEXT NOT NULL,
  flag TEXT,                             -- 'problem'|'interesting'|'follow_up'|NULL
  noted_at TIMESTAMP DEFAULT (datetime('now'))
);
CREATE INDEX idx_oea_event ON OperatorEventAnnotations(event_kind, event_ref);
CREATE INDEX idx_oea_flag ON OperatorEventAnnotations(flag, noted_at) WHERE flag IS NOT NULL;
```

**Code:**
- `internal/store/annotations.go` (new) — CRUD helpers
- `internal/dashboard/handlers.go` — `POST /api/annotations`, `GET /api/annotations?kind=<>&id=<>`, `PUT /api/annotations/:id`, `DELETE /api/annotations/:id`
- UI:
  - Right-click handler on event cards → modal with note textarea + flag radio buttons
  - Existing annotations render as a small icon next to the event card; hover to preview
  - Reflection panel "Events you've flagged" shows annotations grouped by flag

**Pattern (annotations are operator-only writes):**
- Asserts no LLM or system code path INSERTs into OperatorEventAnnotations
- The CLI command `force annotate <kind> <id> <flag> <text>` is the only non-dashboard write path; the existing P25 CLI parity covers it

**Investigator integration:**
- Annotations with `flag='problem'` are visible to Investigator as a signal source (read-only)
- Investigator can include annotated events as evidence in ProposedFeatures

### Validation prompt

```bash
go test -tags sqlite_fts5 -run TestSchemaParity ./internal/store/...
go test -tags sqlite_fts5 -race -count=5 -run TestAnnotations ./internal/store/...
go test -tags sqlite_fts5 -race -count=5 -run TestPattern_AnnotationsOperatorOnly ./internal/audittools/...
```

Manual:
- Right-click an event in convoy drill → modal opens
- Type a note, set flag=problem, save
- Verify annotation icon appears on the event card
- Hover icon → preview of note text
- Reflection panel shows the annotation grouped under "Problems flagged this month"
- Verify CLI parity: `force annotate llm_call 1234 problem "the prompt is missing context"` produces the same result

---

## Task 6B.9 — Transcript archival housekeeping dog

**Depends on:** 6B.1
**Estimated:** 1 hr autonomous
**Track:** `deliverable/3/phase-6b-archival`

### Implementation prompt

Daily housekeeping dog that bounds the SQLite size: transcripts > 30 days OR convoys closed > 7 days get summarized to a 1-line LLM-generated narrative blurb (kept in-row), bodies offloaded to `~/.force/transcripts/<year>/<month>/<id>.txt.gz`. Drill UI loads the offloaded body lazily on click.

**Code:**
- `internal/agents/dogs.go` — register `transcript-archive-housekeeping` dog (daily at 03:00 UTC)
- `dogTranscriptArchive(ctx, db)`:
  1. SELECT transcripts WHERE archived_at IS NULL AND (call_started_at < NOW - 30d OR convoy.status IN ('Completed','Cancelled') AND convoy.closed_at < NOW - 7d)
  2. For each: gzip the body to disk, write a 1-line Haiku summary into `LLMCallTranscripts.response_text` (replacing the full body), set archived_at
  3. Bounded: max 1000 archives per run; rest deferred to next run
- Drill UI checks `archived_at`; if non-null, fetches `/api/drill/transcript/:id/archived` which reads from disk

**Anti-cheat:**
- File path uses controlled template — operator-supplied paths cannot escape `~/.force/transcripts/`
- File contents pre-redacted (already done at write time per 6B.1)
- No deletes — archive is one-way until operator manually removes

### Validation prompt

```bash
go test -tags sqlite_fts5 -race -count=5 -run TestTranscriptArchive ./internal/agents/...
```

Manual:
- Force a few transcripts to be old:

```bash
sqlite3 ~/.force/holocron.db "UPDATE LLMCallTranscripts SET call_started_at = datetime('now', '-31 days') WHERE id IN (SELECT id FROM LLMCallTranscripts ORDER BY id LIMIT 10);"
```

- Trigger the dog manually (debug command `force dog-tick transcript-archive-housekeeping`)
- Verify:
  - Files appear at `~/.force/transcripts/<year>/<month>/`
  - DB rows have `archived_at` set, `response_text` replaced with summary
- Open Drill task view for one of these tasks → archived transcripts show summary line; click expand → fetches from disk and displays

---

## Task 6B.10 — Ask (`/` shortcut)

**Depends on:** 6A.1, 6B.6
**Estimated:** 2 hr autonomous
**Track:** `deliverable/3/phase-6b-ask`

### Implementation prompt

Press `/` from anywhere → floating input bar. Type a free-form question. Backend calls Haiku with the question + DB-tool access; returns a natural-language answer with cite links.

**Capabilities:**
- "what's blocking convoy #47?" → loads convoy state, in-flight tasks, escalations, returns synthesized answer with task-ID links
- "has anyone touched `internal/auth/limiter.go` in the last 2 weeks?" → searches GitOperationLog + LLMCallTranscripts for path mentions, returns list with PR refs
- "show me everything related to medic auto-cleanup" → searches FleetRules + recent ConvoyReviewCycles + LLMCallTranscripts; returns categorized result list

**Code:**
- `internal/agents/ask_handler.go` (new) — Haiku call with DB-query tools (typed read-only queries: getConvoy, getTask, searchTranscripts, listFleetRules, listEscalations, etc.)
- The agent has NO write tools — pattern asserts
- `internal/dashboard/handlers.go` — `POST /api/ask` accepts `{question, context: {current_route?}}`, returns `{answer, cite_links: [{kind, id, label}]}`
- `internal/dashboard/static/ask.js` — floating input; results card shows answer with clickable cite links
- Cost cap: `SystemConfig.ask_daily_cap_usd` (default 3.00)

**Pattern:**
- Asks-handler tools are read-only — pattern asserts no write tools registered
- Cite links must resolve to real rows (Pattern P29 for briefings extends naturally — same fuzz-test pattern)

### Validation prompt

```bash
go test -tags sqlite_fts5 -race -count=5 -run TestAskHandler ./internal/agents/...
go test -tags sqlite_fts5 -race -count=5 -run TestPattern_AskNoWriteTools ./internal/audittools/...
```

Manual:
- Press `/` → input bar appears
- Type "what's the status of convoy 47" → answer with cite link to convoy 47 drill
- Click cite link → opens drill
- Ask "show me the last 3 captain rejections" → answer with 3 cite links
- Verify NO state mutation regardless of question phrasing ("delete convoy 47" → polite refusal, no DB change)

```bash
# Cost bound
sqlite3 ~/.force/holocron.db "SELECT SUM(cost_usd) FROM LLMCallTranscripts WHERE agent='ask' AND call_started_at > datetime('now', '-1 day');"
# Expect: < 3.00 (or fallback active)
```

---

## Task 6B.11 — Reflection: calibration scoreboard

**Depends on:** 6A.10 (BriefingRenders), 6B.7 (ReplayResults), existing CalibrationAuditSamples
**Estimated:** 1.5 hr autonomous
**Track:** `deliverable/3/phase-6b-reflection-calibration`

### Implementation prompt

The Reflection tab's first section: "Your calibration scoreboard." Reads from BriefingRenders, CalibrationAuditSamples, ReplayResults, and per-agent reject-rate baseline data.

**UI components:**
- **Decision-time distribution** per agent (median, p90, count over rolling 30d). From BriefingRenders.decision_time_seconds.
- **Calibration sample accuracy.** From CalibrationAuditSamples; "32 of 35 sample decisions confirmed; 91% accuracy."
- **Trust dial coaching panel.** For each agent:
  - Current dial value
  - Reject rate (rolling 30d)
  - Expected baseline (`SystemConfig.expected_reject_rate_min`, default 0.05)
  - If reject rate < baseline: "You're trusting Captain easily — actual reject rate is 4%. Want me to surface higher-stakes Captain proposals more carefully for the next 50 decisions?" → click → adjusts trust dial down by 5
  - If sample accuracy < 85%: "Recent fast-approves on EC have been correct 7/9 times. Slow down 30s on EC?" → click → adjusts trust dial down by 5
- **Replay-driven recalibration.** "You replayed 14 historical decisions; 9 changed outcome under prompt v19. That's evidence Captain v19 is genuinely better — consider raising trust dial."

**Code:**
- `internal/dashboard/handlers.go` — `GET /api/reflection/calibration`
- `internal/store/calibration_queries.go` — pre-computed queries for the panels above
- `internal/dashboard/static/reflection-calibration.js` — render

**Pattern:**
- Suggestions are SUGGESTIONS; only operator action writes to OperatorTrustDials with `set_by='operator'`
- A "Suggest" path can write `set_by='calibration_suggestion'` rows but those don't change the current dial — they're advisory; UI shows them as "Force suggests..."

### Validation prompt

```bash
go test -tags sqlite_fts5 -race -count=5 -run TestCalibrationQueries ./internal/store/...
```

Manual:
- Make ~20 decisions in Briefing across several agents
- Replay 5 historical decisions
- Open Reflection → calibration scoreboard shows real numbers
- Verify "Lower Captain trust" suggestion appears when actual reject rate < expected baseline
- Click suggestion → dial drops by 5 → verify OperatorTrustDials row inserted with `set_by='operator'` and rationale auto-filled with the suggestion text

---

## Task 6B.12 — Reflection: fleet's learning panel

**Depends on:** existing PromotionProposals + spec_history mechanism + ConvoyReviewCycles
**Estimated:** 1.5 hr autonomous
**Track:** `deliverable/3/phase-6b-reflection-learning`

### Implementation prompt

Reflection's second section: weekly auto-rendered summary of how the fleet itself is changing. Generated Sunday night (or on-demand via "Refresh now" button).

**Content (LLM-synthesized from real DB rows):**
- "This week: 8 PromotionProposals ratified, 14 spec amendments accepted, 23 ProposedFeatures filed (5 promoted, 11 archived, 7 active)"
- "convoy-review-watch dog re-triggered 47 times; 3 convoys needed > 2 review cycles (median is 1)"
- "What's new in fleet behavior:"
  - "Captain's prompt v18 (shipped Tuesday) reduced unmapped-spawn rate from 12% → 7%. Holding."
  - "Council's new approval template (paired-run T-1147) is at +0.04 reject-rate vs control — directionally meaningful, awaiting confirmation."
  - "The `medic-no-cleanup-without-context` rule (promoted Monday) has fired 6 times, prevented 4 escalations."

**Code:**
- `internal/agents/learning_panel_renderer.go` (new) — collects raw stats over the past 7 days; calls Haiku with structured stats + change diffs; outputs the panel content
- Storage: a new `FleetLearningPanels` table with `rendered_at`, `prose`, `cost_usd`, `prompt_version`, `source_event_refs_json`
- Weekly dog: `learning-panel-render` (Sunday at 22:00 local)
- `GET /api/reflection/learning` — returns the most recent panel + a "Refresh now" trigger

**Cost cap:** ~$0.01 per render; $0.05/week max (rounded for fallback)

### Validation prompt

```bash
go test -tags sqlite_fts5 -race -count=5 -run TestLearningPanelRenderer ./internal/agents/...
```

Manual:
- Trigger the dog manually
- Open Reflection → learning panel populated with real data
- "Refresh now" button re-renders
- Verify cite refs in the panel resolve to real rows (PromotionProposals IDs, prompt versions, etc.)

---

## Task 6B.13 — Reflection: 5-min retro generator

**Depends on:** 6B.12, OperatorEventAnnotations
**Estimated:** 1 hr autonomous
**Track:** `deliverable/3/phase-6b-reflection-retro`

### Implementation prompt

Friday button in Reflection: "Run a 5-min retro." Generates a markdown post: top win, top frustration (inferred from rejections + escalations + annotations flagged 'problem'), suggested experiment for next week, draft saved to a path the operator can edit and commit to `docs/retros/<YYYY-MM-DD>.md`.

**Code:**
- `internal/agents/retro_generator.go` (new) — collects week's signal; one Haiku call to synthesize markdown
- `POST /api/reflection/retro/generate` returns `{markdown, suggested_path}`
- UI: button → markdown preview modal → "Save as draft" writes to `docs/retros/<date>.md` (operator commits manually if desired)

### Validation prompt

```bash
go test -tags sqlite_fts5 -race -count=5 -run TestRetroGenerator ./internal/agents/...
```

Manual:
- Click "Run a 5-min retro" Friday in Reflection
- Markdown preview appears with: top win, top frustration, suggested experiment, week's stats
- Verify cite refs resolve to real rows
- Save → file lands at `docs/retros/<date>.md`

---

# Cross-cutting validation

## Full-suite regression after Phase 6 lands

```bash
make test                                       # full suite, sqlite_fts5 tag
make test-audit                                 # all pattern tests
go test -tags sqlite_fts5 -race -count=5 ./...  # green no flakes

# Specific new patterns
go test -tags sqlite_fts5 -race -count=5 \
  -run "TestPattern_P25|TestPattern_P26|TestPattern_P27|TestPattern_P28|TestPattern_P29|TestPattern_P30|TestPattern_P31|TestPattern_P32" \
  ./internal/audittools/...

# Schema parity
go test -tags sqlite_fts5 -run TestSchemaParity ./internal/store/...
```

## Operator end-to-end smoke

After Phase 6B merges:

1. Open dashboard → Pulse loads with narrative panel + fleet pulse panel
2. Wait 30s → narrative panel updates with synthesized prose
3. Trigger a Captain proposal on a critical convoy → cooldown banner appears in Pulse if auto-execute kicks in
4. Open Briefing → queue shows pending decisions sorted by stakes tier; trust-dial-shifted UI tier visible
5. Click highest-stakes decision → focus mode with conversational briefing, prior-similar-decisions panel, cited evidence rendering
6. Approve via `a` → decision dispatched, BriefingRenders row updated
7. Reject one → counter-proposal forcing modal, draft alternative, routes back through Captain
8. Open Reflection → calibration scoreboard, fleet learning panel, trust dial history
9. Press `/` → ask "what's blocking convoy 47?" → synthesized answer with cite links
10. Click a cite link → opens Drill at that event
11. In Drill → timeline + inspectors render; filter by actor, search transcripts, click event → event drill with full transcript
12. Click Replay on a Captain ruling → side-by-side diff between original and replayed-with-current-prompt
13. Right-click event → annotate with flag=problem
14. Verify annotation appears in Reflection's "events flagged this month" panel
15. `force annotate llm_call <id> follow_up "review tomorrow"` → CLI parity confirmed
16. Sleep simulator: stop daemon, fake sleep gap, restart → cinematic plays in Pulse on next dashboard load
17. Trip notification budget on Investigator → past-budget notifications go to digest; verify next-day digest mail

If all 17 steps pass, Phase 6 is operator-ready.

---

## Closure-report appendix

The closure report `docs/closures/DELIVERABLE-3-CLOSURE.md` (already specified in roadmap.md) gets a new section "Dashboard implementation" listing every task above with:
- Track ID
- PR number
- Date merged
- Validation-prompt output (paste of the test results + manual smoke results)
- Any allowlist additions to pattern tests, with rationale
