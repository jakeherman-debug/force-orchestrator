package store

import (
	"database/sql"
	"strings"
)

// columnExists reports whether a given column exists on a given SQLite table.
// Used by runMigrations to gate DDL statements that must not re-run on DBs
// where the column has already been migrated away (e.g. DROP COLUMN after
// the first startup). SQLite 3.35+ errors "no such column" on a second
// DROP COLUMN; the error was previously swallowed by the unchecked db.Exec
// return value (AUDIT-077). Using pragma_table_info is the cheapest check
// that doesn't require a separate reflection round-trip.
func columnExists(db *sql.DB, table, column string) bool {
	var one int
	err := db.QueryRow(
		`SELECT 1 FROM pragma_table_info(?) WHERE name = ?`,
		table, column,
	).Scan(&one)
	return err == nil && one == 1
}

// createSchema creates all Holocron tables if they don't already exist.
// Safe to call on every startup — all statements are idempotent.
func createSchema(db *sql.DB) {
	// Repositories — registered code repos. PR-flow fields (remote_url, default_branch,
	// pr_template_path, pr_flow_enabled) are populated by the Layer B backfill at daemon
	// startup and the FindPRTemplate task. pr_flow_enabled defaults to 1 — repos opt OUT
	// of the PR flow, not in.
	db.Exec(`CREATE TABLE IF NOT EXISTS Repositories (
		name              TEXT PRIMARY KEY,
		local_path        TEXT,
		description       TEXT,
		remote_url        TEXT    DEFAULT '',
		default_branch    TEXT    DEFAULT '',
		pr_template_path  TEXT    DEFAULT '',
		pr_flow_enabled   INTEGER DEFAULT 1,
		quarantined_at    TEXT    DEFAULT '',
		quarantine_reason TEXT    DEFAULT '',
		pr_review_enabled INTEGER DEFAULT 1
	);`)

	db.Exec(`CREATE TABLE IF NOT EXISTS BountyBoard (
		id                        INTEGER PRIMARY KEY AUTOINCREMENT,
		parent_id                 INTEGER DEFAULT 0,
		target_repo               TEXT    DEFAULT '',
		type                      TEXT,
		status                    TEXT,
		payload                   TEXT,
		owner                     TEXT    DEFAULT '',
		error_log                 TEXT    DEFAULT '',
		retry_count               INTEGER DEFAULT 0,
		infra_failures            INTEGER DEFAULT 0,
		locked_at                 TEXT    DEFAULT '',
		convoy_id                 INTEGER DEFAULT 0,
		checkpoint                TEXT    DEFAULT '',
		branch_name               TEXT    DEFAULT '',
		priority                  INTEGER DEFAULT 0,
		task_timeout              INTEGER DEFAULT 0,
		idempotency_key           TEXT    DEFAULT '',
		medic_requeue_count       INTEGER DEFAULT 0,
		reshard_generation        INTEGER DEFAULT 0,
		parse_failure_count       INTEGER DEFAULT 0,
		last_findings_fingerprint TEXT    DEFAULT '',
		created_at                TEXT    DEFAULT (datetime('now'))
	);`)
	// Hot-table indexes (AUDIT-009, Fix #4). Every claim, dashboard refresh, and
	// dog tick hits these columns; without indexes they full-scan BountyBoard at
	// every poll. Keep covering-index column order aligned with the WHERE clauses
	// in ClaimBounty, dashboard queries, and parent/child rollups.
	db.Exec(`CREATE INDEX IF NOT EXISTS idx_bounty_status_type    ON BountyBoard (status, type);`)
	db.Exec(`CREATE INDEX IF NOT EXISTS idx_bounty_convoy_status  ON BountyBoard (convoy_id, status);`)
	db.Exec(`CREATE INDEX IF NOT EXISTS idx_bounty_parent_id      ON BountyBoard (parent_id);`)
	db.Exec(`CREATE INDEX IF NOT EXISTS idx_bounty_created_at     ON BountyBoard (created_at);`)

	// Fix #3 (AUDIT-008/034/035/036): partial UNIQUE index on idempotency_key.
	// Scoped to non-empty keys AND non-terminal statuses so:
	//   - empty keys (the common case for non-idempotent inserts) are not constrained
	//   - a terminal row (Completed/Cancelled/Failed) does NOT block a legitimate
	//     retry under the same key — the dedup only suppresses parallel/live work.
	// AddConvoyTaskIdempotent pairs this index with
	// `INSERT ... ON CONFLICT(idempotency_key) WHERE ... DO NOTHING RETURNING id`
	// so two concurrent callers with the same key cannot both insert.
	db.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS idx_bounty_idem
		ON BountyBoard(idempotency_key)
		WHERE idempotency_key != '' AND status NOT IN ('Completed','Cancelled','Failed')`)

	// Task dependency graph — many-to-many; replaces the old blocked_by column.
	// A task becomes claimable when all its depends_on tasks are Completed.
	db.Exec(`CREATE TABLE IF NOT EXISTS TaskDependencies (
		task_id    INTEGER NOT NULL,
		depends_on INTEGER NOT NULL,
		PRIMARY KEY (task_id, depends_on)
	);`)
	db.Exec(`CREATE INDEX IF NOT EXISTS idx_taskdeps_task_id   ON TaskDependencies (task_id);`)
	db.Exec(`CREATE INDEX IF NOT EXISTS idx_taskdeps_depends_on ON TaskDependencies (depends_on);`)

	// Escalations. State machine: Open → Acknowledged → Closed.
	//   - 'Open'          : operator has not looked at it.
	//   - 'Acknowledged'  : operator has looked but not decided on action.
	//   - 'Closed'        : auto-closed by the sweeper OR manually closed via
	//                       CloseEscalation. Terminal.
	// Legacy 'Resolved' writes were normalized to 'Closed' by the
	// Campaign 2 migration (AUDIT-025). Do not reintroduce 'Resolved'.
	// `auto_resolve_count` is incremented exactly once per row when the
	// escalation-sweeper auto-closes it; a gate in the sweeper skips rows
	// with count >= 1 so an operator re-opening an auto-closed row is
	// respected on the next 10-min tick (AUDIT-149).
	db.Exec(`CREATE TABLE IF NOT EXISTS Escalations (
		id                 INTEGER PRIMARY KEY AUTOINCREMENT,
		task_id            INTEGER NOT NULL,
		severity           TEXT    NOT NULL,
		message            TEXT    NOT NULL,
		status             TEXT    DEFAULT 'Open',
		auto_resolve_count INTEGER DEFAULT 0,
		created_at         TEXT    DEFAULT (datetime('now')),
		acknowledged_at    TEXT    DEFAULT ''
	);`)
	// Hot-table indexes on Escalations (AUDIT-024, Fix #4). escalation-sweeper
	// runs `WHERE status='Open'` every 10 minutes and joins back to BountyBoard
	// by task_id.
	db.Exec(`CREATE INDEX IF NOT EXISTS idx_escalations_status  ON Escalations (status);`)
	db.Exec(`CREATE INDEX IF NOT EXISTS idx_escalations_task_id ON Escalations (task_id);`)

	// Fix #3 (AUDIT-034): partial UNIQUE so multiple Open escalations for the
	// same task cannot accumulate. A task already has one Open row → the next
	// CreateEscalation turns into a message/severity merge via ON CONFLICT.
	// Terminal statuses (Acknowledged/Closed/Resolved) do not participate in
	// the dedup — a legitimate re-escalation after resolution is allowed.
	db.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS idx_escalations_open_task
		ON Escalations(task_id) WHERE status = 'Open'`)

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
	// Hot-table indexes on TaskHistory (AUDIT-010, Fix #4). handleTasks runs
	// correlated subqueries filtering on task_id per row; leaderboards and
	// recency reports sort on created_at; outcome/agent powers per-agent
	// success-rate reports.
	db.Exec(`CREATE INDEX IF NOT EXISTS idx_taskhistory_task_id        ON TaskHistory (task_id);`)
	db.Exec(`CREATE INDEX IF NOT EXISTS idx_taskhistory_created_at     ON TaskHistory (created_at);`)
	db.Exec(`CREATE INDEX IF NOT EXISTS idx_taskhistory_outcome_agent  ON TaskHistory (outcome, agent);`)

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
		consumed_at  TEXT    DEFAULT '',
		created_at   TEXT    DEFAULT (datetime('now'))
	);`)
	// Hot-table indexes on Fleet_Mail (AUDIT-024, Fix #4). Every agent's claim
	// loop reads `WHERE consumed_at='' AND (to_agent=? OR ...)`; MailStats /
	// dashboard refreshes sort by created_at; task-scoped mail lookups filter
	// by task_id.
	db.Exec(`CREATE INDEX IF NOT EXISTS idx_mail_to_consumed ON Fleet_Mail (to_agent, consumed_at);`)
	db.Exec(`CREATE INDEX IF NOT EXISTS idx_mail_task_id     ON Fleet_Mail (task_id);`)
	db.Exec(`CREATE INDEX IF NOT EXISTS idx_mail_created_at  ON Fleet_Mail (created_at);`)

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
	// Hot-table indexes on AuditLog (AUDIT-024, Fix #4). Table-prune / retention
	// scans filter by created_at; per-task audit views filter by task_id.
	db.Exec(`CREATE INDEX IF NOT EXISTS idx_auditlog_created_at ON AuditLog (created_at);`)
	db.Exec(`CREATE INDEX IF NOT EXISTS idx_auditlog_task_id    ON AuditLog (task_id);`)

	// Periodic dog-agent state — cooldown tracking.
	// AUDIT-047 (Fix #8d): heartbeat_at is updated at dog-start so /healthz
	// can detect a wedged dog (the Inquisitor's per-dog context.WithTimeout
	// also bounds the stall, but heartbeat_at is the user-visible signal).
	db.Exec(`CREATE TABLE IF NOT EXISTS Dogs (
		name         TEXT PRIMARY KEY,
		last_run_at  TEXT    DEFAULT '',
		run_count    INTEGER DEFAULT 0,
		heartbeat_at TEXT    DEFAULT ''
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
	// Hot-table index on FleetMemory (AUDIT-024, Fix #4). GetFleetMemories runs
	// per-repo recency retrieval before FTS re-rank; without this index it
	// scans the whole table on every fetch.
	db.Exec(`CREATE INDEX IF NOT EXISTS idx_fleet_memory_repo_created ON FleetMemory (repo, created_at);`)

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
	// ON DELETE CASCADE is required once PRAGMA foreign_keys=ON is set
	// (AUDIT-079 companion): maintenance prunes BountyBoard rows by age, and
	// without the cascade clause the DELETE would fail on any task that has
	// TaskNotes attached.
	db.Exec(`CREATE TABLE IF NOT EXISTS TaskNotes (
		id         INTEGER PRIMARY KEY AUTOINCREMENT,
		task_id    INTEGER NOT NULL REFERENCES BountyBoard(id) ON DELETE CASCADE,
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

	// Fix #3 (AUDIT-036): partial UNIQUE on (blocked_convoy_id, blocking_feature_id)
	// scoped to unresolved rows. `INSERT OR IGNORE` in CreateFeatureBlocker had
	// nothing to conflict against without this — duplicates accumulated on every
	// ResolveFeatureBlockers re-run. resolved_at IS NULL isolates live blockers;
	// a historical (resolved) row does not block a brand-new blocker from landing.
	db.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS idx_feature_blockers_open
		ON FeatureBlockers(blocked_convoy_id, blocking_feature_id) WHERE resolved_at IS NULL`)

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
		id                     INTEGER PRIMARY KEY AUTOINCREMENT,
		task_id                INTEGER NOT NULL,
		convoy_id              INTEGER NOT NULL,
		repo                   TEXT    NOT NULL,
		pr_number              INTEGER DEFAULT 0,
		pr_url                 TEXT    DEFAULT '',
		state                  TEXT    DEFAULT 'Open',
		checks_state           TEXT    DEFAULT 'Pending',
		failure_count          INTEGER DEFAULT 0,
		stall_retrigger_count  INTEGER DEFAULT 0,
		spawned_fix_count      INTEGER DEFAULT 0,
		merged_at              TEXT    DEFAULT '',
		created_at             TEXT    DEFAULT (datetime('now')),
		UNIQUE(repo, pr_number)
	)`)
	db.Exec(`CREATE INDEX IF NOT EXISTS idx_ask_branch_prs_task_id   ON AskBranchPRs (task_id)`)
	db.Exec(`CREATE INDEX IF NOT EXISTS idx_ask_branch_prs_convoy_id ON AskBranchPRs (convoy_id)`)
	db.Exec(`CREATE INDEX IF NOT EXISTS idx_ask_branch_prs_state     ON AskBranchPRs (state)`)
	// Escalation-sweeper runs `SELECT task_id, MAX(id) FROM AskBranchPRs GROUP BY task_id`
	// every 10 minutes. The default task_id-only index forces a sort for MAX(id);
	// a composite (task_id, id DESC) index lets SQLite jump straight to the latest
	// row per task. (Fix #4, addendum to AUDIT-024.)
	db.Exec(`CREATE INDEX IF NOT EXISTS idx_ask_branch_prs_task_id_id_desc ON AskBranchPRs (task_id, id DESC)`)

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
		convoy_id             INTEGER NOT NULL,
		repo                  TEXT    NOT NULL,
		ask_branch            TEXT    NOT NULL,
		ask_branch_base_sha   TEXT    NOT NULL,
		draft_pr_url          TEXT    DEFAULT '',
		draft_pr_number       INTEGER DEFAULT 0,
		draft_pr_state        TEXT    DEFAULT '',
		shipped_at            TEXT    DEFAULT '',
		last_rebased_at       TEXT    DEFAULT '',
		failed_rebase_attempts INTEGER DEFAULT 0,
		created_at            TEXT    DEFAULT (datetime('now')),
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
		classify_attempts      INTEGER DEFAULT 0,
		spawned_task_id        INTEGER DEFAULT 0,
		reply_body             TEXT    DEFAULT '',
		replied_at             TEXT    DEFAULT '',
		thread_resolved_at     TEXT    DEFAULT '',
		created_at             TEXT    DEFAULT (datetime('now')),
		UNIQUE(repo, draft_pr_number, github_comment_id)
	)`)
	db.Exec(`CREATE INDEX IF NOT EXISTS idx_pr_review_comments_convoy ON PRReviewComments (convoy_id)`)
	db.Exec(`CREATE INDEX IF NOT EXISTS idx_pr_review_comments_thread ON PRReviewComments (review_thread_id)`)
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
	// Fix #8c (AUDIT-078): the ALTER above sets default '' which drifts from
	// createSchema's DEFAULT (datetime('now')). A row inserted via the upgrade
	// path before this backfill would have '' and be excluded from
	// `WHERE created_at < datetime('now','-12 hours')` priority aging forever.
	// Running the UPDATE on both the '' and NULL cases re-stamps them in one
	// idempotent sweep (a second run matches zero rows).
	db.Exec(`UPDATE BountyBoard SET created_at = datetime('now') WHERE created_at = '' OR created_at IS NULL`)
	db.Exec(`ALTER TABLE BountyBoard ADD COLUMN idempotency_key TEXT    DEFAULT ''`)
	// Fix #6 — Break the Medic-requeue infinite loop.
	// medic_requeue_count caps the Astromech→Council→Medic→Astromech loop at
	// a bounded number of Medic-driven requeues (default 2) before forcing
	// an escalate decision. reshard_generation stamps auto-shards so the
	// 1→3→9→27 cascade is refused past the generation cap in
	// queueReshardDecompose.
	db.Exec(`ALTER TABLE BountyBoard ADD COLUMN medic_requeue_count INTEGER DEFAULT 0`)
	db.Exec(`ALTER TABLE BountyBoard ADD COLUMN reshard_generation  INTEGER DEFAULT 0`)
	// AUDIT-047 (Fix #8d): Dogs.heartbeat_at tracks the most recent tick
	// a dog started its work. /healthz + the inquisitor can spot a wedged
	// dog by comparing heartbeat_at + last_run_at against its cadence.
	db.Exec(`ALTER TABLE Dogs ADD COLUMN heartbeat_at TEXT DEFAULT ''`)

	// Fix #3: partial UNIQUE idempotency_key index for upgrade paths. See the
	// createSchema copy of this index for the semantics and the rationale on
	// why terminal statuses are excluded from the predicate.
	db.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS idx_bounty_idem
		ON BountyBoard(idempotency_key)
		WHERE idempotency_key != '' AND status NOT IN ('Completed','Cancelled','Failed')`)
	// Partial UNIQUE on Escalations(task_id) WHERE status='Open'.
	db.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS idx_escalations_open_task
		ON Escalations(task_id) WHERE status = 'Open'`)

	// Campaign 2 (AUDIT-025): collapse legacy Escalations.status='Resolved' →
	// 'Closed'. Three sinks (escalation_sweeper, medic auto-complete,
	// pilot_worktree_reset) used to write 'Resolved' but no read-side consumer
	// recognised it — rows accumulated invisibly. Write-side is fixed in the
	// same campaign; this migration normalises the historical rows and is
	// idempotent (a re-run finds zero rows to update).
	db.Exec(`UPDATE Escalations
	         SET status='Closed',
	             acknowledged_at=COALESCE(NULLIF(acknowledged_at,''), datetime('now'))
	         WHERE status='Resolved'`)

	// Campaign 2 (AUDIT-149): auto_resolve_count gates the escalation-sweeper
	// against silently re-closing an escalation the operator has re-opened for
	// deeper investigation. Sweeper increments exactly once; next tick sees
	// count >= 1 and skips.
	db.Exec(`ALTER TABLE Escalations ADD COLUMN auto_resolve_count INTEGER DEFAULT 0`)

	// Fix #7 — ConvoyReview tightening: parse-failure counter and last-pass
	// finding fingerprint for same-set dedup across re-triggered passes.
	db.Exec(`ALTER TABLE BountyBoard ADD COLUMN parse_failure_count INTEGER DEFAULT 0`)
	db.Exec(`ALTER TABLE BountyBoard ADD COLUMN last_findings_fingerprint TEXT DEFAULT ''`)

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

	// Migrate blocked_by column → TaskDependencies table (no-op on fresh DBs).
	// Fix #8c (AUDIT-077): gate the DROP on pragma_table_info so the second
	// startup doesn't error with "no such column: blocked_by". The underlying
	// db.Exec return value is unchecked here — without the gate, an error was
	// swallowed silently on every subsequent startup.
	if columnExists(db, "BountyBoard", "blocked_by") {
		db.Exec(`INSERT OR IGNORE INTO TaskDependencies (task_id, depends_on)
			SELECT id, blocked_by FROM BountyBoard WHERE blocked_by > 0`)
		db.Exec(`ALTER TABLE BountyBoard DROP COLUMN blocked_by`)
	}

	// TaskNotes — operator notes injected into agent context at claim time.
	// The ON DELETE CASCADE clause is required once PRAGMA foreign_keys=ON is
	// set (AUDIT-079 companion). Upgraded DBs may still have the cascade-less
	// definition; rebuild them below.
	db.Exec(`CREATE TABLE IF NOT EXISTS TaskNotes (
		id         INTEGER PRIMARY KEY AUTOINCREMENT,
		task_id    INTEGER NOT NULL REFERENCES BountyBoard(id) ON DELETE CASCADE,
		note       TEXT    NOT NULL,
		created_at DATETIME DEFAULT CURRENT_TIMESTAMP
	)`)
	// If an older install created TaskNotes without the cascade clause, rebuild
	// it. The check-and-rebuild idiom keeps the migration idempotent —
	// sqlite_master holds the verbatim CREATE statement, so once the rebuild
	// has run the guard is false on subsequent startups.
	var taskNotesSQL string
	db.QueryRow(`SELECT IFNULL(sql, '') FROM sqlite_master WHERE type='table' AND name='TaskNotes'`).Scan(&taskNotesSQL)
	if taskNotesSQL != "" && !strings.Contains(taskNotesSQL, "ON DELETE CASCADE") {
		// Standard SQLite "12-step" table rebuild, collapsed for the two-column case:
		//   1. create new table with the desired schema
		//   2. copy data
		//   3. drop old
		//   4. rename new into place
		db.Exec(`CREATE TABLE TaskNotes_new (
			id         INTEGER PRIMARY KEY AUTOINCREMENT,
			task_id    INTEGER NOT NULL REFERENCES BountyBoard(id) ON DELETE CASCADE,
			note       TEXT    NOT NULL,
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP
		)`)
		db.Exec(`INSERT INTO TaskNotes_new (id, task_id, note, created_at)
			SELECT id, task_id, note, created_at FROM TaskNotes`)
		db.Exec(`DROP TABLE TaskNotes`)
		db.Exec(`ALTER TABLE TaskNotes_new RENAME TO TaskNotes`)
	}

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
	// Fix #3: partial UNIQUE on unresolved (blocked, blocking) pair.
	// Dedupe any existing duplicates BEFORE creating the index — older DBs
	// may have accumulated duplicate unresolved rows. We keep the lowest id
	// of each group (stable, deterministic) and delete the rest.
	db.Exec(`DELETE FROM FeatureBlockers
	         WHERE id NOT IN (
	             SELECT MIN(id) FROM FeatureBlockers
	             WHERE resolved_at IS NULL
	             GROUP BY blocked_convoy_id, blocking_feature_id
	         )
	         AND resolved_at IS NULL`)
	db.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS idx_feature_blockers_open
		ON FeatureBlockers(blocked_convoy_id, blocking_feature_id) WHERE resolved_at IS NULL`)
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
		id                     INTEGER PRIMARY KEY AUTOINCREMENT,
		task_id                INTEGER NOT NULL,
		convoy_id              INTEGER NOT NULL,
		repo                   TEXT    NOT NULL,
		pr_number              INTEGER DEFAULT 0,
		pr_url                 TEXT    DEFAULT '',
		state                  TEXT    DEFAULT 'Open',
		checks_state           TEXT    DEFAULT 'Pending',
		failure_count          INTEGER DEFAULT 0,
		stall_retrigger_count  INTEGER DEFAULT 0,
		spawned_fix_count      INTEGER DEFAULT 0,
		merged_at              TEXT    DEFAULT '',
		created_at             TEXT    DEFAULT (datetime('now')),
		UNIQUE(repo, pr_number)
	)`)
	db.Exec(`CREATE INDEX IF NOT EXISTS idx_ask_branch_prs_task_id   ON AskBranchPRs (task_id)`)
	db.Exec(`CREATE INDEX IF NOT EXISTS idx_ask_branch_prs_convoy_id ON AskBranchPRs (convoy_id)`)
	db.Exec(`CREATE INDEX IF NOT EXISTS idx_ask_branch_prs_state     ON AskBranchPRs (state)`)
	// Additive column for existing DBs — counts stuck-runner re-trigger attempts
	// so sub-pr-ci-watch can cap the loop before escalating.
	db.Exec(`ALTER TABLE AskBranchPRs ADD COLUMN stall_retrigger_count INTEGER DEFAULT 0`)
	// Fix #7 (AUDIT-120) — cap Flaky→RealBug concurrent fix task spawns per PR.
	db.Exec(`ALTER TABLE AskBranchPRs ADD COLUMN spawned_fix_count INTEGER DEFAULT 0`)

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
	// Fix #6 — Bounded rebase-conflict retries.
	// Incremented when main-drift-watch re-queues RebaseAskBranch for a
	// convoy whose prior conflict spawn terminated without a SHA update.
	// Past `maxAskBranchConflicts` (3), the dog pauses rebases for this
	// ask-branch and escalates instead of paying another Claude cycle.
	db.Exec(`ALTER TABLE ConvoyAskBranches ADD COLUMN failed_rebase_attempts INTEGER DEFAULT 0`)

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
		classify_attempts      INTEGER DEFAULT 0,
		spawned_task_id        INTEGER DEFAULT 0,
		reply_body             TEXT    DEFAULT '',
		replied_at             TEXT    DEFAULT '',
		thread_resolved_at     TEXT    DEFAULT '',
		created_at             TEXT    DEFAULT (datetime('now')),
		UNIQUE(repo, draft_pr_number, github_comment_id)
	)`)
	db.Exec(`CREATE INDEX IF NOT EXISTS idx_pr_review_comments_convoy ON PRReviewComments (convoy_id)`)
	db.Exec(`CREATE INDEX IF NOT EXISTS idx_pr_review_comments_thread ON PRReviewComments (review_thread_id)`)
	// Fix #7 (AUDIT-032) — classifier retry counter bounds transient failures.
	db.Exec(`ALTER TABLE PRReviewComments ADD COLUMN classify_attempts INTEGER DEFAULT 0`)

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

	// ── Hot-table indexes (Fix #4, AUDIT-009, AUDIT-010, AUDIT-024, AUDIT-134) ─
	// Upgraded DBs never got the indexes from createSchema. Every index is
	// idempotent (CREATE INDEX IF NOT EXISTS) so re-running the migration is a
	// no-op once the index exists. Column order matches WHERE / ORDER BY
	// clauses in the hot paths so SQLite can read them as covering indexes.
	//
	// BountyBoard — ClaimBounty, dashboard, parent rollups, prune sweep.
	db.Exec(`CREATE INDEX IF NOT EXISTS idx_bounty_status_type    ON BountyBoard (status, type)`)
	db.Exec(`CREATE INDEX IF NOT EXISTS idx_bounty_convoy_status  ON BountyBoard (convoy_id, status)`)
	db.Exec(`CREATE INDEX IF NOT EXISTS idx_bounty_parent_id      ON BountyBoard (parent_id)`)
	db.Exec(`CREATE INDEX IF NOT EXISTS idx_bounty_created_at     ON BountyBoard (created_at)`)
	// TaskHistory — handleTasks correlated subqueries, recency, leaderboards.
	db.Exec(`CREATE INDEX IF NOT EXISTS idx_taskhistory_task_id        ON TaskHistory (task_id)`)
	db.Exec(`CREATE INDEX IF NOT EXISTS idx_taskhistory_created_at     ON TaskHistory (created_at)`)
	db.Exec(`CREATE INDEX IF NOT EXISTS idx_taskhistory_outcome_agent  ON TaskHistory (outcome, agent)`)
	// Fleet_Mail — ReadInboxForAgent, MailStats, per-task lookups.
	db.Exec(`CREATE INDEX IF NOT EXISTS idx_mail_to_consumed ON Fleet_Mail (to_agent, consumed_at)`)
	db.Exec(`CREATE INDEX IF NOT EXISTS idx_mail_task_id     ON Fleet_Mail (task_id)`)
	db.Exec(`CREATE INDEX IF NOT EXISTS idx_mail_created_at  ON Fleet_Mail (created_at)`)
	// Escalations — sweeper WHERE status='Open', join back to BountyBoard by task_id.
	db.Exec(`CREATE INDEX IF NOT EXISTS idx_escalations_status  ON Escalations (status)`)
	db.Exec(`CREATE INDEX IF NOT EXISTS idx_escalations_task_id ON Escalations (task_id)`)
	// AuditLog — retention prune, per-task audit view.
	db.Exec(`CREATE INDEX IF NOT EXISTS idx_auditlog_created_at ON AuditLog (created_at)`)
	db.Exec(`CREATE INDEX IF NOT EXISTS idx_auditlog_task_id    ON AuditLog (task_id)`)
	// FleetMemory — per-repo recency retrieval before FTS re-rank.
	db.Exec(`CREATE INDEX IF NOT EXISTS idx_fleet_memory_repo_created ON FleetMemory (repo, created_at)`)
	// AskBranchPRs — escalation-sweeper's `GROUP BY task_id / MAX(id)` needs
	// (task_id, id DESC) so SQLite can pick the latest row per task without sorting.
	db.Exec(`CREATE INDEX IF NOT EXISTS idx_ask_branch_prs_task_id_id_desc ON AskBranchPRs (task_id, id DESC)`)
}
