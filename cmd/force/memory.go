package main

import (
	"database/sql"
	"flag"
	"fmt"
	"os"
	"strings"

	"force-orchestrator/internal/store"
)

// cmdMemories handles the `force memories` subcommands.
//
// Read-only verbs (search, default list) keep their inline switch-case
// bodies. Destructive verbs (delete) are extracted into per-verb
// cmdMemories<Verb> handlers that route through parseSubcommandFlags so
// --help short-circuits BEFORE any FleetMemory row is removed.
func cmdMemories(db *sql.DB, args []string) {
	// Top-level --help / -h interception (the leaf branches handle their
	// own --flags; the top-level dispatch here only filters help shorthand).
	if len(args) >= 1 && (args[0] == "--help" || args[0] == "-h" || args[0] == "help") {
		fmt.Println("Usage: force memories [<repo>|delete <id>|search <repo> <query>]")
		return
	}
	subCmd := ""
	if len(args) >= 1 && !strings.HasPrefix(args[0], "--") {
		subCmd = args[0]
	}

	switch subCmd {
	case "delete":
		cmdMemoriesDelete(db, args[1:])

	case "search":
		if len(args) < 3 {
			fmt.Println("Usage: force memories search <repo> <query>")
			os.Exit(1)
		}
		searchRepo := args[1]
		searchQuery := strings.Join(args[2:], " ")
		memories := store.GetFleetMemories(db, searchRepo, searchQuery, 20)
		if len(memories) == 0 {
			fmt.Printf("No memories found for repo '%s' matching '%s'.\n", searchRepo, searchQuery)
		} else {
			fmt.Printf("FLEET MEMORY SEARCH — %s — %q\n", searchRepo, searchQuery)
			fmt.Println(strings.Repeat("-", 80))
			for _, m := range memories {
				outcome := "[PASS]"
				if m.Outcome == "failure" {
					outcome = "[FAIL]"
				}
				fmt.Printf("%s [Task #%d] %s\n", outcome, m.TaskID, truncate(m.Summary, 100))
				if m.FilesChanged != "" {
					fmt.Printf("  Files: %s\n", m.FilesChanged)
				}
				fmt.Printf("  Recorded: %s\n\n", m.CreatedAt)
			}
		}

	default:
		// List memories, optionally filtered by repo
		repo := subCmd // non-flag, non-keyword arg is treated as repo filter
		limit := 20
		for i := 0; i < len(args); i++ {
			if args[i] == "--limit" && i+1 < len(args) {
				fmt.Sscan(args[i+1], &limit)
				i++
			} else if !strings.HasPrefix(args[i], "--") {
				repo = args[i]
			}
		}
		memories := store.ListAllFleetMemories(db, repo, limit)
		if len(memories) == 0 {
			fmt.Println("No fleet memories recorded yet.")
		} else {
			header := "FLEET MEMORY"
			if repo != "" {
				header += " — " + repo
			}
			fmt.Println(header)
			fmt.Println(strings.Repeat("-", 80))
			for _, m := range memories {
				outcome := "[PASS]"
				if m.Outcome == "failure" {
					outcome = "[FAIL]"
				}
				fmt.Printf("%s [Task #%d] [%s] %s\n", outcome, m.TaskID, m.Repo, truncate(m.Summary, 100))
				if m.FilesChanged != "" {
					fmt.Printf("  Files: %s\n", m.FilesChanged)
				}
				fmt.Printf("  Recorded: %s\n\n", m.CreatedAt)
			}
		}
	}
}

// cmdMemoriesDelete — `force memories delete <id>`. DESTRUCTIVE: removes
// the FleetMemory row with the given id.
func cmdMemoriesDelete(db *sql.DB, args []string) {
	fs := flag.NewFlagSet("memories delete", flag.ContinueOnError)
	helped, perr := parseSubcommandFlags(fs, args, "memories delete",
		"Delete a FleetMemory row by ID.",
		[]flagDoc{{Name: "--help, -h", Desc: "show this help and exit"}},
		[]string{"force memories delete 42"})
	if helped {
		return
	}
	if perr != nil {
		os.Exit(2)
	}
	rest := fs.Args()
	if len(rest) < 1 {
		fmt.Println("Usage: force memories delete <id>")
		os.Exit(1)
	}
	memID := mustParseID(rest[0])
	if store.DeleteFleetMemory(db, memID) {
		fmt.Printf("Memory #%d deleted.\n", memID)
	} else {
		fmt.Printf("Memory #%d not found.\n", memID)
	}
}
