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
-- PR-flow fields: ask_branch is the integration branch every sub-PR merges into;
-- ask_branch_base_sha caches main's HEAD at ask-branch creation, used by
-- main-drift-watch to detect when main has moved and a rebase is needed.
-- draft_pr_* track the final human-gated PR into main.

CREATE TABLE IF NOT EXISTS Convoys (
    id                    INTEGER PRIMARY KEY AUTOINCREMENT,
    name                  TEXT UNIQUE NOT NULL,  -- '[<feature_id>] <preview>'
    status                TEXT    DEFAULT 'Active',
    coordinated           INTEGER DEFAULT 0,     -- 1 = Astromech completions route through Captain
    ask_branch            TEXT    DEFAULT '',    -- 'force/ask-<id>-<slug>'; '' = legacy convoy
    ask_branch_base_sha   TEXT    DEFAULT '',    -- main's HEAD at branch creation; updated on rebase
    draft_pr_url          TEXT    DEFAULT '',
    draft_pr_number       INTEGER DEFAULT 0,
    draft_pr_state        TEXT    DEFAULT '',    -- 'Open' | 'Merged' | 'Closed'
    shipped_at            TEXT    DEFAULT '',    -- set when draft PR is merged
    created_at            TEXT    DEFAULT (datetime('now'))
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
    name              TEXT PRIMARY KEY,
    local_path        TEXT,
    description       TEXT,
    remote_url        TEXT    DEFAULT '',  -- populated by `git remote get-url origin` at startup
    default_branch    TEXT    DEFAULT '',  -- populated by `git symbolic-ref refs/remotes/origin/HEAD`
    pr_template_path  TEXT    DEFAULT '',  -- populated by FindPRTemplate task (may be '' if repo has no template)
    pr_flow_enabled   INTEGER DEFAULT 1,   -- 0 = legacy local-merge, 1 = new PR flow (default)
    quarantined_at    TEXT    DEFAULT '',  -- set by repo-config-check when repo becomes unhealthy
    quarantine_reason TEXT    DEFAULT ''
);

-- ── Ask-branch sub-PR tracking ────────────────────────────────────────────────
-- One row per astromech sub-PR opened against a convoy's ask-branch. Tracks
-- CI state, retry counters, and terminal state transitions.

CREATE TABLE IF NOT EXISTS AskBranchPRs (
    id             INTEGER PRIMARY KEY AUTOINCREMENT,
    task_id        INTEGER NOT NULL,              -- FK → BountyBoard.id
    convoy_id      INTEGER NOT NULL,              -- FK → Convoys.id
    repo           TEXT    NOT NULL,              -- FK → Repositories.name
    pr_number      INTEGER DEFAULT 0,
    pr_url         TEXT    DEFAULT '',
    state          TEXT    DEFAULT 'Open',        -- 'Open' | 'Merged' | 'Closed'
    checks_state   TEXT    DEFAULT 'Pending',     -- 'Pending' | 'Success' | 'Failure'
    failure_count  INTEGER DEFAULT 0,             -- incremented by Medic CIFailureTriage
    merged_at      TEXT    DEFAULT '',
    created_at     TEXT    DEFAULT (datetime('now')),
    UNIQUE(repo, pr_number)
);
CREATE INDEX IF NOT EXISTS idx_ask_branch_prs_task_id   ON AskBranchPRs (task_id);
CREATE INDEX IF NOT EXISTS idx_ask_branch_prs_convoy_id ON AskBranchPRs (convoy_id);
CREATE INDEX IF NOT EXISTS idx_ask_branch_prs_state     ON AskBranchPRs (state);

-- ── Per-(convoy, repo) ask-branch tracking ────────────────────────────────────
-- A convoy's tasks may target multiple repos, so each touched repo gets its own
-- ask-branch and eventually its own draft PR. One row per (convoy, repo).

CREATE TABLE IF NOT EXISTS ConvoyAskBranches (
    convoy_id            INTEGER NOT NULL,          -- FK → Convoys.id
    repo                 TEXT    NOT NULL,          -- FK → Repositories.name
    ask_branch           TEXT    NOT NULL,          -- 'force/ask-<id>-<slug>'
    ask_branch_base_sha  TEXT    NOT NULL,          -- main's HEAD at branch creation; updated by rebase
    draft_pr_url         TEXT    DEFAULT '',
    draft_pr_number      INTEGER DEFAULT 0,
    draft_pr_state       TEXT    DEFAULT '',         -- '' | 'Open' | 'Merged' | 'Closed'
    shipped_at           TEXT    DEFAULT '',
    last_rebased_at      TEXT    DEFAULT '',
    created_at           TEXT    DEFAULT (datetime('now')),
    PRIMARY KEY (convoy_id, repo)
);
CREATE INDEX IF NOT EXISTS idx_convoy_ask_branches_repo ON ConvoyAskBranches (repo);

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

-- ── Convoy events ─────────────────────────────────────────────────────────────
