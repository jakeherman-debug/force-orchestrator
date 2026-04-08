package main

import (
	"database/sql"
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"

	"force-orchestrator/internal/agents"
	"force-orchestrator/internal/store"
)

func printList(db *sql.DB, statusFilter, repoFilter, typeFilter string, limit int) {
	query := `SELECT id, type, status, target_repo, owner, retry_count,
		COALESCE((SELECT MIN(td.depends_on) FROM TaskDependencies td
		          JOIN BountyBoard dep ON dep.id = td.depends_on
		          WHERE td.task_id = bb.id AND dep.status != 'Completed'), 0) AS active_dep,
		payload FROM BountyBoard bb`
	args := []any{}
	conditions := []string{}

	// statusFilter can be a single status or comma-separated list
	if statusFilter != "" {
		statuses := strings.Split(statusFilter, ",")
		placeholders := make([]string, len(statuses))
		for i, s := range statuses {
			placeholders[i] = "?"
			args = append(args, strings.TrimSpace(s))
		}
		conditions = append(conditions, fmt.Sprintf("status IN (%s)", strings.Join(placeholders, ",")))
	}
	if repoFilter != "" {
		conditions = append(conditions, "target_repo = ?")
		args = append(args, repoFilter)
	}
	if typeFilter != "" {
		conditions = append(conditions, "type = ?")
		args = append(args, typeFilter)
	}
	if len(conditions) > 0 {
		query += " WHERE " + strings.Join(conditions, " AND ")
	}
	query += ` ORDER BY id DESC`
	if limit > 0 {
		query += fmt.Sprintf(" LIMIT %d", limit)
	}

	rows, err := db.Query(query, args...)
	if err != nil {
		fmt.Printf("DB error: %v\n", err)
		return
	}
	defer rows.Close()
	fmt.Printf("%-4s %-12s %-22s %-15s %-15s %s\n", "ID", "STATUS", "TYPE", "REPO", "OWNER", "TASK")
	fmt.Println(strings.Repeat("-", 100))
	n := 0
	for rows.Next() {
		n++
		var id, retryCount, activeDep int
		var taskType, status, repo, owner, payload string
		rows.Scan(&id, &taskType, &status, &repo, &owner, &retryCount, &activeDep, &payload)
		taskPreview := payloadSummary(payload, 35)

		abbrev := status
		if a, ok := statusAbbrev[status]; ok {
			abbrev = a
		}
		if retryCount > 0 {
			abbrev = fmt.Sprintf("%s(r%d)", abbrev, retryCount)
		}
		if activeDep > 0 {
			abbrev = fmt.Sprintf("Blocked#%d", activeDep)
		}
		fmt.Printf("%-4d %-12s %-22s %-15s %-15s %s\n", id, truncate(abbrev, 12), truncate(taskType, 22), truncate(repo, 15), truncate(owner, 15), taskPreview)
	}
	if n == 0 {
		fmt.Println("(no tasks)")
	}
}

func printLogs(db *sql.DB, id int) {
	var b store.Bounty
	var errorLog string
	err := db.QueryRow(`
		SELECT id, parent_id, type, status, target_repo, owner, retry_count, infra_failures,
		       convoy_id, checkpoint, branch_name, IFNULL(error_log,''), payload
		FROM BountyBoard WHERE id = ?`, id).
		Scan(&b.ID, &b.ParentID, &b.Type, &b.Status, &b.TargetRepo, &b.Owner, &b.RetryCount,
			&b.InfraFailures, &b.ConvoyID, &b.Checkpoint, &b.BranchName, &errorLog, &b.Payload)
	if err != nil {
		fmt.Printf("Task %d not found\n", id)
		return
	}

	attemptCount := 0
	db.QueryRow(`SELECT COUNT(*) FROM TaskHistory WHERE task_id = ?`, id).Scan(&attemptCount)

	deps := store.GetDependencies(db, id)

	fmt.Printf("=== Task %d ===\n", b.ID)
	fmt.Printf("Type:          %s\n", b.Type)
	fmt.Printf("Status:        %s\n", b.Status)
	fmt.Printf("Repo:          %s\n", b.TargetRepo)
	fmt.Printf("Owner:         %s\n", b.Owner)
	fmt.Printf("Parent ID:     %d\n", b.ParentID)
	if len(deps) > 0 {
		depStrs := make([]string, len(deps))
		for i, d := range deps {
			depStrs[i] = fmt.Sprintf("#%d", d)
		}
		fmt.Printf("Blocked By:    %s\n", strings.Join(depStrs, ", "))
	}
	fmt.Printf("Convoy ID:     %d\n", b.ConvoyID)
	fmt.Printf("Branch:        %s\n", b.BranchName)
	fmt.Printf("Checkpoint:    %s\n", b.Checkpoint)
	fmt.Printf("Retries:       %d / %d\n", b.RetryCount, agents.MaxRetries)
	fmt.Printf("Infra Failures:%d / %d\n", b.InfraFailures, agents.MaxInfraFailures)
	fmt.Printf("History:       %d attempt(s) — run 'force history %d' for full output\n", attemptCount, id)
	fmt.Println("\n--- Payload ---")
	fmt.Println(b.Payload)
	if errorLog != "" {
		fmt.Println("\n--- Error Log ---")
		fmt.Println(errorLog)
	}
}

