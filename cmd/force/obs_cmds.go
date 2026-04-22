package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"force-orchestrator/internal/agents"
	"force-orchestrator/internal/store"
)

func cmdStatus(db *sql.DB) {
	printStatus(db)
}

func cmdWho(db *sql.DB) {
	printWho(db)
}

func cmdStats(db *sql.DB, args []string) {
	port := store.GetConfig(db, "dashboard_port", "8080")
	for i := 0; i < len(args)-1; i++ {
		if args[i] == "--port" {
			port = args[i+1]
			break
		}
	}

	url := fmt.Sprintf("http://localhost:%s/api/stats", port)
	resp, err := http.Get(url) //nolint:noctx
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: daemon not reachable at %s\n  Start with: force daemon\n", url)
		os.Exit(1)
	}
	defer resp.Body.Close()

	var stats struct {
		Tasks         map[string]int `json:"tasks"`
		ActiveAgents  int            `json:"active_agents"`
		ActiveConvoys int            `json:"active_convoys"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&stats); err != nil {
		fmt.Fprintf(os.Stderr, "error: failed to parse /api/stats response: %v\n", err)
		os.Exit(1)
	}
	if stats.Tasks == nil {
		stats.Tasks = map[string]int{}
	}

	// Section 1: Tasks by Status
	fmt.Println("Tasks by Status")
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "STATUS\tCOUNT")
	fmt.Fprintln(w, "------\t-----")
	statuses := make([]string, 0, len(stats.Tasks))
	for s := range stats.Tasks {
		statuses = append(statuses, s)
	}
	sort.Strings(statuses)
	for _, s := range statuses {
		fmt.Fprintf(w, "%s\t%d\n", s, stats.Tasks[s])
	}
	w.Flush()

	// Section 2: Summary
	fmt.Println()
	fmt.Println("Fleet Summary")
	w2 := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w2, "ACTIVE AGENTS\tACTIVE CONVOYS")
	fmt.Fprintln(w2, "-------------\t--------------")
	fmt.Fprintf(w2, "%d\t%d\n", stats.ActiveAgents, stats.ActiveConvoys)
	w2.Flush()
}

func cmdBountyStats(db *sql.DB) {
	printBountyStats(db)
}

func cmdLogsFleet(db *sql.DB, args []string) {
	// Flags: --no-follow, --filter <pattern>, --agent <name>, --task <id>, --convoy <id>
	noFollow := false
	filterPattern := ""
	fleetArgs := args
	for i := 0; i < len(fleetArgs); i++ {
		switch fleetArgs[i] {
		case "--no-follow":
			noFollow = true
		case "--filter":
			if i+1 < len(fleetArgs) {
				filterPattern = fleetArgs[i+1]
				i++
			}
		case "--agent":
			if i+1 < len(fleetArgs) {
				// Escape brackets so grep treats this as a literal match,
				// not a character class — agent names like R2-D2 contain [-].
				filterPattern = `\[` + fleetArgs[i+1] + `\]`
				i++
			}
		case "--task":
			if i+1 < len(fleetArgs) {
				taskIDArg, taskArgErr := strconv.Atoi(fleetArgs[i+1])
				if taskArgErr != nil {
					fmt.Printf("Invalid task ID: %s\n", fleetArgs[i+1])
					os.Exit(1)
				}
				filterPattern = fmt.Sprintf("Task %d[^0-9]", taskIDArg)
				i++
			}
		case "--convoy":
			if i+1 < len(fleetArgs) {
				convoyIDStr := fleetArgs[i+1]
				i++
				var cid int
				fmt.Sscanf(convoyIDStr, "%d", &cid)
				taskRows, qErr := db.Query(`SELECT id FROM BountyBoard WHERE convoy_id = ?`, cid)
				if qErr == nil {
					var parts []string
					for taskRows.Next() {
						var tid int
						taskRows.Scan(&tid)
						parts = append(parts, fmt.Sprintf("Task %d[^0-9]", tid))
					}
					taskRows.Close()
					if len(parts) > 0 {
						filterPattern = strings.Join(parts, "|")
					} else {
						fmt.Printf("No tasks found for convoy %d.\n", cid)
						os.Exit(0)
					}
				}
			}
		}
	}
	if filterPattern != "" {
		if noFollow {
			grepCmd := exec.Command("grep", "-i", filterPattern, "fleet.log")
			grepOut, grepErr := grepCmd.Output()
			if grepErr != nil {
				fmt.Println("fleet.log not found — start the daemon first.")
			} else {
				lines := strings.Split(strings.TrimRight(string(grepOut), "\n"), "\n")
				if len(lines) > 100 {
					lines = lines[len(lines)-100:]
				}
				fmt.Println(strings.Join(lines, "\n"))
			}
		} else {
			tailCmd := exec.Command("tail", "-f", "fleet.log")
			tailOut, pipeErr := tailCmd.StdoutPipe()
			if pipeErr != nil {
				fmt.Println("fleet.log not found — start the daemon first.")
			} else {
				grepCmd := exec.Command("grep", "--line-buffered", "-i", filterPattern)
				grepCmd.Stdin = tailOut
				grepCmd.Stdout = os.Stdout
				grepCmd.Stderr = os.Stderr
				tailCmd.Start()
				grepCmd.Run()
			}
		}
	} else {
		tailArgs := []string{"-f", "fleet.log"}
		if noFollow {
			tailArgs = []string{"-n", "100", "fleet.log"}
		}
		tailCmd := exec.Command("tail", tailArgs...)
		tailCmd.Stdout = os.Stdout
		tailCmd.Stderr = os.Stderr
		if err := tailCmd.Run(); err != nil {
			fmt.Println("fleet.log not found — start the daemon first.")
		}
	}
}

func cmdHolonet(db *sql.DB, args []string) {
	// Flags: --no-follow (dump last 50), --filter <event_type> (grep by type), --task <id>
	noFollow := false
	filterType := ""
	filterTask := ""
	holoArgs := args
	for i := 0; i < len(holoArgs); i++ {
		switch holoArgs[i] {
		case "--no-follow":
			noFollow = true
		case "--filter":
			if i+1 < len(holoArgs) {
				filterType = holoArgs[i+1]
				i++
			}
		case "--task":
			if i+1 < len(holoArgs) {
				if _, taskArgErr := strconv.Atoi(holoArgs[i+1]); taskArgErr != nil {
					fmt.Printf("Invalid task ID: %s\n", holoArgs[i+1])
					os.Exit(1)
				}
				filterTask = holoArgs[i+1]
				i++
			}
		}
	}
	// Build filter pattern for grep (applied before tail/follow)
	if filterType != "" || filterTask != "" {
		typePattern := ""
		taskPattern := ""
		if filterType != "" {
			typePattern = fmt.Sprintf("\"event_type\":\"%s\"", filterType)
		}
		if filterTask != "" {
			taskPattern = fmt.Sprintf("\"task_id\":%s", filterTask)
		}
		if noFollow {
			data, readErr := os.ReadFile("holonet.jsonl")
			if readErr != nil {
				fmt.Println("holonet.jsonl not found — start the daemon first.")
			} else {
				var matched []string
				for _, line := range strings.Split(strings.TrimRight(string(data), "\n"), "\n") {
					if line == "" {
						continue
					}
					if typePattern != "" && !strings.Contains(line, typePattern) {
						continue
					}
					if taskPattern != "" && !strings.Contains(line, taskPattern) {
						continue
					}
					matched = append(matched, line)
				}
				if len(matched) > 50 {
					matched = matched[len(matched)-50:]
				}
				fmt.Println(strings.Join(matched, "\n"))
			}
		} else {
			tailCmd := exec.Command("tail", "-f", "holonet.jsonl")
			tailOut, pipeErr := tailCmd.StdoutPipe()
			if pipeErr != nil {
				fmt.Println("holonet.jsonl not found — start the daemon first.")
			} else if typePattern != "" && taskPattern != "" {
				grep1 := exec.Command("grep", "--line-buffered", typePattern)
				grep2 := exec.Command("grep", "--line-buffered", taskPattern)
				grep1Out, _ := grep1.StdoutPipe()
				grep1.Stdin = tailOut
				grep2.Stdin = grep1Out
				grep2.Stdout = os.Stdout
				grep2.Stderr = os.Stderr
				tailCmd.Start()
				grep1.Start()
				grep2.Run()
			} else {
				pattern := typePattern
				if pattern == "" {
					pattern = taskPattern
				}
				grepCmd := exec.Command("grep", "--line-buffered", pattern)
				grepCmd.Stdin = tailOut
				grepCmd.Stdout = os.Stdout
				grepCmd.Stderr = os.Stderr
				tailCmd.Start()
				grepCmd.Run()
			}
		}
	} else {
		tailArgs := []string{"-f", "holonet.jsonl"}
		if noFollow {
			tailArgs = []string{"-n", "50", "holonet.jsonl"}
		}
		tailCmd := exec.Command("tail", tailArgs...)
		tailCmd.Stdout = os.Stdout
		tailCmd.Stderr = os.Stderr
		if err := tailCmd.Run(); err != nil {
			fmt.Println("holonet.jsonl not found — start the daemon first.")
		}
	}
}

func cmdExport(db *sql.DB, file string) {
	if err := exportFleet(db, file); err != nil {
		fmt.Printf("Export failed: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("Fleet exported to %s\n", file)
}

func cmdImport(db *sql.DB, file string) {
	n, err := importFleet(db, file)
	if err != nil {
		fmt.Printf("Import failed: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("Imported %d task(s).\n", n)
}

func cmdSearch(db *sql.DB, query string) {
	escapedQuery := strings.ReplaceAll(query, `\`, `\\`)
	escapedQuery = strings.ReplaceAll(escapedQuery, `%`, `\%`)
	escapedQuery = strings.ReplaceAll(escapedQuery, `_`, `\_`)
	likeQuery := "%" + escapedQuery + "%"
	rows, err := db.Query(`
		SELECT id, target_repo, type, status, payload
		FROM BountyBoard
		WHERE payload LIKE ? ESCAPE '\' OR error_log LIKE ? ESCAPE '\'
		ORDER BY id DESC
		LIMIT 50`, likeQuery, likeQuery)
	if err != nil {
		fmt.Printf("Search error: %v\n", err)
		os.Exit(1)
	}
	defer rows.Close()
	found := false
	for rows.Next() {
		found = true
		var id int
		var repo, taskType, status, payload string
		rows.Scan(&id, &repo, &taskType, &status, &payload)
		fmt.Printf("[#%d] %s | %s | %s | %s\n", id, status, taskType, repo, payloadSummary(payload, 80))
	}
	if !found {
		fmt.Println("No tasks match your query.")
	}
}

func cmdAudit(db *sql.DB, limit int) {
	entries := store.ListAuditLog(db, limit)
	if len(entries) == 0 {
		fmt.Println("No audit log entries.")
		return
	}
	fmt.Printf("%-4s %-16s %-18s %-6s %-20s %s\n", "ID", "ACTOR", "ACTION", "TASK", "CREATED", "DETAIL")
	fmt.Println(strings.Repeat("-", 100))
	for _, e := range entries {
		taskStr := ""
		if e.TaskID > 0 {
			taskStr = fmt.Sprintf("#%d", e.TaskID)
		}
		fmt.Printf("%-4d %-16s %-18s %-6s %-20s %s\n",
			e.ID, truncate(e.Actor, 16), truncate(e.Action, 18),
			taskStr, truncate(e.CreatedAt, 20), truncate(e.Detail, 40))
	}
}

func cmdPrune(db *sql.DB, keepDays int, dryRun bool) {
	pruneFleet(db, keepDays, dryRun)
}

func cmdDogs(db *sql.DB, args []string) {
	sub := ""
	if len(args) > 0 {
		sub = args[0]
	}
	switch sub {
	case "", "list":
		dogs := agents.ListDogs(db)
		fmt.Printf("%-22s %-10s %-20s %s\n", "DOG", "RUNS", "LAST RUN", "NEXT RUN")
		fmt.Println(strings.Repeat("-", 78))
		for _, d := range dogs {
			lastRun := d.LastRun
			if lastRun == "" {
				lastRun = "(never)"
			}
			fmt.Printf("%-22s %-10d %-20s %s\n", d.Name, d.RunCount, truncate(lastRun, 20), d.NextRun)
		}
	case "run":
		if len(args) < 2 {
			fmt.Println("Usage: force dogs run <name>")
			fmt.Println()
			fmt.Println("Available dogs:")
			for _, name := range agents.DogNames() {
				fmt.Printf("  %s\n", name)
			}
			os.Exit(1)
		}
		name := args[1]
		valid := false
		for _, v := range agents.DogNames() {
			if v == name {
				valid = true
				break
			}
		}
		if !valid {
			fmt.Printf("Unknown dog %q.\n\nAvailable dogs:\n", name)
			for _, v := range agents.DogNames() {
				fmt.Printf("  %s\n", v)
			}
			os.Exit(1)
		}
		fmt.Printf("Running %s...\n", name)
		logger := log.New(os.Stdout, "["+name+"] ", log.LstdFlags)
		if err := agents.RunDogByName(db, name, logger); err != nil {
			fmt.Printf("Dog %s failed: %v\n", name, err)
			os.Exit(1)
		}
		fmt.Printf("Dog %s completed.\n", name)
	default:
		fmt.Printf("Unknown subcommand %q. Usage:\n", sub)
		fmt.Println("  force dogs            # list dogs and last-run times")
		fmt.Println("  force dogs run <name> # force-run a dog immediately")
		os.Exit(1)
	}
}

func cmdEscalations(db *sql.DB, args []string) {
	subCmd := ""
	if len(args) >= 1 {
		subCmd = args[0]
	}
	switch subCmd {
	case "list", "":
		statusFilter := ""
		if len(args) >= 2 {
			statusFilter = args[1]
		}
		printEscalations(db, statusFilter)
	case "ack":
		if len(args) < 2 {
			fmt.Println("Usage: force escalations ack <id>")
			os.Exit(1)
		}
		id := mustParseID(args[1])
		agents.AckEscalation(db, id)
		fmt.Printf("Escalation %d acknowledged.\n", id)
		// Surface the task ID so the operator knows the next step
		var e store.Escalation
		db.QueryRow(`SELECT task_id FROM Escalations WHERE id = ?`, id).Scan(&e.TaskID)
		if e.TaskID > 0 {
			fmt.Printf("Task #%d is still in Escalated status.\n", e.TaskID)
			fmt.Printf("  To requeue for retry:     force escalations requeue %d\n", id)
			fmt.Printf("  To inspect the task:      force logs %d\n", e.TaskID)
			fmt.Printf("  To manually reset it:     force reset %d\n", e.TaskID)
		}
	case "close":
		if len(args) < 2 {
			fmt.Println("Usage: force escalations close <id>")
			os.Exit(1)
		}
		id := mustParseID(args[1])
		agents.CloseEscalation(db, id, false)
		fmt.Printf("Escalation %d closed.\n", id)
	case "requeue":
		if len(args) < 2 {
			fmt.Println("Usage: force escalations requeue <id>")
			os.Exit(1)
		}
		id := mustParseID(args[1])
		agents.CloseEscalation(db, id, true)
		fmt.Printf("Escalation %d closed and task re-queued.\n", id)
	default:
		fmt.Printf("Unknown escalations subcommand: %s\n", subCmd)
		fmt.Println("Usage: force escalations [list|ack <id>|close <id>|requeue <id>]")
		os.Exit(1)
	}
}

func cmdCosts(db *sql.DB) {
	printCosts(db)
}

// cmdTailTask streams the live Claude output for an actively running task.
// The daemon writes fleet-task-<id>.log while Claude runs; this command tails it.
func cmdTailTask(db *sql.DB, taskID int) {
	b, err := store.GetBounty(db, taskID)
	if err != nil {
		fmt.Printf("Task %d not found.\n", taskID)
		os.Exit(1)
	}

	activeStatuses := map[string]bool{
		"Locked": true, "UnderCaptainReview": true, "UnderReview": true,
	}
	if !activeStatuses[b.Status] {
		fmt.Printf("Task #%d is not currently running (status: %s).\n", taskID, b.Status)
		fmt.Printf("  force logs %d      — see its error log\n", taskID)
		fmt.Printf("  force history %d   — see attempt history\n", taskID)
		os.Exit(1)
	}

	taskLogPath := fmt.Sprintf("fleet-task-%d.log", taskID)

	// Poll until the file appears — Claude may still be starting up (worktree
	// setup, branch creation, prompt assembly all happen before RunCLIStreaming).
	for i := 0; i < 20; i++ {
		if _, statErr := os.Stat(taskLogPath); statErr == nil {
			break
		}
		var currentStatus string
		db.QueryRow(`SELECT IFNULL(status,'') FROM BountyBoard WHERE id = ?`, taskID).Scan(&currentStatus)
		if !activeStatuses[currentStatus] {
			fmt.Printf("Task #%d finished before output became available (status: %s).\n", taskID, currentStatus)
			os.Exit(0)
		}
		if i == 0 {
			fmt.Printf("Task #%d is running (owner: %s) — waiting for Claude to start...\n", taskID, b.Owner)
		}
		time.Sleep(500 * time.Millisecond)
	}

	if _, statErr := os.Stat(taskLogPath); statErr != nil {
		fmt.Printf("Task #%d is running but %s does not exist.\n", taskID, taskLogPath)
		fmt.Printf("The daemon may be running from a different working directory.\n")
		fmt.Printf("Try: force logs-fleet --task %d\n", taskID)
		os.Exit(1)
	}

	fmt.Printf("=== force tail: task #%d (owner: %s) ===\n\n", taskID, b.Owner)
	tailCmd := exec.Command("tail", "-f", taskLogPath)
	tailCmd.Stdout = os.Stdout
	tailCmd.Stderr = os.Stderr
	tailCmd.Run()
}

func cmdLeaderboard(db *sql.DB) {
	entries := store.GetLeaderboard(db)
	if len(entries) == 0 {
		fmt.Println("No task history yet.")
		return
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "AGENT\tCOMPLETED\tFAILED\tAVG TURNS\tAVG TIME")
	for _, e := range entries {
		avgTurns := "-"
		if e.AvgTurns > 0 {
			avgTurns = fmt.Sprintf("%.1f", e.AvgTurns)
		}
		avgTime := "-"
		if e.AvgWallSeconds > 0 {
			d := time.Duration(e.AvgWallSeconds) * time.Second
			avgTime = d.String()
		}
		fmt.Fprintf(w, "%s\t%d\t%d\t%s\t%s\n", e.Agent, e.TasksCompleted, e.TasksFailed, avgTurns, avgTime)
	}
	w.Flush()
}

func cmdWatch(db *sql.DB) {
	watchSigChan := make(chan os.Signal, 1)
	signal.Notify(watchSigChan, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-watchSigChan
		fmt.Print("\033[?25h")
		fmt.Print("\033[H\033[2J")
		os.Exit(0)
	}()
	RunCommandCenter(db)
}
