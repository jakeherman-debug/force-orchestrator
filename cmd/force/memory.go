package main

import (
	"database/sql"
	"fmt"
	"os"
	"strings"

	"force-orchestrator/internal/store"
)

func cmdMemories(db *sql.DB, args []string) {
	subCmd := ""
	if len(args) >= 1 && !strings.HasPrefix(args[0], "--") {
		subCmd = args[0]
	}

	switch subCmd {
	case "delete":
		if len(args) < 2 {
			fmt.Println("Usage: force memories delete <id>")
			os.Exit(1)
		}
		memID := mustParseID(args[1])
		if store.DeleteFleetMemory(db, memID) {
			fmt.Printf("Memory #%d deleted.\n", memID)
		} else {
			fmt.Printf("Memory #%d not found.\n", memID)
		}

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
