# force-orchestrator

A local-first, multi-agent software development factory. You submit a feature request in plain English. A fleet of autonomous AI agents decomposes it into tasks, writes the code, reviews it, and merges it — while you watch.

Inspired by Steve Yegge's **Gas Town** pattern: all coordination happens through a shared SQLite ledger, not message queues or in-memory state. Agents are stateless workers that compete for tasks; the database is the single source of truth.

---

## Table of Contents

- [Architecture](#architecture)
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
┌─────────────────────────────────────────────────────────────────┐
│                        operator (you)                           │
│               force add / force watch / force mail              │
└──────────────────────────┬──────────────────────────────────────┘
                           │
                    ┌──────▼──────┐
                    │ holocron.db │  ← SQLite — all state lives here
                    │  (SQLite)   │
                    └──────┬──────┘
                           │
     ┌─────────────────────┼──────────────────────────┐
     │                     │                          │
┌────▼────┐          ┌─────▼──────┐         ┌────────▼────────┐
│Commander│          │ Astromechs │         │   Jedi Council  │
│(Planner)│          │  (Workers) │         │   (Reviewers)   │
└────┬────┘          └─────┬──────┘         └────────┬────────┘
     │                     │                         │
     │               git worktrees              git merge
     │               claude -p                  → main branch
     │                     │
     │              ┌──────▼──────┐
     │              │  Captain    │  ← plan coherence gate
     │              │ (per convoy)│     after each commit,
     │              └──────┬──────┘     before council
     │                     │
┌────▼────┐                │
│Inquisitor│ ← background health monitor
└──────────┘
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
Pending → Locked → AwaitingCaptainReview → AwaitingCouncilReview → Completed
                          ↘ → Pending (captain rejection, retry)    ↘ → Pending (council rejection, retry)
              ↘ Failed (max retries or permanent infra failure)
              ↘ Escalated (agent needs human input)

Planned → (force convoy approve) → Pending   [--plan-only flow only]
```

Tasks in a coordinated convoy (all convoys created by Commander) pass through the Captain after each Astromech commit. The Captain checks whether the implementation fits the larger plan and updates downstream task descriptions if needed, before forwarding to the Jedi Council for code quality review.

Direct `add-task` tasks (not in a convoy) skip the Captain and go straight to council.

`Planned` tasks are created when you submit a feature with `--plan-only`. They sit inert until you inspect the plan and run `force convoy approve <id>` to activate them.

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

---

## Fleet Memory & RAG

The fleet accumulates institutional knowledge across every task it completes or fails. This knowledge is stored in the `FleetMemory` table and injected into every Astromech prompt, giving agents context about what has worked and what hasn't on each repo — even across completely unrelated prior tasks.

### How Memory is Written

| Event | What's stored |
|---|---|
| Council approves a task | Success memory: task description + files changed (parsed from diff) |
| Council permanently rejects a task (max retries) | Failure memory: task description + final rejection reason |
| Infra failure becomes permanent | Failure memory: task description + infra error |

### How Memory is Retrieved (FTS5 RAG)

When an Astromech starts a task, it calls `GetFleetMemories(repo, taskPayload, limit=10)`. The task payload is used as a search query against the `FleetMemory_fts` FTS5 index:

1. **Sanitize** — strip FTS5 special characters, drop single-character words
2. **OR query** — join remaining terms with `OR` so BM25 ranks by vocabulary overlap, not strict AND matching
3. **Two-step fetch** — query FTS for ranked rowids, then look up full records filtered by repo
4. **Recency fallback** — if FTS returns nothing (no vocabulary overlap, or FTS5 not compiled in), fall back to the 10 most recent memories

The result is split into two prompt sections:

```
# FLEET MEMORY
## What has worked on <repo>
- [Task #42] Added POST /users endpoint with JWT auth
  Files: handlers/users.go, middleware/auth.go

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
- 1 Commander
- 1 Inquisitor

The daemon writes a `fleet.pid` file and logs to `fleet.log`. It handles `SIGINT`/`SIGTERM` with a 30-second graceful drain.

### 3. Watch the fleet

In a separate terminal:

```bash
force watch
```

This opens a live command center showing all tasks grouped by status, refreshing every 2 seconds.

### 4. Submit work

```bash
# High-level feature — Commander decomposes it
force add "Add user authentication with JWT tokens and refresh token rotation"

# Direct task to a specific repo (skips Commander)
force add-task myapp "Add rate limiting middleware to the /api/v1 routes"

# From a Jira ticket
force add-jira ENG-1234

# Plan only — inspect before executing (see plan-only workflow below)
force add --plan-only "Refactor the payment service to use the new billing API"
```

### 5. Check your mail

```bash
force mail inbox operator
```

The fleet mails the `operator` role when features complete, tasks fail, escalations arise, and more. Use `force mail read <id>` to read a message.

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
| `force add-repo <name> <path> <desc>` | Register a repository. Verifies path exists and is a git repo. |
| `force repos` | List all registered repositories. Shows `[PATH MISSING]` if a repo has moved. |
| `force repos remove <name>` | Unregister a repository. |

### Convoys

| Command | Description |
|---|---|
| `force convoy list` | List all convoys with progress counts. |
| `force convoy show <id>` | Show progress and dependency tree for a convoy. |
| `force convoy create <name>` | Create a named convoy manually. |
| `force convoy approve <id>` | Activate a plan-only convoy — moves all Planned tasks to Pending. |
| `force convoy reset <id>` | Reset all failed/escalated tasks in a convoy to Pending. |
| `force convoy reject <id> <feedback>` | Reject Commander's plan: cancels un-started tasks, sends feedback to Commander via mail, and requeues the parent Feature task for re-planning. |

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
| `force dashboard [--port N]` | Start an HTTP JSON API at `localhost:8080`. Endpoints: `GET /api/status`, `GET /api/tasks?status=X`, `GET /api/events` (SSE stream). Default port: 8080. |

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

2. Commander picks up Feature #1
   └─ Asks Claude to decompose into tasks
   └─ Creates Convoy #1
   └─ Inserts CodeEdit tasks #2, #3 into BountyBoard
   └─ Mails operator: "Feature #1 → 2 tasks in convoy #1"

3. R2-D2 picks up CodeEdit #2 (no blocked_by)
   └─ Loads FleetMemory for this repo (FTS5 search against task payload)
   └─ Reads inbox (any unread mail addressed to "astromech")
   └─ Runs claude -p in git worktree
   └─ Commits result → status: AwaitingCaptainReview

4. BB-8 tries to pick up CodeEdit #3 — blocked by #2, skips it

5. Captain-Rex picks up #2 for plan coherence review
   └─ Sees: Convoy #1 progress, completed tasks, remaining task #3
   └─ Reviews the diff — does task #3's description still make sense?
   └─ If task #3 description needs updating → rewrites its payload
   └─ Approved → status: AwaitingCouncilReview

6. Council-Yoda picks up #2 for code quality review
   └─ Reads inbox (directives, alerts)
   └─ Gets the git diff, asks Claude to evaluate
   └─ Approved → merges branch, stores FleetMemory success entry
   └─ Unblocks #3 (sets blocked_by = 0)

7. BB-8 picks up CodeEdit #3 (now unblocked, with updated payload if captain revised it)
   └─ FleetMemory now includes #2's success — BB-8 sees what files were touched
   └─ Runs claude -p, commits → AwaitingCaptainReview

8. Captain-Rex reviews #3 — convoy almost done, approves → AwaitingCouncilReview

9. Council-Mace reviews #3 → Approved → merges

10. Inquisitor notices Convoy #1 is complete
   └─ Marks Convoy #1 Completed
   └─ Mails operator: "[CONVOY COMPLETE] [1] Add search to the API"
```
