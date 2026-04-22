package store

import (
	"database/sql"
	"strings"
)

// createSchema creates all Holocron tables if they don't already exist.
// Safe to call on every startup — all statements are idempotent.
func createSchema(db *sql.DB) {
	// Repositories — registered code repos. PR-flow fields (remote_url, default_branch,
	// pr_template_path, pr_flow_enabled) are populated by the Layer B backfill at daemon
	// startup and the FindPRTemplate task. pr_flow_enabled defaults to 1 — repos opt OUT
	// of the PR flow, not in.
	db.Exec(`CREATE TABLE IF NOT EXISTS Repositories (
		name             TEXT PRIMARY KEY,
		local_path       TEXT,
		description      TEXT,
		remote_url       TEXT    DEFAULT '',
		default_branch   TEXT    DEFAULT '',
		pr_template_path TEXT    DEFAULT '',
		pr_flow_enabled  INTEGER DEFAULT 1,
		quarantined_at   TEXT    DEFAULT '',
		quarantine_reason TEXT   DEFAULT ''
	);`)

	db.Exec(`CREATE TABLE IF NOT EXISTS BountyBoard (
		id             INTEGER PRIMARY KEY AUTOINCREMENT,
		parent_id      INTEGER DEFAULT 0,
		target_repo    TEXT    DEFAULT '',
		type           TEXT,
		status         TEXT,
		payload        TEXT,
		owner          TEXT    DEFAULT '',
		error_log      TEXT    DEFAULT '',
		retry_count    INTEGER DEFAULT 0,
		infra_failures INTEGER DEFAULT 0,
		locked_at      TEXT    DEFAULT '',
		convoy_id      INTEGER DEFAULT 0,
		checkpoint     TEXT    DEFAULT '',
		branch_name    TEXT    DEFAULT '',
		priority       INTEGER DEFAULT 0,
		task_timeout   INTEGER DEFAULT 0,
		idempotency_key TEXT   DEFAULT '',
		created_at     TEXT    DEFAULT (datetime('now'))
	);`)

	// Task dependency graph — many-to-many; replaces the old blocked_by column.
	// A task becomes claimable when all its depends_on tasks are Completed.
	db.Exec(`CREATE TABLE IF NOT EXISTS TaskDependencies (
		task_id    INTEGER NOT NULL,
		depends_on INTEGER NOT NULL,
		PRIMARY KEY (task_id, depends_on)
	);`)
	db.Exec(`CREATE INDEX IF NOT EXISTS idx_taskdeps_task_id   ON TaskDependencies (task_id);`)
	db.Exec(`CREATE INDEX IF NOT EXISTS idx_taskdeps_depends_on ON TaskDependencies (depends_on);`)

	db.Exec(`CREATE TABLE IF NOT EXISTS Escalations (
		id               INTEGER PRIMARY KEY AUTOINCREMENT,
		task_id          INTEGER NOT NULL,
		severity         TEXT    NOT NULL,
		message          TEXT    NOT NULL,
		status           TEXT    DEFAULT 'Open',
		created_at       TEXT    DEFAULT (datetime('now')),
		acknowledged_at  TEXT    DEFAULT ''
	);`)

	// Convoys — named groups of related tasks from a single feature request.
	// PR-flow fields: ask_branch is the integration branch every sub-PR merges into;
	// ask_branch_base_sha caches main's HEAD at ask-branch creation, used by
	// main-drift-watch to detect when main has moved and a rebase is needed.
	// draft_pr_* track the final human-gated PR into main. shipped_at is set when
	// the draft PR is merged.
	db.Exec(`CREATE TABLE IF NOT EXISTS Convoys (
		id                   INTEGER PRIMARY KEY AUTOINCREMENT,
		name                 TEXT UNIQUE NOT NULL,
		status               TEXT    DEFAULT 'Active',
		coordinated          INTEGER DEFAULT 0,
		ask_branch           TEXT    DEFAULT '',
		ask_branch_base_sha  TEXT    DEFAULT '',
		draft_pr_url         TEXT    DEFAULT '',
		draft_pr_number      INTEGER DEFAULT 0,
		draft_pr_state       TEXT    DEFAULT '',
		shipped_at           TEXT    DEFAULT '',
		created_at           TEXT    DEFAULT (datetime('now'))
	);`)

	// Persistent agent worktrees — one per agent per repo, reused across task assignments.
	db.Exec(`CREATE TABLE IF NOT EXISTS Agents (
		agent_name    TEXT NOT NULL,
		repo          TEXT NOT NULL,
		worktree_path TEXT NOT NULL,
		PRIMARY KEY (agent_name, repo)
	);`)

	// Task history — full record of every Claude run per task (seance).
	db.Exec(`CREATE TABLE IF NOT EXISTS TaskHistory (
		id            INTEGER PRIMARY KEY AUTOINCREMENT,
		task_id       INTEGER NOT NULL,
		attempt       INTEGER NOT NULL,
		agent         TEXT    NOT NULL,
		session_id    TEXT    NOT NULL,
		claude_output TEXT    NOT NULL,
		outcome       TEXT    NOT NULL,
		tokens_in     INTEGER DEFAULT 0,
		tokens_out    INTEGER DEFAULT 0,
		memory_ids    TEXT    DEFAULT '',   -- CSV of FleetMemory IDs injected into this attempt's prompt
		created_at    TEXT    DEFAULT (datetime('now'))
	);`)

	// Fleet mail — structured inter-agent messaging.
	db.Exec(`CREATE TABLE IF NOT EXISTS Fleet_Mail (
		id           INTEGER PRIMARY KEY AUTOINCREMENT,
		from_agent   TEXT    NOT NULL,
		to_agent     TEXT    NOT NULL,
		subject      TEXT    NOT NULL DEFAULT '',
		body         TEXT    NOT NULL DEFAULT '',
		task_id      INTEGER DEFAULT 0,
		message_type TEXT    NOT NULL DEFAULT 'info',
		read_at      TEXT    DEFAULT '',
		created_at   TEXT    DEFAULT (datetime('now'))
	);`)

	// System configuration — e-stop, max_concurrent, rate-limit state, etc.
	db.Exec(`CREATE TABLE IF NOT EXISTS SystemConfig (
		key   TEXT PRIMARY KEY,
		value TEXT NOT NULL
	);`)

	// Operator audit log — records every destructive/approval action.
	db.Exec(`CREATE TABLE IF NOT EXISTS AuditLog (
		id         INTEGER PRIMARY KEY AUTOINCREMENT,
		actor      TEXT    NOT NULL DEFAULT 'operator',
		action     TEXT    NOT NULL,
		task_id    INTEGER DEFAULT 0,
		detail     TEXT    DEFAULT '',
		created_at TEXT    DEFAULT (datetime('now'))
	);`)

	// Periodic dog-agent state — cooldown tracking.
	db.Exec(`CREATE TABLE IF NOT EXISTS Dogs (
		name        TEXT PRIMARY KEY,
		last_run_at TEXT    DEFAULT '',
		run_count   INTEGER DEFAULT 0
	);`)

	// Fleet memory — lessons learned from completed and failed tasks, injected into future agents.
	// topic_tags is a comma-separated list of 3-5 short topic keywords generated by the Librarian
	// at write time. It's indexed alongside summary/files_changed so queries about a topic
	// (e.g. "authentication") can retrieve memories whose summary uses different words
	// (e.g. "JWT middleware"). Broadens recall without hurting precision — the LLM re-ranker
	// filters noise on the read side.
	db.Exec(`CREATE TABLE IF NOT EXISTS FleetMemory (
		id            INTEGER PRIMARY KEY AUTOINCREMENT,
		repo          TEXT    NOT NULL,
		task_id       INTEGER DEFAULT 0,
		outcome       TEXT    NOT NULL DEFAULT 'success',
		summary       TEXT    NOT NULL,
		files_changed TEXT    DEFAULT '',
		topic_tags    TEXT    DEFAULT '',
		embedding     BLOB    DEFAULT NULL,
		created_at    TEXT    DEFAULT (datetime('now'))
	);`)

	// FTS5 virtual table — full-text search over fleet memory summaries, file paths, and tags.
	// Standalone (not a content table) so FTS sync failures never roll back the main insert.
	// Kept in sync explicitly by StoreFleetMemory.
	//
	// Requires build tag sqlite_fts5 — use `make build`. Without the tag, this silently
	// fails and GetFleetMemories falls back to recency-only retrieval (still functional).
	db.Exec(`CREATE VIRTUAL TABLE IF NOT EXISTS FleetMemory_fts USING fts5(
		summary,
		files_changed,
		topic_tags
	)`)

	// Operator notes on tasks — injected into agent context at claim time.
	db.Exec(`CREATE TABLE IF NOT EXISTS TaskNotes (
		id         INTEGER PRIMARY KEY AUTOINCREMENT,
		task_id    INTEGER NOT NULL REFERENCES BountyBoard(id),
		note       TEXT    NOT NULL,
		created_at DATETIME DEFAULT CURRENT_TIMESTAMP
	)`)

	// FeatureBlockers — convoy blocked until an unplanned Feature's convoy lands.
	db.Exec(`CREATE TABLE IF NOT EXISTS FeatureBlockers (
		id                  INTEGER PRIMARY KEY AUTOINCREMENT,
		blocked_convoy_id   INTEGER NOT NULL,
		blocking_feature_id INTEGER NOT NULL,
		resolved_at         DATETIME,
		created_at          DATETIME DEFAULT (datetime('now'))
	)`)
	db.Exec(`CREATE INDEX IF NOT EXISTS idx_feature_blockers_convoy  ON FeatureBlockers (blocked_convoy_id)`)
	db.Exec(`CREATE INDEX IF NOT EXISTS idx_feature_blockers_feature ON FeatureBlockers (blocking_feature_id)`)

	// ConvoyHolds — hard rejection signal for Captain and Council.
	db.Exec(`CREATE TABLE IF NOT EXISTS ConvoyHolds (
		convoy_id  INTEGER PRIMARY KEY,
		reason     TEXT    NOT NULL,
		created_at DATETIME DEFAULT (datetime('now'))
	)`)

	// Proposed convoys — Commander stores plans here for Chancellor review.
	db.Exec(`CREATE TABLE IF NOT EXISTS ProposedConvoys (
		id          INTEGER PRIMARY KEY AUTOINCREMENT,
		feature_id  INTEGER NOT NULL UNIQUE,
		plan_json   TEXT    NOT NULL,
		status      TEXT    NOT NULL DEFAULT 'pending',
		created_at  DATETIME DEFAULT (datetime('now'))
	)`)

	// AskBranchPRs — one row per astromech sub-PR opened against a convoy's ask-branch.
	// Tracks CI state, retry counters, and terminal state transitions. Unique on
	// (repo, pr_number) so we never double-create a row for the same PR.
	db.Exec(`CREATE TABLE IF NOT EXISTS AskBranchPRs (
		id             INTEGER PRIMARY KEY AUTOINCREMENT,
		task_id        INTEGER NOT NULL,
		convoy_id      INTEGER NOT NULL,
		repo           TEXT    NOT NULL,
		pr_number      INTEGER DEFAULT 0,
		pr_url         TEXT    DEFAULT '',
		state          TEXT    DEFAULT 'Open',
		checks_state   TEXT    DEFAULT 'Pending',
		failure_count  INTEGER DEFAULT 0,
		merged_at      TEXT    DEFAULT '',
		created_at     TEXT    DEFAULT (datetime('now')),
		UNIQUE(repo, pr_number)
	)`)
	db.Exec(`CREATE INDEX IF NOT EXISTS idx_ask_branch_prs_task_id   ON AskBranchPRs (task_id)`)
	db.Exec(`CREATE INDEX IF NOT EXISTS idx_ask_branch_prs_convoy_id ON AskBranchPRs (convoy_id)`)
	db.Exec(`CREATE INDEX IF NOT EXISTS idx_ask_branch_prs_state     ON AskBranchPRs (state)`)

	// ConvoyAskBranches — per-(convoy, repo) integration branch tracking.
	//
	// A convoy's tasks may target multiple repos (Feature "Add OAuth to api and
	// monolith" produces tasks in both). Each touched repo needs its own ask-branch
	// and eventually its own draft PR. We key on (convoy_id, repo) so every repo
	// touched by a convoy carries its own state machine.
	//
	// The Convoys.ask_branch / draft_pr_* scalar fields on Convoys predate this
	// table and are left in place for backwards-compat; new code reads this table.
	db.Exec(`CREATE TABLE IF NOT EXISTS ConvoyAskBranches (
		convoy_id            INTEGER NOT NULL,
		repo                 TEXT    NOT NULL,
		ask_branch           TEXT    NOT NULL,
		ask_branch_base_sha  TEXT    NOT NULL,
		draft_pr_url         TEXT    DEFAULT '',
		draft_pr_number      INTEGER DEFAULT 0,
		draft_pr_state       TEXT    DEFAULT '',
		shipped_at           TEXT    DEFAULT '',
		last_rebased_at      TEXT    DEFAULT '',
		created_at           TEXT    DEFAULT (datetime('now')),
		PRIMARY KEY (convoy_id, repo)
	)`)
	db.Exec(`CREATE INDEX IF NOT EXISTS idx_convoy_ask_branches_repo ON ConvoyAskBranches (repo)`)

	// PRReviewComments — per-comment state for bot and human reviews on draft PRs.
	// author_kind discriminates; classification drives dispatch (see agents/pr_review_triage.go).
	// review_thread_id + thread_depth power the back-and-forth loop detector.
	db.Exec(`CREATE TABLE IF NOT EXISTS PRReviewComments (
		id                     INTEGER PRIMARY KEY AUTOINCREMENT,
		convoy_id              INTEGER NOT NULL,
		repo                   TEXT    NOT NULL,
		draft_pr_number        INTEGER NOT NULL,
		github_comment_id      INTEGER NOT NULL,
		comment_type           TEXT    NOT NULL,
		author                 TEXT    NOT NULL,
		author_kind            TEXT    NOT NULL,
		body                   TEXT    NOT NULL,
		path                   TEXT    DEFAULT '',
		line                   INTEGER DEFAULT 0,
		diff_hunk              TEXT    DEFAULT '',
		review_thread_id       TEXT    DEFAULT '',
		in_reply_to_comment_id INTEGER DEFAULT 0,
		thread_depth           INTEGER DEFAULT 0,
		classification         TEXT    DEFAULT '',
		classification_reason  TEXT    DEFAULT '',
		spawned_task_id        INTEGER DEFAULT 0,
		reply_body             TEXT    DEFAULT '',
		replied_at             TEXT    DEFAULT '',
		thread_resolved_at     TEXT    DEFAULT '',
		created_at             TEXT    DEFAULT (datetime('now')),
		UNIQUE(repo, draft_pr_number, github_comment_id)
	)`)
	db.Exec(`CREATE INDEX IF NOT EXISTS idx_pr_review_comments_convoy ON PRReviewComments (convoy_id)`)
	db.Exec(`CREATE INDEX IF NOT EXISTS idx_pr_review_comments_thread ON PRReviewComments (review_thread_id)`)

	// ConvoyEvents — append-only timeline of convoy state transitions.
	db.Exec(`CREATE TABLE IF NOT EXISTS ConvoyEvents (
		id         INTEGER PRIMARY KEY AUTOINCREMENT,
		convoy_id  INTEGER NOT NULL,
		event_type TEXT    NOT NULL,
		old_value  TEXT    DEFAULT '',
		new_value  TEXT    DEFAULT '',
		detail     TEXT    DEFAULT '',
		created_at TEXT    DEFAULT (datetime('now'))
	)`)
	db.Exec(`CREATE INDEX IF NOT EXISTS idx_convoy_events_convoy_id ON ConvoyEvents (convoy_id)`)
}

