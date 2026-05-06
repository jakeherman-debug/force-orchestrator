---
title: Onboarding
type: operator-doc
last-reviewed: 2026-05-05
audience: operator
scope: First-day-on-the-keyboard guide — install, build, first daemon run, first commission, where to look when stuck.
owner: D13
last_reviewed: 2026-05-05
---

# Onboarding

For a brand-new operator: from `git clone` to "I've watched a real task land." This is the practical first-session walkthrough; once you have the daemon running and a feature commissioned, the [operator runbook](operator-runbook.md) is the reach-for-when-stuck companion.

## Prerequisites

Force is a developer tool that runs locally. You need:

- **Go 1.25+** (toolchain, for `make build`). The repo's `go.mod` pins `go 1.25.3`.
- **Claude CLI** authenticated (`claude --version` returns a version; `claude` runs interactively). Force shells out to `claude -p` for every LLM call — there is no Anthropic HTTP-API path.
- **`git`** in `$PATH`.
- **`gh`** (GitHub CLI) authenticated (`gh auth status` passes). The daemon refuses to start if `gh auth` is broken — the PR-based delivery flow needs it.
- **`sqlite3`** in `$PATH` (for inspecting `holocron.db`; not strictly required to run, but the runbook assumes it).
- A **macOS** environment if you plan to use `make protect-db` (the macOS ACL hook is the implementation; Linux is supported but ACL protection is no-op).

## Build

The canonical build path is `make build`:

```bash
git clone <this repo>
cd force-orchestrator
make build
```

Under the hood: `go build -tags sqlite_fts5 -o force ./cmd/force/`. The `sqlite_fts5` tag enables FTS5 semantic search in `FleetMemory`; without it the `FleetMemory_fts` index is not created and retrieval silently falls back to recency-only ranking. All `make` targets pass the tag automatically — if you invoke `go` directly (e.g. from your editor) add `-tags sqlite_fts5` yourself.

Then either add `force` to your `PATH` or run it as `./force` from the repo root.

Run the doctor once after build:

```bash
./force doctor
```

It verifies `git`, `claude`, repo paths, DB integrity, e-stop state, and permanently blocked tasks. A clean doctor is the contract you want before submitting work.

## First daemon run

### 1. Register a target repository

The fleet only touches **registered** repos. New registrations land in `Repositories.mode = 'read_only'` by default — astromechs cannot claim work against them until the operator promotes the mode (see step 4 below).

```bash
./force add-repo myapp /path/to/myapp "Backend API service in Go"
```

`add-repo` verifies the path is a git repo, populates `remote_url` and `default_branch` from `git remote -v`, and queues a `FindPRTemplate` task so the repo is ready for the PR flow before the first feature lands.

Inspect what's registered:

```bash
./force repos
```

`[PATH MISSING]` after a name means the repo's local clone moved — re-add or `force repo sync` to repair.

### 2. Start the daemon

```bash
./force daemon
```

This spawns all agents as goroutines under one process. Defaults (configurable; see [`docs/operator-runbook.md`](operator-runbook.md) § Operational levers):

- 2 Astromechs, 1 Council, 3 Commanders, 1 Captain, 1 Chancellor (always single-instance), 1 Librarian, 1 Medic, 1 Investigator, 1 Auditor, 1 Pilot, 1 Diplomat, 1 BoS, 1 ISB, 1 Senate, 1 Inquisitor.

The daemon writes `fleet.pid` to the working directory, logs to `fleet.log`, and emits structured telemetry to `holonet.jsonl`. It handles `SIGINT`/`SIGTERM` with a 30 s graceful drain (cancels claim loops, then sweeps in-flight rows via `ReleaseInFlightTasks`).

> **Note on D12.** When D12 lands, the foreground/supervisor split lets you run `force daemon foreground` and the background path becomes the supervised default. Today, `./force daemon` is the only mode.

### 3. Open the dashboard

In a separate shell:

```bash
./force dashboard           # http://localhost:8080
./force dashboard --port 9090   # or pick a port
```

The Fleet Command Center is the **primary** interface — task submission, progress monitoring, escalations, mail, knowledge browser, experiments, logs. It binds 127.0.0.1 only (Pattern P8); for remote access use an SSH tunnel (`ssh -L 8080:localhost:8080`), never relax the bind.

Alternatively, `./force watch` opens a terminal UI if you'd rather stay in the shell.

### 4. Promote your repo to write mode

New repos default to `'read_only'`. The astromech claim query has `r.mode = 'write'` baked in — read-only repos' tasks aren't visible. Promote when you're ready for the fleet to actually edit:

- **Dashboard:** the Repos section has one-click `Promote to Write` / `Quarantine` / `Restore` buttons (audit-logged).
- **HTTP API:** `POST /api/repos/{name}/promote-to-write`, `POST /api/repos/{name}/quarantine`, `POST /api/repos/{name}/restore`.

(There is no `force repo set-mode <name> write` CLI today — promotion happens through the dashboard or its API. A CLI parity wrapper is a future P25 candidate.)

### 5. Commission a Feature

The easiest path is the dashboard's **`+ Queue Task`** modal. From the CLI:

```bash
# High-level feature — Commander decomposes, Chancellor approves, convoy spawns
./force add "Add user authentication with JWT tokens and refresh token rotation"

# From a Jira ticket
./force add-jira ENG-1234

# Plan-only — review the plan before any code is written
./force add --plan-only "Refactor the payment service to use the new billing API"
```