func printHistory(db *sql.DB, id int, full bool) {
	entries := store.GetTaskHistory(db, id)
	if len(entries) == 0 {
		fmt.Printf("No history found for task %d\n", id)
		return
	}
	fmt.Printf("=== History for task %d (%d attempt(s)) ===\n\n", id, len(entries))
	for _, e := range entries {
		fmt.Printf("--- Attempt %d | %s | agent: %s | session: %s ---\n", e.Attempt, e.CreatedAt, e.Agent, e.SessionID)
		fmt.Printf("Outcome: %s\n", e.Outcome)
		if e.TokensIn > 0 || e.TokensOut > 0 {
			fmt.Printf("Tokens:  %d input, %d output\n", e.TokensIn, e.TokensOut)
		}
		if e.ClaudeOutput != "" {
			out := e.ClaudeOutput
			if !full && len(out) > 2000 {
				out = out[:2000] + fmt.Sprintf("\n... (%d chars truncated — use --full to see all)", len(e.ClaudeOutput)-2000)
			}
			fmt.Println(out)
		}
		fmt.Println()
	}
}

func printEscalations(db *sql.DB, status string) {
	escalations := agents.ListEscalations(db, status)
	if len(escalations) == 0 {
		fmt.Println("No escalations found.")
		return
	}
	fmt.Printf("%-4s %-7s %-8s %-12s %-20s %s\n", "ID", "TASK", "SEV", "STATUS", "CREATED", "MESSAGE")
	fmt.Println(strings.Repeat("-", 90))
	for _, e := range escalations {
		fmt.Printf("%-4d %-7d %-8s %-12s %-20s %s\n",
			e.ID, e.TaskID, string(e.Severity), e.Status, e.CreatedAt, truncate(e.Message, 40))
	}
}

