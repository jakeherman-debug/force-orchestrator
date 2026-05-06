package store

import (
	"database/sql"
	"fmt"
	"log"
	"time"
)

// DaemonStartLogEntry is one row in the DaemonStartLog table. Each row
// records a single daemon boot. The crash-budget guard reads
// RecentStartCount(...) before the agent spawn loop; if N starts have
// happened in the configured window, the next start is treated as a
// crash-loop and aborts.
type DaemonStartLogEntry struct {
	ID        int64
	TS        string
	BinarySHA string
	GitSHA    string
	PID       int
	Outcome   string
}

// RecordDaemonStart writes one row to DaemonStartLog with outcome='started'.
// Called from cmd/force/fleet_cmds.go::cmdDaemon AFTER the crash-budget
// check passes and BEFORE the agent spawn loop.
func RecordDaemonStart(db *sql.DB, binarySHA, gitSHA string, pid int) error {
	_, err := db.Exec(`INSERT INTO DaemonStartLog (ts, binary_sha, git_sha, pid, outcome)
		VALUES (datetime('now'), ?, ?, ?, 'started')`,
		binarySHA, gitSHA, pid)
	if err != nil {
		return fmt.Errorf("RecordDaemonStart: insert: %w", err)
	}
	return nil
}

// RecordDaemonStartAborted writes one row with outcome='crash_loop_aborted'.
// Used by the crash-budget guard so the operator can see "we tripped" in
// the history. The guard then exits the process.
func RecordDaemonStartAborted(db *sql.DB, binarySHA, gitSHA string, pid int) error {
	_, err := db.Exec(`INSERT INTO DaemonStartLog (ts, binary_sha, git_sha, pid, outcome)
		VALUES (datetime('now'), ?, ?, ?, 'crash_loop_aborted')`,
		binarySHA, gitSHA, pid)
	if err != nil {
		return fmt.Errorf("RecordDaemonStartAborted: insert: %w", err)
	}
	return nil
}

// RecentStartCount returns the count of DaemonStartLog rows with
// ts >= (now - since). Used by the crash-budget guard. Rows with
// outcome='crash_loop_aborted' are NOT counted — they don't indicate
// a successful boot, just a recorded refusal.
func RecentStartCount(db *sql.DB, since time.Duration) (int, error) {
	var n int
	err := db.QueryRow(`SELECT COUNT(*) FROM DaemonStartLog
		WHERE outcome = 'started'
		  AND ts >= datetime('now', ?)`,
		fmt.Sprintf("-%d seconds", int(since.Seconds())),
	).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("RecentStartCount: query: %w", err)
	}
	return n, nil
}

// ListDaemonStarts returns the most recent N rows (newest first).
// limit <= 0 maps to 50.
func ListDaemonStarts(db *sql.DB, limit int) ([]DaemonStartLogEntry, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := db.Query(`SELECT
			id, ts, binary_sha, IFNULL(git_sha, ''), pid, IFNULL(outcome, 'started')
		FROM DaemonStartLog ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("ListDaemonStarts: query: %w", err)
	}
	defer rows.Close()
	var out []DaemonStartLogEntry
	for rows.Next() {
		var e DaemonStartLogEntry
		if scanErr := rows.Scan(&e.ID, &e.TS, &e.BinarySHA, &e.GitSHA, &e.PID, &e.Outcome); scanErr != nil {
			log.Printf("ListDaemonStarts: scan: %v", scanErr)
			continue
		}
		out = append(out, e)
	}
	if rerr := rows.Err(); rerr != nil {
		return out, fmt.Errorf("ListDaemonStarts: rows: %w", rerr)
	}
	return out, nil
}

// ClearDaemonStartLog truncates the DaemonStartLog table. Used by
// `force daemon clear-crash-budget` after the operator has investigated
// and fixed the underlying issue. Returns the count of rows deleted.
func ClearDaemonStartLog(db *sql.DB) (int, error) {
	res, err := db.Exec(`DELETE FROM DaemonStartLog`)
	if err != nil {
		return 0, fmt.Errorf("ClearDaemonStartLog: delete: %w", err)
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}
