---
title: Architecture overview
type: operator-doc
last-reviewed: 2026-05-05
audience: both
scope: One-shot conceptual orientation — what force is, the agent fleet, the substrate, and the lifecycle of work.
owner: D13
last_reviewed: 2026-05-05
---

# Architecture overview

## What force is

Force is a **local-first, single-operator AI fleet** that runs commissioned coding work end-to-end. You commission a feature in plain English; a fleet of autonomous Claude-backed agents decomposes it into tasks, writes the code in isolated git worktrees, reviews the diffs, opens a draft PR against the target repo, and lets you ship — while you watch through a web dashboard.

Force is inspired by Steve Yegge's **Gas Town** pattern: every cross-agent state transition lands in a single SQLite ledger (`holocron.db`). Agents do not message one another, do not share memory, and do not own state outside their own goroutine — they read and write rows. The DB is the single source of truth and the only coordination surface.

## What force is *not*

A few things calibrate expectations:

- Not multi-tenant. The schema, the dashboard's same-origin gate, and the operator-mail dedup state all assume a single human operator.
- Not a sandboxed execution environment. Astromech worktrees inherit your user, your `gh` token, your SSH agent. The bash-guard binary and capability profiles are defense-in-depth, not kernel-level isolation. The calibrated threat model lives under [`docs/subsystems/`](subsystems/README.md) (security posture doc is a P2 stub).
- Not a CI/CD replacement. Force edits code; you ship through your existing CI/CD. Branch protection and deploy gates on the target repo remain the production-safety boundary.

## The agent fleet at a glance

The fleet is a set of role-specialized agents that compete for tasks on the BountyBoard. Each agent has a distinct purpose, a static YAML capability profile, and a fixed CWD shape (review agents run in `force-orchestrator/`; astromechs run inside per-task target-repo worktrees).

Headlines:

- **Commander** decomposes a Feature into tasks, proposes a plan.
- **Chancellor** approves / sequences / merges / rejects the plan; only this single-instance agent creates a convoy.
- **Astromechs** are the workers — they claim CodeEdit tasks, run `claude -p` in a worktree, commit the result.
- **Captain** is a *plan coherence* gate (not code review) — keeps downstream tasks accurate as implementation diverges from Commander's plan.
- **Bureau of Standards (BoS)** + **Imperial Security Bureau (ISB)** are commit-time invariant + security gates running pure Go AST + deterministic checks (no LLM).
- **Senate** is a repo-scoped advisory layer between proposal and Chancellor.
- **Jedi Council** is the code-quality reviewer; on approval it queues a Librarian memory write and (in PR-flow mode) opens the sub-PR.
- **Pilot** + **Diplomat** are git-ops + draft-PR stewards for the PR-based delivery flow.
- **Librarian** curates `FleetMemory` — concrete 2–4 sentence prose nuggets the next astromech reads.
- **Medic** triages permanent failures (requeue / shard / escalate); biased away from escalation.
- **Inquisitor** is the background watchdog; runs the dog catalogue.
- **Boot** is a lightweight stall-triage oracle the Inquisitor calls.
- **Auditor** + **Investigator** are read-only research + scan agents.
- **Engineering Corps** authors paired-runs experiments + promotion proposals.

The full per-agent reference index lives at [`docs/agents/README.md`](agents/README.md). Each agent doc covers role, file path, roster, inputs/outputs, signals, capability profile, and notable invariants.

## The substrate

Three load-bearing pieces hold the fleet together.

### 1. `holocron.db` and the Gas Town pattern

Every coordination event — a task claim, a status transition, an escalation, a memory write, a paired-runs enrollment, a notification dispatch — is a row in `holocron.db` (SQLite, WAL mode). The implications:

- Any agent can crash and restart without losing state.
- The operator can inspect or modify any state with standard SQL (`sqlite3 holocron.db`).
- Adding parallelism is just spawning more goroutines pointing at the same DB.
- Cross-agent dependencies route through Go interfaces in `internal/clients/<service>/` (Pattern P16) — agents never import each other's concrete types.

The schema lives at [`schema/schema.sql`](../schema/schema.sql). Key tables:

| Table | Purpose |
|---|---|
| `BountyBoard` | The task queue. Every task from `Pending` through `Completed` lives here. |
| `Repositories` | Registered repos. The `mode` column (`'read_only'` / `'write'` / `'quarantined'`) gates whether the fleet may write. |
| `Convoys` | Named groups of tasks spawned from a single feature request. |
| `ConvoyAskBranches` | Per-(convoy, repo) integration branches in the PR-based delivery flow. |
| `Fleet_Mail` | Inter-agent messaging. Role-addressed, type-categorized. |
| `FleetMemory` | Cross-task learning store. FTS5-indexed for semantic retrieval. |
| `TaskHistory` | Full Claude output for every attempt + token counts + cost. |
| `TaskSpendWatch` | Per-task trailing-window spend rows; backs `task-spend-watch` dog. |
| `PromptByteAttribution` | Per-source byte breakdown for every Claude call. |
| `Escalations` | Human-required blockers raised by agents. |
| `Agents` | Persistent worktree registry — one worktree per agent per repo. |
| `AuditLog` | Record of every operator and agent action. |
| `SystemConfig` | Runtime configuration (concurrency, delays, spend caps, etc.). |
| `Dogs` | Cooldown tracking for background watchdog tasks. |

### 2. Per-agent capability profiles