func printStats(db *sql.DB) {
	fmt.Println("=== Fleet Statistics ===")
	fmt.Println()

	// Task counts by status
	fmt.Println("Tasks by status:")
	rows, err := db.Query(`SELECT status, COUNT(*) FROM BountyBoard GROUP BY status ORDER BY COUNT(*) DESC`)
	if err == nil {
		for rows.Next() {
			var status string
			var count int
			rows.Scan(&status, &count)
			fmt.Printf("  %-25s %d\n", status, count)
		}
		rows.Close()
	}

	// Throughput: completed tasks in the last hour, 24h, all time
	fmt.Println()
	fmt.Println("Throughput (CodeEdit completions):")
	for _, window := range []struct {
		label string
		since string
	}{
		{"Last 1 hour", "-1 hour"},
		{"Last 24 hours", "-24 hours"},
		{"Last 7 days", "-7 days"},
	} {
		var n int
		db.QueryRow(`SELECT COUNT(*) FROM TaskHistory WHERE outcome = 'Completed' AND created_at >= datetime('now', ?)`, window.since).Scan(&n)
		fmt.Printf("  %-20s %d\n", window.label, n)
	}
	var allTime int
	db.QueryRow(`SELECT COUNT(*) FROM TaskHistory WHERE outcome = 'Completed'`).Scan(&allTime)
	fmt.Printf("  %-20s %d\n", "All time", allTime)

	// Top agents by completions
	fmt.Println()
	fmt.Println("Top agents by completions:")
	rows, err = db.Query(`SELECT agent, COUNT(*) as n FROM TaskHistory WHERE outcome = 'Completed' GROUP BY agent ORDER BY n DESC LIMIT 10`)
	if err == nil {
		for rows.Next() {
			var agent string
			var n int
			rows.Scan(&agent, &n)
			fmt.Printf("  %-20s %d\n", agent, n)
		}
		rows.Close()
	}

	// Escalations summary
	var openEsc, totalEsc int
	db.QueryRow(`SELECT COUNT(*) FROM Escalations`).Scan(&totalEsc)
	db.QueryRow(`SELECT COUNT(*) FROM Escalations WHERE status = 'Open'`).Scan(&openEsc)
	fmt.Println()
	fmt.Printf("Escalations: %d open / %d total\n", openEsc, totalEsc)

	// Convoy summary
	var activeConvoys, completedConvoys int
	db.QueryRow(`SELECT COUNT(*) FROM Convoys WHERE status = 'Active'`).Scan(&activeConvoys)
	db.QueryRow(`SELECT COUNT(*) FROM Convoys WHERE status = 'Completed'`).Scan(&completedConvoys)
	fmt.Printf("Convoys:     %d active, %d completed\n", activeConvoys, completedConvoys)

	// Failure rate
	var totalAttempts, failedAttempts int
	db.QueryRow(`SELECT COUNT(*) FROM TaskHistory`).Scan(&totalAttempts)
	db.QueryRow(`SELECT COUNT(*) FROM TaskHistory WHERE outcome = 'Failed'`).Scan(&failedAttempts)
	if totalAttempts > 0 {
		rate := float64(failedAttempts) / float64(totalAttempts) * 100
		fmt.Printf("Failure rate: %.1f%% (%d/%d attempts)\n", rate, failedAttempts, totalAttempts)
	}

	// Token usage totals
	var totalIn, totalOut int
	db.QueryRow(`SELECT IFNULL(SUM(tokens_in),0), IFNULL(SUM(tokens_out),0) FROM TaskHistory`).Scan(&totalIn, &totalOut)
	if totalIn > 0 || totalOut > 0 {
		fmt.Println()
		fmt.Printf("Token usage (all time): %d input, %d output\n", totalIn, totalOut)
	}
}

func printBountyStats(db *sql.DB) {
	fmt.Println("=== Bounty Board Statistics ===")
	fmt.Println()

	// Tasks by status
	fmt.Println("Tasks by status:")
	fmt.Printf("  %-25s %s\n", "Status", "Count")
	fmt.Println("  " + strings.Repeat("-", 35))
	rows, err := db.Query(`SELECT status, COUNT(*) FROM BountyBoard GROUP BY status ORDER BY COUNT(*) DESC`)
	if err == nil {
		for rows.Next() {
			var status string
			var count int
			rows.Scan(&status, &count)
			fmt.Printf("  %-25s %d\n", status, count)
		}
		rows.Close()
	}

	// Avg time-to-complete (last 7 days)
	fmt.Println()
	fmt.Println("Avg time-to-complete (last 7 days):")
	var avgSecs sql.NullFloat64
	db.QueryRow(`
		SELECT AVG(strftime('%s', th.created_at) - strftime('%s', bb.created_at))
		FROM TaskHistory th
		JOIN BountyBoard bb ON bb.id = th.task_id
		WHERE th.outcome = 'Completed' AND th.created_at >= datetime('now', '-7 days')`).Scan(&avgSecs)
	if avgSecs.Valid {
		total := int(avgSecs.Float64)
		h := total / 3600
		m := (total % 3600) / 60
		s := total % 60
		fmt.Printf("  %dh %dm %ds\n", h, m, s)
	} else {
		fmt.Println("  no completions in last 7 days")
	}

	// Top 3 agents by tasks completed
	fmt.Println()
	fmt.Println("Top 3 agents by tasks completed:")
	rows, err = db.Query(`SELECT agent, COUNT(*) FROM TaskHistory WHERE outcome='Completed' GROUP BY agent ORDER BY COUNT(*) DESC LIMIT 3`)
	if err == nil {
		fmt.Printf("  %-25s %s\n", "Agent", "Completed")
		fmt.Println("  " + strings.Repeat("-", 35))
		n := 0
		for rows.Next() {
			n++
			var agent string
			var count int
			rows.Scan(&agent, &count)
			fmt.Printf("  %-25s %d\n", agent, count)
		}
		rows.Close()
		if n == 0 {
			fmt.Println("  no data")
		}
	}
}

