package store

import (
	"database/sql"
	"log"
)

// ── Fleet mail ────────────────────────────────────────────────────────────────

func SendMail(db *sql.DB, from, to, subject, body string, taskID int, msgType MailType) int64 {
	// Scrub secrets in subject and body before they land in Fleet_Mail.
	// Operator mail is rendered on the dashboard and in FleetMail.body
	// dumps, so redaction closes the AUDIT-055/056 exfil path at the
	// store boundary (Fix #10).
	res, err := db.Exec(
		`INSERT INTO Fleet_Mail (from_agent, to_agent, subject, body, task_id, message_type) VALUES (?, ?, ?, ?, ?, ?)`,
		from, to, RedactSecrets(subject), RedactSecrets(body), taskID, string(msgType),
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
		if err := sqlRows.Scan(&m.ID, &m.FromAgent, &m.ToAgent, &m.Subject, &m.Body, &m.TaskID, &mt, &m.ReadAt, &m.ConsumedAt, &m.CreatedAt); err != nil {
			log.Printf("ListMail: scan failed: %v", err)
			continue
		}
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
//
// Fix #3 (AUDIT-074): the claim is a single-statement
//
//	UPDATE Fleet_Mail SET consumed_at = datetime('now')
//	WHERE id IN (SELECT id ... WHERE consumed_at = '' ...)
//	RETURNING ...
//
// so two agents whose role/name/task_id scopes overlap cannot both observe
// the same unconsumed row between the SELECT and the per-id UPDATE. The
// previous shape (SELECT then a per-row MarkMailConsumed loop) released the
// single connection between statements, giving the second reader a full
// window to re-SELECT the same rows and double-process their payloads.
func ReadInboxForAgent(db *sql.DB, agentName, role string, taskID int) []FleetMail {
	// Order within the subquery so consumers observe mail in creation order,
	// matching the prior contract. The outer UPDATE ... RETURNING emits rows in
	// UPDATE order (SQLite doesn't guarantee stable order post-UPDATE without
	// an explicit ORDER BY), so we sort client-side after the fact.
	rows, err := db.Query(`
		UPDATE Fleet_Mail
		SET consumed_at = datetime('now')
		WHERE id IN (
			SELECT id FROM Fleet_Mail
			WHERE consumed_at = ''
			  AND (to_agent = ? OR to_agent = ? OR to_agent = 'all')
			  AND (task_id = 0 OR task_id = ?)
		)
		RETURNING id, from_agent, to_agent, subject, body, task_id, message_type,
		          read_at, consumed_at, created_at`,
		agentName, role, taskID)
	if err != nil {
		return nil
	}
	defer rows.Close()

	var mails []FleetMail
	for rows.Next() {
		var m FleetMail
		var mt string
		if err := rows.Scan(&m.ID, &m.FromAgent, &m.ToAgent, &m.Subject, &m.Body, &m.TaskID, &mt, &m.ReadAt, &m.ConsumedAt, &m.CreatedAt); err != nil {
			log.Printf("ReadInboxForAgent: scan failed: %v", err)
			continue
		}
		m.MessageType = MailType(mt)
		mails = append(mails, m)
	}
	// Preserve the pre-fix creation-order contract.
	sortMailsByCreatedAscID(mails)
	return mails
}

// sortMailsByCreatedAscID sorts mails in place by (created_at ASC, id ASC).
// created_at ties are broken by id so re-runs against the same DB observe
// the same order.
func sortMailsByCreatedAscID(mails []FleetMail) {
	// In-place insertion sort — the inbox is almost always very small
	// (single-digit rows per agent tick), so this is cheaper than the
	// allocation from sort.Slice.
	for i := 1; i < len(mails); i++ {
		j := i
		for j > 0 && (mails[j-1].CreatedAt > mails[j].CreatedAt ||
			(mails[j-1].CreatedAt == mails[j].CreatedAt && mails[j-1].ID > mails[j].ID)) {
			mails[j-1], mails[j] = mails[j], mails[j-1]
			j--
		}
	}
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
