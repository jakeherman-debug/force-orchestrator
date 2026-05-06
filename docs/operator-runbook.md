---
audience: operator
scope: Things-go-wrong runbook — daemon crash, stuck convoy, runaway spend, dog failures.
owner: D13
last_reviewed: 2026-05-05
---

# Operator runbook

When the fleet misbehaves: how to diagnose, how to recover, when to escalate to a human-side fix.

## Stub

Currently a placeholder — D13 Phase 2 fills the per-symptom playbooks. Expected sources include FIX-LOG.md narratives (real failure modes), the `force doctor` checklist, and the Inquisitor's dog catalogue.

## Planned sections (P2 fills)

### Daemon-level

- **Daemon won't start** — `gh auth status` failure, reconcile-on-startup failure (the daemon refuses to proceed with an unreliable view), `holocron.db` ACL or permission issue
- **Daemon crashed mid-run** — restart, expected reconcile output, what divergence cases auto-recover vs escalate
- **Daemon graceful drain** — `SIGINT`/`SIGTERM` 30 s drain, what `ReleaseInFlightTasks` does, where pending claims go

### Tasks + convoys

- **Task stuck in Locked** — Inquisitor's stale-lock handling (45 min timeout); Boot agent verdicts (RESET / ESCALATE / WARN / IGNORE)
- **Stalled convoy** — `force convoy show <id>`, `force convoy reset <id>`, when to `cancel`
- **Looping reject/retry** — Medic verdicts (`requeue` / `shard` / `escalate`); how the per-task spend escalate threshold halts a runaway
- **Stuck in `AwaitingChancellorReview`** — Chancellor single-instance serialization; how to inspect the proposal queue
- **PR stuck in `DraftPROpen`** — `convoy-review-watch` cadence, how to read `ConvoyReviewCycles`, force a re-trigger

### Spend + halts

- **Hourly spend cap hit** — `hourly_spend_cap_usd` (soft, claim loops sleep) vs `hourly_spend_estop_usd` (hard, e-stop)
- **Per-task spend escalate** — `BountyBoard.spend_suspended=1`, how to clear after investigation
- **E-stop on, can't resume** — verify the underlying trigger cleared, `force resume`, expected log lines

### Repos + branches

- **Repo went read-only / quarantined** — symptoms in dashboard, how to lift quarantine
- **Branch protection blocking merge** — Force respects the target's protection; this is the operator's CI/CD gate, not a Force bug
- **Orphaned worktrees** — `force cleanup`

### Database

- **`holocron.db` corrupted** — restore from `~/.force/backups/`, how `make protect-db` ACL is bypassed for legitimate maintenance, when `make unprotect-db` is required
- **Schema drift** — `TestSchemaParity` is the gate; how to fix a drift between `createSchema` / `runMigrations` / `schema/schema.sql`

### Dogs + escalations

- **Dog failure** — log line shape, expected operator-mail subject, how to manually re-run
- **Escalation backlog** — auto re-bumping after 4h; threshold for high-escalation banner (>=3)

### Dashboard

- **Dashboard unreachable** — 127.0.0.1 bind only; SSH tunnel for remote (`ssh -L 8080:localhost:8080`)
- **Dashboard mutations 413** — body cap is 256 KB on mutations; payload is too large

### Recovery surfaces

- **`fleet.log`** — human-readable timestamped log
- **`holonet.jsonl`** — structured telemetry; rotates at 50 MB
- **Operator mail** — stable subject prefixes (`[RECONCILE]`, `[TASK SPEND ANOMALY]`, `[CIRCULAR COMMITS]`, `[CHANCELLOR FAIL-CLOSED]`, `[CONTEXT OVERFLOW]`, `[CONVOY REVIEW PASSED]`, …)
- **`AuditLog`** — every operator + dashboard-initiated state transition
