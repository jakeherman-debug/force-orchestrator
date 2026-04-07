package store

import (
	"database/sql"
)

// ── Fleet mail ────────────────────────────────────────────────────────────────

func SendMail(db *sql.DB, from, to, subject, body string, taskID int, msgType MailType) int64 {
	res, err := db.Exec(
		`INSERT INTO Fleet_Mail (from_agent, to_agent, subject, body, task_id, message_type) VALUES (?, ?, ?, ?, ?, ?)`,
		from, to, subject, body, taskID, string(msgType),
	)
	if err != nil {
		return 0
	}
	id, _ := res.LastInsertId()
	return id
}

func ListMail(db *sql.DB, toAgent string) []FleetMail {
	var (
		sqlRows *sql.Rows
		err     error
	)
	if toAgent != "" {
		sqlRows, err = db.Query(
			`SELECT id, from_agent, to_agent, subject, body, task_id, message_type, read_at, consumed_at, created_at
			 FROM Fleet_Mail WHERE to_agent = ? ORDER BY created_at DESC`, toAgent)
	} else {
		sqlRows, err = db.Query(
			`SELECT id, from_agent, to_agent, subject, body, task_id, message_type, read_at, consumed_at, created_at
			 FROM Fleet_Mail ORDER BY created_at DESC`)
	}
	if err != nil {
		return nil
	}
	defer sqlRows.Close()
	var mails []FleetMail
	for sqlRows.Next() {
		var m FleetMail
		var mt string
		sqlRows.Scan(&m.ID, &m.FromAgent, &m.ToAgent, &m.Subject, &m.Body, &m.TaskID, &mt, &m.ReadAt, &m.ConsumedAt, &m.CreatedAt)
		m.MessageType = MailType(mt)
		mails = append(mails, m)
	}
	return mails
}

func GetMail(db *sql.DB, id int) *FleetMail {
	var m FleetMail
	var mt string
	err := db.QueryRow(
		`SELECT id, from_agent, to_agent, subject, body, task_id, message_type, read_at, consumed_at, created_at
		 FROM Fleet_Mail WHERE id = ?`, id).
		Scan(&m.ID, &m.FromAgent, &m.ToAgent, &m.Subject, &m.Body, &m.TaskID, &mt, &m.ReadAt, &m.ConsumedAt, &m.CreatedAt)
	if err != nil {
		return nil
	}
	m.MessageType = MailType(mt)
	return &m
}

// ReadInboxForAgent fetches all unconsumed mail for an agent based on role addressing.
// Matches: to_agent = agentName, OR to_agent = role (e.g. "astromech"), OR to_agent = "all"
// Scoped to: task_id = 0 (standing) OR task_id = taskID (task-specific).
// Marks all returned messages as consumed. Does NOT touch read_at (operator display flag).
func ReadInboxForAgent(db *sql.DB, agentName, role string, taskID int) []FleetMail {
	rows, err := db.Query(`
		SELECT id, from_agent, to_agent, subject, body, task_id, message_type, read_at, consumed_at, created_at
		FROM Fleet_Mail
		WHERE consumed_at = ''
		  AND (to_agent = ? OR to_agent = ? OR to_agent = 'all')
		  AND (task_id = 0 OR task_id = ?)
		ORDER BY created_at ASC`,
		agentName, role, taskID)
	if err != nil {
		return nil
	}

	var mails []FleetMail
	for rows.Next() {
		var m FleetMail
		var mt string
		rows.Scan(&m.ID, &m.FromAgent, &m.ToAgent, &m.Subject, &m.Body, &m.TaskID, &mt, &m.ReadAt, &m.ConsumedAt, &m.CreatedAt)
		m.MessageType = MailType(mt)
		mails = append(mails, m)
	}
	rows.Close() // explicit close required before consume writes on single-connection pool

	// Mark all as consumed now that the agent has processed them.
	for _, m := range mails {
		MarkMailConsumed(db, m.ID)
	}
	return mails
}

// MarkMailRead marks a message as read by a human operator (UI display flag only).
// Does not affect agent consumption — use MarkMailConsumed for that.
func MarkMailRead(db *sql.DB, id int) {
	db.Exec(`UPDATE Fleet_Mail SET read_at = datetime('now') WHERE id = ? AND read_at = ''`, id)
}

// MarkMailConsumed marks a message as consumed by an agent.
// This is separate from read_at so operators reading mail in the dashboard
// cannot silently drop messages before the target agent processes them.
func MarkMailConsumed(db *sql.DB, id int) {
	db.Exec(`UPDATE Fleet_Mail SET consumed_at = datetime('now') WHERE id = ? AND consumed_at = ''`, id)
}

// MailStats returns (unread, total) counts for a given recipient (or fleet-wide if agentName="").
// When agentName is set, uses the same addressing logic as ReadInboxForAgent:
// matches to_agent = agentName, OR to_agent = role, OR to_agent = 'all'.
func MailStats(db *sql.DB, agentName, role string) (unread, total int) {
	if agentName != "" {
		db.QueryRow(
			`SELECT COUNT(*) FROM Fleet_Mail WHERE to_agent = ? OR to_agent = ? OR to_agent = 'all'`,
			agentName, role).Scan(&total)
		db.QueryRow(
			`SELECT COUNT(*) FROM Fleet_Mail WHERE (to_agent = ? OR to_agent = ? OR to_agent = 'all') AND read_at = ''`,
			agentName, role).Scan(&unread)
	} else {
		db.QueryRow(`SELECT COUNT(*) FROM Fleet_Mail`).Scan(&total)
		db.QueryRow(`SELECT COUNT(*) FROM Fleet_Mail WHERE read_at = ''`).Scan(&unread)
	}
	return
}
