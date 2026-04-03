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
			`SELECT id, from_agent, to_agent, subject, body, task_id, message_type, read_at, created_at
			 FROM Fleet_Mail WHERE to_agent = ? ORDER BY created_at DESC`, toAgent)
	} else {
		sqlRows, err = db.Query(
			`SELECT id, from_agent, to_agent, subject, body, task_id, message_type, read_at, created_at
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
		sqlRows.Scan(&m.ID, &m.FromAgent, &m.ToAgent, &m.Subject, &m.Body, &m.TaskID, &mt, &m.ReadAt, &m.CreatedAt)
		m.MessageType = MailType(mt)
		mails = append(mails, m)
	}
	return mails
}

func GetMail(db *sql.DB, id int) *FleetMail {
	var m FleetMail
	var mt string
	err := db.QueryRow(
		`SELECT id, from_agent, to_agent, subject, body, task_id, message_type, read_at, created_at
		 FROM Fleet_Mail WHERE id = ?`, id).
		Scan(&m.ID, &m.FromAgent, &m.ToAgent, &m.Subject, &m.Body, &m.TaskID, &mt, &m.ReadAt, &m.CreatedAt)
	if err != nil {
		return nil
	}
	m.MessageType = MailType(mt)
	return &m
}

// ReadInboxForAgent fetches all unread mail for an agent based on role addressing.
// Matches: to_agent = agentName, OR to_agent = role (e.g. "astromech"), OR to_agent = "all"
// Scoped to: task_id = 0 (standing) OR task_id = taskID (task-specific).
// Marks all returned messages as read.
func ReadInboxForAgent(db *sql.DB, agentName, role string, taskID int) []FleetMail {
	rows, err := db.Query(`
		SELECT id, from_agent, to_agent, subject, body, task_id, message_type, read_at, created_at
		FROM Fleet_Mail
		WHERE read_at = ''
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
		rows.Scan(&m.ID, &m.FromAgent, &m.ToAgent, &m.Subject, &m.Body, &m.TaskID, &mt, &m.ReadAt, &m.CreatedAt)
		m.MessageType = MailType(mt)
		mails = append(mails, m)
	}
	rows.Close() // explicit close required before mark-read writes on single-connection pool

	// Mark all as read now that the agent has consumed them
	for _, m := range mails {
		MarkMailRead(db, m.ID)
	}
	return mails
}

func MarkMailRead(db *sql.DB, id int) {
	db.Exec(`UPDATE Fleet_Mail SET read_at = datetime('now') WHERE id = ? AND read_at = ''`, id)
}

// MailStats returns (unread, total) counts for a given recipient (or fleet-wide if toAgent="").
func MailStats(db *sql.DB, toAgent string) (unread, total int) {
	if toAgent != "" {
		db.QueryRow(`SELECT COUNT(*) FROM Fleet_Mail WHERE to_agent = ?`, toAgent).Scan(&total)
		db.QueryRow(`SELECT COUNT(*) FROM Fleet_Mail WHERE to_agent = ? AND read_at = ''`, toAgent).Scan(&unread)
	} else {
		db.QueryRow(`SELECT COUNT(*) FROM Fleet_Mail`).Scan(&total)
		db.QueryRow(`SELECT COUNT(*) FROM Fleet_Mail WHERE read_at = ''`).Scan(&unread)
	}
	return
}
