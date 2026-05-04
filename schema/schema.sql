-- Galactic Fleet Orchestrator — Holocron Database Schema
-- Source of truth: holocron.go (InitHolocronDSN)
-- Build: go build -tags sqlite_fts5 -o force .
-- NOTE: Do not run this file directly against an existing DB — use it as reference.
--       The application applies all migrations automatically on startup via InitHolocronDSN.

PRAGMA journal_mode=WAL;
PRAGMA foreign_keys=ON;

-- ── Core task board ───────────────────────────────────────────────────────────

CREATE TABLE IF NOT EXISTS BountyBoard (
    id                        INTEGER PRIMARY KEY AUTOINCREMENT,
    parent_id                 INTEGER DEFAULT 0,   -- ID of the Feature/Decompose task that spawned this
    target_repo               TEXT    DEFAULT '',  -- registered repo name (FK → Repositories.name)
    type                      TEXT,                -- 'Feature' | 'Decompose' | 'CodeEdit'
    status                    TEXT,                -- see status lifecycle below
    payload                   TEXT,                -- task description (enriched with [GOAL: ...] prefix by Commander)
    owner                     TEXT    DEFAULT '',  -- agent name currently holding the lock
    error_log                 TEXT    DEFAULT '',  -- last error or rejection reason
    retry_count               INTEGER DEFAULT 0,   -- council/captain rejection count (preserved across Medic requeues, Fix #6)
    infra_failures            INTEGER DEFAULT 0,   -- transient Claude CLI / git failures (preserved across Medic requeues, Fix #6)
    locked_at                 TEXT    DEFAULT '',  -- datetime('now') when locked; '' when free
    convoy_id                 INTEGER DEFAULT 0,   -- FK → Convoys.id (0 = standalone task)
    checkpoint                TEXT    DEFAULT '',  -- mid-task resume state written by Astromechs
    branch_name               TEXT    DEFAULT '',  -- 'agent/<name>/task-<id>' persistent branch
    priority                  INTEGER DEFAULT 0,   -- higher = claimed first (ties broken by id ASC)
    task_timeout              INTEGER DEFAULT 0,   -- per-task override in seconds (0 = default)
    idempotency_key           TEXT    DEFAULT '',  -- Fix #3: see idx_bounty_idem below — partial UNIQUE on non-empty, non-terminal rows
    medic_requeue_count       INTEGER DEFAULT 0,   -- Fix #6: count of Medic-driven requeues; applyMedicRequeue escalates past maxMedicRequeues (2)
    reshard_generation        INTEGER DEFAULT 0,   -- Fix #6: auto-reshard generation stamp; queueReshardDecompose refuses past maxReshardGeneration (2)
    parse_failure_count       INTEGER DEFAULT 0,   -- Fix #7: LLM JSON-parse failures on this row; ConvoyReview escalates at 2
    last_findings_fingerprint TEXT    DEFAULT '',  -- Fix #7: SHA256 of last pass's finding set; pass-to-pass dedup gate
    spend_suspended           INTEGER DEFAULT 0,   -- D2 T1-1: dogTaskSpendWatch sets to 1 when trailing-10m cost > escalate threshold; claim queries skip
    recent_commit_hashes_json TEXT    DEFAULT '[]', -- D2 T1-3.5: JSON array of the last 5 commit tree-hashes produced by this task's worktree; divergence detector escalates on circle
    -- D3 P1: paired-runs holdout/assignment + Captain proposal + prompt-version + revert handling.
    in_holdout                INTEGER DEFAULT 0,   -- inherited from natural unit; populated by treatments.Apply
    experiment_assignments_json TEXT  DEFAULT '{}',-- JSON map: experiment_id -> treatment_id
    proposed_action_json      TEXT    DEFAULT '',  -- Captain's spec-amendment proposal for unmapped spawns
    prompt_version            TEXT    DEFAULT '',  -- which prompt produced this decision (per-version metric correlation)
    prior_review_outcomes_json TEXT   DEFAULT '[]',-- chain of agent decisions on this task
    spawn_spec_link           TEXT    DEFAULT '',  -- 'tied_to_AT-NNN' | 'glue' | 'unmapped' (when this row was Captain-spawned)
    spawn_classification_confidence TEXT DEFAULT '', -- 'high' | 'medium' | 'low'
    spawning_at_id            TEXT    DEFAULT '',  -- concern #9: which AT a ConvoyReview-spawned fix task targets
    deferred_revert           INTEGER DEFAULT 0,   -- concern #7: row scheduled to revert when its dependents complete
    revert_target_task_id     INTEGER DEFAULT 0,   -- concern #7: the task this row reverts (cascade-revert flow)
    stage_id                  INTEGER DEFAULT NULL, -- D5.5 P2: FK → ConvoyStages.id; NULL = legacy/single-mode (task not bound to a specific stage); non-NULL = multi-stage convoy task
    blast_radius_json         TEXT    NOT NULL DEFAULT '{}', -- D8 T2: per-Feature blast-radius computed at convoy-creation time (modified_symbols, affected_consumer_repos, auto_included_tasks); empty '{}' on non-Feature rows + pre-T2 Features
    created_at                TEXT    DEFAULT (datetime('now'))
);
-- Hot-table indexes (AUDIT-009, Fix #4). Without these, every ClaimBounty
-- poll and dashboard refresh full-scans BountyBoard.
CREATE INDEX IF NOT EXISTS idx_bounty_status_type    ON BountyBoard (status, type);
CREATE INDEX IF NOT EXISTS idx_bounty_convoy_status  ON BountyBoard (convoy_id, status);
CREATE INDEX IF NOT EXISTS idx_bounty_parent_id      ON BountyBoard (parent_id);
CREATE INDEX IF NOT EXISTS idx_bounty_created_at     ON BountyBoard (created_at);
-- D5.5 P2 — partial index over populated stage_id rows; per-stage dispatch /
-- convoy-stage-watch dog filter on stage_id. NULL rows excluded by the
-- predicate so legacy single-stage convoys don't bloat the index.
CREATE INDEX IF NOT EXISTS idx_bounty_stage_id ON BountyBoard (stage_id) WHERE stage_id IS NOT NULL;

-- Fix #3 (AUDIT-008/034/035/036): partial UNIQUE index on idempotency_key.
-- Scoped to non-empty keys AND non-terminal statuses so:
--   * empty keys (the default for non-idempotent inserts) are not constrained
--   * a terminal row (Completed/Cancelled/Failed) does NOT block a legitimate
--     retry under the same key — the dedup only suppresses parallel/live work
-- Queue* helpers in the fleet pair this with
--   INSERT ... ON CONFLICT(idempotency_key) WHERE ... DO NOTHING RETURNING id
-- so two concurrent callers with the same key cannot both insert.
CREATE UNIQUE INDEX IF NOT EXISTS idx_bounty_idem
    ON BountyBoard(idempotency_key)
    WHERE idempotency_key != '' AND status NOT IN ('Completed','Cancelled','Failed');

-- Status lifecycle:
--   Pending → Locked (Astromech claims)
--     → AwaitingCaptainReview → UnderCaptainReview (Captain claims)
--       → Pending (Captain rejects, back for rework)
--       → AwaitingCouncilReview → UnderReview (Council claims)
--         → Completed (Council approves + merge)
--         → Pending (Council rejects, back for rework)
--     → Failed (max retries exceeded or infra failures exhausted)
--     → Escalated (Captain/Council escalated to operator)
--   Planned — inserted by Commander in plan-only mode; activated by 'force convoy approve'

-- ── Task dependency graph ─────────────────────────────────────────────────────
-- Many-to-many replacement for the old blocked_by INTEGER column.
-- A task is claimable only when all its depends_on tasks are Completed.
-- ClaimBounty uses a NOT EXISTS correlated subquery — no explicit unblock needed.

CREATE TABLE IF NOT EXISTS TaskDependencies (
    task_id    INTEGER NOT NULL,    -- the waiting task
    depends_on INTEGER NOT NULL,    -- the task that must complete first
    PRIMARY KEY (task_id, depends_on)
);

CREATE INDEX IF NOT EXISTS idx_taskdeps_task_id   ON TaskDependencies (task_id);
CREATE INDEX IF NOT EXISTS idx_taskdeps_depends_on ON TaskDependencies (depends_on);

-- ── Convoy grouping ───────────────────────────────────────────────────────────
-- Named groups of related CodeEdit subtasks produced by one Feature decomposition.
-- PR-flow fields: ask_branch is the integration branch every sub-PR merges into;
-- ask_branch_base_sha caches main's HEAD at ask-branch creation, used by
-- main-drift-watch to detect when main has moved and a rebase is needed.
-- draft_pr_* track the final human-gated PR into main.

CREATE TABLE IF NOT EXISTS Convoys (
    id                          INTEGER PRIMARY KEY AUTOINCREMENT,
    name                        TEXT UNIQUE NOT NULL,  -- '[<feature_id>] <preview>'
    status                      TEXT    DEFAULT 'Active',
    coordinated                 INTEGER DEFAULT 0,     -- 1 = Astromech completions route through Captain
    ask_branch                  TEXT    DEFAULT '',    -- 'force/ask-<id>-<slug>'; '' = legacy convoy
    ask_branch_base_sha         TEXT    DEFAULT '',    -- main's HEAD at branch creation; updated on rebase
    draft_pr_url                TEXT    DEFAULT '',
    draft_pr_number             INTEGER DEFAULT 0,
    draft_pr_state              TEXT    DEFAULT '',    -- 'Open' | 'Merged' | 'Closed'
    shipped_at                  TEXT    DEFAULT '',    -- set when draft PR is merged
    -- D3 P1: paired-runs + verification spec.
    in_holdout                  INTEGER DEFAULT 0,
    experiment_assignments_json TEXT    DEFAULT '{}',
    parent_feature_id           INTEGER DEFAULT 0,
    verification_spec_json      TEXT    DEFAULT '',    -- acceptance tests, exit criteria; { ats: [...], deprecated: [...] } per concern #9
    spec_history_json           TEXT    DEFAULT '[]',  -- operator-ratified amendment audit trail (append-only)
    critical                    INTEGER DEFAULT 0,     -- triage-priority flag
    -- D5.5: staged convoy support. staging_mode='single' = legacy behavior (one ConvoyStage at stage 1, gate=null);
    -- staging_mode='staged' = N stages with gates per ConvoyStages. staging_strategy is the forward-compat enum;
    -- only 'strict' (stage N opens after stage N-1's PRs merge AND gate passes) is implemented in D5.5.
    staging_mode                TEXT    NOT NULL DEFAULT 'single',
    staging_strategy            TEXT    NOT NULL DEFAULT 'strict',
    created_at                  TEXT    DEFAULT (datetime('now'))
);

-- Convoy status values:
--   'Active'            — tasks running
--   'AwaitingDraftPR'   — all sub-PRs merged; Diplomat enqueued
--   'DraftPROpen'       — draft PR exists, waiting for human "Ship it"
--   'Shipped'           — draft PR merged; cleanup may still be pending
--   'Abandoned'         — draft PR closed without merge
--   'Completed'         — legacy pre-PR-flow convoys (ask_branch = '')
--   'Failed'            — one or more tasks Failed/Escalated

-- ── Repository registry ───────────────────────────────────────────────────────
-- PR-flow fields are populated by Layer B backfill at daemon startup (remote_url,
-- default_branch) and by the FindPRTemplate task (pr_template_path). pr_flow_enabled
-- defaults to 1 — repos opt OUT of the PR flow, not in.

CREATE TABLE IF NOT EXISTS Repositories (
    name                  TEXT PRIMARY KEY,
    local_path            TEXT,
    description           TEXT,
    remote_url            TEXT    DEFAULT '',  -- populated by `git remote get-url origin` at startup
    default_branch        TEXT    DEFAULT '',  -- populated by `git symbolic-ref refs/remotes/origin/HEAD`
    pr_template_path      TEXT    DEFAULT '',  -- populated by FindPRTemplate task (may be '' if repo has no template)
    pr_flow_enabled       INTEGER DEFAULT 1,   -- 0 = legacy local-merge, 1 = new PR flow (default)
    quarantined_at        TEXT    DEFAULT '',  -- set by repo-config-check when repo becomes unhealthy
    quarantine_reason     TEXT    DEFAULT '',
    pr_review_enabled     INTEGER DEFAULT 1,   -- 0 = opt-out of PR review-comment triage (AUDIT-023, Fix #4)
    mode                  TEXT    NOT NULL DEFAULT 'read_only' CHECK (mode IN ('read_only','write','quarantined')), -- D2 T1-4
    license               TEXT    NOT NULL DEFAULT '',  -- D5 P0: SPDX id detected from LICENSE file at AddRepo time; backfilled on first runMigrations after upgrade
    release_label_pattern TEXT    NOT NULL DEFAULT '',  -- D5.5: per-repo regex for the release_label_present gate; empty means repo doesn't use release labels
    archaeologist_sweep_disabled INTEGER NOT NULL DEFAULT 0,  -- D9: 1 = operator opt-out of Archaeologist's weekly debt-pattern sweep
    handoff_synthesis_enabled INTEGER NOT NULL DEFAULT 0  -- D10: per-repo opt-in for Diplomat PRHandoffSynthesis + dogArchitectureDocRender. Default 0 (OFF) — D10 ships opt-in.
);

-- ── Ask-branch sub-PR tracking ────────────────────────────────────────────────
-- One row per astromech sub-PR opened against a convoy's ask-branch. Tracks
-- CI state, retry counters, and terminal state transitions.

CREATE TABLE IF NOT EXISTS AskBranchPRs (
    id                    INTEGER PRIMARY KEY AUTOINCREMENT,
    task_id               INTEGER NOT NULL,              -- FK → BountyBoard.id
    convoy_id             INTEGER NOT NULL,              -- FK → Convoys.id
    repo                  TEXT    NOT NULL,              -- FK → Repositories.name
    pr_number             INTEGER DEFAULT 0,
    pr_url                TEXT    DEFAULT '',
    state                 TEXT    DEFAULT 'Open',        -- 'Open' | 'Merged' | 'Closed'
    checks_state          TEXT    DEFAULT 'Pending',     -- 'Pending' | 'Success' | 'Failure'
    failure_count         INTEGER DEFAULT 0,             -- incremented by Medic CIFailureTriage
    stall_retrigger_count INTEGER DEFAULT 0,             -- stuck-CI empty-commit retries (AUDIT-080)
    spawned_fix_count     INTEGER DEFAULT 0,             -- Fix #7: concurrent Flaky→RealBug fix-task spawn guard
    merged_at             TEXT    DEFAULT '',
    created_at            TEXT    DEFAULT (datetime('now')),
    UNIQUE(repo, pr_number)
);
CREATE INDEX IF NOT EXISTS idx_ask_branch_prs_task_id   ON AskBranchPRs (task_id);
CREATE INDEX IF NOT EXISTS idx_ask_branch_prs_convoy_id ON AskBranchPRs (convoy_id);
CREATE INDEX IF NOT EXISTS idx_ask_branch_prs_state     ON AskBranchPRs (state);
-- escalation-sweeper's `GROUP BY task_id / MAX(id)` can read the latest sub-PR
-- per task from this composite index without a sort (AUDIT-024 addendum, Fix #4).
CREATE INDEX IF NOT EXISTS idx_ask_branch_prs_task_id_id_desc ON AskBranchPRs (task_id, id DESC);

-- ── Per-(convoy, repo) ask-branch tracking ────────────────────────────────────
-- A convoy's tasks may target multiple repos, so each touched repo gets its own
-- ask-branch and eventually its own draft PR. One row per (convoy, repo).

CREATE TABLE IF NOT EXISTS ConvoyAskBranches (
    convoy_id              INTEGER NOT NULL,         -- FK → Convoys.id
    repo                   TEXT    NOT NULL,         -- FK → Repositories.name
    ask_branch             TEXT    NOT NULL,         -- 'force/ask-<id>-<slug>'
    ask_branch_base_sha    TEXT    NOT NULL,         -- main's HEAD at branch creation; updated by rebase
    draft_pr_url           TEXT    DEFAULT '',
    draft_pr_number        INTEGER DEFAULT 0,
    draft_pr_state         TEXT    DEFAULT '',        -- '' | 'Open' | 'Merged' | 'Closed'
    shipped_at             TEXT    DEFAULT '',
    last_rebased_at        TEXT    DEFAULT '',
    failed_rebase_attempts INTEGER DEFAULT 0,        -- Fix #6: bounded conflict-retry counter; main-drift-watch/runRebaseAskBranch escalate past maxAskBranchConflicts (3)
    stage_id               INTEGER,                  -- FK → ConvoyStages.id (D5.5); legacy single-mode convoys point at their auto-created stage 1
    created_at             TEXT    DEFAULT (datetime('now')),
    PRIMARY KEY (convoy_id, repo)
);
CREATE INDEX IF NOT EXISTS idx_convoy_ask_branches_repo     ON ConvoyAskBranches (repo);
CREATE INDEX IF NOT EXISTS idx_convoy_ask_branches_stage_id ON ConvoyAskBranches (stage_id);

-- ── ConvoyStages (D5.5) ───────────────────────────────────────────────────────
-- Commander-drafted ordered phase pipeline for a convoy. One row per stage;
-- stage_num is 1-indexed and ordered by execution. Status progresses
-- Pending → Open → AllPRsMerged → AwaitingGate → GatePassed → Verified.
-- Any state may transition to Failed (terminal). Forward-compat migration
-- creates a single Open stage 1 row with gate_type=NULL for every existing
-- (single-mode) convoy.

CREATE TABLE IF NOT EXISTS ConvoyStages (
    id                   INTEGER PRIMARY KEY AUTOINCREMENT,
    convoy_id            INTEGER NOT NULL REFERENCES Convoys(id),
    stage_num            INTEGER NOT NULL,             -- 1-indexed
    intent_text          TEXT    NOT NULL DEFAULT '',  -- Commander's reason for this stage
    status               TEXT    NOT NULL DEFAULT 'Pending', -- Pending|Open|AllPRsMerged|AwaitingGate|GatePassed|Verified|Failed
    gate_type            TEXT,                          -- soak_minutes|operator_confirm|all_of|any_of|null (terminal-stage only) | future P3 leaves
    gate_config_json     TEXT    NOT NULL DEFAULT '{}', -- per-gate-type config
    gate_timeout_minutes INTEGER NOT NULL DEFAULT 10080, -- 7 days default; escalation after
    opened_at            TEXT,
    all_prs_merged_at    TEXT,
    gate_passed_at       TEXT,
    completed_at         TEXT,
    UNIQUE(convoy_id, stage_num)
);
CREATE INDEX IF NOT EXISTS idx_convoy_stages_convoy_id ON ConvoyStages (convoy_id);
CREATE INDEX IF NOT EXISTS idx_convoy_stages_status    ON ConvoyStages (status);

-- ── Persistent agent worktrees ────────────────────────────────────────────────
-- Astromechs reuse their worktree across tasks for the same repo.

CREATE TABLE IF NOT EXISTS Agents (
    agent_name    TEXT NOT NULL,
    repo          TEXT NOT NULL,
    worktree_path TEXT NOT NULL,
    PRIMARY KEY (agent_name, repo)
);

-- ── Per-task attempt history ──────────────────────────────────────────────────
-- Full Claude output for every attempt (seance). Used for debugging and context injection.

CREATE TABLE IF NOT EXISTS TaskHistory (
    id                INTEGER PRIMARY KEY AUTOINCREMENT,
    task_id           INTEGER NOT NULL,
    attempt           INTEGER NOT NULL,
    agent             TEXT    NOT NULL,
    session_id        TEXT    NOT NULL,
    claude_output     TEXT    NOT NULL,
    outcome           TEXT    NOT NULL,  -- 'Completed' | 'Failed' | 'Escalated' | 'Sharded' | 'Timeout' | 'Rejected'
    tokens_in         INTEGER DEFAULT 0,
    tokens_out        INTEGER DEFAULT 0,
    cost_usd_estimate REAL    DEFAULT 0,  -- D2 T1-1: per-attempt cost in USD from claude.pricing.CostUSD(model, in, out)
    memory_ids        TEXT    DEFAULT '', -- CSV of FleetMemory.id values injected into this attempt's prompt
    prompt_version    TEXT    DEFAULT '', -- D3 P1: per-prompt-version metric correlation
    created_at        TEXT    DEFAULT (datetime('now'))
);
-- Hot-table indexes (AUDIT-010, Fix #4).
CREATE INDEX IF NOT EXISTS idx_taskhistory_task_id       ON TaskHistory (task_id);
CREATE INDEX IF NOT EXISTS idx_taskhistory_created_at    ON TaskHistory (created_at);
CREATE INDEX IF NOT EXISTS idx_taskhistory_outcome_agent ON TaskHistory (outcome, agent);

-- ── Per-task spend anomaly ledger (D2 T1-1) ──────────────────────────────────
-- One row per (task_id, window_start) when dogTaskSpendWatch detects a 10-min
-- trailing window cost above per_task_spend_alert_usd. notified_at is the
-- idempotency anchor — a subsequent dog tick within the same window finds the
-- existing row and skips re-mailing.
CREATE TABLE IF NOT EXISTS TaskSpendWatch (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    task_id      INTEGER NOT NULL,
    window_start TEXT    NOT NULL,
    cost_usd     REAL    DEFAULT 0,
    notified_at  TEXT    DEFAULT (datetime('now'))
);
CREATE INDEX IF NOT EXISTS idx_taskspendwatch_task_window ON TaskSpendWatch (task_id, window_start);

-- ── Escalations ───────────────────────────────────────────────────────────────
-- State machine: Open → Acknowledged → Closed.
-- 'Resolved' is a legacy value that three self-healing sinks used to write;
-- no read-side consumer recognised it, so rows accumulated invisibly. Campaign 2
-- (AUDIT-025) collapsed every writer onto 'Closed' and added a migration that
-- UPDATEs any lingering 'Resolved' rows → 'Closed'. Do not reintroduce 'Resolved'.
-- auto_resolve_count (AUDIT-149) is incremented exactly once when the
-- escalation-sweeper auto-closes a row; a gate (count < 1) on the sweeper's
-- UPDATE skips rows the operator has re-opened for deeper investigation so
-- they aren't silently re-closed on the next 10-min tick.

CREATE TABLE IF NOT EXISTS Escalations (
    id                 INTEGER PRIMARY KEY AUTOINCREMENT,
    task_id            INTEGER NOT NULL,
    severity           TEXT    NOT NULL,  -- 'LOW' | 'MEDIUM' | 'HIGH'
    message            TEXT    NOT NULL,
    status             TEXT    DEFAULT 'Open',  -- 'Open' | 'Acknowledged' | 'Closed'
    auto_resolve_count INTEGER DEFAULT 0,       -- AUDIT-149: one-shot auto-close budget
    created_at         TEXT    DEFAULT (datetime('now')),
    acknowledged_at    TEXT    DEFAULT ''
);
-- Hot-table indexes (AUDIT-024, Fix #4). Sweeper runs `WHERE status='Open'`
-- every 10 minutes + joins back to BountyBoard by task_id.
CREATE INDEX IF NOT EXISTS idx_escalations_status  ON Escalations (status);
CREATE INDEX IF NOT EXISTS idx_escalations_task_id ON Escalations (task_id);

-- Fix #3 (AUDIT-034): at most one Open escalation per task. CreateEscalation
-- uses ON CONFLICT DO UPDATE SET severity=MAX(old,new), message=excluded.message
-- so repeated self-healing paths merge into one row instead of spamming
-- the operator inbox with N identical alerts.
CREATE UNIQUE INDEX IF NOT EXISTS idx_escalations_open_task
    ON Escalations(task_id) WHERE status = 'Open';

-- ── Fleet mail ────────────────────────────────────────────────────────────────
-- Structured inter-agent messaging. Agents read their inbox at the start of each task.

CREATE TABLE IF NOT EXISTS Fleet_Mail (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    from_agent   TEXT    NOT NULL,
    to_agent     TEXT    NOT NULL,       -- agent name | role | 'all'
    subject      TEXT    NOT NULL DEFAULT '',
    body         TEXT    NOT NULL DEFAULT '',
    task_id      INTEGER DEFAULT 0,      -- 0 = standing order; >0 = task-specific
    message_type TEXT    NOT NULL DEFAULT 'info',  -- 'directive' | 'feedback' | 'alert' | 'remediation' | 'info'
    read_at      TEXT    DEFAULT '',     -- '' = operator-unread (UI display only)
    consumed_at  TEXT    DEFAULT '',     -- '' = not yet consumed by an agent
    created_at   TEXT    DEFAULT (datetime('now'))
);
-- Hot-table indexes (AUDIT-024, Fix #4).
CREATE INDEX IF NOT EXISTS idx_mail_to_consumed ON Fleet_Mail (to_agent, consumed_at);
CREATE INDEX IF NOT EXISTS idx_mail_task_id     ON Fleet_Mail (task_id);
CREATE INDEX IF NOT EXISTS idx_mail_created_at  ON Fleet_Mail (created_at);

-- ── System configuration ──────────────────────────────────────────────────────
-- Runtime knobs: e-stop, max_concurrent, num_astromech, num_captain, etc.
-- Fix #1 added:
--   hourly_spend_cap_usd       (default 25.0)  — agent claim loops skip-and-sleep
--                                                when trailing-hour spend exceeds
--   hourly_spend_estop_usd     (default 200.0) — spend-burn-watch dog auto-flips
--                                                e-stop when trailing-hour spend
--                                                exceeds this hard cap
--   spend_cap_last_alert_hour  (auto)          — dedup key for operator warning
--                                                mail (hour-of-last-alert)

CREATE TABLE IF NOT EXISTS SystemConfig (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);

-- ── Operator audit log ────────────────────────────────────────────────────────

CREATE TABLE IF NOT EXISTS AuditLog (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    actor      TEXT    NOT NULL DEFAULT 'operator',
    action     TEXT    NOT NULL,
    task_id    INTEGER DEFAULT 0,
    detail     TEXT    DEFAULT '',
    created_at TEXT    DEFAULT (datetime('now'))
);
-- Hot-table indexes (AUDIT-024, Fix #4).
CREATE INDEX IF NOT EXISTS idx_auditlog_created_at ON AuditLog (created_at);
CREATE INDEX IF NOT EXISTS idx_auditlog_task_id    ON AuditLog (task_id);

-- ── Dog agent state ───────────────────────────────────────────────────────────
-- Cooldown tracking for periodic background dogs (log rotation, WAL checkpoint, etc.)

CREATE TABLE IF NOT EXISTS Dogs (
    name         TEXT PRIMARY KEY,
    last_run_at  TEXT    DEFAULT '',
    run_count    INTEGER DEFAULT 0,
    heartbeat_at TEXT    DEFAULT ''
);

-- ── Fleet memory ─────────────────────────────────────────────────────────────
-- Lessons learned from completed/failed tasks, injected into future agents on same repo.

CREATE TABLE IF NOT EXISTS FleetMemory (
    id                INTEGER PRIMARY KEY AUTOINCREMENT,
    repo              TEXT    NOT NULL,
    task_id           INTEGER DEFAULT 0,
    outcome           TEXT    NOT NULL DEFAULT 'success',  -- 'success' | 'failure'
    summary           TEXT    NOT NULL,
    files_changed     TEXT    DEFAULT '',   -- comma-separated affected file paths (success only)
    topic_tags        TEXT    DEFAULT '',   -- comma-separated 3-6 short keywords from Librarian (e.g. "auth, jwt, middleware")
    embedding         BLOB    DEFAULT NULL, -- reserved: float32 vector for future sqlite-vec upgrade
    created_at        TEXT    DEFAULT (datetime('now')),
    -- D4 Phase 0 — Librarian evolution: quality-scoring columns.
    -- freshness_score decays from 1.0 with row age (RecomputeFreshnessScores dog).
    -- validation_score adjusted by RecordValidation (positive/negative outcome feedback).
    -- retrieval_count + last_retrieved_at bumped by RecordRetrieval at memory-injection time.
    -- canonical_id is set by DedupAndMerge to point a non-canonical row at its survivor.
    -- hypothesis_emitted_at stamped by EmitHypothesisCandidates so re-runs don't duplicate.
    freshness_score   REAL    NOT NULL DEFAULT 1.0,
    validation_score  REAL    NOT NULL DEFAULT 0.0,
    retrieval_count   INTEGER NOT NULL DEFAULT 0,
    last_retrieved_at TEXT    DEFAULT '',
    canonical_id      INTEGER NOT NULL DEFAULT 0,
    hypothesis_emitted_at TEXT DEFAULT ''
);
-- Hot-table index (AUDIT-024, Fix #4) — per-repo recency retrieval.
CREATE INDEX IF NOT EXISTS idx_fleet_memory_repo_created ON FleetMemory (repo, created_at);

-- FTS5 full-text search index over fleet memory (requires build tag: sqlite_fts5)
-- Kept in sync explicitly by StoreFleetMemory — not a content table.
-- topic_tags broadens recall: a memory tagged "auth, jwt" matches a query
-- about "login" or "authentication" even when the summary prose uses
-- different vocabulary.
CREATE VIRTUAL TABLE IF NOT EXISTS FleetMemory_fts USING fts5(
    summary,
    files_changed,
    topic_tags
);

-- ── Task notes ────────────────────────────────────────────────────────────────
-- Operator notes on a task, injected into the agent's context at claim time.

CREATE TABLE IF NOT EXISTS TaskNotes (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    task_id    INTEGER NOT NULL REFERENCES BountyBoard(id) ON DELETE CASCADE,
    note       TEXT    NOT NULL,
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP
);

-- ── Chancellor convoy ordering ───────────────────────────────────────────────
-- FeatureBlockers: a convoy is blocked until a Feature (not yet a convoy) completes.
-- Created by Chancellor when approving a convoy that depends on an unplanned Feature.
-- Resolved (and real TaskDependencies wired) when the blocking Feature's convoy is approved.

CREATE TABLE IF NOT EXISTS FeatureBlockers (
    id                  INTEGER PRIMARY KEY AUTOINCREMENT,
    blocked_convoy_id   INTEGER NOT NULL,  -- convoy whose tasks cannot be claimed
    blocking_feature_id INTEGER NOT NULL,  -- Feature that must land first (BountyBoard.id)
    resolved_at         DATETIME,          -- NULL until resolved
    created_at          DATETIME DEFAULT (datetime('now'))
);

CREATE INDEX IF NOT EXISTS idx_feature_blockers_convoy   ON FeatureBlockers (blocked_convoy_id);
CREATE INDEX IF NOT EXISTS idx_feature_blockers_feature  ON FeatureBlockers (blocking_feature_id);

-- Fix #3 (AUDIT-036): partial UNIQUE on unresolved (blocked, blocking) pair.
-- Before Fix #3, CreateFeatureBlocker used INSERT OR IGNORE against no UNIQUE
-- constraint — ResolveFeatureBlockers re-runs produced duplicate rows that
-- its own iteration then re-injected as cross-convoy dependencies twice.
CREATE UNIQUE INDEX IF NOT EXISTS idx_feature_blockers_open
    ON FeatureBlockers(blocked_convoy_id, blocking_feature_id) WHERE resolved_at IS NULL;

-- ConvoyHolds: hard stop for Captain and Council — reject any task from a held convoy.
-- Set alongside FeatureBlockers (or standalone for retroactive holds).
-- Cleared when all FeatureBlockers for a convoy are resolved.

CREATE TABLE IF NOT EXISTS ConvoyHolds (
    convoy_id   INTEGER PRIMARY KEY,
    reason      TEXT    NOT NULL,
    created_at  DATETIME DEFAULT (datetime('now'))
);

-- ── Proposed convoys ──────────────────────────────────────────────────────────
-- Commander stores its plan here instead of creating a convoy directly.
-- The Supreme Chancellor reviews and approves, sequences, or merges proposals.

CREATE TABLE IF NOT EXISTS ProposedConvoys (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    feature_id  INTEGER NOT NULL UNIQUE,              -- references BountyBoard.id (Feature task)
    plan_json   TEXT    NOT NULL,                     -- JSON array of TaskPlan from Commander
    status      TEXT    NOT NULL DEFAULT 'pending',   -- pending | approved | rejected | merged
    created_at  DATETIME DEFAULT (datetime('now'))
);

-- ── PR review comments (bot + human triage) ──────────────────────────────────
-- Per-comment state for review comments on draft PRs. Unified for bot and
-- human authors; author_kind discriminates. Bot comments are classified and
-- replied to automatically. Human comments are classified + drafted-reply is
-- stored but NEVER posted — surfaced on the dashboard for operator action.
-- review_thread_id + thread_depth drive the back-and-forth loop detector
-- (see agents/pr_review_triage.go).

CREATE TABLE IF NOT EXISTS PRReviewComments (
    id                     INTEGER PRIMARY KEY AUTOINCREMENT,
    convoy_id              INTEGER NOT NULL,
    repo                   TEXT    NOT NULL,
    draft_pr_number        INTEGER NOT NULL,
    github_comment_id      INTEGER NOT NULL,    -- stable ID from GitHub
    comment_type           TEXT    NOT NULL,    -- 'review_comment' | 'issue_comment'
    author                 TEXT    NOT NULL,    -- GitHub login
    author_kind            TEXT    NOT NULL,    -- 'bot' | 'human'
    body                   TEXT    NOT NULL,
    path                   TEXT    DEFAULT '',  -- file path (review_comment only)
    line                   INTEGER DEFAULT 0,   -- line number (review_comment only)
    diff_hunk              TEXT    DEFAULT '',  -- inline diff context
    review_thread_id       TEXT    DEFAULT '',  -- GitHub review thread (GraphQL node id) or 'issue:<id>' synthetic
    in_reply_to_comment_id INTEGER DEFAULT 0,   -- parent comment if this is a reply
    thread_depth           INTEGER DEFAULT 0,   -- # of fleet-authored fixes already in this thread
    classification         TEXT    DEFAULT '',  -- '' | 'in_scope_fix' | 'out_of_scope' | 'not_actionable' | 'conflicted_loop' | 'human' | 'ignored'
    classification_reason  TEXT    DEFAULT '',
    classify_attempts      INTEGER DEFAULT 0,   -- Fix #7 (AUDIT-032): bounds transient classifier retries
    spawned_task_id        INTEGER DEFAULT 0,   -- CodeEdit (in_scope) or Feature (out_of_scope)
    reply_body             TEXT    DEFAULT '',  -- the reply text (DRAFT for humans, POSTED for bots)
    replied_at             TEXT    DEFAULT '',  -- empty for humans (never posted) and conflicted_loop
    thread_resolved_at     TEXT    DEFAULT '',  -- populated after GraphQL resolve sweep
    created_at             TEXT    DEFAULT (datetime('now')),
    UNIQUE(repo, draft_pr_number, github_comment_id)
);
CREATE INDEX IF NOT EXISTS idx_pr_review_comments_convoy ON PRReviewComments (convoy_id);
CREATE INDEX IF NOT EXISTS idx_pr_review_comments_thread ON PRReviewComments (review_thread_id);

-- Repositories gains pr_review_enabled (default 1). Set to 0 to opt a repo out
-- of the PR review-comment triage flow.
-- ALTER TABLE Repositories ADD COLUMN pr_review_enabled INTEGER DEFAULT 1;

-- ── Prompt-byte attribution (D2 T1-2) ─────────────────────────────────────────
-- One row per (LLM call, source_tag) breakdown so an operator can see
-- "captain's last call was 60% file_read, 25% claude_md, 10% task_payload".
-- The dashboard's per-agent prompt byte budget view aggregates this table over
-- a rolling window. task_id is 0 for context-less calls (boot, classifier).
-- source_tag is constrained at the application layer to a fixed enum
-- (claude_md / librarian_memory / task_payload / file_read / fleet_rules /
-- senate_context / scope_guard / other); the schema keeps it TEXT to permit
-- a future migration of the enum without a destructive table rebuild.

CREATE TABLE IF NOT EXISTS PromptByteAttribution (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    task_id         INTEGER NOT NULL DEFAULT 0,
    agent_name      TEXT    NOT NULL,
    call_timestamp  TEXT    NOT NULL DEFAULT (datetime('now')),
    source_tag      TEXT    NOT NULL,
    bytes           INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_prompt_byte_attr_task     ON PromptByteAttribution (task_id, call_timestamp);
CREATE INDEX IF NOT EXISTS idx_prompt_byte_attr_agent_ts ON PromptByteAttribution (agent_name, call_timestamp);

-- ── D3 Phase 1 — paired-runs core schema ──────────────────────────────────────
-- Tables for the experiment / treatment / metric primitive (paired-runs.md
-- § Data Model). Phase 1 lands these as data-layer prerequisites; the log-only
-- treatments.Apply wiring in Phase 4 is the first writer. Subsequent phases
-- (single-treatment experiments, EC, factorial, paired shadow) build against
-- these tables.

CREATE TABLE IF NOT EXISTS Experiments (
    id                          INTEGER PRIMARY KEY AUTOINCREMENT,
    name                        TEXT    NOT NULL,
    hypothesis_text             TEXT    NOT NULL DEFAULT '',
    min_practical_effect        REAL    DEFAULT 0,
    stakes_tier                 TEXT    NOT NULL DEFAULT 'low',  -- low|medium|high|safety_critical
    declare_threshold_override  REAL,                            -- nullable; operator-approved
    factorial_dimensions_json   TEXT    DEFAULT '[]',
    kind                        TEXT    NOT NULL DEFAULT 'single' CHECK (kind IN ('single','factorial')),  -- D3 P4
    factors_json                TEXT    DEFAULT '[]',            -- D3 P4: factor catalog [{name, levels}]
    subject_agent               TEXT    NOT NULL DEFAULT '',     -- 'captain'|'chancellor'|...
    assignment_unit             TEXT    NOT NULL DEFAULT 'task', -- 'feature'|'convoy'|'task'
    analysis_framework_version  TEXT    DEFAULT '',
    status                      TEXT    NOT NULL DEFAULT 'authored',
    termination_reason          TEXT    DEFAULT '',
    budget_usd                  REAL    DEFAULT 0,
    hard_cap_usd                REAL    DEFAULT 0,
    duration_cap_hours          INTEGER DEFAULT 0,
    confirm_phase_id            INTEGER DEFAULT 0,
    created_by                  TEXT    NOT NULL DEFAULT '',
    created_at                  TEXT    DEFAULT (datetime('now')),
    ratified_at                 TEXT    DEFAULT '',
    ratified_by                 TEXT    DEFAULT '',
    started_at                  TEXT    DEFAULT '',
    terminated_at               TEXT    DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_experiments_status      ON Experiments (status);
CREATE INDEX IF NOT EXISTS idx_experiments_subject     ON Experiments (subject_agent, status);
CREATE INDEX IF NOT EXISTS idx_experiments_kind_status ON Experiments (kind, status);

-- TreatmentSpecs — content-snapshotted treatment definitions. spec_hash is
-- unique so identical treatments across experiments share rows.
CREATE TABLE IF NOT EXISTS TreatmentSpecs (
    id                       INTEGER PRIMARY KEY AUTOINCREMENT,
    spec_hash                TEXT    UNIQUE NOT NULL,            -- SHA256 of normalised spec
    prompt_template_ref      TEXT    DEFAULT '',                 -- 'captain/default@<git-sha>'
    prompt_template_content  TEXT    DEFAULT '',                 -- frozen snapshot
    rule_set_refs_json       TEXT    DEFAULT '[]',               -- JSON array of FleetRules.id
    memory_bundle_ref        TEXT    DEFAULT '',
    memory_bundle_content    TEXT    DEFAULT '',
    model_identifier         TEXT    DEFAULT '',
    max_turns                INTEGER DEFAULT 0,
    context_size_bytes       INTEGER DEFAULT 0,
    tool_availability_json   TEXT    DEFAULT '[]',
    routing_thresholds_json  TEXT    DEFAULT '{}',
    created_at               TEXT    DEFAULT (datetime('now'))
);

CREATE TABLE IF NOT EXISTS ExperimentTreatments (
    id                    INTEGER PRIMARY KEY AUTOINCREMENT,
    experiment_id         INTEGER NOT NULL,
    arm_label             TEXT    NOT NULL,                      -- 'control', 'tight_rules', ...
    cell_json             TEXT    DEFAULT '{}',                  -- {"prompt":"B","rules":"on"}
    treatment_spec_id     INTEGER NOT NULL,                      -- FK → TreatmentSpecs.id
    target_cell_weight    REAL    DEFAULT 0                      -- 0.25 for balanced 2x2
);
CREATE INDEX IF NOT EXISTS idx_exp_treatments_exp ON ExperimentTreatments (experiment_id);

CREATE TABLE IF NOT EXISTS ExperimentMetrics (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    experiment_id   INTEGER NOT NULL,
    metric_name     TEXT    NOT NULL,
    metric_version  TEXT    NOT NULL,                            -- resolved at experiment start
    direction       TEXT    NOT NULL DEFAULT 'higher_is_better', -- 'higher_is_better'|'lower_is_better'
    params_json     TEXT    DEFAULT '{}',
    is_primary      INTEGER DEFAULT 0                            -- exactly one per experiment drives declare-winner
);
CREATE INDEX IF NOT EXISTS idx_exp_metrics_exp ON ExperimentMetrics (experiment_id);

CREATE TABLE IF NOT EXISTS ExperimentRuns (
    id                       INTEGER PRIMARY KEY AUTOINCREMENT,
    experiment_id            INTEGER NOT NULL,
    treatment_id             INTEGER NOT NULL,
    cell_json                TEXT    DEFAULT '{}',
    natural_unit_kind        TEXT    NOT NULL,                   -- 'feature'|'convoy'|'task'
    natural_unit_id          INTEGER NOT NULL,
    mode                     TEXT    NOT NULL DEFAULT 'holdout', -- 'holdout'|'paired_real'|'paired_shadow'
    paired_with_run_id       INTEGER DEFAULT 0,                  -- self-FK for paired mode
    agent_name               TEXT    NOT NULL DEFAULT '',
    assigned_at              TEXT    DEFAULT (datetime('now')),
    completed_at             TEXT    DEFAULT '',
    score                    REAL,                               -- frozen at scoring time
    score_source             TEXT    DEFAULT '',                 -- 'downstream_verdict'|'llm_judge'|'operator_ratification'
    metric_version           TEXT    DEFAULT '',
    model_substituted_from   TEXT    DEFAULT '',                 -- holdout model substitutions
    model_substituted_to     TEXT    DEFAULT '',
    is_provisional           INTEGER DEFAULT 0                   -- true for llm_judge pending downstream
);
CREATE INDEX IF NOT EXISTS idx_exp_runs_exp_treat ON ExperimentRuns (experiment_id, treatment_id);
CREATE INDEX IF NOT EXISTS idx_exp_runs_unit      ON ExperimentRuns (natural_unit_kind, natural_unit_id);

-- ExperimentInteractions — D3 Phase 4. Per-(factor pair, level pair) interaction
-- estimates for factorial experiments. The 2-way interaction
-- [mean(D1=a,D2=b) - mean(D1=a',D2=b)] - [mean(D1=a,D2=b') - mean(D1=a',D2=b')]
-- is decomposed into per-cell contrasts so 3+-level factors can store the full
-- interaction surface (not just a 2x2 contrast scalar). Single-treatment
-- experiments never write rows here. See paired-runs.md § Factorial Scoring.
CREATE TABLE IF NOT EXISTS ExperimentInteractions (
    id                       INTEGER PRIMARY KEY AUTOINCREMENT,
    experiment_id            INTEGER NOT NULL,
    factor_a                 TEXT    NOT NULL,
    factor_b                 TEXT    NOT NULL,
    level_a                  TEXT    NOT NULL DEFAULT '',
    level_b                  TEXT    NOT NULL DEFAULT '',
    interaction_estimate     REAL    DEFAULT 0,
    posterior_alpha          REAL    DEFAULT 0,
    posterior_beta           REAL    DEFAULT 0,
    posterior_prob_nonzero   REAL    DEFAULT 0,                  -- P(|interaction| > min_practical) under joint posterior
    computed_at              TEXT    DEFAULT (datetime('now'))
);
CREATE INDEX IF NOT EXISTS idx_exp_interactions_exp  ON ExperimentInteractions (experiment_id);
CREATE INDEX IF NOT EXISTS idx_exp_interactions_pair ON ExperimentInteractions (experiment_id, factor_a, factor_b);

CREATE TABLE IF NOT EXISTS ExperimentOutcomes (
    id                          INTEGER PRIMARY KEY AUTOINCREMENT,
    experiment_id               INTEGER NOT NULL UNIQUE,
    terminated_at               TEXT    DEFAULT (datetime('now')),
    termination_reason          TEXT    NOT NULL,                -- declared_winner|declared_null|inconclusive|budget_exhausted|emergency_stop|operator_closed
    winner_treatment_id         INTEGER DEFAULT 0,
    winner_posterior            REAL,
    winner_effect_estimate      REAL,
    cell_means_json             TEXT    DEFAULT '{}',
    fleet_state_hash_at_start   TEXT    DEFAULT '',              -- FK → FleetStateSnapshots.state_hash
    fleet_state_hash_at_end     TEXT    DEFAULT '',
    confirm_phase_outcome       TEXT    DEFAULT '',
    promotion_proposal_id       INTEGER DEFAULT 0
);

-- AnalysisFrameworks — versioned algorithm config; published frameworks are
-- immutable (deprecated_at marks retirement).
CREATE TABLE IF NOT EXISTS AnalysisFrameworks (
    version           TEXT PRIMARY KEY,                          -- '2026-04-23'
    config_content    TEXT    NOT NULL,
    config_hash       TEXT    NOT NULL,
    algorithm_git_sha TEXT    DEFAULT '',
    published_at      TEXT    DEFAULT (datetime('now')),
    published_by      TEXT    NOT NULL DEFAULT '',
    description       TEXT    DEFAULT '',
    deprecated_at     TEXT    DEFAULT ''
);

-- MetricVersions — versioned (metric_name, version) pairs. SQL body + test SQL
-- + manifest JSON snapshotted at publish time.
CREATE TABLE IF NOT EXISTS MetricVersions (
    metric_name    TEXT NOT NULL,
    version        TEXT NOT NULL,
    sql_content    TEXT NOT NULL,
    test_content   TEXT DEFAULT '',
    manifest_json  TEXT DEFAULT '{}',
    published_at   TEXT DEFAULT (datetime('now')),
    published_by   TEXT DEFAULT '',
    description    TEXT DEFAULT '',
    deprecated_at  TEXT DEFAULT '',
    PRIMARY KEY (metric_name, version)
);

-- FleetStateSnapshots — content-addressed snapshots of fleet rule / memory /
-- model / prompt manifests at experiment start/end. state_hash is the FK key
-- referenced by ExperimentOutcomes.
CREATE TABLE IF NOT EXISTS FleetStateSnapshots (
    state_hash                    TEXT PRIMARY KEY,
    computed_at                   TEXT DEFAULT (datetime('now')),
    active_rules_manifest_json    TEXT DEFAULT '{}',             -- hash per rule_key
    active_memories_manifest_json TEXT DEFAULT '{}',             -- hash per repo memory
    active_models_manifest_json   TEXT DEFAULT '{}',             -- model per agent
    active_prompts_manifest_json  TEXT DEFAULT '{}',             -- prompt version per agent
    agent_binary_git_sha          TEXT DEFAULT ''
);

-- GlobalHoldouts — long-term reference cohorts (e.g. baseline-2026).
-- See paired-runs.md § Global Holdout for ramp-up / plateau / fade semantics.
CREATE TABLE IF NOT EXISTS GlobalHoldouts (
    id                 INTEGER PRIMARY KEY AUTOINCREMENT,
    name               TEXT UNIQUE NOT NULL,                     -- 'baseline-2026'
    reference_date     TEXT DEFAULT (datetime('now')),
    fleet_state_hash   TEXT DEFAULT '',                          -- FK → FleetStateSnapshots
    ramp_up_days       INTEGER DEFAULT 7,
    plateau_fraction   REAL    DEFAULT 0.02,
    fade_start_at      TEXT    DEFAULT '',
    fade_days          INTEGER DEFAULT 90,
    retired_at         TEXT    DEFAULT '',
    retired_reason     TEXT    DEFAULT '',
    created_by         TEXT    DEFAULT '',
    notes              TEXT    DEFAULT ''
);

-- ModelAvailability — health-watch ledger for model identifiers. Models are
-- the uniquely fragile treatment dimension — we don't control their
-- availability. Updated by a model-availability dog.
CREATE TABLE IF NOT EXISTS ModelAvailability (
    model_id                 TEXT PRIMARY KEY,
    last_checked_at          TEXT DEFAULT '',
    last_success_at          TEXT DEFAULT '',
    deprecation_detected_at  TEXT DEFAULT '',
    announced_kill_at        TEXT DEFAULT '',
    successor_suggested      TEXT DEFAULT ''
);

-- TreatmentApplyLog — log-only audit trail for treatments.Apply. Phase 4 of
-- D3 ships log-only mode (records the call descriptor + assignment intent
-- without mutating the call). Phase 2 flips this live; the log row stays as
-- the source-of-truth audit record. Mentioned in D3 Phase 4's implementation
-- prompt; not in paired-runs.md schema block — added here so log-only writes
-- have a permanent home that does not corrupt live ExperimentRuns data.
CREATE TABLE IF NOT EXISTS TreatmentApplyLog (
    id                  INTEGER PRIMARY KEY AUTOINCREMENT,
    applied_at          TEXT    DEFAULT (datetime('now')),
    agent_name          TEXT    NOT NULL,
    natural_unit_kind   TEXT    DEFAULT '',
    natural_unit_id     INTEGER DEFAULT 0,
    prompt_template     TEXT    DEFAULT '',
    model               TEXT    DEFAULT '',
    in_holdout          INTEGER DEFAULT 0,
    assignments_json    TEXT    DEFAULT '[]',
    mode                TEXT    NOT NULL DEFAULT 'log_only'      -- 'log_only' (Phase 1) | 'live' (Phase 2+)
);
CREATE INDEX IF NOT EXISTS idx_treatment_apply_log_ts ON TreatmentApplyLog (applied_at);

-- ── D3 Phase 1 — FleetRules + PromotionProposals ─────────────────────────────
-- FleetRules — DB as source of truth for what today lives in CLAUDE.md /
-- SENATE.md / BoS rule files / ISB finder configs. Versioned per rule_key;
-- one row is "active" at a time (active_until = ''). The renderer (D3
-- Phase 3) dispatches by render_to:
--   'claude-md-file'         → CLAUDE.md the file (10 KB target / 20 KB cap)
--   'agent-prompt'           → per-agent --append-system-prompt content
--                               filtered by agent_scope
--   'fix-log'                → FIX-LOG.md historical narrative
--   'pattern-test-docstring' → test file docstring + CLAUDE.md cross-ref
--   'per-domain-doc:<file>'  → docs/<file> domain-specific markdown
--   'discard'                → row kept for history but renders nowhere
-- enforced_by references a Pattern test ID (e.g. 'TestPattern_P12') or the
-- literal 'trust-only' for rules without mechanical enforcement.
CREATE TABLE IF NOT EXISTS FleetRules (
    id                         INTEGER PRIMARY KEY AUTOINCREMENT,
    rule_key                   TEXT    NOT NULL,
    category                   TEXT    NOT NULL DEFAULT '',     -- semantic kind: architecture|schema|security|...
    agent_scope                TEXT    NOT NULL DEFAULT 'all',  -- 'all' or comma-separated list (captain,council,...)
    render_to                  TEXT    NOT NULL,                -- physical render target (controlled enum)
    enforced_by                TEXT    NOT NULL DEFAULT 'trust-only',  -- pattern test ID OR 'trust-only'
    content                    TEXT    NOT NULL,
    content_hash               TEXT    NOT NULL DEFAULT '',
    version                    INTEGER NOT NULL DEFAULT 1,
    active_from                TEXT    DEFAULT (datetime('now')),
    active_until               TEXT    DEFAULT '',              -- '' = active; non-empty = retired
    promoted_by_experiment_id  INTEGER DEFAULT 0,
    created_by                 TEXT    NOT NULL DEFAULT '',
    created_at                 TEXT    DEFAULT (datetime('now'))
);
-- Lineage: one row per (rule_key, version).
CREATE UNIQUE INDEX IF NOT EXISTS idx_fleet_rules_key_version
    ON FleetRules(rule_key, version);
-- Partial UNIQUE: at most one ACTIVE row per rule_key (active_until = '').
CREATE UNIQUE INDEX IF NOT EXISTS idx_fleet_rules_active_key
    ON FleetRules(rule_key) WHERE active_until = '';
-- Renderer query path — filter by render_to + agent_scope on active rows.
CREATE INDEX IF NOT EXISTS idx_fleet_rules_render_active
    ON FleetRules(render_to, agent_scope) WHERE active_until = '';

-- PromotionProposals — Engineering Corps emits these when an experiment
-- concludes; operator ratifies. Concern #7 revert handling lives in the
-- rejection_action / rejection_rationale / revert_task_id /
-- refiled_feature_id columns: a rejected proposal can leave_as_is,
-- clean_revert, cascade_revert, surgical_revert, or escalate.
CREATE TABLE IF NOT EXISTS PromotionProposals (
    id                 INTEGER PRIMARY KEY AUTOINCREMENT,
    experiment_id      INTEGER NOT NULL,
    kind               TEXT    NOT NULL DEFAULT 'promote',      -- 'promote'|'demote'
    rule_key           TEXT    DEFAULT '',                      -- nullable for new rules
    proposed_content   TEXT    DEFAULT '',
    evidence_summary_json TEXT DEFAULT '{}',                    -- cell means, posterior, confirm results
    authored_by        TEXT    NOT NULL DEFAULT '',             -- 'engineering-corps'
    authored_at        TEXT    DEFAULT (datetime('now')),
    ratified_at        TEXT    DEFAULT '',
    ratified_by        TEXT    DEFAULT '',
    rejected_at        TEXT    DEFAULT '',
    rejected_reason    TEXT    DEFAULT '',
    ttl_expires_at     TEXT    DEFAULT '',                      -- 14 days from authored_at
    -- concern #7 revert handling:
    rejection_action   TEXT    DEFAULT 'leave_as_is',           -- leave_as_is|clean_revert|cascade_revert|surgical_revert|escalate
    rejection_rationale TEXT   DEFAULT '',                      -- mandatory when rejection_action != 'leave_as_is'
    revert_task_id     INTEGER DEFAULT 0,                       -- spawned CodeEdit that performs the revert
    refiled_feature_id INTEGER DEFAULT 0,                       -- if rejection re-files as a new feature
    -- D4 Phase 0: source FleetMemory.id for Librarian-emitted candidates.
    -- 0 means "no source memory" (EC promotions, operator-direct-write rows).
    -- Used by EmitHypothesisCandidates for idempotence (one candidate per memory).
    source_memory_id   INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_promotion_proposals_exp   ON PromotionProposals (experiment_id);
CREATE INDEX IF NOT EXISTS idx_promotion_proposals_state ON PromotionProposals (ratified_at, rejected_at);
CREATE INDEX IF NOT EXISTS idx_promotion_proposals_source_memory ON PromotionProposals (source_memory_id) WHERE source_memory_id != 0;

-- ── D4 Phase 0 — ConflictTickets ─────────────────────────────────────────────
-- Pairs of FleetMemory rows the librarian-conflict-watch dog flagged as
-- contradictory. Operator-surfaced via /api/conflicts/tickets; status
-- transitions: 'open' → 'resolved' (+ resolution_note). reason carries a
-- short classifier (e.g. "antonym", "negation", "llm-judge").
CREATE TABLE IF NOT EXISTS ConflictTickets (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    memory_a_id     INTEGER NOT NULL,
    memory_b_id     INTEGER NOT NULL,
    reason          TEXT    NOT NULL DEFAULT '',
    status          TEXT    NOT NULL DEFAULT 'open',
    created_at      TEXT    DEFAULT (datetime('now')),
    resolved_at     TEXT    DEFAULT '',
    resolution_note TEXT    DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_conflict_tickets_status ON ConflictTickets (status, created_at);
CREATE INDEX IF NOT EXISTS idx_conflict_tickets_pair   ON ConflictTickets (memory_a_id, memory_b_id);

-- ── D3 Phase 1 — ProposedFeatures + suppressions + score overrides ───────────
-- ProposedFeatures — Investigator's cross-convoy aggregation queue.
-- fingerprint (canonical-content SHA256) + partial UNIQUE on active rows
-- enforces dedup (concern #10). value_score / complexity_score are
-- CHECK-constrained to {low, medium, high}.
CREATE TABLE IF NOT EXISTS ProposedFeatures (
    id                   INTEGER PRIMARY KEY AUTOINCREMENT,
    observation_summary  TEXT    NOT NULL,
    category             TEXT    NOT NULL,                       -- 'category_b_new_work'|'category_c_spec_amendment'|...
    source               TEXT    NOT NULL,                       -- 'investigator'|'captain'|'ec'|'operator'|'convoy_review'
    source_observations  TEXT    DEFAULT '[]',                   -- JSON [{convoy_id, agent, evidence}, ...]
    fingerprint          TEXT    NOT NULL DEFAULT '',            -- canonical-content SHA256
    occurrence_count     INTEGER DEFAULT 1,                      -- bundled across multiple convoys
    first_seen_at        TEXT    DEFAULT (datetime('now')),
    last_seen_at         TEXT    DEFAULT (datetime('now')),
    evidence_history_json TEXT   DEFAULT '[]',                   -- per-occurrence evidence trail
    value_score          TEXT    NOT NULL DEFAULT 'medium' CHECK(value_score IN ('low','medium','high')),
    complexity_score     TEXT    NOT NULL DEFAULT 'medium' CHECK(complexity_score IN ('low','medium','high')),
    value_rationale      TEXT    DEFAULT '',
    complexity_rationale TEXT    DEFAULT '',
    scored_by            TEXT    NOT NULL DEFAULT '',            -- matches `source` at insert; updated to 'operator' on override
    promoted_at          TEXT    DEFAULT '',                     -- operator-marked active interest
    promotion_deadline   TEXT    DEFAULT '',                     -- self-imposed deadline at promotion time
    status               TEXT    DEFAULT 'pending',              -- 'pending'|'spawned_convoy'|'merged'|'discarded'
    decided_at           TEXT    DEFAULT '',
    decided_by           TEXT    DEFAULT '',                     -- 'operator:<name>'
    decision_action      TEXT    DEFAULT '',                     -- 'new_convoy:<id>'|'amendment:<convoy_id>'|'discard:<reason>'
    archived_at          TEXT    DEFAULT '',                     -- soft-archive (housekeeping or operator)
    archive_reason       TEXT    DEFAULT ''
);
-- Partial UNIQUE: dedup on active rows (archived dups allowed for history).
CREATE UNIQUE INDEX IF NOT EXISTS idx_pf_active_fingerprint
    ON ProposedFeatures(fingerprint)
    WHERE archived_at = '' AND fingerprint != '';
CREATE INDEX IF NOT EXISTS idx_pf_status ON ProposedFeatures(status, last_seen_at);

-- ProposedFeatureSuppressions — operator-installed mute rules.
CREATE TABLE IF NOT EXISTS ProposedFeatureSuppressions (
    id                INTEGER PRIMARY KEY AUTOINCREMENT,
    fingerprint       TEXT    NOT NULL,                          -- matches ProposedFeatures.fingerprint
    rationale         TEXT    NOT NULL CHECK(length(rationale) >= 20),  -- ≥ 20 chars; no infinite mutes
    suppressed_until  TEXT    NOT NULL,                          -- max 1 year out (enforced at store layer)
    created_at        TEXT    DEFAULT (datetime('now')),
    created_by_email  TEXT    NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_pfs_fp
    ON ProposedFeatureSuppressions(fingerprint, suppressed_until);

-- ProposedFeatureScoreOverrides — audit trail for operator score changes.
CREATE TABLE IF NOT EXISTS ProposedFeatureScoreOverrides (
    id                     INTEGER PRIMARY KEY AUTOINCREMENT,
    proposed_feature_id    INTEGER NOT NULL,
    prior_value_score      TEXT    DEFAULT '',
    prior_complexity_score TEXT    DEFAULT '',
    new_value_score        TEXT    DEFAULT '',
    new_complexity_score   TEXT    DEFAULT '',
    rationale              TEXT    NOT NULL,                     -- mandatory; why operator overrode the score
    overridden_at          TEXT    DEFAULT (datetime('now')),
    overridden_by_email    TEXT    NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_pfso_pf
    ON ProposedFeatureScoreOverrides(proposed_feature_id);

-- ConvoyReviewCycles — concern #6: atomic snapshot evaluations against a
-- frozen spec. cycle_number is monotonic per convoy (UNIQUE) and the
-- spec_version_at_start is frozen at cycle start so amendments mid-cycle
-- take effect at the next cycle, never the in-flight one — prevents the
-- 8d→8e→8f noisy-spec drift the operator flagged.
CREATE TABLE IF NOT EXISTS ConvoyReviewCycles (
    id                                    INTEGER PRIMARY KEY AUTOINCREMENT,
    convoy_id                             INTEGER NOT NULL,
    cycle_number                          INTEGER NOT NULL,      -- monotonic per convoy
    spec_version_at_start                 TEXT    NOT NULL,      -- snapshot of spec version this cycle ran against
    cycle_started_at                      TEXT    DEFAULT (datetime('now')),
    cycle_completed_at                    TEXT    DEFAULT '',
    outcomes_json                         TEXT    DEFAULT '{}',  -- {AT-NNN: 'passed'|'failed'|'inconclusive', ...}
    fix_tasks_spawned_json                TEXT    DEFAULT '[]',  -- [{task_id, target_at_id, ...}]
    amendments_proposed_json              TEXT    DEFAULT '[]',  -- LLM-suggested spec amendments
    amendments_ratified_during_cycle_json TEXT    DEFAULT '[]',  -- audit: which amendments operator approved this cycle
    UNIQUE (convoy_id, cycle_number)
);
CREATE INDEX IF NOT EXISTS idx_crc_convoy ON ConvoyReviewCycles(convoy_id, cycle_number);

-- ── D3 Phase 1 — adversarial pairing + golden set + calibration audit ────────
-- AdversarialPairings — Council/Medic/ConvoyReview adversarial-pair results.
-- A primary prompt and a critic prompt run on the same decision; a
-- disagreement surfaces to the operator for resolution.
CREATE TABLE IF NOT EXISTS AdversarialPairings (
    id                       INTEGER PRIMARY KEY AUTOINCREMENT,
    decision_id              INTEGER NOT NULL,                   -- references the original task/decision
    agent                    TEXT    NOT NULL,                   -- 'council'|'medic'|'convoy_review'
    primary_outcome          TEXT    NOT NULL,                   -- structured decision from primary prompt
    critic_outcome           TEXT    NOT NULL,                   -- structured decision from critic prompt
    prompt_version_primary   TEXT    DEFAULT '',                 -- D3 P5: prompt version that produced primary_outcome
    prompt_version_critic    TEXT    DEFAULT '',                 -- D3 P5: prompt version that produced critic_outcome (MUST differ from primary)
    agreement                INTEGER DEFAULT 0,                  -- 1 if outcomes match
    surfaced_at              TEXT    DEFAULT '',                 -- set when surfaced to operator
    operator_resolution      TEXT    DEFAULT '',                 -- what operator decided when surfaced
    created_at               TEXT    DEFAULT (datetime('now'))
);
CREATE INDEX IF NOT EXISTS idx_adv_pairings_agent ON AdversarialPairings(agent, created_at);
CREATE INDEX IF NOT EXISTS idx_adv_pairings_disagreements
    ON AdversarialPairings(agent) WHERE agreement = 0;

-- GoldenSetFixtures — curated input fixtures with known-correct outputs.
-- Source records provenance; retired_at allows removing fixtures whose
-- contracts have shifted.
CREATE TABLE IF NOT EXISTS GoldenSetFixtures (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    agent           TEXT    NOT NULL,                            -- 'captain'|'council'|'medic'|...
    input           TEXT    NOT NULL,                            -- the input the prompt receives
    expected_output TEXT    NOT NULL,                            -- known-correct structured output
    source          TEXT    NOT NULL,                            -- 'auto-clean-shipping'|'operator-curated'|'archaeologist'
    curated_at      TEXT    DEFAULT (datetime('now')),
    curated_by      TEXT    DEFAULT '',                          -- 'system'|'operator:<name>'|'archaeologist'
    retired_at      TEXT    DEFAULT ''                           -- nullable; for fixtures no longer relevant
);
CREATE INDEX IF NOT EXISTS idx_gsf_agent ON GoldenSetFixtures(agent) WHERE retired_at = '';

-- GoldenSetEvaluations — periodic prompt-vs-fixture evaluation results.
-- Aggregated per (agent, prompt_version, week) for accuracy-trend tracking.
CREATE TABLE IF NOT EXISTS GoldenSetEvaluations (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    agent           TEXT    NOT NULL,
    prompt_version  TEXT    NOT NULL,
    fixture_id      INTEGER NOT NULL,                            -- FK → GoldenSetFixtures.id
    actual_output   TEXT    NOT NULL,                            -- what the current prompt produced
    accuracy_score  REAL,                                        -- 0.0-1.0; how closely actual matches expected
    evaluated_at    TEXT    DEFAULT (datetime('now'))
);
CREATE INDEX IF NOT EXISTS idx_gse_agent_version
    ON GoldenSetEvaluations(agent, prompt_version, evaluated_at);

-- CalibrationAuditSamples — weekly calibration sample widget records.
-- Operator confirms / overrides past auto-decisions; surfaces systematic
-- bias.
CREATE TABLE IF NOT EXISTS CalibrationAuditSamples (
    id                  INTEGER PRIMARY KEY AUTOINCREMENT,
    sample_week         TEXT    NOT NULL,                        -- ISO week identifier
    proposal_id         INTEGER NOT NULL,                        -- FK → PromotionProposals.id
    selection_bucket    TEXT    NOT NULL,                        -- 'fast_high_stakes'|'high_approve_rate'|'random'
    surfaced_at         TEXT    DEFAULT (datetime('now')),
    operator_action     TEXT    DEFAULT '',                      -- 'confirm'|'still_approve_after_review'|'should_have_been_rejected'|'snoozed'
    operator_acted_at   TEXT    DEFAULT '',
    operator_rationale  TEXT    DEFAULT ''                       -- when 'should_have_been_rejected'
);
CREATE INDEX IF NOT EXISTS idx_cas_week ON CalibrationAuditSamples(sample_week);

-- DisagreementPairs (D3 P3) — rolling-window per-pair cross-layer disagreement
-- rates populated by dogDisagreementTracker. Surfaced by
-- /api/disagreement-rates. One row per (pair_name, window_start, window_end);
-- a re-tick over the same window UPSERT-overwrites.
-- Pairs: 'captain-council-reject', 'council-ci-fail', 'convoy-review-cant-fix',
-- 'senate-chancellor-decline' (deferred until D4 — Senate ships then),
-- 'operator-revert-30d'.
CREATE TABLE IF NOT EXISTS DisagreementPairs (
    id                 INTEGER PRIMARY KEY AUTOINCREMENT,
    pair_name          TEXT    NOT NULL,                            -- e.g. 'captain-council-reject'
    window_start       TEXT    NOT NULL,                            -- ISO timestamp
    window_end         TEXT    NOT NULL,
    sample_count       INTEGER NOT NULL,                            -- denominator
    disagreement_count INTEGER NOT NULL,                            -- numerator
    rate               REAL    NOT NULL,                            -- disagreement_count / max(sample_count, 1)
    computed_at        TEXT    DEFAULT (datetime('now')),
    UNIQUE(pair_name, window_start, window_end)
);
CREATE INDEX IF NOT EXISTS idx_disagreement_pair_window
    ON DisagreementPairs(pair_name, window_end DESC);

-- ── D3 Phase 6 dashboard data-layer prerequisites ────────────────────────────
-- Per dashboard-implementation.md: schema lands in Phase 1 so 6A/6B build
-- against a stable data layer. No runtime code consumes these yet — the
-- dashboard tasks (6A.2, 6A.4, 6A.5, etc.) wire them up.

-- 6A.2 — heartbeat goroutine ticks every 30s; status banner reads.
CREATE TABLE IF NOT EXISTS DashboardHealthHeartbeats (
    id                 INTEGER PRIMARY KEY AUTOINCREMENT,
    ticked_at          TEXT    NOT NULL DEFAULT (datetime('now')),
    process_pid        INTEGER DEFAULT 0,
    bind_addr          TEXT    DEFAULT '',
    in_flight_requests INTEGER DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_dh_heartbeats_recent ON DashboardHealthHeartbeats(ticked_at DESC);

-- 6A.4 — per-(operator, source, channel) rate-limit configuration.
CREATE TABLE IF NOT EXISTS OperatorNotificationBudgets (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    operator_email  TEXT    NOT NULL,
    source          TEXT    NOT NULL,                            -- 'investigator'|'captain'|'ec'|'fleet'|'convoy_review'|...
    channel         TEXT    NOT NULL,                            -- 'email'|'modal'|'banner'
    max_per_period  INTEGER NOT NULL,
    period_minutes  INTEGER NOT NULL,
    digest_remainder INTEGER NOT NULL DEFAULT 1,
    UNIQUE(operator_email, source, channel)
);

-- 6A.4 — deferred-notification spool.
CREATE TABLE IF NOT EXISTS OperatorNotificationDigest (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    operator_email  TEXT    NOT NULL,
    source          TEXT    NOT NULL,
    channel         TEXT    NOT NULL,
    digest_for_date TEXT    NOT NULL,                            -- 'YYYY-MM-DD'
    payload_json    TEXT    NOT NULL,
    flushed_at      TEXT    DEFAULT '',
    UNIQUE(operator_email, source, channel, digest_for_date)
);

-- 6A.5 — resume-where-you-left-off state. partial_review_state_json bounded
-- to 32 KB at write time (enforced at the store-helper layer).
CREATE TABLE IF NOT EXISTS OperatorSessionState (
    id                        INTEGER PRIMARY KEY AUTOINCREMENT,
    operator_email            TEXT    NOT NULL UNIQUE,
    last_active_at            TEXT    DEFAULT (datetime('now')),
    last_viewed_surface       TEXT    DEFAULT '',                -- 'pulse'|'briefing'|'reflection'|'drill'
    last_viewed_route         TEXT    DEFAULT '',
    last_focused_decision_id  INTEGER DEFAULT 0,
    partial_review_state_json TEXT    DEFAULT ''
);

-- 6A.6 — per-(operator, agent) trust dial (history-preserving).
CREATE TABLE IF NOT EXISTS OperatorTrustDials (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    operator_email  TEXT    NOT NULL,
    agent           TEXT    NOT NULL,
    dial_value      INTEGER NOT NULL CHECK(dial_value BETWEEN 0 AND 100),
    set_at          TEXT    DEFAULT (datetime('now')),
    set_by          TEXT    NOT NULL DEFAULT '',                 -- 'operator'|'calibration_suggestion'|'system_default'
    rationale       TEXT    DEFAULT '',
    UNIQUE(operator_email, agent, set_at)
);

-- 6A.7 — LLM-batched live narrative panel results.
CREATE TABLE IF NOT EXISTS NarrativeRenders (
    id                     INTEGER PRIMARY KEY AUTOINCREMENT,
    rendered_at            TEXT    DEFAULT (datetime('now')),
    event_window_start     TEXT    NOT NULL,
    event_window_end       TEXT    NOT NULL,
    source_event_count     INTEGER NOT NULL DEFAULT 0,
    source_event_refs_json TEXT    NOT NULL DEFAULT '[]',
    prose                  TEXT    NOT NULL,
    prompt_version         TEXT    NOT NULL DEFAULT '',
    cost_usd               REAL    DEFAULT 0,
    cache_hit              INTEGER DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_nr_window ON NarrativeRenders(event_window_end DESC);

-- 6A.10 / 6A.11 — Briefing-rendered text + counter-proposal capture.
CREATE TABLE IF NOT EXISTS BriefingRenders (
    id                           INTEGER PRIMARY KEY AUTOINCREMENT,
    decision_id                  INTEGER NOT NULL,
    decision_kind                TEXT    NOT NULL,
    rendered_at                  TEXT    DEFAULT (datetime('now')),
    briefing_text                TEXT    NOT NULL,
    prior_similar_decisions_json TEXT    DEFAULT '[]',
    prompt_version               TEXT    NOT NULL DEFAULT '',
    cost_usd                     REAL    DEFAULT 0,
    operator_decision            TEXT    DEFAULT '',
    decision_time_seconds        INTEGER DEFAULT 0,
    counter_proposal_kind        TEXT    DEFAULT '',
    counter_proposal_text        TEXT    DEFAULT '',
    counter_proposal_routed_id   INTEGER DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_br_decision ON BriefingRenders(decision_kind, decision_id, rendered_at DESC);

-- 6A.13 — high-stakes auto-execute cooldown banner.
CREATE TABLE IF NOT EXISTS CooldownPauses (
    id                  INTEGER PRIMARY KEY AUTOINCREMENT,
    decision_id         INTEGER NOT NULL,
    decision_kind       TEXT    NOT NULL,
    scheduled_action_at TEXT    NOT NULL,
    paused_at           TEXT    DEFAULT '',
    paused_by_email     TEXT    DEFAULT '',
    resumed_at          TEXT    DEFAULT '',
    cancelled_at        TEXT    DEFAULT '',
    executed_at         TEXT    DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_cp_pending ON CooldownPauses(scheduled_action_at)
    WHERE executed_at = '' AND cancelled_at = '';

-- 6A.14 — operator-pinned attention to convoys / features / agents / rule keys.
CREATE TABLE IF NOT EXISTS OperatorAttentionTags (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    operator_email  TEXT    NOT NULL,
    target_kind     TEXT    NOT NULL,                            -- 'convoy'|'feature'|'agent'|'rule_key'
    target_id       TEXT    NOT NULL,
    attention_level TEXT    NOT NULL CHECK(attention_level IN ('following','normal','muted')),
    set_at          TEXT    DEFAULT (datetime('now')),
    rationale       TEXT    DEFAULT '',                          -- required when attention_level='muted' (enforced at store layer)
    UNIQUE(operator_email, target_kind, target_id)
);

-- 6B.1 — LLM call transcripts; redacted at write time per Fix #10.
CREATE TABLE IF NOT EXISTS LLMCallTranscripts (
    id                     INTEGER PRIMARY KEY AUTOINCREMENT,
    task_id                INTEGER DEFAULT 0,
    agent                  TEXT    NOT NULL,
    prompt_version         TEXT    NOT NULL DEFAULT '',
    call_started_at        TEXT    NOT NULL,
    call_completed_at      TEXT    DEFAULT '',
    system_prompt          TEXT    NOT NULL,                     -- pre-redacted
    user_prompt            TEXT    NOT NULL,                     -- pre-redacted
    response_text          TEXT    DEFAULT '',                   -- pre-redacted
    tool_calls_json        TEXT    DEFAULT '[]',
    cost_usd               REAL    DEFAULT 0,
    input_tokens           INTEGER DEFAULT 0,
    output_tokens          INTEGER DEFAULT 0,
    cache_read_tokens      INTEGER DEFAULT 0,
    cache_creation_tokens  INTEGER DEFAULT 0,
    archived_at            TEXT    DEFAULT ''                    -- when body offloaded to disk
);
CREATE INDEX IF NOT EXISTS idx_llmct_task  ON LLMCallTranscripts(task_id, call_started_at);
CREATE INDEX IF NOT EXISTS idx_llmct_agent ON LLMCallTranscripts(agent, call_started_at);

-- 6B.2 — git/gh op log for the drill view.
CREATE TABLE IF NOT EXISTS GitOperationLog (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    task_id         INTEGER DEFAULT 0,
    convoy_id       INTEGER DEFAULT 0,
    repo            TEXT    NOT NULL,
    operation       TEXT    NOT NULL,                            -- 'fetch'|'push'|'rebase'|'force-push'|'merge'|'reset'|'worktree-add'|'gh-pr'|'gh-checks'|...
    args_json       TEXT    DEFAULT '[]',                        -- pre-redacted
    started_at      TEXT    NOT NULL,
    duration_ms     INTEGER DEFAULT 0,
    exit_code       INTEGER DEFAULT 0,
    stdout_excerpt  TEXT    DEFAULT '',                          -- truncated to 4 KB
    stderr_excerpt  TEXT    DEFAULT '',                          -- truncated to 4 KB
    branch          TEXT    DEFAULT '',
    before_sha      TEXT    DEFAULT '',
    after_sha       TEXT    DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_gol_convoy ON GitOperationLog(convoy_id, started_at);
CREATE INDEX IF NOT EXISTS idx_gol_task   ON GitOperationLog(task_id, started_at);

-- 6B.8 — operator notes on events.
CREATE TABLE IF NOT EXISTS OperatorEventAnnotations (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    operator_email  TEXT    NOT NULL,
    event_kind      TEXT    NOT NULL,                            -- 'llm_call'|'task_transition'|'git_op'|'narrative'|'cycle'|'ruling_council'|...
    event_ref       TEXT    NOT NULL,
    note_text       TEXT    NOT NULL,
    flag            TEXT    DEFAULT '',                          -- 'problem'|'interesting'|'follow_up'|''
    noted_at        TEXT    DEFAULT (datetime('now'))
);
CREATE INDEX IF NOT EXISTS idx_oea_event ON OperatorEventAnnotations(event_kind, event_ref);
CREATE INDEX IF NOT EXISTS idx_oea_flag  ON OperatorEventAnnotations(flag, noted_at) WHERE flag != '';

-- 6B.7 — replay an old decision against the current prompt version.
CREATE TABLE IF NOT EXISTS ReplayResults (
    id                    INTEGER PRIMARY KEY AUTOINCREMENT,
    original_event_id     INTEGER NOT NULL,
    original_event_kind   TEXT    NOT NULL,                      -- 'captain_ruling'|'council_ruling'|'convoy_review_cycle'|'medic_decision'
    replay_prompt_version TEXT    NOT NULL,
    replay_started_at     TEXT    DEFAULT (datetime('now')),
    replay_response       TEXT    DEFAULT '',
    decision_changed      INTEGER DEFAULT 0,
    cost_usd              REAL    DEFAULT 0,
    triggered_by_email    TEXT    NOT NULL DEFAULT ''
);

-- 6B.12 — synthesised "what the fleet learned" panels.
CREATE TABLE IF NOT EXISTS FleetLearningPanels (
    id                     INTEGER PRIMARY KEY AUTOINCREMENT,
    rendered_at            TEXT    DEFAULT (datetime('now')),
    prose                  TEXT    NOT NULL,
    cost_usd               REAL    DEFAULT 0,
    prompt_version         TEXT    NOT NULL DEFAULT '',
    source_event_refs_json TEXT    DEFAULT '[]'
);

-- D4 Phase 1 — SecurityFindings (shared between BoS and ISB-Phase-2).
-- bureau column distinguishes 'BoS' (commit-time AST) from 'ISB' (security).
-- disposition values: ''|'overridden'|'resolved'|'suppressed'|'closed'.
-- bypass_audit_id + bypass_reason capture the override audit trail when a
-- // BOS-BYPASS / ISB-BYPASS comment downgrades a finding from block→advisory.
CREATE TABLE IF NOT EXISTS SecurityFindings (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    task_id         INTEGER NOT NULL DEFAULT 0,
    bureau          TEXT    NOT NULL DEFAULT 'BoS',                  -- 'BoS' | 'ISB'
    rule_id         TEXT    NOT NULL,                                -- e.g. 'BOS-001'
    severity        TEXT    NOT NULL DEFAULT 'advise',               -- 'advise' | 'block'
    file_path       TEXT    NOT NULL DEFAULT '',
    line_number     INTEGER NOT NULL DEFAULT 0,
    message         TEXT    NOT NULL DEFAULT '',
    commit_sha      TEXT    NOT NULL DEFAULT '',
    disposition     TEXT    NOT NULL DEFAULT '',                     -- ''|'overridden'|'resolved'|'suppressed'|'closed'
    bypass_audit_id TEXT    NOT NULL DEFAULT '',                     -- AUDIT-NNN reference when overridden
    bypass_reason   TEXT    NOT NULL DEFAULT '',                     -- operator rationale (>= 10 chars when bypassing)
    created_at      TEXT    DEFAULT (datetime('now')),
    resolved_at     TEXT    NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_sec_findings_task      ON SecurityFindings(task_id);
CREATE INDEX IF NOT EXISTS idx_sec_findings_rule      ON SecurityFindings(rule_id, created_at);
CREATE INDEX IF NOT EXISTS idx_sec_findings_dashboard ON SecurityFindings(rule_id, severity, disposition);

-- D4 Phase 3 — Senate. Three tables back the Senator review layer
-- (docs/next-gen-agents.md § "Senate" / § "Storage"). The Senator is the
-- repo-aware advisor consulted by the Chancellor between the
-- ProposedConvoys write and the AwaitingChancellorReview transition.
--   SenateChambers — one row per Senator (keyed by senator_name).
--   SenateMemory   — append-only memory store the Senator reads in its
--                    prompt context (ranked by weight desc).
--   SenateReview   — one row per (Feature, Senator) verdict.
CREATE TABLE IF NOT EXISTS SenateChambers (
    senator_name      TEXT PRIMARY KEY,                  -- 'force-orchestrator' | 'billing' | ...
    scope             TEXT NOT NULL,                     -- 'repo:<name>' | 'team:<name>'
    senate_md_path    TEXT NOT NULL DEFAULT '',          -- path to SENATE.md in the repo
    status            TEXT NOT NULL DEFAULT 'active',    -- 'onboarding' | 'active' | 'suspended' | 'retired'
    onboarded_at      TEXT NOT NULL DEFAULT '',
    last_refreshed_at TEXT NOT NULL DEFAULT '',
    retired_at        TEXT NOT NULL DEFAULT '',
    created_at        TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE TABLE IF NOT EXISTS SenateMemory (
    id                INTEGER PRIMARY KEY AUTOINCREMENT,
    senator           TEXT NOT NULL,
    topic             TEXT NOT NULL DEFAULT '',
    summary           TEXT NOT NULL,
    source            TEXT NOT NULL DEFAULT 'manual',    -- 'rejection' | 'commit' | 'escalation' | 'manual' | 'bootstrap'
    weight            REAL NOT NULL DEFAULT 1.0,         -- curated by Librarian
    retrieval_count   INTEGER NOT NULL DEFAULT 0,
    last_consulted_at TEXT NOT NULL DEFAULT '',
    created_at        TEXT NOT NULL DEFAULT (datetime('now'))
);
CREATE INDEX IF NOT EXISTS idx_senate_memory_senator ON SenateMemory(senator, weight DESC);

CREATE TABLE IF NOT EXISTS SenateReview (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    feature_id  INTEGER NOT NULL,                        -- references BountyBoard.id (Feature type)
    senator     TEXT    NOT NULL,
    position    TEXT    NOT NULL,                        -- 'concur' | 'amend' | 'dissent'
    concerns    TEXT    NOT NULL DEFAULT '[]',           -- JSON array of {task_id, concern, severity}
    amendments  TEXT    NOT NULL DEFAULT '[]',           -- JSON array of {task_id, new_task}
    rationale   TEXT    NOT NULL DEFAULT '',             -- one-paragraph rationale
    confidence  REAL    NOT NULL DEFAULT 0,              -- LLM-reported confidence; teeth at >=0.8
    created_at  TEXT    NOT NULL DEFAULT (datetime('now'))
);
CREATE INDEX IF NOT EXISTS idx_senate_review_feature ON SenateReview(feature_id);

-- ── D8 Track 1 — Cross-Repo Dependency Graph ─────────────────────────────────
-- Two tables maintained by dogRepoGraphScan (daily cadence). CrossRepoSymbols
-- is the per-repo exported-symbol catalogue; CrossRepoDependencies is the
-- consumer→provider edge set. Repositories is keyed on `name` (TEXT PRIMARY
-- KEY) so the FK column is `repo_name`, not the integer `repo_id` sketched in
-- the roadmap. signature_hash digests the AST-level signature so pure renames
-- don't churn the row. Soft-delete: deleted_at='' means live; non-empty
-- means the consumer site has disappeared but the row is retained for
-- debugging history (Track 1 spec, "Deletion semantics").
CREATE TABLE IF NOT EXISTS CrossRepoSymbols (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    repo_name       TEXT    NOT NULL,                     -- FK → Repositories.name
    symbol_path     TEXT    NOT NULL,                     -- 'package.Type.Method' | 'module/api/UserHandler'
    symbol_kind     TEXT    NOT NULL,                     -- 'function' | 'type' | 'http_handler' | 'cli_command' | 'exported_const'
    file_path       TEXT    NOT NULL,                     -- repo-relative path
    line_number     INTEGER NOT NULL,
    signature_hash  TEXT    NOT NULL,                     -- AST-stable across pure renames
    last_scanned_at TEXT    NOT NULL,                     -- SQLite UTC ('YYYY-MM-DD HH:MM:SS')
    is_public       INTEGER NOT NULL DEFAULT 1,           -- 1=exported, 0=private (v1 only emits public)
    UNIQUE(repo_name, symbol_path)
);
CREATE INDEX IF NOT EXISTS idx_cross_repo_symbols_repo ON CrossRepoSymbols(repo_name);
CREATE INDEX IF NOT EXISTS idx_cross_repo_symbols_kind ON CrossRepoSymbols(symbol_kind);

CREATE TABLE IF NOT EXISTS CrossRepoDependencies (
    id                 INTEGER PRIMARY KEY AUTOINCREMENT,
    consumer_repo_name TEXT    NOT NULL,                  -- FK → Repositories.name
    consumer_file      TEXT    NOT NULL,                  -- repo-relative path
    consumer_line      INTEGER NOT NULL,
    provider_symbol_id INTEGER NOT NULL,                  -- FK → CrossRepoSymbols.id
    discovered_at      TEXT    NOT NULL,
    deleted_at         TEXT    NOT NULL DEFAULT '',       -- '' = live; non-empty = soft-deleted
    UNIQUE(consumer_repo_name, consumer_file, consumer_line, provider_symbol_id)
);
CREATE INDEX IF NOT EXISTS idx_cross_repo_deps_provider ON CrossRepoDependencies(provider_symbol_id);
CREATE INDEX IF NOT EXISTS idx_cross_repo_deps_consumer ON CrossRepoDependencies(consumer_repo_name);

-- D9 Phase 1 — ArchHealthAggregates (Architecture Health Report).
-- One row per (report_month, rule_id, repo_id, author_type) tuple. Rendered
-- into reports/architecture-health-YYYY-MM.md by dogArchitectureHealthReport.
-- author_type ∈ {'human','astromech','archaeologist-migration'}.
-- UNIQUE clause makes the dog idempotent — re-running the same month is a no-op.
CREATE TABLE IF NOT EXISTS ArchHealthAggregates (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    report_month    TEXT    NOT NULL,                         -- 'YYYY-MM'
    rule_id         TEXT    NOT NULL,                         -- e.g. 'BOS-001'
    repo_id         INTEGER NOT NULL,                         -- references Repositories.rowid (Repositories is keyed by name; we use a synthetic id derived from the row order at scan time, see store.ListReposForArchHealth)
    author_type     TEXT    NOT NULL,                         -- 'human' | 'astromech' | 'archaeologist-migration'
    violation_count INTEGER NOT NULL DEFAULT 0,
    created_at      TEXT    NOT NULL DEFAULT (datetime('now')),
    UNIQUE(report_month, rule_id, repo_id, author_type)
);
CREATE INDEX IF NOT EXISTS idx_arch_health_aggregates_month_rule ON ArchHealthAggregates(report_month, rule_id);

-- ── D9 — Archaeologist findings (proactive debt detection) ───────────────────
-- One row per (pattern_id, repo_id, file_path, line_number) hit. Status flows
-- open → proposed → migrated|rejected. The Archaeologist's claim loop writes
-- rows on every ArchaeologistSweep task; ArchaeologistProposeMigration tasks
-- fire when a pattern's open-status hit count exceeds its MinHitsForFeature.
CREATE TABLE IF NOT EXISTS ArchaeologistFindings (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    pattern_id   TEXT    NOT NULL,                        -- 'ARCH-001' | 'ARCH-002' | ...
    repo_id      INTEGER NOT NULL,                        -- Repositories.rowid (joined to .name)
    file_path    TEXT    NOT NULL,                        -- relative to repo local_path
    line_number  INTEGER NOT NULL,
    detail_json  TEXT    NOT NULL DEFAULT '{}',           -- per-pattern auxiliary detail (key, deprecated symbol, ...)
    detected_at  TEXT    NOT NULL,                        -- SQLite UTC timestamp
    status       TEXT    NOT NULL DEFAULT 'open',         -- 'open' | 'proposed' | 'migrated' | 'rejected'
    UNIQUE(pattern_id, repo_id, file_path, line_number)
);
CREATE INDEX IF NOT EXISTS idx_arch_findings_pattern ON ArchaeologistFindings(pattern_id, status);

-- ── D10 Synthetic Handoff Documentation ──────────────────────────────────────
-- One row per Diplomat-emitted reviewer narrative comment posted on a draft
-- PR. PRHandoffSyntheses is the audit trail: the LLM call landed, the gh
-- post landed, and the (convoy, PR url) pair is recorded for the operator
-- + the validating paired-run experiment harness. Per-repo opt-in via
-- Repositories.handoff_synthesis_enabled (default 0).
CREATE TABLE IF NOT EXISTS PRHandoffSyntheses (
    id             INTEGER PRIMARY KEY AUTOINCREMENT,
    convoy_id      INTEGER NOT NULL REFERENCES Convoys(id),
    pr_url         TEXT NOT NULL,
    posted_at      TEXT NOT NULL,                        -- SQLite UTC ('YYYY-MM-DD HH:MM:SS')
    experiment_arm TEXT NOT NULL DEFAULT '',             -- e.g. 'control_off' | 'treatment_on' (D10-handoff experiment)
    comment_id     INTEGER NOT NULL DEFAULT 0            -- GitHub REST comment ID, when the gh poster reports it
);
CREATE INDEX IF NOT EXISTS idx_pr_handoff_convoy ON PRHandoffSyntheses(convoy_id);

-- ── Convoy events ─────────────────────────────────────────────────────────────
