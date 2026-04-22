-- Galactic Fleet Orchestrator — Holocron Database Schema
-- Source of truth: holocron.go (InitHolocronDSN)
-- Build: go build -tags sqlite_fts5 -o force .
-- NOTE: Do not run this file directly against an existing DB — use it as reference.
--       The application applies all migrations automatically on startup via InitHolocronDSN.

PRAGMA journal_mode=WAL;

-- ── Core task board ───────────────────────────────────────────────────────────

CREATE TABLE IF NOT EXISTS BountyBoard (
    id             INTEGER PRIMARY KEY AUTOINCREMENT,
    parent_id      INTEGER DEFAULT 0,   -- ID of the Feature/Decompose task that spawned this
    target_repo    TEXT    DEFAULT '',   -- registered repo name (FK → Repositories.name)
    type           TEXT,                 -- 'Feature' | 'Decompose' | 'CodeEdit'
    status         TEXT,                 -- see status lifecycle below
    payload        TEXT,                 -- task description (enriched with [GOAL: ...] prefix by Commander)
    owner          TEXT    DEFAULT '',   -- agent name currently holding the lock
    error_log      TEXT    DEFAULT '',   -- last error or rejection reason
    retry_count    INTEGER DEFAULT 0,    -- council/captain rejection count
    infra_failures INTEGER DEFAULT 0,   -- transient Claude CLI / git failures
    locked_at      TEXT    DEFAULT '',   -- datetime('now') when locked; '' when free
    convoy_id      INTEGER DEFAULT 0,   -- FK → Convoys.id (0 = standalone task)
    checkpoint     TEXT    DEFAULT '',   -- mid-task resume state written by Astromechs
    branch_name    TEXT    DEFAULT '',   -- 'agent/<name>/task-<id>' persistent branch
    priority       INTEGER DEFAULT 0,   -- higher = claimed first (ties broken by id ASC)
    task_timeout   INTEGER DEFAULT 0,   -- per-task override in seconds (0 = default)
    idempotency_key TEXT    DEFAULT ''  -- client-supplied UUID; duplicate submissions within 60s return the existing task
);

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

CREATE TABLE IF NOT EXISTS Convoys (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    name         TEXT UNIQUE NOT NULL,  -- '[<feature_id>] <preview>'
    status       TEXT    DEFAULT 'Active',   -- 'Active' | 'Completed'
    coordinated  INTEGER DEFAULT 0,     -- 1 = Astromech completions route through Captain
    created_at   TEXT    DEFAULT (datetime('now'))
);

-- ── Repository registry ───────────────────────────────────────────────────────

CREATE TABLE IF NOT EXISTS Repositories (
    name        TEXT PRIMARY KEY,
    local_path  TEXT,
    description TEXT
);

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
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    task_id       INTEGER NOT NULL,
    attempt       INTEGER NOT NULL,
    agent         TEXT    NOT NULL,
    session_id    TEXT    NOT NULL,
    claude_output TEXT    NOT NULL,
    outcome       TEXT    NOT NULL,  -- 'Completed' | 'Failed' | 'Escalated' | 'Sharded' | 'Timeout' | 'Rejected'
    tokens_in     INTEGER DEFAULT 0,
    tokens_out    INTEGER DEFAULT 0,
    created_at    TEXT    DEFAULT (datetime('now'))
);

-- ── Escalations ───────────────────────────────────────────────────────────────

CREATE TABLE IF NOT EXISTS Escalations (
    id               INTEGER PRIMARY KEY AUTOINCREMENT,
    task_id          INTEGER NOT NULL,
    severity         TEXT    NOT NULL,  -- 'LOW' | 'MEDIUM' | 'HIGH'
    message          TEXT    NOT NULL,
    status           TEXT    DEFAULT 'Open',  -- 'Open' | 'Acknowledged' | 'Closed'
    created_at       TEXT    DEFAULT (datetime('now')),
    acknowledged_at  TEXT    DEFAULT ''
);

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

-- ── System configuration ──────────────────────────────────────────────────────
-- Runtime knobs: e-stop, max_concurrent, num_astromech, num_captain, etc.

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

-- ── Dog agent state ───────────────────────────────────────────────────────────
-- Cooldown tracking for periodic background dogs (log rotation, WAL checkpoint, etc.)

CREATE TABLE IF NOT EXISTS Dogs (
    name        TEXT PRIMARY KEY,
    last_run_at TEXT    DEFAULT '',
    run_count   INTEGER DEFAULT 0
);

-- ── Fleet memory ─────────────────────────────────────────────────────────────
-- Lessons learned from completed/failed tasks, injected into future agents on same repo.

CREATE TABLE IF NOT EXISTS FleetMemory (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    repo          TEXT    NOT NULL,
    task_id       INTEGER DEFAULT 0,
    outcome       TEXT    NOT NULL DEFAULT 'success',  -- 'success' | 'failure'
    summary       TEXT    NOT NULL,
    files_changed TEXT    DEFAULT '',   -- comma-separated affected file paths (success only)
    embedding     BLOB    DEFAULT NULL, -- reserved: float32 vector for future sqlite-vec upgrade
    created_at    TEXT    DEFAULT (datetime('now'))
);

-- FTS5 full-text search index over fleet memory (requires build tag: sqlite_fts5)
-- Kept in sync explicitly by StoreFleetMemory — not a content table.
CREATE VIRTUAL TABLE IF NOT EXISTS FleetMemory_fts USING fts5(
    summary,
    files_changed
);

-- ── Task notes ────────────────────────────────────────────────────────────────
-- Operator notes on a task, injected into the agent's context at claim time.

CREATE TABLE IF NOT EXISTS TaskNotes (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    task_id    INTEGER NOT NULL REFERENCES BountyBoard(id),
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

-- ── Convoy events ─────────────────────────────────────────────────────────────
-- Lifecycle timeline for each convoy. Populated by orchestrator components as
-- key events occur (created, status_changed, task_completed, task_failed,
-- ask_branch_created, draft_pr_opened, shipped).

CREATE TABLE IF NOT EXISTS ConvoyEvents (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    convoy_id  INTEGER NOT NULL,                     -- FK → Convoys.id
    event_type TEXT    NOT NULL,                     -- 'created' | 'status_changed' | 'task_completed' | 'task_failed' | 'ask_branch_created' | 'draft_pr_opened' | 'shipped'
    detail     TEXT    DEFAULT '',                   -- optional human-readable context
    created_at TEXT    DEFAULT (datetime('now'))
);