Every Claude-invoking agent runs under a static, YAML-declared capability profile under [`agents/capabilities/`](../agents/capabilities/) (one per agent + `REGISTRY.yaml` + `.forceblocklist.yaml`). The Claude CLI invocation receives `--allowedTools`, `--disallowedTools`, and `--mcp-config` strictly from `capabilities.LoadProfile(agentName)` — no hardcoded literals are permitted (Pattern P13). `--disallowedTools` is the actual hard restriction (per Fix #8e); the loader fails closed on missing YAML, unknown tool, or blocklisted grant. Astromech and Medic-CI are the only agents that can call `Bash`; everyone else is restricted to read-only tools.

### 3. Per-agent worktree isolation

Astromechs do their work inside **persistent git worktrees**, one per (agent, repo). When an astromech claims a task it branches off the convoy's ask-branch (or `main`, in legacy mode), runs `claude -p` in the worktree, commits the result, and forwards to review. Worktrees survive across tasks; the `force cleanup` command removes orphans.

### Claude CLI shelling, not the HTTP API

Agents invoke Claude via `claude -p` (through `internal/claude`), never the Anthropic HTTP API. This preserves the MCP toolchain available to Claude Code and keeps every LLM call inside the model's own tool-use sandbox. The full invocation layering — which CWD each agent runs from, what `CLAUDE.md` auto-loads, which tools are statically removed — is documented in [`docs/architecture/claude-cli-invocation.md`](architecture/claude-cli-invocation.md).

## Lifecycle of a piece of work

A high-level Feature → ship trace:

1. **Commission.** The operator submits a feature description (`force add "..."`, or the dashboard's `+ Queue Task` modal). A `Feature` row lands in `BountyBoard` at `Pending`.
2. **Plan.** Commander claims the Feature, asks Claude to decompose it, and stores a `ProposedConvoy` (plan JSON). The Feature transitions to `AwaitingChancellorReview`.
3. **(Optional) Senate review.** If the proposed plan touches a Senator's repo, a `SenateReview` task is enqueued first; the Senator emits a Verdict (concur / dissent / abstain).
4. **Chancellor rules.** The Chancellor reviews the plan against active work: APPROVE creates the convoy + CodeEdit tasks; SEQUENCE creates the convoy with cross-convoy blocking; REJECT resets the Feature for replan; MERGE synthesizes the plan with another pending proposal.
5. **Pilot cuts ask-branches.** For each repo the convoy touches, Pilot creates a `force/ask-<id>-<slug>` integration branch (PR-flow mode).
6. **Astromechs claim CodeEdit tasks.** Each task: branch off the ask-branch into `<username>/agent/<astromech>/task-<id>`, run `claude -p`, commit, transition to `AwaitingCaptainReview`.
7. **BoS + ISB review (parallel).** After every astromech commit a `BoSReview` and an `ISBReview` infra task are enqueued in parallel with the next-stage review. Both must pass before the source CodeEdit advances to Captain.
8. **Captain reviews plan coherence.** Approves, updates downstream task payloads to match implementation reality, inserts new tasks, rejects, or escalates. Bias toward approval.
9. **Council reviews code quality.** Approves → opens a sub-PR against the ask-branch with auto-merge once Jenkins CI is green; rejects → resets to `Pending` with feedback appended; permanent failure → Medic triages.
10. **Librarian writes a memory.** On Council approval, a `WriteMemory` task spawns; the Librarian curates a 2–4 sentence prose nugget into `FleetMemory` (FTS5-indexed for the next astromech to find).
11. **Diplomat opens the draft PR.** Once all sub-PRs land and ask-branch CI is green, Diplomat populates the repo's PR template via Claude, runs a sanity pass (secret scan + placeholder check), and opens a **draft PR** into main. It immediately queues a `ConvoyReview` task.
12. **ConvoyReview gates the ship.** One LLM pass over the full ask-branch diff vs main checks every commissioned task got delivered. Gaps / regressions / incorrect changes spawn fix CodeEdits on the ask-branch; the `convoy-review-watch` dog re-triggers a fresh pass once they complete. Loop terminates when a pass returns clean.
13. **Operator clicks Ship it.** The dashboard's Ship button (or `force convoy ship <id>`) flips the draft PR to ready-for-review. There is no auto-ship — the human gate is intentional.

The full state-machine reference per task type lives in [`docs/architecture/claude-cli-invocation.md`](architecture/claude-cli-invocation.md) and per-agent docs under [`docs/agents/`](agents/README.md).

## Where to read deeper

- **Per-agent reference:** [`docs/agents/README.md`](agents/README.md)
- **Per-subsystem operator/user guides:** [`docs/subsystems/README.md`](subsystems/README.md) — daemon lifecycle, notification routing, dashboard, supply-chain hygiene, fleet memory + RAG, mail, dogs, security, etc.
- **Audit-pattern enforcement:** [`docs/patterns/README.md`](patterns/README.md) — every grep- / AST-based regression in `internal/audittools/` mapped to the rule it enforces.
- **Claude CLI invocation layering:** [`docs/architecture/claude-cli-invocation.md`](architecture/claude-cli-invocation.md) — which CWD each agent runs from, what `CLAUDE.md` auto-loads, how target-repo `CLAUDE.md` is treated as advisory.
- **Per-deliverable evidence trails:** [`docs/closures/`](closures/) — D0 through D11 closure reports.
- **Operator-facing companions:**
  - [`docs/onboarding.md`](onboarding.md) — install, first daemon run, first task.
  - [`docs/operator-runbook.md`](operator-runbook.md) — when something is wrong.
  - [`docs/roadmap.md`](roadmap.md) — strategic deliverable list with merge order.

The top-level [`README.md`](../README.md) is intentionally short (≤ 200 lines, enforced by `TestReadmeSizeUnder200Lines`) — it is the front door, not the manual.
