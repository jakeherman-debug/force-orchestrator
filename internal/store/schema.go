package store

import "database/sql"

// createSchema creates all Holocron tables if they don't already exist.
// Safe to call on every startup — all statements are idempotent.
func createSchema(db *sql.DB) {
	db.Exec(`CREATE TABLE IF NOT EXISTS Repositories (
		name TEXT PRIMARY KEY, local_path TEXT, description TEXT
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
	db.Exec(`CREATE TABLE IF NOT EXISTS Convoys (
		id           INTEGER PRIMARY KEY AUTOINCREMENT,
		name         TEXT UNIQUE NOT NULL,
		status       TEXT    DEFAULT 'Active',
		coordinated  INTEGER DEFAULT 0,
		created_at   TEXT    DEFAULT (datetime('now'))
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
	db.Exec(`CREATE TABLE IF NOT EXISTS FleetMemory (
		id            INTEGER PRIMARY KEY AUTOINCREMENT,
		repo          TEXT    NOT NULL,
		task_id       INTEGER DEFAULT 0,
		outcome       TEXT    NOT NULL DEFAULT 'success',
		summary       TEXT    NOT NULL,
		files_changed TEXT    DEFAULT '',
		embedding     BLOB    DEFAULT NULL,
		created_at    TEXT    DEFAULT (datetime('now'))
	);`)

	// FTS5 virtual table — full-text search over fleet memory summaries and file paths.
	// Standalone (not a content table) so FTS sync failures never roll back the main insert.
	// Kept in sync explicitly by StoreFleetMemory.
	//
	// Requires build tag sqlite_fts5 — use `make build`. Without the tag, this silently
	// fails and GetFleetMemories falls back to recency-only retrieval (still functional).
	db.Exec(`CREATE VIRTUAL TABLE IF NOT EXISTS FleetMemory_fts USING fts5(
		summary,
		files_changed
	)`)
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
	db.Exec(`ALTER TABLE BountyBoard ADD COLUMN created_at     TEXT    DEFAULT ''`)
	// Backfill existing rows that have no created_at so they don't get pruned immediately.
	db.Exec(`UPDATE BountyBoard SET created_at = datetime('now') WHERE created_at = ''`)

	// TaskHistory column additions
	db.Exec(`ALTER TABLE TaskHistory ADD COLUMN tokens_in  INTEGER DEFAULT 0`)
	db.Exec(`ALTER TABLE TaskHistory ADD COLUMN tokens_out INTEGER DEFAULT 0`)

	// Fleet_Mail column additions
	db.Exec(`ALTER TABLE Fleet_Mail ADD COLUMN message_type TEXT NOT NULL DEFAULT 'info'`)

	// Convoys column additions
	db.Exec(`ALTER TABLE Convoys ADD COLUMN coordinated INTEGER DEFAULT 0`)

	// Rename coordinator → captain status values (no-op on fresh DBs)
	db.Exec(`UPDATE BountyBoard SET status = 'AwaitingCaptainReview' WHERE status = 'AwaitingCoordinatorReview'`)
	db.Exec(`UPDATE BountyBoard SET status = 'UnderCaptainReview'    WHERE status = 'UnderCoordinatorReview'`)

	// Migrate blocked_by column → TaskDependencies table (no-op on fresh DBs)
	db.Exec(`INSERT OR IGNORE INTO TaskDependencies (task_id, depends_on)
		SELECT id, blocked_by FROM BountyBoard WHERE blocked_by > 0`)
	db.Exec(`ALTER TABLE BountyBoard DROP COLUMN blocked_by`)
}
