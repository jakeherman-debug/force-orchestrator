package main

import (
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"force-orchestrator/internal/agents"
	"force-orchestrator/internal/claude"
	igit "force-orchestrator/internal/git"
	"force-orchestrator/internal/store"
	"force-orchestrator/internal/telemetry"
)

func mustParseID(s string) int {
	id, err := strconv.Atoi(s)
	if err != nil {
		fmt.Printf("Invalid ID: %s\n", s)
		os.Exit(1)
	}
	return id
}

func main() {
	command := "status"
	if len(os.Args) >= 2 {
		command = os.Args[1]
	}

	db := store.InitHolocron()
	defer db.Close()
	telemetry.InitTelemetry()

	switch command {
	case "help", "--help", "-h":
		printUsage()

	case "version", "--version", "-v":
		fmt.Println("force-orchestrator — Galactic Fleet Command System")
		fmt.Printf("Built with %s\n", runtime.Version())

	case "daemon":
		// Prevent double-daemon: write PID file, but verify if the existing one is still alive
		pidFile := "fleet.pid"
		if existing, err := os.ReadFile(pidFile); err == nil {
			pid, _ := strconv.Atoi(strings.TrimSpace(string(existing)))
			if pid > 0 {
				proc, procErr := os.FindProcess(pid)
				if procErr == nil && proc.Signal(syscall.Signal(0)) == nil {
					fmt.Printf("Daemon already running (PID %d). Run 'force estop' to halt agents.\n", pid)
					os.Exit(1)
				}
			}
			fmt.Printf("Stale fleet.pid found (PID %s) — previous daemon appears dead, restarting.\n",
				strings.TrimSpace(string(existing)))
		}
		os.WriteFile(pidFile, []byte(fmt.Sprintf("%d", os.Getpid())), 0644)
		defer os.Remove(pidFile)

		numAgents := 2
		if n := store.GetConfig(db, "num_astromechs", ""); n != "" {
			fmt.Sscanf(n, "%d", &numAgents)
		}
		if numAgents < 1 {
			numAgents = 1
		}
		numCouncil := 1
		if n := store.GetConfig(db, "num_council", ""); n != "" {
			fmt.Sscanf(n, "%d", &numCouncil)
		}

		numCaptain := 1
		if n := store.GetConfig(db, "num_captain", ""); n != "" {
			fmt.Sscanf(n, "%d", &numCaptain)
		}

		astromechRoster := []string{"R2-D2", "BB-8", "R5-D4", "K-2SO", "BD-1", "R7-A7", "R4-P17", "D-O", "C1-10P", "R3-S6"}
		councilRoster   := []string{"Council-Yoda", "Council-Mace", "Council-Ki-Adi", "Council-Kit-Fisto", "Council-Shaak-Ti"}
		captainRoster   := []string{"Captain-Rex", "Captain-Wolffe", "Captain-Bly", "Captain-Gree", "Captain-Ponds"}

		fmt.Printf("Starting the Fleet Daemon (%d astromech(s), %d captain(s), %d council member(s))...\n", numAgents, numCaptain, numCouncil)
		go agents.SpawnCommander(db)
		for i := 0; i < numAgents; i++ {
			name := fmt.Sprintf("Astromech-%d", i+1)
			if i < len(astromechRoster) {
				name = astromechRoster[i]
			}
			go agents.SpawnAstromech(db, name)
		}
		for i := 0; i < numCaptain; i++ {
			name := fmt.Sprintf("Captain-%d", i+1)
			if i < len(captainRoster) {
				name = captainRoster[i]
			}
			go agents.SpawnCaptain(db, name)
		}
		for i := 0; i < numCouncil; i++ {
			name := fmt.Sprintf("Council-%d", i+1)
			if i < len(councilRoster) {
				name = councilRoster[i]
			}
			go agents.SpawnJediCouncil(db, name)
		}
		go agents.SpawnInquisitor(db)

		sigChan := make(chan os.Signal, 1)
		signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM, syscall.SIGUSR1)
		spawnedAgents := numAgents
		spawnedCaptains := numCaptain
		spawnedCouncil := numCouncil

		for {
			sig := <-sigChan
			switch sig {
			case syscall.SIGUSR1:
				// Dynamic scale-up: re-read agent counts and spawn any new agents.

				// Astromechs
				newTarget := spawnedAgents
				if n := store.GetConfig(db, "num_astromechs", ""); n != "" {
					fmt.Sscanf(n, "%d", &newTarget)
				}
				if newTarget < 1 {
					newTarget = 1
				}
				for spawnedAgents < newTarget {
					name := fmt.Sprintf("Astromech-%d", spawnedAgents+1)
					if spawnedAgents < len(astromechRoster) {
						name = astromechRoster[spawnedAgents]
					}
					fmt.Printf("Scaling: spawning %s (astromechs: %d → %d)\n", name, spawnedAgents, newTarget)
					go agents.SpawnAstromech(db, name)
					spawnedAgents++
				}
				if newTarget < spawnedAgents {
					fmt.Printf("Scale-down to %d astromech(s) requested (currently %d running) — takes effect on restart.\n", newTarget, spawnedAgents)
				}

				// Captains
				newCaptains := spawnedCaptains
				if n := store.GetConfig(db, "num_captain", ""); n != "" {
					fmt.Sscanf(n, "%d", &newCaptains)
				}
				if newCaptains < 1 {
					newCaptains = 1
				}
				for spawnedCaptains < newCaptains {
					name := fmt.Sprintf("Captain-%d", spawnedCaptains+1)
					if spawnedCaptains < len(captainRoster) {
						name = captainRoster[spawnedCaptains]
					}
					fmt.Printf("Scaling: spawning %s (captains: %d → %d)\n", name, spawnedCaptains, newCaptains)
					go agents.SpawnCaptain(db, name)
					spawnedCaptains++
				}
				if newCaptains < spawnedCaptains {
					fmt.Printf("Scale-down to %d captain(s) requested (currently %d running) — takes effect on restart.\n", newCaptains, spawnedCaptains)
				}

				// Council
				newCouncil := spawnedCouncil
				if n := store.GetConfig(db, "num_council", ""); n != "" {
					fmt.Sscanf(n, "%d", &newCouncil)
				}
				if newCouncil < 1 {
					newCouncil = 1
				}
				for spawnedCouncil < newCouncil {
					name := fmt.Sprintf("Council-%d", spawnedCouncil+1)
					if spawnedCouncil < len(councilRoster) {
						name = councilRoster[spawnedCouncil]
					}
					fmt.Printf("Scaling: spawning %s (council: %d → %d)\n", name, spawnedCouncil, newCouncil)
					go agents.SpawnJediCouncil(db, name)
					spawnedCouncil++
				}
				if newCouncil < spawnedCouncil {
					fmt.Printf("Scale-down to %d council member(s) requested (currently %d running) — takes effect on restart.\n", newCouncil, spawnedCouncil)
				}

			default:
				// SIGINT / SIGTERM — graceful drain then exit.
				fmt.Printf("\nReceived %v — draining in-flight tasks (up to 30s)...\n", sig)
				drainDeadline := time.Now().Add(30 * time.Second)
				for time.Now().Before(drainDeadline) {
					var active int
					db.QueryRow(`SELECT COUNT(*) FROM BountyBoard WHERE status IN ('Locked', 'UnderCaptainReview', 'UnderReview')`).Scan(&active)
					if active == 0 {
						fmt.Println("All tasks drained cleanly.")
						break
					}
					fmt.Printf("  %d task(s) still running, waiting...\n", active)
					time.Sleep(2 * time.Second)
				}
				if n := store.ReleaseInFlightTasks(db, "Fleet: reset on daemon shutdown"); n > 0 {
					fmt.Printf("Force-released %d in-flight task(s) back to Pending.\n", n)
				}
				fmt.Println("Daemon shut down.")
				os.Exit(0)
			}
		}

	case "add":
		if len(os.Args) < 3 {
			fmt.Println("Usage: force add [--priority N] [--plan-only] <task description>")
			os.Exit(1)
		}
		priority := 0
		planOnly := false
		addArgs := os.Args[2:]
		for i := 0; i < len(addArgs); i++ {
			if addArgs[i] == "--priority" && i+1 < len(addArgs) {
				priority = mustParseID(addArgs[i+1])
				addArgs = append(addArgs[:i], addArgs[i+2:]...)
				i--
			} else if addArgs[i] == "--plan-only" {
				planOnly = true
				addArgs = append(addArgs[:i], addArgs[i+1:]...)
				i--
			}
		}
		if len(addArgs) == 0 {
			fmt.Println("Usage: force add [--priority N] [--plan-only] <task description>")
			os.Exit(1)
		}
		taskPayload := strings.Join(addArgs, " ")
		if planOnly {
			taskPayload = "[PLAN_ONLY]\n" + taskPayload
		}
		id := store.AddBounty(db, 0, "Feature", taskPayload)
		if priority != 0 {
			store.SetBountyPriority(db, id, priority)
		}
		planSuffix := ""
		if planOnly {
			planSuffix = " — Commander will plan only; approve with: force convoy approve <convoy-id>"
		}
		fmt.Printf("Order transmitted to the Fleet: '%s'%s\n", strings.Join(addArgs, " "), planSuffix)

	case "add-task":
		// Direct CodeEdit task, skips Commander decomposition
		// Usage: force add-task [--blocked-by <id>] [--convoy <id>] [--priority N] [--timeout <secs>] <repo> <description>
		blockedBy := 0
		convoyID := 0
		priority := 0
		taskTimeout := 0
		taskArgs := os.Args[2:]
		for i := 0; i < len(taskArgs)-1; i++ {
			switch taskArgs[i] {
			case "--blocked-by":
				blockedBy = mustParseID(taskArgs[i+1])
				taskArgs = append(taskArgs[:i], taskArgs[i+2:]...)
				i--
			case "--convoy":
				convoyID = mustParseID(taskArgs[i+1])
				taskArgs = append(taskArgs[:i], taskArgs[i+2:]...)
				i--
			case "--priority":
				priority = mustParseID(taskArgs[i+1])
				taskArgs = append(taskArgs[:i], taskArgs[i+2:]...)
				i--
			case "--timeout":
				taskTimeout = mustParseID(taskArgs[i+1])
				taskArgs = append(taskArgs[:i], taskArgs[i+2:]...)
				i--
			}
		}
		if len(taskArgs) < 2 {
			fmt.Println("Usage: force add-task [--blocked-by <id>] [--convoy <id>] [--priority N] [--timeout <secs>] <repo> <description>")
			os.Exit(1)
		}
		repo := taskArgs[0]
		taskPayload := strings.Join(taskArgs[1:], " ")
		repoPath := store.GetRepoPath(db, repo)
		if repoPath == "" {
			fmt.Printf("Unknown repo '%s'. Register it first with: force add-repo\n", repo)
			os.Exit(1)
		}
		newID := store.AddCodeEditTask(db, repo, taskPayload, convoyID, priority, taskTimeout)
		if blockedBy > 0 {
			store.AddDependency(db, newID, blockedBy)
		}
		var suffix string
		if blockedBy > 0 {
			suffix += fmt.Sprintf(" (blocked by #%d)", blockedBy)
		}
		if convoyID > 0 {
			suffix += fmt.Sprintf(" (convoy %d)", convoyID)
		}
		if priority != 0 {
			suffix += fmt.Sprintf(" (priority %d)", priority)
		}
		if taskTimeout > 0 {
			suffix += fmt.Sprintf(" (timeout %ds)", taskTimeout)
		}
		fmt.Printf("CodeEdit task #%d queued for '%s'%s: %s\n", newID, repo, suffix, taskPayload)

	case "add-jira":
		// Usage: force add-jira [--priority N] [--plan-only] <TICKET-ID>
		priority := 0
		planOnly := false
		jiraArgs := os.Args[2:]
		for i := 0; i < len(jiraArgs); i++ {
			if jiraArgs[i] == "--priority" && i+1 < len(jiraArgs) {
				priority = mustParseID(jiraArgs[i+1])
				jiraArgs = append(jiraArgs[:i], jiraArgs[i+2:]...)
				i--
			} else if jiraArgs[i] == "--plan-only" {
				planOnly = true
				jiraArgs = append(jiraArgs[:i], jiraArgs[i+1:]...)
				i--
			}
		}
		if len(jiraArgs) < 1 {
			fmt.Println("Usage: force add-jira [--priority N] [--plan-only] <TICKET-ID>")
			os.Exit(1)
		}
		ticketID := jiraArgs[0]
		fmt.Printf("Fetching Jira ticket %s...\n", ticketID)

		jiraSystemPrompt := `You are a product manager assistant. Use your Atlassian Jira MCP tools to fetch the requested ticket.
Return a comprehensive feature description as plain text including: ticket title, description, acceptance criteria, and any relevant context from linked tickets.
Do not use markdown formatting. Write it as a clear feature request that a software architect can decompose into coding tasks.`

		description, err := claude.AskClaudeCLI(jiraSystemPrompt,
			fmt.Sprintf("Fetch Jira ticket %s and return its full context as a feature description.", ticketID),
			claude.AtlassianReadTools, 5)
		if err != nil {
			fmt.Printf("Failed to fetch Jira ticket: %v\n", err)
			os.Exit(1)
		}

		payload := fmt.Sprintf("[JIRA: %s]\n%s", ticketID, strings.TrimSpace(description))
		if planOnly {
			payload = "[PLAN_ONLY]\n" + payload
		}
		id := store.AddBounty(db, 0, "Feature", payload)
		if priority != 0 {
			store.SetBountyPriority(db, id, priority)
		}
		planSuffix := ""
		if planOnly {
			planSuffix = " — Commander will plan only; approve with: force convoy approve <convoy-id>"
		}
		fmt.Printf("Jira ticket %s added to the Fleet%s.\n", ticketID, planSuffix)

	case "repos":
		subCmd := ""
		if len(os.Args) >= 3 {
			subCmd = os.Args[2]
		}
		switch subCmd {
		case "remove":
			if len(os.Args) < 4 {
				fmt.Println("Usage: force repos remove <name>")
				os.Exit(1)
			}
			repoName := os.Args[3]
			if store.RemoveRepo(db, repoName) {
				fmt.Printf("Repository '%s' removed.\n", repoName)
			} else {
				fmt.Printf("Repository '%s' not found.\n", repoName)
			}
		default:
			// list repos (default)
			rows, err := db.Query(`SELECT name, local_path, description FROM Repositories ORDER BY name`)
			if err != nil {
				fmt.Printf("DB error: %v\n", err)
				os.Exit(1)
			}
			defer rows.Close()
			fmt.Printf("%-20s %-35s %s\n", "NAME", "PATH", "DESCRIPTION")
			fmt.Println(strings.Repeat("-", 90))
			found := false
			for rows.Next() {
				found = true
				var name, path, desc string
				rows.Scan(&name, &path, &desc)
				exists := ""
				if _, statErr := os.Stat(path); statErr != nil {
					exists = " [PATH MISSING]"
				}
				fmt.Printf("%-20s %-35s %s%s\n", name, truncate(path, 35), truncate(desc, 35), exists)
			}
			if !found {
				fmt.Println("No repositories registered. Run: force add-repo <name> <path> <desc>")
			}
		}

	case "add-repo":
		if len(os.Args) < 5 {
			fmt.Println("Usage: force add-repo <name> <local-path> <description>")
			os.Exit(1)
		}
		name := os.Args[2]
		repoRegPath := os.Args[3]
		desc := strings.Join(os.Args[4:], " ")
		// Verify the path exists and is a git repository
		if _, statErr := os.Stat(repoRegPath); statErr != nil {
			fmt.Printf("Path does not exist: %s\n", repoRegPath)
			os.Exit(1)
		}
		if out, gitErr := exec.Command("git", "-C", repoRegPath, "rev-parse", "--git-dir").CombinedOutput(); gitErr != nil {
			fmt.Printf("'%s' does not appear to be a git repository: %s\n", repoRegPath, strings.TrimSpace(string(out)))
			os.Exit(1)
		}
		store.AddRepo(db, name, repoRegPath, desc)
		fmt.Printf("Repository '%s' registered at %s\n", name, repoRegPath)

	case "reset":
		if len(os.Args) < 3 {
			fmt.Println("Usage: force reset <task-id>")
			os.Exit(1)
		}
		id := mustParseID(os.Args[2])
		store.ResetTask(db, id)
		store.LogAudit(db, "operator", "reset", id, "manual reset via CLI")
		fmt.Printf("Task %d reset to Pending.\n", id)

	case "list":
		// Usage: force list [status[,status2...]] [--limit N]
		statusFilter := ""
		limit := 0
		listArgs := os.Args[2:]
		for i := 0; i < len(listArgs); i++ {
			if listArgs[i] == "--limit" && i+1 < len(listArgs) {
				limit = mustParseID(listArgs[i+1])
				i++
			} else if !strings.HasPrefix(listArgs[i], "--") {
				statusFilter = listArgs[i]
			}
		}
		printList(db, statusFilter, limit)

	case "logs":
		if len(os.Args) < 3 {
			fmt.Println("Usage: force logs <task-id>")
			os.Exit(1)
		}
		printLogs(db, mustParseID(os.Args[2]))

	case "history":
		if len(os.Args) < 3 {
			fmt.Println("Usage: force history [--full] <task-id>")
			os.Exit(1)
		}
		full := false
		histArgs := os.Args[2:]
		if histArgs[0] == "--full" {
			full = true
			histArgs = histArgs[1:]
		}
		if len(histArgs) == 0 {
			fmt.Println("Usage: force history [--full] <task-id>")
			os.Exit(1)
		}
		printHistory(db, mustParseID(histArgs[0]), full)

	case "estop":
		agents.SetEstop(db, true)
		telemetry.EmitEvent(telemetry.EventEstop(true))
		store.LogAudit(db, "operator", "estop", 0, "emergency stop activated")
		fmt.Println("E-STOP ACTIVATED. All agents will halt after their current sleep cycle.")
		fmt.Println("Run 'force resume' to re-enable agents.")

	case "resume":
		agents.SetEstop(db, false)
		telemetry.EmitEvent(telemetry.EventEstop(false))
		store.LogAudit(db, "operator", "resume", 0, "emergency stop cleared")
		fmt.Println("E-stop cleared. Agents will resume on their next cycle.")

	case "escalations":
		subCmd := ""
		if len(os.Args) >= 3 {
			subCmd = os.Args[2]
		}
		switch subCmd {
		case "list", "":
			statusFilter := ""
			if len(os.Args) >= 4 {
				statusFilter = os.Args[3]
			}
			printEscalations(db, statusFilter)
		case "ack":
			if len(os.Args) < 4 {
				fmt.Println("Usage: force escalations ack <id>")
				os.Exit(1)
			}
			id := mustParseID(os.Args[3])
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
			if len(os.Args) < 4 {
				fmt.Println("Usage: force escalations close <id>")
				os.Exit(1)
			}
			id := mustParseID(os.Args[3])
			agents.CloseEscalation(db, id, false)
			fmt.Printf("Escalation %d closed.\n", id)
		case "requeue":
			if len(os.Args) < 4 {
				fmt.Println("Usage: force escalations requeue <id>")
				os.Exit(1)
			}
			id := mustParseID(os.Args[3])
			agents.CloseEscalation(db, id, true)
			fmt.Printf("Escalation %d closed and task re-queued.\n", id)
		default:
			fmt.Printf("Unknown escalations subcommand: %s\n", subCmd)
			fmt.Println("Usage: force escalations [list|ack <id>|close <id>|requeue <id>]")
			os.Exit(1)
		}

	case "convoy":
		cmdConvoy(db, os.Args[2:])

	case "config":
		cmdConfig(db, os.Args[2:])

	case "status":
		printStatus(db)

	case "who":
		printWho(db)

	case "retry-all-failed":
		n := store.ResetAllFailed(db)
		store.LogAudit(db, "operator", "retry-all-failed", 0, fmt.Sprintf("reset %d failed tasks", n))
		fmt.Printf("Reset %d failed task(s) to Pending.\n", n)

	case "retry":
		// Alias for reset
		if len(os.Args) < 3 {
			fmt.Println("Usage: force retry <task-id>")
			os.Exit(1)
		}
		id := mustParseID(os.Args[2])
		store.ResetTask(db, id)
		store.LogAudit(db, "operator", "reset", id, "retry via CLI")
		fmt.Printf("Task %d reset to Pending.\n", id)

	case "cancel":
		if len(os.Args) < 3 {
			fmt.Println("Usage: force cancel <task-id>")
			os.Exit(1)
		}
		id := mustParseID(os.Args[2])
		var currentStatus string
		db.QueryRow(`SELECT status FROM BountyBoard WHERE id = ?`, id).Scan(&currentStatus)
		if currentStatus == "" {
			fmt.Printf("Task %d not found.\n", id)
			os.Exit(1)
		}
		if currentStatus == "Completed" {
			fmt.Printf("Task %d is already Completed and cannot be cancelled.\n", id)
			os.Exit(1)
		}
		store.CancelTask(db, id, "Cancelled by operator")
		store.LogAudit(db, "operator", "cancel", id, "cancelled via CLI")
		fmt.Printf("Task %d cancelled.\n", id)

	case "block":
		if len(os.Args) < 4 {
			fmt.Println("Usage: force block <task-id> <blocker-id>")
			os.Exit(1)
		}
		taskID := mustParseID(os.Args[2])
		blockerID := mustParseID(os.Args[3])
		var count int
		db.QueryRow(`SELECT COUNT(*) FROM BountyBoard WHERE id IN (?, ?)`, taskID, blockerID).Scan(&count)
		if count != 2 {
			fmt.Printf("One or both tasks not found (task %d, blocker %d).\n", taskID, blockerID)
			os.Exit(1)
		}
		store.AddDependency(db, taskID, blockerID)
		fmt.Printf("Task %d is now blocked by task %d.\n", taskID, blockerID)

	case "unblock":
		if len(os.Args) < 3 {
			fmt.Println("Usage: force unblock <task-id>")
			os.Exit(1)
		}
		id := mustParseID(os.Args[2])
		var taskExists int
		db.QueryRow(`SELECT COUNT(*) FROM BountyBoard WHERE id = ?`, id).Scan(&taskExists)
		if taskExists == 0 {
			fmt.Printf("Task %d not found.\n", id)
		} else {
			store.RemoveDependenciesOf(db, id)
			fmt.Printf("Task %d unblocked (all dependencies removed).\n", id)
		}

	case "unblock-dependents":
		if len(os.Args) < 3 {
			fmt.Println("Usage: force unblock-dependents <task-id>")
			os.Exit(1)
		}
		id := mustParseID(os.Args[2])
		count := store.UnblockDependentsOf(db, id)
		if count == 0 {
			fmt.Printf("No tasks were depending on #%d.\n", id)
		} else {
			fmt.Printf("Removed %d dependency edge(s) pointing to #%d.\n", count, id)
		}

	case "tree":
		if len(os.Args) < 3 {
			fmt.Println("Usage: force tree <task-id>")
			os.Exit(1)
		}
		id := mustParseID(os.Args[2])
		printTree(db, id, 0)

	case "diff":
		if len(os.Args) < 3 {
			fmt.Println("Usage: force diff <task-id>")
			os.Exit(1)
		}
		id := mustParseID(os.Args[2])
		b, err := store.GetBounty(db, id)
		if err != nil {
			fmt.Printf("Task %d not found\n", id)
			os.Exit(1)
		}
		if b.BranchName == "" {
			fmt.Printf("Task %d has no branch yet (status: %s)\n", id, b.Status)
			os.Exit(1)
		}
		repoPath := store.GetRepoPath(db, b.TargetRepo)
		if repoPath == "" {
			fmt.Printf("Unknown repo '%s'\n", b.TargetRepo)
			os.Exit(1)
		}
		diff := igit.GetDiff(repoPath, b.BranchName)
		if diff == "" {
			fmt.Printf("No diff found for branch %s — branch may not have any commits yet\n", b.BranchName)
		} else {
			fmt.Printf("Branch: %s\n\n", b.BranchName)
			fmt.Println(diff)
		}

	case "approve":
		// Operator manually approves a task, bypassing the Jedi Council
		if len(os.Args) < 3 {
			fmt.Println("Usage: force approve <task-id>")
			os.Exit(1)
		}
		id := mustParseID(os.Args[2])
		b, err := store.GetBounty(db, id)
		if err != nil {
			fmt.Printf("Task %d not found\n", id)
			os.Exit(1)
		}
		if b.Status != "AwaitingCouncilReview" && b.Status != "UnderReview" &&
			b.Status != "AwaitingCaptainReview" && b.Status != "UnderCaptainReview" {
			fmt.Printf("Task %d is not awaiting review (status: %s)\n", id, b.Status)
			os.Exit(1)
		}
		repoPath := store.GetRepoPath(db, b.TargetRepo)
		if repoPath == "" {
			fmt.Printf("Unknown repo '%s'\n", b.TargetRepo)
			os.Exit(1)
		}
		branchName := b.BranchName
		if branchName == "" {
			branchName = fmt.Sprintf("agent/task-%d", id)
		}
		worktreeDir := igit.ResolveWorktreeDir(db, branchName, repoPath, id, agents.BranchAgentName)
		// Get diff before merge — branch is deleted by MergeAndCleanup.
		diff := igit.GetDiff(repoPath, branchName)
		if mergeErr := igit.MergeAndCleanup(repoPath, branchName, worktreeDir); mergeErr != nil {
			fmt.Printf("Merge failed: %v\n", mergeErr)
			os.Exit(1)
		}
		store.UpdateBountyStatus(db, id, "Completed")
		store.UnblockDependentsOf(db, id)
		if diff != "" {
			changedFiles := igit.ExtractDiffFiles(diff)
			filesStr := strings.Join(changedFiles, ", ")
			store.StoreFleetMemory(db, b.TargetRepo, b.ID, "success",
				fmt.Sprintf("Task: %s", truncate(b.Payload, 400)), filesStr)
		}
		telemetry.EmitEvent(telemetry.TelemetryEvent{
			EventType: "operator_approved",
			Payload:   map[string]any{"task_id": id},
		})
		store.LogAudit(db, "operator", "approve", id, "manually approved and merged")
		fmt.Printf("Task %d approved and merged by operator.\n", id)

	case "reject":
		// Operator manually rejects a task, sending it back with feedback
		if len(os.Args) < 4 {
			fmt.Println("Usage: force reject <task-id> <reason>")
			os.Exit(1)
		}
		id := mustParseID(os.Args[2])
		reason := strings.Join(os.Args[3:], " ")
		b, err := store.GetBounty(db, id)
		if err != nil {
			fmt.Printf("Task %d not found\n", id)
			os.Exit(1)
		}
		retryCount := store.IncrementRetryCount(db, id)
		if retryCount >= agents.MaxRetries {
			store.FailBounty(db, id, fmt.Sprintf("Operator rejected (final): %s", reason))
			fmt.Printf("Task %d permanently failed (max retries reached).\n", id)
		} else {
			newPayload := fmt.Sprintf("%s\n\nOPERATOR FEEDBACK (attempt %d/%d): %s", b.Payload, retryCount, agents.MaxRetries, reason)
			store.ReturnTaskForRework(db, id, newPayload)
			fmt.Printf("Task %d returned for rework (attempt %d/%d): %s\n", id, retryCount, agents.MaxRetries, reason)
		}
		telemetry.EmitEvent(telemetry.TelemetryEvent{
			EventType: "operator_rejected",
			Payload:   map[string]any{"task_id": id, "reason": reason},
		})
		store.LogAudit(db, "operator", "reject", id, reason)

	case "agents":
		printAgents(db)

	case "cleanup":
		runCleanup(db)

	case "logs-fleet":
		// Flags: --no-follow, --filter <pattern>, --agent <name>, --task <id>, --convoy <id>
		noFollow := false
		filterPattern := ""
		fleetArgs := os.Args[2:]
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

	case "holonet":
		// Flags: --no-follow (dump last 50), --filter <event_type> (grep by type), --task <id>
		noFollow := false
		filterType := ""
		filterTask := ""
		holoArgs := os.Args[2:]
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

	case "stats":
		printStats(db)

	case "export":
		// Export the full task board (and optionally repos) to JSON for backup/tooling
		outFile := "fleet-export.json"
		if len(os.Args) >= 3 {
			outFile = os.Args[2]
		}
		if err := exportFleet(db, outFile); err != nil {
			fmt.Printf("Export failed: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("Fleet exported to %s\n", outFile)

	case "import":
		// Import tasks from a JSON file (as produced by force export)
		if len(os.Args) < 3 {
			fmt.Println("Usage: force import <file.json>")
			os.Exit(1)
		}
		n, err := importFleet(db, os.Args[2])
		if err != nil {
			fmt.Printf("Import failed: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("Imported %d task(s).\n", n)

	case "search":
		if len(os.Args) < 3 {
			fmt.Println("Usage: force search <query>")
			os.Exit(1)
		}
		rawQuery := strings.Join(os.Args[2:], " ")
		escapedQuery := strings.ReplaceAll(rawQuery, `\`, `\\`)
		escapedQuery = strings.ReplaceAll(escapedQuery, `%`, `\%`)
		escapedQuery = strings.ReplaceAll(escapedQuery, `_`, `\_`)
		query := "%" + escapedQuery + "%"
		rows, err := db.Query(`
			SELECT id, target_repo, type, status, payload
			FROM BountyBoard
			WHERE payload LIKE ? ESCAPE '\' OR error_log LIKE ? ESCAPE '\'
			ORDER BY id DESC
			LIMIT 50`, query, query)
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
			// Show first line of payload for readability
			firstLine := payload
			if nl := strings.Index(payload, "\n"); nl != -1 {
				firstLine = payload[:nl]
			}
			fmt.Printf("[#%d] %s | %s | %s | %s\n", id, status, taskType, repo, truncate(firstLine, 80))
		}
		if !found {
			fmt.Println("No tasks match your query.")
		}

	case "scale":
		// Dynamically add astromech goroutines to a running daemon via SIGUSR1.
		if len(os.Args) < 3 {
			fmt.Println("Usage: force scale <num_astromechs>")
			os.Exit(1)
		}
		n, err := strconv.Atoi(os.Args[2])
		if err != nil || n < 1 {
			fmt.Fprintln(os.Stderr, "scale: argument must be a positive integer")
			os.Exit(1)
		}
		store.SetConfig(db, "num_astromechs", strconv.Itoa(n))
		pidData, pidErr := os.ReadFile("fleet.pid")
		if pidErr != nil {
			fmt.Printf("Updated num_astromechs=%d. (No running daemon — takes effect on next start.)\n", n)
			break
		}
		pid, _ := strconv.Atoi(strings.TrimSpace(string(pidData)))
		if pid <= 0 {
			fmt.Printf("Updated num_astromechs=%d. (Invalid PID in fleet.pid.)\n", n)
			break
		}
		proc, findErr := os.FindProcess(pid)
		if findErr != nil {
			fmt.Printf("Updated num_astromechs=%d. (Cannot find daemon process.)\n", n)
			break
		}
		if sigErr := proc.Signal(syscall.SIGUSR1); sigErr != nil {
			fmt.Printf("Updated num_astromechs=%d. (Signal failed: %v)\n", n, sigErr)
		} else {
			fmt.Printf("Scaling to %d astromech(s) — SIGUSR1 sent to daemon (PID %d).\n", n, pid)
		}

	case "prioritize":
		// Usage: force prioritize <task-id> <priority>
		if len(os.Args) < 4 {
			fmt.Println("Usage: force prioritize <task-id> <priority>")
			fmt.Println("  priority is an integer — higher values claim first (default 0)")
			os.Exit(1)
		}
		taskID := mustParseID(os.Args[2])
		prio := mustParseID(os.Args[3])
		var exists int
		db.QueryRow(`SELECT COUNT(*) FROM BountyBoard WHERE id = ?`, taskID).Scan(&exists)
		if exists == 0 {
			fmt.Printf("Task %d not found.\n", taskID)
			os.Exit(1)
		}
		store.SetBountyPriority(db, taskID, prio)
		store.LogAudit(db, "operator", "prioritize", taskID, fmt.Sprintf("set priority=%d", prio))
		fmt.Printf("Task %d priority set to %d.\n", taskID, prio)

	case "audit":
		// Usage: force audit [--limit N]
		limit := 50
		for i := 2; i < len(os.Args); i++ {
			if os.Args[i] == "--limit" && i+1 < len(os.Args) {
				limit = mustParseID(os.Args[i+1])
				i++
			}
		}
		entries := store.ListAuditLog(db, limit)
		if len(entries) == 0 {
			fmt.Println("No audit log entries.")
			break
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

	case "prune":
		// Usage: force prune [--keep-days N] [--dry-run]
		keepDays := 30
		dryRun := false
		for i := 2; i < len(os.Args); i++ {
			switch os.Args[i] {
			case "--keep-days":
				if i+1 < len(os.Args) {
					keepDays = mustParseID(os.Args[i+1])
					i++
				}
			case "--dry-run":
				dryRun = true
			}
		}
		pruneFleet(db, keepDays, dryRun)

	case "run":
		// One-shot foreground mode: claim and run a specific task with streamed output.
		// Usage: force run <task-id>
		if len(os.Args) < 3 {
			fmt.Println("Usage: force run <task-id>")
			os.Exit(1)
		}
		runID := mustParseID(os.Args[2])
		agents.RunTaskForeground(db, runID)

	case "memories":
		cmdMemories(db, os.Args[2:])

	case "directive":
		agents.CmdDirective(os.Args[2:])

	case "dogs":
		dogs := agents.ListDogs(db)
		fmt.Printf("%-20s %-10s %-20s %s\n", "DOG", "RUNS", "LAST RUN", "NEXT RUN")
		fmt.Println(strings.Repeat("-", 75))
		for _, d := range dogs {
			lastRun := d.LastRun
			if lastRun == "" {
				lastRun = "(never)"
			}
			fmt.Printf("%-20s %-10d %-20s %s\n", d.Name, d.RunCount, truncate(lastRun, 20), d.NextRun)
		}

	case "doctor":
		clean := false
		for _, arg := range os.Args[2:] {
			if arg == "--clean" {
				clean = true
			}
		}
		runDoctor(db, clean)

	case "mail":
		cmdMail(db, os.Args[2:])

	case "costs":
		printCosts(db)

	case "dashboard":
		port := 8080
		if len(os.Args) >= 4 && os.Args[2] == "--port" {
			port = mustParseID(os.Args[3])
		} else if len(os.Args) >= 3 && strings.HasPrefix(os.Args[2], "--port=") {
			port = mustParseID(strings.TrimPrefix(os.Args[2], "--port="))
		} else if len(os.Args) >= 3 {
			if p := mustParseID(os.Args[2]); p > 0 {
				port = p
			}
		}
		RunDashboard(db, port)

	case "watch":
		sigChan := make(chan os.Signal, 1)
		signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
		go func() {
			<-sigChan
			fmt.Print("\033[?25h")
			fmt.Print("\033[H\033[2J")
			os.Exit(0)
		}()
		RunCommandCenter(db)

	default:
		fmt.Fprintf(os.Stderr, "Unknown command: %s\n\nRun 'force help' for usage.\n", command)
		os.Exit(1)
	}
}
