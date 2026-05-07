---
audience: operator
scope: D9 monthly architecture-health report — `dogArchitectureHealthReport` aggregation + render pipeline + dashboard surface; the longitudinal companion to the on-demand Archaeologist agent.
owner: D9
last_reviewed: 2026-05-07
---

# Architecture health report

The architecture-health report is the fleet's monthly longitudinal heartbeat — one rendered Markdown file per month at `reports/architecture-health-YYYY-MM.md`, plus a dashboard tab that lets the operator browse historical months. It answers the question "is the codebase getting healthier or sicker over time, broken down by author class (human / Astromech / archaeologist-migration)?" by walking every Bureau-of-Standards rule across every registered repo and aggregating violation counts per `(rule_id, repo_id, author_type, report_month)` tuple.

This subsystem is the **report pipeline + dashboard surface** half of D9. The on-demand qualitative complement — the operator-gated Archaeologist agent — lives at [`docs/agents/archaeologist.md`](../agents/archaeologist.md) and [`docs/subsystems/archaeologist.md`](archaeologist.md). The two surfaces share neither a code path nor a cadence: the Archaeologist is operator-triggered debt-pattern detection writing to `ArchaeologistFindings`; arch-health is dog-fired BoS-rule aggregation writing to `ArchHealthAggregates`. Both ship under D9; both are independent.

## Overview

