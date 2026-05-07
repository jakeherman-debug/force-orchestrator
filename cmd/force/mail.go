package main

import (
	"database/sql"
	"flag"
	"fmt"
	"os"
	"strings"

	"force-orchestrator/internal/store"
)

// cmdMail handles the `force mail` subcommands.
//
// Read-only verbs (list, inbox, read) keep their inline switch-case bodies.
// `read` does flip the read_at marker on the row, but is intentionally not
// extracted here — the destructive-verb extraction targets verbs that send
// external messages, mutate config, or delete records. Marking-as-read is
// a benign UI side-effect that the operator already explicitly opts into.
//
// Destructive verbs (send) are extracted into per-verb cmdMail<Verb>
// handlers that route through parseSubcommandFlags so --help short-circuits
// BEFORE the mail row is written / Slack webhook fires.
func cmdMail(db *sql.DB, args []string) {
	subCmd := ""
	if len(args) >= 1 {
		subCmd = args[0]
	}
	switch subCmd {
	case "--help", "-h", "help":
		fmt.Println("Usage: force mail <list|inbox [agent]|read <id>|send <to> <subject> [body]>")
		return
	case "send":
		cmdMailSend(db, args[1:])

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

// cmdMailSend — `force mail send <to-agent> [--task <id>] [--type ...] <subject> [body]`.
// DESTRUCTIVE: writes a Mail row AND, if a webhook is configured for the
// recipient, fires an external Slack notification.
//
// The legacy parser supported --task / --type as inline pair tokens. We keep
// that contract: `parseSubcommandFlags` peels off --help / unknown --flag and
// then the legacy inline scanner handles --task / --type. The unknown-flag
// rejection guards the safety contract; the legacy inline parser doesn't get
// a chance to see "--bogus-flag" because parseSubcommandFlags rejects it
// first. The --task / --type tokens slip past parseSubcommandFlags because
// the Go flag.FlagSet only knows about --help (we don't register them); but
// the FlagSet stops at the first non-flag positional, so the leading
// to-agent positional protects them. To avoid an unknown-flag false-positive
// when a real "--task 5" appears, we register them as no-op string flags
// here and read them back ourselves.
func cmdMailSend(db *sql.DB, args []string) {
	fs := flag.NewFlagSet("mail send", flag.ContinueOnError)
	taskFlag := fs.String("task", "", "associate this mail with a task ID")
	typeFlag := fs.String("type", "", "directive | feedback | alert | remediation | info")
	helped, perr := parseSubcommandFlags(fs, args, "mail send",
		"Send mail from operator to another agent. May fire an external Slack webhook.",
		[]flagDoc{
			{Name: "--task N", Desc: "associate this mail with a task ID"},
			{Name: "--type T", Desc: "directive | feedback | alert | remediation | info"},
			{Name: "--help, -h", Desc: "show this help and exit"},
		},
		[]string{"force mail send commander 'wake up' 'please re-plan convoy 17'"})
	if helped {
		return
	}
	if perr != nil {
		os.Exit(2)
	}
	rest := fs.Args()
	if len(rest) < 2 {
		fmt.Println("Usage: force mail send <to-agent> [--task <id>] [--type directive|feedback|alert|info] <subject> [body]")
		os.Exit(1)
	}
	toAgent := rest[0]
	taskID := 0
	if *taskFlag != "" {
		taskID = mustParseID(*taskFlag)
	}
	msgType := store.MailTypeInfo
	if *typeFlag != "" {
		t := store.MailType(*typeFlag)
		switch t {
		case store.MailTypeDirective, store.MailTypeFeedback, store.MailTypeAlert,
			store.MailTypeRemediation, store.MailTypeInfo:
			msgType = t
		default:
			fmt.Printf("Unknown mail type '%s'. Valid types: directive, feedback, alert, remediation, info\n", *typeFlag)
			os.Exit(1)
		}
	}
	subject := rest[1]
	body := ""
	if len(rest) > 2 {
		body = strings.Join(rest[2:], " ")
	}
	mailID := store.SendMail(db, "operator", toAgent, subject, body, taskID, msgType)
	fmt.Printf("Mail #%d sent to %s [%s]: %s\n", mailID, toAgent, string(msgType), subject)
}
