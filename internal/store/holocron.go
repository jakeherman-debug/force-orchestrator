package store

import (
	"database/sql"
	"log"

	_ "github.com/mattn/go-sqlite3"
)

func InitHolocron() *sql.DB {
	return InitHolocronDSN("./holocron.db?_busy_timeout=5000&_journal_mode=WAL")
}

// InitHolocronDSN opens (or creates) a Holocron database at the given DSN.
// Pass "file::memory:?cache=shared" for an in-memory database in tests.
func InitHolocronDSN(dsn string) *sql.DB {
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		log.Fatal("Failed to open Holocron:", err)
	}
	// Serialize writes through a single connection — prevents SQLITE_BUSY races
	// under concurrent agent goroutines. Reads still fan out through idle conns.
	db.SetMaxOpenConns(1)
	db.Exec("PRAGMA journal_mode=WAL;")

	createSchema(db)
	runMigrations(db)

	return db
}

// ── Repositories ──────────────────────────────────────────────────────────────

// AddRepo inserts or replaces a repo registration. PR-flow fields are initialised
// to their schema defaults — Layer B backfill populates remote_url/default_branch
// at daemon startup, and the FindPRTemplate task populates pr_template_path.
// Preserves any existing PR-flow fields on an update rather than clobbering them.
func AddRepo(db *sql.DB, name, path, desc string) {
	// Preserve existing PR-flow fields when re-adding a repo (e.g. when the operator
	// re-registers to change local_path or description).
	var (
		remoteURL, defaultBranch, templatePath, quarantinedAt, quarantineReason string
		prFlowEnabled                                                           int
	)
	row := db.QueryRow(`SELECT
		IFNULL(remote_url, ''), IFNULL(default_branch, ''), IFNULL(pr_template_path, ''),
		IFNULL(pr_flow_enabled, 1), IFNULL(quarantined_at, ''), IFNULL(quarantine_reason, '')
		FROM Repositories WHERE name = ?`, name)
	existed := row.Scan(&remoteURL, &defaultBranch, &templatePath, &prFlowEnabled, &quarantinedAt, &quarantineReason) == nil
	if !existed {
		prFlowEnabled = 1
	}

	db.Exec(`INSERT OR REPLACE INTO Repositories
		(name, local_path, description, remote_url, default_branch, pr_template_path, pr_flow_enabled, quarantined_at, quarantine_reason)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		name, path, desc, remoteURL, defaultBranch, templatePath, prFlowEnabled, quarantinedAt, quarantineReason)
}

func GetRepoPath(db *sql.DB, repoName string) string {
	var path string
	db.QueryRow(`SELECT local_path FROM Repositories WHERE name = ?`, repoName).Scan(&path)
	return path
}

// GetRepo returns the full Repository row, or nil if not found. Used by PR-flow
// code paths that need remote_url, default_branch, pr_template_path, and the
// pr_flow_enabled flag.
func GetRepo(db *sql.DB, name string) *Repository {
	var (
		r             Repository
		prFlowEnabled int
	)
	err := db.QueryRow(`SELECT
		name, IFNULL(local_path, ''), IFNULL(description, ''),
		IFNULL(remote_url, ''), IFNULL(default_branch, ''), IFNULL(pr_template_path, ''),
		IFNULL(pr_flow_enabled, 1), IFNULL(quarantined_at, ''), IFNULL(quarantine_reason, '')
		FROM Repositories WHERE name = ?`, name).
		Scan(&r.Name, &r.LocalPath, &r.Description,
			&r.RemoteURL, &r.DefaultBranch, &r.PRTemplatePath,
			&prFlowEnabled, &r.QuarantinedAt, &r.QuarantineReason)
	if err != nil {
		return nil
	}
	r.PRFlowEnabled = prFlowEnabled == 1
	return &r
}

// ListRepos returns every registered repository with its full PR-flow configuration,
// ordered by name.
func ListRepos(db *sql.DB) []Repository {
	rows, err := db.Query(`SELECT
		name, IFNULL(local_path, ''), IFNULL(description, ''),
		IFNULL(remote_url, ''), IFNULL(default_branch, ''), IFNULL(pr_template_path, ''),
		IFNULL(pr_flow_enabled, 1), IFNULL(quarantined_at, ''), IFNULL(quarantine_reason, '')
		FROM Repositories ORDER BY name`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var repos []Repository
	for rows.Next() {
		var (
			r             Repository
			prFlowEnabled int
		)
		if err := rows.Scan(&r.Name, &r.LocalPath, &r.Description,
			&r.RemoteURL, &r.DefaultBranch, &r.PRTemplatePath,
			&prFlowEnabled, &r.QuarantinedAt, &r.QuarantineReason); err != nil {
			log.Printf("ListRepos: scan error: %v", err)
			continue
		}
		r.PRFlowEnabled = prFlowEnabled == 1
		repos = append(repos, r)
	}
	return repos
}

// SetRepoRemoteInfo records the origin URL and default branch discovered by
// Layer B backfill at daemon startup. Both fields must be derivable from `git`;
// callers that see an error should mark the repo pr_flow_enabled=0 instead of
// storing empty strings here.
func SetRepoRemoteInfo(db *sql.DB, name, remoteURL, defaultBranch string) error {
	_, err := db.Exec(`UPDATE Repositories SET remote_url = ?, default_branch = ? WHERE name = ?`,
		remoteURL, defaultBranch, name)
	return err
}

// SetRepoPRTemplatePath records the PR template path discovered by the FindPRTemplate
// task. Accepts empty string to indicate "repo has no template" (Diplomat will fall
// back to a structured body).
func SetRepoPRTemplatePath(db *sql.DB, name, templatePath string) error {
	_, err := db.Exec(`UPDATE Repositories SET pr_template_path = ? WHERE name = ?`, templatePath, name)
	return err
}

// SetRepoPRFlowEnabled flips the pr_flow_enabled flag. Disabling sends new tasks
// through the legacy local-merge path; in-flight ask-branches and sub-PRs are
// unaffected and finish out through the new path.
func SetRepoPRFlowEnabled(db *sql.DB, name string, enabled bool) error {
	v := 0
	if enabled {
		v = 1
	}
	_, err := db.Exec(`UPDATE Repositories SET pr_flow_enabled = ? WHERE name = ?`, v, name)
	return err
}

// QuarantineRepo marks a repo as unhealthy, disabling the PR flow for it until
// a successful RevalidateRepoConfig clears the quarantine.
func QuarantineRepo(db *sql.DB, name, reason string) error {
	_, err := db.Exec(`UPDATE Repositories SET quarantined_at = datetime('now'), quarantine_reason = ?, pr_flow_enabled = 0 WHERE name = ?`,
		reason, name)
	return err
}

// UnquarantineRepo clears quarantine state. Caller is responsible for deciding
// whether to re-enable pr_flow_enabled — recovery may want to run a validation
// cycle first.
func UnquarantineRepo(db *sql.DB, name string) error {
	_, err := db.Exec(`UPDATE Repositories SET quarantined_at = '', quarantine_reason = '' WHERE name = ?`, name)
	return err
}

// RemoveRepo removes a repository registration. Returns true if it existed.
func RemoveRepo(db *sql.DB, name string) bool {
	res, _ := db.Exec(`DELETE FROM Repositories WHERE name = ?`, name)
	n, _ := res.RowsAffected()
	return n > 0
}

// ReleaseInFlightTasks resets all in-progress tasks back to Pending.
// Used on daemon shutdown to prevent orphaned locks.
// Returns the number of tasks released.
func ReleaseInFlightTasks(db *sql.DB, reason string) int {
	res, _ := db.Exec(`UPDATE BountyBoard SET status = 'Pending', owner = '', locked_at = '', error_log = ?
		WHERE status IN ('Locked', 'UnderCaptainReview', 'UnderReview')`, reason)
	n, _ := res.RowsAffected()
	return int(n)
}

// ── Audit log ─────────────────────────────────────────────────────────────────

// LogAudit records a destructive or approval operator/agent action.
func LogAudit(db *sql.DB, actor, action string, taskID int, detail string) {
	db.Exec(`INSERT INTO AuditLog (actor, action, task_id, detail) VALUES (?, ?, ?, ?)`,
		actor, action, taskID, detail)
}

// ListAuditLog returns the most recent N audit entries (newest first).
func ListAuditLog(db *sql.DB, limit int) []AuditEntry {
	if limit <= 0 {
		limit = 50
	}
	rows, err := db.Query(
		`SELECT id, actor, action, task_id, detail, created_at FROM AuditLog ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var entries []AuditEntry
	for rows.Next() {
		var e AuditEntry
		rows.Scan(&e.ID, &e.Actor, &e.Action, &e.TaskID, &e.Detail, &e.CreatedAt)
		entries = append(entries, e)
	}
	return entries
}

// ── Dog agent state ───────────────────────────────────────────────────────────

// DogLastRun returns the last-run timestamp for a dog (empty string if never run).
func DogLastRun(db *sql.DB, name string) string {
	var t string
	db.QueryRow(`SELECT last_run_at FROM Dogs WHERE name = ?`, name).Scan(&t)
	return t
}

// DogMarkRun records that a dog just ran.
func DogMarkRun(db *sql.DB, name string) {
	db.Exec(`INSERT INTO Dogs (name, last_run_at, run_count)
		VALUES (?, datetime('now'), 1)
		ON CONFLICT(name) DO UPDATE SET last_run_at = datetime('now'), run_count = run_count + 1`, name)
}

// ── SystemConfig ──────────────────────────────────────────────────────────────

func GetConfig(db *sql.DB, key, defaultVal string) string {
	var val string
	if err := db.QueryRow(`SELECT value FROM SystemConfig WHERE key = ?`, key).Scan(&val); err != nil {
		return defaultVal
	}
	return val
}

func SetConfig(db *sql.DB, key, value string) {
	db.Exec(`INSERT OR REPLACE INTO SystemConfig (key, value) VALUES (?, ?)`, key, value)
}