`--plan-only` creates the convoy in the `Planned` state. Inspect with `force convoy show <id>`, then run `force convoy approve <id>` to activate (or `force convoy reject <id> "reason"` to send Commander back for replan).

Other commission types:

- `./force scan [--repo <name>] <scope/question>` — Auditor scans the codebase; findings are queued as Planned `CodeEdit` tasks in a new convoy. Read the convoy first, then approve.
- `./force investigate [--repo <name>] <question>` — Investigator delivers a prose report as operator mail.

Submission is **idempotent** — repeated calls within a 60 s dedup window return the same task ID.

### 6. Watch the work land

In the dashboard:

- **Tasks** tab — sort, filter, click any row to see Claude attempt history, retry counts, fleet memory used, mail.
- **Convoys** tab — progress cards per convoy; activate Planned convoys; cancel runaway convoys.
- **Escalations** tab — respond to blockers the fleet can't resolve autonomously.
- **Mail** tab — read agent reports.
- **Knowledge** tab — browse `FleetMemory` (Librarian's curated 2–4 sentence nuggets per past success/failure).
- **Logs** tab — live-tailing `fleet.log` and structured `holonet.jsonl`.

CLI equivalents:

```bash
./force list Pending,Active,Failed
./force history <task-id>
./force diff <task-id>
./force mail inbox operator
./force convoy show <convoy-id>
./force convoy pr <convoy-id>     # ask-branch + sub-PR + draft-PR rollup
```

## Inspecting the holocron

`holocron.db` is just SQLite — every coordination event is a row. When the dashboard isn't enough, read the source of truth directly:

```bash
sqlite3 holocron.db
sqlite> SELECT id, status, type, owner, repo FROM BountyBoard ORDER BY id DESC LIMIT 20;
sqlite> SELECT * FROM Escalations WHERE status = 'Open';
sqlite> SELECT key, value FROM SystemConfig ORDER BY key;
```

The schema lives at [`schema/schema.sql`](../schema/schema.sql) (kept in parity with `createSchema` + `runMigrations` by `TestSchemaParity`).

The DB is in the working directory the daemon was launched from (`./holocron.db` plus its `-wal` and `-shm` sidecars). Don't move it; use `make protect-db` to apply a macOS ACL that blocks `unlink`, and `make install-snapshots` to install hourly `sqlite3 .backup` snapshots into `~/.force/backups/`.

## Smoke flows

`make smoke` runs in ~15 s and exercises the fleet's load-bearing boot path: schema creation, all migrations, dashboard `/healthz`, spend-cap defaults, and the protected-branch guard. If smoke fails, the daemon will not start cleanly — fix it before going further.

```bash
make smoke
```

The full suite is `make test` (~2-3 min). Always run it before considering a phase done.

## Plan-only workflow

If you want to read what the fleet plans to do before any code is written:

```bash
# 1. Submit — Commander plans, but tasks are created as Planned (inert)
./force add --plan-only "Migrate the database from Postgres to CockroachDB"

# 2. Inspect
./force convoy list
./force convoy show <convoy-id>

# 3. Approve to activate (or reject with feedback)
./force convoy approve <convoy-id>
./force convoy reject <convoy-id> "Use Spanner not CockroachDB"
```

Audit-style scans (`force scan ...`) always land as Planned convoys regardless of `--plan-only` — Auditor findings always go through operator-approval before the fleet starts editing.

## Where to look when something's stuck

| Symptom | First-look surface |
|---|---|
| Task stuck `Locked` past 45 min | Inquisitor's stale-lock reset (auto, but verify in dashboard) |
| Convoy stuck `DraftPROpen` | `convoy-review-watch` dog cadence; check `force convoy show <id>` |
| Escalation backlog | Dashboard Escalations tab; ack / close / requeue |
| Runaway spend | `Total Spend` header + `force costs`; spend caps are in `SystemConfig` |
| Daemon dropped | `tail -f fleet.log`; restart with `./force daemon`; reconcile output should print on boot |

The full per-symptom playbook is [`docs/operator-runbook.md`](operator-runbook.md).

## Convention reminders

- **Conventional commits.** `feat:`, `fix:`, `docs:`, etc. The body explains *why*, not *what*.
- **No `--no-verify`.** Pre-commit hooks run for a reason. If a hook fails, fix the root cause and re-stage; do **not** `--amend` (the failed commit didn't happen, so amending would touch the previous commit).
- **`make test` (with `-tags sqlite_fts5`)** is the gate before any phase closes. Runs in ~2-3 min.
- **Docs and tests are part of every phase's exit criteria.** A phase is not done until tests are green AND the relevant README / `schema.sql` / `CLAUDE.md` / closure doc is updated.

## Where to go next

- [Architecture overview](overview.md) — what force is, the agent fleet at a glance, the substrate, the lifecycle of work.
- [Operator runbook](operator-runbook.md) — when something's wrong: daemon crash, stuck convoy, runaway spend, dog failures, schema drift.
- [Subsystem index](subsystems/README.md) — daemon lifecycle, dashboard, fleet memory, mail, dogs, security posture, supply-chain hygiene, paired runs.
- [Roadmap](roadmap.md) — what's planned (D12 daemon control surface, D13 docs structure, etc.).