// runMigrations applies schema changes for existing databases.
// All statements are no-ops on a fresh DB — ALTER TABLE ADD COLUMN fails silently
// when the column already exists (standard SQLite migration pattern).
func runMigrations(db *sql.DB) {
	// BountyBoard column additions
	db.Exec(`ALTER TABLE BountyBoard ADD COLUMN retry_count    INTEGER DEFAULT 0`)
	db.Exec(`ALTER TABLE BountyBoard ADD COLUMN infra_failures INTEGER DEFAULT 0`)
	db.Exec(`ALTER TABLE BountyBoard ADD COLUMN locked_at      TEXT    DEFAULT ''`)
	db.Exec(`ALTER TABLE BountyBoard ADD COLUMN convoy_id      INTEGER DEFAULT 0`)
	db.Exec(`ALTER TABLE BountyBoard ADD COLUMN checkpoint     TEXT    DEFAULT ''`)
	db.Exec(`ALTER TABLE BountyBoard ADD COLUMN branch_name    TEXT    DEFAULT ''`)
	db.Exec(`ALTER TABLE BountyBoard ADD COLUMN priority       INTEGER DEFAULT 0`)
	db.Exec(`ALTER TABLE BountyBoard ADD COLUMN task_timeout   INTEGER DEFAULT 0`)
	db.Exec(`ALTER TABLE BountyBoard ADD COLUMN created_at      TEXT    DEFAULT ''`)
	// Backfill existing rows that have no created_at so they don't get pruned immediately.
	db.Exec(`UPDATE BountyBoard SET created_at = datetime('now') WHERE created_at = ''`)
	db.Exec(`ALTER TABLE BountyBoard ADD COLUMN idempotency_key TEXT    DEFAULT ''`)

	// TaskHistory column additions
	db.Exec(`ALTER TABLE TaskHistory ADD COLUMN tokens_in  INTEGER DEFAULT 0`)
	db.Exec(`ALTER TABLE TaskHistory ADD COLUMN tokens_out INTEGER DEFAULT 0`)
	db.Exec(`ALTER TABLE TaskHistory ADD COLUMN memory_ids TEXT    DEFAULT ''`)

	// Fleet_Mail column additions
	db.Exec(`ALTER TABLE Fleet_Mail ADD COLUMN message_type TEXT NOT NULL DEFAULT 'info'`)
	db.Exec(`ALTER TABLE Fleet_Mail ADD COLUMN consumed_at  TEXT         DEFAULT ''`)

	// Convoys column additions
	db.Exec(`ALTER TABLE Convoys ADD COLUMN coordinated INTEGER DEFAULT 0`)

	// Rename coordinator → captain status values (no-op on fresh DBs)
	db.Exec(`UPDATE BountyBoard SET status = 'AwaitingCaptainReview' WHERE status = 'AwaitingCoordinatorReview'`)
	db.Exec(`UPDATE BountyBoard SET status = 'UnderCaptainReview'    WHERE status = 'UnderCoordinatorReview'`)

	// Migrate blocked_by column → TaskDependencies table (no-op on fresh DBs)
	db.Exec(`INSERT OR IGNORE INTO TaskDependencies (task_id, depends_on)
		SELECT id, blocked_by FROM BountyBoard WHERE blocked_by > 0`)
	db.Exec(`ALTER TABLE BountyBoard DROP COLUMN blocked_by`)

	// TaskNotes — operator notes injected into agent context at claim time.
	db.Exec(`CREATE TABLE IF NOT EXISTS TaskNotes (
		id         INTEGER PRIMARY KEY AUTOINCREMENT,
		task_id    INTEGER NOT NULL REFERENCES BountyBoard(id),
		note       TEXT    NOT NULL,
		created_at DATETIME DEFAULT CURRENT_TIMESTAMP
	)`)

	// ProposedConvoys — Commander submits plans here; Chancellor gates convoy creation.
	db.Exec(`CREATE TABLE IF NOT EXISTS ProposedConvoys (
		id          INTEGER PRIMARY KEY AUTOINCREMENT,
		feature_id  INTEGER NOT NULL UNIQUE,
		plan_json   TEXT    NOT NULL,
		status      TEXT    NOT NULL DEFAULT 'pending',
		created_at  DATETIME DEFAULT (datetime('now'))
	)`)

	// FeatureBlockers and ConvoyHolds — Chancellor convoy ordering (idempotent on fresh DBs).
	db.Exec(`CREATE TABLE IF NOT EXISTS FeatureBlockers (
		id                  INTEGER PRIMARY KEY AUTOINCREMENT,
		blocked_convoy_id   INTEGER NOT NULL,
		blocking_feature_id INTEGER NOT NULL,
		resolved_at         DATETIME,
		created_at          DATETIME DEFAULT (datetime('now'))
	)`)
	db.Exec(`CREATE INDEX IF NOT EXISTS idx_feature_blockers_convoy  ON FeatureBlockers (blocked_convoy_id)`)
	db.Exec(`CREATE INDEX IF NOT EXISTS idx_feature_blockers_feature ON FeatureBlockers (blocking_feature_id)`)
	db.Exec(`CREATE TABLE IF NOT EXISTS ConvoyHolds (
		convoy_id  INTEGER PRIMARY KEY,
		reason     TEXT    NOT NULL,
		created_at DATETIME DEFAULT (datetime('now'))
	)`)


	// ── PR flow migration (Layer A) ──────────────────────────────────────────
	// Additive columns for the PR-based delivery flow. Each ALTER silently no-ops
	// when the column already exists, so this block is safe to re-run on every
	// startup (matching the pattern used elsewhere in runMigrations).
	//
	// Repositories: remote, default branch, PR template path, PR flow opt-out flag.
	// pr_flow_enabled defaults to 1 (opt-out model per Decision #3).
	db.Exec(`ALTER TABLE Repositories ADD COLUMN remote_url        TEXT    DEFAULT ''`)
	db.Exec(`ALTER TABLE Repositories ADD COLUMN default_branch    TEXT    DEFAULT ''`)
	db.Exec(`ALTER TABLE Repositories ADD COLUMN pr_template_path  TEXT    DEFAULT ''`)
	db.Exec(`ALTER TABLE Repositories ADD COLUMN pr_flow_enabled   INTEGER DEFAULT 1`)
	db.Exec(`ALTER TABLE Repositories ADD COLUMN quarantined_at    TEXT    DEFAULT ''`)
	db.Exec(`ALTER TABLE Repositories ADD COLUMN quarantine_reason TEXT    DEFAULT ''`)

	// Convoys: ask-branch integration + draft PR tracking.
	db.Exec(`ALTER TABLE Convoys ADD COLUMN ask_branch          TEXT    DEFAULT ''`)
	db.Exec(`ALTER TABLE Convoys ADD COLUMN ask_branch_base_sha TEXT    DEFAULT ''`)
	db.Exec(`ALTER TABLE Convoys ADD COLUMN draft_pr_url        TEXT    DEFAULT ''`)
	db.Exec(`ALTER TABLE Convoys ADD COLUMN draft_pr_number     INTEGER DEFAULT 0`)
	db.Exec(`ALTER TABLE Convoys ADD COLUMN draft_pr_state      TEXT    DEFAULT ''`)
	db.Exec(`ALTER TABLE Convoys ADD COLUMN shipped_at          TEXT    DEFAULT ''`)

	// AskBranchPRs — new table in Layer A. idempotent via IF NOT EXISTS.
	db.Exec(`CREATE TABLE IF NOT EXISTS AskBranchPRs (
		id             INTEGER PRIMARY KEY AUTOINCREMENT,
		task_id        INTEGER NOT NULL,
		convoy_id      INTEGER NOT NULL,
		repo           TEXT    NOT NULL,
		pr_number      INTEGER DEFAULT 0,
		pr_url         TEXT    DEFAULT '',
		state          TEXT    DEFAULT 'Open',
		checks_state   TEXT    DEFAULT 'Pending',
		failure_count  INTEGER DEFAULT 0,
		merged_at      TEXT    DEFAULT '',
		created_at     TEXT    DEFAULT (datetime('now')),
		UNIQUE(repo, pr_number)
	)`)
	db.Exec(`CREATE INDEX IF NOT EXISTS idx_ask_branch_prs_task_id   ON AskBranchPRs (task_id)`)
	db.Exec(`CREATE INDEX IF NOT EXISTS idx_ask_branch_prs_convoy_id ON AskBranchPRs (convoy_id)`)
	db.Exec(`CREATE INDEX IF NOT EXISTS idx_ask_branch_prs_state     ON AskBranchPRs (state)`)

	// ConvoyAskBranches — per-(convoy, repo) integration branch. Added as part of
	// Phase 2; key on (convoy_id, repo) so convoys touching multiple repos work.
	db.Exec(`CREATE TABLE IF NOT EXISTS ConvoyAskBranches (
		convoy_id            INTEGER NOT NULL,
		repo                 TEXT    NOT NULL,
		ask_branch           TEXT    NOT NULL,
		ask_branch_base_sha  TEXT    NOT NULL,
		draft_pr_url         TEXT    DEFAULT '',
		draft_pr_number      INTEGER DEFAULT 0,
		draft_pr_state       TEXT    DEFAULT '',
		shipped_at           TEXT    DEFAULT '',
		last_rebased_at      TEXT    DEFAULT '',
		created_at           TEXT    DEFAULT (datetime('now')),
		PRIMARY KEY (convoy_id, repo)
	)`)
	db.Exec(`CREATE INDEX IF NOT EXISTS idx_convoy_ask_branches_repo ON ConvoyAskBranches (repo)`)

	// ── PR review-comment triage ─────────────────────────────────────────────
	// Additive: table + column. No backfill — empty table is the expected state
	// on first migration (no draft PRs have bot/human review comments yet).
	db.Exec(`ALTER TABLE Repositories ADD COLUMN pr_review_enabled INTEGER DEFAULT 1`)
	db.Exec(`CREATE TABLE IF NOT EXISTS PRReviewComments (
		id                     INTEGER PRIMARY KEY AUTOINCREMENT,
		convoy_id              INTEGER NOT NULL,
		repo                   TEXT    NOT NULL,
		draft_pr_number        INTEGER NOT NULL,
		github_comment_id      INTEGER NOT NULL,
		comment_type           TEXT    NOT NULL,
		author                 TEXT    NOT NULL,
		author_kind            TEXT    NOT NULL,
		body                   TEXT    NOT NULL,
		path                   TEXT    DEFAULT '',
		line                   INTEGER DEFAULT 0,
		diff_hunk              TEXT    DEFAULT '',
		review_thread_id       TEXT    DEFAULT '',
		in_reply_to_comment_id INTEGER DEFAULT 0,
		thread_depth           INTEGER DEFAULT 0,
		classification         TEXT    DEFAULT '',
		classification_reason  TEXT    DEFAULT '',
		spawned_task_id        INTEGER DEFAULT 0,
		reply_body             TEXT    DEFAULT '',
		replied_at             TEXT    DEFAULT '',
		thread_resolved_at     TEXT    DEFAULT '',
		created_at             TEXT    DEFAULT (datetime('now')),
		UNIQUE(repo, draft_pr_number, github_comment_id)
	)`)
	db.Exec(`CREATE INDEX IF NOT EXISTS idx_pr_review_comments_convoy ON PRReviewComments (convoy_id)`)
	db.Exec(`CREATE INDEX IF NOT EXISTS idx_pr_review_comments_thread ON PRReviewComments (review_thread_id)`)

	// ── Fleet memory: topic_tags column + FTS rebuild ────────────────────────
	// Additive column on the main table; for the FTS5 virtual table we need to
	// drop-and-recreate because FTS5 columns are immutable. After recreating,
	// re-populate from FleetMemory so no search data is lost.
	db.Exec(`ALTER TABLE FleetMemory ADD COLUMN topic_tags TEXT DEFAULT ''`)

	// Check whether the current FTS definition already includes topic_tags.
	// sqlite_master stores the CREATE statement verbatim; a Contains check on
	// the column name is the cheapest idempotence check so we don't
	// drop-and-rebuild on every startup.
	var ftsSQL string
	db.QueryRow(`SELECT IFNULL(sql, '') FROM sqlite_master WHERE name = 'FleetMemory_fts'`).Scan(&ftsSQL)
	if ftsSQL != "" && !strings.Contains(ftsSQL, "topic_tags") {
		db.Exec(`DROP TABLE IF EXISTS FleetMemory_fts`)
		db.Exec(`CREATE VIRTUAL TABLE IF NOT EXISTS FleetMemory_fts USING fts5(
			summary,
			files_changed,
			topic_tags
		)`)
		// Re-populate from the main table so existing memories remain searchable.
		db.Exec(`INSERT INTO FleetMemory_fts(rowid, summary, files_changed, topic_tags)
			SELECT id, summary, IFNULL(files_changed, ''), IFNULL(topic_tags, '')
			FROM FleetMemory`)
	}

	// ConvoyEvents — append-only convoy timeline.
	db.Exec(`CREATE TABLE IF NOT EXISTS ConvoyEvents (
		id         INTEGER PRIMARY KEY AUTOINCREMENT,
		convoy_id  INTEGER NOT NULL,
		event_type TEXT    NOT NULL,
		old_value  TEXT    DEFAULT '',
		new_value  TEXT    DEFAULT '',
		detail     TEXT    DEFAULT '',
		created_at TEXT    DEFAULT (datetime('now'))
	)`)
	db.Exec(`CREATE INDEX IF NOT EXISTS idx_convoy_events_convoy_id ON ConvoyEvents (convoy_id)`)
}
