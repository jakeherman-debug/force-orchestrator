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

func AddRepo(db *sql.DB, name, path, desc string) {
	db.Exec(`INSERT OR REPLACE INTO Repositories (name, local_path, description) VALUES (?, ?, ?)`, name, path, desc)
}

func GetRepoPath(db *sql.DB, repoName string) string {
	var path string
	db.QueryRow(`SELECT local_path FROM Repositories WHERE name = ?`, repoName).Scan(&path)
	return path
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
