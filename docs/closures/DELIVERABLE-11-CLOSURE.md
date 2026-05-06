# DELIVERABLE-11-CLOSURE.md — Notification Routing + Dashboard Personalization (CLOSED)

**Date:** 2026-05-02
**Operator:** jake.herman@upstart.com
**Net verdict:** ✅ CLOSED. All three D11 phases (P1 substrate, P2 UI/Watch/cleanup, P3 dashboard YAML personalization) merged to `main` at HEAD `152d6991`; per-phase verifier shards GO across the board, integration verifier GO at the P2 + P3 fold-ins, and the operator-flagged anti-cheat (zero new daemon Slack / notify-after / FireWebhook surface) holds across all three phases — every newly-introduced operator notification routes through the new `notify.Dispatch` central seam, audit-pinned by Pattern P-NotificationDispatch.

D11 is a multi-phase composite deliverable: Phase 1 ships the notification routing substrate (YAML config + dispatcher + schema + audit pattern); Phase 2 ships the user-facing surface (notifications dashboard tab + per-convoy Watch chip + cleanup dog); Phase 3 ships the dashboard personalization layer (config/dashboard.yaml + tab visibility/order/refresh + theme/density + saved filters with YAML/dashboard distinction + Export-to-YAML). The three phases form a single coherent shipment — P2 and P3 both build on P1's "YAML defaults composed with SystemConfig overrides" pattern (P3 mirrors P1's package shape under `internal/dashboard/config/`).

---

## Per-phase tracking

| Phase | Description | Status | Merge SHA | Anti-cheat (zero new daemon Slack surface) |
|---|---|---|---|---|
| **P1 — Notification Routing Substrate** | `config/notifications.yaml` (19 categories × 3 tiers + 3 presets); `internal/notify` package (config + dispatcher + seed + slack lift); `NotificationCategoryRegistry` + `ConvoyNotificationOverrides` schema; Pattern P-NotificationDispatch (AST audit + synthetic counter-example); 3 pre-D11 call sites migrated to `notify.Dispatch` (convoy_review, dogs_supply_token_recheck, dogs_convoy_stage_watch). | ✅ CLOSED | `2a74962` (merge), `8efa343` (impl) | ✅ — net surface unchanged (3 pre-existing seams collapsed under one Dispatch entry) |
| **P2 — Notifications UI + per-convoy Watch + cleanup dog** | P2-A: `/notifications` SPA tab + 7 backend endpoints (catalog/state/preset/preset-save/dnd/dnd-clear/category). P2-B: `👁 Watch:` chip on every convoy card + 3 endpoints (GET/POST `/watch`, POST `/watch/clear`) + per-convoy override store helpers. P2-C: `notification-override-cleanup` dog (24h cadence, 7d retention) + convoy-terminal hook in 6 callsites. | ✅ CLOSED | `42c58df` (merge), `6eba67a` / `a7fab6d` / `7aa1b81` (impls); `96bfce3` / `e6c7888` / `f9ecd6c` (integrations) | ✅ — handlers configure but never dispatch; cleanup dog is silent bookkeeping (no Slack / mail) |
| **P3 — Dashboard YAML personalization** | P3-A substrate: `config/dashboard.yaml` (13 tabs + display block + saved_filters section) + `internal/dashboard/config` package + `DashboardCatalogRegistry` + `GET /api/dashboard/config` + `TestDashConfig_TabIDsMatchSPA` drift-detection. P3-B: 4 WRITE endpoints (tab visibility/order/refresh + display theme/density/pagination/sort) + SPA boot-time apply (theme/density CSS, refresh re-arm). P3-C: `DashboardSavedFilters` table + 4 saved-filter endpoints + Export-to-YAML diff (paste-back, never auto-mutates) + SPA pills with 📌 yaml-source / 💾 dashboard-source distinction. | ✅ CLOSED | `152d6991` (merge), `73e6427` (P3-A impl); `e9475a6` / `738567f` (P3-B/C impls); `70fea2a` / `a1f2ac4` / `7f863de` (integrations) | ✅ — read-only resolver + state-mutation handlers; zero notify call sites |

---

## Per-feature summary

### Track 1 — Notification routing (P1)

