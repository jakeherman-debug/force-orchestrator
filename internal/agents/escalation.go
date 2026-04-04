package agents

import (
	"database/sql"
	"fmt"
	"regexp"
	"strings"
	"time"

	"force-orchestrator/internal/store"
)

// Escalation signal format emitted by Astromechs in their claude output:
//
//	[ESCALATED:LOW:I cannot determine the correct API endpoint without more context]
//	[ESCALATED:MEDIUM:File conflicts exist that require human decision on merge strategy]
//	[ESCALATED:HIGH:Credentials are missing and task cannot proceed]
var escalationSignal = regexp.MustCompile(`\[ESCALATED:(LOW|MEDIUM|HIGH):([^\]]+)\]`)

// ParseEscalationSignal checks Claude output for an escalation signal.
// Returns severity, message, and whether a signal was found.
func ParseEscalationSignal(output string) (store.EscalationSeverity, string, bool) {
	matches := escalationSignal.FindStringSubmatch(output)
	if matches == nil {
		return "", "", false
	}
	return store.EscalationSeverity(matches[1]), strings.TrimSpace(matches[2]), true
}

// CreateEscalation records a new escalation for a task.
func CreateEscalation(db *sql.DB, taskID int, severity store.EscalationSeverity, message string) int {
	res, _ := db.Exec(
		`INSERT INTO Escalations (task_id, severity, message, status) VALUES (?, ?, ?, 'Open')`,
		taskID, string(severity), message,
	)
	id, _ := res.LastInsertId()

	// Mark the task as Escalated so it doesn't get retried automatically
	db.Exec(`UPDATE BountyBoard SET status = 'Escalated', owner = '', locked_at = '' WHERE id = ?`, taskID)

	return int(id)
}

// ListEscalations returns escalations filtered by status ("Open", "Acknowledged", "Closed", or "" for all).
func ListEscalations(db *sql.DB, status string) []store.Escalation {
	query := `SELECT id, task_id, severity, message, status, created_at, acknowledged_at FROM Escalations`
	args := []any{}
	if status != "" {
		query += ` WHERE status = ?`
		args = append(args, status)
	}
	query += ` ORDER BY created_at DESC`

	rows, err := db.Query(query, args...)
	if err != nil {
		return nil
	}
	defer rows.Close()

	var escalations []store.Escalation
	for rows.Next() {
		var e store.Escalation
		rows.Scan(&e.ID, &e.TaskID, &e.Severity, &e.Message, &e.Status, &e.CreatedAt, &e.AcknowledgedAt)
		escalations = append(escalations, e)
	}
	return escalations
}

// AckEscalation marks an escalation as acknowledged by the operator.
func AckEscalation(db *sql.DB, id int) {
	db.Exec(`UPDATE Escalations SET status = 'Acknowledged', acknowledged_at = datetime('now') WHERE id = ?`, id)
}

// CloseEscalation closes an escalation and optionally requeues the task.
func CloseEscalation(db *sql.DB, id int, requeue bool) {
	db.Exec(`UPDATE Escalations SET status = 'Closed' WHERE id = ?`, id)
	if requeue {
		var taskID int
		db.QueryRow(`SELECT task_id FROM Escalations WHERE id = ?`, id).Scan(&taskID)
		if taskID > 0 {
			store.ResetTask(db, taskID)
		}
	}
}

// CheckStaleEscalations re-escalates any Open escalations that haven't been
// acknowledged within the stale threshold. Called by the Inquisitor.
func CheckStaleEscalations(db *sql.DB) {
	const staleThreshold = 4 * time.Hour

	rows, err := db.Query(`
		SELECT id, task_id, severity, message
		FROM Escalations
		WHERE status = 'Open'
		  AND created_at < datetime('now', ?)`, fmt.Sprintf("-%d seconds", int(staleThreshold.Seconds())))
	if err != nil {
		return
	}

	// Drain the cursor before doing per-escalation writes — avoids deadlock on
	// the single-connection SQLite pool (MaxOpenConns=1).
	type staleEsc struct {
		id, taskID int
		sev        store.EscalationSeverity
		msg        string
	}
	var stale []staleEsc
	for rows.Next() {
		var e staleEsc
		rows.Scan(&e.id, &e.taskID, &e.sev, &e.msg)
		stale = append(stale, e)
	}
	rows.Close()

	for _, e := range stale {
		// Bump severity: LOW→MEDIUM→HIGH
		newSev := bumpSeverity(e.sev)
		// Reset created_at to now so this escalation won't fire again for another staleThreshold.
		// Without this, every 5-minute inquisitor cycle would send a new re-escalation mail.
		db.Exec(`UPDATE Escalations SET severity = ?, message = ?, created_at = datetime('now') WHERE id = ?`,
			string(newSev),
			fmt.Sprintf("[RE-ESCALATED after %v unacknowledged] %s", staleThreshold, e.msg),
			e.id,
		)
		store.SendMail(db, "inquisitor", "operator",
			fmt.Sprintf("[RE-ESCALATED:%s] Task #%d unacknowledged %v", string(newSev), e.taskID, staleThreshold),
			fmt.Sprintf("Escalation for task #%d has not been acknowledged in %v — severity raised to %s.\n\nOriginal message: %s\n\nTo acknowledge: force escalations ack %d\nTo requeue the task: force escalations requeue %d",
				e.taskID, staleThreshold, string(newSev), e.msg, e.id, e.id),
			e.taskID, store.MailTypeAlert)
	}
}

func bumpSeverity(s store.EscalationSeverity) store.EscalationSeverity {
	switch s {
	case store.SeverityLow:
		return store.SeverityMedium
	case store.SeverityMedium:
		return store.SeverityHigh
	default:
		return store.SeverityHigh
	}
}