func printCosts(db *sql.DB) {
	fmt.Println("=== Fleet Token Usage ===")
	fmt.Println()

	var totalIn, totalOut int
	db.QueryRow(`SELECT IFNULL(SUM(tokens_in),0), IFNULL(SUM(tokens_out),0) FROM TaskHistory`).Scan(&totalIn, &totalOut)
	fmt.Printf("All time:   %d input tokens, %d output tokens\n", totalIn, totalOut)

	for _, w := range []struct{ label, since string }{
		{"Last 24h", "-24 hours"},
		{"Last 7d", "-7 days"},
	} {
		var in, out int
		db.QueryRow(`SELECT IFNULL(SUM(tokens_in),0), IFNULL(SUM(tokens_out),0) FROM TaskHistory WHERE created_at >= datetime('now', ?)`, w.since).Scan(&in, &out)
		fmt.Printf("%-12s %d input, %d output\n", w.label+":", in, out)
	}

	fmt.Println()
	fmt.Println("By agent (all time):")
	rows, err := db.Query(`SELECT agent, IFNULL(SUM(tokens_in),0), IFNULL(SUM(tokens_out),0)
		FROM TaskHistory GROUP BY agent ORDER BY SUM(tokens_in+tokens_out) DESC LIMIT 10`)
	if err == nil {
		for rows.Next() {
			var agent string
			var in, out int
			rows.Scan(&agent, &in, &out)
			fmt.Printf("  %-22s %6d in  %6d out\n", agent, in, out)
		}
		rows.Close()
	}

	// Warn about sessions where token data is missing (timed-out or very early failures).
	// Claude CLI only prints the token summary at the end of a session; if the process is
	// killed mid-run, no token data is available in the output.
	var missing int
	db.QueryRow(`SELECT COUNT(*) FROM TaskHistory WHERE IFNULL(tokens_in,0) = 0 AND IFNULL(tokens_out,0) = 0`).Scan(&missing)
	if missing > 0 {
		fmt.Printf("\n  Note: %d session(s) have no token data (timed out or early failure — tokens were consumed but not measurable).\n", missing)
	}
}