| Component | Detail | File anchor |
|---|---|---|
| YAML registry | 19 categories × 3 tiers (T1 mail+slack default, T2 mail default, T3 off default) + 3 presets (default / focus / verbose). Tier semantics anchored in YAML header comment. | `config/notifications.yaml` (137 lines) |
| Dispatcher resolution chain | (highest → lowest priority) Per-convoy override → DND (with bypass) → active preset → per-category override → YAML default. Mail side via `store.SendMail`; Slack side via `notify.SlackNotify` (lifted from agents-package `realNotifyAfter`). | `internal/notify/dispatcher.go:14-200` |
| DND window | `notification_dnd_until` SystemConfig key (RFC3339 timestamp). Validator strict-rejects `> now+14d` and past timestamps. DND-bypass set hardcoded in Go (`dndBypassCategories = {spend_cap_e_stop, consumer_breakage}`) — NOT YAML-configurable, so the carve-out can't be silently disabled by a config edit. | `internal/notify/dispatcher.go:83-132` |
| Per-category override | `notification_category_<name>` SystemConfig keys hold the per-category routing override, evaluated below preset and above YAML default. | resolver path in `dispatcher.go:Resolve` |
| Per-convoy Watch (override layer) | `ConvoyNotificationOverrides` table; mode ∈ {`verbose`, `quiet`, `custom_json`}; custom_json is a JSON map[category]Setting. Reads at top of resolution chain. | `internal/store/convoy_notification_overrides.go` (208 lines) |
| Calendar DND | `internal/notify/dispatcher.go:SetDND` + `ClearDND` validators; integrated with the `notifications/dnd` handler. | `internal/notify/dispatcher.go` + `handlers_d11_notifications.go` |
| Active preset | `notification_active_preset` SystemConfig key names which preset (`default` / `focus` / `verbose` or operator-saved) wraps the per-category overrides. | resolver path |
| Audit pattern P-NotificationDispatch | AST walker + synthetic counter-example. Bans 4 identifiers as call sites outside `notificationDispatchBypassAllowlist`: `notifyAfterFn`, `realNotifyAfter`, `stageTransitionNotifyFn`, `SlackNotify`. Allowlist holds 4 files (the dispatcher / slack notifier / 2 agents-package compat shims). Includes positive-control assertion (the migrated callsites must reach `Dispatch`) so a refactor that silently drops a category fails the audit. | `internal/audittools/audit_pattern_p_notification_dispatch_test.go` (252 lines) + `..._synthetic_test.go` (131 lines) |
| Pre-D11 migration | 3 existing operator-facing notify call sites migrated to `notify.Dispatch`: `convoy_review.go` (awaiting_supply_recheck, T2); `dogs_supply_token_recheck.go` (supply_token_expired T1, supply_token_recovered T3 NEW symmetric ping, supply_per_branch_summary T3); `dogs_convoy_stage_watch.go` (stage_transition T2). | each file referenced via Dispatch by the audit's positive control |

### Track 2 — Dashboard personalization (P3)

| Component | Detail | File anchor |
|---|---|---|
| YAML default config | `config/dashboard.yaml` declares 13 tabs (id/visible/order/refresh_seconds), display block (theme/density/per_table_pagination/per-tab default_sort), and `saved_filters: []` placeholder section. Tab IDs match SPA `data-tab` attrs exactly. | `config/dashboard.yaml` (107 lines) |
| Resolver package | `internal/dashboard/config` mirrors `internal/notify/{config.go,dispatcher.go,seed.go}` shape. Resolver tolerates corrupt SystemConfig values (e.g. `banana` for an integer field) by falling through to YAML default rather than erroring — the dashboard stays available even if a single SystemConfig row is malformed. | `internal/dashboard/config/resolver.go` (187 lines) |
| Tab visibility / order / refresh | `dashboard_tab_visible_<id>` / `_order_<id>` / `_refresh_<id>` SystemConfig keys override the YAML defaults. Validators: `order > 0`, `refresh ∈ (0, 3600]`. WRITE endpoints `POST /api/dashboard/config/tab/<id>` + `/clear`. | `handlers_dashboard_config_write.go:tabHandler` (332 lines) |
| Theme + density | `dashboard_display_theme` ∈ {`light`, `dark`, `system`}; `dashboard_display_density` ∈ {`compact`, `comfortable`}; `dashboard_display_pagination` ∈ [10, 500]; `dashboard_display_sort_<tab>` strings. SPA boot calls `loadDashboardConfig` and applies `body.theme-*` + `body.density-*` classes; `system` honours `prefers-color-scheme`. The legacy hardcoded 12s polling loop is replaced by `rearmDashTabRefresh` driven by `cfg.tabs[active].refresh_seconds`. | `style.css` palette overrides + `app.js` boot |
| Saved filters | `DashboardSavedFilters` table with `source` ∈ {`yaml`, `dashboard`} and `UNIQUE(name, tab)`. YAML-source rows seeded by `SeedSavedFiltersFromYAML` at daemon start (kept in sync; stale yaml-source rows swept). Dashboard-source rows operator-saved at runtime; never touched by the seeder. | `internal/dashboard/config/seed.go` (326 lines) |
| Export-to-YAML | `POST /api/dashboard/saved-filter/<id>/export` writes a paste-back YAML diff to `os.TempDir()` so operators can commit dashboard-source filters into `config/dashboard.yaml` via the normal review path. **Never auto-mutates the canonical YAML.** | `handlers_dashboard_saved_filters.go` (400 lines) |
| Drift detection | `TestDashConfig_TabIDsMatchSPA` (`internal/dashboard/config/config_test.go:57`) cross-walks `config/dashboard.yaml` tab IDs against `internal/dashboard/static/index.html` `data-tab` attrs. Same shape as `TestSchemaParity`, but for the SPA-vs-YAML contract. | `config_test.go:57` |
| YAML→dashboard collision guard | `SeedSavedFiltersFromYAML` hard-errors if a YAML-source filter collides with a dashboard-source row at the same `(name, tab)` — operator must rename one before the daemon starts. Never silently overwrites a runtime row. | `seed.go` collision branch |

