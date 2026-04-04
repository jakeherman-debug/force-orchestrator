package main

import (
	"flag"
	"fmt"
	"os"
	"runtime"
	"strconv"
	"strings"

	"force-orchestrator/internal/agents"
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
		cmdDaemon(db)

	case "add":
		if len(os.Args) < 3 {
			fmt.Println("Usage: force add [--priority N] [--plan-only] [--type Feature|CodeEdit|Investigate|Audit] <task description>")
			os.Exit(1)
		}
		cmdAdd(db, os.Args[2:])

	case "add-task":
		cmdAddTask(db, os.Args[2:])

	case "investigate", "add-investigate":
		if len(os.Args) < 3 {
			fmt.Println("Usage: force investigate [--priority N] [--repo <name>] <question>")
			os.Exit(1)
		}
		cmdAddInvestigate(db, os.Args[2:])

	case "scan", "add-audit":
		if len(os.Args) < 3 {
			fmt.Println("Usage: force scan [--priority N] [--repo <name>] <scope/question>")
			os.Exit(1)
		}
		cmdAddAudit(db, os.Args[2:])

	case "add-jira":
		cmdAddJira(db, os.Args[2:])

	case "repos":
		cmdRepos(db, os.Args[2:])

	case "add-repo":
		if len(os.Args) < 5 {
			fmt.Println("Usage: force add-repo <name> <local-path> <description>")
			os.Exit(1)
		}
		cmdAddRepo(db, os.Args[2], os.Args[3], strings.Join(os.Args[4:], " "))

	case "reset":
		if len(os.Args) < 3 {
			fmt.Println("Usage: force reset <task-id>")
			os.Exit(1)
		}
		cmdReset(db, mustParseID(os.Args[2]), "manual reset via CLI")

	case "retry":
		if len(os.Args) < 3 {
			fmt.Println("Usage: force retry <task-id>")
			os.Exit(1)
		}
		cmdReset(db, mustParseID(os.Args[2]), "retry via CLI")

	case "cancel":
		if len(os.Args) < 3 {
			fmt.Println("Usage: force cancel <task-id> [--requeue <type>]")
			os.Exit(1)
		}
		cmdCancel(db, os.Args[2:])

	case "block":
		if len(os.Args) < 4 {
			fmt.Println("Usage: force block <task-id> <blocker-id>")
			os.Exit(1)
		}
		cmdBlock(db, mustParseID(os.Args[2]), mustParseID(os.Args[3]))

	case "unblock":
		if len(os.Args) < 3 {
			fmt.Println("Usage: force unblock <task-id>")
			os.Exit(1)
		}
		cmdUnblock(db, mustParseID(os.Args[2]))

	case "unblock-dependents":
		if len(os.Args) < 3 {
			fmt.Println("Usage: force unblock-dependents <task-id>")
			os.Exit(1)
		}
		cmdUnblockDependents(db, mustParseID(os.Args[2]))

	case "tree":
		if len(os.Args) < 3 {
			fmt.Println("Usage: force tree <task-id>")
			os.Exit(1)
		}
		cmdTree(db, mustParseID(os.Args[2]))

	case "diff":
		if len(os.Args) < 3 {
			fmt.Println("Usage: force diff <task-id>")
			os.Exit(1)
		}
		cmdDiff(db, mustParseID(os.Args[2]))

	case "approve":
		// Operator manually approves a task, bypassing the Jedi Council
		if len(os.Args) < 3 {
			fmt.Println("Usage: force approve <task-id>")
			os.Exit(1)
		}
		cmdApproveTask(db, mustParseID(os.Args[2]))

	case "reject":
		// Operator manually rejects a task, sending it back with feedback
		if len(os.Args) < 4 {
			fmt.Println("Usage: force reject <task-id> <reason>")
			os.Exit(1)
		}
		cmdRejectTask(db, mustParseID(os.Args[2]), strings.Join(os.Args[3:], " "))

	case "prioritize":
		if len(os.Args) < 4 {
			fmt.Println("Usage: force prioritize <task-id> <priority>")
			fmt.Println("  priority is an integer — higher values claim first (default 0)")
			os.Exit(1)
		}
		cmdPrioritize(db, mustParseID(os.Args[2]), mustParseID(os.Args[3]))

	case "retry-all-failed":
		cmdRetryAllFailed(db)

	case "list":
		// Usage: force list [status[,status2...]] [--status <s>] [--repo <name>] [--type <type>] [--limit N]
		statusFilter := ""
		repoFilter := ""
		typeFilter := ""
		limit := 0
		listArgs := os.Args[2:]
		for i := 0; i < len(listArgs); i++ {
			switch listArgs[i] {
			case "--limit":
				if i+1 < len(listArgs) {
					limit = mustParseID(listArgs[i+1])
					i++
				}
			case "--repo":
				if i+1 < len(listArgs) {
					repoFilter = listArgs[i+1]
					i++
				}
			case "--type":
				if i+1 < len(listArgs) {
					typeFilter = listArgs[i+1]
					i++
				}
			case "--status":
				if i+1 < len(listArgs) {
					statusFilter = listArgs[i+1]
					i++
				}
			default:
				if !strings.HasPrefix(listArgs[i], "--") {
					statusFilter = listArgs[i]
					if strings.EqualFold(statusFilter, "active") {
						statusFilter = "Pending,Locked,Planned,AwaitingCaptainReview,UnderCaptainReview,AwaitingCouncilReview,UnderReview,Failed,Escalated,ConflictPending"
					}
				}
			}
		}
		printList(db, statusFilter, repoFilter, typeFilter, limit)

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

	case "agents":
		printAgents(db)

	case "status":
		cmdStatus(db)

	case "who":
		cmdWho(db)

	case "stats":
		cmdStats(db)

	case "logs-fleet":
		cmdLogsFleet(db, os.Args[2:])

	case "tail":
		if len(os.Args) < 3 {
			fmt.Println("Usage: force tail <task-id>")
			os.Exit(1)
		}
		cmdTailTask(db, mustParseID(os.Args[2]))

	case "holonet":
		cmdHolonet(db, os.Args[2:])

	case "export":
		outFile := "fleet-export.json"
		if len(os.Args) >= 3 {
			outFile = os.Args[2]
		}
		cmdExport(db, outFile)

	case "import":
		if len(os.Args) < 3 {
			fmt.Println("Usage: force import <file.json>")
			os.Exit(1)
		}
		cmdImport(db, os.Args[2])

	case "search":
		if len(os.Args) < 3 {
			fmt.Println("Usage: force search <query>")
			os.Exit(1)
		}
		cmdSearch(db, strings.Join(os.Args[2:], " "))

	case "audit":
		// Usage: force audit [--limit N]
		limit := 50
		for i := 2; i < len(os.Args); i++ {
			if os.Args[i] == "--limit" && i+1 < len(os.Args) {
				limit = mustParseID(os.Args[i+1])
				i++
			}
		}
		cmdAudit(db, limit)

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
			default:
				fmt.Fprintf(os.Stderr, "prune: unknown flag %q\nUsage: force prune [--keep-days N] [--dry-run]\n", os.Args[i])
				os.Exit(1)
			}
		}
		cmdPrune(db, keepDays, dryRun)

	case "purge":
		// Usage: force purge [--confirm]
		confirmed := false
		for _, arg := range os.Args[2:] {
			if arg == "--confirm" {
				confirmed = true
			}
		}
		cmdPurge(db, confirmed)

	case "hard-reset":
		// Usage: force hard-reset [--purge-repos] [--confirm]
		confirmed := false
		purgeRepos := false
		for _, arg := range os.Args[2:] {
			switch arg {
			case "--confirm":
				confirmed = true
			case "--purge-repos":
				purgeRepos = true
			}
		}
		cmdHardReset(db, confirmed, purgeRepos)

	case "scale":
		// Dynamically scale agent counts via named flags.
		fs := flag.NewFlagSet("scale", flag.ExitOnError)
		scaleAstromechs := fs.Int("astromechs", -1, "number of astromechs")
		scaleCouncil := fs.Int("council", -1, "number of council members")
		scaleCaptain := fs.Int("captain", -1, "number of captains")
		scaleInvestigators := fs.Int("investigators", -1, "number of investigators")
		scaleAuditors := fs.Int("auditors", -1, "number of auditors")
		fs.Parse(os.Args[2:])
		cmdScale(db, *scaleAstromechs, *scaleCouncil, *scaleCaptain, *scaleInvestigators, *scaleAuditors)

	case "estop":
		cmdEstop(db)

	case "resume":
		cmdResume(db)

	case "escalations":
		cmdEscalations(db, os.Args[2:])

	case "dogs":
		cmdDogs(db)

	case "cleanup":
		cmdCleanup(db)

	case "doctor":
		clean := false
		for _, arg := range os.Args[2:] {
			if arg == "--clean" {
				clean = true
			}
		}
		cmdDoctor(db, clean)

	case "costs":
		cmdCosts(db)

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
		cmdDashboard(db, port)

	case "watch":
		cmdWatch(db)

	case "run":
		// One-shot foreground mode: claim and run a specific task with streamed output.
		// Usage: force run <task-id>
		if len(os.Args) < 3 {
			fmt.Println("Usage: force run <task-id>")
			os.Exit(1)
		}
		agents.RunTaskForeground(db, mustParseID(os.Args[2]))

	case "convoy":
		cmdConvoy(db, os.Args[2:])

	case "config":
		cmdConfig(db, os.Args[2:])

	case "memories":
		cmdMemories(db, os.Args[2:])

	case "directive":
		agents.CmdDirective(os.Args[2:])

	case "mail":
		cmdMail(db, os.Args[2:])

	default:
		fmt.Fprintf(os.Stderr, "Unknown command: %s\n\nRun 'force help' for usage.\n", command)
		os.Exit(1)
	}
}