func printStatus(db *sql.DB) {
	counts := map[string]int{}
	rows, err := db.Query(`SELECT status, COUNT(*) FROM BountyBoard GROUP BY status`)
	if err == nil {
		for rows.Next() {
			var status string
			var count int
			rows.Scan(&status, &count)
			counts[status] = count
		}
		rows.Close()
	}

	var openEsc, highEsc int
	db.QueryRow(`SELECT COUNT(*) FROM Escalations WHERE status = 'Open'`).Scan(&openEsc)
	db.QueryRow(`SELECT COUNT(*) FROM Escalations WHERE status = 'Open' AND severity = 'HIGH'`).Scan(&highEsc)

	var activeConvoys int
	db.QueryRow(`SELECT COUNT(*) FROM Convoys WHERE status = 'Active'`).Scan(&activeConvoys)

	estopStatus := "off"
	if agents.IsEstopped(db) {
		estopStatus = "ACTIVE"
	}

	daemonStatus := "not running"
	if pidBytes, pidErr := os.ReadFile("fleet.pid"); pidErr == nil {
		pid, _ := strconv.Atoi(strings.TrimSpace(string(pidBytes)))
		if pid > 0 {
			proc, procErr := os.FindProcess(pid)
			if procErr == nil && proc.Signal(syscall.Signal(0)) == nil {
				daemonStatus = fmt.Sprintf("running (PID %d)", pid)
			} else {
				daemonStatus = fmt.Sprintf("stale PID %d (crashed?)", pid)
			}
		}
	}

	fmt.Printf("Daemon:        %s\n", daemonStatus)
	fmt.Printf("E-stop:        %s\n", estopStatus)
	fmt.Printf("Pending:       %d\n", counts["Pending"])
	captainActive := counts["AwaitingCaptainReview"] + counts["UnderCaptainReview"]
	fmt.Printf("Active:        %d (Locked: %d, Captain: %d, InReview: %d, AwaitReview: %d)\n",
		counts["Locked"]+captainActive+counts["UnderReview"]+counts["AwaitingCouncilReview"],
		counts["Locked"], captainActive, counts["UnderReview"], counts["AwaitingCouncilReview"])
	fmt.Printf("Completed:     %d\n", counts["Completed"])
	fmt.Printf("Failed:        %d\n", counts["Failed"])
	fmt.Printf("Escalated:     %d\n", counts["Escalated"])
	if openEsc > 0 {
		line := fmt.Sprintf("Open escalations: %d", openEsc)
		if highEsc > 0 {
			line += fmt.Sprintf(" (%d HIGH)", highEsc)
		}
		fmt.Println(line)
	}
	fmt.Printf("Active convoys: %d\n", activeConvoys)

	// Stall detection summary
	var stalled int
	db.QueryRow(`SELECT COUNT(*) FROM BountyBoard
		WHERE status IN ('Locked', 'UnderCaptainReview', 'UnderReview')
		  AND locked_at != ''
		  AND locked_at < datetime('now', ?)`,
		fmt.Sprintf("-%d seconds", int(agents.StallWarnTimeout.Seconds()))).Scan(&stalled)
	if stalled > 0 {
		fmt.Printf("Stalled agents: %d (locked >%v with no recent commits)\n", stalled, agents.StallWarnTimeout)
	}

	unread, totalMail := store.MailStats(db, "", "")
	if totalMail > 0 {
		fmt.Printf("Fleet mail:     %d unread / %d total\n", unread, totalMail)
	}
}

func printWho(db *sql.DB) {
	rows, err := db.Query(`
		SELECT id, type, status, target_repo, owner, payload
		FROM BountyBoard
		WHERE status IN ('Locked', 'UnderReview', 'AwaitingCaptainReview', 'UnderCaptainReview', 'AwaitingCouncilReview')
		  AND owner != ''
		ORDER BY owner, id`)
	if err != nil {
		fmt.Printf("DB error: %v\n", err)
		return
	}
	defer rows.Close()

	found := false
	for rows.Next() {
		if !found {
			fmt.Printf("%-20s %-4s %-14s %-15s %s\n", "AGENT", "ID", "STATUS", "REPO", "TASK")
			fmt.Println(strings.Repeat("-", 90))
			found = true
		}
		var id int
		var taskType, status, repo, owner, payload string
		rows.Scan(&id, &taskType, &status, &repo, &owner, &payload)
		abbrev := status
		if a, ok := statusAbbrev[status]; ok {
			abbrev = a
		}
		fmt.Printf("%-20s %-4d %-14s %-15s %s\n", owner, id, abbrev, truncate(repo, 15),
			payloadSummary(payload, 40))
	}
	if !found {
		fmt.Println("No agents currently active.")
	}
}

// printTree prints a task and all its children (by parent_id) recursively.
func printTree(db *sql.DB, id int, depth int) {
	var taskType, status, repo, payload string
	err := db.QueryRow(`SELECT type, status, target_repo, payload FROM BountyBoard WHERE id = ?`, id).
		Scan(&taskType, &status, &repo, &payload)
	if err != nil {
		fmt.Printf("%s[%d] (not found)\n", strings.Repeat("  ", depth), id)
		return
	}
	abbrev := status
	if a, ok := statusAbbrev[status]; ok {
		abbrev = a
	}
	indicator := ""
	if deps := store.GetDependencies(db, id); len(deps) > 0 {
		parts := make([]string, len(deps))
		for i, d := range deps {
			parts[i] = fmt.Sprintf("#%d", d)
		}
		indicator = fmt.Sprintf(" [blocked by %s]", strings.Join(parts, ", "))
	}
	preview := payloadSummary(payload, 50)
	fmt.Printf("%s[%d] %s | %s | %s%s — %s\n",
		strings.Repeat("  ", depth), id, abbrev, taskType, repo, indicator, preview)

	// Print children (subtasks created by this task).
	// Drain child IDs into a slice first — avoids deadlock on the SQLite single-connection
	// pool when printTree recursively calls db.QueryRow while the cursor is still open.
	rows, err := db.Query(`SELECT id FROM BountyBoard WHERE parent_id = ? ORDER BY id ASC`, id)
	if err != nil {
		return
	}
	var childIDs []int
	for rows.Next() {
		var childID int
		rows.Scan(&childID)
		childIDs = append(childIDs, childID)
	}
	rows.Close()
	for _, childID := range childIDs {
		printTree(db, childID, depth+1)
	}
}

