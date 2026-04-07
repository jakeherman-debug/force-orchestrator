package main

import (
	"database/sql"
	"fmt"
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

func RunCommandCenter(db *sql.DB) {
	fmt.Print("\033[?25l")
	defer fmt.Print("\033[?25h")

	for {
		fmt.Print("\033[H\033[2J")
		fmt.Println("=========================================================================================")
		fmt.Println("                     GALACTIC FLEET COMMAND CENTER — ORDER 66 MONITORING               ")
		fmt.Println("=========================================================================================")

		// Show e-stop status if active
		if agents.IsEstopped(db) {
			fmt.Println("  *** E-STOP ACTIVE — all agents halted. Run: force resume ***")
		}
		fmt.Println()

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
			fmt.Printf("  DATABASE ERROR: %v\n", err)
			time.Sleep(2 * time.Second)
			continue
		}

		var pending, planned, active, coordinating, reviewing, completed, failed, escalated []string

		for rows.Next() {
			var id, activeDep, retryCount int
			var repo, status, payload, owner, errorLog, lockedAt string
			rows.Scan(&id, &repo, &activeDep, &status, &payload, &owner, &errorLog, &retryCount, &lockedAt)

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
			case "AwaitingCaptainReview", "UnderCaptainReview":
				coordinating = append(coordinating, line)
			case "AwaitingCouncilReview":
				reviewing = append(reviewing, line)
			case "Completed":
				completed = append(completed, line)
			}
		}
		rows.Close()

		fmt.Println("--- PENDING & BLOCKED ---")
		for _, p := range pending {
			fmt.Println(p)
		}
		if len(planned) > 0 {
			fmt.Println("\n--- PLANNED (awaiting convoy approve) ---")
			for _, p := range planned {
				fmt.Println(p)
			}
		}
		fmt.Println("\n--- ACTIVE OPERATIONS ---")
		for _, a := range active {
			fmt.Println(a)
		}
		if len(coordinating) > 0 {
			fmt.Println("\n--- CAPTAIN REVIEW ---")
			for _, c := range coordinating {
				fmt.Println(c)
			}
		}
		fmt.Println("\n--- JEDI COUNCIL ---")
		for _, r := range reviewing {
			fmt.Println(r)
		}
		fmt.Println("\n--- COMPLETED ---")
		for _, c := range completed {
			fmt.Println(c)
		}
		fmt.Println("\n--- FAILED ---")
		for _, f := range failed {
			fmt.Println(f)
		}

		if len(escalated) > 0 {
			fmt.Println("\n--- ESCALATED (awaiting operator) ---")
			for _, e := range escalated {
				fmt.Println(e)
			}
		}

		// Escalation summary
		var openEscalations, highEscalations int
		db.QueryRow(`SELECT COUNT(*) FROM Escalations WHERE status = 'Open'`).Scan(&openEscalations)
		db.QueryRow(`SELECT COUNT(*) FROM Escalations WHERE status = 'Open' AND severity = 'HIGH'`).Scan(&highEscalations)
		if openEscalations > 0 {
			fmt.Printf("\n--- ESCALATIONS (%d open", openEscalations)
			if highEscalations > 0 {
				fmt.Printf(", %d HIGH severity", highEscalations)
			}
			fmt.Println(") ---")
			escRows, escErr := db.Query(`SELECT id, task_id, severity, message FROM Escalations WHERE status = 'Open' ORDER BY severity DESC, created_at ASC LIMIT 5`)
			if escErr == nil {
				for escRows.Next() {
					var id, taskID int
					var sev, msg string
					escRows.Scan(&id, &taskID, &sev, &msg)
					fmt.Printf(" [ESC-%02d] task %-3d [%s] %s\n", id, taskID, sev, truncate(msg, 60))
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
				convoyRows.Scan(&c.id, &c.name)
				convoys = append(convoys, c)
			}
			convoyRows.Close()
			if len(convoys) > 0 {
				fmt.Println("\n--- ACTIVE CONVOYS ---")
				for _, c := range convoys {
					completed, total := store.ConvoyProgress(db, c.id)
					pct := 0
					if total > 0 {
						pct = completed * 100 / total
					}
					bar := strings.Repeat("#", pct/5) + strings.Repeat(".", 20-pct/5)
					fmt.Printf(" [%d] [%s] %d%% %s\n", c.id, bar, pct, truncate(c.name, 40))
				}
			}
		}

		fmt.Println("\n=========================================================================================")
		fmt.Printf("Last updated: %-30s  Press Ctrl+C to exit\n", time.Now().Format("2006-01-02 15:04:05"))
		time.Sleep(2 * time.Second)
	}
}

// statusAbbrev converts long status names to short display forms.
var statusAbbrev = map[string]string{
	"Pending":               "Pending",
	"Locked":                "Active",
	"AwaitingCaptainReview": "AwaitCapt",
	"UnderCaptainReview":    "InCapt",
	"AwaitingCouncilReview": "AwaitReview",
	"UnderReview":           "InReview",
	"Completed":             "Completed",
	"Failed":                "Failed",
	"Escalated":             "Escalated",
}