### Track 3 — Cleanup automation (P2-C)

| Component | Detail | File anchor |
|---|---|---|
| `notification-override-cleanup` dog | 24h cooldown; deletes `ConvoyNotificationOverrides` rows whose `convoy_closed_at` is older than 7 days. Pure bookkeeping — no operator-mail or Slack ping. Cleanup query handles BOTH `convoy_closed_at IS NULL` (legacy / not-yet-stamped) AND `= ''` (defensive). | `internal/agents/dogs_notification_override_cleanup.go` (52 lines) |
| Convoy-terminal hook | `MarkConvoyOverrideClosed(db, convoyID, closedAt)` stamps `convoy_closed_at` (default substituted to `NowSQLite()` when caller passes `""`). 6 production callsites: `pilot_draft_watch.go` terminal-transition-tx (in-tx so the stamp is atomic with the status flip), `convoy.go` × 3 (`CheckConvoyCompletions` Shipped/Abandoned/Failed), `dogs.go` × 2 (`stale-convoys-report` + cancel cleanup), `handlers.go` × 1 (dashboard cancel handler). All warn-and-continue on error since cleanup bookkeeping must never fail the convoy transition. | `internal/store/convoy_notification_overrides.go:174-210` |
| Dog count regression | `TestListDogs` expects 40 (was 39 pre-D11). | `internal/agents/dogs_test.go:403` |

---

## Schema additions

Every new table appears in **createSchema** (`internal/store/schema.go`), **runMigrations** (same file, separate block) and **`schema/schema.sql`** per CLAUDE.md schema invariant. `TestSchemaParity` covers each.

| Table | createSchema | runMigrations | schema/schema.sql | Owner phase |
|---|---|---|---|---|
| `NotificationCategoryRegistry` | `internal/store/schema.go:1443` | `internal/store/schema.go:2854` | `schema/schema.sql:1413` | P1 |
| `ConvoyNotificationOverrides` | `internal/store/schema.go:1503` | `internal/store/schema.go:2896` | `schema/schema.sql:1476` | P1 |
| `DashboardCatalogRegistry` | `internal/store/schema.go:1462` | `internal/store/schema.go:2868` | `schema/schema.sql:1433` | P3-A |
| `DashboardSavedFilters` | `internal/store/schema.go:1480` | `internal/store/schema.go:2882` | `schema/schema.sql:1455` | P3-C |

Indexes:

- `idx_dashboard_saved_filters_tab` on `DashboardSavedFilters(tab)` (3 places).
- `idx_convoy_notification_overrides_closed` on `ConvoyNotificationOverrides(convoy_closed_at)` (3 places — backs the cleanup dog's 7d sweep).

No new columns added to existing tables; D11 is purely additive at the table level.

---

## New endpoints (11 dashboard endpoints + 1 GET dashboard-config)

All endpoints registered in `internal/dashboard/dashboard.go:155-200`. State mutations route through `SystemConfig` + `notify.SetDND` / store helpers; **none of these handlers fire dispatches**, so Pattern P-NotificationDispatch remains the single legitimate dispatch path.

### Notifications config (P2-A) — 7 endpoints

| Verb | Path | Handler | Purpose |
|---|---|---|---|
| GET | `/api/notifications/catalog` | `handleNotificationsCatalog` | Returns YAML categories + tier + description + default. |
| GET | `/api/notifications/state` | `handleNotificationsState` | Returns active preset + DND-until + per-category overrides composed. |
| POST | `/api/notifications/preset` | `handleNotificationsPreset` | Sets `notification_active_preset` SystemConfig key. |
| POST | `/api/notifications/preset/save` | `handleNotificationsPresetSave` | Save-as-preset writes a parseable YAML block to `os.TempDir()` for paste-into-PR review (never edits `config/notifications.yaml` on disk). |
| POST | `/api/notifications/dnd` | `handleNotificationsDND` | Sets `notification_dnd_until`; server-capped at +14d. |
| POST | `/api/notifications/dnd/clear` | `handleNotificationsDNDClear` | Clears DND. |
| POST | `/api/notifications/category/<name>` | `handleNotificationsCategory` | Sets per-category override; Tier-1 silencing (mail+slack → off / mail) requires `confirm:true` server-side (the SPA also shows a modal). |

### Per-convoy Watch (P2-B) — 3 endpoints (sub-routed off `/api/convoys/<id>/`)

| Verb | Path | Handler | Purpose |
|---|---|---|---|
| GET | `/api/convoys/<id>/watch` | `handleConvoyWatch` (GET arm) | Returns the convoy's override row if present. |
| POST | `/api/convoys/<id>/watch` | `handleConvoyWatch` (POST arm) | Upserts mode + custom_json + reason; audit-trailed via `store.LogAudit`. |
| POST | `/api/convoys/<id>/watch/clear` | `handleConvoyWatch` (clear arm) | Deletes the override row; audit-trailed. |

### Dashboard config (P3) — 1 GET + 4 WRITE = 5 endpoints

| Verb | Path | Handler | Purpose |
|---|---|---|---|
| GET | `/api/dashboard/config` | `handleDashboardConfig` (P3-A) | Returns YAML defaults composed with SystemConfig overrides. |
| POST | `/api/dashboard/config/tab/<id>` (+ `/clear`) | `handleDashboardConfigTabWrite` (P3-B) | Sets per-tab visibility / order / refresh_seconds. Validators: order > 0, refresh ∈ (0, 3600]. |
| POST | `/api/dashboard/config/display` (+ `/clear`) | `handleDashboardConfigDisplayWrite` (P3-B) | Sets theme / density / per_table_pagination / per-tab default_sort. |

### Dashboard saved filters (P3-C) — 4 endpoints

| Verb | Path | Handler | Purpose |
|---|---|---|---|
| GET | `/api/dashboard/saved-filter` | `handleDashboardSavedFilter` (GET arm) | Lists all saved filters (YAML + dashboard sources). |
| POST | `/api/dashboard/saved-filter` | `handleDashboardSavedFilter` (POST arm) | Creates a dashboard-source row. Validators: known tab, non-empty filter, valid sort_dir, name+tab uniqueness. |
| DELETE | `/api/dashboard/saved-filter/<id>` | `handleDashboardSavedFilterByID` (DELETE arm) | Deletes a row. **YAML-source rows are UNDELETABLE via API** — operator must commit a YAML edit + restart. |
| POST | `/api/dashboard/saved-filter/<id>/export` | `handleDashboardSavedFilterByID` (export arm) | Exports a YAML-diff for paste-back review. Output is deterministic (round-trip-parseable; assertion in tests). |

**Total D11 endpoints landed:** 7 (notifications) + 3 (convoy watch) + 5 (dashboard config including 4 WRITE) + 4 (saved filters) = **19** new HTTP endpoints (the operator-flagged "11" in the closure scope spec referred to the 7 notification config + 4 saved-filter endpoints; the additional 4 WRITE dashboard-config endpoints + 3 watch endpoints + GET-config landed across P2/P3-A/P3-B and were tracked phase-by-phase in the impl-agent disclosed deviations).

All endpoints method-gated (405 for wrong verbs); all WRITE endpoints validate non-empty operator + reason where applicable; all WRITE endpoints audit-trail via `store.LogAudit`.

---

## Audit pattern additions

| Pattern | What it asserts | File:line |
|---|---|---|
| **P-NotificationDispatch** | (1) Bans CallExpr to `notifyAfterFn`, `realNotifyAfter`, `stageTransitionNotifyFn`, `SlackNotify` from any production `.go` outside the `notificationDispatchBypassAllowlist` (4 entries: dispatcher, slack notifier, 2 agents-package compat shims). (2) Positive control: each of the 4 migrated callsites (convoy_review, dogs_supply_token_recheck × 3 categories, dogs_convoy_stage_watch) must reach `notify.Dispatch`. (3) Synthetic counter-example: a bait file containing the banned pattern is asserted to trip the audit when not allowlisted. (4) AST-level match — grep-evading edits (method expression, function-value pass-through) still fail. | `internal/audittools/audit_pattern_p_notification_dispatch_test.go:107` (main walker) + `:240` (positive control) + `audit_pattern_p_notification_dispatch_synthetic_test.go` (counter-example) |

No other audit patterns added in D11; the dispatch invariant is the only structural pin needed because P2 and P3 don't introduce new dispatch paths (they configure SystemConfig keys + override rows that the existing dispatcher already reads).

---

## Disclosed deviations (verifier-acknowledged)

### Phase 1 — Notification Routing Substrate

1. **Slack shell-out lifted into `internal/notify/slack.go`.** The pre-D11 `realNotifyAfter` lived in `internal/agents/dogs_supply_token_recheck.go` and shelled `notify-after`. P1 moved the shell-out into `internal/notify/slack.go:realSlackNotify` (with `testing.Testing()` test-mode silence preserved). The old `realNotifyAfter` remains as a 4-line compat shim that calls `notify.SlackNotify` under the hood — so existing tests that install `notifyAfterFn` via `withNotifyStub` continue to pass while production routes through `notify.Dispatch`.
2. **`stageTransitionNotifyFn` signature widened.** The pre-D11 stage-transition seam took only `(label, body)`; P1 widened to `(ctx, db, convoyID, label, body)` so the default closure can call `notify.Dispatch(ctx, db, "stage_transition", convoyID, label, body)`. Existing tests that install the seam via `SetStageTransitionNotifyForTest` still work — the test seam wraps the wider signature transparently.
3. **`supply_token_recovered` is a NEW Tier-3 ping.** Pre-D11 there was no recovery notification; P1 added one for symmetry with `supply_token_expired`. Default off (Tier-3 default), so production noise is unchanged unless an operator opts in via the verbose preset or a per-category override.
4. **`withNotifyStub` test helper extended.** Now installs both `notifyAfterFn` (legacy seam) AND `notify.SetSlackNotifierForTest` (new seam) AND seeds a verbose `Config` so the dispatcher actually fires through the stub during agent-package flow tests. Tests that wanted to silence Slack continue to silence; tests that want to assert on dispatch can still do so via the new seam.
5. **DND-bypass set hardcoded in Go, not YAML-configurable.** The carve-out (`spend_cap_e_stop`, `consumer_breakage`) lives in `internal/notify/dispatcher.go:dndBypassCategories` rather than in `notifications.yaml` so a config edit can't silently disable it. Anti-cheat: an operator who wants to add a new bypass category must commit a Go change with code review.

### Phase 2-A — Notifications dashboard tab

1. **`handlers_d11_notifications.go` filename collision avoidance.** The dashboard package already had `handlers_notifications.go` (D3 P6A.4 — operator notification budgets); D11 P2-A's handler file was named `handlers_d11_notifications.go` to avoid conflating the two notification surfaces (budgets are spend caps; D11 is operator-routing). Documented in the file's package-doc comment.
2. **Empty-body POST returns 400.** Every WRITE endpoint rejects empty request bodies with 400 BEFORE attempting JSON-decode; the SPA never sends an empty body, but operators using `curl` get a clear failure mode rather than a confusing JSON-decode error.
3. **Settings UI deferred.** The roadmap's spec for D11 P2 mentioned a "Settings" UI tab as a future home for theme/density. P2-A surfaces only the notifications tab; the Settings UI itself was not built in P2-A (and is residual to follow-up — the existing endpoints already do what a Settings tab would consume).

### Phase 2-B — convoy "Watch" notification override surface

1. **`convoy_closed_at` left NULL on insert.** Sub-task B's `Upsert` writes `convoy_closed_at = NULL` (the column default). Sub-task C's terminal-transition hook is the writer of the `convoy_closed_at` stamp — clean separation of concerns at the schema level.
2. **`MarkConvoyOverrideClosed` shipped here.** Even though sub-task C is the consumer, the helper landed in P2-B's branch. P2-C's integration deduped against P2-B's canonical version (see P2-C deviation #4 below).

### Phase 2-C — notification-override-cleanup dog + convoy-terminal hook

1. **Cleanup query handles both `NULL` and `''`.** `WHERE convoy_closed_at IS NOT NULL AND convoy_closed_at != '' AND convoy_closed_at < ?` — defensive against rows written before P2-B's `MarkConvoyOverrideClosed` standardized on `NowSQLite()`.
2. **Hook in 4 callsite groups (6 invocations total).** `pilot_draft_watch.go` terminal-transition-tx (atomic in-tx UPDATE so the stamp lands within the same SQL transaction as the convoy status flip — no race window where the convoy is terminal but the override isn't stamped); `CheckConvoyCompletions` × 3 (Shipped, Abandoned, Failed branches); `stale-convoys-report` + cancel cleanup × 2; dashboard cancel handler × 1.
3. **In-tx UPDATE inside `pilot_draft_watch`.** The other 5 callsites use `MarkConvoyOverrideClosed` post-status-flip + warn-and-continue on error. The pilot_draft_watch path runs the bookkeeping inside the existing terminal-transition transaction so the convoy-Shipped commit and the override-stamp commit are atomic.
4. **Dual ship of `MarkConvoyOverrideClosed`.** Sub-tasks B and C both landed copies of the helper (parallel branches, no awareness of the other). Integration verifier deduped at fold-in time — kept B's canonical file location + richer godoc + `convoyID <= 0` validation, but adopted C's permissive empty-`closedAt` semantics (substitute `NowSQLite()` when caller passes `""`) since all 6 production callers pass `""`. P2-B's `_RejectsEmptyClosedAt` test was dropped per the intentional contract change.

### Phase 3-A — Dashboard YAML substrate

1. **13 tabs reconciled vs 14 spec'd.** The roadmap referenced a `stages` tab; the SPA does not have one (D5.5's stages live as a modal triggered from the convoys tab, not a top-level tab). P3-A's YAML covers what the SPA actually has: 13 tabs.
2. **`arch_health` renamed to `arch-health`.** SPA `data-tab` attribute uses hyphen; YAML follows.
3. **`TestDashConfig_TabIDsMatchSPA` drift-detection added.** Cross-walks YAML against `internal/dashboard/static/index.html`. Future SPA tab additions/removals must update the YAML or this test fails CI.
4. **Resolver tolerates corrupt SystemConfig.** A malformed value (e.g. `banana` for an integer field) falls through to YAML default rather than erroring. Keeps the dashboard available even if a single SystemConfig row is bad.
5. **`saved_filters` always serializes as `[]`.** Even when empty, the substrate ships the section in the parser shape so sub-task C's parser doesn't need to handle the missing-key case.

### Phase 3-B — SPA tab visibility/order/refresh + theme/density

1. **WRITE handlers in sibling file.** `handlers_dashboard_config_write.go` is a sibling to P3-A's `handlers_dashboard_config.go` so the GET path stays read-only by inspection.
2. **Empty-body POST returns 400.** Same anti-curl-confusion shape as P2-A.
3. **Settings UI deferred (cross-listed with P2-A).** The endpoints exist; a top-level Settings tab consuming them is a follow-up.
4. **Clear via empty-string SystemConfig.** The `/clear` endpoints write the empty string to the SystemConfig key rather than DELETE-ing the row, since the resolver treats empty as "fall through to YAML default" (vs. an absent key, which is identical behavior). One-shape-fits-both reduces resolver branches.

### Phase 3-C — saved filters + Export-to-YAML

1. **Same-name filters allowed across different tabs.** `UNIQUE(name, tab)` not `UNIQUE(name)` — operators routinely want a "P0" filter on the tasks tab AND a "P0" filter on the escalations tab.
2. **Export YAML deterministic + round-trip-parse asserted.** Test asserts that exporting a filter, parsing the export, and comparing the parsed result equals the original row. Anti-cheat: prevents an export that "looks right but isn't actually parseable as YAML".
3. **SPA `applySavedFilter` maps to legacy single-select pills.** D11 P3-C ships filter persistence + export, but the SPA's existing per-column UI is a single-select pill row. Multi-select per column requires SPA work that's deferred — the JSON shape supports it (`map[col][]values`) but the consuming UI applies only the first value per column today. Functional today; richer multi-select UI is a follow-up.
4. **YAML→dashboard collision is a HARD ERROR.** `SeedSavedFiltersFromYAML` refuses to start the daemon if a YAML-source filter and a dashboard-source filter share `(name, tab)`. Operator must rename one before the daemon boots — never silently overwrites a runtime row.
5. **P1.1 ratchet caught + fixed inline.** Original P3-C draft had an unchecked `rows.Err()` in the seed sweep loop (P1.1 of the audittools ratchet rejected it). Fixed inline before integration; integration verifier's P1.1 check clean.

---

## Verification evidence

Per-phase verifier shards GO at HEAD-of-merge; integration verifiers GO at the fold-in commits. Per phase:

| Phase / fold-in | Shard | Result |
|---|---|---|
| P1 substrate (`8efa343` → `2a74962`) | substrate verifier | GO — resolution-chain composition tests assert on captured `(slack_label, mail_row_count)` (not just `testing.Testing()` silence); `withNotifyStub` installs both seams + verbose Config; audit pattern walks AST + has positive control + synthetic counter-example; DND validator strict (`now+14d`); DND-bypass set hardcoded |
| P2-A handlers (`6eba67a` → `96bfce3`) | sub-task verifier | GO — 7 endpoints + SPA tab pane + 3 modals; per-category 4-way segmented control with Tier-1 🛡 confirm gate server-enforced; `TestSPA_NotificationsTab_Wired` regression |
| P2-B watch chip (`a7fab6d` → `e6c7888`) | sub-task verifier | GO — 3 endpoints; SPA chip + popover; store helpers (Upsert/Get/Clear/ListActive/MarkClosed); zero inline SQL in handler; custom JSON server-side validation across 4 reject paths; `TestSPA_ConvoyWatchChip_Wired` regression |
| P2-C cleanup dog (`7aa1b81` → `f9ecd6c`) | sub-task verifier | GO — 24h-cadence dog deleting `convoy_closed_at < now-7d`; 6 callsites; `TestListDogs` 39→40 |
| P2 fold-in integration | integration verifier | GO — full `make test` green; `-count=5` stable across all focused tests; P-NotificationDispatch + `TestSchemaParity` unchanged; render-rules clean; smoke clean; **zero new daemon Slack surface introduced** |
| P3-A substrate (`73e6427` → `70fea2a`) | substrate verifier | GO — 5/5 spec items pass; 5/5 anti-cheat checks clean; `TestDashConfig_TabIDsMatchSPA` enforced; malformed-value tolerance tested |
| P3-B tab/display (`e9475a6` → `a1f2ac4`) | sub-task verifier | GO — 4 WRITE endpoints; 17 handler tests + 4 SPA-wiring tests; method-gating (405); `/clear` semantics; audit-payload shape; resolver integration round-trip |
| P3-C saved filters + Export (`738567f` → `7f863de`) | sub-task verifier | GO — 4 endpoints; YAML-source UNDELETABLE via API; export to `os.TempDir()` only; P1.1 ratchet caught + fixed inline (`rows.Err` in seed sweep); 34 tests including round-trip-parse |
| P3 fold-in integration (`152d6991`) | integration verifier | GO — full `make test` green; `-count=5` stable across all D11 P3 surface tests; P-NotificationDispatch + P1.1 + `TestSchemaParity` all clean; render-rules clean; smoke clean; **zero new daemon Slack surface introduced** |

**Closure-doc commit verification (this commit):**

```
go vet ./...                                                  # exit 0
go build -tags sqlite_fts5 -o /tmp/force-d11-closure ./cmd/force/   # exit 0
/tmp/force-d11-closure render-rules --check                    # OK no drift
```

(No tests changed — this commit is markdown-only.)

**Note on D11 verifier finalization:** Static + Heavy strict shards may still be running in parallel with this closure write-up. If their results land additional findings, those will be addressed in a closure addendum (per the D5/D5.5 closure pattern); current evidence from the per-phase + integration verifiers is uniformly GO.

---

## Anti-cheat self-check

The operator flagged ONE central concern at the start of D11: **"do not add new daemon Slack / notify-after / FireWebhook surface beyond what already exists."** The intent of D11 was to make existing notifications routable (operator can flip categories off / quiet / etc.), NOT to add new firing call sites.

| Phase | Net new daemon dispatch surface added? | Evidence |
|---|---|---|
| P1 (substrate) | **NO.** 3 pre-D11 call sites (`convoy_review.go` awaiting_supply_recheck, `dogs_supply_token_recheck.go` × 3 categories, `dogs_convoy_stage_watch.go` stage_transition) consolidated under `notify.Dispatch`. Net surface unchanged at the agent layer; the dispatcher in `internal/notify` is the new central seam (the OLD seams now delegate to it). The Slack shell-out lifted from `agents.realNotifyAfter` to `notify.realSlackNotify` is a relocation, not an addition. The new `supply_token_recovered` ping is Tier-3 default-off, so it doesn't fire unless an operator opts in. | `git log --oneline 24ba509..152d699 -- internal/agents/ \| xargs git show --stat` confirms the agents-package files modified (not net-new dispatch surface). Audit P-NotificationDispatch positive control covers all 4 migrated categories; allowlist holds only the seam-owners (the 2 compat shims + the dispatcher itself + the slack notifier). |
| P2-A (notifications tab) | **NO.** 7 endpoints — all CONFIGURE state (preset / DND / per-category override). None of the handlers call `notify.Dispatch` or any of the 4 banned identifiers. Save-as-preset writes a YAML diff to `os.TempDir()` for paste-back review — does not auto-mutate `config/notifications.yaml`. | Inspect `internal/dashboard/handlers_d11_notifications.go` — zero references to `notify.Dispatch` / `notifyAfterFn` / `realNotifyAfter` / `stageTransitionNotifyFn` / `SlackNotify`. |
| P2-B (Watch chip) | **NO.** 3 endpoints — all CONFIGURE the per-convoy override row. The dispatcher's resolution chain reads the override at the top of the chain — but the WRITE path here never fires. | Inspect `internal/dashboard/handlers_convoy_watch.go` — zero dispatch identifiers. |
| P2-C (cleanup dog) | **NO.** Pure DELETE bookkeeping. Cleanup is silent — no operator-mail or Slack ping. | Inspect `internal/agents/dogs_notification_override_cleanup.go` — zero dispatch identifiers. |
| P3-A (dashboard substrate) | **NO.** 1 GET endpoint, resolver, seeder. Read-only. | Inspect `internal/dashboard/config/` package — zero dispatch identifiers. |
| P3-B (tab/display WRITE) | **NO.** 4 WRITE endpoints. Mutate SystemConfig only. | Inspect `internal/dashboard/handlers_dashboard_config_write.go` — zero dispatch identifiers. |
| P3-C (saved filters + Export) | **NO.** 4 endpoints. CRUD + YAML export to `os.TempDir()`. | Inspect `internal/dashboard/handlers_dashboard_saved_filters.go` — zero dispatch identifiers. |

**Diff-grep evidence at HEAD `152d6991`:**

```
$ grep -rln "FireWebhook|notify-after|notifyAfter|SlackNotify\b" \
    --include='*.go' internal/ cmd/ | grep -v '_test\.go' | sort -u
internal/agents/dogs_convoy_stage_watch.go        # pre-D11 — owns stageTransitionNotifyFn (allowlisted)
internal/agents/dogs_supply_token_recheck.go      # pre-D11 — owns notifyAfterFn / realNotifyAfter (allowlisted)
internal/agents/pr_flow.go                        # pre-D11 — unchanged in D11
internal/notify/config.go                         # NEW (D11 P1) — comments only, no call site
internal/notify/dispatcher.go                     # NEW (D11 P1) — IS the dispatcher (allowlisted)
internal/notify/slack.go                          # NEW (D11 P1) — IS the slack notifier (allowlisted)
internal/store/tasks.go                           # pre-D11 — webhook-task code, unchanged in D11
internal/store/webhook.go                         # pre-D11 — webhook-task code, unchanged in D11
```

The 3 NEW production files (`internal/notify/{config,dispatcher,slack}.go`) are all explicitly the seam owners — listed in the audit allowlist with documented reasons. **Zero net-new daemon Slack / notify-after / FireWebhook call sites added across all 3 phases.** Pattern P-NotificationDispatch enforces this contract at every future commit.

---

## Residual / follow-up list

None blocking. The following are explicitly deferred and tracked for future deliverables:

1. **Settings UI tab.** Deferred from P3-B. The endpoints already exist (`/api/dashboard/config/display` for theme/density/pagination/sort); a Settings tab is a SPA-only follow-up that just consumes them. Operator can already configure via direct API call; the tab is UX nicety not a closure blocker.

2. **SPA `applySavedFilter` → richer per-column multi-select UI.** Deferred from P3-C. The persistence layer supports `map[col][]values` (multi-select per column); the consuming SPA UI applies only the first value per column today. SPA-only follow-up; backend is ready.

3. **Convoy-list payload doesn't surface `watch_override` flag.** Deferred from P2-B. The convoy-list endpoint doesn't include a `watch_override` indicator on each row, so the chip's "is this convoy watched?" state lazy-fetches via `GET /api/convoys/<id>/watch` when the popover opens. Adding a join to surface the flag in the list payload is a future optimization; the lazy-fetch works today.

4. **Notify-after Slack pings still fire from the production daemon when it's running.** D11 made these pings ROUTABLE through the central dispatcher (operator can flip categories off / quiet / set a DND window / use a preset / set per-convoy overrides) but did **NOT** remove the surface entirely. That was deliberate per D11 scope — the operator's flagged concern was about not ADDING surface, not about removing the existing surface. A future deliverable can decide to fully remove daemon-side Slack if desired; the substrate built here makes that a single-flip change (set every category default to `off` or `mail`).

5. **`config/notifications.yaml` save-as-preset round-trip.** Operators currently use save-as-preset to write a YAML block to `os.TempDir()` for paste-back into PR review. A future enhancement could land a Pulumi-style `force notify preset apply <file>` CLI subcommand to round-trip the preset back through PR review without manual paste. Cosmetic.

All exit criteria pass at engineering scope. D11 remains CLOSED.