Once per month (30-day cooldown — no calendar-day gate, see invariant #4) the dog:

1. Walks every registered repo (skipping repos with empty / unreadable `local_path`).
2. Runs every BoS rule (BOS-001..BOS-011) against the repo's working tree.
3. Classifies each violation's author by path heuristic: paths matching `*astromech*` → `astromech`; `*/migrations/*` or `*archaeologist_*` → `archaeologist-migration`; everything else → `human` (v1 ships path-heuristic; v2 swaps to git-blame).
4. Upserts one row per `(report_month, rule_id, repo_id, author_type)` into `ArchHealthAggregates`. The unique constraint makes the dog idempotent — re-running the same month is a no-op.
5. Renders `reports/architecture-health-YYYY-MM.md` from the live `ArchHealthAggregates` rows: per-invariant violation count, month-over-month delta, 6-month sparkline, per-repo invariant-health-score weighted average (weights from `docs/arch-health-weights.yaml`), per-author compliance with a `⚠️` flag when astromech compliance is worse than human.

The dashboard's Arch Health tab serves the same data via three read-only HTTP endpoints, with a month picker so the operator can scroll back through history without grepping the on-disk reports.

## Components

- **`internal/agents/dogs.go`** — registers `architecture-health-report` in `dogCooldowns` (30-day), in `dogOrder`, and in the `runDog` dispatch switch. The dog runs at the same Inquisitor heartbeat as every other dog; the cooldown is what makes the cadence "monthly".
- **`internal/agents/dogs_arch_health_report.go`** — dog body. Walks `ListRepos`, runs BoS rules, classifies authors, upserts into `ArchHealthAggregates`. Renders the report into `reports/architecture-health-YYYY-MM.md` from the live aggregates.
- **`internal/agents/dogs_arch_health_render.go`** — Markdown renderer. Produces the 6-section AUTO-GENERATED-headed body: month header, per-invariant table with month-over-month delta + sparkline, per-repo health-score table (weighted average), per-author compliance table with the `⚠️` astromech-worse-than-human flag, methodology section disclosing the v1 path-heuristic classifier, footer.
- **`internal/store/arch_health.go`** — `UpsertArchHealthAggregate` (idempotent on the 4-tuple unique constraint), `ListArchHealthAggregatesForMonth`, `ListArchHealthMonths`. The store layer never blocks on a missing row — empty months render an "Awaiting first run" stub rather than a 404.
- **Schema (`schema/schema.sql` + `internal/store/schema.go`):**
  ```sql
  CREATE TABLE ArchHealthAggregates (
      id              INTEGER PRIMARY KEY AUTOINCREMENT,
      report_month    TEXT    NOT NULL,                         -- 'YYYY-MM'
      rule_id         TEXT    NOT NULL,                         -- e.g. 'BOS-001'
      repo_id         INTEGER NOT NULL,                         -- synthetic id from ListReposForArchHealth
      author_type     TEXT    NOT NULL,                         -- 'human' | 'astromech' | 'archaeologist-migration'
      violation_count INTEGER NOT NULL DEFAULT 0,
      created_at      TEXT    NOT NULL DEFAULT (datetime('now')),
      UNIQUE(report_month, rule_id, repo_id, author_type)
  );
  CREATE INDEX idx_arch_health_aggregates_month_rule ON ArchHealthAggregates(report_month, rule_id);
  ```
- **`internal/dashboard/handlers_arch_health.go`** — three read-only HTTP endpoints powering the dashboard tab:
  - `GET /api/arch-health/months` → distinct YYYY-MM tokens (for the picker).
  - `GET /api/arch-health/latest` → most-recent month's rows + per-repo + per-author totals.
  - `GET /api/arch-health/<YYYY-MM>` → specific month's rows + per-repo + per-author totals.
  Empty rather than 404 when no aggregates exist — the SPA renders an "Awaiting first run" stub.
- **Dashboard SPA tab** (`internal/dashboard/static/index.html`) — switchTab arm `arch-health`, content pane `tab-arch-health`, `loadArchHealth()` JS function fetching all 3 endpoints and rendering month picker + per-author summary + full table.
- **`docs/arch-health-weights.yaml`** — operator-tunable per-rule weight file. The renderer reads this file directly today; changes land through the D3 promotion pipeline (FleetRules + `force render-rules`), not by hand-editing. Anti-cheat asset for "no health-report metric inflation".
- **`scripts/pre-commit/arch-health-md-check.sh`** — pre-commit hook rejecting any staged `reports/architecture-health-*.md` whose first staged line does not start with the AUTO-GENERATED prefix `<!-- AUTO-GENERATED by \`force dog architecture-health-report`.

## Invariants

1. **AUTO-GENERATED-only artefact.** The first line of every rendered report carries the AUTO-GENERATED prefix. The pre-commit hook rejects hand-edits at staging time. Operators who want to comment on the data add an operator-event annotation (`POST /api/annotations`) or document the observation under `docs/`, NOT in the report itself.
2. **Dog idempotence.** The `UNIQUE(report_month, rule_id, repo_id, author_type)` constraint on `ArchHealthAggregates` makes re-running the same month a no-op. An operator-triggered manual run mid-month is safe.
3. **Weights live in `docs/arch-health-weights.yaml`.** Per-rule weight changes go through the D3 promotion pipeline (FleetRules → `force render-rules`). Hand-editing the file is advisory only — the renderer reads it today, but any quietly re-weighted firing must be traceable to a PromotionProposal that the operator approved. Anti-cheat #C ("no health-report metric inflation") made structural via the file's load-bearing comment.
4. **Cooldown-only gating.** The 30-day cadence is enforced by the `dogCooldowns` entry alone — there is no in-body day-of-month check. A manual mid-month invocation runs if the cooldown allows. The roadmap's "1st of month at 00:00 UTC" target is approximated by the natural inquisitor cadence + cooldown, not by a hard calendar gate (verifier round 1 caught a comment-vs-impl drift here; the comment was the wrong half).
5. **Per-author classification is path-heuristic in v1.** `*astromech*` → astromech; `*/migrations/*` + `*archaeologist_*` → archaeologist-migration; else human. Acknowledged in the report's methodology section. v2 swap to git-blame is a methodology-section-only change — the aggregate column shape doesn't change.
6. **Astromech-worse-than-human triggers `⚠️`.** The per-author compliance table emits the warning rune in the rendered report whenever astromech compliance is worse than human. `TestSPA_PerAuthor_AstroMechWarning` plants synthetic data and asserts the rune. This is the load-bearing detector for "Astromechs cheating their way past BoS rules at scale" — the operator is alerted in-report.
7. **Read-only dashboard API.** All three endpoints are GET-only. P25 CLI-parity carries an explicit allowlist exemption naming each endpoint (matching the D4 read-only-views convention).
8. **Synthetic 1-indexed `repo_id`.** `Repositories` is TEXT-keyed by name; `repo_id` in `ArchHealthAggregates` is a synthetic id derived from `ListReposForArchHealth`'s row order at scan time (same approach as D8 + D9-Archaeologist). The dashboard handlers emit the integer for parity with the schema; the SPA renders the repo name via a join-side lookup.

## Configuration

Per-repo opt-out: there is none for the report. The dog scans every registered repo with a readable `local_path` regardless. The operator's lever is `archaeologist_sweep_disabled` (a sibling D9 column, only governs the Archaeologist sweep) — arch-health intentionally has no opt-out so the longitudinal data set is complete.

Weights file (`docs/arch-health-weights.yaml`):

```yaml
weights:
  BOS-001: 1.0
  BOS-002: 1.0
  # ... one row per registered BoS rule ...
```

Weight 1.0 is the default. Higher weight means a violation contributes more to the per-repo invariant-health score's downside. Changes go through the D3 pipeline; the file's header comment names the pipeline as the authoritative change channel.

Storage: `ArchHealthAggregates` is small — one row per `(month × rule × repo × author_type)`. With ~10 rules × ~20 repos × 3 author types × 12 months, the table fits comfortably under 10 MB even after years of monthly runs. No cleanup dog; longitudinal trend depends on retention.

Render destination: `archHealthReportsDir` (default `reports/`). Tests can override via `setArchHealthReportsDirForTest` to a `t.TempDir()` so tests don't pollute the working tree.

Rule registry: BoS rules live in `internal/agents/bos.go` (the BoS commit-time review package; arch-health re-uses the same rule set rather than declaring its own). Adding a new rule means registering it in `bos.go` + adding a default `1.0` weight entry to `docs/arch-health-weights.yaml`. The schema needs no change — `rule_id` is a free-form TEXT column.

## Operator surface

```bash
force dogs run architecture-health-report          # one-shot manual run (cooldown-permitting)
force dogs status architecture-health-report       # last-run timestamp + cooldown remaining
sqlite3 holocron.db "SELECT report_month, COUNT(*) FROM ArchHealthAggregates GROUP BY 1 ORDER BY 1;"
ls reports/architecture-health-*.md                # rendered report inventory
```

Inspect a specific month's data through the dashboard API:

```bash
curl -s http://localhost:7777/api/arch-health/months  | jq .
curl -s http://localhost:7777/api/arch-health/latest  | jq .
curl -s http://localhost:7777/api/arch-health/2026-04 | jq '.per_author_total'
```

Dashboard:

- **Arch Health tab** — month picker (populated from `/api/arch-health/months`), per-author summary, full per-rule × per-repo × per-author table. The "Refresh" button re-fetches the active month.
- The SPA reads only the three GET endpoints; there is no write surface from the dashboard. Operator-event annotations are a separate substrate (`POST /api/annotations`).

When an Astromech opens a PR that touches a `reports/architecture-health-*.md` file, the pre-commit hook rejects the commit unless the file's first staged line carries the AUTO-GENERATED prefix. The hook's error message names the offending paths and points the operator at `force dogs run architecture-health-report`.

The first calendar-month run lands on the next month boundary after enabling. The structural test suite (`internal/agents/dogs_arch_health_report_test.go`) already pins AUTO-GENERATED header presence, sparkline rune emission, and the per-author `⚠️` flag, so the first real-content run's qualitative spot-check is the only operator-cadence step.

## See also

- [`docs/closures/DELIVERABLE-9-CLOSURE.md`](../closures/DELIVERABLE-9-CLOSURE.md) — D9 closure (both tracks: ArchHealth + Archaeologist) with the full design + evidence trail.
- [`docs/agents/archaeologist.md`](../agents/archaeologist.md) — the agent doc for the operator-gated debt-pattern Archaeologist (the LLM-free claim-loop agent that walks registered Patterns).
- [`docs/subsystems/archaeologist.md`](archaeologist.md) — the existing operator-gated debt-detection AGENT subsystem doc. Distinct from this doc: that one covers the on-demand qualitative complement; this one covers the dog + report pipeline + dashboard.
- [`docs/arch-health-weights.yaml`](../arch-health-weights.yaml) — the per-rule weight file (anti-cheat asset; changes via D3 promotion pipeline).
- [`scripts/pre-commit/arch-health-md-check.sh`](../../scripts/pre-commit/arch-health-md-check.sh) — the AUTO-GENERATED-header pre-commit gate.
- [`docs/subsystems/dashboard.md`](dashboard.md) — broader dashboard substrate; arch-health is one tab among many.
- [`docs/subsystems/dogs.md`](dogs.md) — broader dog cadence + cooldown contract; arch-health-report is one dog among many.