func printAgents(db *sql.DB) {
	rows, err := db.Query(`SELECT agent_name, repo, worktree_path FROM Agents ORDER BY agent_name`)
	if err != nil {
		fmt.Printf("DB error: %v\n", err)
		return
	}
	defer rows.Close()

	fmt.Printf("%-20s %-20s %s\n", "AGENT", "REPO", "WORKTREE PATH")
	fmt.Println(strings.Repeat("-", 80))
	found := false
	for rows.Next() {
		found = true
		var agent, repo, path string
		rows.Scan(&agent, &repo, &path)

		exists := "OK"
		if _, err := os.Stat(path); err != nil {
			exists = "MISSING"
		}
		fmt.Printf("%-20s %-20s %s [%s]\n", agent, repo, path, exists)
	}
	if !found {
		fmt.Println("No agent worktrees registered.")
	}
}

// printConvoyShow renders the header block and task table for `force convoy show <name>`.
func printConvoyShow(db *sql.DB, convoyID int, name, status string, completed, total int) {
	fmt.Printf("Convoy:   %s\n", name)
	fmt.Printf("Status:   %s\n", status)
	fmt.Printf("Progress: %d/%d tasks complete\n\n", completed, total)

	rows, err := db.Query(`
		SELECT id, status, COALESCE(owner,''), COALESCE(payload,'')
		FROM BountyBoard WHERE convoy_id = ? ORDER BY id ASC`, convoyID)
	if err != nil {
		fmt.Printf("DB error: %v\n", err)
		return
	}
	defer rows.Close()

	fmt.Printf("%-6s %-18s %-20s %s\n", "ID", "STATUS", "OWNER", "PAYLOAD")
	fmt.Println(strings.Repeat("-", 110))
	n := 0
	for rows.Next() {
		n++
		var id int
		var taskStatus, owner, payload string
		rows.Scan(&id, &taskStatus, &owner, &payload)
		abbrev := taskStatus
		if a, ok := statusAbbrev[taskStatus]; ok {
			abbrev = a
		}
		fmt.Printf("%-6d %-18s %-20s %s\n", id, truncate(abbrev, 18), truncate(owner, 20), payloadSummary(payload, 60))
	}
	if n == 0 {
		fmt.Println("(no tasks)")
	}
}

