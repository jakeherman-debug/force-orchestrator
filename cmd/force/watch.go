package main

import (
	"database/sql"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"force-orchestrator/internal/agents"
	"force-orchestrator/internal/store"
)

func truncate(text string, maxLen int) string {
	if len(text) > maxLen {
		return text[:maxLen-3] + "..."
	}
	return text + strings.Repeat(" ", maxLen-len(text))
}

// payloadSummary returns a compact single-line preview of a task payload,
// stripping the [GOAL: ...]\n\n prefix Commander adds to subtask payloads.
func payloadSummary(payload string, maxLen int) string {
	s := payload
	if strings.HasPrefix(s, "[GOAL: ") {
		if end := strings.Index(s, "]\n\n"); end != -1 {
			s = s[end+3:]
		}
	}
	return truncate(strings.ReplaceAll(s, "\n", " "), maxLen)
}

// RunCommandCenter is the terminal-based fleet dashboard. Writes ANSI
// escape codes + status summaries to stdout in an infinite loop. Called
// by `force watch`.
//
// AUDIT G4 (Fix #8d): delegates to runCommandCenterTo so tests can pass
// an isolated io.Writer per call (bytes.Buffer, io.Discard) instead of
// racing on the shared os.Stdout global. The production CLI still uses
// os.Stdout directly.
func RunCommandCenter(db *sql.DB) {
	runCommandCenterTo(db, os.Stdout)
}

