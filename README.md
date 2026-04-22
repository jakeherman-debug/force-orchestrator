# force-orchestrator

A local-first, multi-agent software development factory. You submit a feature request in plain English. A fleet of autonomous AI agents decomposes it into tasks, writes the code, reviews it, and merges it — while you watch.

Inspired by Steve Yegge's **Gas Town** pattern: all coordination happens through a shared SQLite ledger, not message queues or in-memory state. Agents are stateless workers that compete for tasks; the database is the single source of truth.

---

## Table of Contents

- [Architecture](#architecture)
- [The Dashboard — Primary Interface](#the-dashboard--primary-interface)
- [The Agents](#the-agents)
- [Fleet Memory & RAG](#fleet-memory--rag)
- [Mail System](#mail-system)
- [Directives](#directives)
- [Installation](#installation)
- [Getting Started](#getting-started)
- [Command Reference](#command-reference)
- [Configuration](#configuration)
- [Watchdog Dogs](#watchdog-dogs)
- [Development](#development)

---

## Architecture

```
                 ┌─────────────────────────────────────────────────┐
                 │               holocron.db  (SQLite)             │
                 │         the shared ledger — single source       │
                 │    agents poll this; they never talk directly   │
                 └─────────────────────────────────────────────────┘

  Feature request path  (force add · dashboard "+ Queue Task")
  ──────────────────────────────────────────────────────────────────
  operator
    │
    ▼
  Commander ──────────────────────────────────────────────────────►
  decomposes Feature into tasks, proposes a plan
    │
    ▼
  Chancellor ─────────────────────────────────────────────────────►
  approve · sequence · reject · merge plans
    │
    └─► convoy created, CodeEdit tasks queued

  CodeEdit task path  (runs in parallel across Astromechs)
  ──────────────────────────────────────────────────────────────────
  Astromechs  ────────────────────────────────────────────────────►
  claim tasks, run claude -p in isolated git worktrees, commit
    │
    ▼
  Captain  ───────────────────────────────────────────────────────►
  plan coherence gate — keeps downstream tasks accurate as reality
  diverges from Commander's original plan; approve / update / reject
    │
    ▼
  Jedi Council ───────────────────────────────────────────────────►
  code quality review — diff evaluation, approve or reject
    │
    ├──► approved  →  git merge → main  →  Librarian writes memory
    └──► failed (max retries)  →  Medic triages
                                         (requeue · shard · escalate)

  Research & scan paths
  ──────────────────────────────────────────────────────────────────
  Investigator  →  prose report delivered as operator mail
  Auditor       →  findings queued as Planned tasks in a new convoy

  Background
  ──────────────────────────────────────────────────────────────────
  Inquisitor  —  stale task reset · stall detection · convoy
                 completion · escalation re-bumping · watchdog dogs
```

**Everything is SQLite-first.** Agents do not communicate directly with each other. They read from and write to `holocron.db`. This means:
- Any agent can crash and restart without losing state
- The operator can inspect or modify any piece of state with standard SQL
- Adding more agents is just spawning more goroutines pointing at the same DB

**Agents shell out to `claude -p`.** The orchestrator never calls the Anthropic HTTP API directly. Every agent invocation goes through the Claude CLI, which means each agent has access to the full MCP toolchain configured in your Claude environment.

### Key Tables

| Table | Purpose |
|---|---|
| `BountyBoard` | The task queue. Every task from `Pending` through `Completed` lives here. |
| `Repositories` | Registered repos the fleet can touch. |
| `Convoys` | Named groups of tasks spawned from a single feature request. |
| `Fleet_Mail` | Inter-agent messaging. Role-addressed, type-categorized. |
| `FleetMemory` | Cross-task learning store. Indexed with FTS5 for semantic retrieval. |
| `TaskHistory` | Full Claude output for every attempt on every task (the seance). |
| `Escalations` | Human-required blockers raised by agents. |
| `Agents` | Persistent worktree registry — one worktree per agent per repo. |
| `AuditLog` | Record of every operator and agent action. |
| `SystemConfig` | Runtime configuration (concurrency, delays, etc). |
| `Dogs` | Cooldown tracking for background watchdog tasks. |

### Task Lifecycle

```
Feature tasks (Commander):
  Pending → Locked → AwaitingChancellorReview → Completed (Chancellor creates convoy + CodeEdit tasks)
                              ↘ → Pending (Chancellor rejects plan; Commander replans)

CodeEdit tasks (Astromechs):
  Pending → Locked → AwaitingCaptainReview → AwaitingCouncilReview → Completed
                           ↘ → Pending (captain rejection, retry)    ↘ → Pending (council rejection, retry)
             ↘ Failed (max retries or permanent infra failure) → MedicReview spawned
             ↘ Escalated (agent needs human input)

WriteMemory tasks (Librarian):
  Pending → Completed   [spawned by Council after each CodeEdit approval]

MedicReview tasks (Medic):
  Pending → Completed   [spawned on permanent CodeEdit failure; may requeue/shard/escalate the original]

Planned → (force convoy approve) → Pending   [--plan-only flow only]
```

**Feature tasks** flow through Commander (planning) and then the Chancellor (conflict review) before any code is written. The Chancellor approves, sequences (waits on other convoys), rejects (replans), or merges the plan with another pending proposal.

**CodeEdit tasks** in a coordinated convoy pass through the Captain after each Astromech commit, then the Jedi Council for code quality review. After approval the Council spawns a `WriteMemory` task for the Librarian.

**Direct `add-task` tasks** (not in a convoy) skip the Captain and go straight to council.

`Planned` tasks are created when you submit a feature with `--plan-only`. They sit inert until you inspect the plan and run `force convoy approve <id>` to activate them.

---

## The Dashboard — Primary Interface

> **The web dashboard is the intended primary way to interact with the fleet.** The CLI commands exist for scripting and power-user workflows, but day-to-day operation — submitting work, monitoring progress, handling escalations, reviewing mail — is designed around the browser UI.

Start it alongside the daemon:

```bash
force daemon          # start the agent fleet
force dashboard       # open Fleet Command Center at http://localhost:8080
force dashboard --port 9090   # or a custom port
```

The dashboard is a single-page app served by an embedded Go HTTP server. It auto-refreshes on a live polling loop — no manual reloads needed. The URL query string stays in sync with your current tab, filter, and sort state so you can bookmark or share a specific view.

### Header

The header is always visible and shows the current fleet state at a glance:

| Element | What it shows |
|---|---|
| **Daemon badge** | Green `● Daemon PID <N>` when running; red `● Daemon offline` otherwise |
| **E-STOP badge** | Appears in red when an emergency stop is active |
| **E-Stop / Resume buttons** | Toggle the fleet pause state — the E-Stop button turns into Resume when active |
| **+ Queue Task button** | Opens the task submission modal |

### Stats Bar

A permanently visible row of live counters (refreshed every 5 seconds):

| Counter | What it counts |
|---|---|
| Running | Tasks currently locked by an agent |
| Pending | Tasks waiting to be claimed |
| Review | Tasks awaiting Captain or Council review |
| Done | All completed tasks |
| Failed | Failed + Escalated tasks |
| Escalations | Open escalations requiring human input |
| Convoys | Active convoys |
| Unread Mail | Unread fleet mail messages |
| Total Spend | Cumulative token cost in dollars |

### Tabs

#### Tasks (default)

The main workspace. A sortable, filterable, paginated table of tasks (50 per page).

**Filter buttons:** Active · Pending · Failed · Cancelled · Completed · All

**Sort columns:** ID, Status, Type, Priority, Created, Cost (click to sort; click again to reverse)

**Inline search:** Filters the current page by keyword across payload, repo, type, status, and owner.

**Pill bar** above the table shows: Pending / Active / Completed Today / Active Convoys — updated every 10 seconds.

**Task row columns:** ID · Status pill · Owner · Type · Payload (truncated) · Repo · Priority · Retry count · Runtime or blocked-by links · Created · Cost

**Clicking any row** opens a slide-in detail panel on the right with the full task context:

- **Meta** — repo, owner, branch name, convoy, retry/infra-failure counts, priority, lock time, runtime, blocked-by links, token cost
- **Broader Goal** — the parent feature description (if this is a CodeEdit subtask)
- **Directive** — the full task payload
- **Error Log** — if the task has failed
- **Attempt History** — every Claude run: agent name, outcome, token counts in/out, timestamp
- **Fleet Memories** — the top-10 RAG memories retrieved for this task's context (what has worked and failed on this repo before)
- **Task Mail** — all fleet mail scoped to this task

**Action buttons** in the panel (context-sensitive):

| Button | When visible | What it does |
|---|---|---|
| Approve & Merge | Task awaiting Captain or Council review | Merges the branch to main and marks Completed |
| Reject | Task awaiting review | Opens a modal to enter rejection feedback; resets task to Pending with the feedback appended |
| Retry | Task is Failed or Escalated | Resets to Pending |
| Reset to Pending | Any non-Completed task | Clears error log and retry count |
| Cancel | Most active states | Marks as Cancelled; optionally re-queues as a different task type |

#### Escalations

Cards for every escalation the fleet has raised. Each card shows the severity (LOW/MEDIUM/HIGH), the originating task, and the agent's message.

**Filter:** Open · Closed · All

**Actions per card:**
- **Acknowledge** — marks the escalation seen without restarting the task
- **Close** — dismisses the escalation
- **Close & Requeue** — dismisses and sends the task back to Pending for another attempt

#### Convoys

Progress cards for every convoy. Each card shows the convoy name, ID, status, a progress bar, and completed/total task counts.

**Filters:** All / Active / Completed + time window (last 1h · 8h · 24h)

**Clicking the convoy name or ID** drills into that convoy's tasks in the Tasks tab with a filter banner.

**Actions per card:**
- **Activate Planned Tasks** — visible when a `--plan-only` convoy has unstarted Planned tasks; equivalent to `force convoy approve`
- **Cancel Convoy** — stops all pending/planned tasks in the convoy

#### Agents

A table of every registered agent worktree. Shows the agent name, repo, current task (linked), task status, and lock timestamp. Agents actively working are highlighted.

#### Mail

A full inbox of all fleet mail (last 200 messages). Unread messages are highlighted.

Clicking a row opens a modal with the full message body (Markdown rendered). Reading a message marks it as read and decrements the unread counter in the header.

**Mark all as read** button clears the entire unread queue.

#### Knowledge

The Fleet Memory browser. Shows every success and failure memory the Librarian has written, searchable and filterable.

**Filters:** All / Successes / Failures + repo dropdown + full-text search

Each row shows the repo, originating task (linked), outcome, memory summary, and files changed. Clicking a row opens a detail modal with the full text. Individual memories can be deleted from the table or the modal.

#### Logs

A live-tailing log viewer, auto-scrolling with a 1000-line cap. Two sources:

| Mode | Source | Content |
|---|---|---|
| `fleet.log` | `fleet.log` on disk | Human-readable timestamped agent log lines |
| `holonet events` | `holonet.jsonl` | Structured JSON telemetry events |

Both streams open as Server-Sent Events connections and reconnect automatically on disconnect.

### "+ Queue Task" Modal

The primary entry point for submitting work. Select a task type and describe the work:

| Type | Behavior |
|---|---|
| **Auto (recommended)** | The fleet classifies the request and routes it appropriately |
| **Feature** | Submitted directly to Commander for planning; Chancellor reviews before convoy is created |
| **Investigate** | A free-form research question; the Investigator delivers a prose report as mail |
| **Audit** | A codebase scan; findings are queued as Planned tasks in a new convoy |

Repo and priority are optional. Submission is idempotent — double-clicks within 60 seconds return the same task ID.

### JSON API

The dashboard also exposes a complete JSON API for scripting and external integrations. All endpoints return `Content-Type: application/json` with CORS headers.

| Method | Endpoint | Description |
|---|---|---|
| `GET` | `/api/status` | Fleet health: daemon PID, e-stop state, task counts by status, open escalations, active convoys, unread mail, total spend |
| `GET` | `/api/stats` | Simplified counts: pending, active, completed today, active convoys, active agents |
| `GET` | `/api/tasks` | Task list — query params: `status` (comma-separated), `convoy_id`, `sort_by`, `sort_dir`, `limit`, `offset` |
| `GET` | `/api/tasks/{id}` | Full task detail including history, memories, mail |
| `POST` | `/api/tasks/{id}/retry` | Retry a Failed/Escalated task |
| `POST` | `/api/tasks/{id}/reset` | Reset any task to Pending |
| `POST` | `/api/tasks/{id}/cancel` | Cancel a task; body `{"requeue_type":"Feature"}` to re-queue |
| `POST` | `/api/tasks/{id}/approve` | Operator-approve and merge a task awaiting review |
| `POST` | `/api/tasks/{id}/reject` | Reject with feedback; body `{"reason":"..."}` |
| `GET` | `/api/escalations` | Escalation list; `?status=Open\|Closed` |
| `POST` | `/api/escalations/{id}/ack` | Acknowledge escalation |
| `POST` | `/api/escalations/{id}/close` | Close escalation |
| `POST` | `/api/escalations/{id}/requeue` | Close and requeue the task |
| `GET` | `/api/convoys` | Convoy list with progress |
| `POST` | `/api/convoys/{id}/approve` | Activate Planned tasks in a convoy |
| `POST` | `/api/convoys/{id}/cancel` | Cancel a convoy |
| `GET` | `/api/agents` | Agent registry with current task |
| `GET` | `/api/mail` | Last 200 fleet mail messages |
| `POST` | `/api/mail/{id}/read` | Mark a message read |
| `POST` | `/api/mail/read-all` | Mark all messages read |
| `GET` | `/api/memories` | Fleet Memory — params: `repo`, `outcome`, `q` (search), `limit` |
| `DELETE` | `/api/memories/{id}` | Delete a memory entry |
| `POST` | `/api/add` | Queue a new task — body: `{"type":"","payload":"","repo":"","priority":0,"idempotency_key":""}` |
| `GET` | `/api/events` | SSE stream of `holonet.jsonl` telemetry events |
| `GET` | `/api/fleet-log` | SSE stream of `fleet.log` (32 KB backfill on connect) |
| `POST` | `/api/control/estop` | Trigger emergency stop |
| `POST` | `/api/control/resume` | Clear emergency stop |
| `GET` | `/healthz` | Health check — returns `{"status":"ok","ts":<unix>}` |

---

## The Agents

### Commander Cody — Planner

**File:** `commander.go`

The Commander receives high-level `Feature` tasks and decomposes them into concrete `CodeEdit` subtasks for the Astromechs. It reads your registered repo descriptions and README files to understand the codebase before planning, then assigns each subtask to the correct repository with explicit dependency ordering (`blocked_by`).

When Commander finishes, it:
- Creates a **Convoy** to group all subtasks together
- Mails the operator a breakdown of what was planned and which repo each task targets
- Marks the Feature task as Completed

If a task is too complex for a single coding session, an Astromech can emit `[SHARD_NEEDED]` and the task is re-queued as a `Decompose` request back to Commander.

### Astromechs — Workers

**File:** `astromech.go`

The worker agents. Multiple Astromechs run in parallel, each competing for `CodeEdit` tasks. When one claims a task it:

1. Creates or reuses a **persistent git worktree** for that agent+repo combination
2. Branches off the current default branch (`agent/<name>/task-<id>`)
3. Assembles a rich prompt including:
   - The task payload and broader goal context
   - **Fleet Memory** — semantically relevant prior successes and failures on this repo (FTS5 ranked)
   - **Seance** — full output from all prior attempts on this task (if retrying)
   - **Inbox** — unread mail addressed to the astromech role or this agent specifically
   - **Directives** — standing orders from the operator (loaded from `./directives/astromech.md`)
4. Runs `claude -p` in the worktree directory
5. Commits the result and sets status to `AwaitingCouncilReview`

Signals the agent can emit:
- `[ESCALATED:LOW|MEDIUM|HIGH:reason]` — needs human input
- `[CHECKPOINT: step_name]` — records progress for resume after interruption
- `[SHARD_NEEDED]` — task is too large, re-queue to Commander
- `[DONE]` — work is committed, send for review immediately

Astromechs handle their own infra failures with exponential backoff. After `maxInfraFailures` consecutive failures, the task is permanently failed, a remediation task is spawned, and the operator is mailed.

### Fleet Captain — Plan Coherence Gate

**File:** `captain.go`  
**Roster:** Captain-Rex, Captain-Wolffe, Captain-Bly, Captain-Gree, Captain-Ponds

The Captain sits between each Astromech commit and the Jedi Council. It is not a code reviewer — it is a plan coherence check. Its job is to ensure the convoy plan stays valid as implementation reality diverges from what Commander originally designed.

After each task commit the Captain receives:
- The full convoy state: completed tasks (already merged), pending tasks (not yet started), and the current diff
- The task's git diff

It can:
- **Approve** — plan is still coherent, forward to the Jedi Council
- **Update downstream tasks** — if the implementation took a different approach, rewrite pending task payloads to match reality before they are claimed
- **Insert new tasks** — if the implementation revealed missing work Commander didn't anticipate
- **Reject** — the implementation is so far off-plan that downstream tasks cannot proceed; return to Astromech for rework
- **Escalate** — the convoy plan has a fundamental problem requiring human judgment

The Captain is biased strongly toward approval. It only rejects if downstream tasks are genuinely broken by the implementation. Minor deviations, unexpected file choices, and stylistic differences are approved.

Only convoys created by Commander are coordinated. Tasks added directly with `force add-task` skip the Captain and go straight to the Jedi Council.

### Jedi Council — Reviewers

**File:** `jedi_council.go`

The review agent. Council members watch for `AwaitingCouncilReview` tasks, compute the git diff against the default branch, and ask Claude to evaluate whether the diff correctly and completely accomplishes the stated task.

The council prompt:
```
{"approved": true/false, "feedback": "concise reason for rejection, or empty string if approved"}
```

On **approval**: merges the branch into main, unblocks dependent tasks, stores a success memory in `FleetMemory`, and mails the operator when the whole convoy is done.

On **rejection**: appends the feedback to the task payload, resets to `Pending`, mails the next Astromech that picks it up, and increments the retry counter. After `maxRetries` rejections, the task is permanently failed and a failure memory is stored.

Council members also read their inbox before reviewing, so operator directives (e.g. "always require tests") are respected.

### Inquisitor — Monitor

**File:** `inquisitor.go`

A background watchdog that runs on a 5-minute cycle. It:

- **Resets stale tasks** — any task `Locked` or `UnderReview` for longer than `staleLockTimeout` (45 min) is returned to `Pending`
- **Detects stalls** — tasks locked longer than `stallWarnTimeout` (20 min) with no new commits are flagged; after `stallEscTimeout` (30 min), the **Boot Agent** decides whether to reset, escalate, warn, or ignore
- **Closes convoys** — marks a convoy Completed when all its tasks are done; mails the operator
- **Re-escalates** — bumps severity on escalations unacknowledged for 4+ hours and mails the operator
- **Cleans orphaned branches** — deletes git branches for permanently failed/escalated tasks
- **Runs dogs** — dispatches background maintenance tasks on their cooldowns

### Boot Agent — Triage

**File:** `boot.go`

A lightweight Claude-backed triage agent called by the Inquisitor when a stall is detected. It examines the task details, agent history, and error logs, then returns one of four verdicts:

| Verdict | Action |
|---|---|
| `RESET` | Return task to Pending — agent likely hung |
| `ESCALATE` | Create an escalation — needs human review |
| `WARN` | Log a warning but take no action yet |
| `IGNORE` | Agent is still making progress |

### Auditor — Codebase Scanner

**File:** `auditor.go`

A read-only analysis agent that systematically scans the codebase (and external systems) for issues and produces structured findings. Each finding becomes a discrete `CodeEdit` task for an Astromech to fix — but they are created as **Planned** tasks in a new convoy, not activated immediately. The operator must review and approve the convoy before any work begins.

Submit an audit with:

```bash
force scan [--priority N] [--repo <name>] <scope/question>
```

- **Without `--repo`**: scans all registered repositories and reports findings across them.
- **With `--repo myapp`**: scopes the audit to that repository only; the agent runs from its local path.

The Auditor uses read-only tools (Read, Glob, Grep, Bash read-only commands, Jira, Confluence, Glean, SonarQube, Datadog). It does not modify any files.

When the audit completes:
1. A **Convoy** is created containing one Planned `CodeEdit` task per finding, labeled with severity (`HIGH`, `MEDIUM`, or `LOW`).
2. The operator receives a mail summarizing the findings and the convoy ID.
3. Run `force convoy show <convoy-id>` to inspect the plan, then `force convoy approve <convoy-id>` to activate the fixes.

If no findings are reported, the Audit task completes immediately with a mail saying so — no convoy is created.

### Investigator — Research Agent

**File:** `investigator.go`

A free-form research agent that investigates an open-ended question and delivers a written report. Unlike the Auditor it produces prose, not structured findings, and does not spawn follow-up tasks.

Submit an investigation with:

```bash
force investigate [--priority N] [--repo <name>] <question>
```

- **Without `--repo`**: the agent operates from the force-orchestrator working directory with access to all registered repos.
- **With `--repo myapp`**: runs from that repository's local path and focuses its investigation there.

The Investigator uses the same read-only toolset as the Auditor (Read, Glob, Grep, Bash, Jira, Confluence, Glean, SonarQube, Datadog). It does not modify any files.

When the investigation completes the full prose report is delivered to the operator as fleet mail:

```bash
force mail inbox operator   # see the report arrive
force mail read <id>        # read the full report
```

### Supreme Chancellor — Convoy Approver

**File:** `chancellor.go`  
**Name:** Supreme-Chancellor-Palpatine (single instance — deliberate serialization point)

The Chancellor is the conflict-resolution gate that sits between Commander's planning output and the actual creation of a convoy. After Commander decomposes a Feature into tasks and stores a proposed plan, the Feature task transitions to `AwaitingChancellorReview`. The Chancellor reviews the plan against all currently active work and other pending proposals.

The Chancellor can rule:

| Ruling | Action |
|---|---|
| `APPROVE` | Plan is safe to execute. Creates the convoy and tasks immediately. |
| `SEQUENCE` | Plan is correct but depends on an active convoy finishing first. Creates the convoy with cross-convoy blocking dependencies on the specified convoy's tail tasks — new root tasks cannot start until upstream work merges. |
| `REJECT` | Fundamental design conflict. Resets the Feature to Pending and mails Commander with the rejection reason for replanning. |
| `MERGE` | This plan overlaps significantly with another pending proposal. The Chancellor calls Claude to synthesize a single unified task list and creates one combined convoy for both features. |

The Chancellor also enforces two optional ordering directives on any ruling:
- **`sequence_after_feature_ids`**: blocks the new convoy until a queued Feature (not yet planned) gets its own convoy and completes.
- **`hold_convoy_ids`**: retroactively places currently active convoys on hold when the Chancellor determines they depend on work this new proposal introduces.

On Claude failure, the Chancellor auto-approves to avoid blocking the pipeline.

### Fleet Librarian — Memory Curator

**File:** `librarian.go`  
**Roster:** Jocasta-Nu, Huyang, Dexter-Jettster

The Librarian writes high-quality memory entries after each successful task. When the Jedi Council approves a task it spawns a `WriteMemory` bounty containing the task description, files changed, council feedback, and the git diff. The Librarian claims this bounty and calls Claude with a strict prompt that produces a 2–4 sentence memory nugget covering:

1. What was built or fixed (specific — named function, file, or component)
2. What was non-obvious or tricky about the implementation
3. Patterns, gotchas, or pitfalls not obvious from reading the code

This replaces the raw task-description memories the Council wrote directly before the Librarian was introduced. The result is injected into future Astromech prompts via the Fleet Memory RAG system.

On Claude failure the Librarian falls back to a truncated task description so no memory slot is lost.

### Fleet Medic — Failure Triage

**File:** `medic.go`  
**Roster:** Bacta, Kolto, Stim

The Medic triages tasks that have permanently failed — exhausted all retry attempts or hit an unrecoverable infra failure. Instead of immediately escalating to the operator, the Medic examines the full attempt history, all council/captain rejection feedback, and the last git diff, then renders one of three verdicts:

| Verdict | Action |
|---|---|
| `requeue` | The task is valid but needed clearer guidance. Resets it to Pending and mails astromechs with specific corrective guidance to prevent the same failure. |
| `shard` | The task was too broad for a single agent. Cancels the original task and inserts 2–5 focused sub-tasks into the same convoy. |
| `escalate` | The failure reveals an architectural ambiguity, missing dependency, or problem a coding agent cannot resolve. Creates an escalation and mails the operator. |

The Medic is biased toward `requeue` or `shard` — `escalate` is a last resort. On Claude failure the Medic escalates directly without looping.

---

### Pilot — PR-Flow Steward

**File:** `pilot.go`
**Roster:** Poe-Dameron, Wedge-Antilles, Hera-Pilot

The Pilot is the git-ops steward for the PR-based delivery flow. It handles infra tasks that do not require code synthesis — cutting per-ask integration branches, rebasing them against main, auto-merging sub-PRs, and cleaning up branches after a convoy ships.

Tasks the Pilot claims:

| Task type | What it does |
|---|---|
| `FindPRTemplate` | Locate the PR template file for a repo. Deterministic filesystem search first (covers `.github/`, root, `docs/`, common variants); falls back to a single Claude call for repos whose templates live in unusual places. Writes the result to `Repositories.pr_template_path`. |
| `CreateAskBranch` | Cut and push the convoy's integration branch (`force/ask-<id>-<slug>`) off main. For multi-repo convoys, fans out per-repo branch creation — each touched repo gets its own branch row in `ConvoyAskBranches`. Idempotent: running twice is a no-op on already-branched repos. |
| `CleanupAskBranch` | Delete branches after the convoy ships or is abandoned. |
| `RebaseAskBranch` | *(Phase 5)* Rebase an ask-branch onto main and force-push; spawn a `RebaseConflict` CodeEdit when the rebase doesn't apply cleanly. |
| `RevalidateRepoConfig` | *(Phase 8)* Revalidate remote URL, default branch, and template path to catch repo renames or moved templates. |

Pilot's happy path is pure shell-out (git + gh) — no LLM — so it's fast and auditable. When a rebase conflicts, Pilot never tries to resolve it itself; it spawns a CodeEdit task and lets an astromech do the code work.

### Diplomat — Draft-PR Opener

**File:** `diplomat.go`
**Roster:** Leia-Organa, Padme-Amidala, Bail-Organa

Diplomat handles the final ship step: once a convoy's sub-PRs are all merged, Diplomat rebases the ask-branch onto main, populates the repo's PR template via Claude, runs a sanity pass (secret scan + placeholder check + length limit), and opens a **draft PR** against main. Immediately after the draft PR is created, Diplomat queues a **ConvoyReview** task (see PR-Based Delivery Flow below). Human operators review the draft PR on GitHub and click Ship it in the dashboard (or merge directly) after ConvoyReview passes.

Sanity pass failures trigger one LLM retry with critic feedback; a second failure escalates.

Diplomat also claims `ConvoyReview` and `PRReviewTriage` tasks in its work loop — these are convoy-level quality gates, not new delivery steps.

### PR-Based Delivery Flow

The fleet's default delivery path is a GitHub PR, not a local merge. For each convoy:

1. **Pilot cuts an ask-branch** off main (e.g. `alice-smith/force/ask-7-oauth-support`) and pushes it to origin — one per (convoy, repo) since a convoy may touch multiple repos. The leading `alice-smith/` is the operator's GitHub username, discovered via a fallback chain (`gh api user --jq .login` → `gh config get user -h github.com` → `git config user.name`). Enterprise repos typically require this prefix for branch ownership / branch-protection rules; local setups with no username configured fall back to the bare name (e.g. `force/ask-7-oauth-support`). Astromech work branches follow the same convention: `<username>/agent/<astromech>/task-<id>`.
2. **Astromechs branch off the ask-branch** (not main) to do their work.
3. **Jedi Council approval** pushes the astromech branch, opens a sub-PR against the ask-branch, and marks it for auto-merge once Jenkins CI is green.
4. **Medic handles any CI failure** (`CIFailureTriage`): classifies Flaky / RealBug / Environmental / BranchProtection / Unfixable, retriggers or spawns a fix task on the astromech branch.
5. **CI circuit breaker**: if a repo accrues 5 Environmental failures in 1 hour, sub-PR creation for that repo pauses for 30 minutes so the fleet doesn't pile up broken PRs.
6. **Pilot rebases the ask-branch periodically** as main drifts — every 15 minutes via `main-drift-watch`, using a cheap `git ls-remote` to detect whether main actually moved before spending compute. On rebase conflict, Pilot spawns a CodeEdit task on the ask-branch itself; when Jedi Council approves it, the special-case handler force-pushes the resolved ask-branch and updates the stored base SHA (no sub-PR, because head==base would be nonsense).
7. **Diplomat opens the draft PR** into main once all sub-PRs are merged and ask-branch CI is green. The PR body is LLM-populated from the repo's `pull_request_template.md` plus the convoy summary; a pre-post sanity pass scans for secrets and unfilled placeholders. Diplomat immediately queues a **ConvoyReview** task (see below).
8. **ConvoyReview** runs one LLM pass over the full ask-branch diff vs main, checking it against every commissioned task. Gaps (commissioned work missing from the diff), regressions (correct code removed), and incorrect changes (does the opposite of what was asked) each spawn a CodeEdit fix task on the ask-branch. Once those fix tasks complete, the `convoy-review-watch` dog re-triggers a fresh pass. The loop terminates when a pass returns clean — at that point the operator's "Ship It" is a true final approval on a verified diff.
9. **Human clicks "Ship it"** in the dashboard (`force convoy ship <id>` on the CLI). There is no auto-ship to main — the human gate is intentional.
10. **Pilot cleans up** the ask-branch after the draft PR merges; Librarian records a convoy-level memory.
11. **ship-it-nag** reminds the operator if a draft PR sits unshipped for 24h / 72h / 1 week.

All 14 dogs under the Inquisitor's 5-minute cycle; most are cheap polls that only trigger work when an actual event is detected.

Per-repo opt-out flag: `pr_flow_enabled` (default `true`). Set to `false` to fall back to the legacy direct-to-main merge path.

### Migration from pre-PR-flow databases

Existing `holocron.db` state is preserved. Migration runs automatically on daemon startup in three layers:

- **Layer A (schema):** additive `ALTER TABLE ADD COLUMN` + `CREATE TABLE IF NOT EXISTS AskBranchPRs`. Idempotent; no data is rewritten.
- **Layer B (repo backfill):** populates `remote_url` and `default_branch` by shelling out to `git` per repo. Repos whose origin is unreachable are marked `pr_flow_enabled=0` so they fall back to the legacy path rather than producing broken PRs.
- **Layer C (convoy backfill):** *(Phase 2)* for each Active convoy without an ask-branch, queues `CreateAskBranch` for Pilot.

Operators can also run the migration explicitly:

```sh
force migrate pr-flow --dry-run    # preview changes
force migrate pr-flow              # apply (auto-snapshots holocron.db first)
force migrate pr-flow --rollback   # restore the most recent snapshot (daemon must be stopped)
```

Before any work the daemon verifies `gh auth status` passes. If it doesn't, startup aborts with a clear error — the fleet refuses to run a half-migrated PR flow.

---

## Fleet Memory & RAG

The fleet accumulates institutional knowledge across every task it completes or fails. This knowledge is stored in the `FleetMemory` table and injected into every Astromech prompt, giving agents context about what has worked and what hasn't on each repo — even across completely unrelated prior tasks.

### How Memory is Written

Success and failure memories take different paths.

**Success path — via the Librarian:**

When the Jedi Council approves a task it does not write the memory directly. Instead it spawns a `WriteMemory` task containing the task description, files changed (parsed from the diff), council feedback, and the full diff itself. The Librarian claims this task and calls Claude with a strict curation prompt:

> Write exactly 2–4 sentences covering: (1) what was built or fixed — be specific, name the function or file; (2) what was non-obvious or tricky about the implementation; (3) patterns, gotchas, or pitfalls not obvious from reading the code alone. Plain prose, no bullet points.

This design is intentional. A raw task description ("Add rate limiting middleware") is a weak retrieval signal — it's too generic to surface usefully against a different future task's vocabulary. The Librarian's output is dense with specific, concrete terms: function names, file paths, library choices, failure modes. BM25 retrieval rewards exactly this kind of term specificity.

**Failure path — written directly:**

| Event | What's stored |
|---|---|
| Council permanently rejects a task (max retries) | Failure memory: task description + final rejection reason |
| Infra failure becomes permanent | Failure memory: task description + infra error |

Failure memories are written by the Council and the infra-failure handler directly, without the Librarian, because the signal value is the rejection reason itself — not a curated narrative.

### How Memory is Retrieved (FTS5 RAG)

When an Astromech starts a task it calls `GetFleetMemories(repo, taskPayload, limit=10)`. The task payload is used as a search query against the `FleetMemory_fts` FTS5 index:

1. **Sanitize** — strip FTS5 special characters, drop single-character words
2. **OR query** — join remaining terms with `OR` so BM25 ranks by vocabulary overlap, not strict AND matching
3. **Two-step fetch** — query FTS for ranked rowids, then look up full records filtered by repo
4. **Recency fallback** — if FTS returns nothing (no vocabulary overlap, or FTS5 not compiled in), fall back to the 10 most recent memories

The OR-based BM25 query means memories that share the most terminology with the incoming task rank highest — "middleware", "JWT", "auth handler" in the task description will surface memories that used those same words, even if they came from completely unrelated prior features. The Librarian's concrete, noun-heavy prose is specifically shaped to make this matching effective.

The result is split into two prompt sections injected into the Astromech's context:

```
# FLEET MEMORY
## What has worked on <repo>
- [Task #42] The JWT middleware was added to middleware/auth.go using the
  golang-jwt/jwt/v5 library. The non-obvious part was that the token expiry
  check must happen before role validation — reversing the order causes a
  nil-pointer panic on expired tokens. Files: middleware/auth.go, handlers/users.go

## What has failed on <repo> — do not repeat these approaches
- [Task #38] Failed after 3 attempts. Final rejection: missing error handling on DB calls
```

### Seance (Per-Task History)

Separate from fleet-wide memory, the **seance** injects the full output of every prior attempt on the *same task* when retrying. On attempt 3, the agent sees both attempt 1 and attempt 2's complete Claude output, each labeled with the agent name and outcome. This prevents agents from repeating the exact approach the council already rejected.

### Build Requirement

FTS5 requires the `sqlite_fts5` build tag for go-sqlite3. Without it, the FTS table is not created and retrieval silently falls back to recency — fully functional, just not semantically ranked.

`make build` and `make test` include this tag automatically. If you invoke `go` directly, pass `-tags sqlite_fts5`.

---

## Mail System

Agents communicate through `Fleet_Mail` — a role-addressed, typed messaging system. Mail is never addressed to a specific agent by name; it's addressed to a **role** so any agent of that role picks it up.

### Roles

| Role | Who reads it |
|---|---|
| `astromech` | Any Astromech that claims the next task |
| `captain` | Any Captain doing a convoy review |
| `jedi-council` | Any council member doing a review |
| `commander` | Commander-Cody (planner) |
| `operator` | You — visible via `force mail inbox operator` |
| `all` | Every agent of any role |

### Message Types

| Type | How agents use it |
|---|---|
| `directive` | Injected as `# STANDING ORDERS` — highest priority, always obeyed |
| `feedback` | Injected as `# PRIOR FEEDBACK` — prior rejection context (from Captain or Council) |
| `alert` | Injected as `# ALERTS` — prominent warnings |
| `remediation` | Informational — infra fix notifications |
| `info` | Low-priority context |

### When Mail is Sent Automatically

| Trigger | From → To | Type |
|---|---|---|
| Council rejects a task | council → astromech | feedback |
| Task permanently fails | agent → operator | alert |
| Infra failure (permanent) | agent → operator | alert |
| Agent escalates | agent → operator | alert |
| Stall escalated by Boot agent | inquisitor → operator | alert |
| Escalation unacknowledged 4h | inquisitor → operator | alert |
| Commander decomposes feature | commander → operator | info |
| Convoy completes | inquisitor → operator | info |
| Remediation task approved | council → operator | remediation |
| Dog fails | inquisitor → operator | alert |
| Chancellor approves a plan | chancellor → operator | info |
| Chancellor sequences a plan | chancellor → operator | info |
| Chancellor rejects a plan | chancellor → operator + commander | alert + feedback |
| Chancellor merges two plans | chancellor → operator | info |
| Chancellor places a convoy on hold | chancellor → operator | alert |
| Medic requeues a failed task | medic → astromech | feedback |
| Medic escalates a failed task | medic → operator | alert |

Mail scoped to a task (`task_id != 0`) is automatically cleaned up by the `mail-cleanup` dog after the task completes and 48 hours have passed.

---

## Directives

Directives are standing operator instructions injected into an agent's system prompt on every task. They are loaded from markdown files on disk — **not** from the database config.

### Lookup order (first match wins)

1. `./directives/<repo>/<role>.md` — per-repo, local to the working directory
2. `~/.force/directives/<repo>/<role>.md` — per-repo, user-global
3. `./directives/<role>.md` — global, local to the working directory
4. `~/.force/directives/<role>.md` — global, user-global

### Roles

| Role filename | Used by |
|---|---|
| `astromech.md` | All Astromech worker agents |
| `commander.md` | Commander Cody (affects decomposition strategy) |
| `council.md` (or `jedi-council.md`) | All Jedi Council reviewers |

### Setting directives

```bash
# See an example for a role
force directive example astromech

# Create the directives directory
mkdir -p directives

# Write a global astromech directive
cat > directives/astromech.md << 'EOF'
All code must have error handling. Never use panic().
Always run existing tests before committing.
EOF

# Write a repo-specific directive (takes precedence over global)
mkdir -p directives/myapp
cat > directives/myapp/astromech.md << 'EOF'
Use the internal logging package, not fmt.Println.
EOF

# Write a council directive
cat > directives/council.md << 'EOF'
Reject any diff that removes test files.
Reject if new public functions lack documentation.
EOF

# Verify the active directive for a role
force directive show astromech
force directive show astromech myapp   # repo-specific
```

Agent signal tokens (`[ESCALATED:...]`, `[DONE]`, etc.) are automatically stripped from directive files to prevent injection.

---

## Installation

**Prerequisites:**
- Go 1.21+
- `claude` CLI installed and authenticated (`claude --version`)
- `git` available in PATH

```bash
git clone <this repo>
cd force-orchestrator
make build
```

Add `force` to your PATH, or run it as `./force`.

Run `force doctor` after installation to verify all dependencies are in order.

---

## Getting Started

### 1. Register a repository

```bash
force add-repo myapp /path/to/myapp "Backend API service in Go"
```

The fleet will only touch registered repositories. Register all repos the agent should be able to modify.

### 2. Start the daemon

```bash
force daemon
```

This starts all agents in the background:
- 2 Astromechs (default; configure with `force config set num_astromechs 4`)
- 1 Council member (configure with `force config set num_council 2`)
- 3 Commanders (default; configure with `force config set num_commanders 1`)
- 1 Captain (configure with `force config set num_captain 2`)
- 1 Chancellor (single instance, always)
- 1 Librarian (configure with `force config set num_librarians 2`)
- 1 Medic (configure with `force config set num_medics 2`)
- 1 Investigator (configure with `force config set num_investigators 2`)
- 1 Auditor (configure with `force config set num_auditors 2`)
- 1 Inquisitor

The daemon writes a `fleet.pid` file and logs to `fleet.log`. It handles `SIGINT`/`SIGTERM` with a 30-second graceful drain.

### 3. Open the dashboard

In a separate terminal:

```bash
force dashboard
```

This opens the **Fleet Command Center** at `http://localhost:8080` — the primary interface for monitoring the fleet, submitting work, handling escalations, and reading mail. Open it in your browser and keep it visible while the fleet runs.

Alternatively, `force watch` opens a terminal UI if you prefer staying in the shell.

### 4. Submit work

The easiest way is to click **+ Queue Task** in the dashboard. From the CLI:

```bash
# High-level feature — Commander decomposes it, Chancellor reviews for conflicts
force add "Add user authentication with JWT tokens and refresh token rotation"

# From a Jira ticket
force add-jira ENG-1234

# Plan only — inspect before executing (see plan-only workflow below)
force add --plan-only "Refactor the payment service to use the new billing API"
```

### 5. Monitor and act

In the dashboard:
- **Tasks tab** — watch tasks move through the pipeline; click any task to see the full Claude output, retry count, and memory context
- **Escalations tab** — respond to blockers the fleet can't resolve autonomously
- **Convoys tab** — track feature-level progress; activate plan-only convoys after inspection
- **Mail tab** — read reports from Investigators and completion summaries from the Inquisitor

From the CLI:

```bash
force mail inbox operator   # fleet mail delivered to you
force mail read <id>        # read a specific message
```

### Plan-only workflow

`--plan-only` lets you review Commander's plan before any code is written:

```bash
# 1. Submit — subtasks are created as "Planned" (inert)
force add --plan-only "Migrate database from Postgres to CockroachDB"

# 2. Inspect the plan
force convoy list
force convoy show <convoy-id>

# 3. Approve to activate (or cancel individual tasks first)
force convoy approve <convoy-id>
```

---

## Command Reference

### Task Management

| Command | Description |
|---|---|
| `force add [--priority N] [--plan-only] <description>` | Submit a feature request. Commander decomposes it into subtasks. `--plan-only` creates subtasks as Planned until you approve. |
| `force add-task [--blocked-by <id>] [--convoy <id>] [--priority N] [--timeout <secs>] <repo> <desc>` | Add a direct CodeEdit task, bypassing Commander. |
| `force add-jira <TICKET-ID>` | Fetch a Jira ticket and submit it as a feature. |
| `force scan [--priority N] [--repo <name>] <scope>` | Submit a codebase audit. Findings are queued as Planned CodeEdit tasks in a new convoy awaiting `force convoy approve`. |
| `force investigate [--priority N] [--repo <name>] <question>` | Submit a research investigation. The Investigator's prose report is delivered as fleet mail to the operator. |
| `force list [status[,status2]] [--limit N]` | List tasks. Comma-separate statuses: `Pending,Failed`. |
| `force logs <id>` | Show the full payload and error log for a task. |
| `force history [--full] <id>` | Show all Claude attempts for a task. `--full` shows complete output. |
| `force reset <id>` | Reset a task to Pending (clears error log, retry count, branch). |
| `force retry <id>` | Alias for reset. |
| `force retry-all-failed` | Reset every Failed task back to Pending. |
| `force cancel <id>` | Mark a task as Failed manually (cannot be undone). |
| `force prioritize <id> <N>` | Set a task's priority (higher = claimed first). |
| `force run <id>` | Run a specific Pending task in the foreground, streaming Claude output to stdout. |
| `force diff <id>` | Show the git diff for a task's branch. |
| `force approve <id>` | Manually approve and merge a task (bypasses council). |
| `force reject <id> <reason>` | Manually reject a task with feedback. |
| `force block <task-id> <blocker-id>` | Make a task wait for another task to complete. |
| `force unblock <id>` | Remove a task's blocked_by dependency. |
| `force unblock-dependents <id>` | Recursively unblock all tasks that depend on `<id>`. |
| `force tree <id>` | Show a task's full subtask tree. |
| `force search <query>` | Search task payloads and error logs. |

### Fleet Control

| Command | Description |
|---|---|
| `force daemon` | Start all agents as background goroutines. |
| `force watch` | Open the live command center dashboard (terminal UI). |
| `force status` | Print a one-line fleet summary. |
| `force who` | Show which agents are active and what task each is running. |
| `force estop` | Emergency stop — pause all agents immediately. |
| `force resume` | Lift the e-stop. |
| `force scale <N>` | Change the number of Astromechs. Sends SIGUSR1 to a running daemon to hot-add agents. Scale-down only takes effect on restart. |
| `force agents` | List registered agent worktrees. |
| `force cleanup` | Remove stale worktrees from disk and DB. |
| `force doctor` | Pre-flight check: verifies `git`, `claude` CLI, repo paths, DB integrity, e-stop state, and permanently blocked tasks. |

### Repositories

| Command | Description |
|---|---|
| `force add-repo <name> <path> <desc>` | Register a repository. Verifies path exists and is a git repo. Eagerly populates `remote_url`/`default_branch` and queues a `FindPRTemplate` task so the repo is ready for the PR flow immediately. |
| `force repos` | List all registered repositories. Shows `[PATH MISSING]` if a repo has moved. |
| `force repos remove <name>` | Unregister a repository. |
| `force repo sync` | Re-discover `remote_url`/`default_branch` for every registered repo and queue a `FindPRTemplate` task for any repo whose template path is unknown. Only needed after origin/config changes — `add-repo` runs this automatically. |
| `force repo set-pr-flow <name> on\|off` | Enable or disable the PR-based delivery flow for a repo. Affects **future tasks only**; in-flight work finishes on the path it started on. Disabling sends tasks through the legacy local-merge path. |

### PR Flow Migration

The PR-based delivery flow replaces direct `git merge` with sub-PRs + a human-gated draft PR. Migration is additive and idempotent.

| Command | Description |
|---|---|
| `force migrate pr-flow --dry-run` | Preview what the migration would change: repos needing `remote_url` backfill, convoys needing ask-branch backfill, existing snapshots. No DB writes. |
| `force migrate pr-flow` | Run the migration. Auto-snapshots `holocron.db` to `holocron.db.pre-pr-flow.<timestamp>` before Layer B. Safe to re-run. |
| `force migrate pr-flow --rollback --confirm` | **DESTRUCTIVE.** Restore `holocron.db` from the most recent pre-migration snapshot. Daemon must be stopped. Loses any state changes since the snapshot (escalations, in-flight work, fleet memory). `--confirm` is required — `--rollback` alone refuses. |

### Convoys

| Command | Description |
|---|---|
| `force convoy list` | List all convoys with progress counts. |
| `force convoy show <id>` | Show progress and dependency tree for a convoy. |
| `force convoy create <name>` | Create a named convoy manually. |
| `force convoy approve <id>` | Activate a plan-only convoy — moves all Planned tasks to Pending. |
| `force convoy reset <id>` | Reset all failed/escalated tasks in a convoy to Pending. |
| `force convoy reject <id> <feedback>` | Reject Commander's plan: cancels un-started tasks, sends feedback to Commander via mail, and requeues the parent Feature task for re-planning. |
| `force convoy pr <id>` | Show per-repo ask-branches, draft PR URLs + state, and a sub-PR rollup (open/merged/closed counts, CI state). |
| `force convoy ship <id> [--merge squash\|merge\|rebase]` | Promote the convoy's draft PR(s) from draft to ready-for-review. With `--merge`, immediately merges using the specified strategy (default: operator reviews on GitHub and merges there). Only valid when the convoy is in `DraftPROpen` state. |

### Escalations

| Command | Description |
|---|---|
| `force escalations` | List open escalations. |
| `force escalations list [status]` | List escalations filtered by status (Open/Acknowledged/Closed). |
| `force escalations ack <id>` | Acknowledge an escalation (does not re-queue the task). |
| `force escalations close <id>` | Close an escalation without re-queuing the task. |
| `force escalations requeue <id>` | Close an escalation and return the task to Pending for retry. |

### Mail

| Command | Description |
|---|---|
| `force mail inbox <agent>` | Show messages for a specific agent or role. Use `operator` to see your own mail. |
| `force mail list` | List all messages (read and unread) across all roles. |
| `force mail read <id>` | Read the full body of a message (marks it as read). |
| `force mail send <to-agent> [--task <id>] [--type directive\|feedback\|alert\|info] <subject> [body]` | Send a message to an agent role. |

**Sending directives to agents:**
```bash
# Standing order to all astromechs
force mail send astromech --type directive \
  "Always write tests" \
  "Every code change must include a corresponding test file."

# Task-specific instruction
force mail send astromech --task 42 --type directive \
  "Use PostgreSQL not SQLite" \
  "This task must use the PostgreSQL driver, not SQLite."
```

### Fleet Memory

| Command | Description |
|---|---|
| `force memories [repo] [--limit N]` | Browse accumulated knowledge. `[PASS]` = success, `[FAIL]` = failure. |
| `force memories search <repo> <query>` | FTS5 search memories for a repo by keyword. |
| `force memories delete <id>` | Delete a specific memory entry (useful for pruning bad/misleading memories). |

### Directives

| Command | Description |
|---|---|
| `force directive show [role]` | Show the currently active directive for a role (default: astromech). |
| `force directive example [role]` | Print an example directive file with comments. Roles: `astromech`, `commander`, `council`. |

### Observability

| Command | Description |
|---|---|
| `force logs-fleet [--no-follow] [--filter <text>] [--agent <name>] [--task <id>] [--convoy <id>]` | Tail `fleet.log` with filtering. |
| `force holonet [--no-follow] [--filter <event_type>] [--task <id>]` | Tail `holonet.jsonl` (structured telemetry events). |
| `force stats` | Task throughput, completion rates, per-agent breakdown. |
| `force costs` | Token usage breakdown by agent and time window. |
| `force audit [--limit N]` | Show the operator/agent action audit log. |
| `force dogs` | Show watchdog status and next scheduled run. |
| `force dashboard [--port N]` | Start the Fleet Command Center web UI at `localhost:8080` (default). The primary interface — see [The Dashboard](#the-dashboard--primary-interface) for full documentation. |

### Maintenance

| Command | Description |
|---|---|
| `force prune [--keep-days N] [--dry-run]` | Delete old completed tasks and history. Default: keep 30 days. |
| `force export [file.json]` | Export the BountyBoard to JSON (default: `fleet-export.json`). |
| `force import <file.json>` | Import tasks from a JSON export. |
| `force config set <key> <value>` | Set a runtime config value. |
| `force config get <key>` | Read a config value. |
| `force config list` | List all config values. |

### Danger Zone

> **These commands are destructive and irreversible. Stop the daemon before running them (`force estop`, then kill the daemon process).**

| Command | Description |
|---|---|
| `force purge [--confirm]` | Delete all filesystem run artifacts: `fleet.log`, `holonet.jsonl`, all agent worktrees, and all agent branches in registered repositories. Dog cooldown timers are also cleared. **Task data in the database is NOT affected.** |
| `force hard-reset [--purge-repos] [--confirm]` | Wipe ALL fleet state: task data, history, memories, mail, escalations, audit log, worktrees, branches, and log files. Repositories and system config are preserved unless `--purge-repos` is also passed. **This cannot be undone.** |

Both commands print a full list of what will be destroyed before executing. Without `--confirm`, you are prompted to type `DELETE` interactively. Passing `--confirm` skips the interactive prompt but the warning is always shown.

---

## Configuration

Set with `force config set <key> <value>`.

| Key | Default | Description |
|---|---|---|
| `num_astromechs` | `2` | How many worker agents to run. Takes effect on restart or `force scale`. |
| `num_captain` | `1` | How many captain agents to run. Takes effect on restart. |
| `num_council` | `1` | How many review agents to run. Takes effect on restart. |
| `num_commanders` | `3` | How many Commander planners to run. Takes effect on restart. |
| `num_investigators` | `1` | How many Investigator research agents to run. Takes effect on restart. |
| `num_auditors` | `1` | How many Auditor scan agents to run. Takes effect on restart. |
| `num_librarians` | `1` | How many Librarian memory-curation agents to run. Takes effect on restart. |
| `num_medics` | `1` | How many Medic failure-triage agents to run. Takes effect on restart. |
| `max_concurrent` | `0` (unlimited) | Maximum tasks running simultaneously fleet-wide. |
| `spawn_delay_ms` | `0` | Milliseconds to wait between each agent claiming a new task. Smooths thundering-herd on large backlogs. |
| `batch_size` | `0` (unlimited) | Maximum tasks claimed fleet-wide in a 60-second window. |
| `max_turns` | `40` | Maximum Claude CLI turns per task. Higher values allow more complex tasks but cost more. |

**Note:** Rate-limit backoff state is tracked automatically under `rl_hits_<agent>` keys. You can inspect these with `force config list` to see if agents are currently throttled.

---

## Watchdog Dogs

Background maintenance tasks that run on a cooldown managed by the Inquisitor. View status with `force dogs`.

| Dog | Cooldown | What it does |
|---|---|---|
| `git-hygiene` | 30 min | Runs `git fetch --prune` and `git gc --auto` in every registered repo |
| `db-vacuum` | 6 hours | Runs `PRAGMA wal_checkpoint`, `ANALYZE`, and `VACUUM` on holocron.db |
| `holonet-rotate` | 24 hours | Rotates `holonet.jsonl` when it exceeds 50 MB |
| `mail-cleanup` | 12 hours | Removes unread task-scoped mail for completed/failed tasks older than 48h; removes read mail older than 30 days |
| `convoy-review-watch` | 5 min | Re-triggers ConvoyReview for `DraftPROpen` convoys once their fix tasks complete; also catches any convoy that missed the Diplomat fast-path trigger |

If any dog fails, the operator receives an alert mail.

---

## Development

```bash
make build   # compile with FTS5 support
make test    # run full test suite
make cover   # run tests and print coverage summary
make clean   # remove force binary and cover.out
```

The `sqlite_fts5` build tag is required to enable FTS5 semantic search in `FleetMemory`. All `make` targets include it automatically. If you invoke `go` directly (e.g. in your editor's build command), add `-tags sqlite_fts5`.

Tests that require `git` or FTS5 skip themselves gracefully when those dependencies are absent. E2E tests for the Claude CLI runner use `testdata/claude-stub` — a shell script stub controlled via environment variables:

| Variable | Default | Purpose |
|---|---|---|
| `CLAUDE_STUB_OUTPUT` | _(empty)_ | Text printed to stdout |
| `CLAUDE_STUB_EXIT` | `0` | Exit code |
| `CLAUDE_STUB_SLEEP_MS` | `0` | Milliseconds to sleep before responding (timeout testing) |

---

## How a Feature Request Flows End-to-End

```
1. force add "Add search to the API"
   └─ BountyBoard: Feature #1 → Pending

2. Commander-Cody picks up Feature #1
   └─ Asks Claude to decompose into tasks
   └─ Stores a ProposedConvoy (plan JSON) against Feature #1
   └─ Feature #1 → status: AwaitingChancellorReview

3. Supreme Chancellor reviews the proposed plan
   └─ Sees: no active convoys, no pending proposals → no conflicts
   └─ APPROVE ruling → creates Convoy #1
   └─ Inserts CodeEdit tasks #2, #3 into BountyBoard (task #3 blocked_by #2)
   └─ Mails operator: "[APPROVED] Feature #1 → convoy #1 (2 tasks)"

4. R2-D2 picks up CodeEdit #2 (no blocked_by)
   └─ Loads FleetMemory for this repo (FTS5 search against task payload)
   └─ Reads inbox (any unread mail addressed to "astromech")
   └─ Runs claude -p in git worktree
   └─ Commits result → status: AwaitingCaptainReview

5. BB-8 tries to pick up CodeEdit #3 — blocked by #2, skips it

6. Captain-Rex picks up #2 for plan coherence review
   └─ Sees: Convoy #1 progress, completed tasks, remaining task #3
   └─ Reviews the diff — does task #3's description still make sense?
   └─ If task #3 description needs updating → rewrites its payload
   └─ Approved → status: AwaitingCouncilReview

7. Council-Yoda picks up #2 for code quality review
   └─ Reads inbox (directives, alerts)
   └─ Gets the git diff, asks Claude to evaluate
   └─ Approved → merges branch, spawns WriteMemory task for Librarian
   └─ Unblocks #3 (sets blocked_by = 0)

8. Librarian (Jocasta-Nu) picks up WriteMemory for task #2
   └─ Calls Claude with task description, files changed, and diff
   └─ Writes a curated 2–4 sentence memory nugget to FleetMemory

9. BB-8 picks up CodeEdit #3 (now unblocked, with updated payload if captain revised it)
   └─ FleetMemory now includes #2's curated memory — BB-8 sees what files were touched
   └─ Runs claude -p, commits → AwaitingCaptainReview

10. Captain-Rex reviews #3 — convoy almost done, approves → AwaitingCouncilReview

11. Council-Mace reviews #3 → Approved → merges → spawns WriteMemory for Librarian

12. Diplomat picks up ShipConvoy
    └─ Rebases ask-branch onto main, runs sanity pass
    └─ Opens draft PR into main
    └─ Queues ConvoyReview #10 for Convoy #1

13. Diplomat picks up ConvoyReview #10
    └─ Reads full ask-branch diff vs main
    └─ Runs LLM pass: finds gap — rateLimitPatterns not updated
    └─ Spawns CodeEdit fix task #11 on the ask-branch
    └─ Marks ConvoyReview #10 Completed

14. R2-D2 picks up CodeEdit #11, fixes the gap → AwaitingCouncilReview
    Council approves → force-pushes ask-branch

15. convoy-review-watch dog sees: DraftPROpen, no pending ConvoyReview, no active fix tasks
    └─ Queues ConvoyReview #12

16. Diplomat picks up ConvoyReview #12
    └─ LLM pass returns "clean" — diff now correctly delivers everything commissioned
    └─ Mails operator: "[CONVOY REVIEW PASSED] Add search to the API — pass 2"
    └─ Marks Completed

17. Operator clicks "Ship it" — confident the diff is complete and correct
```
