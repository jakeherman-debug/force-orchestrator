package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"flag"
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
	"force-orchestrator/internal/clients/codeartifact"
	"force-orchestrator/internal/clients/librarian"
	"force-orchestrator/internal/forcepath"
	"force-orchestrator/internal/store"
)

func cmdStatus(db *sql.DB, args []string) {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	helped, perr := parseSubcommandFlags(fs, args, "status",
		"Print fleet status (agents, queue depth, convoys).",
		[]flagDoc{{Name: "--help, -h", Desc: "show this help and exit"}},
		[]string{"force status"})
	if helped {
		return
	}
	if perr != nil {
		os.Exit(2)
	}
	printStatus(db)
}

func cmdWho(db *sql.DB, args []string) {
	fs := flag.NewFlagSet("who", flag.ContinueOnError)
	helped, perr := parseSubcommandFlags(fs, args, "who",
		"List active agents and what they're working on.",
		[]flagDoc{{Name: "--help, -h", Desc: "show this help and exit"}},
		[]string{"force who"})
	if helped {
		return
	}
	if perr != nil {
		os.Exit(2)
	}
	printWho(db)
}

func cmdStats(db *sql.DB, args []string) {
	fs := flag.NewFlagSet("stats", flag.ContinueOnError)
	portFlag := fs.String("port", "", "dashboard port to query (default: SystemConfig dashboard_port or 8080)")
	helped, perr := parseSubcommandFlags(fs, args, "stats",
		"Hit the daemon's /api/stats endpoint and print fleet statistics.",
		[]flagDoc{
			{Name: "--port P", Desc: "dashboard port to query"},
			{Name: "--help, -h", Desc: "show this help and exit"},
		},
		[]string{"force stats", "force stats --port 8080"})
	if helped {
		return
	}
	if perr != nil {
		os.Exit(2)
	}
	port := store.GetConfig(db, "dashboard_port", "8080")
	if *portFlag != "" {
		port = *portFlag
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

func cmdBountyStats(db *sql.DB, args []string) {
	fs := flag.NewFlagSet("bounty stats", flag.ContinueOnError)
	helped, perr := parseSubcommandFlags(fs, args, "bounty stats",
		"Print BountyBoard statistics by status/type.",
		[]flagDoc{{Name: "--help, -h", Desc: "show this help and exit"}},
		[]string{"force bounty stats"})
	if helped {
		return
	}
	if perr != nil {
		os.Exit(2)
	}
	printBountyStats(db)
}

func cmdLogsFleet(db *sql.DB, args []string) {
	fs := flag.NewFlagSet("logs-fleet", flag.ContinueOnError)
	noFollowFlag := fs.Bool("no-follow", false, "print the tail without following")
	filterFlagVal := fs.String("filter", "", "regex filter pattern")
	agentFlag := fs.String("agent", "", "filter by agent name (escaped as `\\[name\\]`)")
	taskFlag := fs.Int("task", 0, "filter by task ID")
	convoyFlag := fs.Int("convoy", 0, "filter by convoy ID (resolves member tasks)")
	helped, perr := parseSubcommandFlags(fs, args, "logs-fleet",
		"Tail the fleet.log (or print last lines with --no-follow), optionally filtered.",
		[]flagDoc{
			{Name: "--no-follow", Desc: "print the tail without following"},
			{Name: "--filter R", Desc: "regex filter pattern"},
			{Name: "--agent N", Desc: "filter by agent name"},
			{Name: "--task N", Desc: "filter by task ID"},
			{Name: "--convoy N", Desc: "filter by convoy ID"},
			{Name: "--help, -h", Desc: "show this help and exit"},
		},
		[]string{"force logs-fleet", "force logs-fleet --task 42", "force logs-fleet --no-follow --agent R2-D2"})
	if helped {
		return
	}
	if perr != nil {
		os.Exit(2)
	}
	noFollow := *noFollowFlag
	filterPattern := *filterFlagVal
	if *agentFlag != "" {
		// Escape brackets so grep treats this as a literal match,
		// not a character class — agent names like R2-D2 contain [-].
		filterPattern = `\[` + *agentFlag + `\]`
	}
	if *taskFlag != 0 {
		filterPattern = fmt.Sprintf("Task %d[^0-9]", *taskFlag)
	}
	if *convoyFlag != 0 {
		taskRows, qErr := db.Query(`SELECT id FROM BountyBoard WHERE convoy_id = ?`, *convoyFlag)
		if qErr == nil {
			var parts []string
			for taskRows.Next() {
				var tid int
				if err := taskRows.Scan(&tid); err != nil {
					fmt.Fprintf(os.Stderr, "warn: scan failed: %v\n", err)
					continue
				}
				parts = append(parts, fmt.Sprintf("Task %d[^0-9]", tid))
			}
			if rErr := taskRows.Err(); rErr != nil {
				log.Printf("obs_cmds.go:cmdLogsFleet: rows iter error: %v", rErr)
			}
			taskRows.Close()
			if len(parts) > 0 {
				filterPattern = strings.Join(parts, "|")
			} else {
				fmt.Printf("No tasks found for convoy %d.\n", *convoyFlag)
				os.Exit(0)
			}
		}
	}
	// Sweep-F: resolve the canonical fleet log path
	// (~/.force/fleet.log) once and use it for every grep/tail.
	fleetLogPath := forcepath.FleetLog()
	if filterPattern != "" {
		if noFollow {
			// Fix #9 (AUDIT-098): `--` before the pattern so an operator
			// filter like `-r` or `--include=...` can't be re-interpreted
			// by grep as a flag.
			grepCmd := exec.Command("grep", "-i", "--", filterPattern, fleetLogPath)
			grepOut, grepErr := grepCmd.Output()
			if grepErr != nil {
				fmt.Printf("%s not found — start the daemon first.\n", fleetLogPath)
			} else {
				lines := strings.Split(strings.TrimRight(string(grepOut), "\n"), "\n")
				if len(lines) > 100 {
					lines = lines[len(lines)-100:]
				}
				fmt.Println(strings.Join(lines, "\n"))
			}
		} else {
			tailCmd := exec.Command("tail", "-f", "--", fleetLogPath)
			tailOut, pipeErr := tailCmd.StdoutPipe()
			if pipeErr != nil {
				fmt.Printf("%s not found — start the daemon first.\n", fleetLogPath)
			} else {
				// Fix #9 (AUDIT-098): `--` separator applied here too.
				grepCmd := exec.Command("grep", "--line-buffered", "-i", "--", filterPattern)
				grepCmd.Stdin = tailOut
				grepCmd.Stdout = os.Stdout
				grepCmd.Stderr = os.Stderr
				tailCmd.Start()
				grepCmd.Run()
			}
		}
	} else {
		tailArgs := []string{"-f", fleetLogPath}
		if noFollow {
			tailArgs = []string{"-n", "100", fleetLogPath}
		}
		tailCmd := exec.Command("tail", tailArgs...)
		tailCmd.Stdout = os.Stdout
		tailCmd.Stderr = os.Stderr
		if err := tailCmd.Run(); err != nil {
			fmt.Printf("%s not found — start the daemon first.\n", fleetLogPath)
		}
	}
}

func cmdHolonet(db *sql.DB, args []string) {
	fs := flag.NewFlagSet("holonet", flag.ContinueOnError)
	noFollowFlag := fs.Bool("no-follow", false, "dump the last 50 lines and exit")
	filterFlagVal := fs.String("filter", "", "filter by event_type")
	taskFlag := fs.Int("task", 0, "filter by task ID")
	helped, perr := parseSubcommandFlags(fs, args, "holonet",
		"Tail / dump the holonet.jsonl event stream, optionally filtered.",
		[]flagDoc{
			{Name: "--no-follow", Desc: "dump the last 50 lines and exit"},
			{Name: "--filter T", Desc: "filter by event_type"},
			{Name: "--task N", Desc: "filter by task ID"},
			{Name: "--help, -h", Desc: "show this help and exit"},
		},
		[]string{"force holonet", "force holonet --no-follow --filter audit"})
	if helped {
		return
	}
	if perr != nil {
		os.Exit(2)
	}
	noFollow := *noFollowFlag
	filterType := *filterFlagVal
	filterTask := ""
	if *taskFlag != 0 {
		filterTask = strconv.Itoa(*taskFlag)
	}
	// Sweep-F: canonical holonet path (~/.force/holonet.jsonl).
	holonetPath := forcepath.HolonetEventStream()
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
			data, readErr := os.ReadFile(holonetPath)
			if readErr != nil {
				fmt.Printf("%s not found — start the daemon first.\n", holonetPath)
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
			tailCmd := exec.Command("tail", "-f", holonetPath)
			tailOut, pipeErr := tailCmd.StdoutPipe()
			if pipeErr != nil {
				fmt.Printf("%s not found — start the daemon first.\n", holonetPath)
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
		tailArgs := []string{"-f", holonetPath}
		if noFollow {
			tailArgs = []string{"-n", "50", holonetPath}
		}
		tailCmd := exec.Command("tail", tailArgs...)
		tailCmd.Stdout = os.Stdout
		tailCmd.Stderr = os.Stderr
		if err := tailCmd.Run(); err != nil {
			fmt.Printf("%s not found — start the daemon first.\n", holonetPath)
		}
	}
}

func cmdExport(db *sql.DB, args []string) {
	fs := flag.NewFlagSet("export", flag.ContinueOnError)
	helped, perr := parseSubcommandFlags(fs, args, "export",
		"Export the fleet's tasks/dependencies to a JSON file (default: fleet-export.json).",
		[]flagDoc{{Name: "--help, -h", Desc: "show this help and exit"}},
		[]string{"force export", "force export /tmp/fleet.json"})
	if helped {
		return
	}
	if perr != nil {
		os.Exit(2)
	}
	rest := fs.Args()
	file := "fleet-export.json"
	if len(rest) >= 1 {
		file = rest[0]
	}
	if err := exportFleet(db, file); err != nil {
		fmt.Printf("Export failed: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("Fleet exported to %s\n", file)
}

func cmdImport(db *sql.DB, args []string) {
	fs := flag.NewFlagSet("import", flag.ContinueOnError)
	helped, perr := parseSubcommandFlags(fs, args, "import",
		"Import a fleet export JSON file into the current holocron.",
		[]flagDoc{{Name: "--help, -h", Desc: "show this help and exit"}},
		[]string{"force import fleet-export.json"})
	if helped {
		return
	}
	if perr != nil {
		os.Exit(2)
	}
	rest := fs.Args()
	if len(rest) < 1 {
		fmt.Println("Usage: force import <file.json>")
		os.Exit(1)
	}
	n, err := importFleet(db, rest[0])
	if err != nil {
		fmt.Printf("Import failed: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("Imported %d task(s).\n", n)
}

func cmdSearch(db *sql.DB, args []string) {
	fs := flag.NewFlagSet("search", flag.ContinueOnError)
	helped, perr := parseSubcommandFlags(fs, args, "search",
		"Search BountyBoard.payload + error_log for the given LIKE-style query.",
		[]flagDoc{{Name: "--help, -h", Desc: "show this help and exit"}},
		[]string{"force search login flow"})
	if helped {
		return
	}
	if perr != nil {
		os.Exit(2)
	}
	rest := fs.Args()
	if len(rest) < 1 {
		fmt.Println("Usage: force search <query>")
		os.Exit(1)
	}
	query := strings.Join(rest, " ")
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
		if err := rows.Scan(&id, &repo, &taskType, &status, &payload); err != nil {
			fmt.Fprintf(os.Stderr, "warn: scan failed: %v\n", err)
			continue
		}
		fmt.Printf("[#%d] %s | %s | %s | %s\n", id, status, taskType, repo, payloadSummary(payload, 80))
	}
	if rErr := rows.Err(); rErr != nil {
		log.Printf("obs_cmds.go:cmdSearch: rows iter error: %v", rErr)
	}
	if !found {
		fmt.Println("No tasks match your query.")
	}
}

func cmdAudit(db *sql.DB, args []string) {
	fs := flag.NewFlagSet("audit", flag.ContinueOnError)
	limitFlag := fs.Int("limit", 50, "max number of audit log entries")
	helped, perr := parseSubcommandFlags(fs, args, "audit",
		"Print the AuditLog (operator + agent actions).",
		[]flagDoc{
			{Name: "--limit N", Desc: "max number of audit log entries"},
			{Name: "--help, -h", Desc: "show this help and exit"},
		},
		[]string{"force audit", "force audit --limit 200"})
	if helped {
		return
	}
	if perr != nil {
		os.Exit(2)
	}
	entries := store.ListAuditLog(db, *limitFlag)
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

func cmdPrune(db *sql.DB, args []string) {
	fs := flag.NewFlagSet("prune", flag.ContinueOnError)
	keepDaysFlag := fs.Int("keep-days", 30, "keep tasks newer than N days")
	dryRunFlag := fs.Bool("dry-run", false, "report what would be pruned without deleting")
	helped, perr := parseSubcommandFlags(fs, args, "prune",
		"Delete archived/cancelled tasks older than N days. Does NOT touch active tasks.",
		[]flagDoc{
			{Name: "--keep-days N", Desc: "keep tasks newer than N days"},
			{Name: "--dry-run", Desc: "report what would be pruned without deleting"},
			{Name: "--help, -h", Desc: "show this help and exit"},
		},
		[]string{"force prune --keep-days 30", "force prune --dry-run"})
	if helped {
		return
	}
	if perr != nil {
		os.Exit(2)
	}
	pruneFleet(db, *keepDaysFlag, *dryRunFlag)
}

// Fix #8e: ctx threads from main's signal-cancellation ctx so RunDogByName's
// downstream subprocess invocations cancel on SIGINT.
func cmdDogs(ctx context.Context, db *sql.DB, args []string) {
	sub := ""
	if len(args) > 0 {
		sub = args[0]
	}
	switch sub {
	case "", "list":
		fs := flag.NewFlagSet("dogs", flag.ContinueOnError)
		listArgs := args
		if sub == "list" {
			listArgs = args[1:]
		}
		helped, perr := parseSubcommandFlags(fs, listArgs, "dogs",
			"List registered dogs (background sweepers) with last/next-run times.",
			[]flagDoc{{Name: "--help, -h", Desc: "show this help and exit"}},
			[]string{"force dogs", "force dogs run reaper"})
		if helped {
			return
		}
		if perr != nil {
			os.Exit(2)
		}
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
	case "--help", "-h", "help":
		fmt.Println("Usage: force dogs [list|run <name>]")
		return
	case "run":
		fs := flag.NewFlagSet("dogs run", flag.ContinueOnError)
		helped, perr := parseSubcommandFlags(fs, args[1:], "dogs run",
			"Force-run a registered dog (background sweeper) immediately.",
			[]flagDoc{{Name: "--help, -h", Desc: "show this help and exit"}},
			[]string{"force dogs run reaper"})
		if helped {
			return
		}
		if perr != nil {
			os.Exit(2)
		}
		rest := fs.Args()
		if len(rest) < 1 {
			fmt.Println("Usage: force dogs run <name>")
			fmt.Println()
			fmt.Println("Available dogs:")
			for _, name := range agents.DogNames() {
				fmt.Printf("  %s\n", name)
			}
			os.Exit(1)
		}
		name := rest[0]
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
		// D0-B: construct the in-process Librarian at the CLI entry point
		// so RunDogByName has the same client the daemon would have given
		// it; the dashboard does the same in its handler.
		libClient := librarian.NewInProcess(db)
		// D5 Phase 4 (slice α): construct the CodeArtifact client too. On
		// constructor failure (e.g. CI without AWS config) keep nil — the
		// supply-* dogs detect nil and log/skip rather than crash.
		caClient, caErr := codeartifact.NewInProcess(ctx, db)
		if caErr != nil {
			fmt.Fprintf(os.Stderr, "[CODEARTIFACT] construction failed (%v) — supply dogs will skip\n", caErr)
			caClient = nil
		}
		if err := agents.RunDogByName(ctx, db, name, libClient, caClient, logger); err != nil {
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
		fs := flag.NewFlagSet("escalations list", flag.ContinueOnError)
		listArgs := args
		if subCmd == "list" {
			listArgs = args[1:]
		}
		helped, perr := parseSubcommandFlags(fs, listArgs, "escalations list",
			"List escalations. Optional positional: status filter.",
			[]flagDoc{{Name: "--help, -h", Desc: "show this help and exit"}},
			[]string{"force escalations", "force escalations list Open"})
		if helped {
			return
		}
		if perr != nil {
			os.Exit(2)
		}
		statusFilter := ""
		if rest := fs.Args(); len(rest) >= 1 {
			statusFilter = rest[0]
		}
		printEscalations(db, statusFilter)
	case "ack":
		fs := flag.NewFlagSet("escalations ack", flag.ContinueOnError)
		helped, perr := parseSubcommandFlags(fs, args[1:], "escalations ack",
			"Acknowledge an escalation (operator has seen it).",
			[]flagDoc{{Name: "--help, -h", Desc: "show this help and exit"}},
			[]string{"force escalations ack 17"})
		if helped {
			return
		}
		if perr != nil {
			os.Exit(2)
		}
		rest := fs.Args()
		if len(rest) < 1 {
			fmt.Println("Usage: force escalations ack <id>")
			os.Exit(1)
		}
		id := mustParseID(rest[0])
		agents.AckEscalation(db, id)
		fmt.Printf("Escalation %d acknowledged.\n", id)
		var e store.Escalation
		db.QueryRow(`SELECT task_id FROM Escalations WHERE id = ?`, id).Scan(&e.TaskID)
		if e.TaskID > 0 {
			fmt.Printf("Task #%d is still in Escalated status.\n", e.TaskID)
			fmt.Printf("  To requeue for retry:     force escalations requeue %d\n", id)
			fmt.Printf("  To inspect the task:      force logs %d\n", e.TaskID)
			fmt.Printf("  To manually reset it:     force reset %d\n", e.TaskID)
		}
	case "close":
		fs := flag.NewFlagSet("escalations close", flag.ContinueOnError)
		helped, perr := parseSubcommandFlags(fs, args[1:], "escalations close",
			"Close an escalation without re-queueing the task.",
			[]flagDoc{{Name: "--help, -h", Desc: "show this help and exit"}},
			[]string{"force escalations close 17"})
		if helped {
			return
		}
		if perr != nil {
			os.Exit(2)
		}
		rest := fs.Args()
		if len(rest) < 1 {
			fmt.Println("Usage: force escalations close <id>")
			os.Exit(1)
		}
		id := mustParseID(rest[0])
		agents.CloseEscalation(db, id, false)
		fmt.Printf("Escalation %d closed.\n", id)
	case "requeue":
		fs := flag.NewFlagSet("escalations requeue", flag.ContinueOnError)
		helped, perr := parseSubcommandFlags(fs, args[1:], "escalations requeue",
			"Close an escalation AND re-queue the task for retry.",
			[]flagDoc{{Name: "--help, -h", Desc: "show this help and exit"}},
			[]string{"force escalations requeue 17"})
		if helped {
			return
		}
		if perr != nil {
			os.Exit(2)
		}
		rest := fs.Args()
		if len(rest) < 1 {
			fmt.Println("Usage: force escalations requeue <id>")
			os.Exit(1)
		}
		id := mustParseID(rest[0])
		agents.CloseEscalation(db, id, true)
		fmt.Printf("Escalation %d closed and task re-queued.\n", id)
	case "--help", "-h", "help":
		fmt.Println("Usage: force escalations [list|ack <id>|close <id>|requeue <id>]")
	default:
		fmt.Printf("Unknown escalations subcommand: %s\n", subCmd)
		fmt.Println("Usage: force escalations [list|ack <id>|close <id>|requeue <id>]")
		os.Exit(1)
	}
}

func cmdCosts(db *sql.DB, args []string) {
	fs := flag.NewFlagSet("costs", flag.ContinueOnError)
	helped, perr := parseSubcommandFlags(fs, args, "costs",
		"Print cost summary (token spend by agent / convoy).",
		[]flagDoc{{Name: "--help, -h", Desc: "show this help and exit"}},
		[]string{"force costs"})
	if helped {
		return
	}
	if perr != nil {
		os.Exit(2)
	}
	printCosts(db)
}

// cmdTailTask streams the live Claude output for an actively running task.
// The daemon writes fleet-task-<id>.log while Claude runs; this command tails it.
func cmdTailTask(db *sql.DB, args []string) {
	fs := flag.NewFlagSet("tail", flag.ContinueOnError)
	helped, perr := parseSubcommandFlags(fs, args, "tail",
		"Stream the live Claude output for an actively-running task.",
		[]flagDoc{{Name: "--help, -h", Desc: "show this help and exit"}},
		[]string{"force tail 42"})
	if helped {
		return
	}
	if perr != nil {
		os.Exit(2)
	}
	rest := fs.Args()
	if len(rest) < 1 {
		fmt.Println("Usage: force tail <task-id>")
		os.Exit(1)
	}
	taskID := mustParseID(rest[0])
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

	// Sweep-F: canonical scratch path (~/.force/scratch/fleet-task-<id>.log).
	taskLogPath := forcepath.ScratchTaskFile(taskID)

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

func cmdLeaderboard(db *sql.DB, args []string) {
	fs := flag.NewFlagSet("leaderboard", flag.ContinueOnError)
	helped, perr := parseSubcommandFlags(fs, args, "leaderboard",
		"Print agent leaderboard (completion + turn counts).",
		[]flagDoc{{Name: "--help, -h", Desc: "show this help and exit"}},
		[]string{"force leaderboard"})
	if helped {
		return
	}
	if perr != nil {
		os.Exit(2)
	}
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

func cmdWatch(db *sql.DB, args []string) {
	fs := flag.NewFlagSet("watch", flag.ContinueOnError)
	helped, perr := parseSubcommandFlags(fs, args, "watch",
		"Live Command Center TUI — fleet status auto-refreshed.",
		[]flagDoc{{Name: "--help, -h", Desc: "show this help and exit"}},
		[]string{"force watch"})
	if helped {
		return
	}
	if perr != nil {
		os.Exit(2)
	}
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
