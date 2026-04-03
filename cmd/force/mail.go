package main

import (
	"database/sql"
	"fmt"
	"os"
	"strings"

	"force-orchestrator/internal/store"
)

func cmdMail(db *sql.DB, args []string) {
	subCmd := ""
	if len(args) >= 1 {
		subCmd = args[0]
	}
	switch subCmd {
	case "send":
		// force mail send <to> [--task <id>] [--type directive|feedback|alert|info] <subject> [body]
		if len(args) < 3 {
			fmt.Println("Usage: force mail send <to-agent> [--task <id>] [--type directive|feedback|alert|info] <subject> [body]")
			os.Exit(1)
		}
		toAgent := args[1]
		taskID := 0
		msgType := store.MailTypeInfo
		mailArgs := args[2:]
		for i := 0; i < len(mailArgs); i++ {
			switch mailArgs[i] {
			case "--task":
				if i+1 < len(mailArgs) {
					taskID = mustParseID(mailArgs[i+1])
					mailArgs = append(mailArgs[:i], mailArgs[i+2:]...)
					i--
				}
			case "--type":
				if i+1 < len(mailArgs) {
					t := store.MailType(mailArgs[i+1])
					switch t {
					case store.MailTypeDirective, store.MailTypeFeedback, store.MailTypeAlert,
						store.MailTypeRemediation, store.MailTypeInfo:
						msgType = t
					default:
						fmt.Printf("Unknown mail type '%s'. Valid types: directive, feedback, alert, remediation, info\n", mailArgs[i+1])
						os.Exit(1)
					}
					mailArgs = append(mailArgs[:i], mailArgs[i+2:]...)
					i--
				}
			}
		}
		if len(mailArgs) == 0 {
			fmt.Println("Usage: force mail send <to-agent> [--task <id>] [--type directive|feedback|alert|info] <subject> [body]")
			os.Exit(1)
		}
		subject := mailArgs[0]
		body := ""
		if len(mailArgs) > 1 {
			body = strings.Join(mailArgs[1:], " ")
		}
		mailID := store.SendMail(db, "operator", toAgent, subject, body, taskID, msgType)
		fmt.Printf("Mail #%d sent to %s [%s]: %s\n", mailID, toAgent, string(msgType), subject)

	case "list":
		mails := store.ListMail(db, "")
		if len(mails) == 0 {
			fmt.Println("No mail.")
			return
		}
		fmt.Printf("%-4s %-16s %-16s %-11s %-8s %-20s %s\n", "ID", "FROM", "TO", "TYPE", "TASK", "CREATED", "SUBJECT")
		fmt.Println(strings.Repeat("-", 110))
		for _, m := range mails {
			read := ""
			if m.ReadAt == "" {
				read = "*"
			}
			taskStr := ""
			if m.TaskID > 0 {
				taskStr = fmt.Sprintf("#%d", m.TaskID)
			}
			fmt.Printf("%-4d %-16s %-16s %-11s %-8s %-20s %s%s\n",
				m.ID, truncate(m.FromAgent, 16), truncate(m.ToAgent, 16),
				truncate(string(m.MessageType), 11), taskStr, truncate(m.CreatedAt, 20), read, m.Subject)
		}

	case "inbox":
		agent := "operator"
		if len(args) >= 2 {
			agent = args[1]
		}
		mails := store.ListMail(db, agent)
		if len(mails) == 0 {
			fmt.Printf("No mail for %s.\n", agent)
			return
		}
		fmt.Printf("Inbox for %s (%d message(s)):\n\n", agent, len(mails))
		for _, m := range mails {
			read := "[unread]"
			if m.ReadAt != "" {
				read = "[read]  "
			}
			taskStr := ""
			if m.TaskID > 0 {
				taskStr = fmt.Sprintf(" (task #%d)", m.TaskID)
			}
			fmt.Printf("  #%-4d from %-16s %s [%-11s]%s — %s\n",
				m.ID, m.FromAgent, read, string(m.MessageType), taskStr, m.Subject)
		}

	case "read":
		if len(args) < 2 {
			fmt.Println("Usage: force mail read <mail-id>")
			os.Exit(1)
		}
		mailID := mustParseID(args[1])
		m := store.GetMail(db, mailID)
		if m == nil {
			fmt.Printf("Mail #%d not found.\n", mailID)
			os.Exit(1)
		}
		store.MarkMailRead(db, mailID)
		fmt.Printf("=== Mail #%d ===\n", m.ID)
		fmt.Printf("From:    %s\n", m.FromAgent)
		fmt.Printf("To:      %s\n", m.ToAgent)
		fmt.Printf("Date:    %s\n", m.CreatedAt)
		if m.TaskID > 0 {
			fmt.Printf("Task:    #%d\n", m.TaskID)
		}
		fmt.Printf("Subject: %s\n", m.Subject)
		fmt.Println()
		fmt.Println(m.Body)

	default:
		fmt.Println("Usage: force mail <list|inbox [agent]|read <id>|send <to> <subject> [body]>")
	}
}
