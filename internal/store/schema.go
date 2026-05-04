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
	// release_label_pattern (D5.5): per-repo regex used by the staged-convoy
	// `release_label_present` gate; empty means the repo doesn't use release labels.
	db.Exec(`CREATE TABLE IF NOT EXISTS Repositories (
		name                  TEXT PRIMARY KEY,
		local_path            TEXT,
		description           TEXT,
		remote_url            TEXT    DEFAULT '',
		default_branch        TEXT    DEFAULT '',
		pr_template_path      TEXT    DEFAULT '',
		pr_flow_enabled       INTEGER DEFAULT 1,
		quarantined_at        TEXT    DEFAULT '',
		quarantine_reason     TEXT    DEFAULT '',
		pr_review_enabled     INTEGER DEFAULT 1,
		mode                  TEXT    NOT NULL DEFAULT 'read_only' CHECK (mode IN ('read_only','write','quarantined')),
		license               TEXT    NOT NULL DEFAULT '',
		release_label_pattern TEXT    NOT NULL DEFAULT '',
		archaeologist_sweep_disabled INTEGER NOT NULL DEFAULT 0
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
		spend_suspended           INTEGER DEFAULT 0,
		recent_commit_hashes_json TEXT    DEFAULT '[]',
		in_holdout                INTEGER DEFAULT 0,
		experiment_assignments_json TEXT  DEFAULT '{}',
		proposed_action_json      TEXT    DEFAULT '',
		prompt_version            TEXT    DEFAULT '',
		prior_review_outcomes_json TEXT   DEFAULT '[]',
		spawn_spec_link           TEXT    DEFAULT '',
		spawn_classification_confidence TEXT DEFAULT '',
		spawning_at_id            TEXT    DEFAULT '',
		deferred_revert           INTEGER DEFAULT 0,
		revert_target_task_id     INTEGER DEFAULT 0,
		stage_id                  INTEGER DEFAULT NULL,
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
	// stage_id (D5.5 P2) — partial index over the populated subset. Single-mode
	// convoys leave stage_id NULL; the convoy-stage-watch dog and per-stage
	// dispatch queries filter `WHERE stage_id = ?` and benefit from the smaller
	// index. NULL rows are excluded by the partial predicate so legacy single-
	// stage convoys do not bloat the index.
	db.Exec(`CREATE INDEX IF NOT EXISTS idx_bounty_stage_id ON BountyBoard (stage_id) WHERE stage_id IS NOT NULL;`)

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
	// staging_mode (D5.5): 'single' (legacy + default) | 'staged' (Commander-drafted
	// phase pipeline). staging_strategy: 'strict' (default + only value implemented
	// in D5.5) | 'merge_parallel' | 'stacked' (future). The enum is enforced at the
	// agent layer, not via SQL CHECK, so future values are accepted without migration.
	db.Exec(`CREATE TABLE IF NOT EXISTS Convoys (
		id                          INTEGER PRIMARY KEY AUTOINCREMENT,
		name                        TEXT UNIQUE NOT NULL,
		status                      TEXT    DEFAULT 'Active',
		coordinated                 INTEGER DEFAULT 0,
		ask_branch                  TEXT    DEFAULT '',
		ask_branch_base_sha         TEXT    DEFAULT '',
		draft_pr_url                TEXT    DEFAULT '',
		draft_pr_number             INTEGER DEFAULT 0,
		draft_pr_state              TEXT    DEFAULT '',
		shipped_at                  TEXT    DEFAULT '',
		in_holdout                  INTEGER DEFAULT 0,
		experiment_assignments_json TEXT    DEFAULT '{}',
		parent_feature_id           INTEGER DEFAULT 0,
		verification_spec_json      TEXT    DEFAULT '',
		spec_history_json           TEXT    DEFAULT '[]',
		critical                    INTEGER DEFAULT 0,
		staging_mode                TEXT    NOT NULL DEFAULT 'single',
		staging_strategy            TEXT    NOT NULL DEFAULT 'strict',
		created_at                  TEXT    DEFAULT (datetime('now'))
	);`)

	// Persistent agent worktrees — one per agent per repo, reused across task assignments.
	db.Exec(`CREATE TABLE IF NOT EXISTS Agents (
		agent_name    TEXT NOT NULL,
		repo          TEXT NOT NULL,
		worktree_path TEXT NOT NULL,
		PRIMARY KEY (agent_name, repo)
	);`)

	// Task history — full record of every Claude run per task (seance).
	// cost_usd_estimate (D2 T1-1): per-attempt cost in USD, computed at write time
	// from tokens_in / tokens_out and the per-model price table in
	// internal/claude/pricing.go. Stored as REAL so dashboard sums and the
	// per-task spend dog can read it without recomputing.
	db.Exec(`CREATE TABLE IF NOT EXISTS TaskHistory (
		id                INTEGER PRIMARY KEY AUTOINCREMENT,
		task_id           INTEGER NOT NULL,
		attempt           INTEGER NOT NULL,
		agent             TEXT    NOT NULL,
		session_id        TEXT    NOT NULL,
		claude_output     TEXT    NOT NULL,
		outcome           TEXT    NOT NULL,
		tokens_in         INTEGER DEFAULT 0,
		tokens_out        INTEGER DEFAULT 0,
		cost_usd_estimate REAL    DEFAULT 0,
		memory_ids        TEXT    DEFAULT '',   -- CSV of FleetMemory IDs injected into this attempt's prompt
		prompt_version    TEXT    DEFAULT '',   -- D3 P1: enables per-prompt-version metric correlation
		created_at        TEXT    DEFAULT (datetime('now'))
	);`)
	// Hot-table indexes on TaskHistory (AUDIT-010, Fix #4). handleTasks runs
	// correlated subqueries filtering on task_id per row; leaderboards and
	// recency reports sort on created_at; outcome/agent powers per-agent
	// success-rate reports.
	db.Exec(`CREATE INDEX IF NOT EXISTS idx_taskhistory_task_id        ON TaskHistory (task_id);`)
	db.Exec(`CREATE INDEX IF NOT EXISTS idx_taskhistory_created_at     ON TaskHistory (created_at);`)
	db.Exec(`CREATE INDEX IF NOT EXISTS idx_taskhistory_outcome_agent  ON TaskHistory (outcome, agent);`)

	// TaskSpendWatch — anomaly-dedup ledger written by dogTaskSpendWatch (D2 T1-1).
	// One row per (task_id, window_start) when a 10-min trailing-window cost
	// exceeds the per_task_spend_alert_usd threshold. notified_at is the
	// idempotency key — a dog tick within the same window finds the existing
	// row and skips re-mailing the operator.
	db.Exec(`CREATE TABLE IF NOT EXISTS TaskSpendWatch (
		id           INTEGER PRIMARY KEY AUTOINCREMENT,
		task_id      INTEGER NOT NULL,
		window_start TEXT    NOT NULL,
		cost_usd     REAL    DEFAULT 0,
		notified_at  TEXT    DEFAULT (datetime('now'))
	);`)
	db.Exec(`CREATE INDEX IF NOT EXISTS idx_taskspendwatch_task_window ON TaskSpendWatch (task_id, window_start);`)

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
	// D4 Phase 0 — Librarian evolution: quality-scoring columns
	// (freshness_score / validation_score / retrieval_count /
	// last_retrieved_at) are written by the librarian-quality-recompute
	// dog + RecordRetrieval / RecordValidation helpers; canonical_id
	// supports the dedup-and-merge audit trail (rows merged into a
	// canonical entry retain their id but stamp canonical_id at the row
	// they collapsed into).
	db.Exec(`CREATE TABLE IF NOT EXISTS FleetMemory (
		id                INTEGER PRIMARY KEY AUTOINCREMENT,
		repo              TEXT    NOT NULL,
		task_id           INTEGER DEFAULT 0,
		outcome           TEXT    NOT NULL DEFAULT 'success',
		summary           TEXT    NOT NULL,
		files_changed     TEXT    DEFAULT '',
		topic_tags        TEXT    DEFAULT '',
		embedding         BLOB    DEFAULT NULL,
		created_at        TEXT    DEFAULT (datetime('now')),
		freshness_score   REAL    NOT NULL DEFAULT 1.0,
		validation_score  REAL    NOT NULL DEFAULT 0.0,
		retrieval_count   INTEGER NOT NULL DEFAULT 0,
		last_retrieved_at TEXT    DEFAULT '',
		canonical_id      INTEGER NOT NULL DEFAULT 0,
		hypothesis_emitted_at TEXT DEFAULT ''
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
	// stage_id (D5.5) — FK to ConvoyStages.id. For single-stage (legacy) convoys
	// the migration sets every existing row to stage 1 of an implicit single-stage
	// convoy. For staged convoys (D5.5), each (convoy, repo, stage) gets its own
	// ConvoyAskBranches row.
	db.Exec(`CREATE TABLE IF NOT EXISTS ConvoyAskBranches (
		convoy_id              INTEGER NOT NULL,
		repo                   TEXT    NOT NULL,
		ask_branch             TEXT    NOT NULL,
		ask_branch_base_sha    TEXT    NOT NULL,
		draft_pr_url           TEXT    DEFAULT '',
		draft_pr_number        INTEGER DEFAULT 0,
		draft_pr_state         TEXT    DEFAULT '',
		shipped_at             TEXT    DEFAULT '',
		last_rebased_at        TEXT    DEFAULT '',
		failed_rebase_attempts INTEGER DEFAULT 0,
		stage_id               INTEGER,
		created_at             TEXT    DEFAULT (datetime('now')),
		PRIMARY KEY (convoy_id, repo)
	)`)
	db.Exec(`CREATE INDEX IF NOT EXISTS idx_convoy_ask_branches_repo ON ConvoyAskBranches (repo)`)
	db.Exec(`CREATE INDEX IF NOT EXISTS idx_convoy_ask_branches_stage_id ON ConvoyAskBranches (stage_id)`)

	// ConvoyStages (D5.5) — Commander-drafted ordered phase pipeline for a convoy.
	// One row per stage; stage_num is 1-indexed and ordered by execution. Status
	// progresses Pending → Open → AllPRsMerged → AwaitingGate → GatePassed → Verified.
	// gate_type is 'soak_minutes' | 'operator_confirm' | NULL (no gate; terminal
	// stage only) plus in P1 the compounds 'all_of' | 'any_of'. Forward-compat
	// migration creates a single Open stage 1 row with gate_type=NULL for every
	// existing (single-mode) convoy.
	db.Exec(`CREATE TABLE IF NOT EXISTS ConvoyStages (
		id                   INTEGER PRIMARY KEY AUTOINCREMENT,
		convoy_id            INTEGER NOT NULL REFERENCES Convoys(id),
		stage_num            INTEGER NOT NULL,
		intent_text          TEXT    NOT NULL DEFAULT '',
		status               TEXT    NOT NULL DEFAULT 'Pending',
		gate_type            TEXT,
		gate_config_json     TEXT    NOT NULL DEFAULT '{}',
		gate_timeout_minutes INTEGER NOT NULL DEFAULT 10080,
		opened_at            TEXT,
		all_prs_merged_at    TEXT,
		gate_passed_at       TEXT,
		completed_at         TEXT,
		UNIQUE(convoy_id, stage_num)
	)`)
	db.Exec(`CREATE INDEX IF NOT EXISTS idx_convoy_stages_convoy_id ON ConvoyStages (convoy_id)`)
	db.Exec(`CREATE INDEX IF NOT EXISTS idx_convoy_stages_status ON ConvoyStages (status)`)

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

	// PromptByteAttribution — D2 T1-2. Per-call source-tag breakdown of
	// the assembled LLM prompt. One row per (call, source_tag) so an
	// operator can see "captain's last call was 60% file_read, 25%
	// claude_md, 10% task_payload". The dashboard's per-agent prompt
	// byte budget view aggregates this table over a rolling window.
	// task_id is 0 for context-less calls (e.g. boot, classifier).
	// source_tag is constrained at the application layer (enum); the
	// schema keeps it TEXT to permit a future migration of the enum
	// without a destructive table rebuild.
	db.Exec(`CREATE TABLE IF NOT EXISTS PromptByteAttribution (
		id              INTEGER PRIMARY KEY AUTOINCREMENT,
		task_id         INTEGER NOT NULL DEFAULT 0,
		agent_name      TEXT    NOT NULL,
		call_timestamp  TEXT    NOT NULL DEFAULT (datetime('now')),
		source_tag      TEXT    NOT NULL,
		bytes           INTEGER NOT NULL DEFAULT 0
	);`)
	// Hot-table indexes (D2 T1-2). Per-task lookups (task detail view)
	// filter by task_id; per-agent rolling window aggregations filter by
	// agent_name + call_timestamp.
	db.Exec(`CREATE INDEX IF NOT EXISTS idx_prompt_byte_attr_task     ON PromptByteAttribution (task_id, call_timestamp);`)
	db.Exec(`CREATE INDEX IF NOT EXISTS idx_prompt_byte_attr_agent_ts ON PromptByteAttribution (agent_name, call_timestamp);`)

	// ── D3 Phase 1: paired-runs core schema ──────────────────────────────────
	// Tables for the experiment / treatment / metric primitive (paired-runs.md
	// § Data Model). Phase 1 lands these as data-layer prerequisites; no
	// runtime code consumes them yet — the log-only treatments.Apply wiring
	// in Phase 4 is the first writer. Subsequent D3 phases (single-treatment
	// experiments, EC, factorial, paired shadow) build against these.

	// Experiments — one row per registered experiment. status walks
	// authored → ratified → running → confirming → terminated.
	// `kind` (D3 Phase 4) discriminates single-treatment from factorial:
	// 'single' (Phase 2 surface) keeps the one-arm-per-row shape;
	// 'factorial' adds factor definitions in `factors_json` and
	// per-cell ExperimentTreatments rows whose `cell_json` records the
	// factor-level mapping. The CHECK constraint is enforced on fresh
	// DBs only; SQLite ALTER TABLE ADD COLUMN cannot retro-fit CHECK
	// on upgraded DBs, so internal/experiments validates the value
	// before insert (paired-runs.md § Factorial Scoring).
	db.Exec(`CREATE TABLE IF NOT EXISTS Experiments (
		id                          INTEGER PRIMARY KEY AUTOINCREMENT,
		name                        TEXT    NOT NULL,
		hypothesis_text             TEXT    NOT NULL DEFAULT '',
		min_practical_effect        REAL    DEFAULT 0,
		stakes_tier                 TEXT    NOT NULL DEFAULT 'low',
		declare_threshold_override  REAL,
		factorial_dimensions_json   TEXT    DEFAULT '[]',
		kind                        TEXT    NOT NULL DEFAULT 'single' CHECK (kind IN ('single','factorial')),
		factors_json                TEXT    DEFAULT '[]',
		subject_agent               TEXT    NOT NULL DEFAULT '',
		assignment_unit             TEXT    NOT NULL DEFAULT 'task',
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
	);`)
	db.Exec(`CREATE INDEX IF NOT EXISTS idx_experiments_status      ON Experiments (status);`)
	db.Exec(`CREATE INDEX IF NOT EXISTS idx_experiments_subject     ON Experiments (subject_agent, status);`)
	db.Exec(`CREATE INDEX IF NOT EXISTS idx_experiments_kind_status ON Experiments (kind, status);`)

	// TreatmentSpecs — content-snapshotted treatment definitions.
	// `spec_hash` is unique so identical treatments across experiments share
	// rows (cross-experiment "has this exact treatment ever won?" queries).
	db.Exec(`CREATE TABLE IF NOT EXISTS TreatmentSpecs (
		id                       INTEGER PRIMARY KEY AUTOINCREMENT,
		spec_hash                TEXT    UNIQUE NOT NULL,
		prompt_template_ref      TEXT    DEFAULT '',
		prompt_template_content  TEXT    DEFAULT '',
		rule_set_refs_json       TEXT    DEFAULT '[]',
		memory_bundle_ref        TEXT    DEFAULT '',
		memory_bundle_content    TEXT    DEFAULT '',
		model_identifier         TEXT    DEFAULT '',
		max_turns                INTEGER DEFAULT 0,
		context_size_bytes       INTEGER DEFAULT 0,
		tool_availability_json   TEXT    DEFAULT '[]',
		routing_thresholds_json  TEXT    DEFAULT '{}',
		created_at               TEXT    DEFAULT (datetime('now'))
	);`)

	// ExperimentTreatments — one row per arm of an experiment.
	db.Exec(`CREATE TABLE IF NOT EXISTS ExperimentTreatments (
		id                    INTEGER PRIMARY KEY AUTOINCREMENT,
		experiment_id         INTEGER NOT NULL,
		arm_label             TEXT    NOT NULL,
		cell_json             TEXT    DEFAULT '{}',
		treatment_spec_id     INTEGER NOT NULL,
		target_cell_weight    REAL    DEFAULT 0
	);`)
	db.Exec(`CREATE INDEX IF NOT EXISTS idx_exp_treatments_exp ON ExperimentTreatments (experiment_id);`)

	// ExperimentMetrics — metrics tracked per experiment, with one primary
	// metric driving declare-winner.
	db.Exec(`CREATE TABLE IF NOT EXISTS ExperimentMetrics (
		id              INTEGER PRIMARY KEY AUTOINCREMENT,
		experiment_id   INTEGER NOT NULL,
		metric_name     TEXT    NOT NULL,
		metric_version  TEXT    NOT NULL,
		direction       TEXT    NOT NULL DEFAULT 'higher_is_better',
		params_json     TEXT    DEFAULT '{}',
		is_primary      INTEGER DEFAULT 0
	);`)
	db.Exec(`CREATE INDEX IF NOT EXISTS idx_exp_metrics_exp ON ExperimentMetrics (experiment_id);`)

	// ExperimentRuns — one row per natural-unit assignment to a treatment.
	// `mode` discriminates holdout / paired_real / paired_shadow.
	db.Exec(`CREATE TABLE IF NOT EXISTS ExperimentRuns (
		id                       INTEGER PRIMARY KEY AUTOINCREMENT,
		experiment_id            INTEGER NOT NULL,
		treatment_id             INTEGER NOT NULL,
		cell_json                TEXT    DEFAULT '{}',
		natural_unit_kind        TEXT    NOT NULL,
		natural_unit_id          INTEGER NOT NULL,
		mode                     TEXT    NOT NULL DEFAULT 'holdout',
		paired_with_run_id       INTEGER DEFAULT 0,
		agent_name               TEXT    NOT NULL DEFAULT '',
		assigned_at              TEXT    DEFAULT (datetime('now')),
		completed_at             TEXT    DEFAULT '',
		score                    REAL,
		score_source             TEXT    DEFAULT '',
		metric_version           TEXT    DEFAULT '',
		model_substituted_from   TEXT    DEFAULT '',
		model_substituted_to     TEXT    DEFAULT '',
		is_provisional           INTEGER DEFAULT 0
	);`)
	db.Exec(`CREATE INDEX IF NOT EXISTS idx_exp_runs_exp_treat ON ExperimentRuns (experiment_id, treatment_id);`)
	db.Exec(`CREATE INDEX IF NOT EXISTS idx_exp_runs_unit      ON ExperimentRuns (natural_unit_kind, natural_unit_id);`)

	// ExperimentOutcomes — one row per terminated experiment (UNIQUE on
	// experiment_id). Frozen snapshot at termination time.
	db.Exec(`CREATE TABLE IF NOT EXISTS ExperimentOutcomes (
		id                          INTEGER PRIMARY KEY AUTOINCREMENT,
		experiment_id               INTEGER NOT NULL UNIQUE,
		terminated_at               TEXT    DEFAULT (datetime('now')),
		termination_reason          TEXT    NOT NULL,
		winner_treatment_id         INTEGER DEFAULT 0,
		winner_posterior            REAL,
		winner_effect_estimate      REAL,
		cell_means_json             TEXT    DEFAULT '{}',
		fleet_state_hash_at_start   TEXT    DEFAULT '',
		fleet_state_hash_at_end     TEXT    DEFAULT '',
		confirm_phase_outcome       TEXT    DEFAULT '',
		promotion_proposal_id       INTEGER DEFAULT 0
	);`)

	// ExperimentInteractions — D3 Phase 4. Per-(factor pair, level pair)
	// interaction estimates for factorial experiments, written by the
	// factorial analyzer at termination. The 2-way interaction
	// `[mean(D1=a,D2=b) - mean(D1=a',D2=b)] - [mean(D1=a,D2=b') -
	// mean(D1=a',D2=b')]` is decomposed into per-cell contrasts so
	// 3+-level factors can store the full interaction surface (not just
	// a 2x2 contrast scalar). Single-treatment experiments never write
	// rows here; the table stays empty for kind='single'. See
	// paired-runs.md § Factorial Scoring.
	db.Exec(`CREATE TABLE IF NOT EXISTS ExperimentInteractions (
		id                       INTEGER PRIMARY KEY AUTOINCREMENT,
		experiment_id            INTEGER NOT NULL,
		factor_a                 TEXT    NOT NULL,
		factor_b                 TEXT    NOT NULL,
		level_a                  TEXT    NOT NULL DEFAULT '',
		level_b                  TEXT    NOT NULL DEFAULT '',
		interaction_estimate     REAL    DEFAULT 0,
		posterior_alpha          REAL    DEFAULT 0,
		posterior_beta           REAL    DEFAULT 0,
		posterior_prob_nonzero   REAL    DEFAULT 0,
		computed_at              TEXT    DEFAULT (datetime('now'))
	);`)
	db.Exec(`CREATE INDEX IF NOT EXISTS idx_exp_interactions_exp  ON ExperimentInteractions (experiment_id);`)
	db.Exec(`CREATE INDEX IF NOT EXISTS idx_exp_interactions_pair ON ExperimentInteractions (experiment_id, factor_a, factor_b);`)

	// AnalysisFrameworks — versioned algorithm config; published
	// definitions are immutable (deprecated_at marks retirement).
	db.Exec(`CREATE TABLE IF NOT EXISTS AnalysisFrameworks (
		version           TEXT PRIMARY KEY,
		config_content    TEXT    NOT NULL,
		config_hash       TEXT    NOT NULL,
		algorithm_git_sha TEXT    DEFAULT '',
		published_at      TEXT    DEFAULT (datetime('now')),
		published_by      TEXT    NOT NULL DEFAULT '',
		description       TEXT    DEFAULT '',
		deprecated_at     TEXT    DEFAULT ''
	);`)

	// MetricVersions — versioned (metric_name, version) pairs. SQL body
	// + test SQL + manifest JSON snapshotted at publish time.
	db.Exec(`CREATE TABLE IF NOT EXISTS MetricVersions (
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
	);`)

	// FleetStateSnapshots — content-addressed snapshots of fleet rule /
	// memory / model / prompt manifests at experiment start/end.
	db.Exec(`CREATE TABLE IF NOT EXISTS FleetStateSnapshots (
		state_hash                    TEXT PRIMARY KEY,
		computed_at                   TEXT DEFAULT (datetime('now')),
		active_rules_manifest_json    TEXT DEFAULT '{}',
		active_memories_manifest_json TEXT DEFAULT '{}',
		active_models_manifest_json   TEXT DEFAULT '{}',
		active_prompts_manifest_json  TEXT DEFAULT '{}',
		agent_binary_git_sha          TEXT DEFAULT ''
	);`)

	// GlobalHoldouts — long-term reference cohorts (e.g. baseline-2026).
	db.Exec(`CREATE TABLE IF NOT EXISTS GlobalHoldouts (
		id                 INTEGER PRIMARY KEY AUTOINCREMENT,
		name               TEXT UNIQUE NOT NULL,
		reference_date     TEXT DEFAULT (datetime('now')),
		fleet_state_hash   TEXT DEFAULT '',
		ramp_up_days       INTEGER DEFAULT 7,
		plateau_fraction   REAL    DEFAULT 0.02,
		fade_start_at      TEXT    DEFAULT '',
		fade_days          INTEGER DEFAULT 90,
		retired_at         TEXT    DEFAULT '',
		retired_reason     TEXT    DEFAULT '',
		created_by         TEXT    DEFAULT '',
		notes              TEXT    DEFAULT ''
	);`)

	// ModelAvailability — health-watch ledger for model identifiers used
	// as treatment dimensions. Updated by a model-availability dog.
	db.Exec(`CREATE TABLE IF NOT EXISTS ModelAvailability (
		model_id                 TEXT PRIMARY KEY,
		last_checked_at          TEXT DEFAULT '',
		last_success_at          TEXT DEFAULT '',
		deprecation_detected_at  TEXT DEFAULT '',
		announced_kill_at        TEXT DEFAULT '',
		successor_suggested      TEXT DEFAULT ''
	);`)

	// TreatmentApplyLog — log-only audit trail for treatments.Apply.
	// Phase 4 of D3 ships log-only mode (records the call descriptor +
	// assignment intent without mutating the call). Phase 2 flips this
	// live; the log row stays as the source-of-truth audit record.
	// Mentioned in D3 Phase 4's implementation prompt; not in
	// paired-runs.md schema block — added here so log-only writes have
	// a permanent home that does not corrupt live ExperimentRuns data.
	db.Exec(`CREATE TABLE IF NOT EXISTS TreatmentApplyLog (
		id                  INTEGER PRIMARY KEY AUTOINCREMENT,
		applied_at          TEXT    DEFAULT (datetime('now')),
		agent_name          TEXT    NOT NULL,
		natural_unit_kind   TEXT    DEFAULT '',
		natural_unit_id     INTEGER DEFAULT 0,
		prompt_template     TEXT    DEFAULT '',
		model               TEXT    DEFAULT '',
		in_holdout          INTEGER DEFAULT 0,
		assignments_json    TEXT    DEFAULT '[]',
		mode                TEXT    NOT NULL DEFAULT 'log_only'
	);`)
	db.Exec(`CREATE INDEX IF NOT EXISTS idx_treatment_apply_log_ts ON TreatmentApplyLog (applied_at);`)

	// FleetRules — DB as source of truth for what today lives in CLAUDE.md /
	// SENATE.md / BoS rule files / ISB finder configs. Versioned per
	// rule_key; one row is "active" at a time (active_until IS NULL). The
	// renderer (D3 Phase 3) dispatches by render_to:
	//   'claude-md-file'        → CLAUDE.md the file (hard-capped 20 KB)
	//   'agent-prompt'          → per-agent --append-system-prompt content
	//                              filtered by agent_scope
	//   'fix-log'               → FIX-LOG.md historical narrative
	//   'pattern-test-docstring'→ test file docstring + CLAUDE.md cross-ref
	//   'per-domain-doc:<file>' → docs/<file> domain-specific markdown
	//   'discard'               → row kept for history but renders nowhere
	// `enforced_by` references a Pattern test ID (e.g. 'TestPattern_P12') or
	// the literal 'trust-only' for rules without mechanical enforcement.
	db.Exec(`CREATE TABLE IF NOT EXISTS FleetRules (
		id                         INTEGER PRIMARY KEY AUTOINCREMENT,
		rule_key                   TEXT    NOT NULL,
		category                   TEXT    NOT NULL DEFAULT '',
		agent_scope                TEXT    NOT NULL DEFAULT 'all',
		render_to                  TEXT    NOT NULL,
		enforced_by                TEXT    NOT NULL DEFAULT 'trust-only',
		content                    TEXT    NOT NULL,
		content_hash               TEXT    NOT NULL DEFAULT '',
		version                    INTEGER NOT NULL DEFAULT 1,
		active_from                TEXT    DEFAULT (datetime('now')),
		active_until               TEXT    DEFAULT '',
		promoted_by_experiment_id  INTEGER DEFAULT 0,
		created_by                 TEXT    NOT NULL DEFAULT '',
		created_at                 TEXT    DEFAULT (datetime('now'))
	);`)
	// One row per (rule_key, version) — historical lineage is preserved.
	db.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS idx_fleet_rules_key_version
		ON FleetRules(rule_key, version);`)
	// Partial UNIQUE: at most one ACTIVE row per rule_key.
	db.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS idx_fleet_rules_active_key
		ON FleetRules(rule_key) WHERE active_until = '';`)
	// Renderer query path — filter by render_to + agent_scope on active rows.
	db.Exec(`CREATE INDEX IF NOT EXISTS idx_fleet_rules_render_active
		ON FleetRules(render_to, agent_scope) WHERE active_until = '';`)

	// PromotionProposals — Engineering Corps emits these when an experiment
	// concludes; operator ratifies. Concern #7 revert handling is encoded in
	// the rejection_action / rejection_rationale / revert_task_id /
	// refiled_feature_id columns.
	db.Exec(`CREATE TABLE IF NOT EXISTS PromotionProposals (
		id                 INTEGER PRIMARY KEY AUTOINCREMENT,
		experiment_id      INTEGER NOT NULL,
		kind               TEXT    NOT NULL DEFAULT 'promote',
		rule_key           TEXT    DEFAULT '',
		proposed_content   TEXT    DEFAULT '',
		evidence_summary_json TEXT DEFAULT '{}',
		authored_by        TEXT    NOT NULL DEFAULT '',
		authored_at        TEXT    DEFAULT (datetime('now')),
		ratified_at        TEXT    DEFAULT '',
		ratified_by        TEXT    DEFAULT '',
		rejected_at        TEXT    DEFAULT '',
		rejected_reason    TEXT    DEFAULT '',
		ttl_expires_at     TEXT    DEFAULT '',
		rejection_action   TEXT    DEFAULT 'leave_as_is',
		rejection_rationale TEXT   DEFAULT '',
		revert_task_id     INTEGER DEFAULT 0,
		refiled_feature_id INTEGER DEFAULT 0,
		source_memory_id   INTEGER NOT NULL DEFAULT 0
	);`)
	db.Exec(`CREATE INDEX IF NOT EXISTS idx_promotion_proposals_exp ON PromotionProposals (experiment_id);`)
	db.Exec(`CREATE INDEX IF NOT EXISTS idx_promotion_proposals_state ON PromotionProposals (ratified_at, rejected_at);`)
	db.Exec(`CREATE INDEX IF NOT EXISTS idx_promotion_proposals_source_memory ON PromotionProposals (source_memory_id) WHERE source_memory_id != 0;`)

	// D4 Phase 0 — ConflictTickets: pairs of FleetMemory rows that the
	// librarian-conflict-watch dog flagged as contradictory. Operator-
	// surfaced via /api/conflicts/tickets; status transitions: 'open' →
	// 'resolved' (+ resolution_note). reason is a short human-readable
	// classifier (e.g. "antonym", "negation", "llm-judge").
	db.Exec(`CREATE TABLE IF NOT EXISTS ConflictTickets (
		id              INTEGER PRIMARY KEY AUTOINCREMENT,
		memory_a_id     INTEGER NOT NULL,
		memory_b_id     INTEGER NOT NULL,
		reason          TEXT    NOT NULL DEFAULT '',
		status          TEXT    NOT NULL DEFAULT 'open',
		created_at      TEXT    DEFAULT (datetime('now')),
		resolved_at     TEXT    DEFAULT '',
		resolution_note TEXT    DEFAULT ''
	);`)
	db.Exec(`CREATE INDEX IF NOT EXISTS idx_conflict_tickets_status ON ConflictTickets (status, created_at);`)
	// Pair index used by the dedup detector to avoid emitting duplicate
	// tickets for the same pair (memory_a_id, memory_b_id).
	db.Exec(`CREATE INDEX IF NOT EXISTS idx_conflict_tickets_pair ON ConflictTickets (memory_a_id, memory_b_id);`)

	// ProposedFeatures — Investigator's cross-convoy aggregation queue.
	// `fingerprint` (canonical-content SHA256) + partial UNIQUE on active
	// rows enforces dedup (concern #10). value_score / complexity_score are
	// CHECK-constrained to {low, medium, high}; rationale columns let the
	// proposer justify the score.
	db.Exec(`CREATE TABLE IF NOT EXISTS ProposedFeatures (
		id                   INTEGER PRIMARY KEY AUTOINCREMENT,
		observation_summary  TEXT    NOT NULL,
		category             TEXT    NOT NULL,
		source               TEXT    NOT NULL,
		source_observations  TEXT    DEFAULT '[]',
		fingerprint          TEXT    NOT NULL DEFAULT '',
		occurrence_count     INTEGER DEFAULT 1,
		first_seen_at        TEXT    DEFAULT (datetime('now')),
		last_seen_at         TEXT    DEFAULT (datetime('now')),
		evidence_history_json TEXT   DEFAULT '[]',
		value_score          TEXT    NOT NULL DEFAULT 'medium' CHECK(value_score IN ('low','medium','high')),
		complexity_score     TEXT    NOT NULL DEFAULT 'medium' CHECK(complexity_score IN ('low','medium','high')),
		value_rationale      TEXT    DEFAULT '',
		complexity_rationale TEXT    DEFAULT '',
		scored_by            TEXT    NOT NULL DEFAULT '',
		promoted_at          TEXT    DEFAULT '',
		promotion_deadline   TEXT    DEFAULT '',
		status               TEXT    DEFAULT 'pending',
		decided_at           TEXT    DEFAULT '',
		decided_by           TEXT    DEFAULT '',
		decision_action      TEXT    DEFAULT '',
		archived_at          TEXT    DEFAULT '',
		archive_reason       TEXT    DEFAULT ''
	);`)
	// Partial UNIQUE: enforces dedup on active rows; archived dups allowed
	// for history. fingerprint != '' avoids the "blank fingerprint blocks
	// every other blank-fingerprint row" footgun.
	db.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS idx_pf_active_fingerprint
		ON ProposedFeatures(fingerprint)
		WHERE archived_at = '' AND fingerprint != '';`)
	db.Exec(`CREATE INDEX IF NOT EXISTS idx_pf_status ON ProposedFeatures(status, last_seen_at);`)

	// ProposedFeatureSuppressions — operator-installed mute rules. CHECK
	// constraint enforces ≥ 20-char rationale; suppressed_until caps at
	// 1 year out (enforced at the store-helper layer, not the schema).
	db.Exec(`CREATE TABLE IF NOT EXISTS ProposedFeatureSuppressions (
		id                INTEGER PRIMARY KEY AUTOINCREMENT,
		fingerprint       TEXT    NOT NULL,
		rationale         TEXT    NOT NULL CHECK(length(rationale) >= 20),
		suppressed_until  TEXT    NOT NULL,
		created_at        TEXT    DEFAULT (datetime('now')),
		created_by_email  TEXT    NOT NULL
	);`)
	db.Exec(`CREATE INDEX IF NOT EXISTS idx_pfs_fp
		ON ProposedFeatureSuppressions(fingerprint, suppressed_until);`)

	// ProposedFeatureScoreOverrides — audit trail for operator score
	// changes. rationale is mandatory (the operator must justify why they
	// overrode the proposer's value/complexity score).
	db.Exec(`CREATE TABLE IF NOT EXISTS ProposedFeatureScoreOverrides (
		id                     INTEGER PRIMARY KEY AUTOINCREMENT,
		proposed_feature_id    INTEGER NOT NULL,
		prior_value_score      TEXT    DEFAULT '',
		prior_complexity_score TEXT    DEFAULT '',
		new_value_score        TEXT    DEFAULT '',
		new_complexity_score   TEXT    DEFAULT '',
		rationale              TEXT    NOT NULL,
		overridden_at          TEXT    DEFAULT (datetime('now')),
		overridden_by_email    TEXT    NOT NULL
	);`)
	db.Exec(`CREATE INDEX IF NOT EXISTS idx_pfso_pf
		ON ProposedFeatureScoreOverrides(proposed_feature_id);`)

	// ConvoyReviewCycles — concern #6: atomic snapshot evaluations against
	// a frozen spec. cycle_number is monotonic per convoy (UNIQUE) and the
	// spec_version_at_start is frozen at cycle start so amendments
	// mid-cycle take effect at the next cycle, never the in-flight one.
	db.Exec(`CREATE TABLE IF NOT EXISTS ConvoyReviewCycles (
		id                                    INTEGER PRIMARY KEY AUTOINCREMENT,
		convoy_id                             INTEGER NOT NULL,
		cycle_number                          INTEGER NOT NULL,
		spec_version_at_start                 TEXT    NOT NULL,
		cycle_started_at                      TEXT    DEFAULT (datetime('now')),
		cycle_completed_at                    TEXT    DEFAULT '',
		outcomes_json                         TEXT    DEFAULT '{}',
		fix_tasks_spawned_json                TEXT    DEFAULT '[]',
		amendments_proposed_json              TEXT    DEFAULT '[]',
		amendments_ratified_during_cycle_json TEXT    DEFAULT '[]',
		UNIQUE (convoy_id, cycle_number)
	);`)
	db.Exec(`CREATE INDEX IF NOT EXISTS idx_crc_convoy ON ConvoyReviewCycles(convoy_id, cycle_number);`)

	// AdversarialPairings — Council/Medic/ConvoyReview adversarial-pair
	// results. The primary prompt and a critic prompt run on the same
	// decision; a disagreement surfaces to the operator.
	//
	// prompt_version_primary / prompt_version_critic record which prompt
	// produced each outcome. D3 P5 anti-cheat invariant: critic must use
	// a DIFFERENT prompt-version key from primary; identical-prompt pairs
	// are a sham. Both default to '' so legacy rows (written before D3 P5)
	// round-trip cleanly through SELECT.
	db.Exec(`CREATE TABLE IF NOT EXISTS AdversarialPairings (
		id                       INTEGER PRIMARY KEY AUTOINCREMENT,
		decision_id              INTEGER NOT NULL,
		agent                    TEXT    NOT NULL,
		primary_outcome          TEXT    NOT NULL,
		critic_outcome           TEXT    NOT NULL,
		prompt_version_primary   TEXT    DEFAULT '',
		prompt_version_critic    TEXT    DEFAULT '',
		agreement                INTEGER DEFAULT 0,
		surfaced_at              TEXT    DEFAULT '',
		operator_resolution      TEXT    DEFAULT '',
		created_at               TEXT    DEFAULT (datetime('now'))
	);`)
	db.Exec(`CREATE INDEX IF NOT EXISTS idx_adv_pairings_agent ON AdversarialPairings(agent, created_at);`)
	db.Exec(`CREATE INDEX IF NOT EXISTS idx_adv_pairings_disagreements
		ON AdversarialPairings(agent) WHERE agreement = 0;`)

	// GoldenSetFixtures — curated input fixtures with known-correct
	// outputs. Source and curator captured for audit; retired_at allows
	// removing fixtures whose contracts have shifted.
	db.Exec(`CREATE TABLE IF NOT EXISTS GoldenSetFixtures (
		id              INTEGER PRIMARY KEY AUTOINCREMENT,
		agent           TEXT    NOT NULL,
		input           TEXT    NOT NULL,
		expected_output TEXT    NOT NULL,
		source          TEXT    NOT NULL,
		curated_at      TEXT    DEFAULT (datetime('now')),
		curated_by      TEXT    DEFAULT '',
		retired_at      TEXT    DEFAULT ''
	);`)
	db.Exec(`CREATE INDEX IF NOT EXISTS idx_gsf_agent ON GoldenSetFixtures(agent) WHERE retired_at = '';`)

	// GoldenSetEvaluations — periodic prompt-vs-fixture evaluation
	// results. Aggregated per (agent, prompt_version, week) for
	// accuracy-trend tracking.
	db.Exec(`CREATE TABLE IF NOT EXISTS GoldenSetEvaluations (
		id              INTEGER PRIMARY KEY AUTOINCREMENT,
		agent           TEXT    NOT NULL,
		prompt_version  TEXT    NOT NULL,
		fixture_id      INTEGER NOT NULL,
		actual_output   TEXT    NOT NULL,
		accuracy_score  REAL,
		evaluated_at    TEXT    DEFAULT (datetime('now'))
	);`)
	db.Exec(`CREATE INDEX IF NOT EXISTS idx_gse_agent_version
		ON GoldenSetEvaluations(agent, prompt_version, evaluated_at);`)

	// CalibrationAuditSamples — weekly calibration sample widget records.
	// Operator confirms / overrides past auto-decisions; surfaces
	// systematic bias.
	db.Exec(`CREATE TABLE IF NOT EXISTS CalibrationAuditSamples (
		id                  INTEGER PRIMARY KEY AUTOINCREMENT,
		sample_week         TEXT    NOT NULL,
		proposal_id         INTEGER NOT NULL,
		selection_bucket    TEXT    NOT NULL,
		surfaced_at         TEXT    DEFAULT (datetime('now')),
		operator_action     TEXT    DEFAULT '',
		operator_acted_at   TEXT    DEFAULT '',
		operator_rationale  TEXT    DEFAULT ''
	);`)
	db.Exec(`CREATE INDEX IF NOT EXISTS idx_cas_week ON CalibrationAuditSamples(sample_week);`)

	// DisagreementPairs (D3 P3) — rolling-window per-pair cross-layer
	// disagreement rates. Populated by dogDisagreementTracker; surfaced by
	// /api/disagreement-rates. One row per (pair_name, window_start,
	// window_end); a re-tick over the same window UPSERT-overwrites.
	// Pairs tracked: 'captain-council-reject', 'council-ci-fail',
	// 'convoy-review-cant-fix', 'senate-chancellor-decline' (deferred until
	// D4 — Senate ships then), 'operator-revert-30d'.
	db.Exec(`CREATE TABLE IF NOT EXISTS DisagreementPairs (
		id                 INTEGER PRIMARY KEY AUTOINCREMENT,
		pair_name          TEXT    NOT NULL,
		window_start       TEXT    NOT NULL,
		window_end         TEXT    NOT NULL,
		sample_count       INTEGER NOT NULL,
		disagreement_count INTEGER NOT NULL,
		rate               REAL    NOT NULL,
		computed_at        TEXT    DEFAULT (datetime('now')),
		UNIQUE(pair_name, window_start, window_end)
	);`)
	db.Exec(`CREATE INDEX IF NOT EXISTS idx_disagreement_pair_window
		ON DisagreementPairs(pair_name, window_end DESC);`)

	// ── D3 Phase 6 dashboard data-layer prerequisites ────────────────────────
	// Per dashboard-implementation.md: schema lands in Phase 1 so 6A/6B build
	// against a stable data layer. No runtime code consumes these yet — the
	// dashboard tasks (6A.2, 6A.4, 6A.5, etc.) wire them up.

	// DashboardHealthHeartbeats (6A.2) — heartbeat goroutine ticks every 30s.
	db.Exec(`CREATE TABLE IF NOT EXISTS DashboardHealthHeartbeats (
		id                 INTEGER PRIMARY KEY AUTOINCREMENT,
		ticked_at          TEXT    NOT NULL DEFAULT (datetime('now')),
		process_pid        INTEGER DEFAULT 0,
		bind_addr          TEXT    DEFAULT '',
		in_flight_requests INTEGER DEFAULT 0
	);`)
	db.Exec(`CREATE INDEX IF NOT EXISTS idx_dh_heartbeats_recent ON DashboardHealthHeartbeats(ticked_at DESC);`)

	// OperatorNotificationBudgets (6A.4) — per-(operator, source, channel)
	// rate-limit configuration; respectNotificationBudget queries this.
	db.Exec(`CREATE TABLE IF NOT EXISTS OperatorNotificationBudgets (
		id              INTEGER PRIMARY KEY AUTOINCREMENT,
		operator_email  TEXT    NOT NULL,
		source          TEXT    NOT NULL,
		channel         TEXT    NOT NULL,
		max_per_period  INTEGER NOT NULL,
		period_minutes  INTEGER NOT NULL,
		digest_remainder INTEGER NOT NULL DEFAULT 1,
		UNIQUE(operator_email, source, channel)
	);`)

	// OperatorNotificationDigest (6A.4) — deferred-notification spool.
	db.Exec(`CREATE TABLE IF NOT EXISTS OperatorNotificationDigest (
		id              INTEGER PRIMARY KEY AUTOINCREMENT,
		operator_email  TEXT    NOT NULL,
		source          TEXT    NOT NULL,
		channel         TEXT    NOT NULL,
		digest_for_date TEXT    NOT NULL,
		payload_json    TEXT    NOT NULL,
		flushed_at      TEXT    DEFAULT '',
		UNIQUE(operator_email, source, channel, digest_for_date)
	);`)

	// OperatorSessionState (6A.5) — resume-where-you-left-off state.
	// partial_review_state_json is bounded at 32 KB at write time.
	db.Exec(`CREATE TABLE IF NOT EXISTS OperatorSessionState (
		id                        INTEGER PRIMARY KEY AUTOINCREMENT,
		operator_email            TEXT    NOT NULL UNIQUE,
		last_active_at            TEXT    DEFAULT (datetime('now')),
		last_viewed_surface       TEXT    DEFAULT '',
		last_viewed_route         TEXT    DEFAULT '',
		last_focused_decision_id  INTEGER DEFAULT 0,
		partial_review_state_json TEXT    DEFAULT ''
	);`)

	// OperatorTrustDials (6A.6) — per-(operator, agent) trust slider.
	// History-preserving: latest set_at row is current value.
	db.Exec(`CREATE TABLE IF NOT EXISTS OperatorTrustDials (
		id              INTEGER PRIMARY KEY AUTOINCREMENT,
		operator_email  TEXT    NOT NULL,
		agent           TEXT    NOT NULL,
		dial_value      INTEGER NOT NULL CHECK(dial_value BETWEEN 0 AND 100),
		set_at          TEXT    DEFAULT (datetime('now')),
		set_by          TEXT    NOT NULL DEFAULT '',
		rationale       TEXT    DEFAULT '',
		UNIQUE(operator_email, agent, set_at)
	);`)

	// NarrativeRenders (6A.7) — LLM-batched live narrative panel results.
	db.Exec(`CREATE TABLE IF NOT EXISTS NarrativeRenders (
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
	);`)
	db.Exec(`CREATE INDEX IF NOT EXISTS idx_nr_window ON NarrativeRenders(event_window_end DESC);`)

	// BriefingRenders (6A.10/6A.11) — Briefing decision rendered text +
	// counter-proposal capture.
	db.Exec(`CREATE TABLE IF NOT EXISTS BriefingRenders (
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
	);`)
	db.Exec(`CREATE INDEX IF NOT EXISTS idx_br_decision ON BriefingRenders(decision_kind, decision_id, rendered_at DESC);`)

	// CooldownPauses (6A.13) — high-stakes auto-execute cooldown banner.
	db.Exec(`CREATE TABLE IF NOT EXISTS CooldownPauses (
		id                  INTEGER PRIMARY KEY AUTOINCREMENT,
		decision_id         INTEGER NOT NULL,
		decision_kind       TEXT    NOT NULL,
		scheduled_action_at TEXT    NOT NULL,
		paused_at           TEXT    DEFAULT '',
		paused_by_email     TEXT    DEFAULT '',
		resumed_at          TEXT    DEFAULT '',
		cancelled_at        TEXT    DEFAULT '',
		executed_at         TEXT    DEFAULT ''
	);`)
	db.Exec(`CREATE INDEX IF NOT EXISTS idx_cp_pending ON CooldownPauses(scheduled_action_at)
		WHERE executed_at = '' AND cancelled_at = '';`)

	// OperatorAttentionTags (6A.14) — operator-pinned attention to convoys
	// / features / agents / rule keys.
	db.Exec(`CREATE TABLE IF NOT EXISTS OperatorAttentionTags (
		id              INTEGER PRIMARY KEY AUTOINCREMENT,
		operator_email  TEXT    NOT NULL,
		target_kind     TEXT    NOT NULL,
		target_id       TEXT    NOT NULL,
		attention_level TEXT    NOT NULL CHECK(attention_level IN ('following','normal','muted')),
		set_at          TEXT    DEFAULT (datetime('now')),
		rationale       TEXT    DEFAULT '',
		UNIQUE(operator_email, target_kind, target_id)
	);`)

	// LLMCallTranscripts (6B.1) — redacted at write time per Fix #10.
	db.Exec(`CREATE TABLE IF NOT EXISTS LLMCallTranscripts (
		id                     INTEGER PRIMARY KEY AUTOINCREMENT,
		task_id                INTEGER DEFAULT 0,
		agent                  TEXT    NOT NULL,
		prompt_version         TEXT    NOT NULL DEFAULT '',
		call_started_at        TEXT    NOT NULL,
		call_completed_at      TEXT    DEFAULT '',
		system_prompt          TEXT    NOT NULL,
		user_prompt            TEXT    NOT NULL,
		response_text          TEXT    DEFAULT '',
		tool_calls_json        TEXT    DEFAULT '[]',
		cost_usd               REAL    DEFAULT 0,
		input_tokens           INTEGER DEFAULT 0,
		output_tokens          INTEGER DEFAULT 0,
		cache_read_tokens      INTEGER DEFAULT 0,
		cache_creation_tokens  INTEGER DEFAULT 0,
		archived_at            TEXT    DEFAULT ''
	);`)
	db.Exec(`CREATE INDEX IF NOT EXISTS idx_llmct_task ON LLMCallTranscripts(task_id, call_started_at);`)
	db.Exec(`CREATE INDEX IF NOT EXISTS idx_llmct_agent ON LLMCallTranscripts(agent, call_started_at);`)

	// GitOperationLog (6B.2) — every git/gh op is recorded for the drill view.
	db.Exec(`CREATE TABLE IF NOT EXISTS GitOperationLog (
		id              INTEGER PRIMARY KEY AUTOINCREMENT,
		task_id         INTEGER DEFAULT 0,
		convoy_id       INTEGER DEFAULT 0,
		repo            TEXT    NOT NULL,
		operation       TEXT    NOT NULL,
		args_json       TEXT    DEFAULT '[]',
		started_at      TEXT    NOT NULL,
		duration_ms     INTEGER DEFAULT 0,
		exit_code       INTEGER DEFAULT 0,
		stdout_excerpt  TEXT    DEFAULT '',
		stderr_excerpt  TEXT    DEFAULT '',
		branch          TEXT    DEFAULT '',
		before_sha      TEXT    DEFAULT '',
		after_sha       TEXT    DEFAULT ''
	);`)
	db.Exec(`CREATE INDEX IF NOT EXISTS idx_gol_convoy ON GitOperationLog(convoy_id, started_at);`)
	db.Exec(`CREATE INDEX IF NOT EXISTS idx_gol_task   ON GitOperationLog(task_id, started_at);`)

	// OperatorEventAnnotations (6B.8) — operator notes on events.
	db.Exec(`CREATE TABLE IF NOT EXISTS OperatorEventAnnotations (
		id              INTEGER PRIMARY KEY AUTOINCREMENT,
		operator_email  TEXT    NOT NULL,
		event_kind      TEXT    NOT NULL,
		event_ref       TEXT    NOT NULL,
		note_text       TEXT    NOT NULL,
		flag            TEXT    DEFAULT '',
		noted_at        TEXT    DEFAULT (datetime('now'))
	);`)
	db.Exec(`CREATE INDEX IF NOT EXISTS idx_oea_event ON OperatorEventAnnotations(event_kind, event_ref);`)
	db.Exec(`CREATE INDEX IF NOT EXISTS idx_oea_flag  ON OperatorEventAnnotations(flag, noted_at) WHERE flag != '';`)

	// ReplayResults (6B.7) — replay an old decision against the current prompt.
	db.Exec(`CREATE TABLE IF NOT EXISTS ReplayResults (
		id                    INTEGER PRIMARY KEY AUTOINCREMENT,
		original_event_id     INTEGER NOT NULL,
		original_event_kind   TEXT    NOT NULL,
		replay_prompt_version TEXT    NOT NULL,
		replay_started_at     TEXT    DEFAULT (datetime('now')),
		replay_response       TEXT    DEFAULT '',
		decision_changed      INTEGER DEFAULT 0,
		cost_usd              REAL    DEFAULT 0,
		triggered_by_email    TEXT    NOT NULL DEFAULT ''
	);`)

	// FleetLearningPanels (6B.12) — synthesised "what the fleet learned" panels.
	db.Exec(`CREATE TABLE IF NOT EXISTS FleetLearningPanels (
		id                     INTEGER PRIMARY KEY AUTOINCREMENT,
		rendered_at            TEXT    DEFAULT (datetime('now')),
		prose                  TEXT    NOT NULL,
		cost_usd               REAL    DEFAULT 0,
		prompt_version         TEXT    NOT NULL DEFAULT '',
		source_event_refs_json TEXT    DEFAULT '[]'
	);`)

	// D4 Phase 1 — SecurityFindings. Shared between Bureau of Standards (BoS,
	// commit-time AST review) and Imperial Security Bureau (ISB, Phase 2,
	// security review). Each row records one finding from one rule against
	// one task's diff. The disposition column captures override flow:
	//   ''           : finding active / unresolved (default)
	//   'overridden' : a // BOS-BYPASS / ISB-BYPASS comment downgraded the
	//                   finding from block→advisory; bypass_audit_id +
	//                   bypass_reason capture the audit trail.
	//   'resolved'   : the violating code was removed in a follow-up commit.
	//   'suppressed' : operator-level mute (rare; future workflow).
	//   'closed'     : sweeper-closed terminal state.
	// The (rule_id, severity, disposition) index is the dashboard-view path
	// — "show me all open block-severity BOS-001 findings" runs on it.
	db.Exec(`CREATE TABLE IF NOT EXISTS SecurityFindings (
		id              INTEGER PRIMARY KEY AUTOINCREMENT,
		task_id         INTEGER NOT NULL DEFAULT 0,
		bureau          TEXT    NOT NULL DEFAULT 'BoS',
		rule_id         TEXT    NOT NULL,
		severity        TEXT    NOT NULL DEFAULT 'advise',
		file_path       TEXT    NOT NULL DEFAULT '',
		line_number     INTEGER NOT NULL DEFAULT 0,
		message         TEXT    NOT NULL DEFAULT '',
		commit_sha      TEXT    NOT NULL DEFAULT '',
		disposition     TEXT    NOT NULL DEFAULT '',
		bypass_audit_id TEXT    NOT NULL DEFAULT '',
		bypass_reason   TEXT    NOT NULL DEFAULT '',
		created_at      TEXT    DEFAULT (datetime('now')),
		resolved_at     TEXT    NOT NULL DEFAULT ''
	);`)
	db.Exec(`CREATE INDEX IF NOT EXISTS idx_sec_findings_task     ON SecurityFindings(task_id);`)
	db.Exec(`CREATE INDEX IF NOT EXISTS idx_sec_findings_rule     ON SecurityFindings(rule_id, created_at);`)
	db.Exec(`CREATE INDEX IF NOT EXISTS idx_sec_findings_dashboard ON SecurityFindings(rule_id, severity, disposition);`)

	// D4 Phase 3 — Senate. Three tables back the Senator review layer
	// (docs/next-gen-agents.md § "Senate" / § "Storage"). The Senator is
	// the repo-aware advisor consulted by the Chancellor between the
	// ProposedConvoys write and the AwaitingChancellorReview transition.
	//
	//   SenateChambers — one row per Senator (keyed by senator_name).
	//   SenateMemory   — append-only memory store the Senator reads in
	//                    its prompt context (ranked by weight desc).
	//   SenateReview   — one row per (Feature, Senator) verdict.
	db.Exec(`CREATE TABLE IF NOT EXISTS SenateChambers (
		senator_name      TEXT PRIMARY KEY,
		scope             TEXT NOT NULL,
		senate_md_path    TEXT NOT NULL DEFAULT '',
		status            TEXT NOT NULL DEFAULT 'active',
		onboarded_at      TEXT NOT NULL DEFAULT '',
		last_refreshed_at TEXT NOT NULL DEFAULT '',
		retired_at        TEXT NOT NULL DEFAULT '',
		created_at        TEXT NOT NULL DEFAULT (datetime('now'))
	);`)

	db.Exec(`CREATE TABLE IF NOT EXISTS SenateMemory (
		id                 INTEGER PRIMARY KEY AUTOINCREMENT,
		senator            TEXT NOT NULL,
		topic              TEXT NOT NULL DEFAULT '',
		summary            TEXT NOT NULL,
		source             TEXT NOT NULL DEFAULT 'manual',
		weight             REAL NOT NULL DEFAULT 1.0,
		retrieval_count    INTEGER NOT NULL DEFAULT 0,
		last_consulted_at  TEXT NOT NULL DEFAULT '',
		created_at         TEXT NOT NULL DEFAULT (datetime('now'))
	);`)
	db.Exec(`CREATE INDEX IF NOT EXISTS idx_senate_memory_senator ON SenateMemory(senator, weight DESC);`)

	db.Exec(`CREATE TABLE IF NOT EXISTS SenateReview (
		id          INTEGER PRIMARY KEY AUTOINCREMENT,
		feature_id  INTEGER NOT NULL,
		senator     TEXT    NOT NULL,
		position    TEXT    NOT NULL,
		concerns    TEXT    NOT NULL DEFAULT '[]',
		amendments  TEXT    NOT NULL DEFAULT '[]',
		rationale   TEXT    NOT NULL DEFAULT '',
		confidence  REAL    NOT NULL DEFAULT 0,
		created_at  TEXT    NOT NULL DEFAULT (datetime('now'))
	);`)
	db.Exec(`CREATE INDEX IF NOT EXISTS idx_senate_review_feature ON SenateReview(feature_id);`)

	// ── D9 — Archaeologist findings (proactive debt detection) ───────────────
	// One row per (pattern_id, repo_id, file_path, line_number) hit. The
	// Archaeologist's claim loop writes findings on every ArchaeologistSweep
	// task; the ArchaeologistProposeMigration task type fires when a
	// pattern's open-status hit count exceeds Pattern.MinHitsForFeature().
	// Status flows open → proposed → migrated|rejected.
	db.Exec(`CREATE TABLE IF NOT EXISTS ArchaeologistFindings (
		id           INTEGER PRIMARY KEY AUTOINCREMENT,
		pattern_id   TEXT    NOT NULL,
		repo_id      INTEGER NOT NULL,
		file_path    TEXT    NOT NULL,
		line_number  INTEGER NOT NULL,
		detail_json  TEXT    NOT NULL DEFAULT '{}',
		detected_at  TEXT    NOT NULL,
		status       TEXT    NOT NULL DEFAULT 'open',
		UNIQUE(pattern_id, repo_id, file_path, line_number)
	);`)
	db.Exec(`CREATE INDEX IF NOT EXISTS idx_arch_findings_pattern ON ArchaeologistFindings(pattern_id, status);`)
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
	// D2 T1-1 — per-attempt cost estimate (REAL). Default 0 so old rows return
	// a clean zero in dashboard sums without ALTER errors on a re-run.
	db.Exec(`ALTER TABLE TaskHistory ADD COLUMN cost_usd_estimate REAL DEFAULT 0`)

	// D2 T1-1 — BountyBoard.spend_suspended. Set to 1 by dogTaskSpendWatch
	// when a single task's trailing-10-min spend crosses the escalate
	// threshold. ClaimBounty / ClaimForReview / ClaimForCaptainReview filter
	// rows with spend_suspended=1 so a runaway cost loop on one task can't
	// burn another claim cycle.
	db.Exec(`ALTER TABLE BountyBoard ADD COLUMN spend_suspended INTEGER DEFAULT 0`)

	// D2 T1-3.5 — BountyBoard.recent_commit_hashes_json. JSON array of the
	// last `recentCommitHashRingDepth` commit tree-hashes produced by this
	// task's worktree. Default '[]' so existing rows are coherent without a
	// backfill UPDATE; reconcile-on-startup Case E reads the most recent
	// entry as the "task-owned SHA" and verifies the tree-hash is reachable
	// from the recorded branch.
	db.Exec(`ALTER TABLE BountyBoard ADD COLUMN recent_commit_hashes_json TEXT DEFAULT '[]'`)

	// D2 T1-1 — TaskSpendWatch dedup ledger. Created here for upgrade-path DBs
	// that pre-date the createSchema declaration. The CREATE TABLE IF NOT
	// EXISTS form keeps the migration idempotent.
	db.Exec(`CREATE TABLE IF NOT EXISTS TaskSpendWatch (
		id           INTEGER PRIMARY KEY AUTOINCREMENT,
		task_id      INTEGER NOT NULL,
		window_start TEXT    NOT NULL,
		cost_usd     REAL    DEFAULT 0,
		notified_at  TEXT    DEFAULT (datetime('now'))
	)`)
	db.Exec(`CREATE INDEX IF NOT EXISTS idx_taskspendwatch_task_window ON TaskSpendWatch (task_id, window_start)`)

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

	// ── D2 T1-4: Repositories.mode (read_only | write | quarantined) ─────────
	// Tri-state mode column. Fresh installs (createSchema) carry the CHECK
	// constraint and a default of 'read_only' — repos opt INTO write mode via
	// the dashboard's promote-to-write button (audit-logged). Existing repos
	// get 'write' to preserve current behavior; the operator must explicitly
	// step them down if a repo should be read-only.
	//
	// SQLite ALTER TABLE ADD COLUMN cannot retroactively add a CHECK
	// constraint to an existing table without a full rebuild. The CHECK lives
	// on createSchema (where fresh DBs pick it up); migrated DBs rely on the
	// SetRepoMode store-layer validator and the AssertRepoWritable guard for
	// enforcement. This is the same belt-and-suspenders pattern used for
	// other ALTER-added columns where the CHECK lives in createSchema only.
	db.Exec(`ALTER TABLE Repositories ADD COLUMN mode TEXT NOT NULL DEFAULT 'read_only'`)
	// Backfill: existing rows (which predate the mode column) keep behaving
	// as they did pre-migration — i.e. fully writable. New rows added via
	// store.AddRepo opt INTO read-only by writing 'read_only' explicitly.
	// The UPDATE is idempotent: re-running it after a fresh install (where
	// the column already exists with default 'read_only') would clobber
	// new repos. Gate on the column being NULL or empty (the post-ALTER
	// state for pre-existing rows on older SQLite versions). Since
	// `NOT NULL DEFAULT 'read_only'` was applied on ALTER, every existing
	// row got 'read_only' — but we want them to stay as they were
	// (effectively 'write'). We backfill ONCE by checking whether the
	// audit log has ever recorded a mode set for this repo; absent that,
	// stamp 'write'. This keeps the migration idempotent across restarts.
	//
	// Simpler version: a repo whose row pre-existed the migration MUST
	// have remote_url, default_branch, etc. populated by Layer B at some
	// point, OR (for test rows) a non-empty local_path. The migration
	// flag we use is a SystemConfig key — set after the first migration
	// run, checked on subsequent runs.
	var migratedAlready string
	db.QueryRow(`SELECT IFNULL(value, '') FROM SystemConfig WHERE key = 'd2_t14_mode_backfilled'`).Scan(&migratedAlready)
	if migratedAlready != "1" {
		// One-shot: every existing repo row gets mode='write' so prior
		// behavior is preserved. Idempotent guard above prevents
		// re-clobbering on subsequent startups (which would step a
		// freshly-created read-only repo back up to write, defeating the
		// invariant).
		db.Exec(`UPDATE Repositories SET mode = 'write' WHERE mode = 'read_only' OR mode = '' OR mode IS NULL`)
		db.Exec(`INSERT INTO SystemConfig (key, value) VALUES ('d2_t14_mode_backfilled', '1')
			ON CONFLICT(key) DO UPDATE SET value = excluded.value`)
	}

	// ── D5.5 Staged Convoys ──────────────────────────────────────────────────
	// Additive: ConvoyStages table + Convoys.staging_mode/staging_strategy +
	// ConvoyAskBranches.stage_id + Repositories.release_label_pattern. All
	// idempotent (ALTER no-ops on duplicate columns; CREATE IF NOT EXISTS).
	db.Exec(`ALTER TABLE Convoys ADD COLUMN staging_mode     TEXT NOT NULL DEFAULT 'single'`)
	db.Exec(`ALTER TABLE Convoys ADD COLUMN staging_strategy TEXT NOT NULL DEFAULT 'strict'`)
	db.Exec(`ALTER TABLE ConvoyAskBranches ADD COLUMN stage_id INTEGER`)
	db.Exec(`ALTER TABLE Repositories ADD COLUMN release_label_pattern TEXT NOT NULL DEFAULT ''`)
	db.Exec(`CREATE TABLE IF NOT EXISTS ConvoyStages (
		id                   INTEGER PRIMARY KEY AUTOINCREMENT,
		convoy_id            INTEGER NOT NULL REFERENCES Convoys(id),
		stage_num            INTEGER NOT NULL,
		intent_text          TEXT    NOT NULL DEFAULT '',
		status               TEXT    NOT NULL DEFAULT 'Pending',
		gate_type            TEXT,
		gate_config_json     TEXT    NOT NULL DEFAULT '{}',
		gate_timeout_minutes INTEGER NOT NULL DEFAULT 10080,
		opened_at            TEXT,
		all_prs_merged_at    TEXT,
		gate_passed_at       TEXT,
		completed_at         TEXT,
		UNIQUE(convoy_id, stage_num)
	)`)
	db.Exec(`CREATE INDEX IF NOT EXISTS idx_convoy_stages_convoy_id ON ConvoyStages (convoy_id)`)
	db.Exec(`CREATE INDEX IF NOT EXISTS idx_convoy_stages_status    ON ConvoyStages (status)`)
	db.Exec(`CREATE INDEX IF NOT EXISTS idx_convoy_ask_branches_stage_id ON ConvoyAskBranches (stage_id)`)

	// D5.5 P2 — BountyBoard.stage_id. Nullable INTEGER pointing at
	// ConvoyStages.id; NULL means "task is not assigned to any specific
	// stage" (legacy single-mode convoys + non-convoy tasks). Multi-stage
	// convoys populate stage_id at insert time so per-stage dispatch + the
	// convoy-stage-watch dog can scope queries by stage. The ALTER is
	// idempotent (silent no-op on duplicate column name); the partial
	// index follows the createSchema definition so upgraded DBs converge
	// on the same shape.
	db.Exec(`ALTER TABLE BountyBoard ADD COLUMN stage_id INTEGER DEFAULT NULL`)
	db.Exec(`CREATE INDEX IF NOT EXISTS idx_bounty_stage_id ON BountyBoard (stage_id) WHERE stage_id IS NOT NULL`)

	// Forward-compat data migration: every pre-D5.5 convoy is implicitly a
	// single-stage convoy. Create stage 1 (status=Open, gate_type=NULL,
	// opened_at=convoy.created_at) for every convoy that does not already
	// have one, then point each ConvoyAskBranches row at that stage. Both
	// inserts/updates are idempotent — re-running is a no-op.
	db.Exec(`INSERT INTO ConvoyStages
		(convoy_id, stage_num, intent_text, status, gate_type, gate_config_json, opened_at)
		SELECT c.id, 1, '', 'Open', NULL, '{}',
			COALESCE(NULLIF(c.created_at, ''), datetime('now'))
		FROM Convoys c
		WHERE NOT EXISTS (
			SELECT 1 FROM ConvoyStages s
			WHERE s.convoy_id = c.id AND s.stage_num = 1
		)`)
	db.Exec(`UPDATE ConvoyAskBranches
		SET stage_id = (
			SELECT s.id FROM ConvoyStages s
			WHERE s.convoy_id = ConvoyAskBranches.convoy_id AND s.stage_num = 1
		)
		WHERE stage_id IS NULL`)

	// ── Fleet memory: topic_tags column + FTS rebuild ────────────────────────
	// Additive column on the main table; for the FTS5 virtual table we need to
	// drop-and-recreate because FTS5 columns are immutable. After recreating,
	// re-populate from FleetMemory so no search data is lost.
	db.Exec(`ALTER TABLE FleetMemory ADD COLUMN topic_tags TEXT DEFAULT ''`)

	// ── D4 Phase 0 — Librarian evolution: quality-scoring columns ────────────
	// Each ALTER is idempotent via SQLite's silent-failure-on-duplicate
	// behaviour for ADD COLUMN. The createSchema row defaults match these
	// (1.0 freshness, 0.0 validation, 0 retrieval count, '' last-retrieved-
	// at, 0 canonical_id, '' hypothesis_emitted_at) so fresh DBs and
	// upgraded DBs converge on the same shape (per CLAUDE.md "Store /
	// schema conventions" — column adds need to land in createSchema +
	// runMigrations + schema/schema.sql in the same commit).
	db.Exec(`ALTER TABLE FleetMemory ADD COLUMN freshness_score REAL NOT NULL DEFAULT 1.0`)
	db.Exec(`ALTER TABLE FleetMemory ADD COLUMN validation_score REAL NOT NULL DEFAULT 0.0`)
	db.Exec(`ALTER TABLE FleetMemory ADD COLUMN retrieval_count INTEGER NOT NULL DEFAULT 0`)
	db.Exec(`ALTER TABLE FleetMemory ADD COLUMN last_retrieved_at TEXT DEFAULT ''`)
	db.Exec(`ALTER TABLE FleetMemory ADD COLUMN canonical_id INTEGER NOT NULL DEFAULT 0`)
	db.Exec(`ALTER TABLE FleetMemory ADD COLUMN hypothesis_emitted_at TEXT DEFAULT ''`)

	// ── D4 Phase 0 — PromotionProposals.source_memory_id ─────────────────────
	// Used by EmitHypothesisCandidates to track which FleetMemory row a
	// candidate proposal was emitted from, so re-running the dog over the
	// same high-signal memory does not produce duplicates (idempotence
	// invariant). Default 0 matches "no source memory" for non-Librarian-
	// emitted candidates (EC promotions, operator-direct-write rows).
	db.Exec(`ALTER TABLE PromotionProposals ADD COLUMN source_memory_id INTEGER NOT NULL DEFAULT 0`)
	db.Exec(`CREATE INDEX IF NOT EXISTS idx_promotion_proposals_source_memory ON PromotionProposals (source_memory_id) WHERE source_memory_id != 0`)

	// ── D4 Phase 0 — ConflictTickets table ────────────────────────────────────
	db.Exec(`CREATE TABLE IF NOT EXISTS ConflictTickets (
		id              INTEGER PRIMARY KEY AUTOINCREMENT,
		memory_a_id     INTEGER NOT NULL,
		memory_b_id     INTEGER NOT NULL,
		reason          TEXT    NOT NULL DEFAULT '',
		status          TEXT    NOT NULL DEFAULT 'open',
		created_at      TEXT    DEFAULT (datetime('now')),
		resolved_at     TEXT    DEFAULT '',
		resolution_note TEXT    DEFAULT ''
	)`)
	db.Exec(`CREATE INDEX IF NOT EXISTS idx_conflict_tickets_status ON ConflictTickets (status, created_at)`)
	db.Exec(`CREATE INDEX IF NOT EXISTS idx_conflict_tickets_pair ON ConflictTickets (memory_a_id, memory_b_id)`)

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

	// ── PromptByteAttribution (D2 T1-2) ──────────────────────────────────────
	// Per-call source-tag breakdown of the assembled LLM prompt. Idempotent
	// via IF NOT EXISTS; the indexes match the per-task and per-agent +
	// time-window aggregation paths used by the dashboard handler.
	db.Exec(`CREATE TABLE IF NOT EXISTS PromptByteAttribution (
		id              INTEGER PRIMARY KEY AUTOINCREMENT,
		task_id         INTEGER NOT NULL DEFAULT 0,
		agent_name      TEXT    NOT NULL,
		call_timestamp  TEXT    NOT NULL DEFAULT (datetime('now')),
		source_tag      TEXT    NOT NULL,
		bytes           INTEGER NOT NULL DEFAULT 0
	)`)
	db.Exec(`CREATE INDEX IF NOT EXISTS idx_prompt_byte_attr_task     ON PromptByteAttribution (task_id, call_timestamp)`)
	db.Exec(`CREATE INDEX IF NOT EXISTS idx_prompt_byte_attr_agent_ts ON PromptByteAttribution (agent_name, call_timestamp)`)

	// ── D3 Phase 1: paired-runs core schema (upgrade path) ───────────────────
	// Idempotent CREATE TABLE IF NOT EXISTS for upgraded DBs. createSchema
	// holds the authoritative column list (parity-tested); these mirrors
	// keep upgraded DBs in lockstep without paying for a full rebuild.
	db.Exec(`CREATE TABLE IF NOT EXISTS Experiments (
		id                          INTEGER PRIMARY KEY AUTOINCREMENT,
		name                        TEXT    NOT NULL,
		hypothesis_text             TEXT    NOT NULL DEFAULT '',
		min_practical_effect        REAL    DEFAULT 0,
		stakes_tier                 TEXT    NOT NULL DEFAULT 'low',
		declare_threshold_override  REAL,
		factorial_dimensions_json   TEXT    DEFAULT '[]',
		kind                        TEXT    NOT NULL DEFAULT 'single' CHECK (kind IN ('single','factorial')),
		factors_json                TEXT    DEFAULT '[]',
		subject_agent               TEXT    NOT NULL DEFAULT '',
		assignment_unit             TEXT    NOT NULL DEFAULT 'task',
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
	)`)
	db.Exec(`CREATE INDEX IF NOT EXISTS idx_experiments_status      ON Experiments (status)`)
	db.Exec(`CREATE INDEX IF NOT EXISTS idx_experiments_subject     ON Experiments (subject_agent, status)`)
	db.Exec(`CREATE INDEX IF NOT EXISTS idx_experiments_kind_status ON Experiments (kind, status)`)

	db.Exec(`CREATE TABLE IF NOT EXISTS TreatmentSpecs (
		id                       INTEGER PRIMARY KEY AUTOINCREMENT,
		spec_hash                TEXT    UNIQUE NOT NULL,
		prompt_template_ref      TEXT    DEFAULT '',
		prompt_template_content  TEXT    DEFAULT '',
		rule_set_refs_json       TEXT    DEFAULT '[]',
		memory_bundle_ref        TEXT    DEFAULT '',
		memory_bundle_content    TEXT    DEFAULT '',
		model_identifier         TEXT    DEFAULT '',
		max_turns                INTEGER DEFAULT 0,
		context_size_bytes       INTEGER DEFAULT 0,
		tool_availability_json   TEXT    DEFAULT '[]',
		routing_thresholds_json  TEXT    DEFAULT '{}',
		created_at               TEXT    DEFAULT (datetime('now'))
	)`)

	db.Exec(`CREATE TABLE IF NOT EXISTS ExperimentTreatments (
		id                    INTEGER PRIMARY KEY AUTOINCREMENT,
		experiment_id         INTEGER NOT NULL,
		arm_label             TEXT    NOT NULL,
		cell_json             TEXT    DEFAULT '{}',
		treatment_spec_id     INTEGER NOT NULL,
		target_cell_weight    REAL    DEFAULT 0
	)`)
	db.Exec(`CREATE INDEX IF NOT EXISTS idx_exp_treatments_exp ON ExperimentTreatments (experiment_id)`)

	db.Exec(`CREATE TABLE IF NOT EXISTS ExperimentMetrics (
		id              INTEGER PRIMARY KEY AUTOINCREMENT,
		experiment_id   INTEGER NOT NULL,
		metric_name     TEXT    NOT NULL,
		metric_version  TEXT    NOT NULL,
		direction       TEXT    NOT NULL DEFAULT 'higher_is_better',
		params_json     TEXT    DEFAULT '{}',
		is_primary      INTEGER DEFAULT 0
	)`)
	db.Exec(`CREATE INDEX IF NOT EXISTS idx_exp_metrics_exp ON ExperimentMetrics (experiment_id)`)

	db.Exec(`CREATE TABLE IF NOT EXISTS ExperimentRuns (
		id                       INTEGER PRIMARY KEY AUTOINCREMENT,
		experiment_id            INTEGER NOT NULL,
		treatment_id             INTEGER NOT NULL,
		cell_json                TEXT    DEFAULT '{}',
		natural_unit_kind        TEXT    NOT NULL,
		natural_unit_id          INTEGER NOT NULL,
		mode                     TEXT    NOT NULL DEFAULT 'holdout',
		paired_with_run_id       INTEGER DEFAULT 0,
		agent_name               TEXT    NOT NULL DEFAULT '',
		assigned_at              TEXT    DEFAULT (datetime('now')),
		completed_at             TEXT    DEFAULT '',
		score                    REAL,
		score_source             TEXT    DEFAULT '',
		metric_version           TEXT    DEFAULT '',
		model_substituted_from   TEXT    DEFAULT '',
		model_substituted_to     TEXT    DEFAULT '',
		is_provisional           INTEGER DEFAULT 0
	)`)
	db.Exec(`CREATE INDEX IF NOT EXISTS idx_exp_runs_exp_treat ON ExperimentRuns (experiment_id, treatment_id)`)
	db.Exec(`CREATE INDEX IF NOT EXISTS idx_exp_runs_unit      ON ExperimentRuns (natural_unit_kind, natural_unit_id)`)

	db.Exec(`CREATE TABLE IF NOT EXISTS ExperimentOutcomes (
		id                          INTEGER PRIMARY KEY AUTOINCREMENT,
		experiment_id               INTEGER NOT NULL UNIQUE,
		terminated_at               TEXT    DEFAULT (datetime('now')),
		termination_reason          TEXT    NOT NULL,
		winner_treatment_id         INTEGER DEFAULT 0,
		winner_posterior            REAL,
		winner_effect_estimate      REAL,
		cell_means_json             TEXT    DEFAULT '{}',
		fleet_state_hash_at_start   TEXT    DEFAULT '',
		fleet_state_hash_at_end     TEXT    DEFAULT '',
		confirm_phase_outcome       TEXT    DEFAULT '',
		promotion_proposal_id       INTEGER DEFAULT 0
	)`)

	// ExperimentInteractions — D3 Phase 4 (upgrade-path mirror).
	db.Exec(`CREATE TABLE IF NOT EXISTS ExperimentInteractions (
		id                       INTEGER PRIMARY KEY AUTOINCREMENT,
		experiment_id            INTEGER NOT NULL,
		factor_a                 TEXT    NOT NULL,
		factor_b                 TEXT    NOT NULL,
		level_a                  TEXT    NOT NULL DEFAULT '',
		level_b                  TEXT    NOT NULL DEFAULT '',
		interaction_estimate     REAL    DEFAULT 0,
		posterior_alpha          REAL    DEFAULT 0,
		posterior_beta           REAL    DEFAULT 0,
		posterior_prob_nonzero   REAL    DEFAULT 0,
		computed_at              TEXT    DEFAULT (datetime('now'))
	)`)
	db.Exec(`CREATE INDEX IF NOT EXISTS idx_exp_interactions_exp  ON ExperimentInteractions (experiment_id)`)
	db.Exec(`CREATE INDEX IF NOT EXISTS idx_exp_interactions_pair ON ExperimentInteractions (experiment_id, factor_a, factor_b)`)

	db.Exec(`CREATE TABLE IF NOT EXISTS AnalysisFrameworks (
		version           TEXT PRIMARY KEY,
		config_content    TEXT    NOT NULL,
		config_hash       TEXT    NOT NULL,
		algorithm_git_sha TEXT    DEFAULT '',
		published_at      TEXT    DEFAULT (datetime('now')),
		published_by      TEXT    NOT NULL DEFAULT '',
		description       TEXT    DEFAULT '',
		deprecated_at     TEXT    DEFAULT ''
	)`)

	db.Exec(`CREATE TABLE IF NOT EXISTS MetricVersions (
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
	)`)

	db.Exec(`CREATE TABLE IF NOT EXISTS FleetStateSnapshots (
		state_hash                    TEXT PRIMARY KEY,
		computed_at                   TEXT DEFAULT (datetime('now')),
		active_rules_manifest_json    TEXT DEFAULT '{}',
		active_memories_manifest_json TEXT DEFAULT '{}',
		active_models_manifest_json   TEXT DEFAULT '{}',
		active_prompts_manifest_json  TEXT DEFAULT '{}',
		agent_binary_git_sha          TEXT DEFAULT ''
	)`)

	db.Exec(`CREATE TABLE IF NOT EXISTS GlobalHoldouts (
		id                 INTEGER PRIMARY KEY AUTOINCREMENT,
		name               TEXT UNIQUE NOT NULL,
		reference_date     TEXT DEFAULT (datetime('now')),
		fleet_state_hash   TEXT DEFAULT '',
		ramp_up_days       INTEGER DEFAULT 7,
		plateau_fraction   REAL    DEFAULT 0.02,
		fade_start_at      TEXT    DEFAULT '',
		fade_days          INTEGER DEFAULT 90,
		retired_at         TEXT    DEFAULT '',
		retired_reason     TEXT    DEFAULT '',
		created_by         TEXT    DEFAULT '',
		notes              TEXT    DEFAULT ''
	)`)

	db.Exec(`CREATE TABLE IF NOT EXISTS ModelAvailability (
		model_id                 TEXT PRIMARY KEY,
		last_checked_at          TEXT DEFAULT '',
		last_success_at          TEXT DEFAULT '',
		deprecation_detected_at  TEXT DEFAULT '',
		announced_kill_at        TEXT DEFAULT '',
		successor_suggested      TEXT DEFAULT ''
	)`)

	db.Exec(`CREATE TABLE IF NOT EXISTS TreatmentApplyLog (
		id                  INTEGER PRIMARY KEY AUTOINCREMENT,
		applied_at          TEXT    DEFAULT (datetime('now')),
		agent_name          TEXT    NOT NULL,
		natural_unit_kind   TEXT    DEFAULT '',
		natural_unit_id     INTEGER DEFAULT 0,
		prompt_template     TEXT    DEFAULT '',
		model               TEXT    DEFAULT '',
		in_holdout          INTEGER DEFAULT 0,
		assignments_json    TEXT    DEFAULT '[]',
		mode                TEXT    NOT NULL DEFAULT 'log_only'
	)`)
	db.Exec(`CREATE INDEX IF NOT EXISTS idx_treatment_apply_log_ts ON TreatmentApplyLog (applied_at)`)

	// FleetRules + PromotionProposals (upgrade path).
	db.Exec(`CREATE TABLE IF NOT EXISTS FleetRules (
		id                         INTEGER PRIMARY KEY AUTOINCREMENT,
		rule_key                   TEXT    NOT NULL,
		category                   TEXT    NOT NULL DEFAULT '',
		agent_scope                TEXT    NOT NULL DEFAULT 'all',
		render_to                  TEXT    NOT NULL,
		enforced_by                TEXT    NOT NULL DEFAULT 'trust-only',
		content                    TEXT    NOT NULL,
		content_hash               TEXT    NOT NULL DEFAULT '',
		version                    INTEGER NOT NULL DEFAULT 1,
		active_from                TEXT    DEFAULT (datetime('now')),
		active_until               TEXT    DEFAULT '',
		promoted_by_experiment_id  INTEGER DEFAULT 0,
		created_by                 TEXT    NOT NULL DEFAULT '',
		created_at                 TEXT    DEFAULT (datetime('now'))
	)`)
	db.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS idx_fleet_rules_key_version
		ON FleetRules(rule_key, version)`)
	db.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS idx_fleet_rules_active_key
		ON FleetRules(rule_key) WHERE active_until = ''`)
	db.Exec(`CREATE INDEX IF NOT EXISTS idx_fleet_rules_render_active
		ON FleetRules(render_to, agent_scope) WHERE active_until = ''`)

	db.Exec(`CREATE TABLE IF NOT EXISTS PromotionProposals (
		id                 INTEGER PRIMARY KEY AUTOINCREMENT,
		experiment_id      INTEGER NOT NULL,
		kind               TEXT    NOT NULL DEFAULT 'promote',
		rule_key           TEXT    DEFAULT '',
		proposed_content   TEXT    DEFAULT '',
		evidence_summary_json TEXT DEFAULT '{}',
		authored_by        TEXT    NOT NULL DEFAULT '',
		authored_at        TEXT    DEFAULT (datetime('now')),
		ratified_at        TEXT    DEFAULT '',
		ratified_by        TEXT    DEFAULT '',
		rejected_at        TEXT    DEFAULT '',
		rejected_reason    TEXT    DEFAULT '',
		ttl_expires_at     TEXT    DEFAULT '',
		rejection_action   TEXT    DEFAULT 'leave_as_is',
		rejection_rationale TEXT   DEFAULT '',
		revert_task_id     INTEGER DEFAULT 0,
		refiled_feature_id INTEGER DEFAULT 0
	)`)
	db.Exec(`CREATE INDEX IF NOT EXISTS idx_promotion_proposals_exp   ON PromotionProposals (experiment_id)`)
	db.Exec(`CREATE INDEX IF NOT EXISTS idx_promotion_proposals_state ON PromotionProposals (ratified_at, rejected_at)`)

	// ── D3 Phase 1: column extensions on existing tables (upgrade path) ──────
	// BountyBoard — paired-runs holdout/assignment + Captain proposal +
	// prompt-version + revert handling. Each ALTER no-ops on a fresh DB
	// (column already present from createSchema) and on a re-run.
	db.Exec(`ALTER TABLE BountyBoard ADD COLUMN in_holdout                      INTEGER DEFAULT 0`)
	db.Exec(`ALTER TABLE BountyBoard ADD COLUMN experiment_assignments_json     TEXT    DEFAULT '{}'`)
	db.Exec(`ALTER TABLE BountyBoard ADD COLUMN proposed_action_json            TEXT    DEFAULT ''`)
	db.Exec(`ALTER TABLE BountyBoard ADD COLUMN prompt_version                  TEXT    DEFAULT ''`)
	db.Exec(`ALTER TABLE BountyBoard ADD COLUMN prior_review_outcomes_json      TEXT    DEFAULT '[]'`)
	db.Exec(`ALTER TABLE BountyBoard ADD COLUMN spawn_spec_link                 TEXT    DEFAULT ''`)
	db.Exec(`ALTER TABLE BountyBoard ADD COLUMN spawn_classification_confidence TEXT    DEFAULT ''`)
	db.Exec(`ALTER TABLE BountyBoard ADD COLUMN spawning_at_id                  TEXT    DEFAULT ''`)
	db.Exec(`ALTER TABLE BountyBoard ADD COLUMN deferred_revert                 INTEGER DEFAULT 0`)
	db.Exec(`ALTER TABLE BountyBoard ADD COLUMN revert_target_task_id           INTEGER DEFAULT 0`)

	// Convoys — paired-runs holdout/assignment + verification spec +
	// parent-feature backreference + critical flag.
	db.Exec(`ALTER TABLE Convoys ADD COLUMN in_holdout                  INTEGER DEFAULT 0`)
	db.Exec(`ALTER TABLE Convoys ADD COLUMN experiment_assignments_json TEXT    DEFAULT '{}'`)
	db.Exec(`ALTER TABLE Convoys ADD COLUMN parent_feature_id           INTEGER DEFAULT 0`)
	db.Exec(`ALTER TABLE Convoys ADD COLUMN verification_spec_json      TEXT    DEFAULT ''`)
	db.Exec(`ALTER TABLE Convoys ADD COLUMN spec_history_json           TEXT    DEFAULT '[]'`)
	db.Exec(`ALTER TABLE Convoys ADD COLUMN critical                    INTEGER DEFAULT 0`)

	// TaskHistory — per-prompt-version metric correlation.
	db.Exec(`ALTER TABLE TaskHistory ADD COLUMN prompt_version TEXT DEFAULT ''`)

	// ProposedFeatures + suppressions + score overrides + ConvoyReviewCycles
	// (upgrade path).
	db.Exec(`CREATE TABLE IF NOT EXISTS ProposedFeatures (
		id                   INTEGER PRIMARY KEY AUTOINCREMENT,
		observation_summary  TEXT    NOT NULL,
		category             TEXT    NOT NULL,
		source               TEXT    NOT NULL,
		source_observations  TEXT    DEFAULT '[]',
		fingerprint          TEXT    NOT NULL DEFAULT '',
		occurrence_count     INTEGER DEFAULT 1,
		first_seen_at        TEXT    DEFAULT (datetime('now')),
		last_seen_at         TEXT    DEFAULT (datetime('now')),
		evidence_history_json TEXT   DEFAULT '[]',
		value_score          TEXT    NOT NULL DEFAULT 'medium' CHECK(value_score IN ('low','medium','high')),
		complexity_score     TEXT    NOT NULL DEFAULT 'medium' CHECK(complexity_score IN ('low','medium','high')),
		value_rationale      TEXT    DEFAULT '',
		complexity_rationale TEXT    DEFAULT '',
		scored_by            TEXT    NOT NULL DEFAULT '',
		promoted_at          TEXT    DEFAULT '',
		promotion_deadline   TEXT    DEFAULT '',
		status               TEXT    DEFAULT 'pending',
		decided_at           TEXT    DEFAULT '',
		decided_by           TEXT    DEFAULT '',
		decision_action      TEXT    DEFAULT '',
		archived_at          TEXT    DEFAULT '',
		archive_reason       TEXT    DEFAULT ''
	)`)
	db.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS idx_pf_active_fingerprint
		ON ProposedFeatures(fingerprint)
		WHERE archived_at = '' AND fingerprint != ''`)
	db.Exec(`CREATE INDEX IF NOT EXISTS idx_pf_status ON ProposedFeatures(status, last_seen_at)`)

	db.Exec(`CREATE TABLE IF NOT EXISTS ProposedFeatureSuppressions (
		id                INTEGER PRIMARY KEY AUTOINCREMENT,
		fingerprint       TEXT    NOT NULL,
		rationale         TEXT    NOT NULL CHECK(length(rationale) >= 20),
		suppressed_until  TEXT    NOT NULL,
		created_at        TEXT    DEFAULT (datetime('now')),
		created_by_email  TEXT    NOT NULL
	)`)
	db.Exec(`CREATE INDEX IF NOT EXISTS idx_pfs_fp
		ON ProposedFeatureSuppressions(fingerprint, suppressed_until)`)

	db.Exec(`CREATE TABLE IF NOT EXISTS ProposedFeatureScoreOverrides (
		id                     INTEGER PRIMARY KEY AUTOINCREMENT,
		proposed_feature_id    INTEGER NOT NULL,
		prior_value_score      TEXT    DEFAULT '',
		prior_complexity_score TEXT    DEFAULT '',
		new_value_score        TEXT    DEFAULT '',
		new_complexity_score   TEXT    DEFAULT '',
		rationale              TEXT    NOT NULL,
		overridden_at          TEXT    DEFAULT (datetime('now')),
		overridden_by_email    TEXT    NOT NULL
	)`)
	db.Exec(`CREATE INDEX IF NOT EXISTS idx_pfso_pf
		ON ProposedFeatureScoreOverrides(proposed_feature_id)`)

	db.Exec(`CREATE TABLE IF NOT EXISTS ConvoyReviewCycles (
		id                                    INTEGER PRIMARY KEY AUTOINCREMENT,
		convoy_id                             INTEGER NOT NULL,
		cycle_number                          INTEGER NOT NULL,
		spec_version_at_start                 TEXT    NOT NULL,
		cycle_started_at                      TEXT    DEFAULT (datetime('now')),
		cycle_completed_at                    TEXT    DEFAULT '',
		outcomes_json                         TEXT    DEFAULT '{}',
		fix_tasks_spawned_json                TEXT    DEFAULT '[]',
		amendments_proposed_json              TEXT    DEFAULT '[]',
		amendments_ratified_during_cycle_json TEXT    DEFAULT '[]',
		UNIQUE (convoy_id, cycle_number)
	)`)
	db.Exec(`CREATE INDEX IF NOT EXISTS idx_crc_convoy ON ConvoyReviewCycles(convoy_id, cycle_number)`)

	// AdversarialPairings + GoldenSet* + CalibrationAuditSamples (upgrade path).
	db.Exec(`CREATE TABLE IF NOT EXISTS AdversarialPairings (
		id                       INTEGER PRIMARY KEY AUTOINCREMENT,
		decision_id              INTEGER NOT NULL,
		agent                    TEXT    NOT NULL,
		primary_outcome          TEXT    NOT NULL,
		critic_outcome           TEXT    NOT NULL,
		prompt_version_primary   TEXT    DEFAULT '',
		prompt_version_critic    TEXT    DEFAULT '',
		agreement                INTEGER DEFAULT 0,
		surfaced_at              TEXT    DEFAULT '',
		operator_resolution      TEXT    DEFAULT '',
		created_at               TEXT    DEFAULT (datetime('now'))
	)`)
	// D3 P5 — additive ALTERs for DBs created before prompt_version_*
	// columns landed. ADD COLUMN is silently a no-op when the column
	// already exists, so this is idempotent.
	db.Exec(`ALTER TABLE AdversarialPairings ADD COLUMN prompt_version_primary TEXT DEFAULT ''`)
	db.Exec(`ALTER TABLE AdversarialPairings ADD COLUMN prompt_version_critic  TEXT DEFAULT ''`)
	db.Exec(`CREATE INDEX IF NOT EXISTS idx_adv_pairings_agent ON AdversarialPairings(agent, created_at)`)
	db.Exec(`CREATE INDEX IF NOT EXISTS idx_adv_pairings_disagreements
		ON AdversarialPairings(agent) WHERE agreement = 0`)

	db.Exec(`CREATE TABLE IF NOT EXISTS GoldenSetFixtures (
		id              INTEGER PRIMARY KEY AUTOINCREMENT,
		agent           TEXT    NOT NULL,
		input           TEXT    NOT NULL,
		expected_output TEXT    NOT NULL,
		source          TEXT    NOT NULL,
		curated_at      TEXT    DEFAULT (datetime('now')),
		curated_by      TEXT    DEFAULT '',
		retired_at      TEXT    DEFAULT ''
	)`)
	db.Exec(`CREATE INDEX IF NOT EXISTS idx_gsf_agent ON GoldenSetFixtures(agent) WHERE retired_at = ''`)

	db.Exec(`CREATE TABLE IF NOT EXISTS GoldenSetEvaluations (
		id              INTEGER PRIMARY KEY AUTOINCREMENT,
		agent           TEXT    NOT NULL,
		prompt_version  TEXT    NOT NULL,
		fixture_id      INTEGER NOT NULL,
		actual_output   TEXT    NOT NULL,
		accuracy_score  REAL,
		evaluated_at    TEXT    DEFAULT (datetime('now'))
	)`)
	db.Exec(`CREATE INDEX IF NOT EXISTS idx_gse_agent_version
		ON GoldenSetEvaluations(agent, prompt_version, evaluated_at)`)

	db.Exec(`CREATE TABLE IF NOT EXISTS CalibrationAuditSamples (
		id                  INTEGER PRIMARY KEY AUTOINCREMENT,
		sample_week         TEXT    NOT NULL,
		proposal_id         INTEGER NOT NULL,
		selection_bucket    TEXT    NOT NULL,
		surfaced_at         TEXT    DEFAULT (datetime('now')),
		operator_action     TEXT    DEFAULT '',
		operator_acted_at   TEXT    DEFAULT '',
		operator_rationale  TEXT    DEFAULT ''
	)`)
	db.Exec(`CREATE INDEX IF NOT EXISTS idx_cas_week ON CalibrationAuditSamples(sample_week)`)

	// DisagreementPairs (D3 P3, upgrade path) — per-pair cross-layer
	// disagreement rate aggregates.
	db.Exec(`CREATE TABLE IF NOT EXISTS DisagreementPairs (
		id                 INTEGER PRIMARY KEY AUTOINCREMENT,
		pair_name          TEXT    NOT NULL,
		window_start       TEXT    NOT NULL,
		window_end         TEXT    NOT NULL,
		sample_count       INTEGER NOT NULL,
		disagreement_count INTEGER NOT NULL,
		rate               REAL    NOT NULL,
		computed_at        TEXT    DEFAULT (datetime('now')),
		UNIQUE(pair_name, window_start, window_end)
	)`)
	db.Exec(`CREATE INDEX IF NOT EXISTS idx_disagreement_pair_window
		ON DisagreementPairs(pair_name, window_end DESC)`)

	// Dashboard data-layer tables (upgrade path; D3 Phase 6 prerequisites).
	db.Exec(`CREATE TABLE IF NOT EXISTS DashboardHealthHeartbeats (
		id                 INTEGER PRIMARY KEY AUTOINCREMENT,
		ticked_at          TEXT    NOT NULL DEFAULT (datetime('now')),
		process_pid        INTEGER DEFAULT 0,
		bind_addr          TEXT    DEFAULT '',
		in_flight_requests INTEGER DEFAULT 0
	)`)
	db.Exec(`CREATE INDEX IF NOT EXISTS idx_dh_heartbeats_recent ON DashboardHealthHeartbeats(ticked_at DESC)`)

	db.Exec(`CREATE TABLE IF NOT EXISTS OperatorNotificationBudgets (
		id              INTEGER PRIMARY KEY AUTOINCREMENT,
		operator_email  TEXT    NOT NULL,
		source          TEXT    NOT NULL,
		channel         TEXT    NOT NULL,
		max_per_period  INTEGER NOT NULL,
		period_minutes  INTEGER NOT NULL,
		digest_remainder INTEGER NOT NULL DEFAULT 1,
		UNIQUE(operator_email, source, channel)
	)`)

	db.Exec(`CREATE TABLE IF NOT EXISTS OperatorNotificationDigest (
		id              INTEGER PRIMARY KEY AUTOINCREMENT,
		operator_email  TEXT    NOT NULL,
		source          TEXT    NOT NULL,
		channel         TEXT    NOT NULL,
		digest_for_date TEXT    NOT NULL,
		payload_json    TEXT    NOT NULL,
		flushed_at      TEXT    DEFAULT '',
		UNIQUE(operator_email, source, channel, digest_for_date)
	)`)

	db.Exec(`CREATE TABLE IF NOT EXISTS OperatorSessionState (
		id                        INTEGER PRIMARY KEY AUTOINCREMENT,
		operator_email            TEXT    NOT NULL UNIQUE,
		last_active_at            TEXT    DEFAULT (datetime('now')),
		last_viewed_surface       TEXT    DEFAULT '',
		last_viewed_route         TEXT    DEFAULT '',
		last_focused_decision_id  INTEGER DEFAULT 0,
		partial_review_state_json TEXT    DEFAULT ''
	)`)

	db.Exec(`CREATE TABLE IF NOT EXISTS OperatorTrustDials (
		id              INTEGER PRIMARY KEY AUTOINCREMENT,
		operator_email  TEXT    NOT NULL,
		agent           TEXT    NOT NULL,
		dial_value      INTEGER NOT NULL CHECK(dial_value BETWEEN 0 AND 100),
		set_at          TEXT    DEFAULT (datetime('now')),
		set_by          TEXT    NOT NULL DEFAULT '',
		rationale       TEXT    DEFAULT '',
		UNIQUE(operator_email, agent, set_at)
	)`)

	db.Exec(`CREATE TABLE IF NOT EXISTS NarrativeRenders (
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
	)`)
	db.Exec(`CREATE INDEX IF NOT EXISTS idx_nr_window ON NarrativeRenders(event_window_end DESC)`)

	db.Exec(`CREATE TABLE IF NOT EXISTS BriefingRenders (
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
	)`)
	db.Exec(`CREATE INDEX IF NOT EXISTS idx_br_decision ON BriefingRenders(decision_kind, decision_id, rendered_at DESC)`)

	db.Exec(`CREATE TABLE IF NOT EXISTS CooldownPauses (
		id                  INTEGER PRIMARY KEY AUTOINCREMENT,
		decision_id         INTEGER NOT NULL,
		decision_kind       TEXT    NOT NULL,
		scheduled_action_at TEXT    NOT NULL,
		paused_at           TEXT    DEFAULT '',
		paused_by_email     TEXT    DEFAULT '',
		resumed_at          TEXT    DEFAULT '',
		cancelled_at        TEXT    DEFAULT '',
		executed_at         TEXT    DEFAULT ''
	)`)
	db.Exec(`CREATE INDEX IF NOT EXISTS idx_cp_pending ON CooldownPauses(scheduled_action_at)
		WHERE executed_at = '' AND cancelled_at = ''`)

	db.Exec(`CREATE TABLE IF NOT EXISTS OperatorAttentionTags (
		id              INTEGER PRIMARY KEY AUTOINCREMENT,
		operator_email  TEXT    NOT NULL,
		target_kind     TEXT    NOT NULL,
		target_id       TEXT    NOT NULL,
		attention_level TEXT    NOT NULL CHECK(attention_level IN ('following','normal','muted')),
		set_at          TEXT    DEFAULT (datetime('now')),
		rationale       TEXT    DEFAULT '',
		UNIQUE(operator_email, target_kind, target_id)
	)`)

	db.Exec(`CREATE TABLE IF NOT EXISTS LLMCallTranscripts (
		id                     INTEGER PRIMARY KEY AUTOINCREMENT,
		task_id                INTEGER DEFAULT 0,
		agent                  TEXT    NOT NULL,
		prompt_version         TEXT    NOT NULL DEFAULT '',
		call_started_at        TEXT    NOT NULL,
		call_completed_at      TEXT    DEFAULT '',
		system_prompt          TEXT    NOT NULL,
		user_prompt            TEXT    NOT NULL,
		response_text          TEXT    DEFAULT '',
		tool_calls_json        TEXT    DEFAULT '[]',
		cost_usd               REAL    DEFAULT 0,
		input_tokens           INTEGER DEFAULT 0,
		output_tokens          INTEGER DEFAULT 0,
		cache_read_tokens      INTEGER DEFAULT 0,
		cache_creation_tokens  INTEGER DEFAULT 0,
		archived_at            TEXT    DEFAULT ''
	)`)
	db.Exec(`CREATE INDEX IF NOT EXISTS idx_llmct_task  ON LLMCallTranscripts(task_id, call_started_at)`)
	db.Exec(`CREATE INDEX IF NOT EXISTS idx_llmct_agent ON LLMCallTranscripts(agent, call_started_at)`)

	db.Exec(`CREATE TABLE IF NOT EXISTS GitOperationLog (
		id              INTEGER PRIMARY KEY AUTOINCREMENT,
		task_id         INTEGER DEFAULT 0,
		convoy_id       INTEGER DEFAULT 0,
		repo            TEXT    NOT NULL,
		operation       TEXT    NOT NULL,
		args_json       TEXT    DEFAULT '[]',
		started_at      TEXT    NOT NULL,
		duration_ms     INTEGER DEFAULT 0,
		exit_code       INTEGER DEFAULT 0,
		stdout_excerpt  TEXT    DEFAULT '',
		stderr_excerpt  TEXT    DEFAULT '',
		branch          TEXT    DEFAULT '',
		before_sha      TEXT    DEFAULT '',
		after_sha       TEXT    DEFAULT ''
	)`)
	db.Exec(`CREATE INDEX IF NOT EXISTS idx_gol_convoy ON GitOperationLog(convoy_id, started_at)`)
	db.Exec(`CREATE INDEX IF NOT EXISTS idx_gol_task   ON GitOperationLog(task_id, started_at)`)

	db.Exec(`CREATE TABLE IF NOT EXISTS OperatorEventAnnotations (
		id              INTEGER PRIMARY KEY AUTOINCREMENT,
		operator_email  TEXT    NOT NULL,
		event_kind      TEXT    NOT NULL,
		event_ref       TEXT    NOT NULL,
		note_text       TEXT    NOT NULL,
		flag            TEXT    DEFAULT '',
		noted_at        TEXT    DEFAULT (datetime('now'))
	)`)
	db.Exec(`CREATE INDEX IF NOT EXISTS idx_oea_event ON OperatorEventAnnotations(event_kind, event_ref)`)
	db.Exec(`CREATE INDEX IF NOT EXISTS idx_oea_flag  ON OperatorEventAnnotations(flag, noted_at) WHERE flag != ''`)

	db.Exec(`CREATE TABLE IF NOT EXISTS ReplayResults (
		id                    INTEGER PRIMARY KEY AUTOINCREMENT,
		original_event_id     INTEGER NOT NULL,
		original_event_kind   TEXT    NOT NULL,
		replay_prompt_version TEXT    NOT NULL,
		replay_started_at     TEXT    DEFAULT (datetime('now')),
		replay_response       TEXT    DEFAULT '',
		decision_changed      INTEGER DEFAULT 0,
		cost_usd              REAL    DEFAULT 0,
		triggered_by_email    TEXT    NOT NULL DEFAULT ''
	)`)

	db.Exec(`CREATE TABLE IF NOT EXISTS FleetLearningPanels (
		id                     INTEGER PRIMARY KEY AUTOINCREMENT,
		rendered_at            TEXT    DEFAULT (datetime('now')),
		prose                  TEXT    NOT NULL,
		cost_usd               REAL    DEFAULT 0,
		prompt_version         TEXT    NOT NULL DEFAULT '',
		source_event_refs_json TEXT    DEFAULT '[]'
	)`)

	// ── D3 Phase 4: factorial experiment columns (upgrade path) ──────────────
	// kind/factors_json land on existing Experiments rows so upgraded DBs
	// can author factorial experiments without a full rebuild. SQLite's
	// ALTER TABLE ADD COLUMN cannot retro-fit CHECK constraints; the
	// application-layer validator in internal/experiments enforces the
	// allowed kind set ('single', 'factorial') before insert.
	db.Exec(`ALTER TABLE Experiments ADD COLUMN kind         TEXT    NOT NULL DEFAULT 'single'`)
	db.Exec(`ALTER TABLE Experiments ADD COLUMN factors_json TEXT             DEFAULT '[]'`)
	db.Exec(`CREATE INDEX IF NOT EXISTS idx_experiments_kind_status ON Experiments (kind, status)`)

	// ── D4 Phase 1: SecurityFindings (upgrade path) ──────────────────────────
	// Shared between BoS (Phase 1) and ISB (Phase 2). Idempotent
	// CREATE TABLE IF NOT EXISTS so a fresh DB skips this block (table
	// already created in createSchema) and an upgraded DB lands the new
	// table on the next runMigrations sweep.
	db.Exec(`CREATE TABLE IF NOT EXISTS SecurityFindings (
		id              INTEGER PRIMARY KEY AUTOINCREMENT,
		task_id         INTEGER NOT NULL DEFAULT 0,
		bureau          TEXT    NOT NULL DEFAULT 'BoS',
		rule_id         TEXT    NOT NULL,
		severity        TEXT    NOT NULL DEFAULT 'advise',
		file_path       TEXT    NOT NULL DEFAULT '',
		line_number     INTEGER NOT NULL DEFAULT 0,
		message         TEXT    NOT NULL DEFAULT '',
		commit_sha      TEXT    NOT NULL DEFAULT '',
		disposition     TEXT    NOT NULL DEFAULT '',
		bypass_audit_id TEXT    NOT NULL DEFAULT '',
		bypass_reason   TEXT    NOT NULL DEFAULT '',
		created_at      TEXT    DEFAULT (datetime('now')),
		resolved_at     TEXT    NOT NULL DEFAULT ''
	)`)
	db.Exec(`CREATE INDEX IF NOT EXISTS idx_sec_findings_task     ON SecurityFindings(task_id)`)
	db.Exec(`CREATE INDEX IF NOT EXISTS idx_sec_findings_rule     ON SecurityFindings(rule_id, created_at)`)
	db.Exec(`CREATE INDEX IF NOT EXISTS idx_sec_findings_dashboard ON SecurityFindings(rule_id, severity, disposition)`)

	// ── D4 Phase 3: Senate (upgrade path) ────────────────────────────────────
	// Three tables back the Senator review layer. Idempotent
	// CREATE TABLE IF NOT EXISTS so a fresh DB skips this block (tables
	// already created in createSchema) and an upgraded DB lands the new
	// tables on the next runMigrations sweep. Schema parity is enforced
	// by TestSchemaParity (createSchema, runMigrations, schema/schema.sql
	// all agree).
	db.Exec(`CREATE TABLE IF NOT EXISTS SenateChambers (
		senator_name      TEXT PRIMARY KEY,
		scope             TEXT NOT NULL,
		senate_md_path    TEXT NOT NULL DEFAULT '',
		status            TEXT NOT NULL DEFAULT 'active',
		onboarded_at      TEXT NOT NULL DEFAULT '',
		last_refreshed_at TEXT NOT NULL DEFAULT '',
		retired_at        TEXT NOT NULL DEFAULT '',
		created_at        TEXT NOT NULL DEFAULT (datetime('now'))
	)`)
	db.Exec(`CREATE TABLE IF NOT EXISTS SenateMemory (
		id                 INTEGER PRIMARY KEY AUTOINCREMENT,
		senator            TEXT NOT NULL,
		topic              TEXT NOT NULL DEFAULT '',
		summary            TEXT NOT NULL,
		source             TEXT NOT NULL DEFAULT 'manual',
		weight             REAL NOT NULL DEFAULT 1.0,
		retrieval_count    INTEGER NOT NULL DEFAULT 0,
		last_consulted_at  TEXT NOT NULL DEFAULT '',
		created_at         TEXT NOT NULL DEFAULT (datetime('now'))
	)`)
	db.Exec(`CREATE INDEX IF NOT EXISTS idx_senate_memory_senator ON SenateMemory(senator, weight DESC)`)
	db.Exec(`CREATE TABLE IF NOT EXISTS SenateReview (
		id          INTEGER PRIMARY KEY AUTOINCREMENT,
		feature_id  INTEGER NOT NULL,
		senator     TEXT    NOT NULL,
		position    TEXT    NOT NULL,
		concerns    TEXT    NOT NULL DEFAULT '[]',
		amendments  TEXT    NOT NULL DEFAULT '[]',
		rationale   TEXT    NOT NULL DEFAULT '',
		confidence  REAL    NOT NULL DEFAULT 0,
		created_at  TEXT    NOT NULL DEFAULT (datetime('now'))
	)`)
	db.Exec(`CREATE INDEX IF NOT EXISTS idx_senate_review_feature ON SenateReview(feature_id)`)

	// ── D5 Phase 0 — Repositories.license ────────────────────────────────────
	// SUPPLY-004 (license compatibility) needs the repo's declared license
	// to score whether each new dep is compatible. The column is additive
	// (DEFAULT '') so upgraded DBs land it on the next runMigrations sweep.
	// AddRepo populates it on registration via the SPDX detector; the
	// backfill below stamps existing rows from on-disk LICENSE files (best-
	// effort — silently leaves '' when the local_path is missing or no
	// LICENSE file exists). Idempotent: a re-run scans only rows still at
	// '' so already-detected rows stay sticky.
	db.Exec(`ALTER TABLE Repositories ADD COLUMN license TEXT NOT NULL DEFAULT ''`)
	backfillRepositoryLicenses(db)

	// ── D9 — Archaeologist (upgrade path) ────────────────────────────────────
	// Operator opt-out flag (per-repo). Default 0 (sweeping enabled). The
	// Archaeologist's sweep dog filters Repositories WHERE
	// IFNULL(archaeologist_sweep_disabled, 0) = 0. Idempotent ALTER (silent
	// no-op when the column already exists).
	db.Exec(`ALTER TABLE Repositories ADD COLUMN archaeologist_sweep_disabled INTEGER NOT NULL DEFAULT 0`)

	// ArchaeologistFindings — see createSchema for column-level docs. The
	// CREATE TABLE IF NOT EXISTS keeps this idempotent on both fresh
	// (createSchema already ran) and upgrade DBs. Schema parity is
	// enforced by TestSchemaParity.
	db.Exec(`CREATE TABLE IF NOT EXISTS ArchaeologistFindings (
		id           INTEGER PRIMARY KEY AUTOINCREMENT,
		pattern_id   TEXT    NOT NULL,
		repo_id      INTEGER NOT NULL,
		file_path    TEXT    NOT NULL,
		line_number  INTEGER NOT NULL,
		detail_json  TEXT    NOT NULL DEFAULT '{}',
		detected_at  TEXT    NOT NULL,
		status       TEXT    NOT NULL DEFAULT 'open',
		UNIQUE(pattern_id, repo_id, file_path, line_number)
	)`)
	db.Exec(`CREATE INDEX IF NOT EXISTS idx_arch_findings_pattern ON ArchaeologistFindings(pattern_id, status)`)
}
