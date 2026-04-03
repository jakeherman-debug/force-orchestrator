package agents

import (
	"database/sql"
	"fmt"
	"strings"

	"force-orchestrator/internal/store"
)

// buildInboxContext reads unread mail for an agent+role+taskID, marks it read,
// and returns formatted prompt sections grouped by message type.
// Returns empty string if there is no relevant mail.
func buildInboxContext(db *sql.DB, agentName, role string, taskID int, logger interface{ Printf(string, ...any) }) string {
	mails := store.ReadInboxForAgent(db, agentName, role, taskID)
	if len(mails) == 0 {
		return ""
	}

	buckets := map[store.MailType][]store.FleetMail{}
	for _, m := range mails {
		buckets[m.MessageType] = append(buckets[m.MessageType], m)
	}

	var sections []string

	// Directives first — highest priority
	if msgs, ok := buckets[store.MailTypeDirective]; ok {
		var lines []string
		for _, m := range msgs {
			lines = append(lines, fmt.Sprintf("- [%s] %s", m.Subject, m.Body))
		}
		sections = append(sections, "# STANDING ORDERS\nThe following instructions are in effect for this task:\n"+strings.Join(lines, "\n"))
	}

	// Prior rejection feedback — from Captain (plan coherence) or Council (code quality)
	if msgs, ok := buckets[store.MailTypeFeedback]; ok {
		var lines []string
		for _, m := range msgs {
			lines = append(lines, fmt.Sprintf("- [%s] %s", m.Subject, m.Body))
		}
		sections = append(sections, "# PRIOR FEEDBACK\nPrevious work on this task was rejected for the following reasons:\n"+strings.Join(lines, "\n"))
	}

	// Alerts — shown as prominent warnings
	if msgs, ok := buckets[store.MailTypeAlert]; ok {
		var lines []string
		for _, m := range msgs {
			lines = append(lines, fmt.Sprintf("- [%s] %s", m.Subject, m.Body))
		}
		sections = append(sections, "# ALERTS\n"+strings.Join(lines, "\n"))
	}

	// Info — general context, lower prominence
	if msgs, ok := buckets[store.MailTypeInfo]; ok {
		var lines []string
		for _, m := range msgs {
			lines = append(lines, fmt.Sprintf("- [%s] %s", m.Subject, m.Body))
		}
		sections = append(sections, "# FLEET MESSAGES\n"+strings.Join(lines, "\n"))
	}

	// Remediation — infra fix notifications, shown as informational context
	if msgs, ok := buckets[store.MailTypeRemediation]; ok {
		var lines []string
		for _, m := range msgs {
			lines = append(lines, fmt.Sprintf("- [%s] %s", m.Subject, m.Body))
		}
		sections = append(sections, "# REMEDIATION NOTICES\n"+strings.Join(lines, "\n"))
	}

	if len(sections) == 0 {
		return ""
	}
	logger.Printf("Injecting %d mail message(s) into prompt for task %d", len(mails), taskID)
	return "\n\n" + strings.Join(sections, "\n\n")
}