func printUsage() {
	fmt.Println(`Usage: force <command> [args]

Agent control:
  daemon                         Start the fleet daemon (all agents)
  estop                          Emergency stop — halt all agents immediately
  resume                         Clear e-stop and resume agents
  scale [--astromechs N] [--council N] [--captain N] [--commanders N] [--investigators N] [--auditors N] [--librarians N]
                                 Dynamically scale any agent type in a running daemon (SIGUSR1)
  agents                         List registered persistent agent worktrees
  cleanup                        Prune dead git worktrees and stale agent entries
  doctor [--clean]               Pre-flight check: git, claude CLI, repos, DB health
                                   --clean: auto-fix stale fleet.pid and bad dep edges
  dogs                           List periodic dog agents and their last-run status
  config list                    Show all config values
  config get <key>               Read a system config value
  config set <key> <value>       Write a system config value
                                   Keys: num_astromechs, num_captain, num_council,
                                         num_commanders, num_investigators, num_auditors,
                                         max_concurrent, spawn_delay_ms, batch_size, max_turns
Logs written to fleet.log | Telemetry written to holonet.jsonl

Task management:
  add [--priority N] [--plan-only] [--type Feature|Investigate|Audit] <description>
                                        Queue a task (type auto-classified if omitted)
                                        All code changes use Feature — Commander plans, Chancellor reviews
                                        --plan-only: subtasks created as Planned, approve with convoy approve
  add-jira [--priority N] [--plan-only] <TICKET-ID>
                                        Fetch a Jira ticket and queue it as a feature task
  investigate [--priority N] [--repo <name>] <question>
                                        Research question — agent reads code + external systems,
                                        delivers a written report via fleet mail
  scan [--priority N] [--repo <name>] <scope/question>
                                        Scan codebase for issues — findings become Planned
                                        tasks; approve with: force convoy approve <id>
  add-repo <name> <path> <desc>         Register a repository
  repos                                 List registered repositories
  repos remove <name>                   Remove a registered repository
  run <id>                              One-shot foreground run — stream Claude to stdout
  status                                Quick summary of task counts and daemon state
  stats [--port N]                      Task counts by status, active agents, and active convoys (calls daemon)
  who                                   Show which agents are active and what they're working on
  list [status[,status2]] [--status <s>] [--repo <name>] [--type <type>] [--limit N]
                                        List tasks; filters are optional and combinable
  list active                           All non-terminal tasks (Pending, Locked, Planned, etc.)
  logs <id>                             Show full payload and error log for a task
  history [--full] <id>                 Show full Claude output for every attempt on a task
  reset <id>                            Reset a task to Pending (clears all error counts)
  retry <id>                            Alias for reset
  cancel <id> [--requeue <type>]        Permanently cancel a task (marks Failed, no retry)
                                        --requeue: re-queue with same payload as Feature|Investigate|Audit
  retry-all-failed                      Reset all failed tasks to Pending
  prioritize <id> <N>                   Set task priority (higher = claimed first)
  block <task-id> <blocker-id>          Add a dependency: task-id waits for blocker-id
  unblock <id>                          Remove all dependencies from task (unblock entirely)
  unblock-dependents <id>               Remove all dependency edges pointing to <id>
  tree <id>                             Show a task and all its subtasks as a tree
  diff <id>                             Show the current git diff for a task's branch
  search <query>                        Search task payloads and error logs
  export [file.json]                    Export full task board to JSON (default: fleet-export.json)
  import <file.json>                    Import Pending/Failed tasks from a JSON export
  approve <id>                          Operator approval — merge without Jedi Council review
  reject <id> <reason>                  Operator rejection — return task for rework with feedback
  prune [--keep-days N] [--dry-run]     Delete old completed/failed tasks and history (default 30d)
  purge [--confirm]                     Delete all log files, worktrees, and agent branches (keeps DB task data)
  hard-reset [--purge-repos] [--confirm]
                                        Factory reset: wipe all task data, history, memories, logs, worktrees.
                                        Repositories and config are preserved unless --purge-repos is set.
  audit [--limit N]                     Show operator audit log
  costs                                 Show token usage by agent and time window
  mail list                             List all fleet mail
  mail inbox <agent>                    Show inbox for a specific agent
  mail read <id>                        Read a mail message (marks as read)
  mail send <to> [--task <id>] <subj>   Send a mail message to an agent
  memories [repo] [--limit N]          Show fleet memories (agent learnings across tasks)
  memories search <repo> <query>       FTS search fleet memories for a repo
  directive show [role]                Show the active directive for a role
  directive example [role]             Print an example directive file for a role

Escalations:
  escalations list [status]      List escalations (optionally filter: Open/Acknowledged/Closed)
  escalations ack <id>           Acknowledge an escalation
  escalations close <id>         Close an escalation
  escalations requeue <id>       Close an escalation and re-queue its task

Convoys:
  convoy list                    List all convoys with progress
  convoy create <name>           Create a named convoy
  convoy show <name>             Show convoy details and task table
  convoy approve <id>            Approve a plan-only convoy (Planned → Pending)
  convoy reset <id>              Reset all failed/escalated tasks in a convoy to Pending
  convoy reject <id> <feedback>  Reject Commander's plan, cancel tasks, and re-queue for re-planning

Dashboard:
  watch                          Live-updating task dashboard
  dashboard [--port N]           HTTP dashboard at localhost:8080 (JSON API + SSE events)
  logs-fleet [--no-follow] [--filter <pattern>] [--agent <name>] [--task <id>] [--convoy <id>]
                                 Tail fleet.log (all agent output)
  holonet [--no-follow] [--filter <event_type>] [--task <id>]
                                 Tail holonet.jsonl (structured telemetry stream)`)
}