// runCommandCenterTo is the io.Writer-parameterised body. Tests call
// this with io.Discard or a bytes.Buffer so there's no shared-global
// race with parallel test goroutines.
func runCommandCenterTo(db *sql.DB, out io.Writer) {
	fmt.Fprint(out, "\033[?25l")
	defer fmt.Fprint(out, "\033[?25h")

	for {
		fmt.Fprint(out, "\033[H\033[2J")
		fmt.Fprintln(out, "=========================================================================================")
		fmt.Fprintln(out, "                     GALACTIC FLEET COMMAND CENTER — ORDER 66 MONITORING               ")
		fmt.Fprintln(out, "=========================================================================================")

		// Show e-stop status if active
		if agents.IsEstopped(db) {
			fmt.Fprintln(out, "  *** E-STOP ACTIVE — all agents halted. Run: force resume ***")
		}
		fmt.Fprintln(out)

		// Show all non-completed tasks plus the 10 most recent completed ones
		rows, err := db.Query(`
			SELECT id, target_repo,
			    COALESCE((SELECT MIN(td.depends_on) FROM TaskDependencies td
			              JOIN BountyBoard dep ON dep.id = td.depends_on
			              WHERE td.task_id = bb.id AND dep.status != 'Completed'), 0) AS active_dep,
			    status, payload, owner, IFNULL(error_log, ''), retry_count,
			    IFNULL(locked_at, '') AS locked_at
			FROM BountyBoard bb
			WHERE status != 'Completed'
			UNION ALL
			SELECT id, target_repo, 0, status, payload, owner, IFNULL(error_log, ''), retry_count,
			    '' AS locked_at
			FROM (SELECT id, target_repo, status, payload, owner, error_log, retry_count
			      FROM BountyBoard WHERE status = 'Completed' ORDER BY id DESC LIMIT 10)
			ORDER BY id ASC`)
		if err != nil {
			fmt.Fprintf(out, "  DATABASE ERROR: %v\n", err)
			time.Sleep(2 * time.Second)
			continue
		}

		var pending, planned, active, chancellor, coordinating, reviewing, completed, failed, escalated []string

		for rows.Next() {
			var id, activeDep, retryCount int
			var repo, status, payload, owner, errorLog, lockedAt string
			if err := rows.Scan(&id, &repo, &activeDep, &status, &payload, &owner, &errorLog, &retryCount, &lockedAt); err != nil {
				fmt.Fprintf(os.Stderr, "warn: scan failed: %v\n", err)
				continue
			}

			repoStr := truncate(repo, 15)
			taskStr := payloadSummary(payload, 40)

			var line string
			switch {
			case status == "Failed":
				errStr := truncate(errorLog, 35)
				line = fmt.Sprintf(" [ID: %02d] FAILED | %s | %s | ERR: %s", id, repoStr, taskStr, errStr)
				failed = append(failed, line)
				continue
			case status == "Escalated":
				line = fmt.Sprintf(" [ID: %02d] %s | %s | ESCALATED", id, repoStr, taskStr)
				escalated = append(escalated, line)
				continue
			case activeDep > 0:
				line = fmt.Sprintf(" [ID: %02d] BLOCKED BY %02d | %s | %s", id, activeDep, repoStr, taskStr)
			default:
				retryStr := ""
				if retryCount > 0 {
					retryStr = fmt.Sprintf(" [retry %d/%d]", retryCount, agents.MaxRetries)
				}
				ownerStr := truncate(owner, 15)
				elapsedStr := ""
				if lockedAt != "" && (status == "Locked" || status == "UnderCaptainReview" || status == "UnderReview") {
					if t, parseErr := time.Parse("2006-01-02 15:04:05", lockedAt); parseErr == nil {
						elapsedStr = fmt.Sprintf(" | %v", time.Since(t).Round(time.Second))
					}
				}
				line = fmt.Sprintf(" [ID: %02d] %s%s | %s | %s%s", id, ownerStr, elapsedStr, repoStr, taskStr, retryStr)
			}

			switch status {
			case "Pending":
				pending = append(pending, line)
			case "Planned":
				planned = append(planned, line)
			case "Locked", "UnderReview":
				active = append(active, line)
			case "AwaitingChancellorReview":
				chancellor = append(chancellor, line)
			case "AwaitingCaptainReview", "UnderCaptainReview":
				coordinating = append(coordinating, line)
			case "AwaitingCouncilReview":
				reviewing = append(reviewing, line)
			case "Completed":
				completed = append(completed, line)
			}
		}
		rows.Close()

		fmt.Fprintln(out, "--- PENDING & BLOCKED ---")
		for _, p := range pending {
			fmt.Fprintln(out, p)
		}
		if len(planned) > 0 {
			fmt.Fprintln(out, "\n--- PLANNED (awaiting convoy approve) ---")
			for _, p := range planned {
				fmt.Fprintln(out, p)
			}
		}
		fmt.Fprintln(out, "\n--- ACTIVE OPERATIONS ---")
		for _, a := range active {
			fmt.Fprintln(out, a)
		}
		if len(chancellor) > 0 {
			fmt.Fprintln(out, "\n--- CHANCELLOR REVIEW ---")
			for _, c := range chancellor {
				fmt.Fprintln(out, c)
			}
		}
		if len(coordinating) > 0 {
			fmt.Fprintln(out, "\n--- CAPTAIN REVIEW ---")
			for _, c := range coordinating {
				fmt.Fprintln(out, c)
			}
		}
		fmt.Fprintln(out, "\n--- JEDI COUNCIL ---")
		for _, r := range reviewing {
			fmt.Fprintln(out, r)
		}
		fmt.Fprintln(out, "\n--- COMPLETED ---")
		for _, c := range completed {
			fmt.Fprintln(out, c)
		}
		fmt.Fprintln(out, "\n--- FAILED ---")
		for _, f := range failed {
			fmt.Fprintln(out, f)
		}

		if len(escalated) > 0 {
			fmt.Fprintln(out, "\n--- ESCALATED (awaiting operator) ---")
			for _, e := range escalated {
				fmt.Fprintln(out, e)
			}
		}

		// Escalation summary
		var openEscalations, highEscalations int
		db.QueryRow(`SELECT COUNT(*) FROM Escalations WHERE status = 'Open'`).Scan(&openEscalations)
		db.QueryRow(`SELECT COUNT(*) FROM Escalations WHERE status = 'Open' AND severity = 'HIGH'`).Scan(&highEscalations)
		if openEscalations > 0 {
			fmt.Fprintf(out, "\n--- ESCALATIONS (%d open", openEscalations)
			if highEscalations > 0 {
				fmt.Fprintf(out, ", %d HIGH severity", highEscalations)
			}
			fmt.Fprintln(out, ") ---")
			escRows, escErr := db.Query(`SELECT id, task_id, severity, message FROM Escalations WHERE status = 'Open' ORDER BY severity DESC, created_at ASC LIMIT 5`)
			if escErr == nil {
				for escRows.Next() {
					var id, taskID int
					var sev, msg string
					if err := escRows.Scan(&id, &taskID, &sev, &msg); err != nil {
						fmt.Fprintf(os.Stderr, "warn: scan failed: %v\n", err)
						continue
					}
					fmt.Fprintf(out, " [ESC-%02d] task %-3d [%s] %s\n", id, taskID, sev, truncate(msg, 60))
				}
				escRows.Close()
			}
		}

		// Active convoy summary
		convoyRows, convoyErr := db.Query(`SELECT id, name FROM Convoys WHERE status = 'Active' ORDER BY created_at DESC LIMIT 5`)
		if convoyErr == nil {
			type convoyInfo struct {
				id   int
				name string
			}
			var convoys []convoyInfo
			for convoyRows.Next() {
				var c convoyInfo
				if err := convoyRows.Scan(&c.id, &c.name); err != nil {
					fmt.Fprintf(os.Stderr, "warn: scan failed: %v\n", err)
					continue
				}
				convoys = append(convoys, c)
			}
			convoyRows.Close()
			if len(convoys) > 0 {
				fmt.Fprintln(out, "\n--- ACTIVE CONVOYS ---")
				for _, c := range convoys {
					completed, total := store.ConvoyProgress(db, c.id)
					pct := 0
					if total > 0 {
						pct = completed * 100 / total
					}
					bar := strings.Repeat("#", pct/5) + strings.Repeat(".", 20-pct/5)
					fmt.Fprintf(out, " [%d] [%s] %d%% %s\n", c.id, bar, pct, truncate(c.name, 40))
				}
			}
		}

		fmt.Fprintln(out, "\n=========================================================================================")
		fmt.Fprintf(out, "Last updated: %-30s  Press Ctrl+C to exit\n", time.Now().Format("2006-01-02 15:04:05"))
		time.Sleep(2 * time.Second)
	}
}

// statusAbbrev converts long status names to short display forms.
var statusAbbrev = map[string]string{
	"Pending":                    "Pending",
	"Locked":                     "Active",
	"AwaitingChancellorReview":   "AwaitChancellor",
	"AwaitingCaptainReview":      "AwaitCapt",
	"UnderCaptainReview":         "InCapt",
	"AwaitingCouncilReview":      "AwaitReview",
	"UnderReview":                "InReview",
	"Completed":                  "Completed",
	"Failed":                     "Failed",
	"Escalated":                  "Escalated",
}

