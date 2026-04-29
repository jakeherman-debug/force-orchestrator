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
    created_at                TEXT    DEFAULT (datetime('now'))
);
-- Hot-table indexes (AUDIT-009, Fix #4). Without these, every ClaimBounty
-- poll and dashboard refresh full-scans BountyBoard.
CREATE INDEX IF NOT EXISTS idx_bounty_status_type    ON BountyBoard (status, type);
CREATE INDEX IF NOT EXISTS idx_bounty_convoy_status  ON BountyBoard (convoy_id, status);
CREATE INDEX IF NOT EXISTS idx_bounty_parent_id      ON BountyBoard (parent_id);
CREATE INDEX IF NOT EXISTS idx_bounty_created_at     ON BountyBoard (created_at);

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
    quarantine_reason TEXT    DEFAULT '',
    pr_review_enabled INTEGER DEFAULT 1,   -- 0 = opt-out of PR review-comment triage (AUDIT-023, Fix #4)
    mode              TEXT    NOT NULL DEFAULT 'read_only' CHECK (mode IN ('read_only','write','quarantined')) -- D2 T1-4
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
    created_at             TEXT    DEFAULT (datetime('now')),
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
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    repo          TEXT    NOT NULL,
    task_id       INTEGER DEFAULT 0,
    outcome       TEXT    NOT NULL DEFAULT 'success',  -- 'success' | 'failure'
    summary       TEXT    NOT NULL,
    files_changed TEXT    DEFAULT '',   -- comma-separated affected file paths (success only)
    topic_tags    TEXT    DEFAULT '',   -- comma-separated 3-6 short keywords from Librarian (e.g. "auth, jwt, middleware")
    embedding     BLOB    DEFAULT NULL, -- reserved: float32 vector for future sqlite-vec upgrade
    created_at    TEXT    DEFAULT (datetime('now'))
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

-- ── Convoy events ─────────────────────────────────────────────────────────────
